// SPDX-License-Identifier: Apache-2.0
package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/shenli/ramet/engine"
)

type artifactFileCost struct {
	Name            string `json:"name"`
	Size            int64  `json:"size"`
	Chunks          int    `json:"chunks"`
	NewContentBytes int64  `json:"new_content_bytes"`
}
type artifactMeasurement struct {
	Mode              string             `json:"mode"`
	Revision          string             `json:"revision"`
	CheckpointSeconds float64            `json:"checkpoint_seconds"`
	RestoreSeconds    float64            `json:"cold_restore_seconds"`
	SaveTransfer      transferCounts     `json:"save_transfer"`
	RestoreTransfer   transferCounts     `json:"restore_transfer"`
	Files             []artifactFileCost `json:"files"`
	Verified          bool               `json:"verified"`
}

// This opt-in, end-to-end comparison uses generated documents and real durable
// providers. Run without -race for timings; correctness runs use -race.
func TestArtifactCheckpointWorkload(t *testing.T) {
	fixture := os.Getenv("RAMET_ARTIFACT_FIXTURES")
	if fixture == "" {
		t.Skip("set RAMET_ARTIFACT_FIXTURES to generated document revisions")
	}
	fixture, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var revisions []string
	for _, entry := range entries {
		if entry.IsDir() {
			revisions = append(revisions, entry.Name())
		}
	}
	if len(revisions) != 4 {
		t.Fatal("expected four generated revisions")
	}
	var results []artifactMeasurement
	for _, mode := range []string{"whole_files", "chunks"} {
		t.Run(mode, func(t *testing.T) {
			_, ns, _ := setup(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			c, counts := observedContent(t, ns)
			open := func() *engine.Store {
				s, err := engine.OpenContext(ctx, t.TempDir(), engine.Options{Metadata: metadata(t, ctx, ns), Content: c, DisableChunking: mode == "whole_files"})
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			s := open()
			defer func() { _ = s.Close() }()
			before := counts.counts()
			started := time.Now()
			base, err := s.Snapshot(ctx, engine.SnapshotOptions{Source: filepath.Join(fixture, revisions[0])})
			if err != nil {
				t.Fatal(err)
			}
			elapsed, transfer := time.Since(started).Seconds(), counts.counts().since(before)
			saved := []engine.Snapshot{base}
			seen := map[string]bool{}
			record := func(revision string, snap engine.Snapshot, seconds float64, transfer transferCounts) {
				row := artifactMeasurement{Mode: mode, Revision: revision, CheckpointSeconds: seconds, SaveTransfer: transfer}
				for _, entry := range snap.Manifest.Entries {
					if entry.Kind != "file" {
						continue
					}
					file := artifactFileCost{Name: entry.Path, Size: entry.Size, Chunks: len(entry.Chunks)}
					refs := entry.Chunks
					if len(refs) == 0 {
						refs = []engine.ContentRef{{Digest: entry.Digest, Size: entry.Size}}
					}
					for _, ref := range refs {
						if !seen[ref.Digest] {
							file.NewContentBytes += ref.Size
							seen[ref.Digest] = true
						}
					}
					row.Files = append(row.Files, file)
				}
				results = append(results, row)
			}
			record(revisions[0], base, elapsed, transfer)
			p, err := s.CreatePool(ctx, engine.PoolOptions{Base: base.ID, Concurrency: 1, LeaseSeconds: 300})
			if err != nil {
				t.Fatal(err)
			}
			e, err := p.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, revision := range revisions[1:] {
				copyArtifactRevision(t, filepath.Join(fixture, revision), e.Path)
				before = counts.counts()
				started = time.Now()
				snap, err := p.Checkpoint(ctx, e)
				if err != nil {
					t.Fatal(err)
				}
				record(revision, snap, time.Since(started).Seconds(), counts.counts().since(before))
				saved = append(saved, snap)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			// Each restore starts with an empty worker cache, then verifies every file.
			for i, snap := range saved {
				s = open()
				before = counts.counts()
				started = time.Now()
				p, err = s.CreatePool(ctx, engine.PoolOptions{Base: snap.ID, Concurrency: 1})
				if err != nil {
					t.Fatal(err)
				}
				e, err = p.Acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				row := &results[len(results)-len(saved)+i]
				row.RestoreSeconds = time.Since(started).Seconds()
				row.RestoreTransfer = counts.counts().since(before)
				verifyArtifactRevision(t, filepath.Join(fixture, revisions[i]), e.Path)
				row.Verified = true
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	if t.Failed() {
		return
	}
	fixtureInfo, err := os.ReadFile(filepath.Join(fixture, "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	report := struct {
		Go           string                `json:"go"`
		Platform     string                `json:"platform"`
		Fixture      json.RawMessage       `json:"fixture"`
		Measurements []artifactMeasurement `json:"measurements"`
		Notes        string                `json:"notes"`
	}{runtime.Version(), runtime.GOOS + "/" + runtime.GOARCH, fixtureInfo, results,
		"One sequential trial per strategy. Includes hashing and durable publication. Restore includes verified acquire, excludes independent byte comparison. Local MinIO; no WAN latency or monetary cost model. Media-heavy fixtures, not a performance SLO."}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if out := os.Getenv("RAMET_ARTIFACT_REPORT"); out != "" {
		if err = os.WriteFile(out, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range results {
		t.Logf("%s %s: save %.3fs upload %d download %d; restore %.3fs; verified %t",
			row.Mode, row.Revision, row.CheckpointSeconds, row.SaveTransfer.Uploaded, row.SaveTransfer.Downloaded, row.RestoreSeconds, row.Verified)
	}
}

func copyArtifactRevision(t *testing.T, source, dest string) {
	t.Helper()
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		in, err := os.Open(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.Create(filepath.Join(dest, entry.Name()))
		if err != nil {
			in.Close()
			t.Fatal(err)
		}
		_, copyErr := io.Copy(out, in)
		inErr, outErr := in.Close(), out.Close()
		if copyErr != nil || inErr != nil || outErr != nil {
			t.Fatal(copyErr, inErr, outErr)
		}
	}
}

func verifyArtifactRevision(t *testing.T, expected, actual string) {
	t.Helper()
	files := func(dir string) map[string]string {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, entry := range entries {
			f, err := os.Open(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			h := sha256.New()
			_, err = io.Copy(h, f)
			closeErr := f.Close()
			if err != nil || closeErr != nil {
				t.Fatal(err, closeErr)
			}
			out[entry.Name()] = hex.EncodeToString(h.Sum(nil))
		}
		return out
	}
	want, got := files(expected), files(actual)
	if len(want) != len(got) {
		t.Fatal("restored namespace differs")
	}
	for name, hash := range want {
		if got[name] != hash {
			t.Fatal(fmt.Sprintf("restored %s differs", name))
		}
	}
}

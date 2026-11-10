// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func publicationOptions() PublishOptions {
	return PublishOptions{Publication: Publication{ID: strings.Repeat("a", 64),
		RequestDigest: strings.Repeat("b", 64), Result: json.RawMessage(`{"document":"report.txt"}`)}, Mode: "checkpoint"}
}

func TestPublicationReplayConflictAndRetention(t *testing.T) {
	ctx := context.Background()
	s, _, base := fixture(t)
	p, err := s.CreatePool(ctx, PoolOptions{Base: base.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	e, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(e.Path, "report.txt"), "saved work")
	opts := publicationOptions()
	first, err := p.Publish(ctx, e, opts)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(e.Path, "report.txt"), "later unsaved work")
	opts.Publication.Result = json.RawMessage(`"later result"`)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := p.Publish(ctx, e, opts)
			if err != nil || got.ID != first.ID {
				t.Errorf("replay differs: %s %v", got.ID, err)
			}
		}()
	}
	wg.Wait()
	for _, field := range []string{"digest", "mode"} {
		bad := opts
		if field == "digest" {
			bad.Publication.RequestDigest = strings.Repeat("c", 64)
		} else {
			bad.Mode = "promotion"
		}
		if _, err := p.Publish(ctx, e, bad); !errors.Is(err, ErrConflict) {
			t.Fatalf("conflict accepted: %v", err)
		}
	}
	if err := p.Release(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSnapshot(ctx, first.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("publication deleted", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(s.Root(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got, err := restored.GetPublication(ctx, opts.Publication.ID)
	if err != nil || got.ID != first.ID || string(got.Manifest.Publication.Result) != `{"document":"report.txt"}` {
		t.Fatal("restart lost result", got, err)
	}
	p2, err := restored.CreatePool(ctx, PoolOptions{Base: got.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	e2, err := p2.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(e2.Path, "report.txt")) != "saved work" {
		t.Fatal("files/result mismatch")
	}
	if _, err := p2.Publish(ctx, e2, opts); !errors.Is(err, ErrConflict) {
		t.Fatal("changed parent accepted", err)
	}
}

func TestPublicationValidationAndLease(t *testing.T) {
	ctx := context.Background()
	s, _, base := fixture(t)
	p, err := s.CreatePool(ctx, PoolOptions{Base: base.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	e, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"id", "digest", "result", "utf8", "size", "escaped_size", "mode"} {
		opts := publicationOptions()
		switch field {
		case "id":
			opts.Publication.ID = "../escape"
		case "digest":
			opts.Publication.RequestDigest = ""
		case "result":
			opts.Publication.Result = json.RawMessage("not JSON")
		case "utf8":
			opts.Publication.Result = json.RawMessage{'"', 0xff, '"'}
		case "size":
			opts.Publication.Result = json.RawMessage(`"` + strings.Repeat("x", 65536) + `"`)
		case "escaped_size":
			opts.Publication.Result = json.RawMessage(`"` + strings.Repeat("<", 20000) + `"`)
		case "mode":
			opts.Mode = "unknown"
		}
		if _, err := p.Publish(ctx, e, opts); !errors.Is(err, ErrInvalid) {
			t.Fatal(field, err)
		}
	}
	stale := e
	stale.Generation++
	if _, err := p.Publish(ctx, stale, publicationOptions()); err == nil {
		t.Fatal("stale lease accepted")
	}
	if _, err := s.GetPublication(ctx, publicationOptions().Publication.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	snaps, err := s.ListSnapshots(ctx)
	if err != nil || len(snaps) != 1 {
		t.Fatal("invalid attempt published", len(snaps), err)
	}
}

func TestPublicationCrashHelper(t *testing.T) {
	root := os.Getenv("RAMET_PUBLICATION_CRASH_ROOT")
	if root == "" {
		return
	}
	ctx := context.Background()
	s, err := Open(filepath.Join(root, "store"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source")
	write(t, filepath.Join(source, "report.txt"), "original")
	base, err := s.Snapshot(ctx, SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePool(ctx, PoolOptions{Base: base.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	e, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(e.Path, "report.txt"), "completed")
	if os.Getenv("RAMET_PUBLICATION_CRASH_PHASE") == "after" {
		if _, err := p.Publish(ctx, e, publicationOptions()); err != nil {
			t.Fatal(err)
		}
	}
	os.Exit(91) // No pool release, store close, or deferred cleanup.
}

func TestPublicationProcessDeath(t *testing.T) {
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestPublicationCrashHelper$")
			cmd.Env = append(os.Environ(), "RAMET_PUBLICATION_CRASH_ROOT="+root, "RAMET_PUBLICATION_CRASH_PHASE="+phase)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 91 {
				t.Fatalf("helper: %s %v", out, err)
			}
			s, err := Open(filepath.Join(root, "store"), Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			snap, err := s.GetPublication(context.Background(), publicationOptions().Publication.ID)
			if phase == "before" {
				if !errors.Is(err, ErrNotFound) {
					t.Fatal("unpublished result visible", err)
				}
			} else {
				if err != nil || snap.Manifest.Publication == nil {
					t.Fatal("published result lost", err)
				}
				p, err := s.CreatePool(context.Background(), PoolOptions{Base: snap.ID, Concurrency: 1})
				if err != nil {
					t.Fatal(err)
				}
				e, err := p.Acquire(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if read(t, filepath.Join(e.Path, "report.txt")) != "completed" {
					t.Fatal("wrong recovered bytes")
				}
			}
		})
	}
}

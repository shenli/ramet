// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func artifactBytes(size int) []byte {
	b := make([]byte, size)
	_, _ = rand.New(rand.NewSource(42)).Read(b)
	return b
}

func TestChunkedCheckpointLifecycle(t *testing.T) {
	ctx := context.Background()
	root, source := t.TempDir(), t.TempDir()
	s, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	data := artifactBytes(24 << 20)
	write(t, filepath.Join(source, "report.bin"), string(data))
	write(t, filepath.Join(source, "note.txt"), "small file")
	base, err := s.Snapshot(ctx, SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Manifest.Entries[0].Chunks) != 0 || len(base.Manifest.Entries[1].Chunks) < 3 {
		t.Fatal("small/large file representation", base.Manifest.Entries)
	}
	p := poolFor(t, s, base, 1)
	e := acquire(t, p)
	// Insertion changes offsets; CDC should recover boundaries in the unchanged suffix.
	changed := append(bytes.Clone(data[:1000]), []byte("inserted document content")...)
	changed = append(changed, data[1000:]...)
	write(t, filepath.Join(e.Path, "report.bin"), string(changed))
	saved, err := p.Checkpoint(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	oldRefs := map[string]bool{}
	for _, c := range base.Manifest.Entries[1].Chunks {
		oldRefs[c.Digest] = true
	}
	var reused int64
	for _, c := range saved.Manifest.Entries[1].Chunks {
		if oldRefs[c.Digest] {
			reused += c.Size
		}
	}
	if reused < int64(len(data)/2) {
		t.Fatalf("only reused %d of %d bytes", reused, len(data))
	}
	if _, err = os.Stat(filepath.Join(root, "objects", saved.Manifest.Entries[1].Digest)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("chunked file retained a redundant whole-file object", err)
	}
	if _, err = s.GC(ctx, 0, false); err != nil {
		t.Fatal(err)
	}
	e, err = p.Reset(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(e.Path, "report.bin")); got != string(data) {
		t.Fatal("reset changed base bytes")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	p = poolFor(t, s, saved, 1)
	e = acquire(t, p)
	if got := read(t, filepath.Join(e.Path, "report.bin")); got != string(changed) {
		t.Fatal("restart lost saved bytes")
	}
	// Large -> small -> empty must discard obsolete chunk lists.
	for _, value := range []string{"short", ""} {
		write(t, filepath.Join(e.Path, "report.bin"), value)
		snap, err := p.Checkpoint(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		entry := snap.Manifest.Entries[1]
		if len(entry.Chunks) != 0 || entry.Size != int64(len(value)) {
			t.Fatal("obsolete chunks after truncation")
		}
	}
}

func TestChunkCorruptionFailsRestore(t *testing.T) {
	for _, mode := range []string{"bytes", "missing", "order", "full_digest"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(t.TempDir(), Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			source := t.TempDir()
			write(t, filepath.Join(source, "file"), string(artifactBytes(12<<20)))
			snap, err := s.Snapshot(ctx, SnapshotOptions{Source: source})
			if err != nil {
				t.Fatal(err)
			}
			e := &snap.Manifest.Entries[0]
			name := filepath.Join(s.root, "objects", e.Chunks[0].Digest)
			switch mode {
			case "bytes":
				if err = os.Chmod(name, 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(name, []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err = os.Remove(name); err != nil {
					t.Fatal(err)
				}
			case "order":
				e.Chunks[0], e.Chunks[1] = e.Chunks[1], e.Chunks[0]
			case "full_digest":
				e.Digest = hashBytes([]byte("wrong full checksum"))
			}
			if _, err = s.materialize(ctx, snap); err == nil {
				t.Fatal("corruption exposed as a workspace")
			}
			staging, _ := os.ReadDir(filepath.Join(s.root, "staging"))
			executions, _ := os.ReadDir(filepath.Join(s.root, "executions"))
			status, err := s.CacheStatus()
			if err != nil || len(staging) != 0 || len(executions) != 0 || status.PinnedObjects != 0 || status.ReservedDiskBytes != 0 {
				t.Fatal("failed restore leaked resources", status, err)
			}
		})
	}
}

func TestChunkManifestValidation(t *testing.T) {
	digest := hashBytes([]byte("chunk"))
	valid := func() Manifest {
		return Manifest{Version: FormatVersion, Entries: []Entry{{Path: "file", Kind: "file", Size: 8 << 20, Digest: digest,
			Chunks: []ContentRef{{Digest: digest, Size: 4 << 20}, {Digest: digest, Size: 4 << 20}}}}}
	}
	for name, mutate := range map[string]func(*Manifest){
		"old_version": func(m *Manifest) { m.Version = 3 },
		"directory":   func(m *Manifest) { m.Entries[0].Kind = "dir" },
		"zero":        func(m *Manifest) { m.Entries[0].Chunks[0].Size = 0 },
		"negative":    func(m *Manifest) { m.Entries[0].Chunks[0].Size = -1 },
		"oversized":   func(m *Manifest) { m.Entries[0].Chunks[0].Size = 1<<63 - 1 },
		"bad_sum":     func(m *Manifest) { m.Entries[0].Size++ },
		"bad_digest":  func(m *Manifest) { m.Entries[0].Chunks[0].Digest = "bad" },
	} {
		t.Run(name, func(t *testing.T) {
			m := valid()
			mutate(&m)
			if validateManifest(m) == nil {
				t.Fatal("accepted invalid chunks")
			}
		})
	}
	if err := validateManifest(valid()); err != nil {
		t.Fatal(err)
	}
}

func TestChunkGCAndCancellation(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	source := t.TempDir()
	write(t, filepath.Join(source, "file"), string(make([]byte, 8<<20)))
	snap, err := s.Snapshot(ctx, SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := s.GC(ctx, 0, false); err != nil || result.Objects != 0 {
		t.Fatal("collected referenced chunks", result, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.materialize(canceled, snap); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err = s.DeleteSnapshot(ctx, snap.ID); err != nil {
		t.Fatal(err)
	}
	if result, err := s.GC(ctx, 0, false); err != nil || result.Objects == 0 {
		t.Fatal("orphan chunks not collected", result, err)
	}
}

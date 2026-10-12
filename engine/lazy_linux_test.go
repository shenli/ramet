//go:build linux

// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLazyMappedWritesAndResetIsolation(t *testing.T) {
	if os.Getenv("RAMET_TEST_FUSE") != "1" {
		t.Skip("requires Linux FUSE")
	}
	ctx := context.Background()
	source := t.TempDir()
	write(t, filepath.Join(source, "file"), "original")
	s, err := Open(t.TempDir(), Options{Backend: "lazy"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base, err := s.Snapshot(ctx, SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	p := poolFor(t, s, base, 2)
	a, b := acquire(t, p), acquire(t, p)
	file := filepath.Join(a.Path, "file")
	f, err := os.OpenFile(file, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := syscall.Mmap(int(f.Fd()), 0, 8, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	copy(mapped, "modified")
	if err = syscall.Munmap(mapped); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Sync(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	assertContents := func(name, want string) {
		t.Helper()
		data, err := os.ReadFile(name)
		if err != nil || string(data) != want {
			t.Fatalf("%s: got %q, want %q: %v", name, data, want, err)
		}
	}
	assertContents(file, "modified")
	assertContents(filepath.Join(b.Path, "file"), "original")
	assertContents(filepath.Join(source, "file"), "original")
	// Extending a file must expose zeroes, not stale backing/cache bytes.
	if err = os.Truncate(file, 12); err != nil {
		t.Fatal(err)
	}
	assertContents(file, "modified\x00\x00\x00\x00")
	if err = os.Chmod(file, 0400); err != nil {
		t.Fatal(err)
	}
	saved, err := p.Promote(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	restored := acquire(t, poolFor(t, s, saved, 1))
	assertContents(filepath.Join(restored.Path, "file"), "modified\x00\x00\x00\x00")
	info, err := os.Stat(filepath.Join(restored.Path, "file"))
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatal("promoted permissions lost", info, err)
	}
	a, err = p.Reset(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	assertContents(filepath.Join(a.Path, "file"), "original")
	verified, err := p.Verify(ctx, a)
	if err != nil || !verified.Valid {
		t.Fatal(verified, err)
	}
}

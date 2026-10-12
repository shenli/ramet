//go:build linux

// SPDX-License-Identifier: Apache-2.0
package lazyfs

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestHydrationSurvivesInterruptedKernelRequest(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "file")
	if err := os.WriteFile(name, make([]byte, 4), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	ino := info.Sys().(*syscall.Stat_t).Ino
	mountCtx, cancelMount := context.WithCancel(context.Background())
	defer cancelMount()
	m := &Mount{root: root, ctx: mountCtx, pending: map[uint64]File{ino: {Path: "file", Size: 4, Mode: 0644}}, hydrate: func(ctx context.Context, e File, f *os.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := f.WriteAt([]byte("data"), 0)
		return err
	}}
	n := &node{mount: m}
	interrupted, cancel := context.WithCancel(context.Background())
	cancel()
	if errno := n.ensure(interrupted, "file"); errno != 0 {
		t.Fatalf("kernel signal aborted inode hydration: %v", errno)
	}
	b, err := os.ReadFile(name)
	if err != nil || string(b) != "data" {
		t.Fatal(string(b), err)
	}
	if len(m.pending) != 0 {
		t.Fatal("hydrated inode still pending")
	}
}

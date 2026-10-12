// SPDX-License-Identifier: Apache-2.0
package fsutil

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyProducesIndependentWritableInode(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "copy")
	if err := os.WriteFile(src, []byte("immutable"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyFile(context.Background(), src, dst, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(src)
	if err != nil || string(b) != "immutable" {
		t.Fatal("writable inode was shared", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CopyFile(ctx, src, filepath.Join(dir, "cancelled"), false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRemoveTreeDoesNotFollowSymlinks(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	protected := filepath.Join(outside, "keep")
	if err := os.WriteFile(protected, []byte("keep"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTree(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(protected); err != nil {
		t.Fatal("cleanup escaped execution root", err)
	}
}

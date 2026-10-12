// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLazyFilesystem(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("RAMET_TEST_FUSE") != "1" {
		t.Skip("requires RAMET_TEST_FUSE=1 on Linux with FUSE")
	}
	ctx := context.Background()
	source := t.TempDir()
	payload := bytes.Repeat([]byte("lazy verified content\n"), 4096)
	for _, name := range []string{"read", "rename", "truncate", "unlink", "link", "readonly"} {
		if err := os.WriteFile(filepath.Join(source, name), payload, 0644); err != nil {
			t.Fatal(err)
		}
	}
	os.Chmod(filepath.Join(source, "readonly"), 0444)
	stamp := time.Unix(1730000000, 123456789)
	os.Chtimes(filepath.Join(source, "read"), stamp, stamp)
	s, err := Open(t.TempDir(), Options{Backend: "lazy"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap, err := s.Snapshot(ctx, SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	p := poolFor(t, s, snap, 2)
	e, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Stats().CopiedFiles != 0 {
		t.Fatal("acquire eagerly read file contents")
	}
	info, err := os.Stat(filepath.Join(e.Path, "read"))
	if err != nil || info.Size() != int64(len(payload)) || info.ModTime() != stamp {
		t.Fatal(info, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := os.ReadFile(filepath.Join(e.Path, "read"))
			if err != nil || !bytes.Equal(b, payload) {
				t.Errorf("concurrent read: %v", err)
			}
		}()
	}
	wg.Wait()
	if s.Stats().CopiedFiles != 1 {
		t.Fatal("first read not coalesced", s.Stats())
	}
	if err = os.Rename(filepath.Join(e.Path, "rename"), filepath.Join(e.Path, "renamed")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(e.Path, "renamed"))
	if err != nil || !bytes.Equal(b, payload) {
		t.Fatal("rename lost pending content", err)
	}
	if err = os.Link(filepath.Join(e.Path, "link"), filepath.Join(e.Path, "linked")); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(e.Path, "link")); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(filepath.Join(e.Path, "linked"))
	if err != nil || !bytes.Equal(b, payload) {
		t.Fatal("link lost content", err)
	}
	if err = os.Truncate(filepath.Join(e.Path, "truncate"), 3); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(filepath.Join(e.Path, "truncate"))
	if err != nil || !bytes.Equal(b, payload[:3]) {
		t.Fatal("truncate", err)
	}
	before := s.Stats().CopiedFiles
	if err = os.Remove(filepath.Join(e.Path, "unlink")); err != nil {
		t.Fatal(err)
	}
	if s.Stats().CopiedFiles != before {
		t.Fatal("unlink hydrated file")
	}
	b, err = os.ReadFile(filepath.Join(e.Path, "readonly"))
	if err != nil || !bytes.Equal(b, payload) {
		t.Fatal("readonly", err)
	}
	e, err = p.Reset(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.Verify(ctx, e)
	if err != nil || !v.Valid {
		t.Fatal(v, err)
	}
	promoted, err := p.Promote(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if len(promoted.Manifest.Entries) != len(snap.Manifest.Entries) {
		t.Fatal("promotion lost entries")
	}
	if err = p.Release(ctx, e); err != nil {
		t.Fatal(err)
	}
	before = s.Stats().CopiedFiles
	e, err = p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Stats().CopiedFiles != before {
		t.Fatal("idle acquire eagerly hydrated")
	}
	if err = p.Discard(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(s.root, "executions"))
	if err != nil || len(entries) != 0 {
		t.Fatal("mounts leaked", entries, err)
	}
}

func TestLazyGitObjects(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("RAMET_TEST_FUSE") != "1" {
		t.Skip("requires Linux FUSE")
	}
	ctx := context.Background()
	source := t.TempDir()
	write(t, filepath.Join(source, "file"), "base")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid"}, args...)...)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, b, err)
		}
		return strings.TrimSpace(string(b))
	}
	git(source, "init", "--initial-branch=main")
	git(source, "add", ".")
	git(source, "commit", "-m", "base")
	s, err := Open(t.TempDir(), Options{Backend: "lazy"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base, err := s.Snapshot(ctx, SnapshotOptions{Git: &GitBase{Repository: source, Revision: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	p := poolFor(t, s, base, 1)
	e := acquire(t, p)
	if git(e.Path, "status", "--porcelain") != "" {
		t.Fatal("lazy Git import is dirty")
	}
	write(t, filepath.Join(e.Path, "file"), "selected")
	git(e.Path, "add", ".")
	git(e.Path, "commit", "-m", "local")
	selected, err := p.Promote(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Manifest.GitObjects.LocalPack == nil {
		t.Fatal("local Git objects not retained")
	}
	restored := acquire(t, poolFor(t, s, selected, 1))
	if git(restored.Path, "status", "--porcelain") != "" {
		t.Fatal("restored lazy Git state is dirty")
	}
	b, err := os.ReadFile(filepath.Join(restored.Path, "file"))
	if err != nil || string(b) != "selected" {
		t.Fatal(string(b), err)
	}
}

func TestLazyPersistencePreview(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("RAMET_TEST_FUSE") != "1" {
		t.Skip("requires Linux FUSE")
	}
	ctx := context.Background()
	source := t.TempDir()
	write(t, filepath.Join(source, "lock"), "pinned")
	write(t, filepath.Join(source, "generated/file"), "rebuild me")
	write(t, filepath.Join(source, "scratch/file"), "discard me")
	s, err := Open(t.TempDir(), Options{Backend: "lazy"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap, err := s.Snapshot(ctx, SnapshotOptions{Source: source, Policy: Policy{Rules: []Rule{
		{Path: "generated", Class: Rebuildable, Recipe: "build", Inputs: []string{"lock"}},
		{Path: "scratch", Class: Ephemeral},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	p := poolFor(t, s, snap, 1)
	e := acquire(t, p)
	before := s.Stats()
	preview, err := p.PreviewPromotion(ctx, e)
	if err != nil || !preview.Valid {
		t.Fatal(preview, err)
	}
	if s.Stats().ContentBytesWritten != before.ContentBytesWritten || s.Stats().HydratedFiles-before.HydratedFiles != 1 {
		t.Fatalf("preview hydrated omitted files or wrote content: %+v", s.Stats())
	}
	saved, err := p.Promote(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	restored := poolFor(t, s, saved, 1)
	r := acquire(t, restored)
	plan, err := restored.RestorePlan(ctx, r)
	if err != nil || !plan.Valid || len(plan.Steps) != 1 {
		t.Fatal(plan, err)
	}
}

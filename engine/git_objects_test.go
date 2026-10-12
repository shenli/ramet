// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitProvenanceOfflineRepackAndCorruption(t *testing.T) {
	ctx := context.Background()
	s, source, _ := fixture(t)
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, b, err)
		}
		return strings.TrimSpace(string(b))
	}
	git(source, "init", "--initial-branch=main")
	git(source, "add", ".")
	git(source, "commit", "-m", "base")
	base, err := s.Snapshot(ctx, SnapshotOptions{Git: &GitBase{Repository: source, Revision: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	if base.Manifest.GitObjects == nil || base.Manifest.GitObjects.LocalPack != nil {
		t.Fatal("missing optimized base")
	}
	p := poolFor(t, s, base, 2)
	e := acquire(t, p)
	packs, err := filepath.Glob(filepath.Join(e.Path, ".git", "objects", "pack", "*.pack"))
	if err != nil || len(packs) != 1 {
		t.Fatal(packs, err)
	}
	packInfo, err := os.Stat(packs[0])
	if err != nil {
		t.Fatal(err)
	}
	baseInfo, err := os.Stat(filepath.Join(s.root, "objects", base.Manifest.GitObjects.BasePack.Digest))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(packInfo, baseInfo) {
		t.Fatal("Git pack shares a writable inode with CAS")
	}
	packDigest, _, err := digestFile(ctx, packs[0])
	if err != nil || packDigest != base.Manifest.GitObjects.BasePack.Digest {
		t.Fatal("Git pack bytes changed", err)
	}
	write(t, filepath.Join(e.Path, "src/main.txt"), "local commit")
	git(e.Path, "add", ".")
	git(e.Path, "commit", "-m", "local")
	git(e.Path, "branch", "local-only")
	write(t, filepath.Join(e.Path, "src/main.txt"), "staged")
	git(e.Path, "add", ".")
	write(t, filepath.Join(e.Path, "src/main.txt"), "unstaged")
	dangling := git(e.Path, "hash-object", "-w", filepath.Join(e.Path, "src/main.txt"))
	status, index, reflog := git(e.Path, "status", "--porcelain"), git(e.Path, "ls-files", "--stage"), git(e.Path, "reflog", "--format=%H")
	snap, err := p.Promote(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	g := snap.Manifest.GitObjects
	if g.LocalPack == nil || g.BasePack != base.Manifest.GitObjects.BasePack {
		t.Fatal("base pack was not reused")
	}
	git(e.Path, "repack", "-a", "-d")
	again, err := p.Promote(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if *again.Manifest.GitObjects.LocalPack != *g.LocalPack {
		t.Fatal("physical repacking changed local object content")
	}
	if err = os.Rename(source, source+"-offline"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(source+"-offline", source)
	restored := acquire(t, poolFor(t, s, snap, 1))
	if git(restored.Path, "status", "--porcelain") != status || git(restored.Path, "ls-files", "--stage") != index || git(restored.Path, "reflog", "--format=%H") != reflog {
		t.Fatal("Git status/index/reflog changed")
	}
	if git(restored.Path, "cat-file", "-p", dangling) != "unstaged" {
		t.Fatal("dangling object lost")
	}
	git(restored.Path, "show-ref", "--verify", "refs/heads/local-only")
	// A corrupt base pack must fail restore, never return a partially valid tree.
	pack := filepath.Join(s.root, "objects", g.BasePack.Digest)
	if err = os.Chmod(pack, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(pack, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = poolFor(t, s, snap, 1).Acquire(ctx); err == nil {
		t.Fatal("corrupt base pack accepted")
	}
}

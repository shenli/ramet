// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixture(t testing.TB) (*Store, string, Snapshot) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	source := t.TempDir()
	write(t, filepath.Join(source, "src/main.txt"), "base\n")
	write(t, filepath.Join(source, "run.sh"), "#!/bin/sh\necho hi\n")
	if err := os.Chmod(filepath.Join(source, "run.sh"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("src/main.txt", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot(context.Background(), SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	return s, source, snap
}
func write(t testing.TB, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
func read(t testing.TB, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func poolFor(t testing.TB, s *Store, snap Snapshot, n int) *Pool {
	t.Helper()
	p, err := s.CreatePool(context.Background(), PoolOptions{Base: snap.ID, Concurrency: n})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func acquire(t testing.TB, p *Pool) Execution {
	t.Helper()
	e, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestIsolationResetAndLeaseFencing(t *testing.T) {
	s, source, snap := fixture(t)
	p := poolFor(t, s, snap, 2)
	a, b := acquire(t, p), acquire(t, p)
	ctx := context.Background()
	oldPath := a.Path
	f, err := os.OpenFile(filepath.Join(a.Path, "src/main.txt"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	write(t, filepath.Join(a.Path, "src/main.txt"), "changed\n")
	write(t, filepath.Join(a.Path, "extra"), "dirty")
	if read(t, filepath.Join(b.Path, "src/main.txt")) != "base\n" || read(t, filepath.Join(source, "src/main.txt")) != "base\n" {
		t.Fatal("cross-execution mutation")
	}
	v, err := p.Verify(ctx, a)
	if err != nil || v.Valid {
		t.Fatalf("dirty verification: %+v %v", v, err)
	}
	reset, err := p.Reset(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if reset.Path == oldPath || reset.Generation == a.Generation {
		t.Fatal("reset reused directory or generation")
	}
	if _, err = p.Reset(ctx, a); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale reset accepted: %v", err)
	}
	if _, err = f.WriteAt([]byte("stale"), 0); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(reset.Path, "src/main.txt")) != "base\n" {
		t.Fatal("old handle contaminated replacement")
	}
	if v, err = p.Verify(ctx, reset); err != nil || !v.Valid {
		t.Fatalf("reset verification: %+v %v", v, err)
	}
	if err = p.Release(ctx, reset); err != nil {
		t.Fatal(err)
	}
	claimed := acquire(t, p)
	if claimed.ID != reset.ID || claimed.Lease == reset.Lease {
		t.Fatal("recycle must retain slot and rotate lease")
	}
	if err = p.Release(ctx, reset); !errors.Is(err, ErrConflict) {
		t.Fatalf("old owner released new claim: %v", err)
	}
	if err = p.Discard(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err = p.Discard(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	if stats := s.Stats(); stats.ActiveExecutions != 0 || stats.IdleExecutions != 0 || stats.Pools != 0 {
		t.Fatalf("leaked state: %+v", stats)
	}
}

func TestForkIsLocalAndIsolated(t *testing.T) {
	s, _, snap := fixture(t)
	p := poolFor(t, s, snap, 2)
	a := acquire(t, p)
	write(t, filepath.Join(a.Path, "src/main.txt"), "parent")
	before := s.Stats().ContentBytesWritten
	b, err := p.Fork(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(b.Path, "src/main.txt")) != "parent" {
		t.Fatal("fork lost delta")
	}
	write(t, filepath.Join(b.Path, "src/main.txt"), "child")
	if read(t, filepath.Join(a.Path, "src/main.txt")) != "parent" {
		t.Fatal("fork shares writable inode")
	}
	if s.Stats().ContentBytesWritten != before {
		t.Fatal("local fork wrote durable content")
	}
	if _, err = p.Fork(context.Background(), a); !errors.Is(err, ErrConflict) {
		t.Fatal("fork exceeded capacity")
	}
}

func TestPromotionPolicyRestartAndGC(t *testing.T) {
	s, source, _ := fixture(t)
	ctx := context.Background()
	write(t, filepath.Join(source, "cache/base"), "rebuild me")
	write(t, filepath.Join(source, "tmp/base"), "ephemeral")
	write(t, filepath.Join(source, "toolchain"), "immutable")
	policy := Policy{Rules: []Rule{{Path: "cache", Class: Rebuildable, Recipe: "make cache"}, {Path: "tmp", Class: Ephemeral}, {Path: "toolchain", Class: SharedImmutable}}}
	base, err := s.Snapshot(ctx, SnapshotOptions{Source: source, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	p := poolFor(t, s, base, 1)
	e := acquire(t, p)
	write(t, filepath.Join(e.Path, "src/main.txt"), "durable change")
	write(t, filepath.Join(e.Path, ".git/objects/local"), "local-only Git state")
	promoted, err := p.Promote(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	again, err := p.Promote(ctx, e)
	if err != nil || again.ID != promoted.ID {
		t.Fatalf("promotion not idempotent: %v", err)
	}
	for _, entry := range promoted.Manifest.Entries {
		if strings.HasPrefix(entry.Path, "cache") || strings.HasPrefix(entry.Path, "tmp") {
			t.Fatal("persisted excluded path", entry.Path)
		}
	}
	if promoted.Manifest.Parent != base.ID || len(promoted.Manifest.Policy.Rules) != 3 {
		t.Fatal("lost provenance")
	}
	write(t, filepath.Join(e.Path, "toolchain"), "changed")
	if _, err = p.Promote(ctx, e); !errors.Is(err, ErrConflict) {
		t.Fatalf("immutable mutation accepted: %v", err)
	}
	if err = s.DeleteSnapshot(ctx, base.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleted pinned base: %v", err)
	}
	root := s.Root()
	old := e.Path
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err = os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("restart retained ephemeral execution")
	}
	p2 := poolFor(t, s2, promoted, 1)
	e2 := acquire(t, p2)
	if read(t, filepath.Join(e2.Path, "src/main.txt")) != "durable change" {
		t.Fatal("resume lost delta")
	}
	if read(t, filepath.Join(e2.Path, ".git/objects/local")) != "local-only Git state" {
		t.Fatal("resume lost local Git objects")
	}
	r, err := s2.GC(ctx, 0, true)
	if err != nil || r.Objects == 0 {
		t.Fatalf("expected orphan content after failed promotion: %+v %v", r, err)
	}
	if _, err = s2.GC(ctx, 0, false); err != nil {
		t.Fatal(err)
	}
	d, err := s2.Doctor(ctx)
	if err != nil || !d.OK {
		t.Fatalf("GC damaged live state: %+v %v", d, err)
	}
}

func TestCorruptBaseQuarantinesReset(t *testing.T) {
	s, _, snap := fixture(t)
	p := poolFor(t, s, snap, 1)
	e := acquire(t, p)
	var digest string
	for _, entry := range snap.Manifest.Entries {
		if entry.Kind == "file" {
			digest = entry.Digest
			break
		}
	}
	name := filepath.Join(s.Root(), "objects", digest)
	if err := os.Chmod(name, 0600); err != nil {
		t.Fatal(err)
	}
	write(t, name, "corrupt")
	if _, err := p.Reset(context.Background(), e); !errors.Is(err, ErrVerification) {
		t.Fatalf("reset accepted corrupt bytes: %v", err)
	}
	stats := s.Stats()
	if stats.Quarantines != 1 || stats.ActiveExecutions != 0 || stats.IdleExecutions != 0 {
		t.Fatalf("quarantine failed: %+v", stats)
	}
	if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrVerification) {
		t.Fatalf("acquire accepted corrupt base: %v", err)
	}
	report, err := s.Doctor(context.Background())
	if err != nil || report.OK {
		t.Fatalf("doctor missed corruption: %+v %v", report, err)
	}
}

func TestCapacityCancellationDrainAndClose(t *testing.T) {
	s, _, snap := fixture(t)
	p := poolFor(t, s, snap, 1)
	e := acquire(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire did not time out: %v", err)
	}
	if err := p.Close(); !errors.Is(err, ErrConflict) {
		t.Fatal("closed occupied pool")
	}
	done := make(chan error, 1)
	go func() { _, err := p.Acquire(context.Background()); done <- err }()
	if err := s.Drain(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrDraining) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("drain did not wake waiter")
	}
	if err := p.Release(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestConcurrentRecycleSoak(t *testing.T) {
	s, _, snap := fixture(t)
	p := poolFor(t, s, snap, 4)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for worker := 0; worker < 12; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 12; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				e, err := p.Acquire(ctx)
				if err == nil {
					b, readErr := os.ReadFile(filepath.Join(e.Path, "src/main.txt"))
					if readErr != nil || string(b) != "base\n" {
						err = fmt.Errorf("contamination: %s %v", b, readErr)
					}
					if err == nil {
						err = os.WriteFile(filepath.Join(e.Path, fmt.Sprintf("worker-%d", worker)), []byte("dirty"), 0600)
					}
					if err == nil {
						err = p.Release(ctx, e)
					}
				}
				cancel()
				if err != nil {
					errs <- err
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(s.Root(), "executions"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("execution leak: %d %v", len(entries), err)
	}
	if s.Stats().Recycles != 144 {
		t.Fatal("missing recycle operations")
	}
}

func TestStoreLockFormatAndRecovery(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Open(root, Options{}); err == nil {
		other.Close()
		t.Fatal("two owners opened store")
	}
	write(t, filepath.Join(root, "staging/interrupted"), "partial")
	write(t, filepath.Join(root, "executions/orphan/file"), "partial")
	write(t, filepath.Join(root, "snapshots/.pending-interrupted"), "partial")
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"staging", "executions", "snapshots"} {
		entries, err := os.ReadDir(filepath.Join(root, name))
		if err != nil || len(entries) != 0 {
			t.Fatal("recovery failed", name)
		}
	}
	s.Close()
	write(t, filepath.Join(root, "FORMAT"), "99\n")
	if newer, err := Open(root, Options{}); err == nil {
		newer.Close()
		t.Fatal("accepted newer format")
	}
}

func TestPathAndPolicyValidation(t *testing.T) {
	for _, p := range []string{"", ".", "..", "../escape", "/absolute", "a/../../x", "a//b", "a\\b", "x\x00"} {
		if validPath(p) {
			t.Errorf("accepted %q", p)
		}
	}
	for _, rules := range [][]Rule{{{Path: ".git", Class: Ephemeral}}, {{Path: "build", Class: Rebuildable}}, {{Path: "../x", Class: Durable}}, {{Path: "x", Class: "typo"}}} {
		if err := (Policy{Rules: rules}).Validate(); err == nil {
			t.Fatal("invalid policy accepted", rules)
		}
	}
	s, source, _ := fixture(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(context.Background(), SnapshotOptions{Source: source}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("escaping symlink accepted: %v", err)
	}
	if _, err := s.GetSnapshot(context.Background(), "../FORMAT"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(context.Background(), SnapshotOptions{Source: s.Root()}); !errors.Is(err, ErrInvalid) {
		t.Fatal("snapshot accepted own store")
	}
	m := Manifest{Version: 1, Entries: []Entry{{Path: "a", Kind: "symlink", Target: "."}, {Path: "a/file", Kind: "file", Digest: strings.Repeat("0", 64)}}}
	if err := validateManifest(m); err == nil {
		t.Fatal("accepted file under symlink")
	}
}

func TestGitBaseAndLocalCommitSurvive(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	s, source, _ := fixture(t)
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Ramet Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Ramet Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(source, "init", "--initial-branch=main")
	git(source, "add", ".")
	git(source, "commit", "-m", "base")
	base, err := s.Snapshot(context.Background(), SnapshotOptions{Git: &GitBase{Repository: source, Revision: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	if base.Manifest.Git.Revision != git(source, "rev-parse", "HEAD") {
		t.Fatal("revision not pinned")
	}
	p := poolFor(t, s, base, 1)
	e := acquire(t, p)
	write(t, filepath.Join(e.Path, "src/main.txt"), "local commit")
	git(e.Path, "add", ".")
	git(e.Path, "commit", "-m", "local")
	commit := git(e.Path, "rev-parse", "HEAD")
	snap, err := p.Promote(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	p2 := poolFor(t, s, snap, 1)
	restored := acquire(t, p2)
	if git(restored.Path, "rev-parse", "HEAD") != commit || git(restored.Path, "status", "--porcelain") != "" {
		t.Fatal("local Git history or index lost")
	}
}

func TestConcurrentSnapshotDedupAndGC(t *testing.T) {
	s, source, base := fixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 9)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := s.Snapshot(ctx, SnapshotOptions{Source: source})
			if err != nil {
				errs <- err
				return
			}
			if snap.ID != base.ID {
				errs <- fmt.Errorf("same quiesced source produced a different snapshot")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := s.GC(ctx, 0, false)
		if err != nil {
			errs <- err
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	report, err := s.Doctor(ctx)
	if err != nil || !report.OK {
		t.Fatalf("concurrent capture/GC damaged content: %+v %v", report, err)
	}
	var total uint64
	for _, e := range base.Manifest.Entries {
		total += uint64(e.Size)
	}
	if s.Stats().ContentBytesWritten != total {
		t.Fatal("deduplication published duplicate content")
	}
}

func TestPublicationFailureAndCorruptManifestGCFailsClosed(t *testing.T) {
	s, source, _ := fixture(t)
	ctx := context.Background()
	write(t, filepath.Join(source, "unique"), "new uncommitted content")
	dir := filepath.Join(s.Root(), "snapshots")
	if err := os.Rename(dir, dir+".saved"); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "metadata unavailable")
	if _, err := s.Snapshot(ctx, SnapshotOptions{Source: source}); err == nil {
		t.Fatal("acknowledged failed publication")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir+".saved", dir); err != nil {
		t.Fatal(err)
	}
	snaps, err := s.ListSnapshots(ctx)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("partial snapshot published: %v %v", len(snaps), err)
	}
	orphans, err := s.GC(ctx, 0, true)
	if err != nil || orphans.Objects != 1 {
		t.Fatalf("expected safe orphan: %+v %v", orphans, err)
	}
	name := filepath.Join(dir, snaps[0].ID+".json")
	write(t, name, "corrupt manifest")
	if _, err = s.GC(ctx, 0, false); !errors.Is(err, ErrVerification) {
		t.Fatalf("GC accepted unreadable metadata: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(s.Root(), "objects"))
	if err != nil || len(entries) != 3 {
		t.Fatalf("GC removed objects before completing mark: %d %v", len(entries), err)
	}
}

func TestSelectivePromotionRetainsDurableDescendantsAndDeletes(t *testing.T) {
	s, source, _ := fixture(t)
	ctx := context.Background()
	write(t, filepath.Join(source, "cache/retained/base"), "keep")
	write(t, filepath.Join(source, "cache/generated"), "omit")
	base, err := s.Snapshot(ctx, SnapshotOptions{Source: source, Policy: Policy{Rules: []Rule{
		{Path: "cache", Class: Rebuildable, Recipe: "build cache"},
		{Path: "cache/retained", Class: Durable},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	p := poolFor(t, s, base, 1)
	e := acquire(t, p)
	if err = os.Remove(filepath.Join(e.Path, "src/main.txt")); err != nil {
		t.Fatal(err)
	}
	snap, err := p.Promote(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	p2 := poolFor(t, s, snap, 1)
	resumed := acquire(t, p2)
	if read(t, filepath.Join(resumed.Path, "cache/retained/base")) != "keep" {
		t.Fatal("lost durable child")
	}
	for _, name := range []string{"cache/generated", "src/main.txt"} {
		if _, err = os.Stat(filepath.Join(resumed.Path, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("restored excluded or deleted path", name)
		}
	}
}

func TestCloseWakesBlockedAcquire(t *testing.T) {
	s, _, snap := fixture(t)
	p := poolFor(t, s, snap, 1)
	_ = acquire(t, p)
	done := make(chan error, 1)
	go func() { _, err := p.Acquire(context.Background()); done <- err }()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close stranded waiter")
	}
	if s.Ready() {
		t.Fatal("closed store reports ready")
	}
}

func TestContainedParentSymlinksAndChainedEscapes(t *testing.T) {
	s, source, _ := fixture(t)
	ctx := context.Background()
	write(t, filepath.Join(source, "node_modules/tool/bin/main"), "tool")
	if err := os.MkdirAll(filepath.Join(source, "node_modules/.bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../tool/bin/main", filepath.Join(source, "node_modules/.bin/tool")); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot(ctx, SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	e := acquire(t, poolFor(t, s, snap, 1))
	if read(t, filepath.Join(e.Path, "node_modules/.bin/tool")) != "tool" {
		t.Fatal("package-manager symlink was not restored")
	}
	if err = os.Mkdir(filepath.Join(source, "d"), 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("..", filepath.Join(source, "d/up")); err != nil {
		t.Fatal(err)
	}
	// Lexically d/up/../outside is within root; following up first escapes it.
	if err = os.Symlink("up/../outside", filepath.Join(source, "d/escape")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Snapshot(ctx, SnapshotOptions{Source: source}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("chained escape accepted: %v", err)
	}
}

func BenchmarkAcquireRecycle(b *testing.B) {
	s, _, snap := fixture(b)
	p := poolFor(b, s, snap, 16)
	ctx := context.Background()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			e, err := p.Acquire(ctx)
			if err != nil {
				b.Error(err)
				return
			}
			if err = p.Release(ctx, e); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func FuzzManifestPaths(f *testing.F) {
	f.Add("src/a", "file")
	f.Add("../escape", "dir")
	f.Add("a/b", "symlink")
	f.Add("file-\xff", "file")
	f.Fuzz(func(t *testing.T, name, kind string) {
		e := Entry{Path: name, Kind: kind, Digest: strings.Repeat("a", 64)}
		if kind == "dir" {
			e.Digest = ""
		}
		if kind == "symlink" {
			e.Digest = ""
			e.Target = "target"
		}
		m := Manifest{Version: 1, Entries: []Entry{e}}
		err := validateManifest(m)
		if err == nil && !validPath(name) {
			t.Fatalf("accepted unsafe path %q", name)
		}
		if err == nil {
			data, marshalErr := json.Marshal(m)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			var restored Manifest
			if err := json.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			if len(restored.Entries) != 1 || !reflect.DeepEqual(restored.Entries[0], e) {
				t.Fatalf("accepted entry changed during JSON round trip: %q", name)
			}
		}
	})
}

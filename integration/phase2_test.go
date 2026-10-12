// SPDX-License-Identifier: Apache-2.0
package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/shenli/ramet/engine"
	"github.com/shenli/ramet/storage"
)

type failingReadContent struct {
	storage.Content
	fail atomic.Bool
}

func (c *failingReadContent) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	if c.fail.Load() {
		return nil, fmt.Errorf("injected provider read outage")
	}
	return c.Content.Open(ctx, id)
}

func TestCachePressureAndLazyHydration(t *testing.T) {
	for _, backend := range []string{"copy", "lazy"} {
		t.Run(backend, func(t *testing.T) {
			if backend == "lazy" && (runtime.GOOS != "linux" || os.Getenv("RAMET_TEST_FUSE") != "1") {
				t.Skip("requires Linux FUSE")
			}
			ctx, ns, content := setup(t)
			remote := &failingReadContent{Content: content}
			s, err := engine.OpenContext(ctx, t.TempDir(), engine.Options{Metadata: metadata(t, ctx, ns), Content: remote, Backend: backend, CacheBytes: 4 * 65536, MaxIdleExecutions: 2})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			source := t.TempDir()
			for i := 0; i < 16; i++ {
				if err = os.WriteFile(filepath.Join(source, fmt.Sprintf("file-%02d", i)), bytes.Repeat([]byte{byte(i)}, 65536), 0644); err != nil {
					t.Fatal(err)
				}
			}
			snap, err := s.Snapshot(ctx, engine.SnapshotOptions{Source: source})
			if err != nil {
				t.Fatal(err)
			}
			status, err := s.CacheStatus()
			if err != nil || status.Bytes > status.LimitBytes || status.Evictions == 0 {
				t.Fatal(status, err)
			}
			p, err := s.CreatePool(ctx, engine.PoolOptions{Base: snap.ID, Concurrency: 2})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if _, err = s.TrimCache(ctx, 0); err != nil {
				t.Fatal(err)
			}
			before := s.Stats().RemoteBytesDownloaded
			e, err := p.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if backend == "lazy" {
				if s.Stats().RemoteBytesDownloaded != before {
					t.Fatal("lazy acquire fetched file bytes")
				}
				remote.fail.Store(true)
				if _, err = os.ReadFile(filepath.Join(e.Path, "file-00")); err == nil {
					t.Fatal("outage returned placeholder zeros")
				}
				remote.fail.Store(false)
			}
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					b, err := os.ReadFile(filepath.Join(e.Path, "file-00"))
					if err != nil || !bytes.Equal(b, make([]byte, 65536)) {
						t.Errorf("read: %v", err)
					}
				}()
			}
			wg.Wait()
			if backend == "lazy" && s.Stats().RemoteBytesDownloaded-before != 65536 {
				t.Fatal("first reads were not coalesced")
			}
			// Concurrent prefetch, trimming and reading must preserve active bytes. A
			// trim can report pinned pressure; it must never invalidate the reader.
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 8; i++ {
					_, _ = s.TrimCache(ctx, 0)
				}
			}()
			for i := 0; i < 16; i++ {
				b, err := os.ReadFile(filepath.Join(e.Path, fmt.Sprintf("file-%02d", i)))
				if err != nil || !bytes.Equal(b, bytes.Repeat([]byte{byte(i)}, 65536)) {
					t.Fatal("cache churn changed content", i, err)
				}
			}
			wg.Wait()
			if err = os.WriteFile(filepath.Join(e.Path, "file-00"), []byte("private write"), 0644); err != nil {
				t.Fatal(err)
			}
			promoted, err := p.Promote(ctx, e)
			if err != nil {
				t.Fatal(err)
			}
			p2, err := s.CreatePool(ctx, engine.PoolOptions{Base: promoted.ID, Concurrency: 1})
			if err != nil {
				t.Fatal(err)
			}
			restored, err := p2.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(restored.Path, "file-00"))
			if err != nil || string(b) != "private write" {
				t.Fatal("promoted bytes lost", err)
			}
			if err = p2.Discard(ctx, restored); err != nil {
				t.Fatal(err)
			}
			if err = p2.Close(); err != nil {
				t.Fatal(err)
			}
			e, err = p.Reset(ctx, e)
			if err != nil {
				t.Fatal(err)
			}
			v, err := p.Verify(ctx, e)
			if err != nil || !v.Valid {
				t.Fatal(v, err)
			}
			if err = p.Release(ctx, e); err != nil {
				t.Fatal(err)
			}
			if _, err = s.GC(ctx, 0, false); err != nil {
				t.Fatal(err)
			}
			report, err := s.Doctor(ctx)
			if err != nil || !report.OK {
				t.Fatal(report, err)
			}
			status, err = s.CacheStatus()
			if err != nil || status.Bytes > status.LimitBytes || status.PinnedObjects != 0 || status.ReservedDiskBytes != 0 {
				t.Fatal(status, err)
			}
		})
	}
}

func TestGitRemoteReconstruction(t *testing.T) {
	ctx, ns, content := setup(t)
	source := t.TempDir()
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
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	git(source, "init", "--initial-branch=main")
	git(source, "add", ".")
	git(source, "commit", "-m", "base")
	s, err := engine.OpenContext(ctx, t.TempDir(), engine.Options{Metadata: metadata(t, ctx, ns), Content: content})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base, err := s.Snapshot(ctx, engine.SnapshotOptions{Git: &engine.GitBase{Repository: source, Revision: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePool(ctx, engine.PoolOptions{Base: base.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	e, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(e.Path, "file"), []byte("local-only"), 0644); err != nil {
		t.Fatal(err)
	}
	git(e.Path, "add", ".")
	git(e.Path, "commit", "-m", "local")
	commit := git(e.Path, "rev-parse", "HEAD")
	git(e.Path, "update-ref", "refs/remotes/origin/forged", commit)
	selected, err := p.Promote(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Manifest.GitObjects.LocalPack == nil {
		t.Fatal("mutable remote-tracking ref hid local-only objects")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(source, source+"-offline"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(source+"-offline", source)
	replacement, err := engine.OpenContext(ctx, t.TempDir(), engine.Options{Metadata: metadata(t, ctx, ns), Content: content})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	restoredPool, err := replacement.CreatePool(ctx, engine.PoolOptions{Base: selected.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restoredPool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if git(restored.Path, "rev-parse", "HEAD") != commit || git(restored.Path, "status", "--porcelain") != "" {
		t.Fatal("Git reconstruction changed selected state")
	}
	git(restored.Path, "fsck", "--full")
	if replacement.Stats().RemoteBytesDownloaded == 0 {
		t.Fatal("empty-cache reconstruction did not exercise remote objects")
	}
}

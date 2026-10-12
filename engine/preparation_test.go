// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWarmPoolReusesVerifiedTrees(t *testing.T) {
	s, _, snap := fixture(t)
	p := poolFor(t, s, snap, 4)
	ctx := context.Background()
	result, err := p.Warm(ctx, 4)
	if err != nil || result.Prepared != 4 || result.Idle != 4 {
		t.Fatal(result, err)
	}
	if s.Stats().ActiveExecutions != 0 || s.allocated.Load() != 0 {
		t.Fatal("warm leaked active claims")
	}
	before := s.Stats()
	executions, err := p.AcquireMany(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	after := s.Stats()
	if after.ClonedFiles != before.ClonedFiles || after.CopiedFiles != before.CopiedFiles {
		t.Fatal("warm acquire materialized new trees")
	}
	if _, err = p.Warm(ctx, 1); !errors.Is(err, ErrConflict) {
		t.Fatal("warmed beyond pool capacity", err)
	}
	for _, e := range executions {
		if err = p.Release(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	result, err = p.Warm(ctx, 4)
	if err != nil || result.Prepared != 0 || result.Idle != 4 {
		t.Fatal(result, err)
	}
	if s.Stats().WarmedExecutions != 4 {
		t.Fatal(s.Stats())
	}
	if err = s.Drain(); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Warm(ctx, 1); !errors.Is(err, ErrDraining) {
		t.Fatal(err)
	}
}

func TestWarmRollbackQuotaAndCancellation(t *testing.T) {
	s, _, snap := fixture(t)
	p := poolFor(t, s, snap, 4)
	ctx := context.Background()
	s.opts.MaxExecutions = 2
	if _, err := p.Warm(ctx, 3); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.Warm(canceled, 2); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, e := range snap.Manifest.Entries {
		if e.Kind == "file" {
			if err := os.Chmod(filepath.Join(s.root, "objects", e.Digest), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(s.root, "objects", e.Digest), []byte("corrupt"), 0600); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if _, err := p.Warm(ctx, 2); !errors.Is(err, ErrVerification) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(s.root, "executions"))
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
	if p.preparing != 0 || len(p.idle) != 0 || s.allocated.Load() != 0 {
		t.Fatal("preparation leaked capacity")
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentWarmAcquireAndClose(t *testing.T) {
	s, _, snap := fixture(t)
	p := poolFor(t, s, snap, 8)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 10; round++ {
				if _, err := p.Warm(ctx, 4); err != nil && !errors.Is(err, ErrConflict) {
					t.Error(err)
					return
				}
				e, err := p.Acquire(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if err = p.Release(ctx, e); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if s.allocated.Load() != 0 || s.Stats().IdleExecutions != 0 {
		t.Fatal("capacity leaked")
	}
}

func TestPrefetchVerifiesAndLocalityDeduplicates(t *testing.T) {
	s, source, _ := fixture(t)
	write(t, filepath.Join(source, "duplicate.txt"), "base\n")
	snap, err := s.Snapshot(context.Background(), SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.Prefetch(context.Background(), snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int64{}
	var expected int64
	for _, e := range snap.Manifest.Entries {
		if e.Kind == "file" {
			seen[e.Digest] = e.Size
		}
	}
	for _, size := range seen {
		expected += size
	}
	if report.Objects != len(seen) || report.Bytes != expected || report.CachedBytes != expected || report.MissingBytes != 0 {
		t.Fatal(report)
	}
	if s.Stats().Prefetches != 1 || s.Stats().ActiveExecutions != 0 {
		t.Fatal(s.Stats())
	}
	for digest := range seen {
		name := filepath.Join(s.root, "objects", digest)
		if err = os.Chmod(name, 0600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(name, make([]byte, info.Size()), 0600); err != nil {
			t.Fatal(err)
		}
		break
	}
	// Locality is explicitly a residency hint, not a substitute for hashing.
	if _, err = s.Prefetch(context.Background(), snap.ID); !errors.Is(err, ErrVerification) {
		t.Fatal(err)
	}
}

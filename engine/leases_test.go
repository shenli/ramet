// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestBatchReservationAndRollback(t *testing.T) {
	s, _, snap := fixture(t)
	p := poolFor(t, s, snap, 8)
	ctx := context.Background()
	all, err := p.AcquireMany(ctx, 8)
	if err != nil || len(all) != 8 {
		t.Fatal(len(all), err)
	}
	deadline, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err = p.AcquireMany(deadline, 2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	for _, e := range all {
		if err = p.Release(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			es, err := p.AcquireMany(ctx, 4)
			if err != nil {
				t.Error(err)
				return
			}
			for _, e := range es {
				if err = p.Discard(ctx, e); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	var name string
	for _, e := range snap.Manifest.Entries {
		if e.Kind == "file" {
			name = filepath.Join(s.Root(), "objects", e.Digest)
			break
		}
	}
	if err = os.Chmod(name, 0600); err != nil {
		t.Fatal(err)
	}
	write(t, name, "corrupt")
	if _, err = p.AcquireMany(ctx, 8); !errors.Is(err, ErrVerification) {
		t.Fatal(err)
	}
	if s.Stats().ActiveExecutions != 0 || s.allocated.Load() != 0 {
		t.Fatal("failed batch leaked capacity")
	}
}

func TestLeaseRenewalExpiryAndQuota(t *testing.T) {
	s, _, snap := fixture(t)
	s.opts.MaxExecutions = 2
	s.opts.MaxPools = 2
	ctx := context.Background()
	p := poolFor(t, s, snap, 2)
	other := poolFor(t, s, snap, 2)
	if _, err := s.CreatePool(ctx, PoolOptions{Base: snap.ID, Concurrency: 1}); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	all, err := p.AcquireMany(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.Acquire(ctx); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	renewed, err := p.Heartbeat(ctx, all[0])
	if err != nil || !renewed.LeaseExpiresAt.After(all[0].LeaseExpiresAt) {
		t.Fatal(err)
	}
	item, err := p.claim(renewed)
	if err != nil {
		t.Fatal(err)
	}
	item.execution.LeaseExpiresAt = time.Now().Add(-time.Second)
	item.mu.Unlock()
	if _, err = p.Heartbeat(ctx, renewed); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expired lease resurrected: %v", err)
	}
	item, err = p.claim(all[1])
	if err != nil {
		t.Fatal(err)
	}
	item.execution.LeaseExpiresAt = time.Now().Add(-time.Second)
	item.mu.Unlock()
	if err = s.ReapExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if stats := s.Stats(); stats.ExpiredLeases != 2 || stats.ActiveExecutions != 0 || stats.IdleExecutions != 0 {
		t.Fatalf("invalid expiry stats: %+v", stats)
	}
	for _, e := range all {
		if _, err = os.Stat(e.Path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("expired directory survived")
		}
	}
	fresh, err := other.AcquireMany(ctx, 2)
	if err != nil {
		t.Fatal("quota not reclaimed", err)
	}
	for _, e := range fresh {
		if err = other.Discard(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLeaseReaperRunsAutomatically(t *testing.T) {
	s, _, snap := fixture(t)
	ctx := context.Background()
	p, err := s.CreatePool(ctx, PoolOptions{Base: snap.ID, Concurrency: 1, LeaseSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	e := acquire(t, p)
	deadline := time.Now().Add(4 * time.Second)
	for s.Stats().ExpiredLeases == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.Stats().ExpiredLeases != 1 {
		t.Fatal("background lease cleanup did not run")
	}
	if _, err = p.Verify(ctx, e); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

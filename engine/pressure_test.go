// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIdleAndDiskBounds(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	write(t, filepath.Join(source, "file"), "bounded")
	s, err := Open(t.TempDir(), Options{MaxIdleExecutions: 1, IdleTTL: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap, err := s.Snapshot(ctx, SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	p := poolFor(t, s, snap, 2)
	if _, err = p.Warm(ctx, 2); !errors.Is(err, ErrLimit) {
		t.Fatal("warm exceeded idle budget", err)
	}
	a, b := acquire(t, p), acquire(t, p)
	if err = p.Release(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err = p.Release(ctx, b); err != nil {
		t.Fatal(err)
	}
	if s.idleCount.Load() > 1 {
		t.Fatal("release exceeded idle bound")
	}
	if _, err = s.TrimIdle(ctx, false); err != nil {
		t.Fatal(err)
	}
	if s.idleCount.Load() != 0 {
		t.Fatal("idle expiry did not reclaim trees")
	}
	if _, err = s.TrimCache(ctx, 0); !errors.Is(err, ErrConflict) {
		t.Fatal("local authoritative data evictable", err)
	}
	v, err := s.Doctor(ctx)
	if err != nil || !v.OK {
		t.Fatal(v, err)
	}
	s.opts.MinFreeDiskBytes = math.MaxInt64
	if _, err = p.Acquire(ctx); !errors.Is(err, ErrLimit) {
		t.Fatal("disk guard did not fail closed", err)
	}
	status, _ := s.CacheStatus()
	if status.ReservedDiskBytes != 0 || status.PressureFailures == 0 {
		t.Fatal(status)
	}
}
func TestTenantSourceRestrictions(t *testing.T) {
	allowed, denied := t.TempDir(), t.TempDir()
	write(t, filepath.Join(denied, "secret"), "tenant B")
	if err := os.Symlink(denied, filepath.Join(allowed, "escape")); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.TempDir(), Options{RestrictSources: true, SourceRoots: []string{allowed}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, source := range []string{denied, filepath.Join(allowed, "escape")} {
		if _, err = s.Snapshot(context.Background(), SnapshotOptions{Source: source}); !errors.Is(err, ErrInvalid) {
			t.Fatal("source escape accepted", err)
		}
	}
	if _, err = s.Snapshot(context.Background(), SnapshotOptions{Git: &GitBase{Repository: denied, Revision: "main"}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("unlisted repository accepted", err)
	}
}

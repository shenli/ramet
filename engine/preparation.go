// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"time"
)

// Locality describes unique content bytes present on this worker. Residency is
// a point-in-time placement hint, not an integrity check or a reservation.
type Locality struct {
	Snapshot       string `json:"snapshot"`
	Objects        int    `json:"objects"`
	Bytes          int64  `json:"bytes"`
	CachedObjects  int    `json:"cached_objects"`
	CachedBytes    int64  `json:"cached_bytes"`
	MissingBytes   int64  `json:"missing_bytes"`
	IdleExecutions int    `json:"idle_executions"`
}

func (s *Store) Locality(ctx context.Context, id string) (Locality, error) {
	if err := s.enter(); err != nil {
		return Locality{}, err
	}
	defer s.leave()
	s.contentMu.RLock()
	defer s.contentMu.RUnlock()
	snap, err := s.readSnapshot(ctx, id)
	if err != nil {
		return Locality{}, err
	}
	return s.locality(ctx, snap)
}

func (s *Store) locality(ctx context.Context, snap Snapshot) (Locality, error) {
	r := Locality{Snapshot: snap.ID}
	seen := map[string]bool{}
	for _, e := range s.contentRefs(snap.Manifest) {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		if e.Kind != "file" || seen[e.Digest] {
			continue
		}
		seen[e.Digest] = true
		r.Objects++
		r.Bytes += e.Size
		info, err := os.Lstat(filepath.Join(s.root, "objects", e.Digest))
		if err == nil && info.Mode().IsRegular() && info.Size() == e.Size {
			r.CachedObjects++
			r.CachedBytes += e.Size
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return r, err
		}
	}
	r.MissingBytes = r.Bytes - r.CachedBytes
	s.mu.Lock()
	for _, p := range s.pools {
		if p.base.ID == snap.ID {
			p.mu.Lock()
			r.IdleExecutions += len(p.idle)
			p.mu.Unlock()
		}
	}
	s.mu.Unlock()
	return r, nil
}

// Prefetch hydrates and verifies all unique content for a snapshot without
// allocating execution trees or publishing durable state. Partial cache fills
// are safe to retain after cancellation; retrying verifies them again.
func (s *Store) Prefetch(ctx context.Context, id string) (Locality, error) {
	if err := s.enter(); err != nil {
		return Locality{}, err
	}
	defer s.leave()
	s.contentMu.RLock()
	defer s.contentMu.RUnlock()
	snap, err := s.readSnapshot(ctx, id)
	if err != nil {
		return Locality{}, err
	}
	seen := map[string]bool{}
	for _, e := range s.contentRefs(snap.Manifest) {
		if err := ctx.Err(); err != nil {
			return Locality{}, err
		}
		if e.Kind != "file" || seen[e.Digest] {
			continue
		}
		seen[e.Digest] = true
		if err = s.withObject(ctx, e, func(string) error { return s.verifyObject(ctx, e) }); err != nil {
			return Locality{}, err
		}
	}
	s.metrics.prefetches.Add(1)
	return s.locality(ctx, snap)
}

type WarmResult struct {
	Prepared int `json:"prepared"`
	Idle     int `json:"idle"`
}

// Warm prepares enough verified idle trees to reach count at reservation time.
// Concurrent acquisitions may consume them before this method returns. Warming
// reserves pool and worker capacity while constructing, but creates no leases.
func (p *Pool) Warm(ctx context.Context, count int) (WarmResult, error) {
	if err := p.store.enter(); err != nil {
		return WarmResult{}, err
	}
	defer p.store.leave()
	if err := ctx.Err(); err != nil {
		return WarmResult{}, err
	}
	if count < 0 || count > p.capacity {
		return WarmResult{}, fmt.Errorf("%w: warm count must be between 0 and pool concurrency", ErrInvalid)
	}
	p.store.mu.Lock()
	p.mu.Lock()
	draining := p.store.draining
	p.store.mu.Unlock()
	if p.closed || draining {
		p.mu.Unlock()
		if draining {
			return WarmResult{}, ErrDraining
		}
		return WarmResult{}, ErrClosed
	}
	if p.preparing > 0 {
		p.mu.Unlock()
		return WarmResult{}, fmt.Errorf("%w: pool preparation in progress", ErrConflict)
	}
	n := max(0, count-len(p.idle))
	if n == 0 {
		result := WarmResult{Idle: len(p.idle)}
		p.mu.Unlock()
		return result, nil
	}
	if len(p.active)+len(p.idle)+n > p.capacity {
		p.mu.Unlock()
		return WarmResult{}, fmt.Errorf("%w: insufficient idle capacity to warm", ErrConflict)
	}
	if !p.store.reserve(n) {
		p.mu.Unlock()
		return WarmResult{}, ErrLimit
	}
	if !p.store.reserveIdle(n) {
		p.store.allocated.Add(-int64(n))
		p.mu.Unlock()
		return WarmResult{}, fmt.Errorf("%w: idle execution limit", ErrLimit)
	}
	p.preparing = n
	p.mu.Unlock()

	items := make([]*slot, n)
	buildCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int, n)
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var buildErr error
	p.store.contentMu.RLock()
	for worker := 0; worker < min(n, 16); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				dir, err := p.store.materialize(buildCtx, p.base)
				if err != nil {
					errMu.Lock()
					if buildErr == nil {
						buildErr = err
					}
					errMu.Unlock()
					cancel()
				} else {
					items[i] = &slot{idleAt: time.Now(), execution: Execution{ID: newID("exec-"), PoolID: p.id, Base: p.base.ID, Path: dir}}
				}
			}
		}()
	}
	wg.Wait()
	p.store.contentMu.RUnlock()
	if buildErr == nil {
		buildErr = ctx.Err()
	}
	p.store.mu.Lock()
	p.mu.Lock()
	if buildErr == nil && p.store.draining {
		buildErr = ErrDraining
	}
	p.store.mu.Unlock()
	result := WarmResult{}
	if buildErr == nil {
		p.idle = append(p.idle, items...)
		result.Prepared = n
		p.store.metrics.warmed.Add(uint64(n))
	}
	result.Idle = len(p.idle)
	// Keep capacity reserved until rollback cleanup completes. Closing a pool
	// must not race cleanup and expose success while preparation still owns files.
	if buildErr != nil {
		p.mu.Unlock()
		p.store.idleCount.Add(-int64(n))
		for _, item := range items {
			if item != nil {
				buildErr = errors.Join(buildErr, p.store.removeExecution(item.execution.Path))
			}
		}
		p.mu.Lock()
	}
	p.preparing = 0
	p.store.allocated.Add(-int64(n))
	p.notify()
	p.mu.Unlock()
	return result, buildErr
}

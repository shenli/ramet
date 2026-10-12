// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"errors"
	"time"
)

func (s *Store) reserve(n int) bool {
	for {
		current := s.allocated.Load()
		if current+int64(n) > int64(s.opts.MaxExecutions) {
			return false
		}
		if s.allocated.CompareAndSwap(current, current+int64(n)) {
			return true
		}
	}
}

// Heartbeat extends a valid lease. An expired lease is never resurrected.
func (p *Pool) Heartbeat(ctx context.Context, e Execution) (Execution, error) {
	if err := p.store.enter(); err != nil {
		return Execution{}, err
	}
	defer p.store.leave()
	if err := ctx.Err(); err != nil {
		return Execution{}, err
	}
	item, err := p.claim(e)
	if err != nil {
		return Execution{}, err
	}
	defer item.mu.Unlock()
	item.execution.LeaseExpiresAt = time.Now().UTC().Add(p.leaseTTL)
	return item.execution, nil
}

func (s *Store) reapLoop() {
	defer close(s.reaperDone)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.reaperStop:
			return
		case <-ticker.C:
			if err := s.ReapExpired(context.Background()); err != nil && !errors.Is(err, ErrClosed) {
				s.metrics.maintenanceErrors.Add(1)
			}
			if _, err := s.TrimIdle(context.Background(), false); err != nil && !errors.Is(err, ErrClosed) {
				s.metrics.maintenanceErrors.Add(1)
			}
		}
	}
}

// ReapExpired fences and destroys abandoned executions. It never recycles them
// and does not manage workload processes: the runtime must honor the lease.
func (s *Store) ReapExpired(ctx context.Context) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	s.mu.Lock()
	pools := make([]*Pool, 0, len(s.pools))
	for _, p := range s.pools {
		pools = append(pools, p)
	}
	s.mu.Unlock()
	var errs []error
	for _, p := range pools {
		p.mu.Lock()
		items := make([]*slot, 0, len(p.active))
		for _, item := range p.active {
			items = append(items, item)
		}
		p.mu.Unlock()
		for _, item := range items {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(errs, err)...)
			}
			if !item.mu.TryLock() {
				continue
			}
			if item.active && !item.execution.LeaseExpiresAt.IsZero() && !time.Now().Before(item.execution.LeaseExpiresAt) {
				s.metrics.expired.Add(1)
				errs = append(errs, p.quarantine(item))
			}
			item.mu.Unlock()
		}
	}
	return errors.Join(errs...)
}

// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"errors"
	"time"
)

func (s *Store) reserveIdle(n int) bool {
	for {
		current := s.idleCount.Load()
		if current+int64(n) > int64(s.opts.MaxIdleExecutions) {
			return false
		}
		if s.idleCount.CompareAndSwap(current, current+int64(n)) {
			return true
		}
	}
}

// TrimIdle discards idle trees only; active leases and in-progress preparations
// remain protected. all=false applies the configured idle age limit.
func (s *Store) TrimIdle(ctx context.Context, all bool) (int, error) {
	if err := s.enter(); err != nil {
		return 0, err
	}
	defer s.leave()
	s.mu.Lock()
	pools := make([]*Pool, 0, len(s.pools))
	for _, p := range s.pools {
		pools = append(pools, p)
	}
	s.mu.Unlock()
	var paths []string
	var errs []error
	cutoff := time.Now().Add(-s.opts.IdleTTL)
	for _, p := range pools {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		p.mu.Lock()
		kept := p.idle[:0]
		for _, item := range p.idle {
			if all || item.idleAt.Before(cutoff) {
				paths = append(paths, item.execution.Path)
				s.idleCount.Add(-1)
			} else {
				kept = append(kept, item)
			}
		}
		p.idle = kept
		p.notify()
		p.mu.Unlock()
	}
	for _, name := range paths {
		errs = append(errs, s.removeExecution(name))
	}
	return len(paths), errors.Join(errs...)
}

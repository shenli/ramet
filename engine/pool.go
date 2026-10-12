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

type slot struct {
	mu        sync.Mutex
	execution Execution
	active    bool
	idleAt    time.Time
}

// Pool bounds checked-out executions. Release rebuilds and deeply verifies state
// before returning it to the idle set. Each claim gets an unguessable new lease.
type Pool struct {
	store     *Store
	id        string
	base      Snapshot
	capacity  int
	preparing int
	leaseTTL  time.Duration
	mu        sync.Mutex
	active    map[string]*slot
	idle      []*slot
	changed   chan struct{}
	closed    bool
}

func (s *Store) CreatePool(ctx context.Context, opts PoolOptions) (*Pool, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	if opts.Concurrency < 1 || opts.Concurrency > 10000 {
		return nil, fmt.Errorf("%w: concurrency must be between 1 and 10000", ErrInvalid)
	}
	s.contentMu.RLock()
	defer s.contentMu.RUnlock()
	if opts.LeaseSeconds == 0 {
		opts.LeaseSeconds = 900
	}
	if opts.LeaseSeconds < 1 || opts.LeaseSeconds > 86400 {
		return nil, fmt.Errorf("%w: lease_seconds must be between 1 and 86400", ErrInvalid)
	}
	base, err := s.readSnapshot(ctx, opts.Base)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return nil, ErrDraining
	}
	if len(s.pools) >= s.opts.MaxPools {
		return nil, ErrLimit
	}
	p := &Pool{leaseTTL: time.Duration(opts.LeaseSeconds) * time.Second, store: s, id: newID("pool-"), base: base, capacity: opts.Concurrency, active: map[string]*slot{}, changed: make(chan struct{})}
	s.pools[p.id] = p
	return p, nil
}

func (s *Store) Pool(id string) (*Pool, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pools[id]
	if !ok {
		return nil, ErrNotFound
	}
	return p, nil
}
func (p *Pool) ID() string { return p.id }
func (p *Pool) notify()    { close(p.changed); p.changed = make(chan struct{}) }

// Acquire waits for capacity until ctx is cancelled. Independent acquisitions
// materialize concurrently; slow disks do not hold the pool's scheduling lock.
func (p *Pool) Acquire(ctx context.Context) (Execution, error) {
	executions, err := p.AcquireMany(ctx, 1)
	if err != nil {
		return Execution{}, err
	}
	return executions[0], nil
}

// AcquireMany reserves the whole cohort before materialization, so concurrent
// batch callers never deadlock while each holds part of the requested capacity.
func (p *Pool) AcquireMany(ctx context.Context, count int) ([]Execution, error) {
	if count < 1 || count > p.capacity {
		return nil, fmt.Errorf("%w: count must be between 1 and pool concurrency", ErrInvalid)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := p.store.enter(); err != nil {
			return nil, err
		}
		p.store.mu.Lock()
		draining := p.store.draining
		p.mu.Lock()
		p.store.mu.Unlock()
		if p.closed || draining {
			p.mu.Unlock()
			p.store.leave()
			if draining {
				return nil, ErrDraining
			}
			return nil, ErrClosed
		}
		if len(p.active)+p.preparing+count > p.capacity {
			changed := p.changed
			p.mu.Unlock()
			p.store.leave()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-changed:
				continue
			}
		}
		if !p.store.reserve(count) {
			p.mu.Unlock()
			p.store.leave()
			return nil, ErrLimit
		}
		items := make([]*slot, 0, count)
		for i := 0; i < count; i++ {
			var item *slot
			if len(p.idle) > 0 {
				item = p.idle[len(p.idle)-1]
				p.idle = p.idle[:len(p.idle)-1]
				p.store.idleCount.Add(-1)
			} else {
				item = &slot{execution: Execution{ID: newID("exec-"), PoolID: p.id, Base: p.base.ID}}
			}
			item.mu.Lock()
			item.active = true
			item.execution.Lease = newID("lease-")
			item.execution.Generation++
			p.active[item.execution.ID] = item
			items = append(items, item)
		}
		p.mu.Unlock()
		buildCtx, cancel := context.WithCancel(ctx)
		jobs := make(chan *slot, count)
		for _, item := range items {
			jobs <- item
		}
		close(jobs)
		var wg sync.WaitGroup
		var errMu sync.Mutex
		var firstErr error
		p.store.contentMu.RLock()
		for i := 0; i < min(count, 16); i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for item := range jobs {
					var err error
					if item.execution.Path == "" {
						item.execution.Path, err = p.store.materialize(buildCtx, p.base)
					} else {
						var v Verification
						v, err = p.store.verifyPrepared(buildCtx, item.execution.Path, p.base.Manifest)
						if err == nil && !v.Valid {
							err = ErrVerification
						}
					}
					if err != nil {
						errMu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						errMu.Unlock()
						cancel()
					}
				}
			}()
		}
		wg.Wait()
		cancel()
		p.store.contentMu.RUnlock()
		result := make([]Execution, 0, count)
		for _, item := range items {
			if firstErr != nil {
				firstErr = errors.Join(firstErr, p.quarantine(item))
			} else {
				item.execution.LeaseExpiresAt = time.Now().UTC().Add(p.leaseTTL)
				result = append(result, item.execution)
			}
			item.mu.Unlock()
		}
		if firstErr == nil {
			p.store.metrics.acquires.Add(uint64(count))
		}
		p.store.leave()
		return result, firstErr
	}
}

// claim returns a locked active slot. Generation and lease fencing rejects both
// stale callers after reset and a previous owner after release/reacquisition.
func (p *Pool) claim(e Execution) (*slot, error) {
	p.mu.Lock()
	item := p.active[e.ID]
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	if item == nil {
		return nil, ErrNotFound
	}
	item.mu.Lock()
	if !item.active || e.PoolID != p.id || e.Lease == "" || e.Lease != item.execution.Lease || e.Generation != item.execution.Generation {
		item.mu.Unlock()
		return nil, fmt.Errorf("%w: stale execution lease or generation", ErrConflict)
	}
	if !time.Now().Before(item.execution.LeaseExpiresAt) {
		p.store.metrics.expired.Add(1)
		err := p.quarantine(item)
		item.mu.Unlock()
		return nil, errors.Join(ErrLeaseExpired, err)
	}
	return item, nil
}

// quarantine removes a suspect slot from scheduling before cleanup. The caller
// owns item.mu. A failed reset can never put this slot back into circulation.
func (p *Pool) quarantine(item *slot) error {
	item.active = false
	p.mu.Lock()
	delete(p.active, item.execution.ID)
	p.store.allocated.Add(-1)
	p.notify()
	p.mu.Unlock()
	p.store.metrics.quarantines.Add(1)
	return p.store.removeExecution(item.execution.Path)
}

func (p *Pool) reset(ctx context.Context, item *slot) error {
	p.store.contentMu.RLock()
	defer p.store.contentMu.RUnlock()
	dir, err := p.store.materialize(ctx, p.base)
	if err != nil {
		return errors.Join(err, p.quarantine(item))
	}
	// The replacement has a new directory identity. Old open file descriptors
	// point at the removed tree, not the next owner's reusable workspace.
	if err = p.store.removeExecution(item.execution.Path); err != nil {
		return errors.Join(err, p.store.removeExecution(dir), p.quarantine(item))
	}
	item.execution.Path = dir
	item.execution.Generation++
	item.execution.LeaseExpiresAt = time.Now().UTC().Add(p.leaseTTL)
	p.store.metrics.resets.Add(1)
	return nil
}

func (p *Pool) Reset(ctx context.Context, e Execution) (Execution, error) {
	if err := p.store.enter(); err != nil {
		return Execution{}, err
	}
	defer p.store.leave()
	item, err := p.claim(e)
	if err != nil {
		return Execution{}, err
	}
	defer item.mu.Unlock()
	if err = p.reset(ctx, item); err != nil {
		return Execution{}, err
	}
	return item.execution, nil
}

func (p *Pool) Release(ctx context.Context, e Execution) error {
	if err := p.store.enter(); err != nil {
		return err
	}
	defer p.store.leave()
	item, err := p.claim(e)
	if err != nil {
		return err
	}
	defer item.mu.Unlock()
	if err = p.reset(ctx, item); err != nil {
		return err
	}
	item.active = false
	item.execution.Lease = ""
	p.mu.Lock()
	delete(p.active, e.ID)
	p.store.allocated.Add(-1)
	cacheIdle := p.store.reserveIdle(1)
	if cacheIdle {
		item.idleAt = time.Now()
		p.idle = append(p.idle, item)
	}
	p.notify()
	p.mu.Unlock()
	p.store.metrics.recycles.Add(1)
	if !cacheIdle {
		return p.store.removeExecution(item.execution.Path)
	}
	return nil
}

func (p *Pool) Discard(ctx context.Context, e Execution) error {
	if err := p.store.enter(); err != nil {
		return err
	}
	defer p.store.leave()
	if err := ctx.Err(); err != nil {
		return err
	}
	item, err := p.claim(e)
	if err != nil {
		return err
	}
	defer item.mu.Unlock()
	item.active = false
	p.mu.Lock()
	delete(p.active, e.ID)
	p.store.allocated.Add(-1)
	p.notify()
	p.mu.Unlock()
	return p.store.removeExecution(item.execution.Path)
}

// Verify compares current execution state with its pool base. A dirty active
// execution is valid work, so only verification on acquire/reset quarantines.
func (p *Pool) Verify(ctx context.Context, e Execution) (Verification, error) {
	if err := p.store.enter(); err != nil {
		return Verification{}, err
	}
	defer p.store.leave()
	item, err := p.claim(e)
	if err != nil {
		return Verification{}, err
	}
	defer item.mu.Unlock()
	return p.store.verifyTree(ctx, item.execution.Path, p.base.Manifest)
}

// Checkpoint captures all supported state. Promote omits explicitly rebuildable
// and ephemeral paths, retains recipes, and enforces shared-immutable rules.
func (p *Pool) Checkpoint(ctx context.Context, e Execution) (Snapshot, error) {
	return p.capture(ctx, e, false)
}
func (p *Pool) Promote(ctx context.Context, e Execution) (Snapshot, error) {
	return p.capture(ctx, e, true)
}
func (p *Pool) capture(ctx context.Context, e Execution, selective bool) (Snapshot, error) {
	if err := p.store.enter(); err != nil {
		return Snapshot{}, err
	}
	defer p.store.leave()
	item, err := p.claim(e)
	if err != nil {
		return Snapshot{}, err
	}
	defer item.mu.Unlock()
	p.store.contentMu.RLock()
	defer p.store.contentMu.RUnlock()
	m := Manifest{Version: FormatVersion, Parent: p.base.ID, Git: p.base.Manifest.Git, Policy: p.base.Manifest.Policy}
	return p.store.capture(ctx, item.execution.Path, m, selective, &p.base.Manifest)
}

// Fork copies the parent's current local state without publishing a durable
// snapshot. It consumes one additional slot and fails promptly when full.
func (p *Pool) Fork(ctx context.Context, e Execution) (Execution, error) {
	if err := p.store.enter(); err != nil {
		return Execution{}, err
	}
	defer p.store.leave()
	parent, err := p.claim(e)
	if err != nil {
		return Execution{}, err
	}
	defer parent.mu.Unlock()
	p.store.mu.Lock()
	p.mu.Lock()
	draining := p.store.draining
	p.store.mu.Unlock()
	if p.closed || draining || len(p.active)+p.preparing >= p.capacity {
		p.mu.Unlock()
		if draining {
			return Execution{}, ErrDraining
		}
		return Execution{}, fmt.Errorf("%w: pool closed or at capacity", ErrConflict)
	}
	if !p.store.reserve(1) {
		p.mu.Unlock()
		return Execution{}, ErrLimit
	}
	// Consume an idle slot's capacity; its verified base tree is not the fork.
	var unused string
	if len(p.idle) > 0 {
		unused = p.idle[len(p.idle)-1].execution.Path
		p.idle = p.idle[:len(p.idle)-1]
		p.store.idleCount.Add(-1)
	}
	child := &slot{active: true, execution: Execution{ID: newID("exec-"), PoolID: p.id, Base: p.base.ID, Lease: newID("lease-"), Generation: 1}}
	child.mu.Lock()
	defer child.mu.Unlock()
	p.active[child.execution.ID] = child
	p.mu.Unlock()
	if unused != "" {
		if err = p.store.removeExecution(unused); err != nil {
			return Execution{}, errors.Join(err, p.quarantine(child))
		}
	}
	child.execution.Path, err = p.store.cloneExecution(ctx, parent.execution.Path)
	if err != nil {
		return Execution{}, errors.Join(err, p.quarantine(child))
	}
	child.execution.LeaseExpiresAt = time.Now().UTC().Add(p.leaseTTL)
	p.store.metrics.acquires.Add(1)
	return child.execution, nil
}

func (p *Pool) Close() error {
	if err := p.store.enter(); err != nil {
		return err
	}
	defer p.store.leave()
	p.store.mu.Lock()
	defer p.store.mu.Unlock()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	if len(p.active) > 0 || p.preparing > 0 {
		p.mu.Unlock()
		return fmt.Errorf("%w: release active executions and finish preparation before closing pool", ErrConflict)
	}
	p.closed = true
	p.notify()
	idle := p.idle
	p.store.idleCount.Add(-int64(len(idle)))
	p.idle = nil
	p.mu.Unlock()
	delete(p.store.pools, p.id)
	var errs []error
	for _, item := range idle {
		errs = append(errs, p.store.removeExecution(item.execution.Path))
	}
	return errors.Join(errs...)
}

func (p *Pool) closeInternal() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.notify()
	var errs []error
	for _, item := range p.active {
		errs = append(errs, p.store.removeExecution(item.execution.Path))
	}
	for _, item := range p.idle {
		errs = append(errs, p.store.removeExecution(item.execution.Path))
	}
	p.store.allocated.Add(-int64(len(p.active)))
	p.active = map[string]*slot{}
	p.store.idleCount.Add(-int64(len(p.idle)))
	p.idle = nil
	return errors.Join(errs...)
}

// cloneExecution inspects and verifies a local fork, preserving timestamps and
// independent inodes without writing content or metadata to durable storage.
func (s *Store) cloneExecution(ctx context.Context, source string) (string, error) {
	m := Manifest{Version: FormatVersion, Entries: []Entry{}}
	err := filepath.WalkDir(source, func(name string, d os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if name == source {
			return nil
		}
		rel, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		e := Entry{Path: rel, Mode: uint32(info.Mode().Perm()), ModTime: info.ModTime().UnixNano()}
		switch {
		case info.IsDir():
			e.Kind = "dir"
		case info.Mode().IsRegular():
			e.Kind = "file"
			e.Digest, e.Size, err = digestFile(ctx, name)
		case info.Mode()&os.ModeSymlink != 0:
			e.Kind = "symlink"
			e.Mode = 0
			e.ModTime = 0
			e.Target, err = os.Readlink(name)
		default:
			return fmt.Errorf("%w: special file %s cannot be forked", ErrInvalid, rel)
		}
		if err != nil {
			return err
		}
		m.Entries = append(m.Entries, e)
		return nil
	})
	if err != nil {
		return "", err
	}
	// WalkDir's depth-first order is not lexicographic for all names.
	sortEntries(m.Entries)
	if err = validateManifest(m); err != nil {
		return "", err
	}
	return s.materializeFrom(ctx, Snapshot{Manifest: m}, func(e Entry) string { return filepath.Join(source, filepath.FromSlash(e.Path)) })
}

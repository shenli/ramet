// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

type cacheObject struct {
	size  int64
	used  time.Time
	pins  int
	ready chan struct{}
}

type CacheStatus struct {
	Bytes             int64  `json:"bytes"`
	LimitBytes        int64  `json:"limit_bytes"`
	Objects           int    `json:"objects"`
	PinnedObjects     int    `json:"pinned_objects"`
	ReservedDiskBytes int64  `json:"reserved_disk_bytes"`
	FreeDiskBytes     int64  `json:"free_disk_bytes"`
	MinFreeDiskBytes  int64  `json:"min_free_disk_bytes"`
	IdleExecutions    int64  `json:"idle_executions"`
	MaxIdleExecutions int    `json:"max_idle_executions"`
	Hits              uint64 `json:"hits"`
	Misses            uint64 `json:"misses"`
	Evictions         uint64 `json:"evictions"`
	EvictedBytes      uint64 `json:"evicted_bytes"`
	PressureFailures  uint64 `json:"pressure_failures"`
}

func freeDisk(root string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
func (s *Store) initializeCache() error {
	s.cacheObjects = map[string]*cacheObject{}
	entries, err := os.ReadDir(filepath.Join(s.root, "objects"))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !validDigest(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: nonregular cached object", ErrVerification)
		}
		s.cacheObjects[e.Name()] = &cacheObject{size: info.Size(), used: info.ModTime()}
		s.cacheBytes += info.Size()
	}
	if s.opts.Content != nil && s.opts.CacheBytes > 0 {
		return s.evictLocked(s.opts.CacheBytes)
	}
	return nil
}

// evictLocked only removes disposable remote cache objects. In-flight users
// pin entries before opening paths; authoritative local content is never evicted.
func (s *Store) evictLocked(target int64) error {
	if s.opts.Content == nil {
		return nil
	}
	type candidate struct {
		id     string
		object *cacheObject
	}
	var candidates []candidate
	for id, o := range s.cacheObjects {
		if o.pins == 0 && o.ready == nil {
			candidates = append(candidates, candidate{id, o})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].object.used.Before(candidates[j].object.used) })
	for _, c := range candidates {
		if s.cacheBytes <= target {
			break
		}
		if err := os.Remove(filepath.Join(s.root, "objects", c.id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		delete(s.cacheObjects, c.id)
		s.cacheBytes -= c.object.size
		s.metrics.evictions.Add(1)
		s.metrics.evictedBytes.Add(uint64(c.object.size))
	}
	if s.cacheBytes > target {
		s.metrics.pressure.Add(1)
		return fmt.Errorf("%w: cache entries are pinned or object exceeds cache budget", ErrLimit)
	}
	return nil
}
func (s *Store) reserveCacheLocked(size int64) error {
	if s.opts.CacheBytes <= 0 || s.opts.Content == nil {
		return nil
	}
	if size > s.opts.CacheBytes {
		s.metrics.pressure.Add(1)
		return fmt.Errorf("%w: object exceeds cache byte limit", ErrLimit)
	}
	return s.evictLocked(s.opts.CacheBytes - size)
}

// reserveDisk accounts for simultaneous staging/copy writes before allocating
// bytes. It is a managed-I/O guard, not a quota on arbitrary sandbox writes.
func (s *Store) reserveDisk(size int64) (func(), error) {
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	if size < 0 {
		return nil, ErrInvalid
	}
	if s.opts.MinFreeDiskBytes > 0 {
		free, err := freeDisk(s.root)
		if err != nil {
			return nil, err
		}
		if size > free-s.opts.MinFreeDiskBytes-s.diskReserved {
			s.metrics.pressure.Add(1)
			return nil, fmt.Errorf("%w: insufficient disk headroom", ErrLimit)
		}
	}
	s.diskReserved += size
	return func() { s.diskMu.Lock(); s.diskReserved -= size; s.diskMu.Unlock() }, nil
}

// withObject pins a cache entry across hydration and the caller's complete
// read. No caller may retain the pathname after fn returns.
func (s *Store) withObject(ctx context.Context, e Entry, fn func(string) error) error {
	name := filepath.Join(s.root, "objects", e.Digest)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.cacheMu.Lock()
		o := s.cacheObjects[e.Digest]
		if o != nil && o.ready != nil {
			ready := o.ready
			s.cacheMu.Unlock()
			select {
			case <-ready:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if o != nil {
			if o.size != e.Size {
				s.cacheMu.Unlock()
				return fmt.Errorf("%w: conflicting cached size", ErrVerification)
			}
			o.pins++
			o.used = time.Now()
			s.metrics.cacheHits.Add(1)
			s.cacheMu.Unlock()
			err := fn(name)
			s.cacheMu.Lock()
			o.pins--
			s.cacheMu.Unlock()
			return err
		}
		if s.opts.Content == nil {
			s.cacheMu.Unlock()
			return fmt.Errorf("%w: missing local object %s", ErrVerification, e.Digest)
		}
		if err := s.reserveCacheLocked(e.Size); err != nil {
			s.cacheMu.Unlock()
			return err
		}
		o = &cacheObject{size: e.Size, pins: 1, used: time.Now(), ready: make(chan struct{})}
		s.cacheObjects[e.Digest] = o
		s.cacheBytes += e.Size
		s.metrics.cacheMisses.Add(1)
		s.cacheMu.Unlock()
		err := s.downloadObject(ctx, e)
		s.cacheMu.Lock()
		ready := o.ready
		o.ready = nil
		if err != nil {
			delete(s.cacheObjects, e.Digest)
			s.cacheBytes -= e.Size
		}
		close(ready)
		s.cacheMu.Unlock()
		if err == nil {
			err = fn(name)
		}
		s.cacheMu.Lock()
		o.pins--
		s.cacheMu.Unlock()
		return err
	}
}

// installCache publishes a staged object already made durable remotely (or
// authoritative locally). The caller still owns the temporary path.
func (s *Store) installCache(ctx context.Context, tmp string, e Entry) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.cacheMu.Lock()
		if o := s.cacheObjects[e.Digest]; o != nil {
			if o.ready != nil {
				ready := o.ready
				s.cacheMu.Unlock()
				select {
				case <-ready:
					continue
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			o.pins++
			s.cacheMu.Unlock()
			err := s.verifyObject(ctx, e)
			s.cacheMu.Lock()
			o.pins--
			o.used = time.Now()
			s.cacheMu.Unlock()
			return err
		}
		if err := s.reserveCacheLocked(e.Size); err != nil {
			s.cacheMu.Unlock()
			return err
		}
		err := os.Link(tmp, filepath.Join(s.root, "objects", e.Digest))
		if errors.Is(err, os.ErrExist) {
			err = s.verifyObject(ctx, e)
		}
		if err == nil {
			s.cacheObjects[e.Digest] = &cacheObject{size: e.Size, used: time.Now()}
			s.cacheBytes += e.Size
			s.metrics.contentBytes.Add(uint64(e.Size))
		}
		s.cacheMu.Unlock()
		return err
	}
}
func (s *Store) CacheStatus() (CacheStatus, error) {
	r := CacheStatus{LimitBytes: s.opts.CacheBytes, MinFreeDiskBytes: s.opts.MinFreeDiskBytes, MaxIdleExecutions: s.opts.MaxIdleExecutions, IdleExecutions: s.idleCount.Load()}
	s.cacheMu.Lock()
	r.Bytes = s.cacheBytes
	r.Objects = len(s.cacheObjects)
	for _, o := range s.cacheObjects {
		if o.pins > 0 {
			r.PinnedObjects++
		}
	}
	s.cacheMu.Unlock()
	s.diskMu.Lock()
	r.ReservedDiskBytes = s.diskReserved
	s.diskMu.Unlock()
	var err error
	r.FreeDiskBytes, err = freeDisk(s.root)
	r.Hits = s.metrics.cacheHits.Load()
	r.Misses = s.metrics.cacheMisses.Load()
	r.Evictions = s.metrics.evictions.Load()
	r.EvictedBytes = s.metrics.evictedBytes.Load()
	r.PressureFailures = s.metrics.pressure.Load()
	return r, err
}
func (s *Store) TrimCache(ctx context.Context, target int64) (CacheStatus, error) {
	if target < 0 {
		return CacheStatus{}, ErrInvalid
	}
	if err := s.enter(); err != nil {
		return CacheStatus{}, err
	}
	defer s.leave()
	if err := ctx.Err(); err != nil {
		return CacheStatus{}, err
	}
	if s.opts.Content == nil {
		return CacheStatus{}, fmt.Errorf("%w: local objects are authoritative; use GC", ErrConflict)
	}
	s.cacheMu.Lock()
	err := s.evictLocked(target)
	s.cacheMu.Unlock()
	r, reportErr := s.CacheStatus()
	return r, errors.Join(err, reportErr)
}

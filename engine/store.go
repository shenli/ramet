// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shenli/ramet/internal/fsutil"
	"github.com/shenli/ramet/internal/lazyfs"
	"github.com/shenli/ramet/storage"
)

type Options struct {
	// RequireCOW fails materialization if the filesystem cannot clone files.
	Logger     *slog.Logger
	RequireCOW bool
	// Backend selects copy (default) or Linux lazy hydration.
	Backend         string
	RestrictSources bool
	SourceRoots     []string
	GitRepositories []string
	// Metadata and Content must be supplied together. Open takes ownership of
	// Metadata and closes it with the worker (including failed initialization).
	Metadata          storage.Metadata
	Content           storage.Content
	MaxExecutions     int
	MaxPools          int
	CacheBytes        int64
	MinFreeDiskBytes  int64
	MaxIdleExecutions int
	IdleTTL           time.Duration
}

type counters struct {
	hydrationCount, hydrationErrors, hydrationNanos                  atomic.Uint64
	acquires, resets, recycles, verifications, failures, quarantines atomic.Uint64
	cloned, copied, contentBytes                                     atomic.Uint64
	uploaded, downloaded                                             atomic.Uint64
	expired                                                          atomic.Uint64
	cacheHits, cacheMisses, evictions, evictedBytes, pressure        atomic.Uint64
	maintenanceErrors                                                atomic.Uint64
	warmed, prefetches                                               atomic.Uint64
}

// Store owns a single worker's metadata, immutable content, and execution cache.
// One process may open a data directory at a time. Snapshots survive restart;
// ephemeral executions and pools deliberately do not.
type Store struct {
	lazyMu       sync.Mutex
	lazyMounts   map[string]*lazyfs.Mount
	root         string
	opts         Options
	lock         *os.File
	life         sync.RWMutex
	closed       bool
	contentMu    sync.RWMutex // protects in-flight capture/materialization from GC
	mu           sync.Mutex
	pools        map[string]*Pool
	draining     bool
	metrics      counters
	cacheMu      sync.Mutex
	cacheObjects map[string]*cacheObject
	cacheBytes   int64
	diskMu       sync.Mutex
	diskReserved int64
	idleCount    atomic.Int64
	allocated    atomic.Int64
	reaperStop   chan struct{}
	reaperDone   chan struct{}
}

func Open(root string, opts Options) (*Store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return OpenContext(ctx, root, opts)
}

func OpenContext(ctx context.Context, root string, opts Options) (*Store, error) {
	owned := false
	defer func() {
		if !owned && opts.Metadata != nil {
			_ = opts.Metadata.Close()
		}
	}()
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Backend == "" {
		opts.Backend = "copy"
	}
	if opts.Backend != "copy" && opts.Backend != "lazy" {
		return nil, fmt.Errorf("%w: backend must be copy or lazy", ErrInvalid)
	}
	if opts.Backend == "lazy" {
		if opts.RequireCOW {
			return nil, fmt.Errorf("%w: lazy backend cannot require CoW", ErrInvalid)
		}
		if err := lazyfs.Supported(); err != nil {
			return nil, err
		}
	}
	if opts.CacheBytes < 0 || opts.MinFreeDiskBytes < 0 || opts.MaxIdleExecutions < 0 || opts.IdleTTL < 0 {
		return nil, fmt.Errorf("%w: negative cache limit", ErrInvalid)
	}
	if opts.CacheBytes > 0 && opts.Content == nil {
		return nil, fmt.Errorf("%w: cache byte limit requires remote content", ErrInvalid)
	}
	if opts.MaxIdleExecutions == 0 {
		opts.MaxIdleExecutions = 128
	}
	if opts.IdleTTL == 0 {
		opts.IdleTTL = 5 * time.Minute
	}
	if opts.MaxExecutions == 0 {
		opts.MaxExecutions = 4096
	}
	if opts.MaxPools == 0 {
		opts.MaxPools = 128
	}
	if opts.MaxExecutions < 1 || opts.MaxPools < 1 {
		return nil, fmt.Errorf("%w: worker limits must be positive", ErrInvalid)
	}
	if (opts.Metadata == nil) != (opts.Content == nil) {
		return nil, fmt.Errorf("%w: metadata and content providers must be configured together", ErrInvalid)
	}
	if root == "" {
		return nil, fmt.Errorf("%w: data directory is required", ErrInvalid)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	opts.SourceRoots = append([]string(nil), opts.SourceRoots...)
	opts.GitRepositories = append([]string(nil), opts.GitRepositories...)
	for i, sourceRoot := range opts.SourceRoots {
		real, err := filepath.EvalSymlinks(sourceRoot)
		if err != nil {
			return nil, err
		}
		real, err = filepath.Abs(real)
		if err != nil {
			return nil, err
		}
		if within(abs, real) || within(real, abs) {
			return nil, fmt.Errorf("%w: source root overlaps worker storage", ErrInvalid)
		}
		opts.SourceRoots[i] = real
	}
	lock, err := fsutil.Lock(filepath.Join(abs, "LOCK"))
	if err != nil {
		return nil, err
	}
	s := &Store{lazyMounts: map[string]*lazyfs.Mount{}, root: abs, opts: opts, lock: lock, pools: map[string]*Pool{}, reaperStop: make(chan struct{}), reaperDone: make(chan struct{})}
	ok := false
	defer func() {
		if !ok {
			_ = fsutil.Unlock(lock)
		}
	}()
	for _, name := range []string{"objects", "snapshots", "staging", "executions"} {
		if err = os.MkdirAll(filepath.Join(abs, name), 0700); err != nil {
			return nil, err
		}
	}
	formatPath := filepath.Join(abs, "FORMAT")
	b, err := os.ReadFile(formatPath)
	if errors.Is(err, os.ErrNotExist) {
		err = fsutil.AtomicWrite(formatPath, []byte("1\n"), 0600)
	} else if err == nil && string(b) != "1\n" {
		err = fmt.Errorf("unsupported store format %q; this binary reads format 1", strings.TrimSpace(string(b)))
	}
	if err != nil {
		return nil, err
	}
	// The lock proves no live worker owns these temporary directories. Never
	// remove immutable content during crash recovery; GC handles orphan objects.
	if err = s.initializeRemote(ctx); err != nil {
		return nil, err
	}
	for _, name := range []string{"staging", "executions"} {
		entries, e := os.ReadDir(filepath.Join(abs, name))
		if e != nil {
			return nil, e
		}
		for _, entry := range entries {
			path := filepath.Join(abs, name, entry.Name())
			if name == "executions" && strings.HasPrefix(entry.Name(), "lazy-") {
				if err = lazyfs.Recover(filepath.Join(path, "root")); err != nil {
					return nil, err
				}
			}
			if err = fsutil.RemoveTree(path); err != nil {
				return nil, err
			}
		}
	}
	// Atomic manifest publication can leave an unpublished temporary file after
	// a crash. Only this reserved prefix is disposable; final manifests remain.
	entries, err := os.ReadDir(filepath.Join(abs, "snapshots"))
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pending-") {
			if err = os.Remove(filepath.Join(abs, "snapshots", entry.Name())); err != nil {
				return nil, err
			}
		}
	}
	if err = s.initializeCache(); err != nil {
		return nil, err
	}
	ok = true
	owned = true
	go s.reapLoop()
	return s, nil
}

func (s *Store) enter() error {
	s.life.RLock()
	if s.closed {
		s.life.RUnlock()
		return ErrClosed
	}
	return nil
}
func (s *Store) leave() { s.life.RUnlock() }

func (s *Store) Close() error {
	s.life.Lock()
	if s.closed {
		s.life.Unlock()
		<-s.reaperDone
		return nil
	}
	s.closed = true
	close(s.reaperStop)
	var errs []error
	for _, p := range s.pools {
		errs = append(errs, p.closeInternal())
	}
	s.lazyMu.Lock()
	abandoned := make([]string, 0, len(s.lazyMounts))
	for name := range s.lazyMounts {
		abandoned = append(abandoned, name)
	}
	s.lazyMu.Unlock()
	for _, name := range abandoned {
		errs = append(errs, s.removeExecution(name))
	}
	if s.opts.Metadata != nil {
		errs = append(errs, s.opts.Metadata.Close())
	}
	errs = append(errs, fsutil.Unlock(s.lock))
	s.life.Unlock()
	<-s.reaperDone
	return errors.Join(errs...)
}

func (s *Store) Root() string { return s.root }

// RestrictsSources reports whether captures are confined to explicit allowlists.
func (s *Store) RestrictsSources() bool { return s.opts.RestrictSources }

func (s *Store) Drain() error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draining = true
	for _, p := range s.pools {
		p.mu.Lock()
		p.notify()
		p.mu.Unlock()
	}
	return nil
}

func (s *Store) IsDraining() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.draining }

// Ready reports lifecycle readiness, not a deep storage integrity scan.
func (s *Store) Ready() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.ReadyContext(ctx)
}

func (s *Store) ReadyContext(ctx context.Context) bool {
	if err := s.enter(); err != nil {
		return false
	}
	defer s.leave()
	if s.IsDraining() {
		return false
	}
	if s.opts.Metadata != nil {
		return s.opts.Metadata.Ping(ctx) == nil && s.opts.Content.Ping(ctx) == nil
	}
	return true
}

func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}

func (s *Store) Snapshot(ctx context.Context, opts SnapshotOptions) (Snapshot, error) {
	if err := s.enter(); err != nil {
		return Snapshot{}, err
	}
	defer s.leave()
	if err := opts.Policy.Validate(); err != nil {
		return Snapshot{}, err
	}
	if (opts.Source == "") == (opts.Git == nil) {
		return Snapshot{}, fmt.Errorf("%w: provide exactly one of source or git", ErrInvalid)
	}
	s.contentMu.RLock()
	defer s.contentMu.RUnlock()
	source := opts.Source
	var git *GitBase
	if opts.Git != nil {
		if s.opts.RestrictSources {
			allowed := false
			for _, repo := range s.opts.GitRepositories {
				if repo == opts.Git.Repository {
					allowed = true
				}
			}
			if !allowed {
				return Snapshot{}, fmt.Errorf("%w: Git repository is not allowed for this tenant", ErrInvalid)
			}
		}
		dir, base, err := s.checkoutGit(ctx, *opts.Git)
		if err != nil {
			return Snapshot{}, err
		}
		defer fsutil.RemoveTree(dir)
		source, git = dir, &base
	} else {
		abs, err := filepath.Abs(source)
		if err != nil {
			return Snapshot{}, err
		}
		abs, err = filepath.EvalSymlinks(abs)
		if err != nil {
			return Snapshot{}, err
		}
		if s.opts.RestrictSources {
			allowed := false
			for _, root := range s.opts.SourceRoots {
				if within(root, abs) {
					allowed = true
				}
			}
			if !allowed {
				return Snapshot{}, fmt.Errorf("%w: source is not allowed for this tenant", ErrInvalid)
			}
		}
		// Never recursively snapshot Ramet's own storage or an owned execution.
		if within(abs, s.root) || within(s.root, abs) {
			return Snapshot{}, fmt.Errorf("%w: source and data directory must not overlap", ErrInvalid)
		}
		source = abs
	}
	return s.capture(ctx, source, Manifest{Version: FormatVersion, Git: git, Policy: opts.Policy}, false, nil)
}

func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func (s *Store) GetSnapshot(ctx context.Context, id string) (Snapshot, error) {
	if err := s.enter(); err != nil {
		return Snapshot{}, err
	}
	defer s.leave()
	return s.readSnapshot(ctx, id)
}

func (s *Store) ListSnapshots(ctx context.Context) ([]Snapshot, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	return s.listSnapshots(ctx)
}

func (s *Store) listSnapshots(ctx context.Context) ([]Snapshot, error) {
	if s.opts.Metadata != nil {
		records, err := s.opts.Metadata.List(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]Snapshot, 0, len(records))
		for _, r := range records {
			snap, err := decodeSnapshot(r.ID, r.Data)
			if err != nil {
				return nil, err
			}
			if snap.Manifest.Parent != r.Parent {
				return nil, fmt.Errorf("%w: metadata lineage mismatch", ErrVerification)
			}
			out = append(out, snap)
		}
		return out, nil
	}
	entries, err := os.ReadDir(filepath.Join(s.root, "snapshots"))
	if err != nil {
		return nil, err
	}
	out := []Snapshot{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		snap, err := s.readSnapshot(ctx, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// DeleteSnapshot refuses to remove a pool base or a snapshot referenced by
// lineage. Content remains until a separate grace-period garbage collection.
func (s *Store) DeleteSnapshot(ctx context.Context, id string) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	s.contentMu.Lock()
	defer s.contentMu.Unlock()
	if _, err := s.readSnapshot(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.pools {
		if p.base.ID == id {
			return fmt.Errorf("%w: snapshot is a pool base", ErrConflict)
		}
	}
	snaps, err := s.listSnapshots(ctx)
	if err != nil {
		return err
	}
	for _, snap := range snaps {
		if snap.Manifest.Parent == id {
			return fmt.Errorf("%w: snapshot has descendants", ErrConflict)
		}
	}
	if s.opts.Metadata != nil {
		return s.opts.Metadata.Delete(ctx, id)
	}
	if err = os.Remove(filepath.Join(s.root, "snapshots", id+".json")); err != nil {
		return err
	}
	return fsutil.SyncDir(filepath.Join(s.root, "snapshots"))
}

type GCResult struct {
	Objects int   `json:"objects"`
	Bytes   int64 `json:"bytes"`
	DryRun  bool  `json:"dry_run"`
}

func (s *Store) GC(ctx context.Context, grace time.Duration, dryRun bool) (GCResult, error) {
	result := GCResult{DryRun: dryRun}
	if grace < 0 {
		return result, fmt.Errorf("%w: negative GC grace", ErrInvalid)
	}
	if err := s.enter(); err != nil {
		return result, err
	}
	defer s.leave()
	s.contentMu.Lock()
	defer s.contentMu.Unlock()
	snaps, err := s.listSnapshots(ctx)
	if err != nil {
		return result, err
	} // fail closed on unreadable metadata
	marked := map[string]bool{}
	for _, snap := range snaps {
		for _, e := range s.contentRefs(snap.Manifest) {
			if e.Digest != "" {
				marked[e.Digest] = true
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(s.root, "objects"))
	if err != nil {
		return result, err
	}
	cutoff := time.Now().Add(-grace)
	for _, e := range entries {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		if !validDigest(e.Name()) || marked[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return result, err
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if !dryRun {
			s.cacheMu.Lock()
			o := s.cacheObjects[e.Name()]
			if o != nil && (o.pins > 0 || o.ready != nil) {
				s.cacheMu.Unlock()
				continue
			}
			err = os.Remove(filepath.Join(s.root, "objects", e.Name()))
			if err == nil && o != nil {
				delete(s.cacheObjects, e.Name())
				s.cacheBytes -= o.size
			}
			s.cacheMu.Unlock()
			if err != nil {
				return result, err
			}
		}
		result.Objects++
		result.Bytes += info.Size()
	}
	if !dryRun {
		err = fsutil.SyncDir(filepath.Join(s.root, "objects"))
	}
	return result, err
}

func (s *Store) Stats() Stats {
	m := &s.metrics
	stats := Stats{Acquires: m.acquires.Load(), Resets: m.resets.Load(), Recycles: m.recycles.Load(), Verifications: m.verifications.Load(), VerificationFailures: m.failures.Load(), Quarantines: m.quarantines.Load(), ClonedFiles: m.cloned.Load(), CopiedFiles: m.copied.Load(), ContentBytesWritten: m.contentBytes.Load()}
	stats.RemoteBytesUploaded = m.uploaded.Load()
	stats.RemoteBytesDownloaded = m.downloaded.Load()
	stats.ExpiredLeases = m.expired.Load()
	stats.WarmedExecutions = m.warmed.Load()
	stats.Prefetches = m.prefetches.Load()
	stats.MaintenanceErrors = m.maintenanceErrors.Load()
	stats.HydratedFiles = m.hydrationCount.Load()
	stats.HydrationErrors = m.hydrationErrors.Load()
	stats.HydrationSeconds = float64(m.hydrationNanos.Load()) / 1e9
	s.mu.Lock()
	defer s.mu.Unlock()
	stats.Pools = len(s.pools)
	for _, p := range s.pools {
		p.mu.Lock()
		stats.ActiveExecutions += len(p.active)
		stats.IdleExecutions += len(p.idle)
		stats.PreparingExecutions += p.preparing
		p.mu.Unlock()
	}
	return stats
}

type DoctorReport struct {
	OK          bool     `json:"ok"`
	Format      int      `json:"format"`
	CopyOnWrite bool     `json:"copy_on_write"`
	Snapshots   int      `json:"snapshots"`
	Objects     int      `json:"objects"`
	Problems    []string `json:"problems"`
	Durability  string   `json:"durability"`
}

func (s *Store) Doctor(ctx context.Context) (DoctorReport, error) {
	r := DoctorReport{OK: true, Format: FormatVersion, Problems: []string{}}
	r.Durability = "local"
	if s.opts.Metadata != nil {
		r.Durability = "remote"
	}
	if err := s.enter(); err != nil {
		return r, err
	}
	defer s.leave()
	s.contentMu.RLock()
	defer s.contentMu.RUnlock()
	snaps, err := s.listSnapshots(ctx)
	if err != nil {
		return r, err
	}
	r.Snapshots = len(snaps)
	seen := map[string]bool{}
	ids := map[string]bool{}
	for _, snap := range snaps {
		ids[snap.ID] = true
	}
	for _, snap := range snaps {
		if snap.Manifest.Parent != "" && !ids[snap.Manifest.Parent] {
			r.Problems = append(r.Problems, "missing parent of "+snap.ID)
		}
		for _, e := range s.contentRefs(snap.Manifest) {
			if e.Digest == "" || seen[e.Digest] {
				continue
			}
			seen[e.Digest] = true
			r.Objects++
			if err := s.verifyDurableObject(ctx, e); err != nil {
				r.Problems = append(r.Problems, err.Error())
			}
		}
	}
	dir, err := os.MkdirTemp(filepath.Join(s.root, "staging"), "doctor-")
	if err != nil {
		return r, err
	}
	defer fsutil.RemoveTree(dir)
	if err = os.WriteFile(filepath.Join(dir, "source"), []byte("ramet clone probe\n"), 0600); err != nil {
		return r, err
	}
	r.CopyOnWrite, _ = fsutil.CopyFile(ctx, filepath.Join(dir, "source"), filepath.Join(dir, "clone"), true)
	if s.opts.RequireCOW && !r.CopyOnWrite {
		r.Problems = append(r.Problems, "copy-on-write is required but unsupported on this volume")
	}
	r.OK = len(r.Problems) == 0
	return r, nil
}

// MarshalJSON on a pool deliberately excludes worker internals.
func (p *Pool) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID          string `json:"id"`
		Base        string `json:"base"`
		Concurrency int    `json:"concurrency"`
	}{p.id, p.base.ID, p.capacity})
}

// Backend reports the configured materialization backend.
func (s *Store) Backend() string { return s.opts.Backend }

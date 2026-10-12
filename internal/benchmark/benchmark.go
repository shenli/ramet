// SPDX-License-Identifier: Apache-2.0
// Package benchmark runs an end-to-end local lifecycle workload.
package benchmark

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shenli/ramet/engine"
)

type Latency struct {
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`
}
type Result struct {
	OS                           string       `json:"os"`
	Arch                         string       `json:"arch"`
	GoVersion                    string       `json:"go_version"`
	Concurrency                  int          `json:"concurrency"`
	Iterations                   int          `json:"iterations"`
	Successful                   int64        `json:"successful"`
	Seconds                      float64      `json:"seconds"`
	ExecutionsPerSecond          float64      `json:"executions_per_second"`
	SnapshotSeconds              float64      `json:"snapshot_seconds"`
	ColdReady                    Latency      `json:"cold_ready"`
	WarmReady                    Latency      `json:"warm_ready"`
	Recycle                      Latency      `json:"recycle"`
	BaseLogicalBytes             int64        `json:"base_logical_bytes"`
	Snapshot                     string       `json:"snapshot"`
	CopyOnWrite                  bool         `json:"copy_on_write"`
	DurableBytesDuringExecutions uint64       `json:"durable_bytes_during_executions"`
	HeapBytes                    uint64       `json:"heap_bytes_after"`
	Stats                        engine.Stats `json:"stats"`
}

func quantiles(values []float64) Latency {
	if len(values) == 0 {
		return Latency{}
	}
	sort.Float64s(values)
	q := func(f float64) float64 { return values[int(float64(len(values)-1)*f)] }
	return Latency{q(.5), q(.95), q(.99)}
}

func Run(ctx context.Context, s *engine.Store, source string, concurrency, iterations int) (Result, error) {
	r := Result{OS: runtime.GOOS, Arch: runtime.GOARCH, GoVersion: runtime.Version(), Concurrency: concurrency, Iterations: iterations}
	if concurrency < 1 || iterations < 1 {
		return r, fmt.Errorf("concurrency and iterations must be positive")
	}
	if _, err := os.Lstat(filepath.Join(source, ".ramet-benchmark-attempt")); !os.IsNotExist(err) {
		return r, fmt.Errorf("benchmark source must not contain .ramet-benchmark-attempt")
	}
	start := time.Now()
	snap, err := s.Snapshot(ctx, engine.SnapshotOptions{Source: source})
	if err != nil {
		return r, err
	}
	r.SnapshotSeconds = time.Since(start).Seconds()
	r.Snapshot = snap.ID
	for _, e := range snap.Manifest.Entries {
		r.BaseLogicalBytes += e.Size
	}
	p, err := s.CreatePool(ctx, engine.PoolOptions{Base: snap.ID, Concurrency: concurrency})
	if err != nil {
		return r, err
	}
	cold := []float64{}
	warm := []float64{}
	recycle := []float64{}
	claims := make([]engine.Execution, 0, concurrency)
	defer func() {
		// A cancelled cold start may leave earlier claims checked out. Discard
		// them before closing; already-released lease values are safely rejected.
		for _, e := range claims {
			_ = p.Discard(context.Background(), e)
		}
		_ = p.Close()
	}()
	for i := 0; i < concurrency; i++ {
		start := time.Now()
		e, err := p.Acquire(ctx)
		if err != nil {
			return r, err
		}
		claims = append(claims, e)
		cold = append(cold, float64(time.Since(start))/float64(time.Millisecond))
	}
	for _, e := range claims {
		if err = p.Release(ctx, e); err != nil {
			return r, err
		}
	}
	before := s.Stats().ContentBytesWritten
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var next, success atomic.Int64
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	start = time.Now()
	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				index := next.Add(1)
				if index > int64(iterations) {
					return
				}
				readyStart := time.Now()
				e, err := p.Acquire(ctx)
				readyMS := float64(time.Since(readyStart)) / float64(time.Millisecond)
				if err == nil {
					marker := filepath.Join(e.Path, ".ramet-benchmark-attempt")
					if _, statErr := os.Lstat(marker); !os.IsNotExist(statErr) {
						err = fmt.Errorf("contamination: marker exists before execution %d", index)
					}
					if err == nil {
						err = os.WriteFile(marker, []byte(fmt.Sprintf("attempt %d\n", index)), 0600)
					}
					recycleStart := time.Now()
					if err == nil {
						err = p.Release(ctx, e)
					} else {
						_ = p.Discard(context.Background(), e)
					}
					recycleMS := float64(time.Since(recycleStart)) / float64(time.Millisecond)
					if err == nil {
						success.Add(1)
						mu.Lock()
						warm = append(warm, readyMS)
						recycle = append(recycle, recycleMS)
						mu.Unlock()
					}
				}
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					cancel()
					return
				}
			}
		}()
	}
	wg.Wait()
	r.Seconds = time.Since(start).Seconds()
	r.Successful = success.Load()
	r.ExecutionsPerSecond = float64(r.Successful) / r.Seconds
	r.ColdReady = quantiles(cold)
	r.WarmReady = quantiles(warm)
	r.Recycle = quantiles(recycle)
	r.Stats = s.Stats()
	r.CopyOnWrite = r.Stats.ClonedFiles > 0 && r.Stats.CopiedFiles == 0
	r.DurableBytesDuringExecutions = r.Stats.ContentBytesWritten - before
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	r.HeapBytes = mem.HeapAlloc
	if firstErr != nil {
		return r, firstErr
	}
	if err = p.Close(); err != nil {
		return r, err
	}
	return r, nil
}

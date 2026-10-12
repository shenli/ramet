// SPDX-License-Identifier: Apache-2.0
// Package server exposes the single-worker Ramet HTTP API.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shenli/ramet/engine"
	"github.com/shenli/ramet/storage"
)

type Options struct {
	Version        string
	Tenant         string
	MaxInflight    int
	Token          string
	Logger         *slog.Logger
	RequestTimeout time.Duration
}

type API struct {
	admission  chan struct{}
	observed   observations
	store      *engine.Store
	opts       Options
	handler    http.Handler
	operations *operationRegistry
}

func New(store *engine.Store, opts Options) *API {
	if opts.Version == "" {
		opts.Version = "development"
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 2 * time.Minute
	}
	if opts.MaxInflight <= 0 {
		opts.MaxInflight = 128
	}
	a := &API{store: store, opts: opts, admission: make(chan struct{}, opts.MaxInflight), observed: observations{values: map[string]*observation{}}}
	a.operations = newOperationRegistry()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/capabilities", a.capabilities)
	mux.HandleFunc("GET /v1/operations/new", a.operations.issue)
	mux.HandleFunc("GET /v1/operations/{id}", a.operations.lookup)
	mux.HandleFunc("GET /live", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]string{"status": "live"}) })
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if !store.ReadyContext(ctx) {
			respond(w, 503, map[string]string{"status": "unavailable"})
			return
		}
		respond(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /metrics", a.metrics)
	mux.HandleFunc("GET /v1/worker/cache", func(w http.ResponseWriter, r *http.Request) {
		result, err := store.CacheStatus()
		a.result(w, result, err)
	})
	mux.HandleFunc("POST /v1/worker/cache/trim", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TargetBytes int64 `json:"target_bytes"`
		}
		if !a.decode(w, r, &req) {
			return
		}
		result, err := store.TrimCache(r.Context(), req.TargetBytes)
		a.result(w, result, err)
	})
	mux.HandleFunc("POST /v1/worker/idle/trim", func(w http.ResponseWriter, r *http.Request) {
		count, err := store.TrimIdle(r.Context(), true)
		a.result(w, map[string]int{"removed": count}, err)
	})
	mux.HandleFunc("GET /v1/doctor", func(w http.ResponseWriter, r *http.Request) {
		report, err := store.Doctor(r.Context())
		a.result(w, report, err)
	})
	mux.HandleFunc("POST /v1/worker/drain", func(w http.ResponseWriter, r *http.Request) { a.result(w, nil, store.Drain()) })
	mux.HandleFunc("POST /v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		var opts engine.SnapshotOptions
		if !a.decode(w, r, &opts) {
			return
		}
		snap, err := store.Snapshot(r.Context(), opts)
		a.result(w, snap, err)
	})
	mux.HandleFunc("GET /v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		snaps, err := store.ListSnapshots(r.Context())
		a.result(w, snaps, err)
	})
	mux.HandleFunc("GET /v1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		snap, err := store.GetSnapshot(r.Context(), r.PathValue("id"))
		a.result(w, snap, err)
	})
	mux.HandleFunc("GET /v1/snapshots/{id}/restore-plan", func(w http.ResponseWriter, r *http.Request) {
		result, err := store.RestorePlan(r.Context(), r.PathValue("id"))
		a.result(w, result, err)
	})
	mux.HandleFunc("GET /v1/snapshots/{id}/locality", func(w http.ResponseWriter, r *http.Request) {
		result, err := store.Locality(r.Context(), r.PathValue("id"))
		a.result(w, result, err)
	})
	mux.HandleFunc("POST /v1/snapshots/{id}/prefetch", func(w http.ResponseWriter, r *http.Request) {
		result, err := store.Prefetch(r.Context(), r.PathValue("id"))
		a.result(w, result, err)
	})
	mux.HandleFunc("DELETE /v1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		a.result(w, nil, store.DeleteSnapshot(r.Context(), r.PathValue("id")))
	})
	mux.HandleFunc("POST /v1/pools", func(w http.ResponseWriter, r *http.Request) {
		var opts engine.PoolOptions
		if !a.decode(w, r, &opts) {
			return
		}
		p, err := store.CreatePool(r.Context(), opts)
		a.result(w, p, err)
	})
	mux.HandleFunc("GET /v1/pools/{pool}", func(w http.ResponseWriter, r *http.Request) {
		p, err := store.Pool(r.PathValue("pool"))
		a.result(w, p, err)
	})
	mux.HandleFunc("DELETE /v1/pools/{pool}", func(w http.ResponseWriter, r *http.Request) {
		p, err := store.Pool(r.PathValue("pool"))
		if err == nil {
			err = p.Close()
		}
		a.result(w, nil, err)
	})
	mux.HandleFunc("POST /v1/pools/{pool}/warm", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Count int `json:"count"`
		}
		if !a.decode(w, r, &req) {
			return
		}
		p, err := store.Pool(r.PathValue("pool"))
		if err != nil {
			a.result(w, nil, err)
			return
		}
		result, err := p.Warm(r.Context(), req.Count)
		a.result(w, result, err)
	})
	mux.HandleFunc("POST /v1/pools/{pool}/acquire", func(w http.ResponseWriter, r *http.Request) {
		p, err := store.Pool(r.PathValue("pool"))
		if err != nil {
			a.result(w, nil, err)
			return
		}
		e, err := p.Acquire(r.Context())
		a.result(w, e, err)
	})
	mux.HandleFunc("POST /v1/pools/{pool}/acquire-many", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Count int `json:"count"`
		}
		if !a.decode(w, r, &req) {
			return
		}
		p, err := store.Pool(r.PathValue("pool"))
		if err != nil {
			a.result(w, nil, err)
			return
		}
		executions, err := p.AcquireMany(r.Context(), req.Count)
		a.result(w, executions, err)
	})
	mux.HandleFunc("POST /v1/pools/{pool}/executions/{execution}/{action}", a.execution)
	mux.HandleFunc("POST /v1/gc", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			GraceSeconds *int64 `json:"grace_seconds"`
			DryRun       *bool  `json:"dry_run"`
		}
		if !a.decode(w, r, &req) {
			return
		}
		grace := int64(3600)
		dry := true
		if req.GraceSeconds != nil {
			grace = *req.GraceSeconds
		}
		if req.DryRun != nil {
			dry = *req.DryRun
		}
		if grace < 0 || grace > 31536000 {
			a.result(w, nil, fmt.Errorf("%w: grace_seconds must be between 0 and 31536000", engine.ErrInvalid))
			return
		}
		result, err := store.GC(r.Context(), time.Duration(grace)*time.Second, dry)
		a.result(w, result, err)
	})
	a.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.observe(w, r, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !controlRequest(r) {
				select {
				case a.admission <- struct{}{}:
					defer func() { <-a.admission }()
				default:
					w.Header().Set("Retry-After", "1")
					respond(w, 429, map[string]string{"code": "request_limit", "error": "tenant request limit reached"})
					return
				}
			}
			a.operations.handle(w, r, structuredRoutes(mux))
		}))
	})
	return a
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ensureRequestID(w)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path != "/live" && r.URL.Path != "/ready" && a.opts.Token != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		wantHash, gotHash := sha256.Sum256([]byte(a.opts.Token)), sha256.Sum256([]byte(got))
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(wantHash[:], gotHash[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			respond(w, 401, map[string]string{"code": "unauthorized", "error": "valid bearer token required"})
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.opts.RequestTimeout)
	defer cancel()
	a.handler.ServeHTTP(w, r.WithContext(ctx))
}

func (a *API) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	err := d.Decode(v)
	if err == nil {
		var extra any
		if e := d.Decode(&extra); e != io.EOF {
			err = fmt.Errorf("expected exactly one JSON object")
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			respond(w, 413, map[string]string{"code": "body_too_large", "error": "request body exceeds 1 MiB"})
		} else {
			respond(w, 400, map[string]string{"code": "invalid_request", "error": "invalid JSON request: " + err.Error()})
		}
		return false
	}
	return true
}

func (a *API) execution(w http.ResponseWriter, r *http.Request) {
	var lease struct {
		Lease      string `json:"lease"`
		Generation uint64 `json:"generation"`
	}
	if !a.decode(w, r, &lease) {
		return
	}
	p, err := a.store.Pool(r.PathValue("pool"))
	if err != nil {
		a.result(w, nil, err)
		return
	}
	e := engine.Execution{ID: r.PathValue("execution"), PoolID: p.ID(), Lease: lease.Lease, Generation: lease.Generation}
	var result any
	switch r.PathValue("action") {
	case "heartbeat":
		result, err = p.Heartbeat(r.Context(), e)
	case "reset":
		result, err = p.Reset(r.Context(), e)
	case "release":
		err = p.Release(r.Context(), e)
	case "discard":
		err = p.Discard(r.Context(), e)
	case "verify":
		result, err = p.Verify(r.Context(), e)
	case "fork":
		result, err = p.Fork(r.Context(), e)
	case "checkpoint":
		result, err = p.Checkpoint(r.Context(), e)
	case "preview-promotion":
		result, err = p.PreviewPromotion(r.Context(), e)
	case "restore-plan":
		result, err = p.RestorePlan(r.Context(), e)
	case "promote":
		result, err = p.Promote(r.Context(), e)
	default:
		err = engine.ErrNotFound
	}
	a.result(w, result, err)
}

func (a *API) result(w http.ResponseWriter, v any, err error) {
	if err == nil {
		if v == nil {
			w.WriteHeader(204)
		} else {
			respond(w, 200, v)
		}
		return
	}
	status, code := 500, "internal_error"
	switch {
	case errors.Is(err, engine.ErrInvalid):
		status, code = 400, "invalid_request"
	case errors.Is(err, engine.ErrNotFound):
		status, code = 404, "not_found"
	case errors.Is(err, engine.ErrConflict), errors.Is(err, engine.ErrClosed):
		status, code = 409, "conflict"
	case errors.Is(err, engine.ErrLeaseExpired):
		status, code = 409, "lease_expired"
	case errors.Is(err, engine.ErrLimit):
		status, code = 429, "quota_exceeded"
	case errors.Is(err, engine.ErrDraining):
		status, code = 503, "draining"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		status, code = 408, "timeout"
	case errors.Is(err, engine.ErrVerification), errors.Is(err, storage.ErrCorrupt):
		status, code = 422, "verification_failed"
	}
	message := err.Error()
	if status == 500 {
		a.opts.Logger.Error("request failed", "request_id", w.Header().Get("X-Request-ID"), "tenant", a.opts.Tenant, "error", err)
		message = "internal error; see worker logs"
	}
	respond(w, status, map[string]string{"code": code, "error": message})
}

func respond(w http.ResponseWriter, status int, v any) {
	if fields, ok := v.(map[string]string); ok && status >= 400 {
		fields["request_id"] = ensureRequestID(w)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	s := a.store.Stats()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	values := []struct {
		name, kind, help string
		value            uint64
	}{
		{"hydrated_files_total", "counter", "Files lazily hydrated on first open.", s.HydratedFiles},
		{"hydration_errors_total", "counter", "Failed lazy file hydration attempts.", s.HydrationErrors},
		{"maintenance_errors_total", "counter", "Background lease or idle cleanup failures.", s.MaintenanceErrors},
		{"warmed_executions_total", "counter", "Verified idle executions prepared explicitly.", s.WarmedExecutions},
		{"prefetches_total", "counter", "Successful snapshot content prefetches.", s.Prefetches},
		{"preparing_executions", "gauge", "Reserved execution slots being warmed.", uint64(s.PreparingExecutions)},
		{"acquires_total", "counter", "Successful execution claims and forks.", s.Acquires},
		{"resets_total", "counter", "Successful verified resets.", s.Resets},
		{"recycles_total", "counter", "Executions returned to the idle pool.", s.Recycles},
		{"verifications_total", "counter", "Deep workspace verifications.", s.Verifications},
		{"verification_failures_total", "counter", "Workspace verifications reporting drift or failure.", s.VerificationFailures},
		{"quarantines_total", "counter", "Suspect execution slots removed from scheduling.", s.Quarantines},
		{"cloned_files_total", "counter", "Files materialized using copy-on-write.", s.ClonedFiles},
		{"copied_files_total", "counter", "Files materialized using full-copy fallback.", s.CopiedFiles},
		{"content_bytes_written_total", "counter", "New immutable bytes written to local content storage.", s.ContentBytesWritten},
		{"remote_bytes_uploaded_total", "counter", "New immutable bytes uploaded to durable remote content storage.", s.RemoteBytesUploaded},
		{"remote_bytes_downloaded_total", "counter", "Content bytes hydrated into the worker cache.", s.RemoteBytesDownloaded},
		{"expired_leases_total", "counter", "Expired execution leases fenced and destroyed.", s.ExpiredLeases},
		{"active_executions", "gauge", "Checked-out or materializing executions.", uint64(s.ActiveExecutions)},
		{"idle_executions", "gauge", "Verified idle execution slots.", uint64(s.IdleExecutions)},
		{"pools", "gauge", "Open execution pools.", uint64(s.Pools)},
	}
	cache, err := a.store.CacheStatus()
	if err == nil {
		fmt.Fprintf(w, "# TYPE ramet_cache_bytes gauge\nramet_cache_bytes %d\n# TYPE ramet_cache_limit_bytes gauge\nramet_cache_limit_bytes %d\n# TYPE ramet_cache_evictions_total counter\nramet_cache_evictions_total %d\n# TYPE ramet_cache_evicted_bytes_total counter\nramet_cache_evicted_bytes_total %d\n# TYPE ramet_cache_pressure_failures_total counter\nramet_cache_pressure_failures_total %d\n# TYPE ramet_cache_hits_total counter\nramet_cache_hits_total %d\n# TYPE ramet_cache_misses_total counter\nramet_cache_misses_total %d\n# TYPE ramet_disk_reserved_bytes gauge\nramet_disk_reserved_bytes %d\n", cache.Bytes, cache.LimitBytes, cache.Evictions, cache.EvictedBytes, cache.PressureFailures, cache.Hits, cache.Misses, cache.ReservedDiskBytes)
	}
	fmt.Fprintf(w, "# TYPE ramet_hydration_seconds_total counter\nramet_hydration_seconds_total %g\n", s.HydrationSeconds)
	a.requestMetrics(w)
	for _, v := range values {
		fmt.Fprintf(w, "# HELP ramet_%s %s\n# TYPE ramet_%s %s\nramet_%s %d\n", v.name, v.help, v.name, v.kind, v.name, v.value)
	}
}

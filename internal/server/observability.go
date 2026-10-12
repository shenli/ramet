// SPDX-License-Identifier: Apache-2.0
package server

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

var latencyBounds = []float64{.001, .005, .01, .05, .1, .5, 1, 5, 30, 120}

type observation struct {
	count   uint64
	seconds float64
	buckets [10]uint64
}
type observations struct {
	mu     sync.Mutex
	values map[string]*observation
}
type responseStatus struct {
	http.ResponseWriter
	status int
}

func (w *responseStatus) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *responseStatus) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *responseStatus) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Labels come from matched route templates, never user-supplied paths or IDs.
func (a *API) observe(w http.ResponseWriter, r *http.Request, next http.Handler) {
	start := time.Now()
	requestID := ensureRequestID(w)
	status := &responseStatus{ResponseWriter: w}
	defer func() {
		elapsed := time.Since(start).Seconds()
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		// execution actions share one bounded route label.
		outcome := fmt.Sprintf("%dxx", status.status/100)
		if status.status == 0 {
			outcome = "2xx"
		}
		key := fmt.Sprintf("route=%q,outcome=%q", route, outcome)
		a.observed.mu.Lock()
		item := a.observed.values[key]
		if item == nil {
			item = &observation{}
			a.observed.values[key] = item
		}
		item.count++
		item.seconds += elapsed
		for i, bound := range latencyBounds {
			if elapsed <= bound {
				item.buckets[i]++
			}
		}
		a.observed.mu.Unlock()
		attrs := []any{"request_id", requestID, "tenant", a.opts.Tenant, "route", route, "status", status.status, "duration_seconds", elapsed}
		for _, name := range []string{"id", "pool", "execution"} {
			if value := r.PathValue(name); value != "" && len(value) <= 80 {
				attrs = append(attrs, name, value)
			}
		}
		if status.status >= 500 {
			a.opts.Logger.Error("request failed", attrs...)
		} else {
			a.opts.Logger.Debug("request completed", attrs...)
		}
	}()
	next.ServeHTTP(status, r)
}
func (a *API) requestMetrics(w http.ResponseWriter) {
	fmt.Fprintln(w, "# HELP ramet_http_request_duration_seconds HTTP request latency by bounded route and outcome.\n# TYPE ramet_http_request_duration_seconds histogram")
	a.observed.mu.Lock()
	defer a.observed.mu.Unlock()
	for key, item := range a.observed.values {
		for i, bound := range latencyBounds {
			fmt.Fprintf(w, "ramet_http_request_duration_seconds_bucket{%s,le=%q} %d\n", key, fmt.Sprint(bound), item.buckets[i])
		}
		fmt.Fprintf(w, "ramet_http_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\nramet_http_request_duration_seconds_count{%s} %d\nramet_http_request_duration_seconds_sum{%s} %g\n", key, item.count, key, item.count, key, item.seconds)
	}
	fmt.Fprintf(w, "# TYPE ramet_http_inflight gauge\nramet_http_inflight %d\n", len(a.admission))
}
func controlRequest(r *http.Request) bool {
	return (r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/operations/")) || r.URL.Path == "/live" || r.URL.Path == "/ready" || r.URL.Path == "/metrics" || strings.HasSuffix(r.URL.Path, "/heartbeat")
}

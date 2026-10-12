// SPDX-License-Identifier: Apache-2.0
package server_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shenli/ramet/client"
	"github.com/shenli/ramet/engine"
	"github.com/shenli/ramet/internal/server"
)

func worker(t *testing.T) (*client.Client, *engine.Store, string) {
	t.Helper()
	s, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h := httptest.NewServer(server.New(s, server.Options{Token: "test-token"}))
	t.Cleanup(h.Close)
	source := t.TempDir()
	if err = os.WriteFile(filepath.Join(source, "base"), []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	return client.New(h.URL, "test-token"), s, source
}

func TestHTTPWorkflowAndAuth(t *testing.T) {
	c, store, source := worker(t)
	ctx := context.Background()
	unauth := client.New(c.URL, "")
	if _, err := unauth.Snapshots(ctx); err == nil {
		t.Fatal("missing token accepted")
	}
	var health map[string]string
	if err := unauth.Do(ctx, "GET", "/live", nil, &health); err != nil {
		t.Fatal(err)
	}
	snap, err := c.Snapshot(ctx, engine.SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.CreatePool(ctx, engine.PoolOptions{Base: snap.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	e, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Apply the short deadline only to the intentionally blocked request;
	// filesystem publication should not race a 100ms test-only deadline.
	deadlineServer := httptest.NewServer(server.New(store, server.Options{Token: "test-token", RequestTimeout: 100 * time.Millisecond}))
	defer deadlineServer.Close()
	deadlinePool := client.New(deadlineServer.URL, "test-token").Pool(p.ID)
	if _, err = deadlinePool.Acquire(ctx); err == nil {
		t.Fatal("acquire exceeded capacity")
	} else {
		var apiErr *client.Error
		if !errors.As(err, &apiErr) || apiErr.Status != 408 {
			t.Fatalf("expected timeout: %v", err)
		}
	}
	if err = os.WriteFile(filepath.Join(e.Path, "new"), []byte("durable"), 0600); err != nil {
		t.Fatal(err)
	}
	promoted, err := p.Promote(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Manifest.Parent != snap.ID {
		t.Fatal("lost lineage")
	}
	reset, err := p.Reset(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Discard(ctx, e); err == nil {
		t.Fatal("stale generation accepted")
	}
	v, err := p.Verify(ctx, reset)
	if err != nil || !v.Valid {
		t.Fatalf("verify: %+v %v", v, err)
	}
	if err = p.Release(ctx, reset); err != nil {
		t.Fatal(err)
	}
	if err = p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", c.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "ramet_recycles_total 1") || !strings.Contains(string(b), "ramet_active_executions 0") {
		t.Fatalf("bad metrics: %s", b)
	}
	if err = c.Do(ctx, "POST", "/v1/worker/drain", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err = unauth.Do(ctx, "GET", "/ready", nil, nil); err == nil {
		t.Fatal("draining worker remained ready")
	}
}

func TestBadJSONAndBodyLimit(t *testing.T) {
	c, _, _ := worker(t)
	for _, test := range []struct {
		body   string
		status int
	}{{`{"unknown":true}`, 400}, {`{} {}`, 400}, {`{"source":"` + strings.Repeat("x", 1<<20) + `"}`, 413}} {
		req, _ := http.NewRequest("POST", c.URL+"/v1/snapshots", strings.NewReader(test.body))
		req.Header.Set("Authorization", "Bearer test-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != test.status {
			t.Fatalf("got %d want %d", resp.StatusCode, test.status)
		}
	}
}

func TestGCDryRunDefault(t *testing.T) {
	c, s, _ := worker(t)
	id := strings.Repeat("a", 64)
	name := filepath.Join(s.Root(), "objects", id)
	if err := os.WriteFile(name, []byte("orphan"), 0400); err != nil {
		t.Fatal(err)
	}
	var result engine.GCResult
	if err := c.Do(context.Background(), "POST", "/v1/gc", map[string]int{"grace_seconds": 0}, &result); err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || result.Objects != 1 {
		t.Fatalf("unexpected GC: %+v", result)
	}
	if _, err := os.Stat(name); err != nil {
		t.Fatal("dry-run deleted content")
	}
}

func TestPreparationAPI(t *testing.T) {
	c, s, source := worker(t)
	ctx := context.Background()
	snap, err := c.Snapshot(ctx, engine.SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	locality, err := c.Prefetch(ctx, snap.ID)
	if err != nil || locality.MissingBytes != 0 {
		t.Fatal(locality, err)
	}
	p, err := c.CreatePool(ctx, engine.PoolOptions{Base: snap.ID, Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	warm, err := p.Warm(ctx, 2)
	if err != nil || warm.Prepared != 2 || warm.Idle != 2 {
		t.Fatal(warm, err)
	}
	locality, err = c.Locality(ctx, snap.ID)
	if err != nil || locality.IdleExecutions != 2 {
		t.Fatal(locality, err)
	}
	before := s.Stats()
	all, err := p.AcquireMany(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range all {
		if err = p.Discard(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if after := s.Stats(); after.CopiedFiles != before.CopiedFiles || after.ClonedFiles != before.ClonedFiles {
		t.Fatal("acquire did not use warm trees")
	}
	if err = p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Warm(ctx, 1); err == nil {
		t.Fatal("closed pool accepted preparation")
	}
}

func TestPersistenceInspectionAPI(t *testing.T) {
	c, store, source := worker(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(source, "scratch"), []byte("discard"), 0600); err != nil {
		t.Fatal(err)
	}
	snap, err := c.Snapshot(ctx, engine.SnapshotOptions{Source: source, Policy: engine.Policy{Rules: []engine.Rule{{Path: "scratch", Class: engine.Ephemeral}}}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.CreatePool(ctx, engine.PoolOptions{Base: snap.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	e, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := p.PreviewPromotion(ctx, e)
	if err != nil || !preview.Valid || preview.OmittedBytes != 7 {
		t.Fatalf("preview %+v %v", preview, err)
	}
	snapshots, err := store.ListSnapshots(ctx)
	if err != nil || len(snapshots) != 1 {
		t.Fatal("preview wrote snapshot", err)
	}
	saved, err := p.Promote(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := c.RestorePlan(ctx, saved.ID)
	if err != nil || plan.SaveMode != "promotion" || !plan.Valid {
		t.Fatalf("plan %+v %v", plan, err)
	}
	plan, err = p.RestorePlan(ctx, e)
	if err != nil || plan.SaveMode != "base" {
		t.Fatalf("execution plan %+v %v", plan, err)
	}
	if err := p.Release(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PreviewPromotion(ctx, e); err == nil {
		t.Fatal("released lease accepted")
	}
}

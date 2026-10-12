// SPDX-License-Identifier: Apache-2.0
package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shenli/ramet/engine"
)

func TestTenantGatewayAndBoundedMetrics(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("shared"), 0644); err != nil {
		t.Fatal(err)
	}
	apis := map[string]*API{}
	pools := map[string]string{}
	for _, id := range []string{"a", "b"} {
		s, err := engine.Open(t.TempDir(), engine.Options{MaxExecutions: 1, RestrictSources: true, SourceRoots: []string{source}})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		snap, err := s.Snapshot(context.Background(), engine.SnapshotOptions{Source: source})
		if err != nil {
			t.Fatal(err)
		}
		p, err := s.CreatePool(context.Background(), engine.PoolOptions{Base: snap.ID, Concurrency: 1})
		if err != nil {
			t.Fatal(err)
		}
		pools[id] = p.ID()
		apis[id] = New(s, Options{Token: "token-" + id, MaxInflight: 1})
	}
	gateway, err := NewGateway(apis)
	if err != nil {
		t.Fatal(err)
	}
	request := func(token, method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Tenant-ID", "b")
		w := httptest.NewRecorder()
		gateway.ServeHTTP(w, r)
		return w
	}
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("tenant A selected state"), 0644); err != nil {
		t.Fatal(err)
	}
	private, err := apis["a"].store.Snapshot(context.Background(), engine.SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if w := request("token-b", "GET", "/v1/snapshots/"+private.ID); w.Code != 404 {
		t.Fatal("cross-tenant snapshot access", w.Code)
	}
	if w := request("token-a", "GET", "/v1/pools/"+pools["b"]); w.Code != 404 {
		t.Fatal("cross-tenant access", w.Code, w.Body)
	}
	operationID := issued(t, apis["a"].operations)
	acquire := httptest.NewRequest("POST", "/v1/pools/"+pools["a"]+"/acquire", nil)
	acquire.Header.Set("Authorization", "Bearer token-a")
	acquire.Header.Set("Idempotency-Key", operationID)
	saved := httptest.NewRecorder()
	gateway.ServeHTTP(saved, acquire)
	if saved.Code != 200 {
		t.Fatal(saved.Code, saved.Body.String())
	}
	if w := request("token-a", "GET", "/v1/operations/"+operationID); w.Code != 200 || !strings.Contains(w.Body.String(), "lease") {
		t.Fatal("owner receipt unavailable", w.Code)
	}
	if w := request("token-b", "GET", "/v1/operations/"+operationID); w.Code != 409 || strings.Contains(w.Body.String(), "lease") {
		t.Fatal("cross-tenant receipt access", w.Code)
	}
	apis["a"].admission <- struct{}{}
	if w := request("token-a", "GET", "/v1/snapshots"); w.Code != 429 {
		t.Fatal(w.Code)
	}
	if w := request("token-b", "GET", "/v1/snapshots"); w.Code != 200 {
		t.Fatal("tenant quota affected neighbor", w.Code)
	}
	<-apis["a"].admission
	if w := request("wrong", "GET", "/v1/snapshots"); w.Code != 401 {
		t.Fatal(w.Code)
	}
	for i := 0; i < 100; i++ {
		request("token-a", "GET", fmt.Sprintf("/unknown/%d", i))
	}
	w := request("token-a", "GET", "/metrics")
	if w.Code != 200 || strings.Contains(w.Body.String(), pools["b"]) || strings.Contains(w.Body.String(), "/unknown/99") || !strings.Contains(w.Body.String(), "ramet_http_request_duration_seconds_count") {
		t.Fatal("metrics labels", w.Body)
	}
	apis["b"].opts.Token = "token-a"
	if _, err = NewGateway(apis); err == nil {
		t.Fatal("duplicate tenant credentials accepted")
	}
}

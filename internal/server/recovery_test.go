// SPDX-License-Identifier: Apache-2.0
package server_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shenli/ramet/client"
	"github.com/shenli/ramet/engine"
	"github.com/shenli/ramet/internal/server"
)

type dropResponse struct {
	base http.RoundTripper
	drop bool
}

func (d *dropResponse) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := d.base.RoundTrip(r)
	if err == nil && d.drop {
		d.drop = false
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return nil, fmt.Errorf("injected response loss after commit")
	}
	return response, err
}
func TestLostResetResponseRecoveredExactlyOnce(t *testing.T) {
	c, store, source := worker(t)
	ctx := context.Background()
	snap, err := c.Snapshot(ctx, engine.SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.CreatePool(ctx, engine.PoolOptions{Base: snap.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.NewOperation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	keyed := client.WithOperationID(ctx, id)
	c.HTTP.Transport = &dropResponse{base: http.DefaultTransport, drop: true}
	_, err = p.Reset(keyed, execution)
	var unknown *client.OutcomeUnknownError
	if !errors.As(err, &unknown) || unknown.OperationID != id {
		t.Fatalf("lost response lacks recovery identity: %v", err)
	}
	receipt, err := c.Operation(ctx, id)
	if err != nil || receipt.State != "completed" || receipt.Status != 200 {
		t.Fatal(receipt, err)
	}
	reset, err := p.Reset(keyed, execution)
	if err != nil {
		t.Fatal(err)
	}
	if reset.Generation != execution.Generation+1 {
		t.Fatal(reset.Generation)
	}
	verified, err := p.Verify(ctx, reset)
	if err != nil || !verified.Valid {
		t.Fatal(verified, err)
	}
	if store.Stats().Resets != 1 {
		t.Fatal("reset executed twice", store.Stats())
	}
	_, err = p.Reset(keyed, reset)
	var rejected *client.Error
	if !errors.As(err, &rejected) || rejected.Code != "operation_conflict" {
		t.Fatal("changed request accepted", err)
	}
	if err = p.Release(ctx, reset); err != nil {
		t.Fatal(err)
	}
	// A different API session must never reinterpret an old ID as a new mutation.
	restarted := httptest.NewServer(server.New(store, server.Options{Token: "test-token"}))
	defer restarted.Close()
	other := client.New(restarted.URL, "test-token")
	_, err = other.Operation(ctx, id)
	if !errors.As(err, &rejected) || rejected.Code != "operation_session_changed" {
		t.Fatal(err)
	}
}
func TestStructuredRoutingAndRequestCorrelation(t *testing.T) {
	c, _, _ := worker(t)
	for _, test := range []struct {
		method, path, code string
		status             int
	}{{"GET", "/v1/missing", "endpoint_not_found", 404}, {"PATCH", "/v1/snapshots", "method_not_allowed", 405}} {
		err := c.Do(context.Background(), test.method, test.path, nil, nil)
		var e *client.Error
		if !errors.As(err, &e) || e.Status != test.status || e.Code != test.code || e.RequestID == "" {
			t.Fatal(test, err)
		}
	}
	unauth := client.New(c.URL, "")
	err := unauth.Do(context.Background(), "GET", "/v1/capabilities", nil, nil)
	var e *client.Error
	if !errors.As(err, &e) || e.Status != 401 || e.RequestID == "" {
		t.Fatal(err)
	}
}

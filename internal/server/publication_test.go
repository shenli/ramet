// SPDX-License-Identifier: Apache-2.0
package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shenli/ramet/client"
	"github.com/shenli/ramet/engine"
	"github.com/shenli/ramet/internal/server"
)

func TestPublicationResponseLossAndWorkerRestart(t *testing.T) {
	ctx := context.Background()
	c, store, source := worker(t)
	base, err := c.Snapshot(ctx, engine.SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.CreatePool(ctx, engine.PoolOptions{Base: base.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	e, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(e.Path, "report"), []byte("saved"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := engine.PublishOptions{Publication: engine.Publication{ID: strings.Repeat("c", 64),
		RequestDigest: strings.Repeat("d", 64), Result: json.RawMessage(`{"done":true}`)}, Mode: "checkpoint"}
	c.HTTP.Transport = &dropResponse{base: http.DefaultTransport, drop: true}
	if _, err = p.Publish(ctx, e, opts); err == nil {
		t.Fatal("response was not dropped")
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := engine.Open(store.Root(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	h := httptest.NewServer(server.New(replacement, server.Options{Token: "test-token"}))
	defer h.Close()
	resumed := client.New(h.URL, "test-token")
	found, err := resumed.Publication(ctx, opts.Publication.ID)
	if err != nil || string(found.Manifest.Publication.Result) != `{"done":true}` {
		t.Fatal(found, err)
	}
	unauthorized := client.New(h.URL, "")
	if _, err := unauthorized.Publication(ctx, opts.Publication.ID); err == nil {
		t.Fatal("unauthorized lookup")
	}
	missing, err := resumed.Publication(ctx, strings.Repeat("e", 64))
	var apiError *client.Error
	if !errors.As(err, &apiError) || apiError.Status != 404 {
		t.Fatal(missing, err)
	}
	newPool, err := resumed.CreatePool(ctx, engine.PoolOptions{Base: base.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	newExecution, err := newPool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := newPool.Publish(ctx, newExecution, opts)
	if err != nil || replayed.ID != found.ID {
		t.Fatal("replay recaptured base", replayed.ID, err)
	}
	opts.Publication.RequestDigest = strings.Repeat("f", 64)
	if _, err = newPool.Publish(ctx, newExecution, opts); !errors.As(err, &apiError) || apiError.Status != 409 {
		t.Fatal("changed context accepted", err)
	}
}

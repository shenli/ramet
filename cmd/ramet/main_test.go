// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shenli/ramet/engine"
	"github.com/shenli/ramet/internal/benchmark"
	"github.com/shenli/ramet/internal/server"
)

func TestDemo(t *testing.T) {
	var out, stderr bytes.Buffer
	if err := run(context.Background(), []string{"demo", "--concurrency", "2", "--iterations", "4"}, &out, &stderr); err != nil {
		t.Fatal(err, stderr.String())
	}
	var r benchmark.Result
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Successful != 4 || r.DurableBytesDuringExecutions != 0 || r.Stats.VerificationFailures != 0 {
		t.Fatalf("invalid demo result: %+v", r)
	}
}

func TestNonLoopbackRequiresAuth(t *testing.T) {
	t.Setenv("RAMET_TOKEN", "")
	var out bytes.Buffer
	err := run(context.Background(), []string{"serve", "--listen", "0.0.0.0:7433", "--data", t.TempDir()}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "RAMET_TOKEN") {
		t.Fatalf("insecure listener accepted: %v", err)
	}
}

func TestPersistenceCLI(t *testing.T) {
	ctx := context.Background()
	store, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "fixed"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Snapshot(ctx, engine.SnapshotOptions{Source: source, Policy: engine.Policy{Rules: []engine.Rule{{Path: "fixed", Class: engine.SharedImmutable}}}})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := store.CreatePool(ctx, engine.PoolOptions{Base: snap.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(execution)
	if err != nil {
		t.Fatal(err)
	}
	handle := filepath.Join(t.TempDir(), "execution.json")
	if err := os.WriteFile(handle, b, 0600); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.New(store, server.Options{}))
	defer httpServer.Close()
	var output, stderr bytes.Buffer
	if err := run(ctx, []string{"preview-promotion", "--url", httpServer.URL, "--execution", handle}, &output, &stderr); err != nil {
		t.Fatal(err)
	}
	var preview engine.PromotionPreview
	if err := json.Unmarshal(output.Bytes(), &preview); err != nil || !preview.Valid {
		t.Fatal(preview, err)
	}
	if err := os.WriteFile(filepath.Join(execution.Path, "fixed"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run(ctx, []string{"preview-promotion", "--url", httpServer.URL, "--execution", handle}, &output, &stderr); err == nil {
		t.Fatal("invalid preview exited successfully")
	}
	if err := json.Unmarshal(output.Bytes(), &preview); err != nil || preview.Valid {
		t.Fatal(preview, err)
	}
	output.Reset()
	if err := run(ctx, []string{"restore-plan", "--url", httpServer.URL, "--id", snap.ID}, &output, &stderr); err != nil {
		t.Fatal(err)
	}
	var plan engine.RestorePlan
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil || plan.SaveMode != "base" {
		t.Fatal(plan, err)
	}
}

func TestAXPreflightAndJSONErrors(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"snapshot", "--url", "http://127.0.0.1:1"}, "--source or --git is required"},
		{[]string{"pool", "--url", "http://127.0.0.1:1"}, "--base is required"},
		{[]string{"snapshot", "--git", "repo"}, "--revision is required"},
	} {
		var stdout, stderr bytes.Buffer
		err := run(context.Background(), test.args, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatal(err, test.want)
		}
		emitError(&stderr, err, true)
		var output map[string]any
		if err = json.Unmarshal(stderr.Bytes(), &output); err != nil || output["code"] != "invalid_argument" {
			t.Fatal(stderr.String(), err)
		}
	}
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(`{"rulez":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"policy-validate", "--policy", policy}, {"snapshot", "--source", "/project", "--policy", policy, "--url", "http://127.0.0.1:1"}} {
		var output bytes.Buffer
		if err := run(context.Background(), args, &output, &output); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatal(err)
		}
	}
}

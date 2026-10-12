// SPDX-License-Identifier: Apache-2.0
package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/shenli/ramet/engine"
	"github.com/shenli/ramet/storage"
	"github.com/shenli/ramet/storage/postgres"
	"github.com/shenli/ramet/storage/s3"
)

func setup(t *testing.T) (context.Context, string, *s3.Store) {
	t.Helper()
	if os.Getenv("RAMET_TEST_POSTGRES") == "" {
		t.Skip("set RAMET_TEST_POSTGRES and RAMET_TEST_S3_* for real provider tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	namespace := fmt.Sprintf("test-%d", time.Now().UnixNano())
	endpoint := os.Getenv("RAMET_TEST_S3_ENDPOINT")
	bucket := os.Getenv("RAMET_TEST_S3_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Fatal("S3 endpoint and dedicated test bucket are required")
	}
	key, secret := os.Getenv("RAMET_TEST_S3_ACCESS_KEY"), os.Getenv("RAMET_TEST_S3_SECRET_KEY")
	secure := strings.HasPrefix(endpoint, "https://")
	c, err := minio.New(strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://"), &minio.Options{Creds: credentials.NewStaticV4(key, secret, ""), Secure: secure, Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	exists, err := c.BucketExists(ctx, bucket)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		if err = c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
			t.Fatal(err)
		}
	}
	content, err := s3.New(s3.Options{Endpoint: endpoint, Bucket: bucket, Prefix: namespace, Region: "us-east-1", AccessKey: key, SecretKey: secret})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, namespace, content
}
func metadata(t *testing.T, ctx context.Context, namespace string) *postgres.Store {
	t.Helper()
	m, err := postgres.Open(ctx, os.Getenv("RAMET_TEST_POSTGRES"), namespace)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestProviderConformance(t *testing.T) {
	ctx, ns, c := setup(t)
	m := metadata(t, ctx, ns)
	defer m.Close()
	if err := m.BindContent(ctx, c.Identity()); err != nil {
		t.Fatal(err)
	}
	if err := m.BindContent(ctx, "different-store"); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("content binding changed", err)
	}
	if other, err := postgres.Open(ctx, os.Getenv("RAMET_TEST_POSTGRES"), ns); err == nil {
		other.Close()
		t.Fatal("second namespace writer accepted")
	}
	b := []byte("shared durable content")
	ref := storage.Ref{Digest: digest(b), Size: int64(len(b))}
	added, err := c.Put(ctx, ref, bytes.NewReader(b))
	if err != nil || !added {
		t.Fatal(added, err)
	}
	added, err = c.Put(ctx, ref, bytes.NewReader(b))
	if err != nil || added {
		t.Fatal("content put was not idempotent", added, err)
	}
	if _, err = c.Put(ctx, ref, bytes.NewReader([]byte("wrong bytes"))); !errors.Is(err, storage.ErrCorrupt) {
		t.Fatal(err)
	}
	object, err := c.Open(ctx, ref.Digest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(object)
	object.Close()
	if err != nil || !bytes.Equal(got, b) {
		t.Fatal("object differs", err)
	}
	if _, err = c.Open(ctx, strings.Repeat("a", 64)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
	r := storage.Record{ID: digest(b), Data: b}
	if err = m.Publish(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err = m.Publish(ctx, r); err != nil {
		t.Fatal("publication not idempotent", err)
	}
	child := []byte("child manifest")
	cr := storage.Record{ID: digest(child), Parent: r.ID, Data: child}
	if err = m.Publish(ctx, cr); err != nil {
		t.Fatal(err)
	}
	if err = m.Delete(ctx, r.ID); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("referenced parent deleted", err)
	}
	all, err := m.List(ctx)
	if err != nil || len(all) != 2 {
		t.Fatal(len(all), err)
	}
	gotRecord, err := m.Get(ctx, cr.ID)
	if err != nil || gotRecord.Parent != r.ID || !bytes.Equal(gotRecord.Data, child) {
		t.Fatal("metadata differs", err)
	}
	if err = m.Delete(ctx, cr.ID); err != nil {
		t.Fatal(err)
	}
	if err = m.Delete(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Get(ctx, r.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestRemoteWorkerReplacement(t *testing.T) {
	ctx, ns, c := setup(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "source.txt"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	first, err := engine.OpenContext(ctx, t.TempDir(), engine.Options{Metadata: metadata(t, ctx, ns), Content: c})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	base, err := first.Snapshot(ctx, engine.SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	p, err := first.CreatePool(ctx, engine.PoolOptions{Base: base.ID, Concurrency: 8})
	if err != nil {
		t.Fatal(err)
	}
	before := first.Stats().RemoteBytesUploaded
	executions, err := p.AcquireMany(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range executions {
		if err = os.WriteFile(filepath.Join(e.Path, "attempt"), []byte("ephemeral"), 0600); err != nil {
			t.Fatal(err)
		}
		if err = p.Release(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if first.Stats().RemoteBytesUploaded != before {
		t.Fatal("ephemeral loop wrote remote bytes")
	}
	e, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(e.Path, "result"), []byte("selected"), 0600); err != nil {
		t.Fatal(err)
	}
	selected, err := p.Promote(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	// A completely different, empty cache proves remote durability, not reuse
	// of local manifests or objects left behind by the first worker.
	second, err := engine.OpenContext(ctx, t.TempDir(), engine.Options{Metadata: metadata(t, ctx, ns), Content: c})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	locality, err := second.Locality(ctx, selected.ID)
	if err != nil || locality.CachedObjects != 0 || locality.MissingBytes == 0 {
		t.Fatal(locality, err)
	}
	locality, err = second.Prefetch(ctx, selected.ID)
	if err != nil || locality.MissingBytes != 0 || locality.CachedObjects != locality.Objects {
		t.Fatal(locality, err)
	}
	downloaded := second.Stats().RemoteBytesDownloaded
	if _, err = second.Prefetch(ctx, selected.ID); err != nil {
		t.Fatal(err)
	}
	if second.Stats().RemoteBytesDownloaded != downloaded {
		t.Fatal("prefetch fetched cached bytes again")
	}
	resumed, err := second.CreatePool(ctx, engine.PoolOptions{Base: selected.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resumed.Warm(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if second.Stats().RemoteBytesDownloaded != downloaded {
		t.Fatal("warm fetched prefetched bytes again")
	}
	restored, err := resumed.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(restored.Path, "result"))
	if err != nil || string(b) != "selected" {
		t.Fatal("promotion not durable", err)
	}
	if second.Stats().RemoteBytesDownloaded == 0 {
		t.Fatal("empty-cache resume did not hydrate")
	}
	report, err := second.Doctor(ctx)
	if err != nil || !report.OK || report.Durability != "remote" {
		t.Fatalf("doctor: %+v %v", report, err)
	}
}

type failingMetadata struct{ storage.Metadata }

func (f failingMetadata) Publish(context.Context, storage.Record) error {
	return errors.New("injected metadata failure")
}

type failingContent struct{ storage.Content }

func (f failingContent) Put(context.Context, storage.Ref, io.ReadSeeker) (bool, error) {
	return false, errors.New("injected upload failure")
}

func TestRemotePublicationFailures(t *testing.T) {
	for _, stage := range []string{"upload", "metadata"} {
		t.Run(stage, func(t *testing.T) {
			ctx, ns, c := setup(t)
			m := metadata(t, ctx, ns)
			opts := engine.Options{Metadata: m, Content: c}
			if stage == "upload" {
				opts.Content = failingContent{c}
			} else {
				opts.Metadata = failingMetadata{m}
			}
			s, err := engine.OpenContext(ctx, t.TempDir(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			source := t.TempDir()
			if err = os.WriteFile(filepath.Join(source, "file"), []byte("irreproducible"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Snapshot(ctx, engine.SnapshotOptions{Source: source}); err == nil {
				t.Fatal("failed durable capture acknowledged")
			}
			records, err := m.List(ctx)
			if err != nil || len(records) != 0 {
				t.Fatal("partial manifest published", err)
			}
		})
	}
}

func TestPostgresOwnershipLossFailsClosed(t *testing.T) {
	ctx, ns, c := setup(t)
	m := metadata(t, ctx, ns)
	if err := m.BindContent(ctx, c.Identity()); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	admin, err := pgx.Connect(ctx, os.Getenv("RAMET_TEST_POSTGRES"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var killed bool
	if err = admin.QueryRow(ctx, "SELECT pg_terminate_backend(pid,5000) FROM pg_stat_activity WHERE application_name=$1", "ramet/"+ns).Scan(&killed); err != nil || !killed {
		t.Fatal("failed to terminate test owner", err)
	}
	replacement := metadata(t, ctx, ns)
	defer replacement.Close()
	b := []byte("must not reconnect")
	if err := m.Publish(ctx, storage.Record{ID: digest(b), Data: b}); err == nil {
		t.Fatal("lost owner silently reconnected")
	}
}

func TestPostgresRejectsFutureSchema(t *testing.T) {
	ctx, ns, _ := setup(t)
	m := metadata(t, ctx, ns)
	m.Close()
	admin, err := pgx.Connect(ctx, os.Getenv("RAMET_TEST_POSTGRES"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	if _, err = admin.Exec(ctx, "UPDATE ramet_schema_version SET version=999 WHERE singleton"); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "UPDATE ramet_schema_version SET version=1 WHERE singleton")
	if m, err = postgres.Open(ctx, os.Getenv("RAMET_TEST_POSTGRES"), ns); err == nil {
		m.Close()
		t.Fatal("newer schema accepted")
	}
}

// Delayed reads let tests assert cancellation while preparation owns capacity.
type blockedContent struct {
	storage.Content
	entered chan struct{}
	once    sync.Once
}

func (c *blockedContent) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	c.once.Do(func() { close(c.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestWarmCancellationAndDrain(t *testing.T) {
	ctx, ns, c := setup(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "base"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := engine.OpenContext(ctx, t.TempDir(), engine.Options{Metadata: metadata(t, ctx, ns), Content: c})
	if err != nil {
		t.Fatal(err)
	}
	base, err := first.Snapshot(ctx, engine.SnapshotOptions{Source: source})
	closeErr := first.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	blocked := &blockedContent{Content: c, entered: make(chan struct{})}
	second, err := engine.OpenContext(ctx, t.TempDir(), engine.Options{Metadata: metadata(t, ctx, ns), Content: blocked, MaxExecutions: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	p, err := second.CreatePool(ctx, engine.PoolOptions{Base: base.ID, Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	warmCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := p.Warm(warmCtx, 2); done <- err }()
	select {
	case <-blocked.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err = p.Close(); !errors.Is(err, engine.ErrConflict) {
		t.Fatal(err)
	}
	if _, err = p.Warm(ctx, 2); !errors.Is(err, engine.ErrConflict) {
		t.Fatal(err)
	}
	if stats := second.Stats(); stats.PreparingExecutions != 2 || stats.ActiveExecutions != 0 {
		t.Fatal(stats)
	}
	waitCtx, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	_, err = p.Acquire(waitCtx)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err = second.Drain(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if stats := second.Stats(); stats.PreparingExecutions != 0 || stats.IdleExecutions != 0 {
		t.Fatal(stats)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPersistenceContractSurvivesWorkerReplacement(t *testing.T) {
	ctx, ns, content := setup(t)
	source := t.TempDir()
	for name, value := range map[string]string{"lock": "pinned", "generated": "rebuild", "scratch": "discard"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	first, err := engine.OpenContext(ctx, t.TempDir(), engine.Options{Metadata: metadata(t, ctx, ns), Content: content})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	base, err := first.Snapshot(ctx, engine.SnapshotOptions{Source: source, Policy: engine.Policy{Rules: []engine.Rule{
		{Path: "generated", Class: engine.Rebuildable, Recipe: "build", Inputs: []string{"lock"}},
		{Path: "scratch", Class: engine.Ephemeral},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := first.CreatePool(ctx, engine.PoolOptions{Base: base.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := first.Stats().RemoteBytesUploaded
	preview, err := pool.PreviewPromotion(ctx, execution)
	if err != nil || !preview.Valid {
		t.Fatal(preview, err)
	}
	if first.Stats().RemoteBytesUploaded != before {
		t.Fatal("preview uploaded content")
	}
	saved, err := pool.Promote(ctx, execution)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := engine.OpenContext(ctx, t.TempDir(), engine.Options{Metadata: metadata(t, ctx, ns), Content: content})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	plan, err := replacement.RestorePlan(ctx, saved.ID)
	if err != nil || !plan.Valid || plan.SaveMode != "promotion" || len(plan.Steps) != 1 || plan.Steps[0].Inputs[0].SHA256 != digest([]byte("pinned")) {
		t.Fatal(plan, err)
	}
	restored, err := replacement.CreatePool(ctx, engine.PoolOptions{Base: saved.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	e, err := restored.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.Path, "generated")); !os.IsNotExist(err) {
		t.Fatal("rebuildable bytes survived")
	}
	executionPlan, err := restored.RestorePlan(ctx, e)
	if err != nil || !executionPlan.Valid {
		t.Fatal(executionPlan, err)
	}
}

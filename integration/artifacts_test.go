// SPDX-License-Identifier: Apache-2.0
package integration_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/shenli/ramet/engine"
	"github.com/shenli/ramet/storage"
	"github.com/shenli/ramet/storage/s3"
)

type transferCounts struct {
	Heads      int64 `json:"head_requests"`
	Gets       int64 `json:"get_requests"`
	Puts       int64 `json:"put_requests"`
	Uploaded   int64 `json:"uploaded_bytes"`
	Downloaded int64 `json:"downloaded_bytes"`
}
type observedTransport struct {
	heads, gets, puts, uploaded, downloaded atomic.Int64
}
type countedBody struct {
	io.ReadCloser
	count *atomic.Int64
}

func (b countedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.count.Add(int64(n))
	return n, err
}
func (c *observedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.Method {
	case http.MethodHead:
		c.heads.Add(1)
	case http.MethodGet:
		c.gets.Add(1)
	case http.MethodPut:
		c.puts.Add(1)
		if req.Body != nil {
			req.Body = countedBody{req.Body, &c.uploaded}
		}
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err == nil && req.Method == http.MethodGet {
		resp.Body = countedBody{resp.Body, &c.downloaded}
	}
	return resp, err
}
func (c *observedTransport) counts() transferCounts {
	return transferCounts{c.heads.Load(), c.gets.Load(), c.puts.Load(), c.uploaded.Load(), c.downloaded.Load()}
}
func (c transferCounts) since(old transferCounts) transferCounts {
	return transferCounts{c.Heads - old.Heads, c.Gets - old.Gets, c.Puts - old.Puts, c.Uploaded - old.Uploaded, c.Downloaded - old.Downloaded}
}
func observedContent(t *testing.T, ns string) (*s3.Store, *observedTransport) {
	t.Helper()
	counts := &observedTransport{}
	c, err := s3.New(s3.Options{Endpoint: os.Getenv("RAMET_TEST_S3_ENDPOINT"), Bucket: os.Getenv("RAMET_TEST_S3_BUCKET"), Prefix: ns,
		Region: "us-east-1", AccessKey: os.Getenv("RAMET_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("RAMET_TEST_S3_SECRET_KEY"), Transport: counts})
	if err != nil {
		t.Fatal(err)
	}
	return c, counts
}

func TestChunkedRemoteRecovery(t *testing.T) {
	for _, backend := range []string{"copy", "lazy"} {
		t.Run(backend, func(t *testing.T) {
			if backend == "lazy" && (runtime.GOOS != "linux" || os.Getenv("RAMET_TEST_FUSE") != "1") {
				t.Skip("requires Linux FUSE")
			}
			ctx, ns, _ := setup(t)
			c, counts := observedContent(t, ns)
			data := make([]byte, 24<<20)
			_, _ = rand.New(rand.NewSource(73)).Read(data)
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "artifact.bin"), data, 0600); err != nil {
				t.Fatal(err)
			}
			s, err := engine.OpenContext(ctx, t.TempDir(), engine.Options{Metadata: metadata(t, ctx, ns), Content: c, CacheBytes: 4 << 20, Backend: backend})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			base, err := s.Snapshot(ctx, engine.SnapshotOptions{Source: source})
			if err != nil {
				t.Fatal(err)
			}
			p, err := s.CreatePool(ctx, engine.PoolOptions{Base: base.ID, Concurrency: 1})
			if err != nil {
				t.Fatal(err)
			}
			e, err := p.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(e.Path, "artifact.bin"))
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("base restore", err)
			}
			before := counts.counts()
			if _, err = p.Checkpoint(ctx, e); err != nil {
				t.Fatal(err)
			}
			transfer := counts.counts().since(before)
			if transfer.Gets != 0 || transfer.Puts != 0 {
				t.Fatalf("unchanged checkpoint transferred bodies: %+v", transfer)
			}
			copy(data[12<<20:], []byte("updated artifact contents"))
			if err = os.WriteFile(filepath.Join(e.Path, "artifact.bin"), data, 0600); err != nil {
				t.Fatal(err)
			}
			before = counts.counts()
			saved, err := p.Checkpoint(ctx, e)
			if err != nil {
				t.Fatal(err)
			}
			transfer = counts.counts().since(before)
			if transfer.Gets != 0 || transfer.Uploaded <= 0 || transfer.Uploaded >= int64(len(data)/2) {
				t.Fatalf("localized edit did not reuse chunks: %+v", transfer)
			}
			t.Logf("localized edit of %d bytes: %+v", len(data), transfer)
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			// Empty cache, same remote namespace: every byte must come from durable chunks.
			reads := &interruptedArtifactReads{Content: c}
			s, err = engine.OpenContext(ctx, t.TempDir(), engine.Options{Metadata: metadata(t, ctx, ns), Content: reads, CacheBytes: 4 << 20, Backend: backend})
			if err != nil {
				t.Fatal(err)
			}
			p, err = s.CreatePool(ctx, engine.PoolOptions{Base: saved.ID, Concurrency: 1})
			if err != nil {
				t.Fatal(err)
			}
			e, err = p.Acquire(ctx)
			if backend == "copy" {
				if err == nil {
					t.Fatal("interrupted restore exposed an execution")
				}
				e, err = p.Acquire(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			if backend == "lazy" {
				if _, err = os.ReadFile(filepath.Join(e.Path, "artifact.bin")); err == nil {
					t.Fatal("interrupted lazy hydration returned partial artifact")
				}
			}
			got, err = os.ReadFile(filepath.Join(e.Path, "artifact.bin"))
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("replacement worker lost artifact", err)
			}
			if v, err := p.Verify(ctx, e); err != nil || !v.Valid {
				t.Fatal(v, err)
			}
			status, err := s.CacheStatus()
			if err != nil || status.Bytes > status.LimitBytes || status.PinnedObjects != 0 || status.ReservedDiskBytes != 0 {
				t.Fatal(status, err)
			}
		})
	}
}

// Fail the second chunk read once, after partial reconstruction has begun.
type interruptedArtifactReads struct {
	storage.Content
	opens atomic.Int32
}

func (c *interruptedArtifactReads) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	if c.opens.Add(1) == 2 {
		return nil, errors.New("injected interrupted chunk download")
	}
	return c.Content.Open(ctx, id)
}

type interruptedChunks struct {
	storage.Content
	calls  int
	failAt int
}

func (c *interruptedChunks) Put(ctx context.Context, ref storage.Ref, src io.ReadSeeker) (bool, error) {
	c.calls++
	if c.calls == c.failAt {
		return false, errors.New("injected interrupted chunk upload")
	}
	return c.Content.Put(ctx, ref, src)
}

func TestInterruptedChunkPublication(t *testing.T) {
	ctx, ns, content := setup(t)
	m := metadata(t, ctx, ns)
	c := &interruptedChunks{Content: content, failAt: 2}
	s, err := engine.OpenContext(ctx, t.TempDir(), engine.Options{Metadata: m, Content: c})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	data := make([]byte, 12<<20)
	_, _ = rand.New(rand.NewSource(83)).Read(data)
	source := t.TempDir()
	if err = os.WriteFile(filepath.Join(source, "artifact.bin"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Snapshot(ctx, engine.SnapshotOptions{Source: source}); err == nil {
		t.Fatal("partial upload acknowledged")
	}
	if records, err := m.List(ctx); err != nil || len(records) != 0 {
		t.Fatal("partial snapshot published", records, err)
	}
	if s.Stats().RemoteBytesUploaded == 0 {
		t.Fatal("failure was not after a successful chunk")
	}
	c.failAt = 0
	saved, err := s.Snapshot(ctx, engine.SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePool(ctx, engine.PoolOptions{Base: saved.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	e, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(e.Path, "artifact.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("retry lost bytes", err)
	}
}

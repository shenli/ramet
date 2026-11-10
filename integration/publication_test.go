// SPDX-License-Identifier: Apache-2.0
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shenli/ramet/engine"
	"github.com/shenli/ramet/storage"
)

type publicationReplyLoss struct {
	storage.Metadata
	failAfter bool
}

func (m *publicationReplyLoss) Publish(ctx context.Context, r storage.Record) error {
	if err := m.Metadata.Publish(ctx, r); err != nil {
		return err
	}
	if m.failAfter {
		return errors.New("injected loss after durable metadata commit")
	}
	return nil
}

func TestPublicationReconcilesRemoteCommitWithoutReply(t *testing.T) {
	ctx, ns, content := setup(t)
	provider := &publicationReplyLoss{Metadata: metadata(t, ctx, ns)}
	first, err := engine.OpenContext(ctx, t.TempDir(), engine.Options{Metadata: provider, Content: content})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	source := t.TempDir()
	if err = os.WriteFile(filepath.Join(source, "report"), []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	base, err := first.Snapshot(ctx, engine.SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	p, err := first.CreatePool(ctx, engine.PoolOptions{Base: base.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	e, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(e.Path, "report"), []byte("completed"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := engine.PublishOptions{Publication: engine.Publication{ID: strings.Repeat("1", 64),
		RequestDigest: strings.Repeat("2", 64), Result: json.RawMessage(`{"report":"completed"}`)}, Mode: "checkpoint"}
	provider.failAfter = true
	if _, err = p.Publish(ctx, e, opts); err == nil {
		t.Fatal("commit reply loss not injected")
	}
	found, err := first.GetPublication(ctx, opts.Publication.ID)
	if err != nil {
		t.Fatal("cached miss hid a committed publication", err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := engine.OpenContext(ctx, t.TempDir(), engine.Options{Metadata: metadata(t, ctx, ns), Content: content})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	recovered, err := second.GetPublication(ctx, opts.Publication.ID)
	if err != nil || recovered.ID != found.ID {
		t.Fatal(recovered, err)
	}
	p2, err := second.CreatePool(ctx, engine.PoolOptions{Base: recovered.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	e2, err := p2.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(e2.Path, "report"))
	if err != nil || string(data) != "completed" {
		t.Fatal(string(data), err)
	}
	if string(recovered.Manifest.Publication.Result) != `{"report":"completed"}` {
		t.Fatal("result mismatch")
	}
}

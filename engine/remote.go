// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/shenli/ramet/internal/fsutil"
	"github.com/shenli/ramet/storage"
)

func (s *Store) initializeRemote(ctx context.Context) error {
	binding := filepath.Join(s.root, "REMOTE")
	previous, err := os.ReadFile(binding)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if s.opts.Metadata == nil {
		if err == nil {
			return fmt.Errorf("%w: this cache belongs to remote storage; configure the original providers", ErrInvalid)
		}
		return nil
	}
	identity := s.opts.Content.Identity()
	if err == nil && string(previous) != identity {
		return fmt.Errorf("%w: content provider does not match this cache", ErrConflict)
	}
	entries, err := os.ReadDir(filepath.Join(s.root, "snapshots"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".json" {
			return fmt.Errorf("%w: use a new cache directory for remote storage; local snapshots exist", ErrConflict)
		}
	}
	if err = s.opts.Metadata.Ping(ctx); err != nil {
		return err
	}
	if err = s.opts.Content.Ping(ctx); err != nil {
		return err
	}
	if err = s.opts.Metadata.BindContent(ctx, identity); err != nil {
		return err
	}
	return fsutil.AtomicWrite(binding, []byte(identity), 0600)
}

func (s *Store) publishSnapshot(ctx context.Context, id string, m Manifest, data []byte) error {
	if s.opts.Metadata == nil {
		return fsutil.AtomicWrite(filepath.Join(s.root, "snapshots", id+".json"), data, 0600)
	}
	// Every object created by putFile is uploaded before it enters the cache.
	// Inherited references already belong to a published snapshot. Confirm any
	// cached/durable references through a pinned read before metadata publication.
	seen := map[string]bool{}
	for _, e := range s.contentRefs(m) {
		if seen[e.Digest] {
			continue
		}
		seen[e.Digest] = true
		if err := s.withObject(ctx, e, func(name string) error {
			f, err := os.Open(name)
			if err != nil {
				return err
			}
			defer f.Close()
			added, err := s.opts.Content.Put(ctx, storage.Ref{Digest: e.Digest, Size: e.Size}, f)
			if err == nil && added {
				s.metrics.uploaded.Add(uint64(e.Size))
			}
			return err
		}); err != nil {
			return err
		}
	}
	// Remote metadata cannot become visible until every remote object confirms
	// durability. A failed upload or metadata call never returns durable success.
	return s.opts.Metadata.Publish(ctx, storage.Record{ID: id, Parent: m.Parent, Data: data})
}

func (s *Store) ensureObject(ctx context.Context, e Entry) error {
	return s.withObject(ctx, e, func(string) error { return nil })
}
func (s *Store) downloadObject(ctx context.Context, e Entry) error {
	release, err := s.reserveDisk(e.Size)
	if err != nil {
		return err
	}
	defer release()
	dest := filepath.Join(s.root, "objects", e.Digest)
	in, err := s.opts.Content.Open(ctx, e.Digest)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Join(s.root, "staging"), "hydrate-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := fsutil.Copy(ctx, io.MultiWriter(tmp, h), io.LimitReader(in, e.Size+1))
	if err == nil && (n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.Digest) {
		err = fmt.Errorf("%w: remote object digest mismatch", ErrVerification)
	}
	if err == nil {
		err = tmp.Chmod(0400)
	}
	if err == nil {
		err = tmp.Sync()
	}
	err = errors.Join(err, tmp.Close())
	if err != nil {
		return err
	}
	if err = os.Link(tmp.Name(), dest); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err = fsutil.SyncDir(filepath.Dir(dest)); err != nil {
		return err
	}
	s.metrics.downloaded.Add(uint64(n))
	return nil
}

func (s *Store) verifyDurableObject(ctx context.Context, e Entry) error {
	if s.opts.Content == nil {
		return s.verifyObject(ctx, e)
	}
	r, err := s.opts.Content.Open(ctx, e.Digest)
	if err != nil {
		return err
	}
	defer r.Close()
	h := sha256.New()
	n, err := fsutil.Copy(ctx, h, io.LimitReader(r, e.Size+1))
	if err != nil {
		return err
	}
	if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.Digest {
		return fmt.Errorf("%w: remote object %s", ErrVerification, e.Digest)
	}
	return nil
}

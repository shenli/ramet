// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// Publication binds a logical tool boundary to its immutable files and result.
// IDs and request digests are caller-supplied SHA-256 values. Keep the ID stable
// across retries and bind the request digest to the tool inputs and environment.
type Publication struct {
	ID            string          `json:"id"`
	RequestDigest string          `json:"request_digest"`
	Result        json.RawMessage `json:"result"`
}

type PublishOptions struct {
	Publication Publication `json:"publication"`
	Mode        string      `json:"mode"`
}

func (p Publication) validate() error {
	if !validDigest(p.ID) || !validDigest(p.RequestDigest) {
		return fmt.Errorf("%w: publication ID and request digest must be SHA-256 hex", ErrInvalid)
	}
	if len(p.Result) == 0 || len(p.Result) > 65536 || !utf8.Valid(p.Result) || !json.Valid(p.Result) {
		return fmt.Errorf("%w: publication result must be valid JSON, at most 64 KiB", ErrInvalid)
	}
	// Marshal may expand HTML-sensitive characters. Enforce the stored bound
	// too, so publication cannot create a manifest its own reader rejects.
	encoded, err := json.Marshal(p.Result)
	if err != nil || len(encoded) > 65536 {
		return fmt.Errorf("%w: encoded publication result exceeds 64 KiB", ErrInvalid)
	}
	return nil
}

func validatePublication(m Manifest) error {
	if m.Publication == nil {
		return nil
	}
	if m.Version < 5 || m.Save == nil || (m.Save.Mode != "checkpoint" && m.Save.Mode != "promotion") || m.Parent == "" {
		return fmt.Errorf("%w: publication requires a version 5 checkpoint or promotion", ErrInvalid)
	}
	return m.Publication.validate()
}

// publicationIndex must be called with publicationMu held. Namespace ownership
// excludes other workers. Rebuild once from authoritative manifests, then retain
// only ID -> snapshot ID pairs; reads still verify the authoritative snapshot.
func (s *Store) publicationIndex(ctx context.Context) error {
	if s.publications != nil {
		return nil
	}
	snaps, err := s.listSnapshots(ctx)
	if err != nil {
		return err
	}
	index := map[string]string{}
	for _, snap := range snaps {
		if p := snap.Manifest.Publication; p != nil {
			if previous, ok := index[p.ID]; ok && previous != snap.ID {
				return fmt.Errorf("%w: duplicate durable publication ID", ErrVerification)
			}
			index[p.ID] = snap.ID
		}
	}
	s.publications = index
	return nil
}

// GetPublication resolves publication even after worker replacement. NotFound
// means no published result at this read; it does NOT prove a live publisher
// has stopped. Never execute a replacement attempt until its predecessor is fenced.
func (s *Store) GetPublication(ctx context.Context, id string) (Snapshot, error) {
	if !validDigest(id) {
		return Snapshot{}, fmt.Errorf("%w: publication ID must be SHA-256 hex", ErrInvalid)
	}
	if err := s.enter(); err != nil {
		return Snapshot{}, err
	}
	defer s.leave()
	s.publicationMu.Lock()
	defer s.publicationMu.Unlock()
	if err := s.publicationIndex(ctx); err != nil {
		return Snapshot{}, err
	}
	snapshot, ok := s.publications[id]
	if !ok {
		// An index hit is revalidated below. A miss must also check provider
		// ownership/reachability rather than hide a lost connection as 404.
		if s.opts.Metadata != nil {
			if err := s.opts.Metadata.Ping(ctx); err != nil {
				return Snapshot{}, err
			}
		}
		return Snapshot{}, ErrNotFound
	}
	return s.readSnapshot(ctx, snapshot)
}

// Publish captures a quiesced execution exactly once per logical publication ID.
// A matching completed publication is returned without recapturing changed files.
// The result is part of the manifest: there is no snapshot/receipt dual-write gap.
// This does not make the tool itself or external effects execute exactly once.
func (p *Pool) Publish(ctx context.Context, e Execution, opts PublishOptions) (Snapshot, error) {
	if err := opts.Publication.validate(); err != nil {
		return Snapshot{}, err
	}
	if opts.Mode != "checkpoint" && opts.Mode != "promotion" {
		return Snapshot{}, fmt.Errorf("%w: publication mode must be checkpoint or promotion", ErrInvalid)
	}
	if err := p.store.enter(); err != nil {
		return Snapshot{}, err
	}
	defer p.store.leave()
	s := p.store
	s.publicationMu.Lock()
	defer s.publicationMu.Unlock()
	if err := s.publicationIndex(ctx); err != nil {
		return Snapshot{}, err
	}
	if id, ok := s.publications[opts.Publication.ID]; ok {
		snap, err := s.readSnapshot(ctx, id)
		if err != nil {
			return Snapshot{}, err
		}
		if snap.Manifest.Parent != p.base.ID || snap.Manifest.Save.Mode != opts.Mode ||
			snap.Manifest.Publication.RequestDigest != opts.Publication.RequestDigest {
			return Snapshot{}, fmt.Errorf("%w: publication ID is bound to different inputs, base or mode", ErrConflict)
		}
		return snap, nil
	}
	item, err := p.claim(e)
	if err != nil {
		return Snapshot{}, err
	}
	defer item.mu.Unlock()
	s.contentMu.RLock()
	defer s.contentMu.RUnlock()
	publication := opts.Publication
	publication.Result = append(json.RawMessage(nil), publication.Result...)
	m := Manifest{Version: FormatVersion, Parent: p.base.ID, Git: p.base.Manifest.Git,
		Policy: p.base.Manifest.Policy, Publication: &publication}
	snap, err := s.capture(ctx, item.execution.Path, m, opts.Mode == "promotion", &p.base.Manifest)
	if err != nil {
		// Publication may have committed before the provider lost its reply.
		// A later lookup must rebuild from durable manifests, never trust a miss.
		s.publications = nil
		return Snapshot{}, err
	}
	s.publications[publication.ID] = snap.ID
	return snap, nil
}

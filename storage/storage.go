// SPDX-License-Identifier: Apache-2.0
// Package storage defines the durable provider contract. Execution filesystem
// operations remain local and do not call these interfaces.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrCorrupt  = errors.New("corrupt durable content")
)

type Record struct {
	ID     string
	Parent string
	Data   []byte
}
type Ref struct {
	Digest string
	Size   int64
}

// Metadata owns a single namespace exclusively until Close. Implementations
// must fail closed after losing ownership. Publish is atomic and idempotent;
// Delete must reject parents of published snapshots.
type Metadata interface {
	BindContent(context.Context, string) error
	Publish(context.Context, Record) error
	Get(context.Context, string) (Record, error)
	List(context.Context) ([]Record, error)
	Delete(context.Context, string) error
	Ping(context.Context) error
	Close() error
}

// Content confirms durability before Put returns. Keys are immutable digests.
// Physical remote deletion is deliberately not part of this Phase 1 contract:
// retiring cache entries must never delete another worker's durable data.
type Content interface {
	Put(context.Context, Ref, io.ReadSeeker) (bool, error)
	Open(context.Context, string) (io.ReadCloser, error)
	Ping(context.Context) error
	Identity() string
}

// ContentChecker optionally confirms an existing object's size and integrity
// without a local copy. Implementations must not trust user-supplied metadata
// as proof of content integrity. ErrNotFound means no durable object exists.
type ContentChecker interface {
	Check(context.Context, Ref) error
}

func ValidDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func ValidateRecord(r Record) error {
	h := sha256.Sum256(r.Data)
	if !ValidDigest(r.ID) || hex.EncodeToString(h[:]) != r.ID || r.Parent != "" && !ValidDigest(r.Parent) {
		return fmt.Errorf("%w: invalid manifest record", ErrCorrupt)
	}
	return nil
}

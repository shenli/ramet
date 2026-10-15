// SPDX-License-Identifier: Apache-2.0
package engine

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

	"github.com/restic/chunker"
	"github.com/shenli/ramet/internal/fsutil"
)

const (
	largeFileThreshold = 8 << 20
	minChunkSize       = 512 << 10
	maxChunkSize       = 4 << 20
	// Fixed format parameters allow reuse across workers and restarts. Chunk
	// boundaries are not secrets; SHA-256, not Rabin, identifies stored bytes.
	chunkPolynomial = chunker.Pol(0x3DA3358B4DC173)
)

func validateChunks(version int, e Entry) error {
	if len(e.Chunks) == 0 {
		return nil
	}
	if version < 4 || e.Kind != "file" || e.Size < largeFileThreshold {
		return fmt.Errorf("%w: invalid chunked entry %s", ErrInvalid, e.Path)
	}
	var total int64
	for i, c := range e.Chunks {
		if !validDigest(c.Digest) || c.Size <= 0 || c.Size > maxChunkSize ||
			(i < len(e.Chunks)-1 && c.Size < minChunkSize) || c.Size > e.Size-total {
			return fmt.Errorf("%w: invalid chunk in %s", ErrInvalid, e.Path)
		}
		total += c.Size
	}
	if total != e.Size {
		return fmt.Errorf("%w: chunk sizes do not match %s", ErrInvalid, e.Path)
	}
	return nil
}

func (s *Store) captureFile(ctx context.Context, name string, size int64) (string, int64, []ContentRef, error) {
	if size < largeFileThreshold || s.opts.DisableChunking {
		digest, n, err := s.putFile(ctx, name)
		return digest, n, nil, err
	}
	f, err := os.Open(name)
	if err != nil {
		return "", 0, nil, err
	}
	defer f.Close()
	h := sha256.New()
	c := chunker.New(io.TeeReader(f, h), chunkPolynomial,
		chunker.WithBoundaries(minChunkSize, maxChunkSize), chunker.WithAverageBits(20))
	buf := make([]byte, 0, maxChunkSize)
	var refs []ContentRef
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, nil, err
		}
		chunk, err := c.Next(buf)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", 0, nil, err
		}
		digest, n, err := s.putObject(ctx, bytes.NewReader(chunk.Data), int64(chunk.Length))
		if err != nil {
			return "", 0, nil, err
		}
		refs = append(refs, ContentRef{Digest: digest, Size: n})
		total += n
		buf = chunk.Data[:0]
	}
	if total != size {
		return "", 0, nil, fmt.Errorf("%w: source changed during chunking; stop writers first", ErrConflict)
	}
	return hex.EncodeToString(h.Sum(nil)), total, refs, nil
}

// writeFile verifies each physical object and the complete logical file while
// streaming. Callers must not expose the destination until it returns success.
func (s *Store) writeFile(ctx context.Context, e Entry, dest io.Writer) error {
	refs := e.Chunks
	if len(refs) == 0 {
		refs = []ContentRef{{Digest: e.Digest, Size: e.Size}}
	}
	whole := sha256.New()
	var total int64
	for _, ref := range refs {
		err := s.withObject(ctx, refEntry(ref), func(name string) error {
			f, err := os.Open(name)
			if err != nil {
				return err
			}
			defer f.Close()
			h := sha256.New()
			n, err := fsutil.Copy(ctx, io.MultiWriter(dest, whole, h), io.LimitReader(f, ref.Size+1))
			if err != nil {
				return err
			}
			if n != ref.Size || hex.EncodeToString(h.Sum(nil)) != ref.Digest {
				return fmt.Errorf("%w: file object %s", ErrVerification, ref.Digest)
			}
			total += n
			return nil
		})
		if err != nil {
			return err
		}
	}
	if total != e.Size || hex.EncodeToString(whole.Sum(nil)) != e.Digest {
		return fmt.Errorf("%w: assembled file %s", ErrVerification, e.Path)
	}
	return nil
}

// withFile presents a verified complete file for copy/reflink materialization.
// Chunked files use temporary assembly, never a second durable full-file copy.
func (s *Store) withFile(ctx context.Context, e Entry, fn func(string) error) error {
	if len(e.Chunks) == 0 {
		return s.withObject(ctx, e, fn)
	}
	release, err := s.reserveDisk(e.Size)
	if err != nil {
		return err
	}
	defer release()
	f, err := os.CreateTemp(filepath.Join(s.root, "staging"), "assemble-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = s.writeFile(ctx, e, f)
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return fn(f.Name())
}

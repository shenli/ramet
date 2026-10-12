// SPDX-License-Identifier: Apache-2.0
package fsutil

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// SyncDir makes rename publication durable on the local filesystem.
func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// AtomicWrite publishes only after the file contents have reached storage.
func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func Copy(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, contextReader{ctx, src})
}

// CopyFile tries an independent copy-on-write clone, never a writable hardlink.
// The returned boolean distinguishes an actual clone from a full-copy fallback.
func CopyFile(ctx context.Context, src, dst string, requireClone bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := cloneFile(src, dst); err == nil {
		return true, nil
	} else if requireClone {
		return false, err
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	in, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return false, err
	}
	_, err = Copy(ctx, out, in)
	err = errors.Join(err, out.Close())
	return false, err
}

// RemoveTree handles directories whose original snapshot permissions were read-only.
func RemoveTree(path string) error {
	err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(p, 0700)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}

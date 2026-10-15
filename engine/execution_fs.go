// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shenli/ramet/internal/fsutil"
	"github.com/shenli/ramet/internal/lazyfs"
)

func (s *Store) removeExecution(name string) error {
	s.lazyMu.Lock()
	defer s.lazyMu.Unlock()
	if mount := s.lazyMounts[name]; mount != nil {
		if err := mount.Close(); err != nil {
			return err
		}
		delete(s.lazyMounts, name)
		return fsutil.RemoveTree(filepath.Dir(name))
	}
	return fsutil.RemoveTree(name)
}

// Prepared lazy trees have not been exposed to a workload. File contents are
// checked on first open; explicit Verify still performs a complete deep scan.
func (s *Store) verifyPrepared(ctx context.Context, name string, m Manifest) (Verification, error) {
	s.lazyMu.Lock()
	lazy := s.lazyMounts[name] != nil
	s.lazyMu.Unlock()
	if lazy {
		return s.verifyTreeMode(ctx, name, m, false)
	}
	return s.verifyTree(ctx, name, m)
}

func (s *Store) materializeLazy(ctx context.Context, snap Snapshot) (string, error) {
	parent, err := os.MkdirTemp(filepath.Join(s.root, "executions"), "lazy-")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = fsutil.RemoveTree(parent)
		}
	}()
	backing, target := filepath.Join(parent, "backing"), filepath.Join(parent, "root")
	if err = os.Mkdir(backing, 0700); err != nil {
		return "", err
	}
	if err = os.Mkdir(target, 0700); err != nil {
		return "", err
	}
	var files []lazyfs.File
	entries := map[string]Entry{}
	for _, e := range snap.Manifest.Entries {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		dest := filepath.Join(backing, filepath.FromSlash(e.Path))
		switch e.Kind {
		case "dir":
			err = os.Mkdir(dest, 0700)
		case "symlink":
			err = os.Symlink(e.Target, dest)
		case "file":
			if strings.HasPrefix(e.Path, ".git/") {
				err = s.withFile(ctx, e, func(source string) error {
					digest, size, err := digestFile(ctx, source)
					if err != nil {
						return err
					}
					if digest != e.Digest || size != e.Size {
						return ErrVerification
					}
					release, err := s.reserveDisk(e.Size)
					if err != nil {
						return err
					}
					defer release()
					_, err = fsutil.CopyFile(ctx, source, dest, false)
					return err
				})
				if err == nil {
					err = os.Chmod(dest, os.FileMode(e.Mode))
				}
				if err == nil {
					t := time.Unix(0, e.ModTime)
					err = os.Chtimes(dest, t, t)
				}
			} else {
				var f *os.File
				f, err = os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err == nil {
					err = errors.Join(f.Truncate(e.Size), f.Close())
				}
				entries[e.Path] = e
				files = append(files, lazyfs.File{Path: e.Path, Size: e.Size, Mode: e.Mode, ModTime: e.ModTime})
			}
		}
		if err != nil {
			return "", err
		}
	}
	if err = s.restoreGitObjects(ctx, backing, snap.Manifest.GitObjects); err != nil {
		return "", err
	}
	for i := len(snap.Manifest.Entries) - 1; i >= 0; i-- {
		e := snap.Manifest.Entries[i]
		if e.Kind != "dir" {
			continue
		}
		dest := filepath.Join(backing, e.Path)
		t := time.Unix(0, e.ModTime)
		if err = os.Chtimes(dest, t, t); err != nil {
			return "", err
		}
		if err = os.Chmod(dest, os.FileMode(e.Mode)); err != nil {
			return "", err
		}
	}
	mount, err := lazyfs.Start(backing, target, files, func(ctx context.Context, file lazyfs.File, dest *os.File) (hydrateErr error) {
		began := time.Now()
		defer func() {
			s.metrics.hydrationNanos.Add(uint64(time.Since(began)))
			if hydrateErr != nil {
				s.metrics.hydrationErrors.Add(1)
				s.opts.Logger.Error("lazy hydration failed", "digest", entries[file.Path].Digest, "error", hydrateErr)
			} else {
				s.metrics.hydrationCount.Add(1)
			}
		}()
		e := entries[file.Path]
		release, err := s.reserveDisk(e.Size)
		if err != nil {
			return err
		}
		defer release()
		if err = dest.Truncate(e.Size); err != nil {
			return err
		}
		if _, err = dest.Seek(0, 0); err != nil {
			return err
		}
		err = s.writeFile(ctx, e, dest)
		if err == nil {
			s.metrics.copied.Add(1)
		}
		return err
	})
	if err != nil {
		return "", err
	}
	s.lazyMu.Lock()
	s.lazyMounts[target] = mount
	s.lazyMu.Unlock()
	ok = true
	verification, err := s.verifyPrepared(ctx, target, snap.Manifest)
	if err != nil || !verification.Valid {
		if err == nil {
			err = ErrVerification
		}
		return "", errors.Join(err, s.removeExecution(target))
	}
	return target, nil
}

// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shenli/ramet/internal/fsutil"
	"github.com/shenli/ramet/storage"
)

func hashBytes(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func digestFile(ctx context.Context, name string) (string, int64, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := fsutil.Copy(ctx, h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func (s *Store) putFile(ctx context.Context, name string) (string, int64, error) {
	in, err := os.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", 0, err
	}
	release, err := s.reserveDisk(info.Size())
	if err != nil {
		return "", 0, err
	}
	defer release()
	tmp, err := os.CreateTemp(filepath.Join(s.root, "staging"), "content-")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := fsutil.Copy(ctx, io.MultiWriter(tmp, h), in)
	if err == nil {
		err = tmp.Chmod(0400)
	}
	if err == nil {
		err = tmp.Sync()
	}
	err = errors.Join(err, tmp.Close())
	if err != nil {
		return "", 0, err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	dest := filepath.Join(s.root, "objects", digest)
	if s.opts.Content != nil {
		f, openErr := os.Open(tmp.Name())
		if openErr != nil {
			return "", 0, openErr
		}
		added, putErr := s.opts.Content.Put(ctx, storage.Ref{Digest: digest, Size: n}, f)
		err = errors.Join(putErr, f.Close())
		if err != nil {
			return "", 0, err
		}
		if added {
			s.metrics.uploaded.Add(uint64(n))
		}
	}
	err = s.installCache(ctx, tmp.Name(), Entry{Kind: "file", Digest: digest, Size: n})
	if err == nil {
		err = fsutil.SyncDir(filepath.Dir(dest))
	}
	return digest, n, err
}

func (s *Store) verifyObject(ctx context.Context, e Entry) error {
	digest, n, err := digestFile(ctx, filepath.Join(s.root, "objects", e.Digest))
	if err != nil {
		return fmt.Errorf("object %s: %w", e.Digest, err)
	}
	if digest != e.Digest || n != e.Size {
		return fmt.Errorf("%w: corrupt object %s", ErrVerification, e.Digest)
	}
	return nil
}

func (s *Store) capture(ctx context.Context, source string, manifest Manifest, selective bool, base *Manifest) (Snapshot, error) {
	info, err := os.Stat(source)
	if err != nil {
		return Snapshot{}, err
	}
	if !info.IsDir() {
		return Snapshot{}, fmt.Errorf("%w: source must be a directory", ErrInvalid)
	}
	mode := "base"
	if base != nil {
		mode = "checkpoint"
	}
	if selective {
		mode = "promotion"
	}
	manifest.Save = &SaveReport{Mode: mode, Excluded: []OmittedPath{}, Rebuild: []RebuildStep{}}
	manifest.Entries = []Entry{}
	if manifest.Git != nil || (base != nil && base.GitObjects != nil) {
		var baseline *GitObjects
		if base != nil {
			baseline = base.GitObjects
		}
		manifest.GitObjects, err = s.captureGitObjects(ctx, source, baseline)
		if err != nil {
			return Snapshot{}, err
		}
	}
	baseEntries := map[string]Entry{}
	if base != nil {
		for _, e := range base.Entries {
			baseEntries[e.Path] = e
		}
	}
	seen := map[string]bool{}
	err = filepath.WalkDir(source, func(name string, d os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if name == source {
			return nil
		}
		rel, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !validPath(rel) {
			return fmt.Errorf("%w: invalid path %q", ErrInvalid, rel)
		}
		if manifest.GitObjects != nil && rel == ".git/objects" {
			return filepath.SkipDir
		}
		class := manifest.Policy.Classify(rel)
		// Walk excluded directories too: a more specific durable rule can retain
		// a descendant. Empty excluded directories are pruned below.
		if selective && (class == Rebuildable || class == Ephemeral) && !d.IsDir() {
			info, statErr := d.Info()
			if statErr != nil {
				return statErr
			}
			manifest.Save.Excluded = append(manifest.Save.Excluded, omitted(rel, class, info))
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		e := Entry{Path: rel, Mode: uint32(info.Mode().Perm()), ModTime: info.ModTime().UnixNano()}
		switch {
		case info.IsDir():
			e.Kind = "dir"
		case info.Mode().IsRegular():
			e.Kind = "file"
			e.Digest, e.Size, err = s.putFile(ctx, name)
			if err != nil {
				return err
			}
			after, err := os.Lstat(name)
			if err != nil {
				return err
			}
			if !os.SameFile(info, after) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) || after.Mode() != info.Mode() {
				return fmt.Errorf("%w: source changed during snapshot: %s; stop writers first", ErrConflict, rel)
			}
		case info.Mode()&os.ModeSymlink != 0:
			e.Kind, e.ModTime, e.Mode = "symlink", 0, 0
			e.Target, err = os.Readlink(name)
			if err != nil {
				return err
			}
			if !safeTarget(rel, e.Target) {
				return fmt.Errorf("%w: unsupported symlink %s -> %s", ErrInvalid, rel, e.Target)
			}
		default:
			return fmt.Errorf("%w: unsupported special file %s; classify it as ephemeral before promotion", ErrInvalid, rel)
		}
		if selective && class == SharedImmutable {
			original, ok := baseEntries[rel]
			if !ok || !sameEntry(original, e) {
				return fmt.Errorf("%w: shared immutable path changed: %s", ErrConflict, rel)
			}
		}
		seen[rel] = true
		manifest.Entries = append(manifest.Entries, e)
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	if selective {
		for name := range baseEntries {
			if manifest.Policy.Classify(name) == SharedImmutable && !seen[name] {
				return Snapshot{}, fmt.Errorf("%w: shared immutable path deleted: %s", ErrConflict, name)
			}
		}
		needed := map[string]bool{}
		for _, e := range manifest.Entries {
			class := manifest.Policy.Classify(e.Path)
			if class == Ephemeral || class == Rebuildable {
				continue
			}
			for p := e.Path; p != "."; p = path.Dir(p) {
				needed[p] = true
			}
		}
		kept := manifest.Entries[:0]
		for _, e := range manifest.Entries {
			if needed[e.Path] {
				kept = append(kept, e)
			} else {
				manifest.Save.Excluded = append(manifest.Save.Excluded, OmittedPath{Path: e.Path, Class: manifest.Policy.Classify(e.Path), Kind: e.Kind})
			}
		}
		manifest.Entries = kept
	}
	sort.Slice(manifest.Save.Excluded, func(i, j int) bool { return manifest.Save.Excluded[i].Path < manifest.Save.Excluded[j].Path })
	// A full checkpoint of a selectively saved base must not erase pending
	// reconstruction requirements merely because omitted files remain absent.
	if selective || (base != nil && base.Save != nil && len(base.Save.Rebuild) > 0) {
		manifest.Save.Rebuild, err = rebuildSteps(manifest.Policy, manifest.Entries)
		if err != nil {
			return Snapshot{}, err
		}
	}
	sort.Slice(manifest.Entries, func(i, j int) bool { return manifest.Entries[i].Path < manifest.Entries[j].Path })
	if err = validateManifest(manifest); err != nil {
		return Snapshot{}, err
	}
	b, err := json.Marshal(manifest)
	if err != nil {
		return Snapshot{}, err
	}
	id := hashBytes(b)
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	// All referenced content has been synced before this single publication.
	if err = s.publishSnapshot(ctx, id, manifest, b); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{ID: id, Manifest: manifest}, nil
}

func safeTarget(name, target string) bool {
	return utf8.ValidString(target) && target != "" && !path.IsAbs(target) && !strings.ContainsAny(target, "\\\x00")
}

func validateManifest(m Manifest) error {
	if m.Version != 1 && m.Version != 2 && m.Version != FormatVersion {
		return fmt.Errorf("unsupported manifest version %d", m.Version)
	}
	if err := m.Policy.Validate(); err != nil {
		return err
	}
	if m.Git != nil && (!utf8.ValidString(m.Git.Repository) || !utf8.ValidString(m.Git.Revision)) {
		return fmt.Errorf("%w: Git provenance must be valid UTF-8", ErrInvalid)
	}
	if m.Parent != "" && !validDigest(m.Parent) {
		return fmt.Errorf("%w: invalid parent", ErrInvalid)
	}
	seen := map[string]string{}
	last := ""
	for _, e := range m.Entries {
		if !validPath(e.Path) || e.Path <= last || e.Mode > 0777 || e.Size < 0 {
			return fmt.Errorf("%w: invalid manifest entry %q", ErrInvalid, e.Path)
		}
		last = e.Path
		if dir := path.Dir(e.Path); dir != "." && seen[dir] != "dir" {
			return fmt.Errorf("%w: missing/non-directory parent of %s", ErrInvalid, e.Path)
		}
		switch e.Kind {
		case "file":
			if !validDigest(e.Digest) || e.Target != "" {
				return fmt.Errorf("%w: invalid file %s", ErrInvalid, e.Path)
			}
		case "dir":
			if e.Digest != "" || e.Target != "" || e.Size != 0 {
				return fmt.Errorf("%w: invalid directory %s", ErrInvalid, e.Path)
			}
		case "symlink":
			if !safeTarget(e.Path, e.Target) || e.Digest != "" || e.Size != 0 || e.Mode != 0 || e.ModTime != 0 {
				return fmt.Errorf("%w: invalid symlink %s", ErrInvalid, e.Path)
			}
		default:
			return fmt.Errorf("%w: invalid entry kind %q", ErrInvalid, e.Kind)
		}
		seen[e.Path] = e.Kind
	}
	if err := validateGitObjects(m); err != nil {
		return err
	}
	if err := validateSaveReport(m); err != nil {
		return err
	}
	return validateSymlinks(m.Entries)
}

// Resolve links against the manifest namespace, never the host filesystem.
// Parent links used by package managers are valid when their full chain stays
// within the workspace. Cleaning a target lexically is insufficient: a/.. can
// escape if a itself points to a shallower directory.
func validateSymlinks(entries []Entry) error {
	links := map[string]string{}
	for _, e := range entries {
		if e.Kind == "symlink" {
			links[e.Path] = e.Target
		}
	}
	for name, target := range links {
		var stack []string
		if dir := path.Dir(name); dir != "." {
			stack = strings.Split(dir, "/")
		}
		pending := strings.Split(target, "/")
		hops := 0
		for len(pending) > 0 {
			part := pending[0]
			pending = pending[1:]
			switch part {
			case "", ".":
				continue
			case "..":
				if len(stack) == 0 {
					return fmt.Errorf("%w: symlink %s escapes workspace", ErrInvalid, name)
				}
				stack = stack[:len(stack)-1]
			default:
				stack = append(stack, part)
				if next, ok := links[strings.Join(stack, "/")]; ok {
					hops++
					if hops > 40 {
						return fmt.Errorf("%w: symlink %s has a cycle or excessive depth", ErrInvalid, name)
					}
					stack = stack[:len(stack)-1]
					pending = append(strings.Split(next, "/"), pending...)
				}
			}
		}
	}
	return nil
}

func (s *Store) readSnapshot(ctx context.Context, id string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if !validDigest(id) {
		return Snapshot{}, fmt.Errorf("%w: snapshot ID must be a SHA-256 digest", ErrInvalid)
	}
	if s.opts.Metadata != nil {
		r, err := s.opts.Metadata.Get(ctx, id)
		if err != nil {
			return Snapshot{}, err
		}
		snap, err := decodeSnapshot(id, r.Data)
		if err == nil && snap.Manifest.Parent != r.Parent {
			return Snapshot{}, fmt.Errorf("%w: metadata lineage mismatch", ErrVerification)
		}
		return snap, err
	}
	b, err := os.ReadFile(filepath.Join(s.root, "snapshots", id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, err
	}
	return decodeSnapshot(id, b)
}

func decodeSnapshot(id string, b []byte) (Snapshot, error) {
	if hashBytes(b) != id {
		return Snapshot{}, fmt.Errorf("%w: manifest digest mismatch", ErrVerification)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Snapshot{}, err
	}
	if err := validateManifest(m); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{ID: id, Manifest: m}, nil
}

func sortEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
}

func (s *Store) materialize(ctx context.Context, snap Snapshot) (string, error) {
	if s.opts.Backend == "lazy" {
		return s.materializeLazy(ctx, snap)
	}
	return s.materializeFrom(ctx, snap, func(e Entry) string { return filepath.Join(s.root, "objects", e.Digest) })
}

func (s *Store) materializeFrom(ctx context.Context, snap Snapshot, source func(Entry) string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(filepath.Join(s.root, "executions"), "tree-")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = fsutil.RemoveTree(dir)
		}
	}()
	for _, e := range snap.Manifest.Entries {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		dest := filepath.Join(dir, filepath.FromSlash(e.Path))
		switch e.Kind {
		case "dir":
			err = os.Mkdir(dest, 0700)
		case "symlink":
			err = os.Symlink(e.Target, dest)
		case "file":
			var cloned bool
			copySource := func(name string) error {
				release, reserveErr := s.reserveDisk(e.Size)
				if reserveErr != nil {
					return reserveErr
				}
				defer release()
				var copyErr error
				cloned, copyErr = fsutil.CopyFile(ctx, name, dest, s.opts.RequireCOW)
				return copyErr
			}
			if source(e) == filepath.Join(s.root, "objects", e.Digest) {
				err = s.withObject(ctx, e, copySource)
			} else {
				err = copySource(source(e))
			}
			if err == nil {
				if cloned {
					s.metrics.cloned.Add(1)
				} else {
					s.metrics.copied.Add(1)
				}
				err = os.Chmod(dest, os.FileMode(e.Mode))
				if err == nil {
					t := time.Unix(0, e.ModTime)
					err = os.Chtimes(dest, t, t)
				}
			}
		}
		if err != nil {
			return "", fmt.Errorf("materialize %s: %w", e.Path, err)
		}
	}
	if err = s.restoreGitObjects(ctx, dir, snap.Manifest.GitObjects); err != nil {
		return "", err
	}
	// Restore directory mtimes after children have been created, deepest first.
	for i := len(snap.Manifest.Entries) - 1; i >= 0; i-- {
		e := snap.Manifest.Entries[i]
		if e.Kind != "dir" {
			continue
		}
		dest := filepath.Join(dir, filepath.FromSlash(e.Path))
		t := time.Unix(0, e.ModTime)
		if err = os.Chtimes(dest, t, t); err != nil {
			return "", err
		}
		if err = os.Chmod(dest, os.FileMode(e.Mode)); err != nil {
			return "", err
		}
	}
	verification, err := s.verifyTree(ctx, dir, snap.Manifest)
	if err != nil {
		return "", err
	}
	if !verification.Valid {
		return "", fmt.Errorf("%w: %s", ErrVerification, strings.Join(verification.Differences, "; "))
	}
	ok = true
	return dir, nil
}

func sameEntry(a, b Entry) bool { return a == b }

func (s *Store) verifyTree(ctx context.Context, dir string, m Manifest) (Verification, error) {
	return s.verifyTreeMode(ctx, dir, m, true)
}
func (s *Store) verifyTreeMode(ctx context.Context, dir string, m Manifest, contents bool) (Verification, error) {
	s.metrics.verifications.Add(1)
	r := Verification{Valid: true, Differences: []string{}}
	expected := make(map[string]Entry, len(m.Entries))
	for _, e := range m.Entries {
		expected[e.Path] = e
	}
	err := filepath.WalkDir(dir, func(name string, d os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if name == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if m.GitObjects != nil && rel == ".git/objects" {
			return filepath.SkipDir
		}
		e, ok := expected[rel]
		if !ok {
			r.Differences = append(r.Differences, "unexpected: "+rel)
			return nil
		}
		delete(expected, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		actual := Entry{Path: rel, Mode: uint32(info.Mode().Perm()), ModTime: info.ModTime().UnixNano()}
		switch {
		case info.IsDir():
			actual.Kind = "dir"
		case info.Mode().IsRegular():
			actual.Kind = "file"
			if contents {
				actual.Digest, actual.Size, err = digestFile(ctx, name)
			} else {
				actual.Digest = e.Digest
				actual.Size = info.Size()
			}
		case info.Mode()&os.ModeSymlink != 0:
			actual.Kind = "symlink"
			actual.ModTime = 0
			actual.Mode = 0
			actual.Target, err = os.Readlink(name)
		default:
			actual.Kind = "special"
		}
		if err != nil {
			return err
		}
		if !sameEntry(e, actual) {
			r.Differences = append(r.Differences, "changed: "+rel)
		}
		return nil
	})
	if err == nil && m.GitObjects != nil {
		if gitErr := verifyGitInventory(ctx, dir, m.GitObjects); gitErr != nil {
			r.Differences = append(r.Differences, gitErr.Error())
		}
	}
	for name := range expected {
		r.Differences = append(r.Differences, "missing: "+name)
	}
	sort.Strings(r.Differences)
	r.Valid = err == nil && len(r.Differences) == 0
	if !r.Valid {
		s.metrics.failures.Add(1)
	}
	return r, err
}

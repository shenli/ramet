// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shenli/ramet/internal/fsutil"
)

// ContentRef identifies a complete immutable object in Ramet content storage.
type ContentRef struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// GitObjects normalizes Git's mutable loose/pack layout into one shared base
// pack and an optional local-only pack. Both are durable, so reconstruction does
// not depend on continued remote availability or mutable remote-tracking refs.
type GitObjects struct {
	ObjectFormat string      `json:"object_format"`
	BasePack     ContentRef  `json:"base_pack"`
	BaseIndex    ContentRef  `json:"base_index"`
	LocalPack    *ContentRef `json:"local_pack,omitempty"`
	Inventory    string      `json:"inventory"`
	Count        int         `json:"count"`
}

func gitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	flags := []string{"-c", "core.hooksPath=/dev/null", "-c", "protocol.allow=never", "-c", "gc.auto=0"}
	if dir != "" {
		flags = append(flags, "-C", dir)
	}
	cmd := exec.CommandContext(ctx, "git", append(flags, args...)...)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(key, "GIT_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1")
	return cmd
}

func gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	b, err := gitCommand(ctx, dir, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("Git object operation %s failed: %w", args[0], err)
	}
	return b, nil
}

func objectInventory(ctx context.Context, dir string) ([]string, error) {
	b, err := gitOutput(ctx, dir, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(b))
	sort.Strings(ids)
	return ids, nil
}
func inventoryBytes(ids []string) []byte { return []byte(strings.Join(ids, "\n") + "\n") }
func refEntry(r ContentRef) Entry        { return Entry{Kind: "file", Digest: r.Digest, Size: r.Size} }
func (s *Store) contentRefs(m Manifest) []Entry {
	out := make([]Entry, 0, len(m.Entries)+3)
	for _, e := range m.Entries {
		if e.Kind == "file" {
			if len(e.Chunks) == 0 {
				out = append(out, e)
			} else {
				for _, c := range e.Chunks {
					out = append(out, refEntry(c))
				}
			}
		}
	}
	if g := m.GitObjects; g != nil {
		out = append(out, refEntry(g.BasePack), refEntry(g.BaseIndex))
		if g.LocalPack != nil {
			out = append(out, refEntry(*g.LocalPack))
		}
	}
	return out
}
func (s *Store) readContent(ctx context.Context, r ContentRef) ([]byte, error) {
	var b []byte
	err := s.withObject(ctx, refEntry(r), func(name string) error {
		if err := s.verifyObject(ctx, refEntry(r)); err != nil {
			return err
		}
		var err error
		b, err = os.ReadFile(name)
		return err
	})
	return b, err
}
func (s *Store) saveContent(ctx context.Context, b []byte) (ContentRef, error) {
	tmp, err := os.CreateTemp(filepath.Join(s.root, "staging"), "git-index-")
	if err != nil {
		return ContentRef{}, err
	}
	defer os.Remove(tmp.Name())
	_, err = (diskWriter{s, tmp}).Write(b)
	err = errors.Join(err, tmp.Close())
	if err != nil {
		return ContentRef{}, err
	}
	digest, size, err := s.putFile(ctx, tmp.Name())
	return ContentRef{digest, size}, err
}
func (s *Store) packGitObjects(ctx context.Context, dir string, ids []string) (ContentRef, error) {
	tmp, err := os.CreateTemp(filepath.Join(s.root, "staging"), "git-pack-")
	if err != nil {
		return ContentRef{}, err
	}
	defer os.Remove(tmp.Name())
	cmd := gitCommand(ctx, dir, "pack-objects", "--stdout", "--window=0", "--compression=6", "--no-reuse-object", "--no-reuse-delta")
	cmd.Stdin = bytes.NewReader(inventoryBytes(ids))
	cmd.Stdout = diskWriter{s, tmp}
	err = cmd.Run()
	err = errors.Join(err, tmp.Close())
	if err != nil {
		return ContentRef{}, fmt.Errorf("pack Git objects: %w", err)
	}
	digest, size, err := s.putFile(ctx, tmp.Name())
	return ContentRef{digest, size}, err
}

func (s *Store) captureGitObjects(ctx context.Context, dir string, base *GitObjects) (*GitObjects, error) {
	info, err := os.Lstat(filepath.Join(dir, ".git"))
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: Git optimization requires a standalone .git directory", ErrInvalid)
	}
	for _, name := range []string{"shallow", "objects/info/alternates", "objects/info/http-alternates"} {
		if _, err := os.Lstat(filepath.Join(dir, ".git", name)); err == nil {
			return nil, fmt.Errorf("%w: Git object optimization rejects %s", ErrInvalid, name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if _, err := gitOutput(ctx, dir, "fsck", "--full", "--no-reflogs"); err != nil {
		return nil, fmt.Errorf("%w: corrupt source Git objects: %v", ErrVerification, err)
	}
	format, err := gitOutput(ctx, dir, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, err
	}
	ids, err := objectInventory(ctx, dir)
	if err != nil {
		return nil, err
	}
	g := &GitObjects{ObjectFormat: strings.TrimSpace(string(format)), Count: len(ids), Inventory: hashBytes(inventoryBytes(ids))}
	if g.ObjectFormat != "sha1" && g.ObjectFormat != "sha256" {
		return nil, fmt.Errorf("%w: unsupported Git object format", ErrInvalid)
	}
	if base == nil {
		g.BasePack, err = s.packGitObjects(ctx, dir, ids)
		if err != nil {
			return nil, err
		}
		g.BaseIndex, err = s.saveContent(ctx, inventoryBytes(ids))
		return g, err
	}
	if base.ObjectFormat != g.ObjectFormat {
		return nil, fmt.Errorf("%w: Git object format changed", ErrConflict)
	}
	g.BasePack, g.BaseIndex = base.BasePack, base.BaseIndex
	index, err := s.readContent(ctx, base.BaseIndex)
	if err != nil {
		return nil, err
	}
	baseline := map[string]bool{}
	for _, id := range strings.Fields(string(index)) {
		baseline[id] = true
	}
	present := map[string]bool{}
	var local []string
	for _, id := range ids {
		present[id] = true
		if !baseline[id] {
			local = append(local, id)
		}
	}
	// Base objects may have been pruned after an agent moved refs. Reusing the
	// original base pack would silently resurrect deleted objects; fail closed.
	for id := range baseline {
		if !present[id] {
			return nil, fmt.Errorf("%w: Git base object was removed; capture a fresh Git base", ErrConflict)
		}
	}
	if len(local) > 0 {
		ref, err := s.packGitObjects(ctx, dir, local)
		if err != nil {
			return nil, err
		}
		g.LocalPack = &ref
	}
	return g, nil
}

func (s *Store) restoreGitObjects(ctx context.Context, dir string, g *GitObjects) error {
	if g == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git", "objects", "pack"), 0700); err != nil {
		return err
	}
	refs := []ContentRef{g.BasePack}
	if g.LocalPack != nil {
		refs = append(refs, *g.LocalPack)
	}
	for _, ref := range refs {
		err := s.withObject(ctx, refEntry(ref), func(name string) error {
			// index-pack writes the pack and its index into this private tree.
			release, err := s.reserveDisk(ref.Size*2 + int64(g.Count)*64)
			if err != nil {
				return err
			}
			defer release()
			if err := s.verifyObject(ctx, refEntry(ref)); err != nil {
				return err
			}
			f, err := os.Open(name)
			if err != nil {
				return err
			}
			defer f.Close()
			// Git names packs by their trailing object-format checksum, not
			// the SHA-256 digest used by Ramet CAS. Clone the pack into an
			// independent inode before generating its disposable Git index.
			checksumBytes := 20
			if g.ObjectFormat == "sha256" {
				checksumBytes = 32
			}
			checksum := make([]byte, checksumBytes)
			if _, err = f.Seek(-int64(checksumBytes), io.SeekEnd); err != nil {
				return err
			}
			if _, err = io.ReadFull(f, checksum); err != nil {
				return err
			}
			pack := filepath.Join(dir, ".git", "objects", "pack", "pack-"+hex.EncodeToString(checksum)+".pack")
			cloned, err := fsutil.CopyFile(ctx, name, pack, s.opts.RequireCOW)
			if err != nil {
				return err
			}
			if cloned {
				s.metrics.cloned.Add(1)
			} else {
				s.metrics.copied.Add(1)
			}
			if err = os.Chmod(pack, 0444); err != nil {
				return err
			}
			cmd := gitCommand(ctx, dir, "index-pack", "--strict", pack)
			cmd.Stdout = io.Discard
			return cmd.Run()
		})
		if err != nil {
			return fmt.Errorf("reconstruct Git objects: %w", err)
		}
	}
	return verifyGitInventory(ctx, dir, g)
}

// diskWriter checks headroom for streaming Git output before each allocation.
type diskWriter struct {
	store       *Store
	destination io.Writer
}

func (w diskWriter) Write(b []byte) (int, error) {
	release, err := w.store.reserveDisk(int64(len(b)))
	if err != nil {
		return 0, err
	}
	defer release()
	return w.destination.Write(b)
}
func verifyGitInventory(ctx context.Context, dir string, g *GitObjects) error {
	ids, err := objectInventory(ctx, dir)
	if err != nil {
		return err
	}
	if len(ids) != g.Count || hashBytes(inventoryBytes(ids)) != g.Inventory {
		return fmt.Errorf("%w: Git object inventory differs", ErrVerification)
	}
	// fsck validates pack/object integrity and connectivity while allowing
	// intentional dangling objects, which are included in the inventory.
	if _, err = gitOutput(ctx, dir, "fsck", "--full", "--no-reflogs"); err != nil {
		return fmt.Errorf("%w: Git object integrity: %v", ErrVerification, err)
	}
	return nil
}

func validateGitObjects(m Manifest) error {
	g := m.GitObjects
	if g == nil {
		return nil
	}
	if m.Version < 2 || (g.ObjectFormat != "sha1" && g.ObjectFormat != "sha256") || !validDigest(g.Inventory) || g.Count < 0 {
		return fmt.Errorf("%w: invalid Git object recipe", ErrInvalid)
	}
	refs := []ContentRef{g.BasePack, g.BaseIndex}
	if g.LocalPack != nil {
		refs = append(refs, *g.LocalPack)
	}
	for _, r := range refs {
		if !validDigest(r.Digest) || r.Size < 0 {
			return fmt.Errorf("%w: invalid Git pack reference", ErrInvalid)
		}
	}
	found := false
	for _, e := range m.Entries {
		if e.Path == ".git" && e.Kind == "dir" {
			found = true
		}
		if e.Path == ".git/objects" || strings.HasPrefix(e.Path, ".git/objects/") {
			return fmt.Errorf("%w: mixed Git object representations", ErrInvalid)
		}
	}
	if !found {
		return fmt.Errorf("%w: Git metadata directory missing", ErrInvalid)
	}
	return nil
}

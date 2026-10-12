//go:build linux

// SPDX-License-Identifier: Apache-2.0
package lazyfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"
)

type Mount struct {
	mu      sync.Mutex // serializes hydration and namespace mutations
	server  *fuse.Server
	root    *os.Root
	pending map[uint64]File
	hydrate Hydrate
	ctx     context.Context
	cancel  context.CancelFunc
}
type node struct {
	fs.LoopbackNode
	mount *Mount
}

func Supported() error {
	f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("lazy backend: %w", err)
	}
	f.Close()
	if _, err = exec.LookPath("fusermount3"); err != nil {
		_, err = exec.LookPath("fusermount")
	}
	return err
}

func Start(backing, target string, files []File, hydrate Hydrate) (*Mount, error) {
	r, err := os.OpenRoot(backing)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Mount{root: r, pending: map[uint64]File{}, hydrate: hydrate, ctx: ctx, cancel: cancel}
	ok := false
	defer func() {
		if !ok {
			cancel()
			r.Close()
		}
	}()
	for _, entry := range files {
		var st syscall.Stat_t
		if err = syscall.Lstat(filepath.Join(backing, entry.Path), &st); err != nil {
			return nil, err
		}
		m.pending[st.Ino] = entry
	}
	var st syscall.Stat_t
	if err = syscall.Stat(backing, &st); err != nil {
		return nil, err
	}
	data := &fs.LoopbackRoot{Path: backing, Dev: uint64(st.Dev)}
	root := &node{LoopbackNode: fs.LoopbackNode{RootData: data}, mount: m}
	data.RootNode = root
	data.NewNode = func(d *fs.LoopbackRoot, parent *fs.Inode, name string, st *syscall.Stat_t) fs.InodeEmbedder {
		return &node{LoopbackNode: fs.LoopbackNode{RootData: d}, mount: m}
	}
	m.server, err = fs.Mount(target, root, &fs.Options{MountOptions: fuse.MountOptions{FsName: "ramet", Name: "ramet", Options: []string{"default_permissions", "nosuid", "nodev"}, DisableXAttrs: true}, NullPermissions: true})
	if err != nil {
		return nil, err
	}
	ok = true
	return m, nil
}

func (m *Mount) Close() error {
	m.cancel()
	if err := m.server.Unmount(); err != nil {
		return err
	}
	m.server.Wait()
	return m.root.Close()
}

// Recover unmounts only Ramet-owned FUSE mounts at this exact private path.
func Recover(target string) error {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		path := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\134`, `\`).Replace(fields[4])
		if path != target {
			continue
		}
		if !strings.Contains(line, " - fuse.ramet ") {
			return fmt.Errorf("refusing to unmount foreign filesystem at %s", target)
		}
		command, err := exec.LookPath("fusermount3")
		if err != nil {
			command, err = exec.LookPath("fusermount")
		}
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = exec.CommandContext(ctx, command, "-u", target).Run()
		cancel()
		return err
	}
	return nil
}
func (n *node) relative() string { return n.Path(n.RootData.RootNode.EmbeddedInode()) }
func (n *node) attr(a *fuse.Attr) {
	if e, ok := n.mount.pending[a.Ino]; ok {
		a.Mode = syscall.S_IFREG | e.Mode
		a.Mtime, a.Mtimensec = uint64(e.ModTime/1e9), uint32(e.ModTime%1e9)
	}
}
func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.mount.mu.Lock()
	defer n.mount.mu.Unlock()
	inode, errno := n.LoopbackNode.Lookup(ctx, name, out)
	if errno == 0 {
		n.attr(&out.Attr)
	}
	return inode, errno
}
func (n *node) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.mount.mu.Lock()
	defer n.mount.mu.Unlock()
	errno := n.LoopbackNode.Getattr(ctx, f, out)
	if errno == 0 {
		n.attr(&out.Attr)
	}
	return errno
}

// The mount lock keeps a rename/unlink from changing the file selected here.
// Root.OpenFile also prevents following a path outside the private backing tree.
func (n *node) ensure(ctx context.Context, relative string) syscall.Errno {
	info, err := n.mount.root.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		return fs.ToErrno(err)
	}
	ino := info.Sys().(*syscall.Stat_t).Ino
	e, ok := n.mount.pending[ino]
	if !ok {
		return 0
	}
	f, err := n.mount.root.OpenFile(relative, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fs.ToErrno(err)
	}
	defer f.Close()
	// A FUSE request can be interrupted by the reader's runtime signals (for
	// example Go asynchronous preemption), even while the file remains needed.
	// Hydration belongs to the inode, not that individual kernel request. Keep
	// it bounded by mount lifetime and timeout so a retry sees complete bytes.
	ctx, cancel := context.WithTimeout(n.mount.ctx, 2*time.Minute)
	defer cancel()
	if err = n.mount.hydrate(ctx, e, f); err != nil {
		return syscall.EIO
	}
	if err = f.Chmod(os.FileMode(e.Mode)); err != nil {
		return fs.ToErrno(err)
	}
	if err = unix.UtimesNanoAt(int(f.Fd()), "", []unix.Timespec{unix.NsecToTimespec(e.ModTime), unix.NsecToTimespec(e.ModTime)}, unix.AT_EMPTY_PATH); err != nil {
		return fs.ToErrno(err)
	}
	delete(n.mount.pending, ino)
	return 0
}
func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	n.mount.mu.Lock()
	defer n.mount.mu.Unlock()
	if errno := n.ensure(ctx, n.relative()); errno != 0 {
		return nil, 0, errno
	}
	return n.LoopbackNode.Open(ctx, flags)
}
func (n *node) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	n.mount.mu.Lock()
	defer n.mount.mu.Unlock()
	if errno := n.ensure(ctx, n.relative()); errno != 0 {
		return errno
	}
	return n.LoopbackNode.Setattr(ctx, f, in, out)
}
func (n *node) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	n.mount.mu.Lock()
	defer n.mount.mu.Unlock()
	if errno := n.ensure(ctx, filepath.Join(n.relative(), name)); errno != 0 {
		return nil, nil, 0, errno
	}
	return n.LoopbackNode.Create(ctx, name, flags, mode, out)
}
func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	n.mount.mu.Lock()
	defer n.mount.mu.Unlock()
	// Hydrating before unlink also makes still-open hard links independent of
	// future namespace/inode reuse. Plain unopened deletes need no download.
	info, _ := n.mount.root.Lstat(filepath.Join(n.relative(), name))
	errno := n.LoopbackNode.Unlink(ctx, name)
	if errno == 0 && info != nil {
		st := info.Sys().(*syscall.Stat_t)
		if st.Nlink == 1 {
			delete(n.mount.pending, st.Ino)
		}
	}
	return errno
}
func (n *node) Rename(ctx context.Context, name string, parent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	n.mount.mu.Lock()
	defer n.mount.mu.Unlock()
	p, ok := parent.(*node)
	if !ok || p.mount != n.mount {
		return syscall.EXDEV
	}
	// Materialize an overwritten destination before inode reuse (including
	// RENAME_EXCHANGE). Source identity follows its backing inode after rename.
	if errno := n.ensure(ctx, filepath.Join(p.relative(), newName)); errno != 0 {
		return errno
	}
	return n.LoopbackNode.Rename(ctx, name, parent, newName, flags)
}
func (n *node) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.mount.mu.Lock()
	defer n.mount.mu.Unlock()
	inode, errno := n.LoopbackNode.Link(ctx, target, name, out)
	if errno == 0 {
		n.attr(&out.Attr)
	}
	return inode, errno
}

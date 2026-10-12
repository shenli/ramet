// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/shenli/ramet/internal/fsutil"
)

func (s *Store) checkoutGit(ctx context.Context, base GitBase) (string, GitBase, error) {
	if base.Repository == "" || base.Revision == "" || strings.HasPrefix(base.Repository, "-") || strings.HasPrefix(base.Revision, "-") || strings.ContainsAny(base.Revision, "\x00\n\r") {
		return "", base, fmt.Errorf("%w: repository and explicit revision are required", ErrInvalid)
	}
	if u, err := url.Parse(base.Repository); err == nil && u.User != nil {
		_, password := u.User.Password()
		if password || u.Scheme != "ssh" {
			return "", base, fmt.Errorf("%w: use SSH agent authentication, not credentials in the repository URL", ErrInvalid)
		}
	}
	dir, err := os.MkdirTemp(filepath.Join(s.root, "staging"), "git-")
	if err != nil {
		return "", base, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = fsutil.RemoveTree(dir)
		}
	}()
	run := func(args ...string) (string, error) {
		args = append([]string{"-c", "core.hooksPath=/dev/null", "-c", "protocol.allow=never",
			"-c", "protocol.https.allow=always", "-c", "protocol.http.allow=always",
			"-c", "protocol.ssh.allow=always", "-c", "protocol.git.allow=always", "-c", "protocol.file.allow=always"}, args...)
		cmd := exec.CommandContext(ctx, "git", args...)
		// Library callers may themselves be inside Git hooks or worktrees. Do
		// not let ambient repository/index/config variables redirect this clone.
		for _, value := range os.Environ() {
			key, _, _ := strings.Cut(value, "=")
			if !strings.HasPrefix(key, "GIT_") || key == "GIT_SSH" || key == "GIT_SSH_COMMAND" {
				cmd.Env = append(cmd.Env, value)
			}
		}
		cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TEMPLATE_DIR=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git operation failed: %w: %.2048s", err, out)
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err = run("clone", "--no-local", "--no-checkout", "--", base.Repository, dir); err != nil {
		return "", base, err
	}
	commit, err := run("-C", dir, "rev-parse", "--verify", "--end-of-options", base.Revision+"^{commit}")
	if err != nil {
		// Clone creates a local branch only for the remote HEAD. Other branches
		// are available as remote-tracking refs and still resolve to a fixed commit.
		commit, err = run("-C", dir, "rev-parse", "--verify", "--end-of-options", "refs/remotes/origin/"+base.Revision+"^{commit}")
	}
	if err != nil {
		return "", base, err
	}
	if _, err = run("-C", dir, "checkout", "--detach", commit); err != nil {
		return "", base, err
	}
	// Submodules and LFS require explicit hydration recipes; a success response
	// must not silently pretend those bytes were captured.
	if _, err = os.Stat(filepath.Join(dir, ".gitmodules")); err == nil {
		return "", base, fmt.Errorf("%w: hydrate submodules yourself and snapshot a directory", ErrInvalid)
	} else if !os.IsNotExist(err) {
		return "", base, err
	}
	base.Revision = commit
	ok = true
	return dir, base, nil
}

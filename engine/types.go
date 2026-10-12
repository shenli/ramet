// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shenli/ramet/storage"
)

const FormatVersion = 3

var (
	ErrNotFound     = storage.ErrNotFound
	ErrInvalid      = errors.New("invalid request")
	ErrConflict     = storage.ErrConflict
	ErrClosed       = errors.New("closed")
	ErrDraining     = errors.New("worker is draining")
	ErrVerification = errors.New("verification failed")
	ErrLimit        = errors.New("worker quota exceeded")
	ErrLeaseExpired = errors.New("execution lease expired")
)

type PersistenceClass string

const (
	SharedImmutable PersistenceClass = "shared_immutable"
	Durable         PersistenceClass = "durable"
	Rebuildable     PersistenceClass = "rebuildable"
	Ephemeral       PersistenceClass = "ephemeral"
)

// Rule applies to a relative path and its descendants. The longest matching
// prefix wins. Unknown paths are durable; policy must never guess away user data.
type Rule struct {
	Path    string           `json:"path"`
	Class   PersistenceClass `json:"class"`
	Recipe  string           `json:"recipe,omitempty"`
	Inputs  []string         `json:"inputs,omitempty"`
	Runtime string           `json:"runtime,omitempty"`
}
type Policy struct {
	Rules []Rule `json:"rules,omitempty"`
}

func (p Policy) Validate() error {
	if data, err := json.Marshal(p); err != nil || len(data) > 1<<20 {
		return fmt.Errorf("%w: policy exceeds its JSON size limit", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, r := range p.Rules {
		if !utf8.ValidString(r.Recipe) || !utf8.ValidString(r.Runtime) {
			return fmt.Errorf("%w: policy recipes must be valid UTF-8", ErrInvalid)
		}
		if !validPath(r.Path) || r.Path == ".git" || strings.HasPrefix(r.Path, ".git/") || seen[r.Path] {
			return fmt.Errorf("%w: invalid or duplicate policy path %q (.git is always durable)", ErrInvalid, r.Path)
		}
		inputs := map[string]bool{}
		for _, input := range r.Inputs {
			if !validPath(input) || inputs[input] {
				return fmt.Errorf("%w: invalid or duplicate recipe input %q", ErrInvalid, input)
			}
			inputs[input] = true
		}
		if r.Class != Rebuildable && (len(r.Inputs) > 0 || r.Runtime != "") {
			return fmt.Errorf("%w: inputs and runtime require rebuildable class", ErrInvalid)
		}
		seen[r.Path] = true
		switch r.Class {
		case Durable, Ephemeral, SharedImmutable:
		case Rebuildable:
			if strings.TrimSpace(r.Recipe) == "" {
				return fmt.Errorf("%w: rebuildable path %q needs a recipe", ErrInvalid, r.Path)
			}
		default:
			return fmt.Errorf("%w: unknown persistence class %q", ErrInvalid, r.Class)
		}
	}
	return nil
}

func (p Policy) Classify(name string) PersistenceClass {
	class, longest := Durable, 0
	for _, r := range p.Rules {
		if (name == r.Path || strings.HasPrefix(name, r.Path+"/")) && len(r.Path) > longest {
			class, longest = r.Class, len(r.Path)
		}
	}
	return class
}

type GitBase struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
}

// Entry preserves namespace, bytes, permission bits, symlink targets, and mtime.
// Ownership, xattrs, hardlink identity, processes, and mounts are outside v1.
type Entry struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Mode    uint32 `json:"mode"`
	Size    int64  `json:"size,omitempty"`
	ModTime int64  `json:"mtime_ns,omitempty"`
	Digest  string `json:"digest,omitempty"`
	Target  string `json:"target,omitempty"`
}

type Manifest struct {
	Save       *SaveReport `json:"save,omitempty"`
	GitObjects *GitObjects `json:"git_objects,omitempty"`
	Version    int         `json:"version"`
	Parent     string      `json:"parent,omitempty"`
	Git        *GitBase    `json:"git,omitempty"`
	Policy     Policy      `json:"policy"`
	Entries    []Entry     `json:"entries"`
}
type Snapshot struct {
	ID       string   `json:"id"`
	Manifest Manifest `json:"manifest"`
}
type SnapshotOptions struct {
	Source string   `json:"source,omitempty"`
	Git    *GitBase `json:"git,omitempty"`
	Policy Policy   `json:"policy,omitempty"`
}

type PoolOptions struct {
	Base         string `json:"base"`
	Concurrency  int    `json:"concurrency"`
	LeaseSeconds int    `json:"lease_seconds,omitempty"`
}
type Execution struct {
	ID             string    `json:"id"`
	PoolID         string    `json:"pool_id"`
	Path           string    `json:"path"`
	Base           string    `json:"base"`
	Lease          string    `json:"lease"`
	Generation     uint64    `json:"generation"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}
type Verification struct {
	Valid       bool     `json:"valid"`
	Differences []string `json:"differences"`
}

type Stats struct {
	HydratedFiles         uint64  `json:"hydrated_files"`
	HydrationErrors       uint64  `json:"hydration_errors"`
	HydrationSeconds      float64 `json:"hydration_seconds"`
	MaintenanceErrors     uint64  `json:"maintenance_errors"`
	WarmedExecutions      uint64  `json:"warmed_executions"`
	Prefetches            uint64  `json:"prefetches"`
	PreparingExecutions   int     `json:"preparing_executions"`
	Acquires              uint64  `json:"acquires"`
	Resets                uint64  `json:"resets"`
	Recycles              uint64  `json:"recycles"`
	Verifications         uint64  `json:"verifications"`
	VerificationFailures  uint64  `json:"verification_failures"`
	Quarantines           uint64  `json:"quarantines"`
	ClonedFiles           uint64  `json:"cloned_files"`
	CopiedFiles           uint64  `json:"copied_files"`
	ContentBytesWritten   uint64  `json:"content_bytes_written"`
	RemoteBytesUploaded   uint64  `json:"remote_bytes_uploaded"`
	RemoteBytesDownloaded uint64  `json:"remote_bytes_downloaded"`
	ExpiredLeases         uint64  `json:"expired_leases"`
	ActiveExecutions      int     `json:"active_executions"`
	IdleExecutions        int     `json:"idle_executions"`
	Pools                 int     `json:"pools"`
}

func validPath(p string) bool {
	return utf8.ValidString(p) && p != "" && p != "." && p != ".." && !strings.HasPrefix(p, "../") &&
		!strings.ContainsAny(p, "\\\x00") && !path.IsAbs(p) && path.Clean(p) == p
}
func validDigest(s string) bool {
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

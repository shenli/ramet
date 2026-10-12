// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
)

// SaveReport records what this publication did, not what a later policy might do.
type SaveReport struct {
	Mode     string        `json:"mode"`
	Excluded []OmittedPath `json:"excluded"`
	Rebuild  []RebuildStep `json:"rebuild"`
}
type OmittedPath struct {
	Path  string           `json:"path"`
	Class PersistenceClass `json:"class"`
	Kind  string           `json:"kind"`
	Bytes int64            `json:"bytes"`
}
type RecipeInput struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// ID binds approval to the recipe, input digests and caller-declared runtime.
// Runtime is provenance, not proof of the environment actually used.
type RebuildStep struct {
	ID      string        `json:"id"`
	Path    string        `json:"path"`
	Recipe  string        `json:"recipe"`
	Runtime string        `json:"runtime,omitempty"`
	Inputs  []RecipeInput `json:"inputs"`
}
type PolicyViolation struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}
type PathDecision struct {
	Path   string           `json:"path"`
	Class  PersistenceClass `json:"class"`
	Action string           `json:"action"`
	Kind   string           `json:"kind"`
	Bytes  int64            `json:"bytes"`
}
type PromotionPreview struct {
	Valid         bool              `json:"valid"`
	RetainedBytes int64             `json:"retained_bytes"`
	OmittedBytes  int64             `json:"omitted_bytes"`
	OptimizedGit  bool              `json:"optimized_git"`
	Paths         []PathDecision    `json:"paths"`
	Rebuild       []RebuildStep     `json:"rebuild"`
	Violations    []PolicyViolation `json:"violations"`
}
type RestorePlan struct {
	Snapshot   string            `json:"snapshot"`
	SaveMode   string            `json:"save_mode"`
	Valid      bool              `json:"valid"`
	Steps      []RebuildStep     `json:"steps"`
	Violations []PolicyViolation `json:"violations"`
}

func fileKind(info os.FileInfo) string {
	switch {
	case info.IsDir():
		return "dir"
	case info.Mode().IsRegular():
		return "file"
	case info.Mode()&os.ModeSymlink != 0:
		return "symlink"
	default:
		return "special"
	}
}
func omitted(name string, class PersistenceClass, info os.FileInfo) OmittedPath {
	bytes := int64(0)
	if info.Mode().IsRegular() {
		bytes = info.Size()
	}
	return OmittedPath{Path: name, Class: class, Kind: fileKind(info), Bytes: bytes}
}
func rebuildSteps(policy Policy, entries []Entry) ([]RebuildStep, error) {
	byPath := map[string]Entry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}
	steps := []RebuildStep{}
	for _, rule := range policy.Rules {
		if rule.Class != Rebuildable {
			continue
		}
		step := RebuildStep{Path: rule.Path, Recipe: rule.Recipe, Runtime: rule.Runtime, Inputs: []RecipeInput{}}
		for _, name := range rule.Inputs {
			entry, ok := byPath[name]
			if !ok || entry.Kind != "file" || !validDigest(entry.Digest) {
				return nil, fmt.Errorf("%w: recipe %s input %s must be a retained regular file", ErrInvalid, rule.Path, name)
			}
			step.Inputs = append(step.Inputs, RecipeInput{Path: name, SHA256: entry.Digest})
		}
		sort.Slice(step.Inputs, func(i, j int) bool { return step.Inputs[i].Path < step.Inputs[j].Path })
		b, _ := json.Marshal(step)
		step.ID = hashBytes(b)
		steps = append(steps, step)
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].Path < steps[j].Path })
	return steps, nil
}
func validateSaveReport(m Manifest) error {
	if m.Save == nil {
		return nil
	} // Includes internal verification manifests.
	if m.Version < 3 {
		return fmt.Errorf("%w: save report requires manifest v3", ErrInvalid)
	}
	switch m.Save.Mode {
	case "base", "checkpoint", "promotion":
	default:
		return fmt.Errorf("%w: invalid save mode", ErrInvalid)
	}
	entries := map[string]bool{}
	for _, e := range m.Entries {
		entries[e.Path] = true
	}
	last := ""
	for _, e := range m.Save.Excluded {
		if !validPath(e.Path) || e.Path <= last || entries[e.Path] || e.Bytes < 0 ||
			(e.Class != Ephemeral && e.Class != Rebuildable) || e.Class != m.Policy.Classify(e.Path) ||
			(e.Kind != "dir" && e.Kind != "file" && e.Kind != "symlink" && e.Kind != "special") ||
			(e.Kind != "file" && e.Bytes != 0) || m.Save.Mode != "promotion" {
			return fmt.Errorf("%w: invalid excluded path %q", ErrInvalid, e.Path)
		}
		last = e.Path
	}
	if m.Save.Mode == "base" && len(m.Save.Rebuild) > 0 {
		return fmt.Errorf("%w: base capture cannot need reconstruction", ErrInvalid)
	}
	if len(m.Save.Rebuild) > 0 || m.Save.Mode == "promotion" {
		steps, err := rebuildSteps(m.Policy, m.Entries)
		if err != nil {
			return err
		}
		// nil and empty both represent no steps.
		if len(steps) != len(m.Save.Rebuild) || (len(steps) > 0 && !reflect.DeepEqual(steps, m.Save.Rebuild)) {
			return fmt.Errorf("%w: rebuild plan disagrees with policy or inputs", ErrInvalid)
		}
	}
	return nil
}

// PreviewPromotion is advisory. Stop writers; a later promotion scans again.
// It publishes no snapshot or content. Lazy reads may hydrate the local cache.
// Bytes are logical file sizes, not deduplicated transfer or disk estimates.
// Optimized Git object packs are excluded from the byte totals and checked by
// promotion itself, not by this preview.
func (p *Pool) PreviewPromotion(ctx context.Context, e Execution) (PromotionPreview, error) {
	if err := p.store.enter(); err != nil {
		return PromotionPreview{}, err
	}
	defer p.store.leave()
	item, err := p.claim(e)
	if err != nil {
		return PromotionPreview{}, err
	}
	defer item.mu.Unlock()
	base := p.base.Manifest
	out := PromotionPreview{Valid: true, OptimizedGit: base.GitObjects != nil, Paths: []PathDecision{}, Rebuild: []RebuildStep{}, Violations: []PolicyViolation{}}
	violation := func(name, reason string) {
		out.Valid = false
		out.Violations = append(out.Violations, PolicyViolation{name, reason})
	}
	original := map[string]Entry{}
	for _, entry := range base.Entries {
		original[entry.Path] = entry
	}
	seen := map[string]bool{}
	needed := map[string]bool{}
	retained := []Entry{}
	err = filepath.WalkDir(item.execution.Path, func(name string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == item.execution.Path {
			return nil
		}
		rel, err := filepath.Rel(item.execution.Path, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !validPath(rel) {
			return fmt.Errorf("%w: invalid path %q", ErrInvalid, rel)
		}
		if out.OptimizedGit && rel == ".git/objects" {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		class := base.Policy.Classify(rel)
		decision := PathDecision{Path: rel, Class: class, Action: "preserve", Kind: fileKind(info)}
		if info.Mode().IsRegular() {
			decision.Bytes = info.Size()
		}
		if class == Ephemeral {
			decision.Action = "discard"
		}
		if class == Rebuildable {
			decision.Action = "rebuild"
		}
		out.Paths = append(out.Paths, decision)
		seen[rel] = true
		if (class == Rebuildable || class == Ephemeral) && !d.IsDir() {
			return nil
		}
		entry := Entry{Path: rel, Kind: decision.Kind, Mode: uint32(info.Mode().Perm()), ModTime: info.ModTime().UnixNano()}
		switch entry.Kind {
		case "file":
			entry.Digest, entry.Size, err = digestFile(ctx, name)
			if err != nil {
				return err
			}
			after, err := os.Lstat(name)
			if err != nil {
				return err
			}
			if !os.SameFile(info, after) || info.Size() != after.Size() || info.ModTime() != after.ModTime() || info.Mode() != after.Mode() {
				return fmt.Errorf("%w: source changed during preview; stop writers", ErrConflict)
			}
		case "symlink":
			entry.Mode, entry.ModTime = 0, 0
			entry.Target, err = os.Readlink(name)
			if err != nil {
				return err
			}
			if !safeTarget(rel, entry.Target) {
				violation(rel, "symlink escapes workspace or has an unsupported target")
			}
		case "dir":
		default:
			violation(rel, "unsupported special file; classify as ephemeral")
		}
		if class == SharedImmutable {
			before, ok := original[rel]
			if !ok || !sameEntry(before, entry) {
				violation(rel, "shared immutable path added or changed")
			}
		}
		retained = append(retained, entry)
		if class != Ephemeral && class != Rebuildable {
			for parent := rel; parent != "."; parent = path.Dir(parent) {
				needed[parent] = true
			}
		}
		return nil
	})
	if err != nil {
		return PromotionPreview{}, err
	}
	for _, entry := range base.Entries {
		if base.Policy.Classify(entry.Path) == SharedImmutable && !seen[entry.Path] {
			violation(entry.Path, "shared immutable path deleted")
		}
	}
	kept := []Entry{}
	for _, entry := range retained {
		if needed[entry.Path] {
			kept = append(kept, entry)
		}
	}
	for i := range out.Paths {
		decision := &out.Paths[i]
		if needed[decision.Path] {
			if decision.Action != "preserve" {
				decision.Action = "retain_parent"
			}
			out.RetainedBytes += decision.Bytes
		} else {
			out.OmittedBytes += decision.Bytes
		}
	}
	out.Rebuild, err = rebuildSteps(base.Policy, kept)
	if err != nil {
		violation("", err.Error())
		out.Rebuild = []RebuildStep{}
	}
	if err := validateSymlinks(kept); err != nil {
		violation("", err.Error())
	}
	sort.Slice(out.Violations, func(i, j int) bool { return out.Violations[i].Path < out.Violations[j].Path })
	return out, nil
}

func snapshotRestorePlan(snapshot Snapshot) RestorePlan {
	plan := RestorePlan{Snapshot: snapshot.ID, SaveMode: "legacy_unknown", Valid: true, Steps: []RebuildStep{}, Violations: []PolicyViolation{}}
	if snapshot.Manifest.Save != nil {
		plan.SaveMode = snapshot.Manifest.Save.Mode
		plan.Steps = append(plan.Steps, snapshot.Manifest.Save.Rebuild...)
	} else {
		// Older snapshots did not record whether exclusions had been applied.
		for _, rule := range snapshot.Manifest.Policy.Rules {
			if rule.Class == Rebuildable {
				plan.Valid = false
				plan.Violations = append(plan.Violations, PolicyViolation{rule.Path, "legacy snapshot has no recorded reconstruction contract"})
			}
		}
	}
	return plan
}
func (s *Store) RestorePlan(ctx context.Context, id string) (RestorePlan, error) {
	snapshot, err := s.GetSnapshot(ctx, id)
	if err != nil {
		return RestorePlan{}, err
	}
	return snapshotRestorePlan(snapshot), nil
}

// RestorePlan checks declared inputs against this execution's current bytes.
// Valid means inputs match, not that recipes have run or the application is ready.
func (p *Pool) RestorePlan(ctx context.Context, e Execution) (RestorePlan, error) {
	if err := p.store.enter(); err != nil {
		return RestorePlan{}, err
	}
	defer p.store.leave()
	item, err := p.claim(e)
	if err != nil {
		return RestorePlan{}, err
	}
	defer item.mu.Unlock()
	plan := snapshotRestorePlan(p.base)
	for _, step := range plan.Steps {
		for _, input := range step.Inputs {
			if err := ctx.Err(); err != nil {
				return RestorePlan{}, err
			}
			name := filepath.Join(item.execution.Path, filepath.FromSlash(input.Path))
			resolved, resolveErr := filepath.EvalSymlinks(name)
			info, statErr := os.Lstat(name)
			// Reject links in inputs, including ancestor directories, before opening.
			if resolveErr != nil || statErr != nil || filepath.Clean(resolved) != filepath.Clean(name) || !info.Mode().IsRegular() {
				plan.Valid = false
				plan.Violations = append(plan.Violations, PolicyViolation{input.Path, "recipe input missing or not a regular unlinked path"})
				continue
			}
			digest, _, err := digestFile(ctx, name)
			if err != nil {
				return RestorePlan{}, err
			}
			if digest != input.SHA256 {
				plan.Valid = false
				plan.Violations = append(plan.Violations, PolicyViolation{input.Path, "recipe input digest changed"})
			}
		}
	}
	return plan, nil
}

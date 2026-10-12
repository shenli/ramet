// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func persistenceFixture(t *testing.T) (*Store, *Pool, Execution) {
	t.Helper()
	s, source, _ := fixture(t)
	write(t, filepath.Join(source, "lock.json"), "locked dependencies")
	write(t, filepath.Join(source, "deps/package"), "generated")
	write(t, filepath.Join(source, "scratch/debug"), "temporary")
	write(t, filepath.Join(source, "scratch/reports/keep"), "useful")
	write(t, filepath.Join(source, "fixtures/input"), "fixed")
	policy := Policy{Rules: []Rule{
		{Path: "deps", Class: Rebuildable, Recipe: "install locked dependencies", Inputs: []string{"lock.json"}, Runtime: "python:test"},
		{Path: "scratch", Class: Ephemeral}, {Path: "scratch/reports", Class: Durable},
		{Path: "fixtures", Class: SharedImmutable},
	}}
	base, err := s.Snapshot(context.Background(), SnapshotOptions{Source: source, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if base.Manifest.Save.Mode != "base" || len(base.Manifest.Save.Rebuild) != 0 {
		t.Fatal(base.Manifest.Save)
	}
	p := poolFor(t, s, base, 2)
	return s, p, acquire(t, p)
}
func TestPromotionPreviewAndRestoreContract(t *testing.T) {
	s, p, e := persistenceFixture(t)
	ctx := context.Background()
	before := s.Stats()
	snapshots, _ := s.ListSnapshots(ctx)
	preview, err := p.PreviewPromotion(ctx, e)
	if err != nil || !preview.Valid {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	after := s.Stats()
	next, _ := s.ListSnapshots(ctx)
	if len(next) != len(snapshots) || before.ContentBytesWritten != after.ContentBytesWritten || before.RemoteBytesUploaded != after.RemoteBytesUploaded {
		t.Fatal("preview published content")
	}
	decisions := map[string]PathDecision{}
	var retained int64
	for _, d := range preview.Paths {
		decisions[d.Path] = d
		if d.Action == "preserve" || d.Action == "retain_parent" {
			retained += d.Bytes
		}
	}
	if decisions["scratch"].Action != "retain_parent" || decisions["scratch/debug"].Action != "discard" || decisions["deps/package"].Action != "rebuild" || retained != preview.RetainedBytes {
		t.Fatal(preview)
	}
	if preview.OmittedBytes != int64(len("generatedtemporary")) {
		t.Fatal(preview.OmittedBytes)
	}
	full, err := p.Checkpoint(ctx, e)
	if err != nil || full.Manifest.Save.Mode != "checkpoint" || len(full.Manifest.Save.Excluded) != 0 {
		t.Fatalf("checkpoint: %+v %v", full, err)
	}
	saved, err := p.Promote(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Manifest.Save.Mode != "promotion" || !reflect.DeepEqual(saved.Manifest.Save.Rebuild, preview.Rebuild) {
		t.Fatal(saved.Manifest.Save)
	}
	for _, entry := range saved.Manifest.Entries {
		if decisions[entry.Path].Action != "preserve" && decisions[entry.Path].Action != "retain_parent" {
			t.Fatalf("unexpected retained %s", entry.Path)
		}
	}
	for _, entry := range saved.Manifest.Save.Excluded {
		if decisions[entry.Path].Action == "preserve" || decisions[entry.Path].Action == "retain_parent" {
			t.Fatal(entry)
		}
	}
	restored := poolFor(t, s, saved, 1)
	execution := acquire(t, restored)
	plan, err := restored.RestorePlan(ctx, execution)
	if err != nil || !plan.Valid || len(plan.Steps) != 1 || plan.Steps[0].Inputs[0].SHA256 != hashBytes([]byte("locked dependencies")) {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if _, err := os.Stat(filepath.Join(execution.Path, "deps")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rebuildable retained")
	}
	if read(t, filepath.Join(execution.Path, "scratch/reports/keep")) != "useful" {
		t.Fatal("lost durable override")
	}
	checkpoint, err := restored.Checkpoint(ctx, execution)
	if err != nil || len(checkpoint.Manifest.Save.Rebuild) != 1 {
		t.Fatalf("lost inherited rebuild plan: %+v %v", checkpoint, err)
	}
	write(t, filepath.Join(execution.Path, "lock.json"), "changed")
	plan, err = restored.RestorePlan(ctx, execution)
	if err != nil || plan.Valid || len(plan.Violations) != 1 {
		t.Fatalf("changed inputs accepted: %+v %v", plan, err)
	}
	_, err = restored.Reset(ctx, execution)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restored.RestorePlan(ctx, execution); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale handle: %v", err)
	}
}
func TestPreviewRejectsImmutableChangesAndInvalidRecipeInputs(t *testing.T) {
	for _, change := range []string{"edit", "delete", "add", "excluded_input"} {
		t.Run(change, func(t *testing.T) {
			_, p, e := persistenceFixture(t)
			ctx := context.Background()
			switch change {
			case "edit":
				write(t, filepath.Join(e.Path, "fixtures/input"), "modified")
			case "delete":
				if err := os.Remove(filepath.Join(e.Path, "fixtures/input")); err != nil {
					t.Fatal(err)
				}
			case "add":
				write(t, filepath.Join(e.Path, "fixtures/new"), "new")
			case "excluded_input":
				p.base.Manifest.Policy.Rules[0].Inputs = []string{"scratch/debug"}
			}
			preview, err := p.PreviewPromotion(ctx, e)
			if err != nil || preview.Valid || len(preview.Violations) == 0 {
				t.Fatalf("preview accepted: %+v %v", preview, err)
			}
			if _, err := p.Promote(ctx, e); err == nil {
				t.Fatal("promotion accepted invalid state")
			}
		})
	}
}
func TestSaveReportValidationAndLegacy(t *testing.T) {
	_, p, e := persistenceFixture(t)
	saved, err := p.Promote(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Manifest){
		func(m *Manifest) { m.Save.Mode = "unknown" },
		func(m *Manifest) { m.Save.Rebuild[0].Recipe = "unapproved command" },
		func(m *Manifest) { m.Save.Rebuild[0].Inputs[0].SHA256 = hashBytes([]byte("other")) },
		func(m *Manifest) { m.Save.Excluded[0].Path = "../outside" },
		func(m *Manifest) { m.Save.Excluded[0].Class = Durable },
	} {
		b, _ := json.Marshal(saved.Manifest)
		var m Manifest
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		mutate(&m)
		if err := validateManifest(m); err == nil {
			t.Fatalf("accepted invalid report: %+v", m.Save)
		}
	}
	old := saved
	old.Manifest.Version = 2
	old.Manifest.Save = nil
	if err := validateManifest(old.Manifest); err != nil {
		t.Fatal(err)
	}
	if snapshotRestorePlan(old).Valid {
		t.Fatal("legacy recipe claimed ready")
	}
}
func TestRestorePlanRejectsSymlinkInput(t *testing.T) {
	s, p, e := persistenceFixture(t)
	ctx := context.Background()
	saved, err := p.Promote(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	restored := poolFor(t, s, saved, 1)
	r := acquire(t, restored)
	if err := os.Remove(filepath.Join(r.Path, "lock.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("src/main.txt", filepath.Join(r.Path, "lock.json")); err != nil {
		t.Fatal(err)
	}
	plan, err := restored.RestorePlan(ctx, r)
	if err != nil || plan.Valid {
		t.Fatalf("symlink input accepted %+v %v", plan, err)
	}
}

func TestExamplePersistencePolicies(t *testing.T) {
	for _, name := range []string{"python", "node"} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("..", "examples", "policies", name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var policy Policy
			if err := json.Unmarshal(b, &policy); err != nil {
				t.Fatal(err)
			}
			if err := policy.Validate(); err != nil {
				t.Fatal(err)
			}
			s, source, _ := fixture(t)
			for _, rule := range policy.Rules {
				for _, input := range rule.Inputs {
					write(t, filepath.Join(source, input), "pinned fixture input")
				}
				write(t, filepath.Join(source, rule.Path, "entry"), "test bytes")
			}
			write(t, filepath.Join(source, ".git/refs/heads/local"), "local history fixture")
			snap, err := s.Snapshot(context.Background(), SnapshotOptions{Source: source, Policy: policy})
			if err != nil {
				t.Fatal(err)
			}
			p := poolFor(t, s, snap, 1)
			e := acquire(t, p)
			preview, err := p.PreviewPromotion(context.Background(), e)
			if err != nil || !preview.Valid {
				t.Fatal(preview, err)
			}
			saved, err := p.Promote(context.Background(), e)
			if err != nil {
				t.Fatal(err)
			}
			present := map[string]bool{}
			for _, entry := range saved.Manifest.Entries {
				present[entry.Path] = true
			}
			if !present[".git/refs/heads/local"] || !present["scratch/reports/entry"] || present["scratch/entry"] {
				t.Fatal(present)
			}
			if len(saved.Manifest.Save.Rebuild) != 1 {
				t.Fatal(saved.Manifest.Save.Rebuild)
			}
		})
	}
}

// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Regression: JSON replaces invalid UTF-8 with U+FFFD without returning an
// error. An acknowledged capture must never silently change its namespace.
func TestSnapshotRejectsUnrepresentableNamespace(t *testing.T) {
	for _, value := range []string{"file-\xff", "file-\xc0\xaf", "file-\xed\xa0\x80"} {
		for _, kind := range []string{"filename", "symlink"} {
			t.Run(kind+"/"+strings.ReplaceAll(value, "file-", ""), func(t *testing.T) {
				s, err := Open(t.TempDir(), Options{})
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				source := t.TempDir()
				if kind == "filename" {
					err = os.WriteFile(filepath.Join(source, value), []byte("selected"), 0600)
				} else {
					err = os.Symlink(value, filepath.Join(source, "link"))
				}
				if err != nil {
					t.Skipf("filesystem cannot represent this byte sequence: %v", err)
				}
				if _, err = s.Snapshot(context.Background(), SnapshotOptions{Source: source}); !errors.Is(err, ErrInvalid) {
					t.Fatalf("expected explicit invalid request, got %v", err)
				}
				snapshots, err := s.ListSnapshots(context.Background())
				if err != nil || len(snapshots) != 0 {
					t.Fatalf("invalid namespace published: %v %v", snapshots, err)
				}
			})
		}
	}
}

func TestManifestRejectsUnrepresentableText(t *testing.T) {
	for _, m := range []Manifest{
		{Version: FormatVersion, Policy: Policy{Rules: []Rule{{Path: "cache", Class: Rebuildable, Recipe: "command-\xff"}}}},
		{Version: FormatVersion, Policy: Policy{Rules: []Rule{{Path: "cache-\xff", Class: Ephemeral}}}},
		{Version: FormatVersion, Git: &GitBase{Repository: "repo-\xff", Revision: "main"}},
		{Version: FormatVersion, Git: &GitBase{Repository: "repo", Revision: "ref-\xff"}},
	} {
		if err := validateManifest(m); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted unrepresentable metadata: %+v (%v)", m, err)
		}
	}
}

func FuzzSymlinkTargetRoundTrip(f *testing.F) {
	for _, target := range []string{"file", "sub/../file", "../escape", "file-\xff", "é", "e\u0301"} {
		f.Add(target)
	}
	f.Fuzz(func(t *testing.T, target string) {
		original := Entry{Path: "link", Kind: "symlink", Target: target}
		m := Manifest{Version: FormatVersion, Entries: []Entry{original}}
		if validateManifest(m) != nil {
			return
		}
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var restored Manifest
		if err = json.Unmarshal(data, &restored); err != nil {
			t.Fatal(err)
		}
		if len(restored.Entries) != 1 || !reflect.DeepEqual(restored.Entries[0], original) {
			t.Fatalf("target changed during serialization: %q", target)
		}
	})
}

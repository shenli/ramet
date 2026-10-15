// SPDX-License-Identifier: Apache-2.0
package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/shenli/ramet/engine"
)

func TestStrictConfiguration(t *testing.T) {
	for _, body := range []string{"worker:\n  typo: true\n", "metadata:\n  provider: typo\n", "metadata:\n  provider: postgres\ncontent:\n  provider: local\n", "worker: {}\n---\nworker: {}\n"} {
		name := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(name, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(name); err == nil {
			t.Fatal("invalid configuration accepted", body)
		}
	}
	c, err := Load("../../configs/postgres-s3.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAMET_DATABASE_URL", "")
	if _, err = c.Options(context.Background()); err == nil {
		t.Fatal("missing credentials accepted")
	}
}

func TestWholeFileStorageConfiguration(t *testing.T) {
	name := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(name, []byte("worker:\n  disable_chunking: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(name)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := c.Options(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s, err := engine.Open(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	source := t.TempDir()
	if err = os.WriteFile(filepath.Join(source, "large"), make([]byte, 8<<20), 0600); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot(context.Background(), engine.SnapshotOptions{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Manifest.Entries[0].Chunks) != 0 {
		t.Fatal("configured whole-file storage was ignored")
	}
}

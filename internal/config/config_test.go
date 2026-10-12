// SPDX-License-Identifier: Apache-2.0
package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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

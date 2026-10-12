// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestLazyCrashHelper(t *testing.T) {
	root := os.Getenv("RAMET_LAZY_CRASH_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	s, err := Open(root, Options{Backend: "lazy"})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot(context.Background(), SnapshotOptions{Source: os.Getenv("RAMET_LAZY_CRASH_SOURCE")})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePool(context.Background(), PoolOptions{Base: snap.ID, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	e, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(e)
	if err = os.WriteFile(os.Getenv("RAMET_LAZY_CRASH_RESULT"), data, 0600); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestLazyCrashRecovery(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("RAMET_TEST_FUSE") != "1" {
		t.Skip("requires Linux FUSE")
	}
	source := t.TempDir()
	write(t, filepath.Join(source, "file"), "survives worker crash")
	root, result := filepath.Join(t.TempDir(), "store"), filepath.Join(t.TempDir(), "result.json")
	command := exec.Command(os.Args[0], "-test.run=^TestLazyCrashHelper$")
	command.Env = append(os.Environ(), "RAMET_LAZY_CRASH_ROOT="+root, "RAMET_LAZY_CRASH_SOURCE="+source, "RAMET_LAZY_CRASH_RESULT="+result)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	var data []byte
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		data, err = os.ReadFile(result)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var e Execution
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatal("helper did not acquire", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	s, err := Open(root, Options{Backend: "lazy"})
	if err != nil {
		t.Fatal("abandoned FUSE mount blocked recovery", err)
	}
	defer s.Close()
	entries, err := os.ReadDir(filepath.Join(root, "executions"))
	if err != nil || len(entries) != 0 {
		t.Fatal("stale mount remains", entries, err)
	}
	snap, err := s.GetSnapshot(context.Background(), e.Base)
	if err != nil {
		t.Fatal(err)
	}
	restored := acquire(t, poolFor(t, s, snap, 1))
	b, err := os.ReadFile(filepath.Join(restored.Path, "file"))
	if err != nil || string(b) != "survives worker crash" {
		t.Fatal(string(b), err)
	}
}

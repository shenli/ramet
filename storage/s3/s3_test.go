// SPDX-License-Identifier: Apache-2.0
package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/shenli/ramet/storage"
)

func TestCheckIntegrityAndReadAvoidance(t *testing.T) {
	data := []byte("verified artifact content")
	sum := sha256.Sum256(data)
	ref := storage.Ref{Digest: hex.EncodeToString(sum[:]), Size: int64(len(data))}
	native := base64.StdEncoding.EncodeToString(sum[:])
	for _, tc := range []struct {
		name, checksum, mode        string
		corrupt, badSize, wantError bool
		gets                        int32
	}{
		{name: "native", checksum: native},
		{name: "legacy", gets: 1},
		{name: "composite", checksum: native + "-2", mode: "COMPOSITE", gets: 1},
		{name: "wrong_native", checksum: base64.StdEncoding.EncodeToString(make([]byte, 32)), wantError: true},
		{name: "malformed_native", checksum: "invalid", wantError: true},
		{name: "forged_user_metadata", corrupt: true, gets: 1, wantError: true},
		{name: "wrong_size", checksum: native, badSize: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gets atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead && r.Method != http.MethodGet {
					t.Errorf("unexpected %s", r.Method)
					w.WriteHeader(500)
					return
				}
				body := bytes.Clone(data)
				if tc.corrupt {
					body[0] ^= 1
				}
				if tc.badSize {
					body = append(body, 'x')
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.Header().Set("Last-Modified", "Mon, 12 Oct 2026 12:00:00 GMT")
				w.Header().Set("ETag", `"opaque-etag"`)
				w.Header().Set("X-Amz-Meta-Sha256", ref.Digest)
				if tc.checksum != "" {
					w.Header().Set("X-Amz-Checksum-Sha256", tc.checksum)
				}
				if tc.mode != "" {
					w.Header().Set("X-Amz-Checksum-Type", tc.mode)
				}
				if r.Method == http.MethodGet {
					gets.Add(1)
					_, _ = w.Write(body)
				}
			}))
			defer server.Close()
			s, err := New(Options{Endpoint: server.URL, Region: "us-east-1", Bucket: "test-bucket", Prefix: "objects", AccessKey: "test", SecretKey: "test"})
			if err != nil {
				t.Fatal(err)
			}
			err = s.Check(context.Background(), ref)
			if tc.wantError != errors.Is(err, storage.ErrCorrupt) || (!tc.wantError && err != nil) {
				t.Fatalf("Check: %v", err)
			}
			if gets.Load() != tc.gets {
				t.Fatalf("GETs = %d, want %d", gets.Load(), tc.gets)
			}
			if tc.name == "native" {
				added, err := s.Put(context.Background(), ref, bytes.NewReader(data))
				if err != nil || added || gets.Load() != 0 {
					t.Fatal("unchanged upload downloaded content", added, err)
				}
				if _, err = s.Put(context.Background(), ref, bytes.NewReader([]byte("wrong"))); !errors.Is(err, storage.ErrCorrupt) {
					t.Fatal("invalid upload source accepted", err)
				}
			}
		})
	}
}

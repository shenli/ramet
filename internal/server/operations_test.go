// SPDX-License-Identifier: Apache-2.0
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func issued(t *testing.T, o *operationRegistry) string {
	t.Helper()
	w := httptest.NewRecorder()
	o.issue(w, httptest.NewRequest("GET", "/v1/operations/new", nil))
	var value struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value.ID
}
func keyed(o *operationRegistry, id, body string, next http.Handler) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/test", strings.NewReader(body))
	r.Header.Set("Idempotency-Key", id)
	w := httptest.NewRecorder()
	o.handle(w, r, next)
	return w
}
func TestOperationConcurrentReplayConflictAndExpiry(t *testing.T) {
	o := newOperationRegistry()
	now := time.Now()
	o.now = func() time.Time { return now }
	id := issued(t, o)
	var effects atomic.Int32
	entered, finish, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effects.Add(1)
		close(entered)
		<-finish
		respond(w, 200, map[string]string{"value": "saved"})
	})
	go func() { defer close(done); keyed(o, id, `{"generation":1}`, handler) }()
	<-entered
	if w := keyed(o, id, `{"generation":1}`, handler); w.Code != 409 || !strings.Contains(w.Body.String(), "operation_pending") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := keyed(o, id, `{"generation":2}`, handler); w.Code != 409 || !strings.Contains(w.Body.String(), "operation_conflict") {
		t.Fatal(w.Code, w.Body.String())
	}
	close(finish)
	<-done
	if w := keyed(o, id, `{"generation":1}`, handler); w.Code != 200 || !strings.Contains(w.Body.String(), "saved") {
		t.Fatal(w.Code, w.Body.String())
	}
	if effects.Load() != 1 {
		t.Fatal("duplicated mutation", effects.Load())
	}
	now = now.Add(operationTTL + time.Second)
	if w := keyed(o, id, `{"generation":1}`, handler); w.Code != 410 {
		t.Fatal(w.Code)
	}
	fresh := newOperationRegistry()
	if w := keyed(fresh, id, `{"generation":1}`, handler); w.Code != 409 || !strings.Contains(w.Body.String(), "operation_session_changed") {
		t.Fatal(w.Code, w.Body.String())
	}
	if effects.Load() != 1 {
		t.Fatal("expired/session-changed ID reexecuted")
	}
}
func TestOperationCapacityAndOversizeFailClosed(t *testing.T) {
	o := newOperationRegistry()
	id := issued(t, o)
	calls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.Copy(w, bytes.NewReader(bytes.Repeat([]byte("x"), operationMaxBody+1)))
	})
	o.bytes = operationMaxBytes
	if w := keyed(o, id, `{}`, handler); w.Code != 429 || calls != 0 {
		t.Fatal("capacity allowed a side effect")
	}
	o.bytes = 0
	for i := 0; i < 2; i++ {
		if w := keyed(o, id, `{}`, handler); w.Code != 409 || !strings.Contains(w.Body.String(), "operation_result_unavailable") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if calls != 1 {
		t.Fatal("unknown outcome reexecuted", calls)
	}
}
func TestOperationRejectedResponsesAreReceipted(t *testing.T) {
	o := newOperationRegistry()
	id := issued(t, o)
	calls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "1")
		operationError(w, 429, "quota_exceeded", "full")
	})
	for i := 0; i < 2; i++ {
		w := keyed(o, id, `{}`, handler)
		if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
			t.Fatal(w.Code, w.Header())
		}
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}

type expireOnRead struct {
	io.Reader
	expire func()
}

func (r *expireOnRead) Read(b []byte) (int, error) { r.expire(); return r.Reader.Read(b) }
func TestOperationCannotExpireBetweenValidationAndAcceptance(t *testing.T) {
	o := newOperationRegistry()
	now := time.Now()
	o.now = func() time.Time { return now }
	id := issued(t, o)
	calls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; respond(w, 200, map[string]string{"ok": "yes"}) })
	if w := keyed(o, id, `{}`, handler); w.Code != 200 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("POST", "/v1/test", nil)
	r.Header.Set("Idempotency-Key", id)
	r.Body = io.NopCloser(&expireOnRead{Reader: strings.NewReader(`{}`), expire: func() { now = now.Add(operationTTL + time.Second) }})
	w := httptest.NewRecorder()
	o.handle(w, r, handler)
	if w.Code != 410 || calls != 1 {
		t.Fatal("expired receipt reexecuted", w.Code, calls)
	}
}

func TestOperationMalformedResponseIsNotReexecuted(t *testing.T) {
	o := newOperationRegistry()
	id := issued(t, o)
	calls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; _, _ = w.Write([]byte("not JSON")) })
	for i := 0; i < 2; i++ {
		if w := keyed(o, id, `{}`, handler); w.Code != 409 {
			t.Fatal(w.Code)
		}
	}
	if calls != 1 {
		t.Fatal("malformed response allowed another effect")
	}
}

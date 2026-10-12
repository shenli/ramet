// SPDX-License-Identifier: Apache-2.0
package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const operationTTL = time.Hour
const operationMaxBody = 8 << 20
const operationMaxBytes = 64 << 20
const operationMaxCount = 10000

// Receipts are intentionally scoped to one API session. Expired/session-mismatched
// IDs are rejected even after eviction, so forgetting a receipt cannot repeat an effect.
type operationReceipt struct {
	ID          string          `json:"id"`
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	State       string          `json:"state"`
	ExpiresAt   time.Time       `json:"expires_at"`
	Status      int             `json:"status,omitempty"`
	RequestID   string          `json:"request_id,omitempty"`
	RetryAfter  string          `json:"retry_after,omitempty"`
	Body        json.RawMessage `json:"body,omitempty"`
	fingerprint [32]byte
}
type operationRegistry struct {
	mu      sync.Mutex
	epoch   string
	entries map[string]*operationReceipt
	bytes   int
	now     func() time.Time
}

func randomHex() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func newOperationRegistry() *operationRegistry {
	return &operationRegistry{epoch: randomHex(), entries: map[string]*operationReceipt{}, now: time.Now}
}
func (o *operationRegistry) issue(w http.ResponseWriter, r *http.Request) {
	expires := o.now().Add(operationTTL).Truncate(time.Second)
	respond(w, 200, map[string]any{"id": o.epoch + "." + strconv.FormatInt(expires.Unix(), 10) + "." + randomHex(), "expires_at": expires})
}
func operationError(w http.ResponseWriter, status int, code, message string) {
	respond(w, status, map[string]string{"code": code, "error": message})
}
func (o *operationRegistry) valid(w http.ResponseWriter, id string) (time.Time, bool) {
	parts := strings.Split(id, ".")
	if len(parts) != 3 || len(parts[0]) != 32 || len(parts[2]) != 32 {
		operationError(w, 400, "invalid_operation_id", "use an ID from GET /v1/operations/new")
		return time.Time{}, false
	}
	if _, err := hex.DecodeString(parts[2]); err != nil {
		operationError(w, 400, "invalid_operation_id", "invalid operation ID")
		return time.Time{}, false
	}
	if parts[0] != o.epoch {
		operationError(w, 409, "operation_session_changed", "receipt belongs to another worker session; outcome is unknown, do not repeat with a new ID")
		return time.Time{}, false
	}
	seconds, err := strconv.ParseInt(parts[1], 10, 64)
	expires := time.Unix(seconds, 0)
	if err != nil || expires.After(o.now().Add(operationTTL)) {
		operationError(w, 400, "invalid_operation_id", "invalid receipt expiration")
		return time.Time{}, false
	}
	if !o.now().Before(expires) {
		operationError(w, 410, "operation_expired", "receipt expired; outcome is unknown, do not repeat with a new ID")
		return time.Time{}, false
	}
	return expires, true
}
func (o *operationRegistry) lookup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := o.valid(w, id); !ok {
		return
	}
	o.mu.Lock()
	receipt := o.entries[id]
	if receipt == nil {
		o.mu.Unlock()
		operationError(w, 404, "operation_not_found", "operation has not been accepted in this session; retry only the same request and ID")
		return
	}
	copy := *receipt
	o.mu.Unlock()
	respond(w, 200, &copy)
}
func receiptReplay(w http.ResponseWriter, receipt *operationReceipt) {
	w.Header().Set("X-Operation-ID", receipt.ID)
	w.Header().Set("X-Operation-Request-ID", receipt.RequestID)
	if receipt.State == "pending" {
		operationError(w, 409, "operation_pending", "operation is still running; inspect its receipt before retrying")
		return
	}
	if receipt.State != "completed" {
		operationError(w, 409, "operation_result_unavailable", "operation may have completed; result exceeded receipt limit, was malformed, or handler failed; do not repeat with a new ID")
		return
	}
	if receipt.RetryAfter != "" {
		w.Header().Set("Retry-After", receipt.RetryAfter)
	}
	if receipt.Status != 204 {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(receipt.Status)
	_, _ = w.Write(receipt.Body)
}

type receiptWriter struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	overflow bool
}

func (w *receiptWriter) Header() http.Header { return w.header }
func (w *receiptWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *receiptWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if w.body.Len()+len(b) > operationMaxBody {
		w.overflow = true
	} else if !w.overflow {
		_, _ = w.body.Write(b)
	}
	return len(b), nil
}
func (o *operationRegistry) handle(w http.ResponseWriter, r *http.Request, next http.Handler) {
	id := r.Header.Get("Idempotency-Key")
	if id == "" {
		next.ServeHTTP(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/") || (r.Method != "POST" && r.Method != "DELETE") {
		operationError(w, 400, "invalid_operation", "operation IDs require a mutation")
		return
	}
	expires, ok := o.valid(w, id)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil {
		operationError(w, 400, "invalid_request", "cannot read request body")
		return
	}
	if len(body) > 1<<20 {
		operationError(w, 413, "body_too_large", "request body exceeds 1 MiB")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	fingerprint := sha256.Sum256(append([]byte(r.Method+"\n"+r.URL.RequestURI()+"\n"), body...))
	o.mu.Lock()
	if !o.now().Before(expires) {
		o.mu.Unlock()
		operationError(w, 410, "operation_expired", "receipt expired while reading the request; do not repeat with a new ID")
		return
	}
	for key, receipt := range o.entries {
		if !o.now().Before(receipt.ExpiresAt) && receipt.State != "pending" {
			o.bytes -= len(receipt.Body)
			delete(o.entries, key)
		}
	}
	if receipt := o.entries[id]; receipt != nil {
		if receipt.fingerprint != fingerprint {
			o.mu.Unlock()
			operationError(w, 409, "operation_conflict", "operation ID is bound to a different method, path, or request body")
			return
		}
		copy := *receipt
		o.mu.Unlock()
		receiptReplay(w, &copy)
		return
	}
	// Reserve the maximum response size before allowing a side effect.
	if len(o.entries) >= operationMaxCount || o.bytes+operationMaxBody > operationMaxBytes {
		o.mu.Unlock()
		operationError(w, 429, "operation_capacity", "receipt capacity reached; no operation accepted; wait for receipts to expire")
		return
	}
	receipt := &operationReceipt{ID: id, Method: r.Method, Path: r.URL.RequestURI(), State: "pending", ExpiresAt: expires, RequestID: w.Header().Get("X-Request-ID"), fingerprint: fingerprint}
	o.entries[id] = receipt
	o.bytes += operationMaxBody
	o.mu.Unlock()
	captured := &receiptWriter{header: make(http.Header)}
	captured.header.Set("X-Request-ID", receipt.RequestID)
	// Record terminal uncertainty on panic; never make this ID executable again.
	func() {
		defer func() {
			if v := recover(); v != nil {
				o.mu.Lock()
				receipt.State = "unknown"
				o.bytes -= operationMaxBody
				o.mu.Unlock()
				panic(v)
			}
		}()
		next.ServeHTTP(captured, r)
	}()
	o.mu.Lock()
	receipt.State = "completed"
	receipt.Status = captured.status
	if receipt.Status == 0 {
		receipt.Status = 200
	}
	receipt.RetryAfter = captured.header.Get("Retry-After")
	o.bytes -= operationMaxBody
	if captured.overflow || (receipt.Status != 204 && !json.Valid(captured.body.Bytes())) {
		receipt.State = "unknown"
	} else {
		receipt.Body = append([]byte(nil), captured.body.Bytes()...)
		o.bytes += len(receipt.Body)
	}
	copy := *receipt
	o.mu.Unlock()
	for key, values := range captured.header {
		w.Header()[key] = values
	}
	receiptReplay(w, &copy)
}

func (a *API) capabilities(w http.ResponseWriter, r *http.Request) {
	respond(w, 200, map[string]any{
		"api_version": "v1", "server_version": a.opts.Version,
		"platform": runtime.GOOS, "backend": a.store.Backend(),
		"operation_receipts": map[string]any{
			"scope": "worker_session", "ttl_seconds": int(operationTTL.Seconds()),
			"max_response_bytes": operationMaxBody, "max_total_bytes": operationMaxBytes,
			"max_count": operationMaxCount, "issue": "/v1/operations/new", "lookup": "/v1/operations/{id}",
		},
		"policy_schema": "https://raw.githubusercontent.com/shenli/ramet/main/schemas/policy-v1.schema.json",
		"persistence":   "explicit_checkpoint_or_promotion",
	})
}

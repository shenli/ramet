// SPDX-License-Identifier: Apache-2.0
// Package client provides the Go HTTP SDK for a Ramet worker.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shenli/ramet/engine"
)

type Client struct {
	URL, Token string
	HTTP       *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{URL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 125 * time.Second}}
}

type Error struct {
	Status      int
	RequestID   string `json:"request_id,omitempty"`
	RetryAfter  string `json:"-"`
	OperationID string `json:"-"`
	Code        string `json:"code"`
	Message     string `json:"error"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("ramet HTTP %d (%s): %s", e.Status, e.Code, e.Message)
}

// Do sends one request. Mutations are never retried automatically: a lost
// response may have committed, and an acquire must not leak a second lease.
func (c *Client) Do(ctx context.Context, method, path string, body, result any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, reader)
	if err != nil {
		return err
	}
	if id, _ := ctx.Value(operationKey{}).(string); id != "" {
		req.Header.Set("Idempotency-Key", id)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 125 * time.Second}
	}
	resp, err := h.Do(req)
	if err != nil {
		return unknownOutcome(req, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		e := &Error{Status: resp.StatusCode, RequestID: resp.Header.Get("X-Request-ID"), RetryAfter: resp.Header.Get("Retry-After"), OperationID: req.Header.Get("Idempotency-Key")}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(e); err != nil {
			e.Message = resp.Status
		}
		return e
	}
	if result == nil || resp.StatusCode == 204 {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return unknownOutcome(req, err)
	}
	return nil
}

func (c *Client) Snapshot(ctx context.Context, opts engine.SnapshotOptions) (engine.Snapshot, error) {
	var s engine.Snapshot
	err := c.Do(ctx, "POST", "/v1/snapshots", opts, &s)
	return s, err
}
func (c *Client) Snapshots(ctx context.Context) ([]engine.Snapshot, error) {
	var s []engine.Snapshot
	err := c.Do(ctx, "GET", "/v1/snapshots", nil, &s)
	return s, err
}

type Pool struct {
	ID          string `json:"id"`
	Base        string `json:"base"`
	Concurrency int    `json:"concurrency"`
	client      *Client
}

func (c *Client) CreatePool(ctx context.Context, opts engine.PoolOptions) (*Pool, error) {
	p := &Pool{client: c}
	err := c.Do(ctx, "POST", "/v1/pools", opts, p)
	return p, err
}
func (c *Client) Pool(id string) *Pool { return &Pool{ID: id, client: c} }
func (p *Pool) path() string           { return "/v1/pools/" + url.PathEscape(p.ID) }
func (p *Pool) Acquire(ctx context.Context) (engine.Execution, error) {
	var e engine.Execution
	err := p.client.Do(ctx, "POST", p.path()+"/acquire", nil, &e)
	return e, err
}

func (p *Pool) AcquireMany(ctx context.Context, count int) ([]engine.Execution, error) {
	var out []engine.Execution
	err := p.client.Do(ctx, "POST", p.path()+"/acquire-many", map[string]int{"count": count}, &out)
	return out, err
}
func (p *Pool) Heartbeat(ctx context.Context, e engine.Execution) (engine.Execution, error) {
	var out engine.Execution
	err := p.action(ctx, e, "heartbeat", &out)
	return out, err
}
func (p *Pool) action(ctx context.Context, e engine.Execution, action string, out any) error {
	return p.client.Do(ctx, "POST", p.path()+"/executions/"+url.PathEscape(e.ID)+"/"+action, struct {
		Lease      string `json:"lease"`
		Generation uint64 `json:"generation"`
	}{e.Lease, e.Generation}, out)
}
func (p *Pool) Reset(ctx context.Context, e engine.Execution) (engine.Execution, error) {
	var out engine.Execution
	err := p.action(ctx, e, "reset", &out)
	return out, err
}
func (p *Pool) Fork(ctx context.Context, e engine.Execution) (engine.Execution, error) {
	var out engine.Execution
	err := p.action(ctx, e, "fork", &out)
	return out, err
}
func (p *Pool) Release(ctx context.Context, e engine.Execution) error {
	return p.action(ctx, e, "release", nil)
}
func (p *Pool) Discard(ctx context.Context, e engine.Execution) error {
	return p.action(ctx, e, "discard", nil)
}
func (p *Pool) Verify(ctx context.Context, e engine.Execution) (engine.Verification, error) {
	var out engine.Verification
	err := p.action(ctx, e, "verify", &out)
	return out, err
}
func (p *Pool) Promote(ctx context.Context, e engine.Execution) (engine.Snapshot, error) {
	var out engine.Snapshot
	err := p.action(ctx, e, "promote", &out)
	return out, err
}
func (p *Pool) Checkpoint(ctx context.Context, e engine.Execution) (engine.Snapshot, error) {
	var out engine.Snapshot
	err := p.action(ctx, e, "checkpoint", &out)
	return out, err
}
func (p *Pool) Close(ctx context.Context) error {
	return p.client.Do(ctx, "DELETE", p.path(), nil, nil)
}

// Locality reports this worker's resident content and idle executions for a base.
func (c *Client) Locality(ctx context.Context, id string) (engine.Locality, error) {
	var result engine.Locality
	err := c.Do(ctx, "GET", "/v1/snapshots/"+url.PathEscape(id)+"/locality", nil, &result)
	return result, err
}
func (c *Client) Prefetch(ctx context.Context, id string) (engine.Locality, error) {
	var result engine.Locality
	err := c.Do(ctx, "POST", "/v1/snapshots/"+url.PathEscape(id)+"/prefetch", nil, &result)
	return result, err
}
func (p *Pool) Warm(ctx context.Context, count int) (engine.WarmResult, error) {
	var result engine.WarmResult
	err := p.client.Do(ctx, "POST", p.path()+"/warm", map[string]int{"count": count}, &result)
	return result, err
}

func (c *Client) CacheStatus(ctx context.Context) (engine.CacheStatus, error) {
	var result engine.CacheStatus
	err := c.Do(ctx, "GET", "/v1/worker/cache", nil, &result)
	return result, err
}
func (c *Client) TrimCache(ctx context.Context, targetBytes int64) (engine.CacheStatus, error) {
	var result engine.CacheStatus
	err := c.Do(ctx, "POST", "/v1/worker/cache/trim", map[string]int64{"target_bytes": targetBytes}, &result)
	return result, err
}
func (c *Client) TrimIdle(ctx context.Context) (int, error) {
	var result struct {
		Removed int `json:"removed"`
	}
	err := c.Do(ctx, "POST", "/v1/worker/idle/trim", nil, &result)
	return result.Removed, err
}

// PreviewPromotion inspects policy decisions without publishing a snapshot.
func (p *Pool) PreviewPromotion(ctx context.Context, e engine.Execution) (engine.PromotionPreview, error) {
	var out engine.PromotionPreview
	err := p.action(ctx, e, "preview-promotion", &out)
	return out, err
}
func (p *Pool) RestorePlan(ctx context.Context, e engine.Execution) (engine.RestorePlan, error) {
	var out engine.RestorePlan
	err := p.action(ctx, e, "restore-plan", &out)
	return out, err
}
func (c *Client) RestorePlan(ctx context.Context, id string) (engine.RestorePlan, error) {
	var out engine.RestorePlan
	err := c.Do(ctx, "GET", "/v1/snapshots/"+url.PathEscape(id)+"/restore-plan", nil, &out)
	return out, err
}

// SPDX-License-Identifier: Apache-2.0
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type operationKey struct{}

// WithOperationID binds a mutation to an ID from NewOperation. Reuse only with
// the identical request to recover its original response; never use a new ID
// merely because a previous mutation timed out.
func WithOperationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, operationKey{}, id)
}

type OutcomeUnknownError struct {
	OperationID, Method, Path string
	Err                       error
}

func (e *OutcomeUnknownError) Error() string {
	return fmt.Sprintf("operation %s outcome unknown: %v; inspect its receipt or retry the identical request with the same ID", e.OperationID, e.Err)
}
func (e *OutcomeUnknownError) Unwrap() error { return e.Err }
func unknownOutcome(req *http.Request, err error) error {
	if id := req.Header.Get("Idempotency-Key"); id != "" && (req.Method == "POST" || req.Method == "DELETE") {
		return &OutcomeUnknownError{OperationID: id, Method: req.Method, Path: req.URL.RequestURI(), Err: err}
	}
	return err
}

type Operation struct {
	ID         string          `json:"id"`
	Method     string          `json:"method"`
	Path       string          `json:"path"`
	State      string          `json:"state"`
	ExpiresAt  time.Time       `json:"expires_at"`
	Status     int             `json:"status,omitempty"`
	RequestID  string          `json:"request_id,omitempty"`
	RetryAfter string          `json:"retry_after,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
}

func (c *Client) NewOperation(ctx context.Context) (string, error) {
	var issued struct {
		ID string `json:"id"`
	}
	err := c.Do(ctx, "GET", "/v1/operations/new", nil, &issued)
	return issued.ID, err
}
func (c *Client) Operation(ctx context.Context, id string) (Operation, error) {
	var result Operation
	err := c.Do(ctx, "GET", "/v1/operations/"+url.PathEscape(id), nil, &result)
	return result, err
}
func (c *Client) Capabilities(ctx context.Context) (map[string]any, error) {
	var result map[string]any
	err := c.Do(ctx, "GET", "/v1/capabilities", nil, &result)
	return result, err
}

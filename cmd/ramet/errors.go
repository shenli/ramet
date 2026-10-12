// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/shenli/ramet/client"
)

func errorMode(args []string) ([]string, bool) {
	out := make([]string, 0, len(args))
	structured := false
	for _, arg := range args {
		if arg == "--json-errors" {
			structured = true
		} else {
			out = append(out, arg)
		}
	}
	return out, structured
}
func emitError(w io.Writer, err error, structured bool) {
	if !structured {
		fmt.Fprintln(w, "ramet:", err)
		return
	}
	fields := map[string]any{"code": "invalid_argument", "error": err.Error()}
	var remote *client.Error
	var unknown *client.OutcomeUnknownError
	var network net.Error
	switch {
	case errors.As(err, &unknown):
		fields["code"] = "outcome_unknown"
		fields["operation_id"] = unknown.OperationID
	case errors.As(err, &remote):
		fields["code"] = remote.Code
		fields["status"] = remote.Status
		fields["request_id"] = remote.RequestID
		if remote.RetryAfter != "" {
			fields["retry_after"] = remote.RetryAfter
		}
		if remote.OperationID != "" {
			fields["operation_id"] = remote.OperationID
		}
	case errors.As(err, &network):
		fields["code"] = "transport_error"
	}
	_ = printJSON(w, fields)
}

type jsonErrorKey struct{}

func flagOutput(ctx context.Context, w io.Writer) io.Writer {
	if enabled, _ := ctx.Value(jsonErrorKey{}).(bool); enabled {
		return io.Discard
	}
	return w
}

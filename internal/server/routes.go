// SPDX-License-Identifier: Apache-2.0
package server

import (
	"net/http"
	"strings"
)

func structuredRoutes(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler, pattern := mux.Handler(r)
		if pattern == "" && (r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/")) {
			response := &receiptWriter{header: make(http.Header)}
			handler.ServeHTTP(response, r)
			for key, values := range response.header {
				if key != "Content-Type" && key != "Content-Length" {
					w.Header()[key] = values
				}
			}
			if response.status == 405 {
				operationError(w, 405, "method_not_allowed", "method is not supported for this endpoint")
			} else {
				operationError(w, 404, "endpoint_not_found", "unknown API endpoint; see GET /v1/capabilities")
			}
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func ensureRequestID(w http.ResponseWriter) string {
	id := w.Header().Get("X-Request-ID")
	if id == "" {
		id = randomHex()
		w.Header().Set("X-Request-ID", id)
	}
	return id
}

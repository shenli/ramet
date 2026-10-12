// SPDX-License-Identifier: Apache-2.0
package server

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
)

// NewGateway routes credentials to independently owned worker stores. Tenant
// tokens cannot select another namespace through headers, paths, or body fields.
// The caller owns the workers and must drain/close every one during shutdown.
func NewGateway(tenants map[string]*API) (http.Handler, error) {
	if len(tenants) == 0 {
		return nil, fmt.Errorf("at least one tenant is required")
	}
	routes := map[[32]byte]*API{}
	roots := []string{}
	for id, api := range tenants {
		if id == "" || api == nil || api.opts.Token == "" {
			return nil, fmt.Errorf("tenant ID, worker and token are required")
		}
		if !api.store.RestrictsSources() {
			return nil, fmt.Errorf("tenant workers require source allowlists")
		}
		hash := sha256.Sum256([]byte(api.opts.Token))
		if routes[hash] != nil {
			return nil, fmt.Errorf("tenant credentials must be unique")
		}
		root := api.store.Root()
		for _, other := range roots {
			rel, _ := filepath.Rel(root, other)
			back, _ := filepath.Rel(other, root)
			inside := func(p string) bool {
				return p == "." || p != ".." && !strings.HasPrefix(p, ".."+string(filepath.Separator))
			}
			if inside(rel) || inside(back) {
				return nil, fmt.Errorf("tenant data directories must not overlap")
			}
		}
		roots = append(roots, root)
		api.opts.Tenant = id
		routes[hash] = api
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ensureRequestID(w)
		if r.URL.Path == "/live" {
			respond(w, 200, map[string]string{"status": "live"})
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if api := routes[sha256.Sum256([]byte(token))]; ok && api != nil {
			api.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		respond(w, 401, map[string]string{"code": "unauthorized", "error": "valid tenant bearer token required"})
	}), nil
}

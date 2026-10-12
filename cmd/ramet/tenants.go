// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/shenli/ramet/engine"
	"github.com/shenli/ramet/internal/config"
	"github.com/shenli/ramet/internal/server"
)

func openTenants(ctx context.Context, filename string, cfg config.Config, logger *slog.Logger) (http.Handler, []*engine.Store, error) {
	apis := map[string]*server.API{}
	stores := []*engine.Store{}
	configs := []config.Config{}
	identities, namespaces := map[string]bool{}, map[string]bool{}
	ok := false
	defer func() {
		if !ok {
			for _, s := range stores {
				_ = s.Close()
			}
		}
	}()
	for _, tenant := range cfg.Tenants {
		if tenant.ID == "" || apis[tenant.ID] != nil || tenant.Config == "" {
			return nil, nil, errors.New("unique tenant ID and config are required")
		}
		name := tenant.Config
		if !filepath.IsAbs(name) {
			name = filepath.Join(filepath.Dir(filename), name)
		}
		c, err := config.Load(name)
		if err != nil {
			return nil, nil, err
		}
		if len(c.Tenants) != 0 {
			return nil, nil, errors.New("nested tenant gateways are not supported")
		}
		token := os.Getenv(c.Server.TokenEnv)
		if token == "" || c.Worker.Data == "" {
			return nil, nil, errors.New("each tenant requires server.token_env and worker.data")
		}
		opts, err := c.Options(ctx)
		if err != nil {
			return nil, nil, err
		}
		if opts.Content != nil {
			identity := opts.Content.Identity()
			if identities[identity] || namespaces[c.Metadata.Namespace] {
				opts.Metadata.Close()
				return nil, nil, errors.New("tenant content prefixes and metadata namespaces must be unique")
			}
			identities[identity] = true
			namespaces[c.Metadata.Namespace] = true
		}
		opts.Logger = logger.With("tenant", tenant.ID)
		opts.RestrictSources = true
		s, err := engine.OpenContext(ctx, c.Worker.Data, opts)
		if err != nil {
			return nil, nil, err
		}
		stores = append(stores, s)
		configs = append(configs, c)
		apis[tenant.ID] = server.New(s, server.Options{Version: version, Token: token, Tenant: tenant.ID, Logger: logger, MaxInflight: c.Server.MaxInflight})
	}
	// Source allowlists must not grant one tenant access to another worker's
	// data, even when a broad parent directory was configured accidentally.
	for _, c := range configs {
		for _, source := range c.Worker.SourceRoots {
			root, err := filepath.EvalSymlinks(source)
			if err != nil {
				return nil, nil, err
			}
			root, err = filepath.Abs(root)
			if err != nil {
				return nil, nil, err
			}
			for _, s := range stores {
				rel, _ := filepath.Rel(root, s.Root())
				back, _ := filepath.Rel(s.Root(), root)
				inside := func(p string) bool {
					return p == "." || p != ".." && !strings.HasPrefix(p, ".."+string(filepath.Separator))
				}
				if inside(rel) || inside(back) {
					return nil, nil, fmt.Errorf("source allowlist overlaps tenant storage")
				}
			}
		}
	}
	handler, err := server.NewGateway(apis)
	if err != nil {
		return nil, nil, err
	}
	ok = true
	return handler, stores, nil
}

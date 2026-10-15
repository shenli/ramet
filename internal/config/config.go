// SPDX-License-Identifier: Apache-2.0
package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/shenli/ramet/engine"
	"github.com/shenli/ramet/storage/postgres"
	"github.com/shenli/ramet/storage/s3"
	"go.yaml.in/yaml/v3"
)

type Config struct {
	Tenants []Tenant `yaml:"tenants"`
	Server  struct {
		MaxInflight int    `yaml:"max_inflight"`
		Listen      string `yaml:"listen"`
		TokenEnv    string `yaml:"token_env"`
	} `yaml:"server"`
	Worker struct {
		Backend           string        `yaml:"backend"`
		CacheBytes        int64         `yaml:"cache_bytes"`
		MinFreeDiskBytes  int64         `yaml:"min_free_disk_bytes"`
		MaxIdleExecutions int           `yaml:"max_idle_executions"`
		IdleTTL           time.Duration `yaml:"idle_ttl"`
		RestrictSources   bool          `yaml:"restrict_sources"`
		SourceRoots       []string      `yaml:"source_roots"`
		GitRepositories   []string      `yaml:"git_repositories"`
		Data              string        `yaml:"data"`
		RequireCOW        bool          `yaml:"require_cow"`
		DisableChunking   bool          `yaml:"disable_chunking"`
		MaxExecutions     int           `yaml:"max_executions"`
		MaxPools          int           `yaml:"max_pools"`
	} `yaml:"worker"`
	Metadata struct {
		Provider  string `yaml:"provider"`
		DSNEnv    string `yaml:"dsn_env"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Content struct {
		Provider        string `yaml:"provider"`
		Endpoint        string `yaml:"endpoint"`
		Region          string `yaml:"region"`
		Bucket          string `yaml:"bucket"`
		Prefix          string `yaml:"prefix"`
		AccessKeyEnv    string `yaml:"access_key_env"`
		SecretKeyEnv    string `yaml:"secret_key_env"`
		SessionTokenEnv string `yaml:"session_token_env"`
	} `yaml:"content"`
}

func Load(name string) (Config, error) {
	var c Config
	f, err := os.Open(name)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := yaml.NewDecoder(io.LimitReader(f, 1<<20))
	d.KnownFields(true)
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, errors.New("configuration must contain one YAML document")
	}
	if c.Metadata.Provider != "" && c.Metadata.Provider != "local" && c.Metadata.Provider != "postgres" {
		return c, errors.New("metadata.provider must be local or postgres")
	}
	if c.Content.Provider != "" && c.Content.Provider != "local" && c.Content.Provider != "s3" {
		return c, errors.New("content.provider must be local or s3")
	}
	if (c.Metadata.Provider == "postgres") != (c.Content.Provider == "s3") {
		return c, errors.New("postgres metadata requires s3 content, and vice versa")
	}
	return c, nil
}

func (c Config) Options(ctx context.Context) (engine.Options, error) {
	opts := engine.Options{Backend: c.Worker.Backend, CacheBytes: c.Worker.CacheBytes, MinFreeDiskBytes: c.Worker.MinFreeDiskBytes, MaxIdleExecutions: c.Worker.MaxIdleExecutions, IdleTTL: c.Worker.IdleTTL, RestrictSources: c.Worker.RestrictSources, SourceRoots: c.Worker.SourceRoots, GitRepositories: c.Worker.GitRepositories, RequireCOW: c.Worker.RequireCOW, MaxExecutions: c.Worker.MaxExecutions, MaxPools: c.Worker.MaxPools}
	opts.DisableChunking = c.Worker.DisableChunking
	if c.Metadata.Provider != "postgres" {
		return opts, nil
	}
	if c.Metadata.DSNEnv == "" || os.Getenv(c.Metadata.DSNEnv) == "" {
		return opts, fmt.Errorf("metadata.dsn_env must name a populated environment variable")
	}
	var access, secret string
	if c.Content.AccessKeyEnv != "" {
		access = os.Getenv(c.Content.AccessKeyEnv)
		if access == "" {
			return opts, errors.New("content access-key environment variable is empty")
		}
	}
	if c.Content.SecretKeyEnv != "" {
		secret = os.Getenv(c.Content.SecretKeyEnv)
		if secret == "" {
			return opts, errors.New("content secret-key environment variable is empty")
		}
	}
	content, err := s3.New(s3.Options{Endpoint: c.Content.Endpoint, Region: c.Content.Region, Bucket: c.Content.Bucket, Prefix: c.Content.Prefix, AccessKey: access, SecretKey: secret, SessionToken: os.Getenv(c.Content.SessionTokenEnv)})
	if err != nil {
		return opts, err
	}
	metadata, err := postgres.Open(ctx, os.Getenv(c.Metadata.DSNEnv), c.Metadata.Namespace)
	if err != nil {
		return opts, err
	}
	opts.Metadata, opts.Content = metadata, content
	return opts, nil
}

// Tenant selects a dedicated worker configuration; the gateway never accepts a
// tenant identity from request headers or paths.
type Tenant struct {
	ID     string `yaml:"id"`
	Config string `yaml:"config"`
}

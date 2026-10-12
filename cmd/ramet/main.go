// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/shenli/ramet/client"
	"github.com/shenli/ramet/engine"
	"github.com/shenli/ramet/internal/benchmark"
	"github.com/shenli/ramet/internal/config"
	"github.com/shenli/ramet/internal/server"
	"github.com/shenli/ramet/storage/postgres"
)

var version = "0.3.0-alpha.1"

const usage = `Ramet — persistent, forkable workspaces for AI agents

Usage: ramet <command> [flags]

  dev, serve     Start the single-node worker and HTTP API
  demo           Run a self-contained verified parallel execution demo
  bench          Benchmark a local directory (--source required)
  capabilities   Inspect API/backend and receipt limits
  operation-id   Issue an ID for one recoverable mutation
  operation      Inspect a mutation receipt (--id)
  policy-validate Validate a policy JSON file locally (--policy)
  snapshot       Capture a directory or Git revision
  snapshots      List durable snapshots
  pool           Create an execution pool
  warm           Prepare verified idle trees (--pool, --count)
  prefetch       Hydrate snapshot content (--id)
  locality       Report cached snapshot content (--id)
  cache          Report cache capacity, pressure and residency
  trim-cache     Evict unpinned remote cache (--target-bytes, default 0)
  trim-idle      Discard all idle execution trees
  acquire        Acquire an execution from --pool
  reset          Reset an execution described by --execution <JSON file>
  fork           Fork an execution's current state
  verify         Compare an execution with its base
  checkpoint     Persist an execution's complete supported state
  preview-promotion Preview policy decisions without saving an execution
  restore-plan   Show reconstruction requirements for a snapshot (--id)
  promote        Persist state according to its snapshot policy
  release        Verify/reset an execution and recycle its slot
  discard        Delete an execution
  close-pool     Close an empty pool
  delete-snapshot Delete an unreferenced snapshot
  health         Check worker readiness
  doctor         Check store integrity and filesystem capabilities
  gc             Collect unreferenced content (dry run unless --run)
  drain          Stop accepting new executions
  heartbeat      Renew an execution lease
  migrate        Apply supported database migrations (--config required)
  version        Print version

Use ramet <command> --help for flags. Add --json-errors for JSON errors on stderr.
Client configuration: RAMET_URL (default http://127.0.0.1:7433), RAMET_TOKEN.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	args, jsonErrors := errorMode(os.Args[1:])
	if jsonErrors {
		ctx = context.WithValue(ctx, jsonErrorKey{}, true)
	}
	if err := run(ctx, args, os.Stdout, os.Stderr); err != nil {
		emitError(os.Stderr, err, jsonErrors)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		_, err := fmt.Fprint(out, usage)
		return err
	}
	if args[0] == "version" {
		_, err := fmt.Fprintln(out, version)
		return err
	}
	switch args[0] {
	case "dev", "serve":
		return serve(ctx, args[1:], errOut)
	case "migrate":
		return migrate(ctx, args[1:], out, errOut)
	case "demo", "bench":
		return bench(ctx, args[0], args[1:], out, errOut)
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(flagOutput(ctx, errOut))
	baseURL := fs.String("url", env("RAMET_URL", "http://127.0.0.1:7433"), "Ramet worker URL")
	operationID := fs.String("operation-id", "", "ID from operation-id; reuse only for the identical mutation")
	var source, repository, revision, policyFile, base, poolID, executionFile, id *string
	var concurrency, count, leaseSeconds *int
	var gcRun *bool
	var grace *time.Duration
	var targetBytes *int64
	switch args[0] {
	case "policy-validate":
		policyFile = fs.String("policy", "", "JSON policy file (required)")
	case "snapshot":
		source = fs.String("source", "", "absolute directory on the worker")
		repository = fs.String("git", "", "Git repository URL or worker-local path")
		revision = fs.String("revision", "", "Git revision (required with --git)")
		policyFile = fs.String("policy", "", "JSON persistence policy file")
	case "pool":
		base = fs.String("base", "", "snapshot digest")
		concurrency = fs.Int("concurrency", 8, "maximum active executions")
		leaseSeconds = fs.Int("lease-seconds", 900, "lease lifetime; renew with heartbeat")
	case "acquire", "close-pool", "warm":
		poolID = fs.String("pool", "", "pool ID")
		if args[0] == "acquire" || args[0] == "warm" {
			count = fs.Int("count", 1, "number of executions to reserve together")
		}
	case "reset", "fork", "verify", "checkpoint", "promote", "preview-promotion", "release", "discard", "heartbeat":
		executionFile = fs.String("execution", "", "JSON execution file, or - for stdin")
	case "delete-snapshot", "prefetch", "locality", "restore-plan", "operation":
		id = fs.String("id", "", "snapshot digest")
	case "gc":
		gcRun = fs.Bool("run", false, "delete eligible objects (default dry run)")
		grace = fs.Duration("grace", time.Hour, "minimum orphan object age")
	case "trim-cache":
		targetBytes = fs.Int64("target-bytes", 0, "target remote cache bytes")
	case "snapshots", "doctor", "drain", "health", "cache", "trim-idle", "capabilities", "operation-id":
	default:
		return fmt.Errorf("unknown command %q; use ramet --help", args[0])
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *operationID != "" {
		ctx = client.WithOperationID(ctx, *operationID)
	}
	if args[0] == "snapshot" {
		if (*source == "") == (*repository == "") {
			return fmt.Errorf("exactly one of --source or --git is required; example: ramet snapshot --source /absolute/project")
		}
		if *repository != "" && *revision == "" {
			return fmt.Errorf("--revision is required with --git")
		}
		if *source != "" && *revision != "" {
			return fmt.Errorf("--revision requires --git")
		}
	}
	if args[0] == "pool" && *base == "" {
		return fmt.Errorf("--base is required; example: ramet pool --base <snapshot-id>")
	}
	c := client.New(*baseURL, os.Getenv("RAMET_TOKEN"))
	var result any
	var err error
	switch args[0] {
	case "policy-validate":
		if *policyFile == "" {
			return fmt.Errorf("--policy is required")
		}
		f, e := os.Open(*policyFile)
		if e != nil {
			return e
		}
		defer f.Close()
		policy, e := engine.DecodePolicy(f)
		if e != nil {
			return e
		}
		result = map[string]any{"valid": true, "policy": policy}
	case "capabilities":
		result, err = c.Capabilities(ctx)
	case "operation-id":
		var op string
		op, err = c.NewOperation(ctx)
		result = map[string]string{"id": op}
	case "snapshot":
		opts := engine.SnapshotOptions{Source: *source}
		if *repository != "" {
			opts.Git = &engine.GitBase{Repository: *repository, Revision: *revision}
		}
		if *policyFile != "" {
			f, e := os.Open(*policyFile)
			if e != nil {
				return e
			}
			defer f.Close()
			if opts.Policy, e = engine.DecodePolicy(f); e != nil {
				return e
			}
		}
		result, err = c.Snapshot(ctx, opts)
	case "cache":
		result, err = c.CacheStatus(ctx)
	case "trim-cache":
		result, err = c.TrimCache(ctx, *targetBytes)
	case "trim-idle":
		result, err = c.TrimIdle(ctx)
	case "snapshots":
		result, err = c.Snapshots(ctx)
	case "pool":
		result, err = c.CreatePool(ctx, engine.PoolOptions{Base: *base, Concurrency: *concurrency, LeaseSeconds: *leaseSeconds})
	case "warm":
		if *poolID == "" {
			return fmt.Errorf("--pool is required")
		}
		result, err = c.Pool(*poolID).Warm(ctx, *count)
	case "prefetch", "locality", "restore-plan", "operation":
		if *id == "" {
			return fmt.Errorf("--id is required")
		}
		if args[0] == "operation" {
			result, err = c.Operation(ctx, *id)
		} else if args[0] == "prefetch" {
			result, err = c.Prefetch(ctx, *id)
		} else if args[0] == "restore-plan" {
			result, err = c.RestorePlan(ctx, *id)
		} else {
			result, err = c.Locality(ctx, *id)
		}
	case "acquire":
		if *poolID == "" {
			return fmt.Errorf("--pool is required")
		}
		if *count == 1 {
			result, err = c.Pool(*poolID).Acquire(ctx)
		} else {
			result, err = c.Pool(*poolID).AcquireMany(ctx, *count)
		}
	case "close-pool":
		if *poolID == "" {
			return fmt.Errorf("--pool is required")
		}
		err = c.Pool(*poolID).Close(ctx)
	case "health":
		var h map[string]string
		err = c.Do(ctx, "GET", "/ready", nil, &h)
		result = h
	case "doctor":
		var r engine.DoctorReport
		err = c.Do(ctx, "GET", "/v1/doctor", nil, &r)
		result = r
		if err == nil && !r.OK {
			_ = printJSON(out, r)
			return fmt.Errorf("store integrity check failed")
		}
	case "drain":
		err = c.Do(ctx, "POST", "/v1/worker/drain", nil, nil)
	case "delete-snapshot":
		if *id == "" {
			return fmt.Errorf("--id is required")
		}
		err = c.Do(ctx, "DELETE", "/v1/snapshots/"+*id, nil, nil)
	case "gc":
		var r engine.GCResult
		err = c.Do(ctx, "POST", "/v1/gc", map[string]any{"grace_seconds": int64(grace.Seconds()), "dry_run": !*gcRun}, &r)
		result = r
	default:
		if *executionFile == "" {
			return fmt.Errorf("--execution is required")
		}
		var b []byte
		if *executionFile == "-" {
			b, err = io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
		} else {
			b, err = os.ReadFile(*executionFile)
		}
		if err != nil {
			return err
		}
		var e engine.Execution
		if err = json.Unmarshal(b, &e); err != nil {
			return err
		}
		if e.ID == "" || e.PoolID == "" || e.Lease == "" {
			return fmt.Errorf("execution JSON is missing identity or lease")
		}
		p := c.Pool(e.PoolID)
		switch args[0] {
		case "heartbeat":
			result, err = p.Heartbeat(ctx, e)
		case "reset":
			result, err = p.Reset(ctx, e)
		case "fork":
			result, err = p.Fork(ctx, e)
		case "verify":
			var v engine.Verification
			v, err = p.Verify(ctx, e)
			result = v
			if err == nil && !v.Valid {
				_ = printJSON(out, v)
				return fmt.Errorf("execution differs from its base")
			}
		case "checkpoint":
			result, err = p.Checkpoint(ctx, e)
		case "preview-promotion":
			var preview engine.PromotionPreview
			preview, err = p.PreviewPromotion(ctx, e)
			result = preview
			if err == nil && !preview.Valid {
				_ = printJSON(out, preview)
				return fmt.Errorf("promotion preview has policy violations")
			}
		case "promote":
			result, err = p.Promote(ctx, e)
		case "release":
			err = p.Release(ctx, e)
		case "discard":
			err = p.Discard(ctx, e)
		}
	}
	if err != nil {
		return err
	}
	if result != nil {
		return printJSON(out, result)
	}
	return nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func printJSON(w io.Writer, v any) error {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(v)
}

func serve(ctx context.Context, args []string, errOut io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(flagOutput(ctx, errOut))
	data := fs.String("data", env("RAMET_DATA", ".ramet"), "private data directory")
	listen := fs.String("listen", env("RAMET_LISTEN", "127.0.0.1:7433"), "HTTP listen address")
	cow := fs.Bool("require-cow", false, "fail if copy-on-write is unavailable")
	configFile := fs.String("config", "", "YAML configuration file")
	maxExecutions := fs.Int("max-executions", 0, "worker-wide active execution limit (default 4096)")
	maxPools := fs.Int("max-pools", 0, "worker-wide pool limit (default 128)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected serve arguments")
	}
	token := os.Getenv("RAMET_TOKEN")
	var cfg config.Config
	if *configFile != "" {
		var err error
		cfg, err = config.Load(*configFile)
		if err != nil {
			return err
		}
		visited := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
		if !visited["data"] && os.Getenv("RAMET_DATA") == "" && cfg.Worker.Data != "" {
			*data = cfg.Worker.Data
		}
		if !visited["listen"] && os.Getenv("RAMET_LISTEN") == "" && cfg.Server.Listen != "" {
			*listen = cfg.Server.Listen
		}
		if !visited["require-cow"] {
			*cow = cfg.Worker.RequireCOW
		}
		if !visited["max-executions"] {
			*maxExecutions = cfg.Worker.MaxExecutions
		}
		if !visited["max-pools"] {
			*maxPools = cfg.Worker.MaxPools
		}
		if cfg.Server.TokenEnv != "" {
			token = os.Getenv(cfg.Server.TokenEnv)
			if token == "" {
				return fmt.Errorf("configured server token environment variable is empty")
			}
		}
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if token == "" && len(cfg.Tenants) == 0 && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("RAMET_TOKEN is required when listening beyond a numeric loopback address")
	}
	initCtx, initCancel := context.WithTimeout(ctx, 30*time.Second)
	defer initCancel()
	logger := slog.New(slog.NewJSONHandler(errOut, nil))
	var handler http.Handler
	var stores []*engine.Store
	if len(cfg.Tenants) > 0 {
		handler, stores, err = openTenants(initCtx, *configFile, cfg, logger)
		if err != nil {
			return err
		}
	} else {
		opts, err := cfg.Options(initCtx)
		if err != nil {
			return err
		}
		opts.Logger = logger
		opts.RequireCOW = *cow
		opts.MaxExecutions = *maxExecutions
		opts.MaxPools = *maxPools
		s, err := engine.OpenContext(initCtx, *data, opts)
		if err != nil {
			return err
		}
		stores = []*engine.Store{s}
		handler = server.New(s, server.Options{Version: version, Token: token, Logger: logger, MaxInflight: cfg.Server.MaxInflight})
	}
	defer func() {
		for _, s := range stores {
			if closeErr := s.Close(); closeErr != nil {
				logger.Error("worker close failed", "error", closeErr)
			}
		}
	}()
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	h := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 130 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	logger.Info("Ramet worker ready", "address", ln.Addr().String(), "workers", len(stores), "version", version, "authenticated", token != "" || len(cfg.Tenants) > 0)
	done := make(chan error, 1)
	go func() { done <- h.Serve(ln) }()
	select {
	case err = <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		for _, s := range stores {
			_ = s.Drain()
		}
		logger.Info("draining worker")
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err = h.Shutdown(shutdown); err != nil {
			_ = h.Close()
			return err
		}
		return nil
	}
}

func bench(ctx context.Context, command string, args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(flagOutput(ctx, errOut))
	source := fs.String("source", "", "directory to snapshot")
	concurrency := fs.Int("concurrency", 8, "parallel executions")
	iterations := fs.Int("iterations", 100, "total acquire/write/recycle cycles")
	cow := fs.Bool("require-cow", false, "fail if copy-on-write is unavailable")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected benchmark arguments")
	}
	root, err := os.MkdirTemp("", "ramet-bench-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	if command == "demo" && *source == "" {
		*source = filepath.Join(root, "source")
		if err = os.Mkdir(*source, 0755); err != nil {
			return err
		}
		for i := 0; i < 32; i++ {
			b := []byte(strings.Repeat(fmt.Sprintf("shared base file %02d\n", i), 4096))
			if err = os.WriteFile(filepath.Join(*source, fmt.Sprintf("file-%02d.txt", i)), b, 0644); err != nil {
				return err
			}
		}
	}
	if *source == "" {
		return fmt.Errorf("--source is required for bench")
	}
	s, err := engine.Open(filepath.Join(root, "store"), engine.Options{RequireCOW: *cow})
	if err != nil {
		return err
	}
	defer s.Close()
	r, err := benchmark.Run(ctx, s, *source, *concurrency, *iterations)
	if printErr := printJSON(out, r); printErr != nil {
		return printErr
	}
	return err
}

func migrate(ctx context.Context, args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(flagOutput(ctx, errOut))
	name := fs.String("config", "", "YAML configuration file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *name == "" || fs.NArg() != 0 {
		return fmt.Errorf("migrate requires --config and no positional arguments")
	}
	cfg, err := config.Load(*name)
	if err != nil {
		return err
	}
	if cfg.Metadata.Provider != "postgres" {
		return fmt.Errorf("migrate requires PostgreSQL metadata")
	}
	opts, err := cfg.Options(ctx)
	if err != nil {
		return err
	}
	defer opts.Metadata.Close()
	return printJSON(out, map[string]any{"schema_version": postgres.SchemaVersion, "status": "current"})
}

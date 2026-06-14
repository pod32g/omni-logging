// Command omnilog is the Omni-logging server and log forwarder.
//
// Usage:
//
//	omnilog serve   [flags]   # run the logging server (API + embedded UI)
//	omnilog forward [flags]   # tail files and ship them to a server
//	omnilog version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pod32g/omni-logging/internal/api"
	"github.com/pod32g/omni-logging/internal/config"
	"github.com/pod32g/omni-logging/internal/forward"
	"github.com/pod32g/omni-logging/internal/ingest"
	"github.com/pod32g/omni-logging/internal/store/sqlite"
	"github.com/pod32g/omni-logging/internal/tail"
	"github.com/pod32g/omni-logging/internal/web"
)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:], logger)
	case "forward":
		err = runForward(os.Args[2:], logger)
	case "version", "-v", "--version":
		fmt.Println("omnilog", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `omnilog — centralized logging server and forwarder

Commands:
  serve     Run the logging server (HTTP API + embedded web UI)
  forward   Tail one or more files and forward them to a server
  version   Print the version

Run "omnilog <command> -h" for command-specific flags.
`)
}

// runServe starts the server. Flags override config-file and env values.
func runServe(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var (
		configPath = fs.String("config", "", "path to a YAML config file")
		addr       = fs.String("addr", "", "listen address (default :8080)")
		db         = fs.String("db", "", "path to the SQLite database (default omni.db)")
		adminToken = fs.String("admin-token", "", "admin token required for query/UI (empty = open)")
		ingestKeys = fs.String("ingest-key", "", "comma-separated ingest API keys (empty = open)")
		retention  = fs.Int("retention-days", -1, "delete logs older than N days (0 = keep forever)")
		tlsCert    = fs.String("tls-cert", "", "TLS certificate file (enables HTTPS with -tls-key)")
		tlsKey     = fs.String("tls-key", "", "TLS key file")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	// Apply only the flags the user explicitly set, so they win over file/env.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["addr"] {
		cfg.Addr = *addr
	}
	if set["db"] {
		cfg.DBPath = *db
	}
	if set["admin-token"] {
		cfg.AdminToken = *adminToken
	}
	if set["ingest-key"] {
		cfg.IngestKeys = splitCSV(*ingestKeys)
	}
	if set["retention-days"] {
		cfg.RetentionDays = *retention
	}
	if set["tls-cert"] {
		cfg.TLSCert = *tlsCert
	}
	if set["tls-key"] {
		cfg.TLSKey = *tlsKey
	}

	store, err := sqlite.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	hub := tail.NewHub()
	ing := ingest.New(store, hub, ingest.Options{
		BufferSize:    cfg.BufferSize,
		BatchSize:     cfg.BatchSize,
		FlushInterval: time.Duration(cfg.FlushIntervalMS) * time.Millisecond,
		Logger:        logger,
	})
	ing.Start()
	defer ing.Stop()

	srv := api.New(api.Deps{
		Config:   cfg,
		Store:    store,
		Ingestor: ing,
		Hub:      hub,
		UI:       web.FS(),
		Logger:   logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.RetentionDays > 0 {
		go runRetention(ctx, store, cfg.RetentionDays, logger)
	}

	httpSrv := &http.Server{Addr: cfg.Addr, Handler: srv.Handler()}

	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	logger.Info("omnilog serving",
		"addr", cfg.Addr, "db", cfg.DBPath, "tls", cfg.TLSEnabled(),
		"auth", cfg.AdminToken != "", "retention_days", cfg.RetentionDays)

	if cfg.TLSEnabled() {
		err = httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	} else {
		err = httpSrv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// runRetention periodically purges logs older than retentionDays.
func runRetention(ctx context.Context, store *sqlite.DB, retentionDays int, logger *slog.Logger) {
	purge := func() {
		cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
		n, err := store.Purge(ctx, cutoff)
		if err != nil {
			logger.Error("retention purge failed", "error", err)
			return
		}
		if n > 0 {
			logger.Info("retention purge", "removed", n, "older_than", cutoff.Format(time.RFC3339))
		}
	}
	purge() // run once at startup
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			purge()
		}
	}
}

// runForward tails files and forwards them to a server.
func runForward(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("forward", flag.ExitOnError)
	var files multiFlag
	fs.Var(&files, "file", "file to tail (repeatable)")
	var (
		server    = fs.String("server", "http://localhost:8080", "server base URL")
		apiKey    = fs.String("api-key", "", "ingest API key")
		service   = fs.String("service", "", "service name to tag forwarded logs")
		source    = fs.String("source", "", "source/host (default: hostname)")
		fromStart = fs.Bool("from-start", false, "forward existing file contents before following")
		batch     = fs.Int("batch", 200, "max lines per request")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	fwd, err := forward.New(forward.Options{
		ServerURL: *server,
		APIKey:    *apiKey,
		Service:   *service,
		Source:    *source,
		Files:     files,
		FromStart: *fromStart,
		Batch:     *batch,
		Logger:    logger,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("omnilog forwarding", "server", *server, "files", []string(files), "service", *service)
	return fwd.Run(ctx)
}

// multiFlag collects a repeatable string flag into a slice.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

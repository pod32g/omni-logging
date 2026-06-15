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
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pod32g/omni-logging/internal/admission"
	"github.com/pod32g/omni-logging/internal/api"
	"github.com/pod32g/omni-logging/internal/config"
	"github.com/pod32g/omni-logging/internal/forward"
	"github.com/pod32g/omni-logging/internal/ingest"
	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/queryclient"
	"github.com/pod32g/omni-logging/internal/store/sqlite"
	"github.com/pod32g/omni-logging/internal/tail"
	"github.com/pod32g/omni-logging/internal/wal"
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
	case "query":
		err = runQuery(os.Args[2:])
	case "backup":
		err = runBackup(os.Args[2:])
	case "integrity":
		err = runIntegrity(os.Args[2:])
	case "healthcheck":
		err = runHealthcheck(os.Args[2:])
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
  serve        Run the logging server (HTTP API + embedded web UI)
  forward      Tail one or more files and forward them to a server
  query        Search a server from the terminal (table/JSON/NDJSON, --follow)
  backup       Write a consistent online snapshot of the database (VACUUM INTO)
  integrity    Run PRAGMA integrity_check; exit non-zero if the DB is unsound
  healthcheck  HTTP-probe a URL; exit non-zero unless it returns 2xx
  version      Print the version

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
		walDir     = fs.String("wal-dir", "", "ingest write-ahead log dir (default <db dir>/wal)")
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
	if set["wal-dir"] {
		cfg.WALDir = *walDir
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

	// Apply the configured log level now that config is resolved.
	if lvl, ok := parseLogLevel(cfg.LogLevel); ok {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
		slog.SetDefault(logger)
	}

	store, err := sqlite.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// Open the ingest write-ahead log (durable accept) for file-backed databases.
	var w *wal.WAL
	if walDir := cfg.ResolveWALDir(); walDir != "" {
		w, err = wal.Open(wal.Options{Dir: walDir})
		if err != nil {
			return fmt.Errorf("open wal: %w", err)
		}
		defer w.Close()
	}

	limiter := admission.New(admission.Limits{
		RatePerSec:  cfg.RateLimitPerSec,
		Burst:       cfg.RateBurst,
		DailyEvents: cfg.DailyQuotaEvents,
		DailyBytes:  cfg.DailyQuotaBytes,
	}, time.Now)

	hub := tail.NewHub()
	ing := ingest.New(store, hub, ingest.Options{
		BufferSize:    cfg.BufferSize,
		BatchSize:     cfg.BatchSize,
		FlushInterval: time.Duration(cfg.FlushIntervalMS) * time.Millisecond,
		Logger:        logger,
		WAL:           w,
		Limiter:       limiter,
	})
	// Replay any events accepted before a previous crash, then start the writer.
	if w != nil {
		n, rerr := ing.Recover(context.Background())
		if rerr != nil {
			return fmt.Errorf("wal recovery: %w", rerr)
		}
		if n > 0 {
			logger.Info("wal recovery: replayed accepted events", "events", n)
		}
	}
	ing.Start()
	defer ing.Stop()

	srv := api.New(api.Deps{
		Config:   cfg,
		Store:    store,
		Ingestor: ing,
		Hub:      hub,
		UI:       web.FS(),
		Logger:   logger,
		Version:  version,
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

// runQuery searches a server from the terminal.
func runQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	var (
		server = fs.String("server", "http://localhost:8080", "server base URL")
		token  = fs.String("token", "", "admin token (or OMNILOG_ADMIN_TOKEN)")
		q      = fs.String("q", "", "query expression")
		last   = fs.String("last", "1h", "relative window (e.g. 15m, 1h, 7d; empty = all time)")
		from   = fs.String("from", "", "absolute start (RFC3339 or unix seconds)")
		to     = fs.String("to", "", "absolute end")
		limit  = fs.String("limit", "", "max events to return")
		order  = fs.String("order", "", "newest|oldest")
		format = fs.String("format", "table", "output: table|json|ndjson")
		follow = fs.Bool("follow", false, "stream live matching events (SSE)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *token == "" {
		*token = os.Getenv("OMNILOG_ADMIN_TOKEN")
	}
	c := &queryclient.Client{ServerURL: *server, Token: *token}
	params := map[string]string{"q": *q, "last": *last, "from": *from, "to": *to, "limit": *limit, "order": *order}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *follow {
		err := c.Follow(ctx, params, func(e model.LogEvent) {
			_ = queryclient.FormatEventLine(os.Stdout, e, *format)
		})
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}

	res, err := c.Search(ctx, params)
	if err != nil {
		return err
	}
	switch *format {
	case "json":
		return queryclient.WriteJSON(os.Stdout, res)
	case "ndjson":
		return queryclient.WriteNDJSON(os.Stdout, res.Events)
	default:
		return queryclient.WriteTable(os.Stdout, res.Events)
	}
}

// runBackup writes a consistent online snapshot of the database to --out using
// SQLite's VACUUM INTO (WAL-safe). Used by the deploy to back up before changes.
func runBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	db := fs.String("db", "omni.db", "path to the SQLite database")
	out := fs.String("out", "", "destination snapshot path (must not already exist)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("backup: --out is required")
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return fmt.Errorf("backup: create destination dir: %w", err)
	}
	store, err := sqlite.Open(*db)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.BackupTo(context.Background(), *out); err != nil {
		return err
	}
	fmt.Printf("backup written to %s\n", *out)
	return nil
}

// runIntegrity runs PRAGMA integrity_check and exits non-zero if not OK. Used by
// the deploy to verify the database after a release (and trigger auto-heal).
func runIntegrity(args []string) error {
	fs := flag.NewFlagSet("integrity", flag.ExitOnError)
	db := fs.String("db", "omni.db", "path to the SQLite database")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := sqlite.Open(*db)
	if err != nil {
		return err
	}
	defer store.Close()
	ok, problems, err := store.IntegrityCheck(context.Background())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("integrity check FAILED: %s", strings.Join(problems, "; "))
	}
	fmt.Println("integrity check: ok")
	return nil
}

// runHealthcheck performs an HTTP GET and exits non-zero unless it returns 2xx.
// Used as the container HEALTHCHECK since the distroless image has no curl/wget.
func runHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	url := fs.String("url", "http://localhost:8080/api/v1/healthz", "URL to probe")
	timeout := fs.Duration("timeout", 5*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: *timeout}).Get(*url)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("healthcheck: %s returned %d", *url, resp.StatusCode)
	}
	return nil
}

// multiFlag collects a repeatable string flag into a slice.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// parseLogLevel maps a config string to a slog level. ok is false for empty or
// unrecognized values (the caller keeps the default).
func parseLogLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
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

// Package logging initializes a structured logger based on log/slog
// (stdlib, Go 1.21+) and injects the current OpenTelemetry trace_id and
// span_id into every log line via a small Handler wrapper.
//
// One Init() call per process. After that, any package can call
// slog.Info / slog.Warn / slog.Error / *Context variants and the
// output will be structured (text or JSON) and correlated with the
// active OTel span:
//
//	func main() {
//	    logging.Init(logging.Config{
//	        Level:   logging.LevelFromEnv(),
//	        Format:  logging.FormatFromEnv(),  // "text" (default) or "json"
//	        Service: "api",
//	        Version: "dev",
//	    })
//	    // ... rest of main
//	}
//
// Defaults (override via env vars before Init):
//   - LOG_LEVEL   (default: info)
//   - LOG_FORMAT  (default: text)
//   - SERVICE_NAME / SERVICE_VERSION are honored if Config.Service /
//     Config.Version are empty, matching the pattern used by tracing.Init.
//
// Why slog? It's stdlib, zero new deps, and has a Handler interface that
// makes the OTel correlation wrapper ~30 LOC. If we ever outgrow slog's
// throughput (we won't — ~1k lines/sec is the bar), swap to zap/zerolog
// and keep ContextHandler as a shared middle layer.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Config controls the global slog logger built by Init.
type Config struct {
	// Level is the minimum level to emit. Records below this are dropped.
	Level slog.Level
	// Format is "text" (default) or "json". Case-insensitive.
	// Unknown values fall back to text and log a warning.
	Format string
	// Service is added to every record as `service=<value>`. If empty,
	// reads SERVICE_NAME env var; defaults to "unknown".
	Service string
	// Version is added to every record as `service_version=<value>`. If
	// empty, reads SERVICE_VERSION env var; defaults to "dev".
	Version string
	// Output is the destination for the formatted log lines. If nil,
	// defaults to os.Stdout. The default intentionally matches the
	// 12-factor app convention; the existing scripts/start-bg.sh
	// captures both stdout and stderr to logs/<svc>.log, so the file
	// layout doesn't change.
	Output io.Writer
}

// Init builds the global slog logger from cfg and calls slog.SetDefault
// so any slog.Info / slog.Warn / slog.Error call in the binary inherits
// it. Returns the same logger for callers that want a per-instance handle.
//
// After Init, the recommended call pattern is:
//
//	slog.InfoContext(ctx, "reservation inserted", "id", id)
//
// The *Context variant carries ctx into the handler, where
// ContextHandler pulls trace_id and span_id from the active OTel span
// (if any). For non-OTel paths (e.g. one-shot CLIs) or code that
// doesn't have a ctx in scope, the bare Info / Warn / Error variants
// log without trace correlation — still structured, still useful.
func Init(cfg Config) *slog.Logger {
	if cfg.Output == nil {
		cfg.Output = os.Stdout
	}
	if cfg.Service == "" {
		cfg.Service = envOr("SERVICE_NAME", "unknown")
	}
	if cfg.Version == "" {
		cfg.Version = envOr("SERVICE_VERSION", "dev")
	}

	opts := &slog.HandlerOptions{Level: cfg.Level}

	var base slog.Handler
	switch strings.ToLower(strings.TrimSpace(cfg.Format)) {
	case "json":
		base = slog.NewJSONHandler(cfg.Output, opts)
	case "", "text":
		base = slog.NewTextHandler(cfg.Output, opts)
	default:
		// Invalid format — fall back to text so the binary still boots,
		// and warn once so the operator notices. We use the
		// not-yet-initialised default logger here (text handler at
		// info level on stderr) because Init is supposed to be the
		// first thing that runs in main; warning via slog.Default
		// would route to the new (mis-configured) handler.
		fmt.Fprintf(os.Stderr, "logging: unknown LOG_FORMAT=%q, falling back to text\n", cfg.Format)
		base = slog.NewTextHandler(cfg.Output, opts)
	}

	handler := NewContextHandler(base)

	// service + service_version are attached to every record via
	// logger.With, which calls handler.WithAttrs under the hood.
	// Our ContextHandler re-wraps the result so trace_id injection
	// still works after WithAttrs composition.
	logger := slog.New(handler).With(
		"service", cfg.Service,
		"service_version", cfg.Version,
	)
	slog.SetDefault(logger)
	return logger
}

// LevelFromEnv reads LOG_LEVEL and parses it via LevelFromString.
// Returns slog.LevelInfo on parse error or unset.
func LevelFromEnv() slog.Level {
	lvl, _ := LevelFromString(os.Getenv("LOG_LEVEL"))
	return lvl
}

// FormatFromEnv reads LOG_FORMAT. Returns "text" on empty/invalid
// (Init will also fall back to text, so this stays consistent).
func FormatFromEnv() string {
	return strings.ToLower(strings.TrimSpace(os.Getenv("LOG_FORMAT")))
}

// LevelFromString parses "debug" / "info" / "warn" / "error" / "warning"
// (case-insensitive, leading/trailing whitespace ignored). An empty
// string returns slog.LevelInfo (the most useful dev default). Unknown
// values return slog.LevelInfo and a non-nil error so the caller can
// log the bad value without losing the chance to start up.
func LevelFromString(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("logging: unknown level %q (want debug|info|warn|error)", s)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

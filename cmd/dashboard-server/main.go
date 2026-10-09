// Command dashboard-server is a tiny dev tool that serves the k6 live
// dashboard (loadtest/dashboard.html) on http://localhost:8082/ and
// reverse-proxies /v1/* to the running k6 instance's REST API
// (default http://localhost:6565). Solves the CORS problem of opening
// the HTML from file:// against k6's REST API.
//
// Usage:
//
//	./bin/dashboard-server                       # uses defaults
//	DASHBOARD_ADDR=:9090 ./bin/dashboard-server  # custom port
//	K6_REST_URL=http://k6:6565 ./bin/dashboard-server  # remote k6
//
// Run order:
//
//	./bin/dashboard-server &      # background
//	k6 run --address :6565 --linger loadtest/burst.js
//	open http://localhost:8082/   # see live metrics
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"

	"github.com/emmitt-k/ticket-deal/internal/logging"
)

func main() {
	logging.Init(logging.Config{
		Level:   logging.LevelFromEnv(),
		Format:  logging.FormatFromEnv(),
		Service: "dashboard-server",
		Version: os.Getenv("SERVICE_VERSION"),
	})

	addr := flag.String("addr", envOr("DASHBOARD_ADDR", ":8082"), "address to listen on (e.g. :8082)")
	k6URL := flag.String("k6", envOr("K6_REST_URL", "http://localhost:6565"), "k6 REST API base URL")
	htmlPath := flag.String("html", envOr("DASHBOARD_HTML", "loadtest/dashboard.html"), "path to dashboard.html")
	flag.Parse()

	// Load the HTML once at startup; log a clear error if it's missing.
	html, err := os.ReadFile(*htmlPath)
	if err != nil {
		slog.Error("cannot read dashboard HTML (run from the repo root, or set -html)",
			"path", *htmlPath, "error", err)
		os.Exit(1)
	}

	target, err := url.Parse(*k6URL)
	if err != nil {
		slog.Error("bad k6 URL", "url", *k6URL, "error", err)
		os.Exit(1)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// Inject a permissive Origin so the (proxied) k6 response is treated same-origin
	// by the browser — k6's REST API has no CORS layer, so we go through this proxy
	// specifically to avoid having to install a browser extension.
	originalDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		originalDirector(r)
		r.Header.Set("Origin", *k6URL)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(html)
	})

	slog.Info("dashboard serving",
		"addr", *addr,
		"k6_rest", *k6URL,
		"html", *htmlPath,
	)
	slog.Error("dashboard server exited", "error", http.ListenAndServe(*addr, mux))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

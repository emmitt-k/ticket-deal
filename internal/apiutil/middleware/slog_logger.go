// Package middleware holds HTTP middleware for the API. Right now it
// only contains a slog-based access log, but the same package can
// grow to host request-scoped middleware (auth helpers, request IDs,
// rate-limit shorthands) without polluting internal/apiutil/response.go.
package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// NewSlogAccessLog returns a chi-compatible middleware that emits one
// structured slog line per HTTP request after the response is written.
// Replaces chi/middleware.Logger, which uses its own unstructured
// format and has no way to inject the active OTel trace_id.
//
// The ctx passed into the handler already carries the otelhttp span
// (because otelhttp.NewHandler wraps the router and starts the span
// before any inner middleware runs), so slog.InfoContext(r.Context(),
// ...) gets trace_id / span_id auto-injected by logging.ContextHandler.
//
// Recorded fields (all string-typed for cheap grep/jq):
//
//	method        GET/POST/...
//	path          request URL path (no query — query is logged separately
//	              if your route needs it, to avoid logging tokens)
//	status        numeric HTTP status code as int
//	bytes         response body size in bytes
//	duration_ms   wall time spent inside the handler chain
//	client_ip     best-effort client IP (honours X-Forwarded-For)
//	user_agent    raw User-Agent header
func NewSlogAccessLog() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(ww, r)

			// Don't log /metrics scrapes — Prometheus hits this every
			// 15s and it would dwarf the rest of the log volume.
			if r.URL.Path == "/metrics" {
				return
			}

			slog.InfoContext(r.Context(), "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.status,
				"bytes", ww.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
				"client_ip", clientIP(r),
				"user_agent", r.UserAgent(),
			)
		})
	}
}

// statusRecorder wraps http.ResponseWriter to capture the status code
// and the number of bytes written. The stdlib doesn't expose either
// by default; we need both for the access log. Default status is 200
// (matches the stdlib's "no WriteHeader call → 200" behaviour).
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// clientIP extracts the client IP from the request, checking
// X-Forwarded-For first (for proxies) and falling back to
// RemoteAddr. Strips the port from RemoteAddr. Kept here (not in
// the waiting-room package) because the access log is the
// canonical place that wants it.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if idx := strings.Index(fwd, ","); idx != -1 {
			fwd = fwd[:idx]
		}
		return strings.TrimSpace(fwd)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

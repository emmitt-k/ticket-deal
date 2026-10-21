package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/emmitt-k/ticket-deal/internal/logging"
)

// fixedTestTraceID is a 32-hex-char (16-byte) trace ID used to assert
// that the access log line carries the active OTel trace_id.
const fixedTestTraceID = "11111111111111111111111111111111"

func fixedTestTraceIDBytes() trace.TraceID {
	tid, _ := trace.TraceIDFromHex(fixedTestTraceID)
	return tid
}

// newCtxWithSpan returns a request context that carries a valid
// OTel SpanContext so ContextHandler will inject trace_id.
func newCtxWithSpan(t *testing.T, r *http.Request) *http.Request {
	t.Helper()
	tid := fixedTestTraceIDBytes()
	sid, _ := trace.SpanIDFromHex("2222222222222222")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
		Remote:     false,
	})
	ctx := trace.ContextWithSpanContext(r.Context(), sc)
	return r.WithContext(ctx)
}

// captureSlog swaps slog.Default for one that writes to buf, and
// returns a cleanup func. The handler is wrapped with
// logging.ContextHandler so trace_id injection (the whole point of
// the access log) is exercised in tests.
func captureSlog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	base := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(logging.NewContextHandler(base)))
	return &buf, func() { slog.SetDefault(prev) }
}

// TestNewSlogAccessLog_BasicFields verifies the access log emits all
// promised structured fields.
func TestNewSlogAccessLog_BasicFields(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	mw := NewSlogAccessLog()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest("GET", "/api/tickets/reserve", nil)
	req.Header.Set("User-Agent", "test-agent/1.0")
	req = newCtxWithSpan(t, req)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	out := buf.String()
	for _, want := range []string{
		"msg=\"http request\"",
		"method=GET",
		"path=/api/tickets/reserve",
		"status=418", // StatusTeapot
		"bytes=2",    // len("ok")
		"duration_ms=",
		"client_ip=",
		"user_agent=test-agent/1.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull: %s", want, out)
		}
	}
}

// TestNewSlogAccessLog_TraceCorrelation verifies that a request with
// an active OTel span gets the trace_id injected into the access log.
// This is the whole point of using slog.InfoContext here instead of
// slog.Info.
func TestNewSlogAccessLog_TraceCorrelation(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	mw := NewSlogAccessLog()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/api/tickets/enter", nil)
	req = newCtxWithSpan(t, req)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	out := buf.String()
	if !strings.Contains(out, "trace_id="+fixedTestTraceID) {
		t.Errorf("expected trace_id=%s in access log, got: %s", fixedTestTraceID, out)
	}
}

// TestNewSlogAccessLog_DefaultStatus200 verifies that a handler which
// doesn't call WriteHeader still produces status=200 in the log line.
func TestNewSlogAccessLog_DefaultStatus200(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	mw := NewSlogAccessLog()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No WriteHeader call — stdlib defaults to 200.
		_, _ = w.Write([]byte("hi"))
	}))

	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	out := buf.String()
	if !strings.Contains(out, "status=200") {
		t.Errorf("expected status=200 default, got: %s", out)
	}
}

// TestNewSlogAccessLog_SkipsMetrics verifies that the /metrics path
// does NOT produce a log line (would be too noisy under Prometheus
// scraping every 15s).
func TestNewSlogAccessLog_SkipsMetrics(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	mw := NewSlogAccessLog()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("metrics_payload"))
	}))

	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if buf.Len() != 0 {
		t.Errorf("expected no log line for /metrics, got: %s", buf.String())
	}
}

// TestNewSlogAccessLog_NoSpanNoTraceID verifies that requests without
// an active OTel span don't get a trace_id attribute (degrades
// gracefully for tests / health checks / untraced endpoints).
func TestNewSlogAccessLog_NoSpanNoTraceID(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	mw := NewSlogAccessLog()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// No newCtxWithSpan — bare request, no OTel context.
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	out := buf.String()
	if strings.Contains(out, "trace_id=") {
		t.Errorf("expected no trace_id for unspanned request, got: %s", out)
	}
	// Sanity: the line was still emitted.
	if !strings.Contains(out, "msg=\"http request\"") {
		t.Errorf("expected http request line, got: %s", out)
	}
}

// TestClientIP checks the X-Forwarded-For and RemoteAddr paths.
func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(*http.Request)
		remoteAddr string
		want       string
	}{
		{
			name:       "X-Forwarded-For single",
			setup:      func(r *http.Request) { r.Header.Set("X-Forwarded-For", "203.0.113.1") },
			remoteAddr: "10.0.0.1:54321",
			want:       "203.0.113.1",
		},
		{
			name: "X-Forwarded-For chain (first wins)",
			setup: func(r *http.Request) {
				r.Header.Set("X-Forwarded-For", "203.0.113.1, 198.51.100.1, 10.0.0.1")
			},
			remoteAddr: "10.0.0.1:54321",
			want:       "203.0.113.1",
		},
		{
			name:       "RemoteAddr fallback",
			setup:      func(r *http.Request) {},
			remoteAddr: "10.0.0.1:54321",
			want:       "10.0.0.1",
		},
		{
			name:       "RemoteAddr IPv6",
			setup:      func(r *http.Request) {},
			remoteAddr: "[2001:db8::1]:54321",
			want:       "2001:db8::1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			tt.setup(r)
			r.RemoteAddr = tt.remoteAddr
			got := clientIP(r)
			if got != tt.want {
				t.Errorf("clientIP: got %q, want %q", got, tt.want)
			}
		})
	}
}

// avoid unused import warnings if the file's import list changes
var _ = otel.Tracer

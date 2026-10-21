package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// fixedTraceID is a 32-hex-char (16-byte) trace ID used across tests.
const fixedTraceID = "fac1f09d3a4b5c6d7e8f9a0b1c2d3e4f"

// fixedSpanID is a 16-hex-char (8-byte) span ID used across tests.
const fixedSpanID = "71bebc50a1b2c3d4"

// makeTestCtx returns a context with a valid, sampled OTel SpanContext
// (manually constructed — no SDK required, no docker).
func makeTestCtx(t *testing.T) context.Context {
	t.Helper()
	tid, err := trace.TraceIDFromHex(fixedTraceID)
	if err != nil {
		t.Fatalf("parse trace id: %v", err)
	}
	sid, err := trace.SpanIDFromHex(fixedSpanID)
	if err != nil {
		t.Fatalf("parse span id: %v", err)
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
		Remote:     false,
	})
	return trace.ContextWithSpanContext(context.Background(), sc)
}

// TestContextHandler_NoSpan verifies that a context without a span
// passes through the wrapper unchanged — no trace_id attribute is
// added. This is the case before tracing.Init runs, or in one-shot
// CLIs that don't initialise tracing at all.
func TestContextHandler_NoSpan(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := NewContextHandler(inner)

	logger := slog.New(h)
	logger.Info("hello", "key", "value")

	line := buf.String()
	if strings.Contains(line, "trace_id") {
		t.Errorf("expected no trace_id in output, got: %s", line)
	}
	if !strings.Contains(line, `"msg":"hello"`) {
		t.Errorf("expected msg=hello, got: %s", line)
	}
	if !strings.Contains(line, `"key":"value"`) {
		t.Errorf("expected key=value, got: %s", line)
	}
}

// TestContextHandler_WithValidSpan verifies that a context with a
// valid OTel span produces the expected trace_id and span_id attrs.
func TestContextHandler_WithValidSpan(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := NewContextHandler(inner)

	logger := slog.New(h)
	logger.InfoContext(makeTestCtx(t), "hello", "key", "value")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("parse JSON output: %v\nraw: %s", err, buf.String())
	}
	if got, want := rec["trace_id"], fixedTraceID; got != want {
		t.Errorf("trace_id: got %v, want %s", got, want)
	}
	if got, want := rec["span_id"], fixedSpanID; got != want {
		t.Errorf("span_id: got %v, want %s", got, want)
	}
	if got, want := rec["msg"], "hello"; got != want {
		t.Errorf("msg: got %v, want %v", got, want)
	}
	if got, want := rec["key"], "value"; got != want {
		t.Errorf("key: got %v, want %v", got, want)
	}
}

// TestContextHandler_InvalidSpan verifies that a context with the
// zero-value SpanContext (e.g. one created with
// trace.ContextWithSpanContext(ctx, trace.SpanContext{})) does NOT
// inject trace_id. The wrapper must not emit a zero-valued trace ID.
func TestContextHandler_InvalidSpan(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := NewContextHandler(inner)

	// Empty SpanContext — IsValid() returns false
	zeroCtx := trace.ContextWithSpanContext(context.Background(), trace.SpanContext{})
	logger := slog.New(h)
	logger.InfoContext(zeroCtx, "hello")

	if strings.Contains(buf.String(), "trace_id") {
		t.Errorf("expected no trace_id for zero span context, got: %s", buf.String())
	}
}

// TestContextHandler_WithAttrs verifies that logger.With(...) chains
// (used to attach service= and service_version=) still produce a
// working handler that injects trace_id. This is the regression
// test for "I called logger.With and then trace_id disappeared".
func TestContextHandler_WithAttrs(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := NewContextHandler(inner)

	logger := slog.New(h).With("service", "api", "service_version", "dev")
	logger.InfoContext(makeTestCtx(t), "hello")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("parse JSON output: %v\nraw: %s", err, buf.String())
	}
	if got, want := rec["service"], "api"; got != want {
		t.Errorf("service: got %v, want %v", got, want)
	}
	if got, want := rec["service_version"], "dev"; got != want {
		t.Errorf("service_version: got %v, want %v", got, want)
	}
	if got, want := rec["trace_id"], fixedTraceID; got != want {
		t.Errorf("trace_id: got %v, want %s (lost after With)", got, want)
	}
}

// TestContextHandler_WithGroup documents the WithGroup behavior: when a
// caller does logger.WithGroup("request"), slog nests ALL subsequent
// attributes (including our injected trace_id / span_id) under the
// group key. The trace_id is still findable (e.g. "request.trace_id"
// in jq / Loki) — it's just not at the top level of the record.
//
// This is a fundamental slog behavior shared by every standard
// handler (JSON, Text, anything that delegates via WithGroup). The
// only way to keep trace_id at the absolute top level regardless of
// groups would be a custom formatter that bypasses the inner
// handler entirely — not worth the complexity for an edge case
// nothing in this repo currently uses. If a future caller needs
// "trace_id always at top level", that's a v2 of ContextHandler.
func TestContextHandler_WithGroup(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := NewContextHandler(inner)

	logger := slog.New(h).WithGroup("request")
	logger.InfoContext(makeTestCtx(t), "hello", "method", "POST")

	// With group, everything (including injected trace_id) lands
	// under the "request" key.
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("parse JSON output: %v\nraw: %s", err, buf.String())
	}
	req, ok := rec["request"].(map[string]any)
	if !ok {
		t.Fatalf("expected request group, got top-level keys: %v", keysOf(rec))
	}
	if got, want := req["trace_id"], fixedTraceID; got != want {
		t.Errorf("request.trace_id: got %v, want %s", got, want)
	}
	if got, want := req["span_id"], fixedSpanID; got != want {
		t.Errorf("request.span_id: got %v, want %s", got, want)
	}
	if got, want := req["method"], "POST"; got != want {
		t.Errorf("request.method: got %v, want %v", got, want)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestContextHandler_Enabled verifies that the level filter is
// honoured by delegation to the inner handler.
func TestContextHandler_Enabled(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	h := NewContextHandler(inner)

	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Errorf("Enabled(Info) with Warn threshold: got true, want false")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Errorf("Enabled(Error) with Warn threshold: got false, want true")
	}
}

// TestLevelFromString covers the parser.
func TestLevelFromString(t *testing.T) {
	cases := []struct {
		in     string
		want   slog.Level
		errOk  bool // true if an error is expected
	}{
		{"", slog.LevelInfo, false},
		{"info", slog.LevelInfo, false},
		{"INFO", slog.LevelInfo, false},
		{"  Info  ", slog.LevelInfo, false},
		{"debug", slog.LevelDebug, false},
		{"DEBUG", slog.LevelDebug, false},
		{"warn", slog.LevelWarn, false},
		{"warning", slog.LevelWarn, false},
		{"WARN", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"ERROR", slog.LevelError, false},
		{"trace", slog.LevelInfo, true}, // unknown
		{"fatal", slog.LevelInfo, true},  // unknown (Go's slog has no fatal)
		{"verbose", slog.LevelInfo, true},
	}
	for _, c := range cases {
		got, err := LevelFromString(c.in)
		if (err != nil) != c.errOk {
			t.Errorf("LevelFromString(%q) err = %v, wantErr=%v", c.in, err, c.errOk)
		}
		if got != c.want {
			t.Errorf("LevelFromString(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestInit_TextFormat verifies that Init with Format="text" produces
// human-readable text output with service/version prepended.
func TestInit_TextFormat(t *testing.T) {
	var buf bytes.Buffer
	Init(Config{
		Level:   slog.LevelInfo,
		Format:  "text",
		Service: "test-svc",
		Version: "1.2.3",
		Output:  &buf,
	})
	// Reset default after this test so other tests don't see it.
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	slog.Info("hello", "key", "value")

	out := buf.String()
	if !strings.Contains(out, `service=test-svc`) {
		t.Errorf("expected service=test-svc in output: %s", out)
	}
	if !strings.Contains(out, `service_version=1.2.3`) {
		t.Errorf("expected service_version=1.2.3 in output: %s", out)
	}
	if !strings.Contains(out, `msg=hello`) {
		t.Errorf("expected msg=hello in output: %s", out)
	}
	if !strings.Contains(out, `key=value`) {
		t.Errorf("expected key=value in output: %s", out)
	}
	// Text format: must NOT be JSON
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("text format should not start with '{': %s", out)
	}
}

// TestInit_JSONFormat verifies that Init with Format="json" produces
// valid JSON output.
func TestInit_JSONFormat(t *testing.T) {
	var buf bytes.Buffer
	Init(Config{
		Level:   slog.LevelInfo,
		Format:  "json",
		Service: "test-svc",
		Version: "1.2.3",
		Output:  &buf,
	})
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	slog.Info("hello", "key", "value")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("parse JSON: %v\nraw: %s", err, buf.String())
	}
	if got, want := rec["service"], "test-svc"; got != want {
		t.Errorf("service: got %v, want %v", got, want)
	}
	if got, want := rec["service_version"], "1.2.3"; got != want {
		t.Errorf("service_version: got %v, want %v", got, want)
	}
	if got, want := rec["msg"], "hello"; got != want {
		t.Errorf("msg: got %v, want %v", got, want)
	}
	if got, want := rec["key"], "value"; got != want {
		t.Errorf("key: got %v, want %v", got, want)
	}
}

// TestInit_DefaultsApplied verifies that empty Service/Version fall
// back to env vars, then to "unknown"/"dev".
func TestInit_DefaultsApplied(t *testing.T) {
	t.Setenv("SERVICE_NAME", "from-env")
	t.Setenv("SERVICE_VERSION", "v9.9.9")

	var buf bytes.Buffer
	Init(Config{
		Level:  slog.LevelInfo,
		Format: "text",
		Output: &buf,
	})
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	slog.Info("hello")

	out := buf.String()
	if !strings.Contains(out, `service=from-env`) {
		t.Errorf("expected service=from-env from env var, got: %s", out)
	}
	if !strings.Contains(out, `service_version=v9.9.9`) {
		t.Errorf("expected service_version=v9.9.9 from env var, got: %s", out)
	}
}

// TestInit_InvalidFormatFallsBack verifies that an unknown format
// string logs a warning and falls back to text rather than panicking.
func TestInit_InvalidFormatFallsBack(t *testing.T) {
	var buf bytes.Buffer
	Init(Config{
		Level:  slog.LevelInfo,
		Format: "yaml", // unsupported on purpose
		Output: &buf,
	})
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	slog.Info("hello")

	// Even with Format="yaml", the output should be text-ish
	// (key=value pairs) — not panic, not error.
	if !strings.Contains(buf.String(), `msg=hello`) {
		t.Errorf("expected text fallback output, got: %s", buf.String())
	}
}

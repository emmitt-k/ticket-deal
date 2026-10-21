package logging

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// ContextHandler is an slog.Handler wrapper that injects the active
// OTel trace_id and span_id into every record whose context carries a
// valid SpanContext.
//
// If the context has no span (or the SpanContext is the zero value,
// e.g. before tracing.Init has run, or in one-shot CLIs that don't
// initialise tracing at all), the wrapped handler is called unchanged.
// That means the wrapper is safe to install unconditionally in main() —
// it degrades gracefully instead of forcing every call site to check
// for an active span.
//
// The injected attribute keys are exactly "trace_id" and "span_id"
// (lowercase, snake_case) so they match the W3C TraceContext field
// names. Jaeger / Grafana / Loki all recognise this naming.
//
// Composition with WithAttrs / WithGroup: every delegation returns a
// new *ContextHandler wrapping the inner result, so logger.With(...)
// chains (used to attach service=api etc.) keep trace_id injection
// working at every level.
type ContextHandler struct {
	inner slog.Handler
}

// Compile-time assertion: *ContextHandler must satisfy slog.Handler.
var _ slog.Handler = (*ContextHandler)(nil)

// NewContextHandler wraps inner. The returned handler adds trace_id
// and span_id attributes to every record whose context has a valid
// OTel span. If inner is nil the returned handler is also nil.
func NewContextHandler(inner slog.Handler) *ContextHandler {
	if inner == nil {
		return nil
	}
	return &ContextHandler{inner: inner}
}

// Enabled implements slog.Handler by delegating to the inner handler.
// We don't try to short-circuit on context — the inner handler knows
// its own level rules.
func (h *ContextHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

// Handle implements slog.Handler. It pulls the OTel SpanContext from
// ctx and, if valid, appends trace_id and span_id to the record before
// delegating to the inner handler. The inner handler decides the final
// format (text or JSON) of those attributes.
func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	// SpanContextFromContext is cheap: a ctx.Value lookup that returns
	// a value type. Safe to call on every record.
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		// We inject whether or not IsSampled() is true. Reasoning:
		// even a dropped trace has a real trace_id that an operator
		// might be searching for after the fact ("what happened on
		// trace abc?"). If the span is the zero value (no tracing
		// initialised) sc.IsValid() is false and we skip entirely.
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs implements slog.Handler. Returns a new ContextHandler
// wrapping the inner handler's WithAttrs result, so trace_id injection
// still works after the chained WithAttrs calls (service=, service_version=, etc.).
func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup implements slog.Handler. Same delegation pattern as WithAttrs.
//
// Caveat: when a group is active, the inner slog handler nests
// ALL subsequent attributes under that group — including the
// trace_id and span_id this wrapper would normally inject. So
// "request.trace_id" instead of top-level "trace_id". The trace_id
// is still findable via dot-path queries in jq / Loki. This is
// inherent to slog's Handler contract, not a bug. No code in this
// repo currently uses WithGroup; if a future caller needs top-level
// trace_id always, that needs a v2 of ContextHandler with a custom
// formatter.
func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{inner: h.inner.WithGroup(name)}
}

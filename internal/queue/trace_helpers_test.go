package queue

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// These tests cover the PURE half of the trace propagation: the
// helpers that read and write the W3C traceparent without needing a
// broker. The SQS-facing half lives in sqs_test.go.
//
// The body fallback is the one that matters most: ElasticMQ in local
// dev strips MessageAttributes on the roundtrip, so the `_traceparent`
// field embedded in the JSON body is the ONLY thing that survives —
// and when it does, the worker's span is a child of the API's span
// instead of a disconnected root.

// TestExtractTraceparentFromBody — the documented fallback, and its
// edges. A malformed body must degrade to "no propagation" (a root
// span), never to an error.
func TestExtractTraceparentFromBody(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "the field on its own",
			body: `{"_traceparent":"00-abc-def-01"}`,
			want: "00-abc-def-01",
		},
		{
			name: "the field among others",
			body: `{"a":1,"_traceparent":"00-abc-def-01","b":2}`,
			want: "00-abc-def-01",
		},
		{
			name: "no such field",
			body: `{"a":1}`,
			want: "",
		},
		{
			name: "an empty body",
			body: "",
			want: "",
		},
		{
			name: "not JSON at all",
			body: "this is not a json body",
			want: "",
		},
		{
			name: "an unterminated string",
			body: `{"_traceparent":"unterminated`,
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, extractTraceparentFromBody(tc.body))
		})
	}
}

// TestTraceContextFromContext — the trace_id/span_id reader used when
// the SQS attributes cannot be set (e.g. SendMessageBatched, which has
// limited attribute space).
func TestTraceContextFromContext(t *testing.T) {
	t.Run("no active span", func(t *testing.T) {
		traceID, spanID := TraceContextFromContext(context.Background())
		require.Empty(t, traceID)
		require.Empty(t, spanID)
	})

	t.Run("with an active span", func(t *testing.T) {
		tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
		otel.SetTracerProvider(tp)

		ctx, span := tp.Tracer("test").Start(context.Background(), "publisher-span")
		defer span.End()

		traceID, spanID := TraceContextFromContext(ctx)
		require.NotEmpty(t, traceID)
		require.NotEmpty(t, spanID)
		require.Equal(t, span.SpanContext().TraceID().String(), traceID)
		require.Equal(t, span.SpanContext().SpanID().String(), spanID)
	})
}

// TestTraceparentFromCtx — the publisher reads the traceparent back out
// of the propagator so it can embed it in the message body.
func TestTraceparentFromCtx(t *testing.T) {
	t.Run("no active span", func(t *testing.T) {
		require.Empty(t, traceparentFromCtx(context.Background()))
	})

	t.Run("with an active span", func(t *testing.T) {
		tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))

		ctx, span := tp.Tracer("test").Start(context.Background(), "publisher-span")
		defer span.End()

		tpHeader := traceparentFromCtx(ctx)
		require.NotEmpty(t, tpHeader, "a span context must produce a traceparent")
		// The W3C traceparent format: version-traceid-spanid-flags.
		require.Regexp(t, `^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`, tpHeader)
	})
}

// TestCarrierKeys — the carriers implement the OTel TextMapCarrier
// interface, which includes Keys(). Keys() is what the propagator
// iterates over, so an empty implementation would silently drop
// propagation.
func TestCarrierKeys(t *testing.T) {
	t.Run("the SQS attribute carrier", func(t *testing.T) {
		c := sqsAttributeCarrier{attrs: map[string]string{
			"traceparent": "00-abc-def-01",
			"baggage":     "k=v",
		}}
		require.Equal(t, "00-abc-def-01", c.Get("traceparent"))
		require.Equal(t, "", c.Get("absent"))

		keys := c.Keys()
		require.Len(t, keys, 2)
		require.Contains(t, keys, "traceparent")
		require.Contains(t, keys, "baggage")
	})

	t.Run("the inject-only carrier", func(t *testing.T) {
		c := injectOnlyCarrier{attrs: map[string]string{}}
		c.Set("traceparent", "00-abc-def-01")
		require.Equal(t, "00-abc-def-01", c.Get("traceparent"))
		require.Nil(t, c.Keys(),
			"the inject-only carrier never needs to enumerate its keys")
	})
}

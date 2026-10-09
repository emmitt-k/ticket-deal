package queue

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// SQS propagator helpers
//
// OpenTelemetry's context propagation model uses a `TextMapCarrier` —
// a flat key→string map. SQS message attributes are exactly that
// shape, so we can use them as a carrier.
//
// Why propagate through SQS at all?
//   - Without it, the worker's `worker.handleMessage` span is a ROOT
//     span, completely disconnected from the API's `reserve.handle`.
//   - With it, a single trace_id covers api → sqs → worker → pg, and
//     you can see end-to-end timing (queue time included).
//
// The `traceparent` attribute is the W3C standard
// (https://www.w3.org/TR/trace-context/). Baggage is a sidecar for
// custom key/value pairs; we don't use it yet but the propagator
// already carries it for free.

const (
	// traceparentAttr and baggageAttr are reserved message-attribute
	// names per the OTel messaging semantic conventions.
	traceparentAttr = "traceparent"
	baggageAttr     = "baggage"
)

// sqsAttributeCarrier adapts SQS message attributes to OTel's
// TextMapCarrier interface. Used for both injection (publishing) and
// extraction (consuming).
type sqsAttributeCarrier struct {
	attrs map[string]string
}

// Get returns the value for the given key. Returns "" if not present.
func (c sqsAttributeCarrier) Get(key string) string {
	return c.attrs[key]
}

// Set stores a key/value pair.
func (c sqsAttributeCarrier) Set(key, value string) {
	c.attrs[key] = value
}

// Keys lists the keys in this carrier (for iteration).
func (c sqsAttributeCarrier) Keys() []string {
	keys := make([]string, 0, len(c.attrs))
	for k := range c.attrs {
		keys = append(keys, k)
	}
	return keys
}

// injectSQSContext writes the current ctx's trace context into the
// given SQS SendMessageInput's MessageAttributes. Safe to call from
// any ctx (will be a no-op if there's no active span).
//
// Use this in your publisher right before SendMessage.
//
//	var input sqs.SendMessageInput
//	queue.InjectTraceContext(ctx, &input)
//	_, err := client.SendMessage(ctx, &input)
func InjectTraceContext(ctx context.Context, input *sqs.SendMessageInput) {
	if input == nil {
		return
	}
	carrier := sqsAttributeCarrier{attrs: make(map[string]string)}
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	// Only set attributes we actually populated. An empty map would
	// still send a non-null MessageAttributes field which the SDK
	// validates, so short-circuit if there's nothing to carry.
	if len(carrier.attrs) == 0 {
		return
	}
	if input.MessageAttributes == nil {
		input.MessageAttributes = make(map[string]types.MessageAttributeValue)
	}
	for k, v := range carrier.attrs {
		// "String" is the only DataType SQS guarantees across all
		// clients; using Number/Binary risks JSON-encoding issues
		// in non-AWS brokers like ElasticMQ.
		input.MessageAttributes[k] = types.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(v),
		}
	}
}

// ExtractTraceContext pulls the trace context from an SQS message's
// attributes and returns a new ctx that has it. If the message has
// no `traceparent` attribute, the original ctx is returned unchanged
// and the resulting root span is a fresh root (not a child).
//
// Use this in your consumer right before invoking the handler:
//
//	ctx = queue.ExtractTraceContext(ctx, msg)
//	tracer.Start(ctx, "worker.handleMessage")
//
// Some local SQS emulators (ElasticMQ) don't preserve
// MessageAttributes through the roundtrip, so we fall back to
// the message body's `_traceparent` field if the attribute is
// missing. Production SQS (real AWS) preserves attributes, so
// the body fallback is a no-op there.
func ExtractTraceContext(ctx context.Context, msg types.Message) context.Context {
	carrier := sqsAttributeCarrier{attrs: make(map[string]string)}

	// First try: SQS message attributes (preferred, set by AWS SDK
	// clients that respect the SQS API).
	if len(msg.MessageAttributes) > 0 {
		for k, v := range msg.MessageAttributes {
			if v.StringValue != nil {
				carrier.attrs[k] = *v.StringValue
			}
		}
	}

	// Second try: message body `_traceparent` (set by some publishers
	// that want guaranteed delivery across all SQS-compatible brokers).
	// Look for a top-level `_traceparent` field in the JSON body.
	if carrier.attrs["traceparent"] == "" {
		if body := aws.ToString(msg.Body); body != "" {
			carrier.attrs["traceparent"] = extractTraceparentFromBody(body)
		}
	}

	if len(carrier.attrs) == 0 {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

// extractTraceparentFromBody parses the JSON message body looking
// for a top-level `_traceparent` field. Returns "" if not present
// or body isn't valid JSON. Doesn't fail loudly on parse errors —
// the worst case is a root span, which is the same as no propagation.
func extractTraceparentFromBody(body string) string {
	// Cheap and cheerful: avoid importing encoding/json by using a
	// simple string scan. The publisher embeds the field as
	// `"_traceparent":"00-...-...-01",` so a substring search is
	// reliable enough for the dev-time fallback.
	const key = `"_traceparent":"`
	i := strings.Index(body, key)
	if i < 0 {
		return ""
	}
	rest := body[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// traceContextFromContext returns the trace_id and span_id of the
// active span, or "" if there is none. Useful for logging the
// trace context when SQS attributes can't be set (e.g. calling
// SendMessageBatched which has limited attribute space).
func TraceContextFromContext(ctx context.Context) (traceID, spanID string) {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return "", ""
	}
	return sc.TraceID().String(), sc.SpanID().String()
}

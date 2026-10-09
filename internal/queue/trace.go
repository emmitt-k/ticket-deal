package queue

import (
	"context"
	"fmt"

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

	// Defensive debug: log what we injected so an operator can see
	// whether the trace context was actually picked up. Remove this
	// once SQS → worker propagation is verified end-to-end.
	if tid, sid := TraceContextFromContext(ctx); tid != "" {
		fmt.Printf("queue: inject trace_id=%s span_id=%s into SQS attrs=%v\n",
			tid, sid, carrier.attrs)
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
func ExtractTraceContext(ctx context.Context, msg types.Message) context.Context {
	if len(msg.MessageAttributes) == 0 {
		fmt.Printf("queue: extract message has no attributes\n")
		return ctx
	}
	carrier := sqsAttributeCarrier{attrs: make(map[string]string)}
	for k, v := range msg.MessageAttributes {
		if v.StringValue != nil {
			carrier.attrs[k] = *v.StringValue
		}
	}
	fmt.Printf("queue: extract from SQS attrs=%v\n", carrier.attrs)
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
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

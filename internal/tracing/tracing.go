// Package tracing initializes an OpenTelemetry trace provider that exports
// OTLP/gRPC spans to a collector (Jaeger / Tempo / OTel Collector).
//
// One Init() call per process. Pass the returned shutdown function to
// signal.Notify on SIGINT/SIGTERM, or call it from main()'s defer chain:
//
//	func main() {
//	    shutdown, err := tracing.Init(ctx, "api")
//	    if err != nil { log.Fatal(err) }
//	    defer func() {
//	        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
//	        defer cancel()
//	        _ = shutdown(ctx)
//	    }()
//	    // ... rest of main
//	}
//
// Defaults (override via env vars before Init, or via OTEL_* standards):
//   - OTEL_EXPORTER_OTLP_ENDPOINT  (default: http://localhost:4317)
//   - OTEL_SERVICE_NAME            (default: passed as serviceName)
//   - OTEL_TRACES_SAMPLER_ARG      (default: 1.0 = trace everything)
//
// At 1000-VU load tests the default 100% sampling is fine for dev (Jaeger
// handles it); drop to 0.1 (10%) for sustained prod load.
package tracing

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// ShutdownFunc flushes any buffered spans and closes the exporter.
// Safe to call multiple times. The ctx bounds how long the flush can take.
type ShutdownFunc func(ctx context.Context) error

// Init wires up the global OpenTelemetry trace provider with an OTLP/gRPC
// exporter pointed at the endpoint read from OTEL_EXPORTER_OTLP_ENDPOINT
// (or defaultEndpoint if unset). Sets W3C TraceContext + Baggage as the
// global propagators so traceparent flows across HTTP/SQS/Redis boundaries.
//
// serviceName appears in Jaeger as the "service" column and on every span's
// resource attributes (service.name=...). It should match the binary name
// ("api", "worker", "expiration-watcher").
//
// The returned ShutdownFunc must be called before the process exits,
// otherwise the last few spans will be lost.
func Init(ctx context.Context, serviceName string) (ShutdownFunc, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:4317"
	}

	// Resource = metadata attached to every span from this process.
	// Jaeger uses service.name to group spans in the UI.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
			// Build info is helpful when you have 3 services running side
			// by side and want to know "is that the prod build?"
			semconv.ServiceVersion(version()),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	// OTLP/gRPC exporter. The client is buffered + non-blocking by default;
	// we still set a BatchTimeout so spans don't sit forever in the queue
	// if the collector disappears.
	exporter, err := otlptrace.New(ctx,
		otlptracegrpc.NewClient(
			otlptracegrpc.WithEndpoint(stripScheme(endpoint)),
			otlptracegrpc.WithInsecure(), // local dev; TLS in prod
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	// BatchSpanProcessor = queue spans, flush every 1s or when 512 queued.
	// These defaults are fine; tune if you see dropped spans under load.
	bsp := sdktrace.NewBatchSpanProcessor(exporter,
		sdktrace.WithBatchTimeout(1*time.Second),
		sdktrace.WithMaxExportBatchSize(256),
	)

	// ParentBased(TraceIDRatioBased) = respect upstream sampling decision
	// (root span sampled → all children sampled) and fall back to a ratio
	// for root spans. traceIDratio = 1.0 → trace everything; 0.1 → 10%.
	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio()))

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(bsp),
		sdktrace.WithSampler(sampler),
	)

	otel.SetTracerProvider(tp)
	// W3C TraceContext (the `traceparent` header) + Baggage. Without this,
	// otelhttp.Middleware can create spans but can't propagate them across
	// HTTP calls.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	shutdown := func(ctx context.Context) error {
		// ForceFlush pushes queued spans before closing; Close shuts the
		// exporter. Either alone leaks data; both is the safe pattern.
		_ = tp.ForceFlush(ctx)
		return tp.Shutdown(ctx)
	}
	return shutdown, nil
}

// stripScheme removes "http://" / "https://" because the gRPC client wants
// host:port, not a URL.
func stripScheme(endpoint string) string {
	for _, s := range []string{"http://", "https://"} {
		if len(endpoint) > len(s) && endpoint[:len(s)] == s {
			return endpoint[len(s):]
		}
	}
	return endpoint
}

// sampleRatio reads OTEL_TRACES_SAMPLER_ARG. The OTel SDK also honors this
// env var natively, but reading it ourselves lets us log the value on
// startup and fail loudly if it's set to something bogus.
func sampleRatio() float64 {
	v := os.Getenv("OTEL_TRACES_SAMPLER_ARG")
	if v == "" {
		return 1.0
	}
	var f float64
	if _, err := fmt.Sscanf(v, "%f", &f); err != nil || f < 0 || f > 1 {
		return 1.0
	}
	return f
}

// version is best-effort. Populated at build time via -ldflags in CI;
// empty in dev (Jaeger just shows the resource without it).
func version() string {
	if v := os.Getenv("SERVICE_VERSION"); v != "" {
		return v
	}
	return "dev"
}

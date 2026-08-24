package telemetry

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

func SetupTracing(ctx context.Context, service string) (func(context.Context) error, error) {
	// the exporter to who we send the spans
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}
	// each span with its service name

	// NewSchemaless, not NewWithAttributes(semconv.SchemaURL, ...): resource.Default()
	// carries the SDK's own schema URL (1.43.0 as of otel v1.45.0). Merge refuses to
	// combine two different ones, so passing semconv v1.24.0's URL here returns
	// "conflicting Schema URL" and every service dies at startup.
	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(semconv.ServiceName(service)),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(0.5)), // sample ~50% of traces,// sample each trace can be store 
	) //
	otel.SetTracerProvider(tp)

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}

func Tracing(service string, next http.Handler) http.Handler {
	// Tracing(service, next) wraps the whole server. It creates the server span
	// for each request and reads the incoming traceparent header, linking the
	// span to the caller's trace for distributed tracing.
	return otelhttp.NewHandler(next, service,
		// Skip Prometheus scrapes so /metrics does not create thousands of junk spans.
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/metrics"
		}),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
	)
}
//maybe change to be more precise
func RouteSpanName() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				span := trace.SpanFromContext(r.Context())
				rc := chi.RouteContext(r.Context())
				//if error name HTTP GET for exemple instead of GET /something-random-123456
				if rc == nil || rc.RoutePattern() == "" {
					span.SetName("HTTP " + r.Method)
					return
				}
				span.SetName(r.Method + " " + rc.RoutePattern())
			}()

			next.ServeHTTP(w, r)
		})
	}
}

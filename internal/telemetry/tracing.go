package telemetry

import (
	"context"
	"net/http"

	"github.com/go-chi/chi"
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

	res, err := resource.Merge(resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(service)),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
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

func RouteSpanName() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rc := chi.RouteContext(r.Context())
				if rc == nil || rc.RoutePattern() == "" {
					return
				}
				trace.SpanFromContext(r.Context()).SetName(r.Method + " " + rc.RoutePattern())
			}()

			next.ServeHTTP(w, r)
		})
	}
}

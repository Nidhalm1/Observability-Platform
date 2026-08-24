// Package telemetry holds the HTTP instrumentation shared by all three
// services.
//
// It lives here rather than under services/ because each service is its own
// `package main` -- a shared importable package is the only way all three can
// emit identically-shaped series. `internal/` keeps it private to this module.
package telemetry

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel/trace"
)

var (
	httpRequests = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests.",
		},
		[]string{"service", "method", "route", "status"},
	)

	// No `status` label here, deliberately. Errors are usually fast -- a 400
	// returns immediately -- so mixing them into the latency distribution skews
	// it. Rate and Errors come from the counter above; Duration comes from here.
	httpDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "http_request_duration_seconds",
			Help: "HTTP request latency.",
			// .2 and .3 are not in the Prometheus defaults. They are here so a
			// future SLO ("99% of /orders under 300ms") can be measured as a
			// ratio of bucket counts rather than an interpolated quantile.
			// Changing these boundaries later invalidates the history.
			Buckets: []float64{.005, .01, .025, .05, .1, .2, .3, .5, 1, 2.5, 5, 10},
		},
		[]string{"service", "method", "route"},
	)
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Metrics returns a chi-compatible middleware.
func Metrics(service string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// don't measure the scrape endpoint itself
			if r.URL.Path == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			// defer, so a panic in the handler still records the request
			defer func() {
				// The route PATTERN, never r.URL.Path. Raw paths mean
				// /orders/1, /orders/2, /orders/3 each become their own time
				// series -- the cardinality explosion.
				route := "unmatched" // 404s have no pattern; do not label them
				if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
					route = rc.RoutePattern()
				}
				httpRequests.WithLabelValues(
					service, r.Method, route, strconv.Itoa(rec.status),
				).Inc()

				elapsed := time.Since(start).Seconds()
				obs := httpDuration.WithLabelValues(service, r.Method, route)
				// get trace id from this request
				sc := trace.SpanContextFromContext(r.Context())
				if o, ok := obs.(prometheus.ExemplarObserver); ok && sc.IsSampled() {
					o.ObserveWithExemplar(elapsed, prometheus.Labels{
						"trace_id": sc.TraceID().String(),
					})
				} else {
					obs.Observe(elapsed)
				}
			}()

			next.ServeHTTP(rec, r)
		})
	}
}

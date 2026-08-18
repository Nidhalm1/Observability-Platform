package services

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	httpRequests = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests.",
		},
		[]string{"service", "method", "route", "status"},
	)
	httpDuration = promauto.NewHistogramVec(
    prometheus.HistogramOpts{
        Name:    "http_request_duration_seconds",
        Help:    "HTTP request latency.",
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
			rec := &statusRecorder{ResponseWriter: w, status: 200}

			// defer, so a panic in the handler still records the request
			defer func() {
				route := chi.RouteContext(r.Context()).RoutePattern()
				if route == "" {
					route = "unmatched" // 404s: never use the raw path
				}
				httpRequests.WithLabelValues(
					service, r.Method, route, strconv.Itoa(rec.status),
				).Inc()
				httpDuration.WithLabelValues(
					service, r.Method, route,
				).Observe(time.Since(start).Seconds())
			}()

			next.ServeHTTP(rec, r)
		})
	}
}

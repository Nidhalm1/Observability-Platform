// Package telemetry holds the HTTP instrumentation shared by all three
// services.
//
// It lives here rather than under services/ because each service is its own
// `package main` -- a shared importable package is the only way all three can
// emit identically-shaped series. `internal/` keeps it private to this module.
package telemetry

import (
	"database/sql"
	"net/http"
	"runtime"
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

func DBPoolMetrics(service string, db *sql.DB) error {
	labels := prometheus.Labels{"service": service}

	// Point-in-time levels. max_open is included so saturation can be expressed
	// as a ratio -- db_pool_in_use / db_pool_max_open -- without hardcoding the
	// limit into every dashboard query.
	gauges := []struct { // gauge means can go up and down 
		name, help string
		val        func(sql.DBStats) float64
	}{
		{"db_pool_max_open", "Connection limit, from DB_MAX_OPEN_CONNS.",
			func(st sql.DBStats) float64 { return float64(st.MaxOpenConnections) }}, // return the maximum allowed connecxions
		{"db_pool_open", "Connections currently established, in use or idle.",
			func(st sql.DBStats) float64 { return float64(st.OpenConnections) }}, // number of connection established
		{"db_pool_in_use", "Connections currently executing a query.",
			func(st sql.DBStats) float64 { return float64(st.InUse) }}, // how many connxion are used 
		{"db_pool_idle", "Connections currently idle.",
			func(st sql.DBStats) float64 { return float64(st.Idle) }}, // how many established conxions are free
	}
	for _, g := range gauges {
		c := prometheus.NewGaugeFunc(
			// When Prometheus requests /metrics, the function gets: db.Stats() and then g.val(...)
			prometheus.GaugeOpts{Name: g.name, Help: g.help, ConstLabels: labels}, // the name ect of metrics
			func() float64 { return g.val(db.Stats()) },//the value of the metric //get /metrics will excute this function 
		)
		if err := prometheus.Register(c); err != nil {
			return err
		}
	}

// the same to count Total number of waits for a connection and Total time blocked waiting for a connection.
	counters := []struct {
		name, help string
		val        func(sql.DBStats) float64
	}{
		{"db_pool_wait_count_total", "Total number of waits for a connection.",
			func(st sql.DBStats) float64 { return float64(st.WaitCount) }},
		{"db_pool_wait_seconds_total", "Total time blocked waiting for a connection.",
			func(st sql.DBStats) float64 { return st.WaitDuration.Seconds() }},
	}
	for _, c := range counters {
		col := prometheus.NewCounterFunc(
			prometheus.CounterOpts{Name: c.name, Help: c.help, ConstLabels: labels},
			func() float64 { return c.val(db.Stats()) }, //func allo us to get the current value
		)
		if err := prometheus.Register(col); err != nil {
			return err
		}
	}

	return nil
}

// how many goroutines exist if the number is up or down each request = goroutine
func GoroutineMetrics(service string) error {
	return prometheus.Register(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name:        "go_goroutines_custom",
			Help:        "Goroutines that currently exist, labelled by service.",
			ConstLabels: prometheus.Labels{"service": service},
		},
		// Evaluated at scrape time, like the pool gauges: a request parked on a
		// slow dependency is a goroutine, so a client with no timeout shows up
		// here as a ramp that does not come back down.
		func() float64 { return float64(runtime.NumGoroutine()) },
	))
}

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
				//selecting histogram
				obs := httpDuration.WithLabelValues(service, r.Method, route)
				// get trace id , and span id from this request from my new created span  but only use trace id

				sc := trace.SpanContextFromContext(r.Context())
				// can he store exemplars ? and is this particul trace is selected to be stored 
				if o, ok := obs.(prometheus.ExemplarObserver); ok && sc.IsSampled() {
					o.ObserveWithExemplar(elapsed, prometheus.Labels{
						//if yes store in this histogram  + trace id
						"trace_id": sc.TraceID().String(),
					})
				} else {
					//if no only in histogram
					obs.Observe(elapsed)
				}
			}()

			next.ServeHTTP(rec, r)
		})
	}
}

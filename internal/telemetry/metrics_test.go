package telemetry

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The cardinality rule, encoded. /orders/1, /orders/2, /orders/3 must all land
// on the single series route="/orders/{id}". If this test ever fails, the
// middleware is labelling with r.URL.Path and Prometheus will grow one series
// per order id.
func TestMetricsLabelsWithRoutePatternNotRawPath(t *testing.T) {
	r := chi.NewRouter()
	r.Use(Metrics("test"))
	r.Get("/orders/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/orders/8812")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	if got := testutil.ToFloat64(
		httpRequests.WithLabelValues("test", "GET", "/orders/{id}", "200"),
	); got != 1 {
		t.Errorf(`counter for route="/orders/{id}" = %v, want 1`, got)
	}

	if got := testutil.ToFloat64(
		httpRequests.WithLabelValues("test", "GET", "/orders/8812", "200"),
	); got != 0 {
		t.Errorf("a series exists for the raw path /orders/8812: cardinality explosion")
	}
}

// A 404 has no route pattern. Labelling it with the requested path would let
// any caller create unbounded series just by making up URLs.
func TestMetricsLabelsUnmatchedRoutesAsUnmatched(t *testing.T) {
	r := chi.NewRouter()
	r.Use(Metrics("test404"))
	r.Get("/orders/{id}", func(w http.ResponseWriter, _ *http.Request) {})

	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/no/such/path")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	if got := testutil.ToFloat64(
		httpRequests.WithLabelValues("test404", "GET", "unmatched", "404"),
	); got != 1 {
		t.Errorf(`counter for route="unmatched" = %v, want 1`, got)
	}
}

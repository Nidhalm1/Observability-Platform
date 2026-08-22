package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Nidhalm1/Observability-Platform/internal/telemetry"
)

type Item struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

type OrderRequest struct {
	CustomerID int    `json:"customer_id"`
	Items      []Item `json:"items"`
}

type server struct {
	ordersURL string
	client    *http.Client
	logger    *slog.Logger
}

func main() {
	logger := telemetry.SetupLogger("gateway")

	ordersURL := os.Getenv("ORDERS_SERVICE_URL")
	if ordersURL == "" {
		// Fail at startup, not per-request. An empty URL turns every call into
		// a confusing 500 that looks like an orders bug. slog has no Fatal, so
		// the exit is explicit.
		logger.Error("ORDERS_SERVICE_URL is not set")
		os.Exit(1)
	}

	s := &server{
		ordersURL: ordersURL,
		logger:    logger,
		// http.DefaultClient has NO timeout -- it waits forever, so a slow
		// orders service takes the gateway down with it.
		client: &http.Client{Timeout: 3 * time.Second},
	}

	r := chi.NewRouter()
	r.Use(telemetry.Metrics("gateway"))
	r.Post("/orders", s.createOrder)
	r.Get("/orders/{id}", s.getOrder)
	r.Handle("/metrics", promhttp.Handler())

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	logger.Info("gateway listening", "port", port, "orders_url", ordersURL)
	// listen on all interfaces, so the container is reachable from outside
	if err := http.ListenAndServe(":"+port, r); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func (s *server) createOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var order OrderRequest
	if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
		s.logger.Warn("invalid JSON", "error", err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if !verify(order) {
		s.logger.Warn("invalid order", "customer_id", order.CustomerID, "items", len(order.Items))
		http.Error(w, "invalid order", http.StatusBadRequest)
		return
	}

	body, err := json.Marshal(order)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		s.ordersURL+"/orders",
		bytes.NewReader(body),
	)
	if err != nil {
		http.Error(w, "request error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	s.forward(w, req)
}

func (s *server) getOrder(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		// Validate before forwarding: keeps junk out of the downstream service
		// and gives the caller a 400 instead of a 404 from two hops away.
		s.logger.Warn("invalid order id", "id", id)
		http.Error(w, "invalid order id", http.StatusBadRequest)
		return
	}

	req, err := http.NewRequestWithContext(
		r.Context(),
		http.MethodGet,
		s.ordersURL+"/orders/"+id,
		nil,
	)
	if err != nil {
		http.Error(w, "request error", http.StatusInternalServerError)
		return
	}

	s.forward(w, req)
}

// forward sends req to the orders service and copies the response back.
//
// Every request built in this file uses http.NewRequestWithContext with the
// request-scoped ctx. That is not cosmetic: in Phase 5 the context is what
// carries the trace ID into the `traceparent` header. Using context.Background()
// here compiles, works, and produces orphaned single-span traces.
func (s *server) forward(w http.ResponseWriter, req *http.Request) {
	resp, err := s.client.Do(req)
	if err != nil {
		s.logger.Error("call orders failed", "url", req.URL.String(), "error", err)
		http.Error(w, "orders service unavailable", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close() // AFTER the err check: on error resp is nil -> panic

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func verify(order OrderRequest) bool {
	if order.CustomerID <= 0 {
		return false
	}
	if len(order.Items) == 0 {
		return false
	}
	for _, item := range order.Items {
		if item.SKU == "" || item.Qty <= 0 {
			return false
		}
	}
	return true
}

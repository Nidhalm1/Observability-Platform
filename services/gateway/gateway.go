package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

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
	exitCode := 0
	//garantee to us to flutsh before quit
	defer func() { os.Exit(exitCode) }()

	logger := telemetry.SetupLogger("gateway")
	//when he get a signal from the contain abort and go to ctx.done()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ordersURL := os.Getenv("ORDERS_SERVICE_URL")
	if ordersURL == "" {
		// Fail at startup, not per-request. An empty URL turns every call into
		// a confusing 500 that looks like an orders bug. slog has no Fatal, so
		// the exit is explicit.
		logger.Error("ORDERS_SERVICE_URL is not set")
		exitCode = 1
		return
	}

	shutdownTracing, err := telemetry.SetupTracing(ctx, "gateway")
	if err != nil {
		logger.Error("tracing setup failed", "error", err)
		exitCode = 1
		return
	}
	//schedule when main ends to flush the traces and shutdown the tracer provider
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(flushCtx); err != nil {
			logger.Error("tracing shutdown failed", "error", err)
		}
	}()

	s := &server{
		ordersURL: ordersURL,
		logger:    logger,
		// http.DefaultClient has NO timeout -- it waits forever, so a slow
		// orders service takes the gateway down with it.
		client: &http.Client{
			Timeout:   3 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport), //to send traceparent header to the orders service
		},
	}

	r := chi.NewRouter()
	r.Use(telemetry.Metrics("gateway"))
	r.Use(telemetry.RouteSpanName()) // cretae same sapn name for the same method
	r.Post("/orders", s.createOrder)
	r.Get("/orders/{id}", s.getOrder)
	r.Handle("/metrics", promhttp.Handler())

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{Addr: ":" + port, Handler: telemetry.Tracing("gateway", r)}

	serveErr := make(chan error, 1)
	go func() {
		// ErrServerClosed is what Shutdown returns on the way out. It is the
		// success case, not a failure.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	logger.Info("gateway listening", "port", port, "orders_url", ordersURL)

	select {
	case err := <-serveErr:
		logger.Error("server stopped", "error", err)
		exitCode = 1
		return
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}
	//contex.background because maybe the firsto contexct gets direct buy sigterm
	//wait for handle to finish and shutdown the server
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		exitCode = 1
	}
}

func (s *server) createOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var order OrderRequest
	if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
		telemetry.LogWith(ctx).Warn("invalid JSON", "error", err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if !verify(order) {
		telemetry.LogWith(ctx).Warn("invalid order", "customer_id", order.CustomerID, "items", len(order.Items))
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
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		// Validate before forwarding: keeps junk out of the downstream service
		// and gives the caller a 400 instead of a 404 from two hops away.
		telemetry.LogWith(ctx).Warn("invalid order id", "id", id)
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

func (s *server) forward(w http.ResponseWriter, req *http.Request) {
	resp, err := s.client.Do(req)
	if err != nil {
		telemetry.LogWith(req.Context()).Error("call orders failed", "url", req.URL.String(), "error", err)
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

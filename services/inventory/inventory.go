package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Nidhalm1/Observability-Platform/internal/telemetry"
)

// A DB call with no deadline inherits only the client's patience. Bound it here
// so a slow query surfaces as a fast error instead of a hung goroutine.
const dbTimeout = 2 * time.Second

type server struct {
	// db connection pool
	pool   *pgxpool.Pool
	logger *slog.Logger
}

type Item struct {
	SKU   string `json:"sku"`
	Qty   int    `json:"qty"`
	Price int    `json:"price"`
}

type OrderRequest struct {
	CustomerID int    `json:"customer_id"`
	Items      []Item `json:"items"`
}

type StockResponse struct {
	SKU       string `json:"sku"`
	Warehouse string `json:"warehouse"`
	Quantity  int    `json:"quantity"`
	Reserved  int    `json:"reserved"`
	Price     int    `json:"unit_price_cents"`
}

func main() {
	ctx := context.Background()

	logger := telemetry.SetupLogger("inventory")

	// Fail at startup, not per-request: slog has no Fatal, so the exit is
	// explicit. os.Exit(1) keeps the container-restart behaviour log.Fatal had.
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		logger.Error("DATABASE_URL is not set")
		os.Exit(1)
	}

	s := &server{logger: logger}
	var err error
	s.pool, err = pgxpool.Connect(ctx, databaseURL)
	if err != nil {
		logger.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}

	r := chi.NewRouter()
	r.Use(telemetry.Metrics("inventory"))
	r.Post("/check", s.checkOrder)
	r.Get("/inventory/{sku}", s.getStock)
	r.Handle("/metrics", promhttp.Handler())

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	// The pool lives for the whole process. A `defer pool.Close()` here would be
	// dead code twice over: it sat after a blocking call, and log.Fatal exits via
	// os.Exit, which skips defers.
	logger.Info("inventory listening", "port", port)
	// listen on all interfaces, so the container is reachable from outside
	if err := http.ListenAndServe(":"+port, r); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// getStock is the read-only view of a SKU. Same unindexed WHERE as checkOrder,
// but without the write -- the cleanest endpoint to point k6 at when
// demonstrating Fault 1.
func (s *server) getStock(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	var out StockResponse
	// `WHERE sku = $1` stays unindexed on purpose: this is Fault 1.
	err := s.pool.QueryRow(ctx,
		`SELECT sku, warehouse, quantity, reserved, unit_price_cents
		   FROM inventory
		  WHERE sku = $1`,
		chi.URLParam(r, "sku"),
	).Scan(&out.SKU, &out.Warehouse, &out.Quantity, &out.Reserved, &out.Price)

	if errors.Is(err, pgx.ErrNoRows) {
		s.logger.Warn("unknown sku", "sku", chi.URLParam(r, "sku"))
		http.Error(w, "unknown sku", http.StatusNotFound)
		return
	}
	if err != nil {
		s.logger.Error("get stock failed", "sku", chi.URLParam(r, "sku"), "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *server) checkOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var order OrderRequest
	err := json.NewDecoder(r.Body).Decode(&order)
	if err != nil {
		s.logger.Warn("invalid JSON", "error", err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	for i, item := range order.Items {
		var granted int
		var price int

		// One atomic statement instead of SELECT-then-UPDATE.
		//
		// The old version read `reserved`, then wrote it in a second round trip:
		// two concurrent orders for the same SKU both read the same value and
		// both succeeded, so stock was oversold (lost update).
		//
		// The CTE takes a row lock and captures available-BEFORE, which RETURNING
		// can still read via `a` -- that is what makes the granted amount knowable
		// in a single trip. LEAST() handles partial fulfilment.
		//
		// `WHERE sku = $1` stays unindexed on purpose: this is Fault 1.
		dbCtx, cancel := context.WithTimeout(ctx, dbTimeout)
		err := s.pool.QueryRow(
			dbCtx,
			`WITH a AS (
                 SELECT id, quantity - reserved AS available, unit_price_cents
                 FROM inventory
                 WHERE sku = $1
                 FOR UPDATE
             )
             UPDATE inventory i
             SET reserved = i.reserved + LEAST($2, a.available)
             FROM a
             WHERE i.id = a.id
             RETURNING LEAST($2, a.available), a.unit_price_cents`,
			item.SKU,
			item.Qty,
		).Scan(&granted, &price)
		cancel()

		if err != nil {
			// no rows = unknown SKU; anything else = a real DB error. Both are
			// reported to the caller as -1, but only one of them is worth a log
			// line at ERROR once Phase 3 adds levels.
			s.logger.Error("reserve failed", "sku", item.SKU, "qty", item.Qty, "error", err)
			order.Items[i] = Item{SKU: item.SKU, Qty: -1, Price: 0}
			continue
		}

		// granted < item.Qty means partial. The old code `continue`d on this path
		// without writing order.Items[i], so the caller got its own requested
		// quantity back and believed the reservation was full.
		order.Items[i] = Item{
			SKU:   item.SKU,
			Qty:   granted,
			Price: price,
		}
	}

	s.sendOrder(w, order)
}

func (s *server) sendOrder(w http.ResponseWriter, order OrderRequest) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	json.NewEncoder(w).Encode(order)
}

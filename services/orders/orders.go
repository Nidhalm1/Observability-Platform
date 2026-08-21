package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/XSAM/otelsql"
	"github.com/go-chi/chi"

	// Registers the "pgx/v4" driver with database/sql. Imported for that side
	// effect alone -- nothing in this file calls into it directly.
	_ "github.com/jackc/pgx/v4/stdlib"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"

	"github.com/Nidhalm1/Observability-Platform/internal/telemetry"
)

// A DB call with no deadline inherits only the client's patience. Bound it here
// so a slow query surfaces as a fast error instead of a hung goroutine.
const dbTimeout = 2 * time.Second

type Item struct {
	SKU   string `json:"sku"`
	Qty   int    `json:"qty"`
	Price int    `json:"price"` // for the messages received from the inventory service
}

type OrderRequest struct {
	CustomerID int    `json:"customer_id"`
	Items      []Item `json:"items"`
}

type OrderResponse struct {
	ID         int64     `json:"id"`
	CustomerID int64     `json:"customer_id"`
	Status     string    `json:"status"`
	TotalCents int64     `json:"total_cents"`
	CreatedAt  time.Time `json:"created_at"`
	Items      []Item    `json:"items"`
}

type server struct {
	db           *sql.DB
	inventoryURL string
	client       *http.Client
	logger       *slog.Logger
}

func main() {
	exitCode := 0
	//garantee to us to flutsh before quit
	defer func() { os.Exit(exitCode) }()

	logger := telemetry.SetupLogger("orders")
	//when he get a signal from the contain er do not stop directrly
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		logger.Error("DATABASE_URL is not set")
		exitCode = 1
		return
	}
	inventoryURL := os.Getenv("INVENTORY_SERVICE_URL")
	if inventoryURL == "" {
		logger.Error("INVENTORY_SERVICE_URL is not set")
		exitCode = 1
		return
	}

	shutdownTracing, err := telemetry.SetupTracing(ctx, "orders")
	if err != nil {
		logger.Error("tracing setup failed", "error", err)
		exitCode = 1
		return
	}

	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(flushCtx); err != nil {
			logger.Error("tracing shutdown failed", "error", err)
		}
	}()

	s := &server{
		inventoryURL: inventoryURL,
		logger:       logger,
		client:       &http.Client{Timeout: 2 * time.Second},
	}

	s.db, err = otelsql.Open("pgx/v4", databaseURL,
		otelsql.WithAttributes(semconv.DBSystemPostgreSQL),
		otelsql.WithSpanOptions(otelsql.SpanOptions{
			DisableErrSkip:       true,
			OmitConnResetSession: true,
		}),
	)
	if err != nil {
		logger.Error("failed to open database", "error", err)
		exitCode = 1
		return
	}
	defer s.db.Close()

	s.db.SetMaxOpenConns(10)
	s.db.SetMaxIdleConns(10)
	s.db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	err = s.db.PingContext(pingCtx)
	cancelPing()
	if err != nil {
		logger.Error("failed to connect to database", "error", err)
		exitCode = 1
		return
	}

	r := chi.NewRouter()
	r.Use(telemetry.Metrics("orders"))
	r.Use(telemetry.RouteSpanName())
	r.Post("/orders", s.createOrder)
	r.Get("/orders/{id}", s.getOrder)
	r.Handle("/metrics", promhttp.Handler())

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{Addr: ":" + port, Handler: telemetry.Tracing("orders", r)}

	serveErr := make(chan error, 1)
	go func() {
		// ErrServerClosed is what Shutdown returns on the way out. It is the
		// success case, not a failure.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	logger.Info("orders listening", "port", port, "inventory_url", inventoryURL)

	select {
	case err := <-serveErr:
		logger.Error("server stopped", "error", err)
		exitCode = 1
		return
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		exitCode = 1
	}
}

func (s *server) getOrder(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid order id", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	var out OrderResponse
	// status::text because pgx cannot scan a user-defined enum OID into a
	// string on its own.
	err = s.db.QueryRowContext(ctx,
		`SELECT id, customer_id, status::text, total_cents, created_at
		   FROM orders
		  WHERE id = $1`,
		id,
	).Scan(&out.ID, &out.CustomerID, &out.Status, &out.TotalCents, &out.CreatedAt)

	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.logger.Error("get order failed", "order_id", id, "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT sku, qty, unit_price_cents
		   FROM order_items
		  WHERE order_id = $1
		  ORDER BY id`,
		id,
	)
	if err != nil {
		s.logger.Error("get items failed", "order_id", id, "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out.Items = []Item{} // not nil: encodes as [] rather than null
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.SKU, &it.Qty, &it.Price); err != nil {
			s.logger.Error("scan item failed", "order_id", id, "error", err)
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		out.Items = append(out.Items, it)
	}
	// rows.Err() reports failures that happen mid-iteration, which the loop
	// above cannot see. Skipping it silently truncates result sets.
	if err := rows.Err(); err != nil {
		s.logger.Error("iterate items failed", "order_id", id, "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *server) createOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var order OrderRequest
	err := json.NewDecoder(r.Body).Decode(&order)
	if err != nil {
		s.logger.Warn("invalid JSON", "error", err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	// The gateway validates too. Orders is directly reachable on its own port,
	// so it cannot assume it was called through the gateway.
	if order.CustomerID <= 0 || len(order.Items) == 0 {
		s.logger.Warn("invalid order", "customer_id", order.CustomerID, "items", len(order.Items))
		http.Error(w, "invalid order", http.StatusBadRequest)
		return
	}

	insCtx, cancel := context.WithTimeout(ctx, dbTimeout)
	var orderID int64
	var createdAt time.Time
	err = s.db.QueryRowContext(insCtx,
		`INSERT INTO orders (customer_id, status)
		 VALUES ($1, $2::order_status)
		 RETURNING id, created_at`,
		order.CustomerID, "pending",
	).Scan(&orderID, &createdAt)
	cancel()
	if err != nil {
		s.logger.Error("insert order failed", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	// --- ask inventory to reserve stock ---

	body, err := json.Marshal(order)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, s.inventoryURL+"/check", bytes.NewReader(body),
	)
	if err != nil {
		http.Error(w, "request error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		s.logger.Error("call inventory failed", "error", err)
		http.Error(w, "inventory service unavailable", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close() // AFTER the err check: on error resp is nil -> panic

	var inventoryResponse OrderRequest
	if err := json.NewDecoder(resp.Body).Decode(&inventoryResponse); err != nil {
		http.Error(w, "invalid inventory response", http.StatusInternalServerError)
		return
	}

	// --- reconcile what we asked for against what we got ---

	same := true
	var totalCents int64
	rows := make([][]any, 0, len(inventoryResponse.Items))

	for i, item := range inventoryResponse.Items {
		// bounds check: inventory is another service, its reply length is not
		// ours to trust
		if i >= len(order.Items) || order.Items[i].Qty != item.Qty {
			same = false
		}
		// Skip non-positive quantities: 0 = out of stock, -1 = inventory error.
		// order_items has CHECK (qty > 0), so copying these in fails the whole
		// CopyFrom and turns every out-of-stock SKU into a 500.
		if item.Qty <= 0 {
			continue
		}
		totalCents += int64(item.Qty) * int64(item.Price)
		rows = append(rows, []any{orderID, item.SKU, item.Qty, item.Price})
	}
	if len(inventoryResponse.Items) != len(order.Items) {
		same = false
	}

	status := "partially_confirmed"
	if same {
		status = "confirmed"
	} else if len(rows) == 0 {
		// Nothing could be reserved at all -- "partially confirmed" would be a
		// lie to anyone reading the table.
		status = "rejected"
	}

	// Underscore, not a space: the enum in 000001_init_schema.up.sql is
	// 'partially_confirmed'. A mismatch here is an invalid-input error from
	// Postgres, i.e. a 500 on every partial order.
	updCtx, cancel := context.WithTimeout(ctx, dbTimeout)
	_, err = s.db.ExecContext(updCtx,
		`UPDATE orders
		    SET status = $2::order_status, total_cents = $3, updated_at = now()
		  WHERE id = $1`,
		orderID, status, totalCents,
	)
	cancel()
	if err != nil {
		s.logger.Error("update order failed", "order_id", orderID, "error", err)
		http.Error(w, "failed to update order status", http.StatusInternalServerError)
		return
	}

	if len(rows) > 0 {
		var sb strings.Builder
		sb.WriteString(`INSERT INTO order_items (order_id, sku, qty, unit_price_cents) VALUES `)
		args := make([]any, 0, len(rows)*4)
		for i, row := range rows {
			if i > 0 {
				sb.WriteByte(',')
			}
			n := i * 4
			fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d)", n+1, n+2, n+3, n+4)
			args = append(args, row...)
		}

		insertCtx, cancel := context.WithTimeout(ctx, dbTimeout)
		_, err = s.db.ExecContext(insertCtx, sb.String(), args...)
		cancel()
		if err != nil {
			s.logger.Error("insert items failed", "order_id", orderID, "error", err)
			http.Error(w, "failed to insert order items", http.StatusInternalServerError)
			return
		}
	}

	out := OrderResponse{
		ID:         orderID,
		CustomerID: int64(order.CustomerID),
		Status:     status,
		TotalCents: totalCents,
		CreatedAt:  createdAt,
		Items:      inventoryResponse.Items,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(out)
}

//todo add a call when a client can cancel an order if not confirmed

package main

import (
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
	"syscall"
	"time"

	"github.com/XSAM/otelsql"
	"github.com/go-chi/chi/v5"

	_ "github.com/jackc/pgx/v4/stdlib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"

	"github.com/Nidhalm1/Observability-Platform/internal/faults"
	"github.com/Nidhalm1/Observability-Platform/internal/telemetry"
)

// A DB call with no deadline inherits only the client's patience. Bound it here
// so a slow query surfaces as a fast error instead of a hung goroutine.
const dbTimeout = 2 * time.Second

// skuQuery is a query filtering on inventory.sku, kept in both of its
// planner-visible forms so arming Fault 1 costs nothing per request.
type skuQuery struct{ indexed, seqScan string }

// newSKUQuery fills the %s in tmpl with each form of the SKU predicate.
func newSKUQuery(tmpl string) skuQuery {
	return skuQuery{
		// Matches idx_inventory_sku from migration 000004: Index Scan.
		indexed: fmt.Sprintf(tmpl, `sku = $1`),

		seqScan: fmt.Sprintf(tmpl, `sku || '' = $1`), // to avoid using index and check all rows
	}
}

// stmt picks the form matching the current fault state. faults.NoIndex reads an
// atomic: /admin/fault writes it from whichever goroutine served that request.
// Named stmt, not sql, so it does not read like the database/sql package.
func (q skuQuery) stmt() string {
	if faults.NoIndex() {
		return q.seqScan
	}
	return q.indexed
}

var (
	// GET /inventory/{sku} -- read-only, the cleanest endpoint to point k6 at.
	stockQuery = newSKUQuery(`SELECT sku, warehouse, quantity, reserved, unit_price_cents
		   FROM inventory
		  WHERE %s`)

	// POST /check -- the reservation path runs through the same lookup, so the
	// toggle degrades a whole POST /orders trace and not just the standalone
	// read. Without this one, flipping the fault does nothing to the main flow.
	reserveQuery = newSKUQuery(`WITH a AS (
                 SELECT id, quantity - reserved AS available, unit_price_cents
                 FROM inventory
                 WHERE %s
                 FOR UPDATE
             )
             UPDATE inventory i
             SET reserved = i.reserved + LEAST($2, a.available)
             FROM a
             WHERE i.id = a.id
             RETURNING LEAST($2, a.available), a.unit_price_cents`)
)

type server struct {
	db     *sql.DB
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
	exitCode := 0
	//garantee to us to flutsh before quit
	defer func() { os.Exit(exitCode) }()

	logger := telemetry.SetupLogger("inventory")
	//when he get a signal from the contain abort and go to ctx.done()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Fail at startup, not per-request: slog has no Fatal, so the exit is
	// explicit. The deferred os.Exit(1) keeps the container-restart behaviour.
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		logger.Error("DATABASE_URL is not set")
		exitCode = 1
		return
	}

	shutdownTracing, err := telemetry.SetupTracing(ctx, "inventory")
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

	s := &server{logger: logger}

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

	// Fault 3 (pool exhaustion) is set here, not on /admin/fault: the pool size

	maxConns := 10
	if v := os.Getenv("DB_MAX_OPEN_CONNS"); v != "" {
		// Junk is ignored rather than fatal. This knob exists to break the
		// service deliberately; a typo should not look like the fault.
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxConns = n
		} else {
			logger.Warn("ignoring invalid DB_MAX_OPEN_CONNS", "value", v)
		}
	}
	s.db.SetMaxOpenConns(maxConns)
	// Idle tracks max so connections are not torn down and rebuilt between
	// bursts -- otherwise the reconnect cost muddies the wait-time signal.
	s.db.SetMaxIdleConns(maxConns)
	s.db.SetConnMaxLifetime(30 * time.Minute)
	logger.Info("db pool configured", "max_open_conns", maxConns)

	if err := telemetry.DBPoolMetrics("inventory", s.db); err != nil {
		logger.Error("db pool metrics registration failed", "error", err)
		exitCode = 1
		return
	}

	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	err = s.db.PingContext(pingCtx)
	cancelPing()
	if err != nil {
		logger.Error("failed to connect to database", "error", err)
		exitCode = 1
		return
	}

	r := chi.NewRouter()
	r.Use(telemetry.Metrics("inventory"))
	r.Use(telemetry.RouteSpanName()) // cretae same sapn name for the same method
	r.Post("/check", s.checkOrder)
	r.Get("/inventory/{sku}", s.getStock)
	// Fault switchboard: POST /admin/fault?noindex=true|false. No auth -- this
	// is a demo rig, and the endpoint has no business existing in production.
	r.Post("/admin/fault", faults.Handler())
	// OpenMetrics: the classic text format has no exemplar syntax, so
	// exemplars would be dropped at serialization.
	r.Handle("/metrics", promhttp.HandlerFor(
		prometheus.DefaultGatherer,
		promhttp.HandlerOpts{EnableOpenMetrics: true},
	))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	// telemetry.Tracing("inventory", r) for linking spans , reading traceparent ect.

	// listen on all interfaces, so the container is reachable from outside
	srv := &http.Server{Addr: ":" + port, Handler: telemetry.Tracing("inventory", r)}

	serveErr := make(chan error, 1)
	go func() {
		// ErrServerClosed is what Shutdown returns on the way out. It is the
		// success case, not a failure.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	logger.Info("inventory listening", "port", port)

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

// getStock is the read-only view of a SKU. Same SKU lookup as checkOrder but
// without the write, which makes it the cleanest endpoint to point k6 at when
// demonstrating Fault 1: no row locks in the way of the measurement.
func (s *server) getStock(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	var out StockResponse
	// Index Scan normally, Seq Scan while Fault 1 is armed -- see skuQuery. The
	// otelsql span around this call is what makes the difference visible in a
	// trace rather than only in EXPLAIN.
	err := s.db.QueryRowContext(ctx,
		stockQuery.stmt(),
		chi.URLParam(r, "sku"),
	).Scan(&out.SKU, &out.Warehouse, &out.Quantity, &out.Reserved, &out.Price)

	if errors.Is(err, sql.ErrNoRows) {
		telemetry.LogWith(ctx).Warn("unknown sku", "sku", chi.URLParam(r, "sku"))
		http.Error(w, "unknown sku", http.StatusNotFound)
		return
	}
	if err != nil {
		telemetry.LogWith(ctx).Error("get stock failed", "sku", chi.URLParam(r, "sku"), "error", err)
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
		telemetry.LogWith(ctx).Warn("invalid JSON", "error", err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	for i, item := range order.Items {
		var granted int
		var price int
		dbCtx, cancel := context.WithTimeout(ctx, dbTimeout)
		err := s.db.QueryRowContext(
			dbCtx,
			reserveQuery.stmt(),
			item.SKU,
			item.Qty,
		).Scan(&granted, &price)
		cancel()

		if err != nil {
			// no rows = unknown SKU; anything else = a real DB error. Both are
			// reported to the caller as -1, but only one of them is worth a log
			// line at ERROR once Phase 3 adds levels.
			telemetry.LogWith(ctx).Error("reserve failed", "sku", item.SKU, "qty", item.Qty, "error", err)
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

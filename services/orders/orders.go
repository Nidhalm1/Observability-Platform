package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"

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
	// db connection pool
	pool         *pgxpool.Pool
	inventoryURL string
	client       *http.Client
}

func main() {
	ctx := context.Background()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is not set")
	}
	inventoryURL := os.Getenv("INVENTORY_SERVICE_URL")
	if inventoryURL == "" {
		log.Fatal("INVENTORY_SERVICE_URL is not set")
	}

	s := &server{
		inventoryURL: inventoryURL,
		// http.DefaultClient has NO timeout -- it waits forever, so a slow
		// inventory takes orders down with it. This is Fault 4; removing the
		// timeout must be a deliberate act, not the default.
		client: &http.Client{Timeout: 2 * time.Second},
	}

	var err error
	s.pool, err = pgxpool.Connect(ctx, databaseURL)
	if err != nil {
		log.Fatal(err)
	}

	r := chi.NewRouter()
	r.Use(telemetry.Metrics("orders"))
	r.Post("/orders", s.createOrder)
	r.Get("/orders/{id}", s.getOrder)
	r.Handle("/metrics", promhttp.Handler())

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	// The pool lives for the whole process. A `defer pool.Close()` here would be
	// dead code twice over: it sat after a blocking call, and log.Fatal exits via
	// os.Exit, which skips defers.
	log.Printf("orders listening on :%s, inventory at %s", port, inventoryURL)
	// listen on all interfaces, so the container is reachable from outside
	log.Fatal(http.ListenAndServe(":"+port, r))
}

// getOrder reads an order and its line items in TWO queries: one for the
// header, one for all items. This is the correct baseline. Fault 2 (N+1) is
// this same read done wrong -- a query per item -- and goes behind a flag in
// Phase 7, so keep this version intact to compare waterfalls against.
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
	err = s.pool.QueryRow(ctx,
		`SELECT id, customer_id, status::text, total_cents, created_at
		   FROM orders
		  WHERE id = $1`,
		id,
	).Scan(&out.ID, &out.CustomerID, &out.Status, &out.TotalCents, &out.CreatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("orders: get order %d: %v", id, err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	rows, err := s.pool.Query(ctx,
		`SELECT sku, qty, unit_price_cents
		   FROM order_items
		  WHERE order_id = $1
		  ORDER BY id`,
		id,
	)
	if err != nil {
		log.Printf("orders: get items %d: %v", id, err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out.Items = []Item{} // not nil: encodes as [] rather than null
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.SKU, &it.Qty, &it.Price); err != nil {
			log.Printf("orders: scan item %d: %v", id, err)
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		out.Items = append(out.Items, it)
	}
	// rows.Err() reports failures that happen mid-iteration, which the loop
	// above cannot see. Skipping it silently truncates result sets.
	if err := rows.Err(); err != nil {
		log.Printf("orders: iterate items %d: %v", id, err)
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
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	// The gateway validates too. Orders is directly reachable on its own port,
	// so it cannot assume it was called through the gateway.
	if order.CustomerID <= 0 || len(order.Items) == 0 {
		http.Error(w, "invalid order", http.StatusBadRequest)
		return
	}

	insCtx, cancel := context.WithTimeout(ctx, dbTimeout)
	var orderID int64
	var createdAt time.Time
	err = s.pool.QueryRow(insCtx,
		`INSERT INTO orders (customer_id, status)
		 VALUES ($1, $2::order_status)
		 RETURNING id, created_at`,
		order.CustomerID, "pending",
	).Scan(&orderID, &createdAt)
	cancel()
	if err != nil {
		log.Printf("orders: insert order: %v", err)
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
		log.Printf("orders: call inventory: %v", err)
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
	_, err = s.pool.Exec(updCtx,
		`UPDATE orders
		    SET status = $2::order_status, total_cents = $3, updated_at = now()
		  WHERE id = $1`,
		orderID, status, totalCents,
	)
	cancel()
	if err != nil {
		log.Printf("orders: update order %d: %v", orderID, err)
		http.Error(w, "failed to update order status", http.StatusInternalServerError)
		return
	}

	if len(rows) > 0 {
		copyCtx, cancel := context.WithTimeout(ctx, dbTimeout)
		_, err = s.pool.CopyFrom(
			copyCtx,
			pgx.Identifier{"order_items"},
			[]string{"order_id", "sku", "qty", "unit_price_cents"},
			pgx.CopyFromRows(rows),
		)
		cancel()
		if err != nil {
			log.Printf("orders: insert items for %d: %v", orderID, err)
			http.Error(w, "failed to insert order items", http.StatusInternalServerError)
			return
		}
	}

	// resp.Body is already drained by the decoder above, so io.Copy would send
	// nothing. Build the response from what we know instead -- and return the
	// id, so the caller can GET /orders/{id} straight afterwards.
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

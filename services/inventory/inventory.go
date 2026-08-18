package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/go-chi/chi"
	"github.com/jackc/pgx/v4/pgxpool"
)

type server struct {
	// db connection pool
	pool *pgxpool.Pool
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

func main() {
	// entry point
	// all of them listen same port ?
	ctx := context.Background()
	var databaseURL = os.Getenv("DATABASE_URL")
	server := &server{}
	var err error
	server.pool, err = pgxpool.Connect(ctx, databaseURL)
	if err != nil {
		log.Fatal(err)
	}

	r := chi.NewRouter()
	r.Use(Metrics("inventory"))  
	r.Post("/check", server.checkOrder)
	r.Handle("/metrics", promhttp.Handler())
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	// The pool lives for the whole process. A `defer pool.Close()` here would be
	// dead code twice over: it sat after a blocking call, and log.Fatal exits via
	// os.Exit, which skips defers.
	// listen on all interfaces, so the container is reachable from outside
	log.Fatal(http.ListenAndServe(":"+port, r))
}

func (s *server) checkOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var order OrderRequest
	err := json.NewDecoder(r.Body).Decode(&order)
	if err != nil {
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
		err := s.pool.QueryRow(
			ctx,
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

		if err != nil {
			// no rows = unknown SKU; anything else = real DB error
			log.Printf("inventory: reserve %s: %v", item.SKU, err)
			order.Items[i] = Item{item.SKU, -1, 0} // -1 for error
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

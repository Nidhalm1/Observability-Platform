package services

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
	r.Post("/check", server.checkOrder)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	http.ListenAndServe(":"+port, r) // listen all interrface , so in container we have specilai interface
	defer server.pool.Close()
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
		var quantity int
		var reserved int
		var price int

		err := s.pool.QueryRow(
			ctx,
			`SELECT quantity , reserved , unit_price_cents
         FROM inventory
         WHERE sku = $1`,
			item.SKU,
		).Scan(&quantity, &reserved, &price)

		if err != nil {
			order.Items[i] = Item{item.SKU, -1, 0} // -1 for error
		}
		available := quantity - reserved
		newStock := 0
		if available < item.Qty {
			newStock = available
			_, err = s.pool.Exec(
				ctx,
				`UPDATE inventory
             SET reserved = quantity
             WHERE sku = $1`,
				item.SKU,
			)
			if err != nil {
				order.Items[i] = Item{item.SKU, -1, 0}
			}
			continue
		} else {
			newStock = item.Qty
			_, err = s.pool.Exec(
				ctx,
				`UPDATE inventory
             SET reserved = reserved + $2
             WHERE sku = $1`,
				item.SKU,
				item.Qty,
			)
			if err != nil {
				order.Items[i] = Item{item.SKU, -1, 0}
			}
		}
		nItem := Item{
			SKU:   item.SKU,
			Qty:   newStock,
			Price: price,
		}
		order.Items[i] = nItem
	}
	s.sendOrder(w, order)

}
func (s *server) sendOrder(w http.ResponseWriter, order OrderRequest) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	json.NewEncoder(w).Encode(order)
}

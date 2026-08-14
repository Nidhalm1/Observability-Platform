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

type Item struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

type OrderRequest struct {
	CustomerID int    `json:"customer_id"`
	Items      []Item `json:"items"`
}
type server struct {
	// db connection pool
	pool *pgxpool.Pool
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
	r.Post("/orders", server.createOrder)
	r.Get("/orders/{id}", server.getOrder)
	http.ListenAndServe(":8080", r)
}

func (s *server) getOrder(w http.ResponseWriter, r *http.Request) {
	// code that handles GET /orders/{id}
	// respond with the order details for the given id
}

func (s *server) createOrder(w http.ResponseWriter, r *http.Request) {
	// code that handles POST /orders
	// repond  with the status of the request after
	// verify request is good
	// send the htpp to the good resever and wait for the repons
	//send the ctx with it
	// then thenrecipient  verifiy that trancpert ? headert ?
	ctx := r.Context()

	var order OrderRequest
	err := json.NewDecoder(r.Body).Decode(&order)
	if err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO orders (customer_id) VALUES ($1)`,
		order.CustomerID,
	)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}

	defer s.pool.Close()
}

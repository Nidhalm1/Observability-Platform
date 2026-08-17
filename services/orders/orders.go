package services

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/go-chi/chi"
	"github.com/jackc/pgx/v4/pgxpool"
)

type Item struct {
	SKU   string `json:"sku"`
	Qty   int    `json:"qty"`
	Price int    `json:"price"` // for the messages received from the inventory service
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
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	http.ListenAndServe(":"+port, r) // listen all interrface , so in container we have specilai interface
	defer server.pool.Close()
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
		`INSERT INTO orders (customer_id, status) VALUES ($1, $2)`,
		order.CustomerID, "pending",
	)
	// if dosnt exist in my table then

	//send inventory

	body, err := json.Marshal(order)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var inventoryServiceURL = os.Getenv("INVENTORY_SERVICE_URL")
	req, err := http.NewRequestWithContext(ctx, "POST", inventoryServiceURL+"/check", bytes.NewReader(body))
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "inventory service unavailable", 503)
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

//todo add a call when a client can cancel an order if note confirmed
//todo add a time out see the paper

//todo see if he answers mee the quantité of each item if its is available and add only those are available
//but he reponds me with the same struct with the real available quantity?
//update in the table if confiremed or partialy confirmed or not confirmed and send the status to the client

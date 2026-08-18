package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi"
)

// should the server keep a list of order no i think because the order do
type Item struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

type OrderRequest struct {
	CustomerID int    `json:"customer_id"`
	Items      []Item `json:"items"`
}

// http.DefaultClient has NO timeout -- it waits forever, so a slow orders
// service takes the gateway down with it.
var ordersClient = &http.Client{Timeout: 3 * time.Second}

func main() {
	// entry point
	r := chi.NewRouter()

	r := chi.NewRouter()
	r.Use(Metrics("orders"))  
	r.Post("/orders", server.createOrder)
	r.Get("/orders/{id}", server.getOrder)
	r.Handle("/metrics", promhttp.Handler())
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Fatal(http.ListenAndServe(":"+port, r)) // todo: change the port for the container
}
func getOrder(w http.ResponseWriter, r *http.Request) {
	// code that handles GET /orders/{id}
	// respond with the order details for the given id
}

func createOrder(w http.ResponseWriter, r *http.Request) {
	// code that handles POST /orders
	// repond  with the status of the request after
	ctx := r.Context()
	// verify request is good
	// send the htpp to the good resever and wait for the repons
	//send the ctx with it
	// then thenrecipient  verifiy that trancpert ? headert ?
	var order OrderRequest
	err := json.NewDecoder(r.Body).Decode(&order)
	if err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if !verfiy(order) {
		http.Error(w, "invalid order", http.StatusBadRequest)
		return
	}
	// but if the port changes
	body, err := json.Marshal(order)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var ordersServiceURL = os.Getenv("ORDERS_SERVICE_URL")
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		ordersServiceURL+"/orders",
		bytes.NewReader(body),
	)
	if err != nil {
		http.Error(w, "request error", 500)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ordersClient.Do(req)
	if err != nil {
		http.Error(w, "orders service unavailable", 503)
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)

}

// i want to add many traces sah
func verfiy(order OrderRequest) bool {
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

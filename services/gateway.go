package services

import (
	"encoding/json"
	"net/http"
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

func main() {
	// entry point
	http.ListenAndServe(":8080", nil)
	http.HandleFunc("/orders", createOrder)
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
	verfiy(order)
}

// i want to add many traces sah
func verfiy(order OrderRequest) bool {

	return true
}

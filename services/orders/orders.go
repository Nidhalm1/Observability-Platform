package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi"
	"github.com/jackc/pgx/v4"
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

// http.DefaultClient has NO timeout -- it waits forever, so a slow inventory
// takes orders down with it. This is Fault 4; make it deliberate, not default.
var inventoryClient = &http.Client{Timeout: 2 * time.Second}

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
	r.Use(Metrics("orders"))  
	r.Post("/orders", server.createOrder)
	r.Get("/orders/{id}", server.getOrder)
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
	var orderID int
	err = s.pool.QueryRow(ctx,
    `INSERT INTO orders (customer_id, status)
     VALUES ($1, $2::order_status)
     RETURNING id`,
    order.CustomerID, "pending",
	).Scan(&orderID)
	// if dosnt exist in my table then
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
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
	resp, err := inventoryClient.Do(req)
	if err != nil {
		http.Error(w, "inventory service unavailable", 503)
		return
	}
	defer resp.Body.Close() // AFTER the err check: on error resp is nil -> panic
	var inventoryResponse OrderRequest
	err = json.NewDecoder(resp.Body).Decode(&inventoryResponse)
	if err != nil {
		http.Error(w, "invalid inventory response", http.StatusInternalServerError)
		return
	}
	
	var same bool
	same = true
	rows := make([][]any, 0, len(inventoryResponse.Items))
	for i, item := range inventoryResponse.Items {
		// bounds check: inventory is another service, its reply length is not ours to trust
		if i >= len(order.Items) || order.Items[i].Qty != item.Qty {
			same = false
		}
		// Skip non-positive quantities: 0 = out of stock, -1 = inventory error.
		// order_items has CHECK (qty > 0), so copying these in fails the whole
		// CopyFrom and turns every out-of-stock SKU into a 500.
		if item.Qty <= 0 {
			continue
		}
		rows = append(rows, []any{
			orderID,
			item.SKU,
			item.Qty,
			item.Price,
		})
	}
	if len(inventoryResponse.Items) != len(order.Items) {
		same = false
	}
	// Exec returns (CommandTag, error) -- two values.
	if same {
		_, err = s.pool.Exec(ctx,
			`UPDATE orders
			SET status = $2::order_status
			WHERE id = $1`,
			orderID, "confirmed",
		)
	} else {
		_, err = s.pool.Exec(ctx,
			`UPDATE orders
			SET status = $2::order_status
			WHERE id = $1`,
			orderID, "partially confirmed",
		)
	}
	if err != nil {
		http.Error(w, "failed to update order status", http.StatusInternalServerError)
		return
	}
	
	if len(rows) > 0 {
		_, err = s.pool.CopyFrom(
			ctx,
			pgx.Identifier{"order_items"},
			[]string{"order_id", "sku", "qty", "unit_price_cents"},
			pgx.CopyFromRows(rows),
		)

		if err != nil {
			http.Error(w, "failed to insert order items", http.StatusInternalServerError)
			return
		}
	}
	// resp.Body is already drained by the decoder above, so io.Copy would send
	// nothing. Encode the struct we decoded instead.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(inventoryResponse)
}

//todo add a call when a client can cancel an order if note confirmed
//todo add a time out see the paper

//todo see if he answers mee the quantité of each item if its is available and add only those are available
//but he reponds me with the same struct with the real available quantity?
//update in the table if confiremed or partialy confirmed or not confirmed and send the status to the client

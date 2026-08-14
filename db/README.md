# Database

## Apply

```sh
migrate -path db/migrations -database "$DATABASE_URL" up
```

Or plain psql, in order:

```sh
for f in db/migrations/*.up.sql; do psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$f"; done
```

## Tables

| Table         | Rows  | Notes                                              |
| ------------- | ----- | -------------------------------------------------- |
| `inventory`   | 200k  | `sku` **unindexed on purpose** — this is Fault 1     |
| `orders`      | 5k    | 500 customers, spread over 30 days                  |
| `order_items` | ~102k | 1–40 items per order — variable width drives Fault 2 |

Seed data is deterministic (modular arithmetic, no `random()`), so before/after
numbers are comparable across machines and reruns.

## Fault 1 — measure it

Capture both numbers for the README. Run each twice and take the second, so you
are comparing warm cache to warm cache rather than cold to warm.

```sh
psql "$DATABASE_URL" -c "EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM inventory WHERE sku = 'ABC';"
```

Before: `Seq Scan on inventory`, ~200k rows scanned.
Then apply `db/fixes/001_add_inventory_sku_index.sql` and rerun.
After: `Index Scan using idx_inventory_sku`.

Note the seq scan reads every row regardless of where `ABC` sits in the heap —
`sku` is not unique, so Postgres has no way to stop at the first match.

## Queries the services need

```sql
-- inventory: GET /inventory/{sku}   <- Fault 1 lives here
SELECT id, sku, warehouse, quantity, reserved, unit_price_cents
FROM inventory WHERE sku = $1;

-- orders: POST /orders
INSERT INTO orders (customer_id, status) VALUES ($1, 'pending') RETURNING id, created_at;
INSERT INTO order_items (order_id, sku, qty, unit_price_cents) VALUES ($1, $2, $3, $4);

-- orders: GET /orders/{id}  -- the CORRECT version (one round trip)
SELECT id, order_id, sku, qty, unit_price_cents FROM order_items WHERE order_id = $1;

-- Fault 2 is the same read done wrong: SELECT the order, then loop over its
-- items issuing one query per item. Put it behind a flag, e.g. FAULT_N_PLUS_1=1,
-- so you can flip it live.
```

## Gotcha

There is no foreign key from `order_items.sku` to `inventory.sku` — a FK
requires a unique target, and making `sku` unique would create an index and
destroy Fault 1. The application validates the SKU instead.

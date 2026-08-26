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
| `inventory`   | 200k  | `sku` indexed by 000004; Fault 1 defeats it at runtime |
| `orders`      | 5k    | 500 customers, spread over 30 days                  |
| `order_items` | ~102k | 1–40 items per order — variable width drives Fault 2 |

Seed data is deterministic (modular arithmetic, no `random()`), so before/after
numbers are comparable across machines and reruns.

## Fault 1 — how it is armed

The index is real and always present (`000004_inventory_index`). The fault is in
the *query*: when it is armed, the inventory service filters on `sku || '' = $1`
instead of `sku = $1`. Same value, but the planner cannot match an expression
against a plain column index, so it falls back to a seq scan.

```sh
# arm it, then disarm it
curl -XPOST "http://localhost:5048/admin/fault?noindex=true"
curl -XPOST "http://localhost:5048/admin/fault?noindex=false"
```

Each service process holds its own flags, so arm it on the service you are
measuring. The response echoes the full current state.

## Fault 1 — measure it

Capture both numbers for the README. Run each twice and take the second, so you
are comparing warm cache to warm cache rather than cold to warm.

```sh
# broken
psql "$DATABASE_URL" -c "EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM inventory WHERE sku || '' = 'ABC';"
# fixed
psql "$DATABASE_URL" -c "EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM inventory WHERE sku = 'ABC';"
```

Before: `Seq Scan on inventory`, ~200k rows scanned.
After: `Index Scan using idx_inventory_sku`.

Note the seq scan reads every row regardless of where `ABC` sits in the heap —
`sku` is not unique, so Postgres has no way to stop at the first match.

If you would rather demonstrate the *real* repair — the one you would actually
ship — drop the index, measure, and recreate it:

```sh
psql "$DATABASE_URL" -c "DROP INDEX idx_inventory_sku;"
psql "$DATABASE_URL" -c "CREATE INDEX CONCURRENTLY idx_inventory_sku ON inventory (sku); ANALYZE inventory;"
```

`CONCURRENTLY` is what production wants: a plain `CREATE INDEX` takes an ACCESS
EXCLUSIVE lock and blocks writes for its duration. It cannot run inside a
transaction block, which is why 000004 does not use it.

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
-- items issuing one query per item. Flip it live with
-- POST /admin/fault?n1=true on the orders service.
```

## Gotcha

There is no foreign key from `order_items.sku` to `inventory.sku` — a FK
requires a unique target, and `sku` is not unique (the same SKU appears in
several warehouses). The application validates the SKU instead.

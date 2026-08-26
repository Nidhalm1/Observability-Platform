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
| `orders`      | 5,001 | 500 customers over 30 days, plus the wide order from 000005 |
| `order_items` | ~102k | 1–40 items per order, plus one with 50 — width drives Fault 2 |

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

## Fault 2 — how it is armed

`GET /orders/{id}` loads line items one of two ways. Armed, it runs a cheap
query for the item ids and then one round trip per id — same rows back, N+1
times the round trips.

```sh
curl -XPOST "http://localhost:5049/admin/fault?n1=true"
curl -XPOST "http://localhost:5049/admin/fault?n1=false"
```

Port 5049 is orders, not 5048 — each service holds its own flags.

## Fault 2 — measure it

The fault scales with item count, so compare a narrow order against a wide one
rather than looking at any single number:

```sh
# the 50-item order seeded by 000005
psql "$DATABASE_URL" -tAc "SELECT id FROM orders WHERE customer_id = 501;"

# a 2-item order for contrast (id % 40 = 1)
curl -s localhost:5049/orders/4961 | jq '.items | length'
```

Armed, the wide order draws 51 database spans in its trace waterfall against
the narrow order's 3. That ratio is the tell: a query that is merely slow shows
one fat span, an N+1 shows a staircase of thin ones.

Note `getOrder` bounds the whole handler at `dbTimeout` (2s). Under enough
concurrency the armed path will exhaust that and return 500 rather than a slow
200 — that is the fault getting worse, not a separate bug.

## Fault 3 — how it is armed

Pool size is fixed when the pool is built, so this one is an env var and a
restart, not an `/admin/fault` toggle:

```sh
cd deploy/compose
DB_MAX_OPEN_CONNS=3 docker compose up -d --force-recreate orders
docker compose up -d --force-recreate orders          # back to the default of 10
```

PowerShell wants `$env:DB_MAX_OPEN_CONNS=3` on its own line first. Note this is
*not* `docker compose up -e` — `up` has no `-e` flag; the value is interpolated
into the compose file from the shell environment.

## Fault 3 — measure it

Pool exhaustion is invisible in HTTP metrics alone: a request blocked waiting
for a free connection and a request running a slow query look identical from
the outside. These are the series that separate them:

```promql
# saturation -- how much of the pool is committed
db_pool_in_use / db_pool_max_open

# the actual tell: time spent queued for a connection
rate(db_pool_wait_seconds_total[1m])
rate(db_pool_wait_count_total[1m])
```

Put `rate(db_pool_wait_seconds_total[1m])` next to
`histogram_quantile(0.99, sum by (le, service, route) (rate(http_request_duration_seconds_bucket[5m])))`
on one graph. **Wait time climbing while query duration stays flat is the whole
lesson** — the database is fine, the queue in front of it is not. Utilization
tells you how busy something is; saturation tells you how much work is stuck
behind it, and only the second one explains the latency.

Arm Fault 2 at the same time for a much sharper demo: N+1 makes each request
cycle through the pool N times, so a pool of 3 falls over almost immediately.

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

-- Fault 2 is the same read done wrong: SELECT the item ids, then loop issuing
-- one query per id. Flip it live with POST /admin/fault?n1=true on orders.
SELECT sku, qty, unit_price_cents FROM order_items WHERE id = $1;  -- x N
```

## Gotcha

There is no foreign key from `order_items.sku` to `inventory.sku` — a FK
requires a unique target, and `sku` is not unique (the same SKU appears in
several warehouses). The application validates the SKU instead.

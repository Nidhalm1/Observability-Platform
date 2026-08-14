-- Seed 5,000 orders with a VARIABLE number of line items (1..40).
--
-- The variable width is the whole point. Fault 2 (N+1) has the signature
-- "latency scales with order size" -- if every order had the same item count,
-- the N+1 loop would look like a flat constant slowdown and you would have
-- nothing to correlate against. Pick order 4999 (40 items) vs order 4961
-- (2 items) for a side-by-side trace waterfall screenshot.

BEGIN;

INSERT INTO orders (customer_id, status, created_at)
SELECT
    1 + (g % 500),                                          -- 500 customers, so per-customer queries return real result sets
    (ARRAY['pending', 'confirmed', 'rejected', 'cancelled']::order_status[])[1 + (g % 4)],
    now() - (INTERVAL '1 minute' * (g % 43200))             -- spread over the last 30 days
FROM generate_series(1, 5000) AS g;

-- CROSS JOIN LATERAL gives each order its own item count.
-- Yields ~102k rows total.
INSERT INTO order_items (order_id, sku, qty, unit_price_cents)
SELECT
    o.id,
    'SKU-' || LPAD((1 + ((o.id * 7919 + i * 104729) % 200000))::text, 8, '0'),
    1 + (i % 5),
    199 + ((o.id + i) * 13) % 49800
FROM orders o
CROSS JOIN LATERAL generate_series(1, 1 + (o.id % 40)) AS i;

-- Keep the denormalised total consistent with the line items.
UPDATE orders o
SET total_cents = s.total
FROM (
    SELECT order_id, SUM(qty::BIGINT * unit_price_cents) AS total
    FROM order_items
    GROUP BY order_id
) s
WHERE o.id = s.order_id;

COMMIT;

ANALYZE orders;
ANALYZE order_items;

-- for fault 2 crete a customerid = 501 and gets its id then insert 50 items with order id = this id , 
-- to make fault2 works because if all custerm have only few itme than its will always be fast

BEGIN;

-- A data-modifying CTE: the INSERT's RETURNING feeds the item rows, so the new
-- order id never has to be hardcoded or read back in a second statement.
WITH o AS (
    INSERT INTO orders (customer_id, status, created_at)
    VALUES (501, 'confirmed', now() - INTERVAL '1 hour')
    RETURNING id
)
INSERT INTO order_items (order_id, sku, qty, unit_price_cents)
SELECT
    o.id,
    -- Same deterministic formula as 000003. 104729 is coprime with 200000, so
    -- i -> sku is injective and all 50 SKUs in this order are distinct.
    'SKU-' || LPAD((1 + ((o.id * 7919 + i * 104729) % 200000))::text, 8, '0'),
    1 + (i % 5),
    199 + ((o.id + i) * 13) % 49800
FROM o
CROSS JOIN generate_series(1, 50) AS i;

-- Keep the denormalised total consistent, the way 000003 does. Scoped to this
-- one order so a rerun cannot rewrite the other 5,000.
UPDATE orders o
SET total_cents = s.total
FROM (
    SELECT order_id, SUM(qty::BIGINT * unit_price_cents) AS total
    FROM order_items
    GROUP BY order_id
) s
WHERE o.id = s.order_id
  AND o.customer_id = 501;

COMMIT;

ANALYZE order_items;

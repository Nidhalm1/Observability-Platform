-- Seed 200k inventory rows.
--
-- Set-based insert via generate_series: one statement, ~2-4s. Do NOT generate
-- 200k individual INSERTs -- that takes minutes and bloats the repo.
--
-- All values are deterministic (modular arithmetic, not random()) so every
-- teammate and every CI run gets a byte-identical table. Your "before index /
-- after index" numbers are only comparable if the data is identical.

BEGIN;

INSERT INTO inventory (sku, warehouse, quantity, reserved, unit_price_cents)
SELECT
    'SKU-' || LPAD(g::text, 8, '0'),
    (ARRAY['EU-WEST-1', 'EU-CENTRAL-1', 'US-EAST-1', 'AP-SOUTH-1'])[1 + (g % 4)],
    CASE
        WHEN g % 97 = 0 THEN 0              -- ~2k out-of-stock SKUs, for the 409 path
        WHEN g % 31 = 0 THEN g % 5          -- ~6k low-stock SKUs, for partial fulfilment
        ELSE 50 + (g % 500)
    END,
    g % 7,
    199 + (g * 7) % 49800                   -- $1.99 .. $499.99
FROM generate_series(1, 200000) AS g;

-- Human-typeable SKUs for curl/k6. 'ABC' is the one in the guide's smoke test:
--   curl -X POST localhost:8080/orders -d '{"sku":"ABC","qty":2}'
INSERT INTO inventory (sku, warehouse, quantity, reserved, unit_price_cents) VALUES
    ('ABC',        'EU-WEST-1',    5000, 0,  1999),
    ('WIDGET-1',   'EU-WEST-1',    2500, 12, 4999),
    ('WIDGET-2',   'EU-CENTRAL-1',  800, 40, 12999),
    ('GADGET-PRO', 'US-EAST-1',     150, 3,  89999),
    ('OUT-OF-STOCK', 'EU-WEST-1',     0, 0,  2999);  -- deterministic 409 for error-path tests

COMMIT;

-- Without stats the planner guesses row counts and your EXPLAIN output is
-- meaningless. (ANALYZE is transaction-safe, unlike VACUUM -- it is outside the
-- COMMIT here only so it sees the committed rows.)
ANALYZE inventory;

-- FIX FOR FAULT 1 (missing index).
--
-- NOT a migration. Deliberately kept out of db/migrations/ so a fresh
-- `migrate up` always reproduces the broken state. Apply by hand during the
-- demo, after you have captured the "before" numbers.
--
--   psql "$DATABASE_URL" -f db/fixes/001_add_inventory_sku_index.sql
--
-- Roll back with:  DROP INDEX idx_inventory_sku;

-- CREATE INDEX takes an ACCESS EXCLUSIVE lock and blocks writes for its
-- duration. On 200k rows that is milliseconds, so it does not matter here --
-- but in production you would use CREATE INDEX CONCURRENTLY, which cannot run
-- inside a transaction block and takes roughly twice as long. Knowing that
-- distinction is worth saying out loud in the interview.
CREATE INDEX idx_inventory_sku ON inventory (sku);

ANALYZE inventory;

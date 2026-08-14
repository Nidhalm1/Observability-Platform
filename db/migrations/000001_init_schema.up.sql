-- Phase 1 schema: orders, order_items, inventory.
--
-- READ BEFORE CHANGING:
-- `inventory.sku` deliberately has NO index and NO UNIQUE constraint. That is
-- Fault 1 (missing index). Adding UNIQUE here makes Postgres create an index
-- automatically and silently destroys the fault. The surrogate `id` PK exists
-- purely so `sku` can stay unindexed.

BEGIN;

CREATE TYPE order_status AS ENUM ('pending', 'confirmed', 'rejected', 'cancelled');

CREATE TABLE inventory (
    id               BIGSERIAL   PRIMARY KEY,
    sku              TEXT        NOT NULL,
    warehouse        TEXT        NOT NULL,
    quantity         INTEGER     NOT NULL DEFAULT 0 CHECK (quantity >= 0),
    reserved         INTEGER     NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    unit_price_cents INTEGER     NOT NULL CHECK (unit_price_cents >= 0),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- NO INDEX ON inventory.sku -- see header. Fix lives in db/fixes/001_add_inventory_sku_index.sql

CREATE TABLE orders (
    id          BIGSERIAL    PRIMARY KEY,
    customer_id BIGINT       NOT NULL,
    status      order_status NOT NULL DEFAULT 'pending',
    total_cents BIGINT       NOT NULL DEFAULT 0 CHECK (total_cents >= 0),
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX idx_orders_customer_created ON orders (customer_id, created_at DESC);

CREATE TABLE order_items (
    id               BIGSERIAL PRIMARY KEY,
    order_id         BIGINT    NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    sku              TEXT      NOT NULL,
    qty              INTEGER   NOT NULL CHECK (qty > 0),
    unit_price_cents INTEGER   NOT NULL CHECK (unit_price_cents >= 0)
);

-- This index IS present on purpose. Fault 2 (N+1) must be about the number of
-- round trips, not about slow individual queries -- otherwise you cannot tell
-- the two faults apart in a trace waterfall.
CREATE INDEX idx_order_items_order_id ON order_items (order_id);

COMMIT;

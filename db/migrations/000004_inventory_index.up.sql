-- FAULT 1 (missing index), now toggled at runtime instead of at schema level.

CREATE INDEX idx_inventory_sku ON inventory (sku);

ANALYZE inventory;

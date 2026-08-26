.
CREATE INDEX idx_inventory_sku ON inventory (sku);

-- Without fresh stats the planner can keep choosing a seq scan for the first
-- queries after the index appears. ANALYZE is allowed inside a transaction
-- block (VACUUM is not), so it is safe to leave here.
ANALYZE inventory;

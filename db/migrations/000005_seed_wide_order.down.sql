-- order_items goes with it via ON DELETE CASCADE on order_items.order_id.
DELETE FROM orders WHERE customer_id = 501;

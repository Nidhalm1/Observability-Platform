-- order_items is removed by the CASCADE on orders.id.
TRUNCATE TABLE orders RESTART IDENTITY CASCADE;

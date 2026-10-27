-- A small shop for the stack suite to connect, sync and draft an ontology of.
CREATE TABLE customers (
  id serial PRIMARY KEY,
  email text NOT NULL UNIQUE,
  name text NOT NULL
);
CREATE TABLE products (
  id serial PRIMARY KEY,
  sku text NOT NULL UNIQUE,
  title text NOT NULL,
  price_cents integer NOT NULL
);
CREATE TABLE orders (
  id serial PRIMARY KEY,
  customer_id integer NOT NULL REFERENCES customers (id),
  placed_at timestamptz NOT NULL,
  status text NOT NULL
);
CREATE TABLE order_items (
  order_id integer NOT NULL REFERENCES orders (id),
  product_id integer NOT NULL REFERENCES products (id),
  quantity integer NOT NULL,
  PRIMARY KEY (order_id, product_id)
);
INSERT INTO customers (email, name) VALUES
  ('ada@example.com', 'Ada Lovelace'),
  ('grace@example.com', 'Grace Hopper'),
  ('alan@example.com', 'Alan Turing');
INSERT INTO products (sku, title, price_cents) VALUES
  ('KB-01', 'Mechanical keyboard', 12900),
  ('MS-02', 'Wireless mouse', 4900),
  ('MN-27', '27-inch monitor', 32900);
INSERT INTO orders (customer_id, placed_at, status) VALUES
  (1, '2026-09-01T10:00:00Z', 'shipped'),
  (2, '2026-09-03T15:30:00Z', 'shipped'),
  (1, '2026-09-20T09:12:00Z', 'pending'),
  (3, '2026-10-02T18:45:00Z', 'cancelled');
INSERT INTO order_items (order_id, product_id, quantity) VALUES
  (1, 1, 1), (1, 2, 1), (2, 3, 2), (3, 2, 3), (4, 1, 1);

-- The sample order-management schema. docs/schema.md explains each decision.
-- Run with: sentinel-cli < docs/schema.sql

CREATE TABLE customers (
    id      INT PRIMARY KEY,
    name    TEXT NOT NULL,
    address TEXT
);

CREATE TABLE accounts (
    id            INT PRIMARY KEY,
    customer_id   INT NOT NULL REFERENCES customers (id),
    balance_cents INT NOT NULL
);

CREATE TABLE products (
    id          INT PRIMARY KEY,
    name        TEXT NOT NULL,
    price_cents INT NOT NULL
);

CREATE TABLE orders (
    id          INT PRIMARY KEY,
    customer_id INT NOT NULL REFERENCES customers (id),
    status      TEXT NOT NULL
);

CREATE TABLE order_items (
    order_id         INT,
    product_id       INT,
    quantity         INT NOT NULL,
    unit_price_cents INT NOT NULL,
    PRIMARY KEY (order_id, product_id),
    FOREIGN KEY (order_id) REFERENCES orders (id),
    FOREIGN KEY (product_id) REFERENCES products (id)
);

INSERT INTO customers VALUES (1, 'Asha Rao', 'Bengaluru'), (2, 'Ravi Kumar', NULL), (42, 'Meera Iyer', 'Mysuru');
INSERT INTO accounts VALUES (1, 1, 100000), (2, 42, 50000);
INSERT INTO products VALUES (10, 'Fountain pen', 50000), (11, 'Notebook', 25000);
INSERT INTO orders VALUES (100, 42, 'open'), (101, 42, 'shipped'), (102, 1, 'open');
INSERT INTO order_items VALUES (100, 10, 3, 50000), (100, 11, 1, 25000), (102, 10, 1, 48000);

-- frl demo — bootstrap schema for cmd/frl/demo/seed.sql.
--
-- Creates a minimal one-table "orders" schema under database /FRL/demo,
-- schema main. Run ONCE before loading seed.sql:
--
--   frl sql --database /FRL/demo -f cmd/frl/demo/schema.sql
--
-- Not idempotent — each of these statements errors if the object
-- already exists. DROP + recreate manually if you need to reset.

CREATE DATABASE /FRL/demo;

CREATE SCHEMA TEMPLATE orders_tpl
CREATE TABLE orders (
  order_id BIGINT NOT NULL,
  customer STRING,
  price    DOUBLE,
  PRIMARY KEY (order_id)
);

CREATE SCHEMA /FRL/demo/main WITH TEMPLATE orders_tpl;

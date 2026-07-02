# frl demo — stand up a working cluster in 4 steps

A minimal end-to-end demo of `frl meta catalog`, `frl sql`, and layered
addressing against a real relational cluster. Bootstraps FDB in Docker,
creates one database / schema / template, seeds 1 000 rows into an
`orders` table, and walks through the commands worth trying first.

Everything here is demo data — `frl fdb down` when you're done.

## Prerequisites

- Docker (FoundationDB 7.3 image pulled on first run)
- `go` toolchain (binary isn't prebuilt)
- ~400 MB free RAM for the FDB container

## Setup

### 1. Start a single-node FDB cluster

One command — starts the container, configures it, writes and activates
a frl context (progress on stderr; stdout is the cluster-file path,
which you can also chain via `--cluster-file $(frl fdb up)`):

```sh
go run ./cmd/frl fdb up --name frl-demo --context frl-demo
```

Smoke check:

```sh
go run ./cmd/frl tx read-version       # prints the current GRV
```

### 2. Bootstrap the schema

```sh
go run ./cmd/frl sql --database /demo -f cmd/frl/demo/schema.sql
```

Creates `/demo`, template `orders_tpl` with one table `orders (order_id BIGINT,
customer STRING, price DOUBLE)`, and binds `/demo/main` to that template.

### 3. Load 1 000 rows

```sh
go run ./cmd/frl sql --database /demo --schema main -f cmd/frl/demo/seed.sql
```

Should print 10 × `OK (100 rows affected, …)` — the seed file is ten
INSERT batches of 100 rows each, deterministic (`srand(42)`), rerunnable
(the first line `DELETE FROM orders WHERE order_id >= 0` wipes prior state).

### 4. Poke around

```sh
# catalog discovery — no operator config needed beyond the cluster file
go run ./cmd/frl meta catalog databases
go run ./cmd/frl meta catalog schemas
go run ./cmd/frl meta catalog templates
go run ./cmd/frl meta catalog get orders_tpl         # full MetaData proto

# SQL one-shot
go run ./cmd/frl sql --database /demo --schema main \
  -c 'SELECT count(*) FROM orders'
go run ./cmd/frl sql --database /demo --schema main \
  -c 'SELECT order_id, customer, price FROM orders ORDER BY price DESC LIMIT 10'
go run ./cmd/frl sql --database /demo --schema main \
  -c 'SELECT customer, count(*) AS n, sum(price) AS total FROM orders GROUP BY customer'

# SQL interactive
go run ./cmd/frl sql --database /demo --schema main
# > \?              — help
# > \d              — list tables in current schema (via catalog)
# > \dt             — list templates (via SHOW SCHEMA TEMPLATES)
# > SELECT ...;     — multi-line; ends at `;`
# > BEGIN;          — tx — prompt gains `*`
# > INSERT ...;
# > ROLLBACK;       — or COMMIT
# > \q              — quit (also Ctrl-D)

# Layered addressing — the record-layer x-ray on the SAME store the SQL
# rows live in (keyspace + metadata resolved from the catalog):
go run ./cmd/frl record scan  --database /demo --schema main --limit 3
go run ./cmd/frl record get 1,1 --database /demo --schema main
go run ./cmd/frl store info   --database /demo --schema main
go run ./cmd/frl store dump   --database /demo --schema main --limit 20
```

## Tear down

```sh
go run ./cmd/frl fdb down --name frl-demo
```

## What's actually in here

- `schema.sql` — `CREATE DATABASE` + `CREATE SCHEMA TEMPLATE` + `CREATE
  SCHEMA`. Bootstrap only, non-idempotent (each statement errors if the
  object already exists).
- `seed.sql` — `DELETE` + 10 × 100-row `INSERT`s. Idempotent — safe to
  rerun.
- `README.md` — this file.

## Growing the dataset

`seed.sql` is generated, not hand-written. To produce a 10k-row version,
run the generator script in the commit that added this file (or:
open the file, it's 10 almost-identical `INSERT INTO orders VALUES (...)`
blocks; dup them).

## Sanity-check expectations after step 3

- `meta catalog databases` → shows `/__SYS` and `/demo`
- `meta catalog schemas` → shows `/__SYS/CATALOG` and `/demo/main`
- `meta catalog templates` → shows `CATALOG_TEMPLATE` and `orders_tpl`
- `SELECT count(*) FROM orders` → `1000`
- `SELECT customer, count(*) FROM orders GROUP BY customer` → 10 rows,
  one per customer name in the generator (alice/bob/…/judy), counts
  in the 90–120 range (deterministic from `srand(42)`)

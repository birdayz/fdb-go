# frl demo — stand up a working cluster in 5 steps

A minimal end-to-end demo of `frl meta catalog` and `frl sql` against a
real relational cluster. Bootstraps FDB in Docker, creates one
database / schema / template, seeds 1 000 rows into an `orders`
table, and walks through the commands worth trying first.

Everything here is read-only + demo data — nothing to roll back on the
cluster, just `docker rm -f` when you're done.

## Prerequisites

- Docker (FoundationDB 7.3 image pulled on first run)
- `go` toolchain (binary isn't prebuilt)
- ~400 MB free RAM for the FDB container

## Setup

### 1. Start a single-node FDB cluster

```sh
docker run -d --name frl-demo --network host foundationdb/foundationdb:7.3.77
sleep 3
docker exec frl-demo fdbcli --exec 'configure new single memory'
```

Wait a couple seconds until `docker exec frl-demo fdbcli --exec 'status minimal'`
says `The database is available.`

### 2. Grab the cluster file + point frl at it

```sh
mkdir -p /tmp/frl-demo
docker cp frl-demo:/var/fdb/fdb.cluster /tmp/frl-demo/fdb.cluster

cat > /tmp/frl-demo/config.yaml <<EOF
current_context: demo
contexts:
  - name: demo
    cluster_file: /tmp/frl-demo/fdb.cluster
    keyspace_path: /unused  # unused — meta catalog + sql target /__SYS/CATALOG
EOF

export FRL_CONFIG=/tmp/frl-demo/config.yaml
```

Smoke check:

```sh
go run ./cmd/frl tx read-version       # prints the current GRV
```

### 3. Bootstrap the schema

```sh
go run ./cmd/frl sql --database /demo -f cmd/frl/demo/schema.sql
```

Creates `/demo`, template `orders_tpl` with one table `orders (order_id BIGINT,
customer STRING, price DOUBLE)`, and binds `/demo/main` to that template.

### 4. Load 1 000 rows

```sh
go run ./cmd/frl sql --database /demo --schema main -f cmd/frl/demo/seed.sql
```

Should print 10 × `OK (100 rows affected, …)` — the seed file is ten
INSERT batches of 100 rows each, deterministic (`srand(42)`), rerunnable
(the first line `DELETE FROM orders WHERE order_id >= 0` wipes prior state).

### 5. Poke around

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
```

## Tear down

```sh
docker rm -f frl-demo
rm -rf /tmp/frl-demo
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

## Sanity-check expectations after step 4

- `meta catalog databases` → shows `/__SYS` and `/demo`
- `meta catalog schemas` → shows `/__SYS/CATALOG` and `/demo/main`
- `meta catalog templates` → shows `CATALOG_TEMPLATE` and `orders_tpl`
- `SELECT count(*) FROM orders` → `1000`
- `SELECT customer, count(*) FROM orders GROUP BY customer` → 10 rows,
  one per customer name in the generator (alice/bob/…/judy), counts
  in the 90–120 range (deterministic from `srand(42)`)

---
title: SQL and catalog discovery
description: "Run SQL scripts and an interactive shell, then inspect the same rows through Record Layer and catalog commands."
weight: 2
---

This walkthrough creates a SQL schema, loads 1,000 rows, and inspects the same
store through `fdb rl`. SQL uses metadata from the relational catalog. Do not
supply `--descriptor-set` or `--meta-file` for this workflow.

You need Docker, the Go version in
[`go.mod`](https://github.com/birdayz/fdb-go/blob/master/go.mod), and `jq` for the
primary-key extraction below. All data is disposable. Run the commands in the
same shell, from a source checkout.

## 1. Build the CLI and start FoundationDB

```sh
git clone https://github.com/birdayz/fdb-go.git
cd fdb-go
go build -o .tmpbuild/fdb ./cmd/fdb
export PATH="$PWD/.tmpbuild:$PATH"
fdb version

fdb dev up --name fdb-sql-demo --context fdb-sql-demo --port 4511
fdb tx read-version
```

If you already have a checkout, use it instead of cloning another copy. The
build uses that checkout's source, including unmerged local changes.

The named development instance uses port `4511` and activates the
`fdb-sql-demo` context. If that name or port is already in use, remove the old
instance or choose another name, context, and port. See
[local development commands](/docs/cli/reference/#local-development-database)
for the Docker and network requirements.

## 2. Create a database, template, and schema

The source checkout includes the complete
[schema](https://github.com/birdayz/fdb-go/blob/master/cmd/fdb/demo/schema.sql)
and [seed data](https://github.com/birdayz/fdb-go/blob/master/cmd/fdb/demo/seed.sql):

```sh
fdb sql --database /FRL/demo -f cmd/fdb/demo/schema.sql
fdb sql --database /FRL/demo --schema main -f cmd/fdb/demo/seed.sql
```

The schema creates database `/FRL/DEMO`, template `ORDERS_TPL`, and schema
`MAIN`, with one table:

```sql
CREATE TABLE orders (
  order_id BIGINT,
  customer STRING,
  price DOUBLE,
  PRIMARY KEY (order_id)
);
```

The seed file inserts 1,000 orders. Its first statement deletes the previous
seed rows, so you can rerun the seed. The schema file is **not** idempotent:
creating objects that already exist returns an error. Run it once against the
new development database.

Unquoted SQL identifiers fold to uppercase. Lowercase `--schema main` and
uppercase `--schema MAIN` select the same schema here; catalog output uses the
uppercase names.

## 3. Discover what the catalog contains

```sh
fdb rl meta catalog databases
fdb rl meta catalog schemas
fdb rl meta catalog templates
fdb rl meta catalog get ORDERS_TPL
```

The lists include `/FRL/DEMO`, `/FRL/DEMO/MAIN`, and `ORDERS_TPL`, as well as
the system catalog entries. `get` displays the template's metadata. These
commands discover catalog objects; they do not compile protobuf annotations.

## 4. Query rows

```sh
fdb sql --database /FRL/demo --schema main \
  -c 'SELECT count(*) FROM orders'
fdb sql --database /FRL/demo --schema main \
  -c 'SELECT order_id, customer, price FROM orders ORDER BY price DESC LIMIT 10'
fdb sql --database /FRL/demo --schema main \
  -c 'SELECT customer, count(*) AS n, sum(price) AS total FROM orders GROUP BY customer'
```

The count is `1000`. For machine-readable output, choose a SQL format explicitly:

```sh
fdb sql --database /FRL/demo --schema main -o json \
  -c 'SELECT order_id, customer, price FROM orders ORDER BY order_id LIMIT 3'
```

SQL supports `table`, `csv`, `json`, and `ndjson` output. In the machine formats,
the row-count/timing footer goes to stderr instead of contaminating stdout.
Use `-c` for a one-shot statement and `-f` for a script that stops on an error.
Redirected stdin is also a fail-fast SQL script, without an interactive banner
or prompts:

```sh
printf 'SELECT count(*) FROM orders;\n' | \
  fdb sql --database /FRL/demo --schema main -o csv
```

Backslash commands belong to the interactive shell, not redirected SQL scripts.
SQL and statistics commands can use `database` and `schema` defaults from a
relational context. An explicit `--database` starts a fresh address and does
not inherit the context's schema; pass `--schema` as well for these table
queries. A schema-only override keeps the context's database. See
[contexts and store addresses](/docs/cli/metadata/#contexts-and-store-addresses).

## 5. Use the interactive shell

```sh
fdb sql --database /FRL/demo --schema main
```

Inside the shell:

```text
\?
\d
\d orders
\dt
SELECT count(*) FROM orders;
BEGIN;
SELECT order_id FROM orders ORDER BY order_id LIMIT 1;
ROLLBACK;
\q
```

SQL statements end with `;`; backslash commands run immediately. `\d` lists
tables or describes one, and `\dt` lists schema templates. The prompt gains
`*` inside a transaction. Ctrl-D also exits. History is stored in
`~/.fdb/sql_history`.

SQL writes and DDL do not use the native Record Layer write-confirmation guards.
An `INSERT`, `DELETE`, or DDL statement has its normal SQL effect. See the
[SQL command reference](/docs/cli/reference/#sql) for the remaining shell
commands and output details.

## 6. Inspect the same store through Record Layer

The catalog resolves both the native store address and its schema-pinned
metadata. Keep the `--database` and `--schema` flags together:

```sh
fdb rl meta types ls --database /FRL/demo --schema main
fdb rl record scan --database /FRL/demo --schema main --limit 3
fdb rl store info --database /FRL/demo --schema main
fdb rl store dump --database /FRL/demo --schema main --limit 20
```

A SQL row's stored primary key can include a record-type prefix. Discover the
actual key rather than guessing that prefix:

```sh
fdb rl record scan --database /FRL/demo --schema main --limit 1 > first-record.ndjson
cat first-record.ndjson
pk="$(jq -r '.primary_key' first-record.ndjson)"
fdb rl record get "$pk" --database /FRL/demo --schema main
```

The supplied schema's first order has a composite key such as `0,1`, not just
the SQL `order_id` value. The `primary_key` printed by `record scan` is the value
to pass to `record get` for these integer keys. `--type` can also supply a
record-type prefix for types whose key expression includes one; see the
[record reference](/docs/cli/reference/#record-commands).

Use SQL for relational updates. Native `fdb rl record put` bypasses SQL-level
constraints that are not encoded in Record Layer metadata.

## 7. Clean up

```sh
fdb dev down --name fdb-sql-demo
rm -f first-record.ndjson
```

Removing the container deletes the development data. The CLI context and cluster
file remain; switch contexts with `fdb config use-context <name>` if needed.

For native protobuf schemas and secondary indexes, continue with the
[protobuf tutorial](/docs/cli/protobuf/). For an existing cluster, see
[connections and metadata](/docs/cli/metadata/).

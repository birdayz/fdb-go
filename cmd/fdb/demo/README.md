# SQL demo assets

The runnable walkthrough lives on the website:
[SQL and catalog discovery](https://fdb.dev/docs/cli/sql/).
It covers source builds, `fdb dev up`, these scripts, SQL queries and the
interactive shell, catalog discovery, Record Layer inspection, primary-key
discovery, and named-instance cleanup.

- `schema.sql` creates database `/FRL/DEMO`, template `ORDERS_TPL`, and schema
  `MAIN`, with one `ORDERS` table. Run it once against a fresh development
  database; it is not idempotent.
- `seed.sql` deletes the previous nonnegative order IDs and inserts 1,000 orders
  in ten batches of 100. It can be rerun.

From a checkout with an active development-cluster context:

```sh
go run ./cmd/fdb sql --database /FRL/demo -f cmd/fdb/demo/schema.sql
go run ./cmd/fdb sql --database /FRL/demo --schema main -f cmd/fdb/demo/seed.sql
go run ./cmd/fdb sql --database /FRL/demo --schema main \
  -c 'SELECT count(*) FROM orders'
```

Unquoted names fold to uppercase; `--schema main` and `--schema MAIN` both work.
Discover a stored primary key with `fdb rl record scan` rather than guessing
its record-type prefix. The website walkthrough shows how to pass that exact
key to `fdb rl record get`.

For a native store with protobuf-defined records and secondary indexes, see
[Protobuf records and indexes](https://fdb.dev/docs/cli/protobuf/).

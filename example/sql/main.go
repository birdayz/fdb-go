// Command sql is a runnable quickstart for the SQL engine via Go's standard
// database/sql interface. It creates a schema, inserts rows, and runs a few
// queries — the same surface a production app uses.
//
// Run it against a local FoundationDB:
//
//	fdbserver ...                       # or `docker run foundationdb/foundationdb`
//	go run ./example/sql                # uses FDB_CLUSTER_FILE or the default file
//
// To use Apple's C client instead of the pure-Go one, rebuild with the tag:
//
//	CGO_ENABLED=1 go run -tags libfdbc ./example/sql
//
// This file is built in CI (it must always compile); running it needs a live
// cluster.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"

	// Registers the "fdbsql" driver with database/sql.
	_ "github.com/birdayz/fdb-record-layer-go/pkg/relational/sqldriver"
)

func main() {
	ctx := context.Background()

	// The cluster file comes from FDB_CLUSTER_FILE (or FDB's default location
	// when empty). The DSN mirrors Java's JDBC URL without the "jdbc:" prefix.
	clusterFile := os.Getenv("FDB_CLUSTER_FILE")
	const dbPath = "/quickstart"

	// A "setup" handle (no default schema) for the DDL that creates the
	// database, the schema template, and the schema.
	setup, err := sql.Open("fdbsql", dsn(dbPath, clusterFile, ""))
	if err != nil {
		log.Fatalf("open setup connection: %v", err)
	}
	defer setup.Close()

	// Idempotent setup so the quickstart is re-runnable: a second run hits
	// "already exists" on these CREATEs, which setupExec logs and skips.
	setupExec(ctx, setup, "CREATE DATABASE "+dbPath)
	setupExec(ctx, setup, "CREATE SCHEMA TEMPLATE quickstart_tmpl "+
		"CREATE TABLE orders ("+
		"  id BIGINT NOT NULL,"+
		"  customer STRING,"+
		"  amount BIGINT,"+
		"  PRIMARY KEY (id))"+
		"CREATE INDEX orders_by_customer ON orders (customer)")
	setupExec(ctx, setup, "CREATE SCHEMA "+dbPath+"/app WITH TEMPLATE quickstart_tmpl")

	// The application handle, bound to the "app" schema.
	db, err := sql.Open("fdbsql", dsn(dbPath, clusterFile, "app"))
	if err != nil {
		log.Fatalf("open app connection: %v", err)
	}
	defer db.Close()

	// Clear any rows from a previous run so the seed below doesn't hit a
	// primary-key conflict (keeps the quickstart re-runnable).
	setupExec(ctx, db, "DELETE FROM orders")

	// Insert some rows. Parameter placeholders use ? (positional).
	for _, o := range []struct {
		id       int64
		customer string
		amount   int64
	}{
		{1, "alice", 100},
		{2, "bob", 250},
		{3, "alice", 75},
	} {
		if _, err := db.ExecContext(ctx,
			"INSERT INTO orders VALUES (?, ?, ?)", o.id, o.customer, o.amount); err != nil {
			log.Fatalf("insert order %d: %v", o.id, err)
		}
	}

	// Point query.
	var customer string
	var amount int64
	if err := db.QueryRowContext(ctx,
		"SELECT customer, amount FROM orders WHERE id = ?", int64(2)).
		Scan(&customer, &amount); err != nil {
		log.Fatalf("point query: %v", err)
	}
	fmt.Printf("order 2: %s spent %d\n", customer, amount)

	// Aggregate with GROUP BY — uses the orders_by_customer index.
	rows, err := db.QueryContext(ctx,
		"SELECT customer, COUNT(*), SUM(amount) FROM orders GROUP BY customer ORDER BY customer")
	if err != nil {
		log.Fatalf("aggregate query: %v", err)
	}
	defer rows.Close()

	fmt.Println("totals by customer:")
	for rows.Next() {
		var c string
		var n, total int64
		if err := rows.Scan(&c, &n, &total); err != nil {
			log.Fatalf("scan: %v", err)
		}
		fmt.Printf("  %-6s orders=%d total=%d\n", c, n, total)
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("rows: %v", err)
	}
}

// dsn builds an fdbsql DSN. An empty clusterFile uses FDB's default file; an
// empty schema omits the default-schema binding (used for the setup handle).
func dsn(dbPath, clusterFile, schema string) string {
	d := "fdbsql://" + dbPath
	sep := "?"
	if clusterFile != "" {
		d += sep + "cluster_file=" + clusterFile
		sep = "&"
	}
	if schema != "" {
		d += sep + "schema=" + schema
	}
	return d
}

// setupExec runs idempotent setup/cleanup DDL: it logs and CONTINUES on error so
// the quickstart is re-runnable. A second run hits "already exists" on the
// CREATEs (and DELETE on an empty table is a no-op) — neither should abort.
func setupExec(ctx context.Context, db *sql.DB, stmt string) {
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		log.Printf("setup (continuing): %v", err)
	}
}

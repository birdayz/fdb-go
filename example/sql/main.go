// Command sql is a runnable quickstart for the SQL engine via Go's standard
// database/sql interface. It creates a schema, inserts rows, and runs a few
// queries — the same surface a production app uses.
//
// Run it against a local FoundationDB:
//
//	FDB_CLUSTER_FILE=/path/to/fdb.cluster go run ./example/sql
//
// See README.md for disposable-cluster setup with frl fdb up.
//
// To use Apple's C client instead of the pure-Go one, rebuild with the tag:
//
//	CGO_ENABLED=1 go run -tags libfdbc ./example/sql
//
// main_test.go runs it against a real FoundationDB.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"fdb.dev/pkg/relational/sqldriver"
)

func main() {
	// The cluster file comes from FDB_CLUSTER_FILE (or FDB's default location
	// when empty).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := run(ctx, os.Getenv("FDB_CLUSTER_FILE"), os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, clusterFile string, out io.Writer) error {
	// Domains must be registered by the application, as in Java.
	sqldriver.RegisterDomainIfNotExists("FRL")
	const dbPath = "/FRL/QUICKSTART"

	// A "setup" handle (no default schema) for the DDL that creates the
	// database, the schema template, and the schema.
	setup, err := sql.Open("fdbsql", dsn(dbPath, clusterFile, ""))
	if err != nil {
		return fmt.Errorf("open setup connection: %w", err)
	}
	defer setup.Close()

	// This demo recreates its database and template: use a disposable cluster.
	// As in Java, only ARRAY columns accept NOT NULL.
	for _, stmt := range []string{
		"DROP DATABASE IF EXISTS " + dbPath,
		"DROP SCHEMA TEMPLATE IF EXISTS quickstart_tmpl",
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE quickstart_tmpl " +
			"CREATE TABLE orders (id BIGINT, customer STRING, amount BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX orders_by_customer ON orders (customer)",
		"CREATE SCHEMA " + dbPath + "/app WITH TEMPLATE quickstart_tmpl",
	} {
		if _, err := setup.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("setup %q: %w", stmt, err)
		}
	}

	// The application handle, bound to the "app" schema.
	db, err := sql.Open("fdbsql", dsn(dbPath, clusterFile, "app"))
	if err != nil {
		return fmt.Errorf("open app connection: %w", err)
	}
	defer db.Close()

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
			return fmt.Errorf("insert order %d: %w", o.id, err)
		}
	}

	// Point query.
	var customer string
	var amount int64
	if err := db.QueryRowContext(ctx,
		"SELECT customer, amount FROM orders WHERE id = ?", int64(2)).
		Scan(&customer, &amount); err != nil {
		return fmt.Errorf("point query: %w", err)
	}
	if _, err := fmt.Fprintf(out, "order 2: %s spent %d\n", customer, amount); err != nil {
		return err
	}

	// Aggregate with GROUP BY.
	rows, err := db.QueryContext(ctx,
		"SELECT customer, COUNT(*), SUM(amount) FROM orders GROUP BY customer ORDER BY customer")
	if err != nil {
		return fmt.Errorf("aggregate query: %w", err)
	}
	defer rows.Close()

	if _, err := fmt.Fprintln(out, "totals by customer:"); err != nil {
		return err
	}
	for rows.Next() {
		var c string
		var n, total int64
		if err := rows.Scan(&c, &n, &total); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		if _, err := fmt.Fprintf(out, "  %-6s orders=%d total=%d\n", c, n, total); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows: %w", err)
	}
	return nil
}

// dsn builds an fdbsql DSN. An empty clusterFile uses FDB's default file; an
// empty schema omits the default-schema binding (used for the setup handle).
func dsn(dbPath, clusterFile, schema string) string {
	params := url.Values{}
	if clusterFile != "" {
		params.Set("cluster_file", clusterFile)
	}
	if schema != "" {
		params.Set("schema", strings.ToUpper(schema))
	}
	u := url.URL{Scheme: "fdbsql", Path: dbPath, RawQuery: params.Encode()}
	return u.String()
}

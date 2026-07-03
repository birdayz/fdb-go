package sqldriver_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// TestFDB_RFC173W4b_UpdateSetCorrelatedBinder settles the reachability of the
// UPDATE-transform twin of the round-5 projection binder gap. Probing found
// something WORSE than the predicted silent NULL: the builder treated the SET
// RHS subquery as a plain expression and wrote its LITERAL TEXT into the row
// ("(SELECT l.tag FROM ...)" as the value — silent data corruption, identical
// on master). Subqueries in UPDATE ... SET now DECLINE with a clean
// unsupported error (Java's ExpressionVisitor has no subquery arm for the SET
// RHS either); this pin asserts the decline AND that no row is touched. If
// the shape ever gains support, flip the pin to correct rows — never text.
func TestFDB_RFC173W4b_UpdateSetCorrelatedBinder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/w4b_ub"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	const tmpl = "w4b_ub_tmpl"
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE customers (id BIGINT, name STRING, PRIMARY KEY (id))"+
		" CREATE TABLE labels (id BIGINT, customer_id BIGINT, tag STRING, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO customers VALUES (1, 'alice'), (2, 'bob')"); err != nil {
		t.Fatalf("seed customers: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO labels VALUES (10, 1, 'gold'), (11, 2, 'silver')"); err != nil {
		t.Fatalf("seed labels: %v", err)
	}

	_, execErr := db.ExecContext(ctx,
		"UPDATE customers SET name = (SELECT l.tag FROM labels l WHERE l.customer_id = customers.id) WHERE id = 1")
	if execErr == nil {
		// The shape planned: the SET value must be the CORRELATED result —
		// never NULL (the binder-gap failure mode) and NEVER the subquery text
		// (the corruption this pin was born red on).
		var name sql.NullString
		if err := db.QueryRowContext(ctx, "SELECT name FROM customers WHERE id = 1").Scan(&name); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if !name.Valid || name.String != "gold" {
			t.Fatalf("correlated UPDATE SET wrote %q (valid=%v), want gold", name.String, name.Valid)
		}
		return
	}
	if !strings.Contains(execErr.Error(), "0AF00") {
		t.Errorf("correlated SET decline error = %v, want the clean unsupported class (0AF00)", execErr)
	}
	// The decline must leave every row untouched — the pre-guard behavior
	// WROTE the literal subquery text.
	var name sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT name FROM customers WHERE id = 1").Scan(&name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !name.Valid || name.String != "alice" {
		t.Errorf("declined UPDATE mutated the row: name = %q (valid=%v), want alice untouched", name.String, name.Valid)
	}
}

package sqldriver_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// TestFDB_UpdateSetSubqueryDecline pins a correlated subquery in an
// UPDATE ... SET RHS. The builder used to treat the SET RHS subquery as a
// plain expression and write its LITERAL TEXT into the row
// ("(SELECT l.tag FROM ...)" as the value — silent data corruption). Subqueries
// in UPDATE ... SET now DECLINE with a clean unsupported error (Java's
// ExpressionVisitor has no subquery arm for the SET RHS either); this pin
// asserts the decline AND that no row is touched. If the shape ever gains
// support, flip the pin to correct rows — never text.
func TestFDB_UpdateSetSubqueryDecline(t *testing.T) {
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

	// The EXISTS sibling: the grammar has a SEPARATE atom type for EXISTS
	// (ExistsExpressionAtomContext), so a subquery-only guard walks right past
	// `SET name = EXISTS(SELECT ...)` — same text-corruption mechanism, same
	// decline required, same no-mutation assertion.
	_, existsErr := db.ExecContext(ctx,
		"UPDATE customers SET name = EXISTS(SELECT l.tag FROM labels l WHERE l.customer_id = customers.id) WHERE id = 1")
	if existsErr == nil {
		if err := db.QueryRowContext(ctx, "SELECT name FROM customers WHERE id = 1").Scan(&name); err != nil {
			t.Fatalf("read back after EXISTS SET: %v", err)
		}
		t.Fatalf("EXISTS in UPDATE SET unexpectedly planned; wrote %q — flip this pin to a correctness assertion if the shape gained real support", name.String)
	}
	if !strings.Contains(existsErr.Error(), "0AF00") {
		t.Errorf("EXISTS SET decline error = %v, want the clean unsupported class (0AF00)", existsErr)
	}
	if err := db.QueryRowContext(ctx, "SELECT name FROM customers WHERE id = 1").Scan(&name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !name.Valid || name.String != "alice" {
		t.Errorf("declined EXISTS UPDATE mutated the row: name = %q (valid=%v), want alice untouched", name.String, name.Valid)
	}

	// The IN-subquery sibling: `IN (SELECT ...)` is an InListContext carrying
	// a QueryExpressionBody — a third query-bearing form the atom-type checks
	// walk past. Same corruption mechanism, same decline, same no-mutation.
	_, inErr := db.ExecContext(ctx,
		"UPDATE customers SET name = id IN (SELECT l.customer_id FROM labels l) WHERE id = 1")
	if inErr == nil {
		if err := db.QueryRowContext(ctx, "SELECT name FROM customers WHERE id = 1").Scan(&name); err != nil {
			t.Fatalf("read back after IN SET: %v", err)
		}
		t.Fatalf("IN-subquery in UPDATE SET unexpectedly planned; wrote %q — flip this pin to a correctness assertion if the shape gained real support", name.String)
	}
	if !strings.Contains(inErr.Error(), "0AF00") {
		t.Errorf("IN-subquery SET decline error = %v, want the clean unsupported class (0AF00)", inErr)
	}
	if err := db.QueryRowContext(ctx, "SELECT name FROM customers WHERE id = 1").Scan(&name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !name.Valid || name.String != "alice" {
		t.Errorf("declined IN-subquery UPDATE mutated the row: name = %q (valid=%v), want alice untouched", name.String, name.Valid)
	}
}

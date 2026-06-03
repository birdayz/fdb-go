package plandiff

import (
	"context"
	"testing"
)

// TestGoSQLRunner_JoinProjectionColumnTypes is the regression test for the
// join-projection column-type bug.
//
// A projection over a join references columns from MULTIPLE record types
// (`SELECT u.name, o.total FROM Users u, Orders o ...`). The column-metadata
// derivation only resolved types against a single join leaf (the inner scan),
// so a column from the other leaf — here `o.total` from Orders — had no
// descriptor to resolve against and was reported with type UNKNOWN, diverging
// from Java (which reports BIGINT). Now every projected column is resolved
// against all join-leaf descriptors.
//
// Teeth: before the fix O.TOTAL came back typed UNKNOWN.
func TestGoSQLRunner_JoinProjectionColumnTypes(t *testing.T) {
	t.Parallel()
	if goSQLClusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	r := NewGoSQLSetupRunner(goSQLClusterFilePath)
	const schema = "CREATE TABLE Users (uid BIGINT, name STRING, PRIMARY KEY (uid))" +
		" CREATE TABLE Orders (oid BIGINT, uid BIGINT, total BIGINT, PRIMARY KEY (oid))"
	setup := []string{
		"INSERT INTO Users VALUES (1, 'alice')",
		"INSERT INTO Users VALUES (2, 'bob')",
		"INSERT INTO Orders VALUES (10, 1, 100)",
		"INSERT INTO Orders VALUES (11, 1, 200)",
		"INSERT INTO Orders VALUES (12, 2, 300)",
	}
	const query = "SELECT u.name, o.total FROM Users u, Orders o WHERE u.uid = o.uid ORDER BY o.oid"

	got := r.RunWithSetup(context.Background(), schema, setup, query)
	if got.Err != nil {
		t.Fatalf("RunWithSetup: %v", got.Err)
	}
	if len(got.Rows.Columns) != 2 {
		t.Fatalf("got %d columns %+v, want 2", len(got.Rows.Columns), got.Rows.Columns)
	}
	// Column 0: u.name (Users.name) → STRING. Column 1: o.total (Orders.total)
	// → BIGINT. The Orders column is the one that regressed to UNKNOWN when
	// only the inner (Users) leaf descriptor was consulted.
	want := []struct{ name, typ string }{
		{"U.NAME", "STRING"},
		{"O.TOTAL", "BIGINT"},
	}
	for i, w := range want {
		if got.Rows.Columns[i].Name != w.name {
			t.Errorf("column %d name = %q, want %q", i, got.Rows.Columns[i].Name, w.name)
		}
		if got.Rows.Columns[i].Type != w.typ {
			t.Errorf("column %d (%s) type = %q, want %q (join-leaf type not resolved)",
				i, got.Rows.Columns[i].Name, got.Rows.Columns[i].Type, w.typ)
		}
	}
}

// TestGoSQLRunner_JoinSameNamedColumnsDisambiguateByQualifier pins the
// cross-leg disambiguation: when both join legs define a same-named column of
// DIFFERENT types, the projection's qualifier must select the right leg's
// descriptor. Resolving every column against the first leaf (or first match on
// the bare name) would mis-type the second column. Here both legs have `val`
// (A.val BIGINT, B.val STRING); the table-name qualifier identifies the leg.
func TestGoSQLRunner_JoinSameNamedColumnsDisambiguateByQualifier(t *testing.T) {
	t.Parallel()
	if goSQLClusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	r := NewGoSQLSetupRunner(goSQLClusterFilePath)
	const schema = "CREATE TABLE A (id BIGINT, val BIGINT, PRIMARY KEY (id))" +
		" CREATE TABLE B (id BIGINT, val STRING, PRIMARY KEY (id))"
	setup := []string{
		"INSERT INTO A VALUES (1, 100)",
		"INSERT INTO B VALUES (1, 'hello')",
	}
	const query = "SELECT A.val, B.val FROM A, B WHERE A.id = B.id"

	got := r.RunWithSetup(context.Background(), schema, setup, query)
	if got.Err != nil {
		t.Fatalf("RunWithSetup: %v", got.Err)
	}
	if len(got.Rows.Columns) != 2 {
		t.Fatalf("got %d columns %+v, want 2", len(got.Rows.Columns), got.Rows.Columns)
	}
	// A.val → BIGINT, B.val → STRING. First-match-on-bare-name would type
	// both as whichever leg appears first.
	if got.Rows.Columns[0].Type != "BIGINT" {
		t.Errorf("col 0 (A.val) type = %q, want BIGINT", got.Rows.Columns[0].Type)
	}
	if got.Rows.Columns[1].Type != "STRING" {
		t.Errorf("col 1 (B.val) type = %q, want STRING (qualifier did not disambiguate)", got.Rows.Columns[1].Type)
	}
}

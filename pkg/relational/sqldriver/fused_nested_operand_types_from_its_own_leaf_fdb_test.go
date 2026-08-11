package sqldriver_test

// Pins the arithmetic result type of an expression whose operand is a NESTED
// struct-member reference, on the one fixture where the answer discriminates:
// the member's leaf name is ALSO the name of a top-level column of a DIFFERENT
// type.
//
// A fused nested reference is ONE FieldValue carrying the whole path. The only
// single name askable of it is its LEAF's — that is what Java's fused
// FieldValue answers (getLastFieldName, FieldValue.java:134-135) and what this
// tree's mints now agree on. But a leaf name is a name INSIDE a struct, and the
// struct's namespace is not the record's: `n.sk` and `sk` are different columns
// that may legally share a spelling and differ in type.
//
// So a consumer that takes that leaf name and looks it up in the RECORD
// descriptor asks the wrong namespace a question it will confidently answer.
// operandTypeNameViaDesc did exactly that, and the answer it got back was the
// FLAT column's type. Java settles the direction and it is not the descriptor:
// FieldValue.computeResultType (FieldValue.java:143-148) is
// `fieldPath.getLastFieldType()` — the value's own path states its type, and
// there is no name-keyed re-derivation anywhere upstream.
//
// THE FIXTURE MUST KEEP THE TYPE COLLISION. `n.sk BIGINT` alongside a flat
// `sk DOUBLE` is the entire discriminating power of this test: make the two
// types agree and every arm below passes with the defect fully present. The
// controls assert the collision is live rather than assuming it, so the day the
// engine stops distinguishing them this test says so instead of going quietly
// green.
//
// The DOUBLE/BIGINT pair is chosen because numeric promotion makes a wrong read
// STRICTLY WIDER — a wrong answer here is a type the correct one promotes to,
// which is the direction that survives a casual eyeballing of the metadata.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_NestedOperandTypeComesFromItsOwnLeafNotAFlatNamesake(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nlopnd")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nlopnd")
	// `nn.sk` is BIGINT and the TOP-LEVEL `sk` is DOUBLE. That collision is the
	// fixture's whole point; see the header before changing either type.
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE nlopnd "+
			"CREATE TYPE AS STRUCT nn (sk BIGINT, co STRING) "+
			"CREATE TABLE t (id BIGINT, n nn, sk DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nlopnd/s WITH TEMPLATE nlopnd")
	dsn := fmt.Sprintf("fdbsql:///testdb_nlopnd?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// One row: the assertion is on metadata, which the engine derives from the
	// plan, not from the data. The row exists so the query is not trivially
	// empty and the value arm below has something to read.
	if _, e := db.ExecContext(ctx,
		"INSERT INTO t (id, n, sk) VALUES (1, (7, 'a'), 2.5)"); e != nil {
		t.Fatalf("insert: %v", e)
	}

	probeType := func(q string) string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cts, err := rows.ColumnTypes()
		if err != nil {
			t.Fatalf("ColumnTypes %q: %v", q, err)
		}
		if len(cts) != 1 {
			t.Fatalf("query %q: %d columns, want 1", q, len(cts))
		}
		return cts[0].DatabaseTypeName()
	}

	// CONTROL A: the flat namesake. This is the column a name-keyed descriptor
	// lookup finds, and its type is what a wrong answer will report.
	flatType := probeType("SELECT sk + 1 FROM t")
	if flatType != "DOUBLE" {
		t.Fatalf("SELECT sk + 1: DatabaseTypeName = %q, want DOUBLE — the fixture's flat "+
			"column is declared DOUBLE and this test's discriminating power depends on it",
			flatType)
	}

	// CONTROL B: the collision is live. If the two ever report the same type
	// name, the subject assertion below cannot tell which namespace was
	// consulted and is worth nothing.
	memberOnlyType := probeType("SELECT n.sk FROM t")
	if memberOnlyType != "BIGINT" {
		t.Fatalf("SELECT n.sk: DatabaseTypeName = %q, want BIGINT — the struct member is "+
			"declared BIGINT", memberOnlyType)
	}
	if memberOnlyType == flatType {
		t.Fatalf("the struct member `n.sk` and the flat column `sk` both report %q, so this "+
			"test can no longer tell which one an arithmetic operand resolved against — "+
			"restore the type collision the fixture is built on", memberOnlyType)
	}

	// THE SUBJECT. `n.sk` is BIGINT, so BIGINT + INTEGER promotes to BIGINT.
	// Reading the operand's type out of the record descriptor by the fused
	// reference's leaf NAME finds the flat DOUBLE column instead and promotes to
	// DOUBLE — a strictly wider wrong answer.
	got := probeType("SELECT n.sk + 1 FROM t")
	if got == flatType {
		t.Errorf("SELECT n.sk + 1: DatabaseTypeName = %q — the FLAT column `sk`'s type. "+
			"The arithmetic operand's type was re-derived by looking the fused nested "+
			"reference's LEAF NAME up in the record descriptor, which is a different "+
			"namespace: `n.sk` is a member of struct `nn`, not a top-level field. Java "+
			"takes the path's own last field type (FieldValue.computeResultType, "+
			"FieldValue.java:143-148) and never re-derives from a name. Want %q.",
			got, memberOnlyType)
	}
	if got != "BIGINT" {
		t.Errorf("SELECT n.sk + 1: DatabaseTypeName = %q, want BIGINT — the promotion of the "+
			"member's own declared type (BIGINT) with an integer literal.", got)
	}

	// The nested operand on the RIGHT of the operator too: a fix that reaches
	// only the operand the test happens to place first is not a fix.
	if got := probeType("SELECT 1 + n.sk FROM t"); got != "BIGINT" {
		t.Errorf("SELECT 1 + n.sk: DatabaseTypeName = %q, want BIGINT (same expression, "+
			"operands swapped — both sides go through the same per-operand derivation).",
			got)
	}

	// The VALUE must agree with the metadata. A metadata-only fix that reports
	// BIGINT while the engine computes in DOUBLE would pass every arm above.
	var v any
	if e := db.QueryRowContext(ctx, "SELECT n.sk + 1 FROM t").Scan(&v); e != nil {
		t.Fatalf("scan n.sk + 1: %v", e)
	}
	if iv, ok := v.(int64); !ok || iv != 8 {
		t.Errorf("SELECT n.sk + 1 returned %#v (%T), want int64(8) — the member holds 7", v, v)
	}
}

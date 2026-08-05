package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// RFC-210 §5.1 clause 4: a SPARSE (WHERE-filtered) UNIQUE index proves NOTHING
// about DISTINCT, and this is the clause whose failure is duplicate rows rather
// than a slower plan.
//
// A sparse index omits every record its stored predicate rejects, so its UNIQUE
// declaration constrains only the rows the predicate ADMITS. The rows it
// excludes are unconstrained and may hold arbitrarily many duplicates of an
// admitted value — which is exactly what this fixture builds.
//
// §7.2 requires this row end-to-end with real data and warns against the
// weakened form it decays into: "a sparse index exists" is not the test, because
// a sparse-index fixture whose EXCLUDED rows happen to be distinct passes with
// the bug fully present. The excluded rows here are therefore chosen to be the
// discriminator:
//
//	(1, 'a@x', keep=1)  ADMITTED
//	(2, 'b@x', keep=1)  ADMITTED
//	(3, 'a@x', keep=0)  EXCLUDED — a duplicate of an ADMITTED value
//	(4, 'a@x', keep=0)  EXCLUDED — and a second one
//	(5, 'c@x', keep=0)  EXCLUDED — a value that exists ONLY outside the predicate
//
// The index holds two entries and is perfectly unique over them. The TABLE holds
// three distinct emails across five rows, three of which share one value. Admit
// this index as a proof and R3 narrows the operator: 'a@x' is non-exempt, so all
// three of its rows pass through retaining nothing, and the statement returns
// five rows where three are correct.
//
// The DDL prerequisite — that UNIQUE and a stored predicate combine at all — is
// settled separately in embedded's TestSparseUniqueIndexDDL_Accepts...; it is
// what makes this test exercise clause 4 rather than a full unique index wearing
// a sparse name.
func TestFDB_SparseUniqueIndexDoesNotProveDistinct(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_sparseu")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_sparseu")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE sparseu "+
			"CREATE TABLE sp (id BIGINT, email STRING, keep BIGINT, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX sparse_u AS SELECT email FROM sp WHERE keep > 0 ORDER BY email "+
			// The control. Same table, same column, no predicate — so it IS a
			// qualifying proof. Without it, a rule that stopped drawing ANY
			// secondary-unique proof on this table (a broken candidate walk, a
			// metadata mishap) would satisfy every assertion below while proving
			// nothing about clause 4.
			"CREATE TABLE fu (id BIGINT, email STRING, keep BIGINT, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX full_u AS SELECT email FROM fu ORDER BY email")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_sparseu/s WITH TEMPLATE sparseu")

	dsn := fmt.Sprintf("fdbsql:///testdb_sparseu?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO sp (id, email, keep) VALUES "+
		"(1, 'a@x', 1), (2, 'b@x', 1), (3, 'a@x', 0), (4, 'a@x', 0), (5, 'c@x', 0)")
	// The control table's own duplicates would violate ITS unique index, so it
	// carries only distinct values — which is the point: a full unique index
	// genuinely guarantees that, and a sparse one does not.
	mwjoMustExec(t, db, ctx, "INSERT INTO fu (id, email, keep) VALUES "+
		"(1, 'a@x', 1), (2, 'b@x', 1), (3, 'c@x', 0)")

	// ---- the control fires ---------------------------------------------
	fullExplain := explainPlan(t, ctx, db, "SELECT DISTINCT email FROM fu")
	t.Logf("EXPLAIN control  => %s", fullExplain)
	if !strings.Contains(fullExplain, "narrowed-by:FULL_U") {
		t.Fatalf("the CONTROL did not draw a proof from its full unique index: %s\n"+
			"Every assertion below is a claim that the sparse index is refused "+
			"SPECIFICALLY, and that claim is vacuous if no proof is drawn on this "+
			"table shape at all.", fullExplain)
	}

	// ---- the sparse index is refused ------------------------------------
	sparseExplain := explainPlan(t, ctx, db, "SELECT DISTINCT email FROM sp")
	t.Logf("EXPLAIN sparse   => %s", sparseExplain)
	if !strings.Contains(sparseExplain, "Distinct(") {
		t.Fatalf("the DISTINCT was ELIDED over a SPARSE unique index: %s\n"+
			"SPARSE_U holds one entry per admitted email and says nothing about the "+
			"three excluded rows, two of which duplicate an admitted value.",
			sparseExplain)
	}
	if strings.Contains(sparseExplain, "narrowed-by") {
		t.Fatalf("the DISTINCT was NARROWED by a SPARSE unique index: %s\n"+
			"Narrowing retains only exempt (NULL/NaN) keys and passes every other "+
			"row through — so the excluded duplicates of 'a@x' flow straight to the "+
			"output. Clause 4 refuses this candidate inside "+
			"candidatePreservesBaseRecordCardinality, on predicateProto != nil.",
			sparseExplain)
	}
	if strings.Contains(sparseExplain, "distinct-by") {
		t.Fatalf("a sparse unique index was recorded as a proof dependency: %s",
			sparseExplain)
	}

	// ---- and the rows are right -----------------------------------------
	// The assertion that does not depend on how the plan renders: whatever the
	// operator, three distinct emails must come back. A wrong admission returns
	// five.
	//
	// Deliberately NOT `ORDER BY email`, and the ordering is done in Go instead.
	// Admitting the sparse candidate does not only narrow the unordered
	// operator — it also makes SPARSE_U reachable as an ORDERED access path, and
	// the ordered query then plans an index scan that the executor's
	// filtered-index guard rejects outright. That is a second, independent
	// defence and a good one, but it means an ORDER BY here measures the GUARD
	// rather than the operator: the query errors before it can return the
	// duplicate rows this assertion exists to catch. Measured under exactly that
	// mutation. Keeping the query unordered keeps this assertion aimed at the
	// unordered shape the EXPLAIN above pins.
	rows, qerr := db.QueryContext(ctx, "SELECT DISTINCT email FROM sp")
	if qerr != nil {
		t.Fatalf("query: %v", qerr)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var email string
		if scanErr := rows.Scan(&email); scanErr != nil {
			t.Fatalf("scan: %v", scanErr)
		}
		got = append(got, email)
	}
	if rerr := rows.Err(); rerr != nil {
		t.Fatalf("rows: %v", rerr)
	}
	sort.Strings(got)
	want := []string{"a@x", "b@x", "c@x"}
	if len(got) != len(want) {
		t.Fatalf("SELECT DISTINCT over the sparse-unique table returned %d rows %v, "+
			"want %d %v.\nThe table holds five rows and three distinct emails; three "+
			"rows share 'a@x' and only ONE of them is inside the index's predicate. "+
			"A count of five means the operator passed the excluded duplicates "+
			"through on a uniqueness guarantee that never covered them.",
			len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %q, want %q (full result %v)", i, got[i], want[i], got)
		}
	}
}

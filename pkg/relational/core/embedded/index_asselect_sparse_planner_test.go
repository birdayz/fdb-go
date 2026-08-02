package embedded

import (
	"strings"
	"testing"
)

// RFC-202 S5 planner safety: a SPARSE (WHERE-filtered) index's match
// candidate carries its stored predicate
// (ValueIndexExpansionVisitor.java:138-162 → Go's
// cascades.IndexDefWithPredicate + the expansion's candidate-side predicate),
// and the matcher refuses a candidate predicate it cannot account for. The
// live defect this pins: before the candidate carried the predicate, `SELECT
// col1 FROM t1 WHERE col1 < 453` planned IndexScan(I1) over an index holding
// only col1 < 200 — silently missing every row in [200, 453).
func TestSparseIndexCandidate_NeverMatchedAsFull(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE T1 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id))
CREATE INDEX i1 AS SELECT col1 FROM t1 WHERE col1 < 200
`
	for _, q := range []string{
		// NOT implied by the index predicate — using I1 loses rows.
		"SELECT col1 FROM t1 WHERE col1 < 453",
		// Implied by the index predicate — see the companion negative pin
		// below for why this too must not use I1 today.
		"SELECT col1 FROM t1 WHERE col1 < 100",
	} {
		plan, err := PlanQueryForTest(q, schema, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if strings.Contains(plan, "IndexScan(I1") || strings.Contains(plan, "COVERING(I1") {
			t.Errorf("%s planned over the SPARSE index I1 (predicate col1 < 200):\n%s\n"+
				"— the index omits non-matching records, so this plan silently loses rows",
				q, plan)
		}
	}
}

// TestSparseIndexCandidate_ImpliedQueryFallsBack is a NEGATIVE result with a
// named re-arm: Java's ranges arm (ValueIndexExpansionVisitor.java:146-158)
// re-expresses a DNF-of-ranges index predicate as extra ranges on the
// candidate's placeholders, so a query whose predicate IMPLIES the index
// predicate (col1 < 100 ⇒ col1 < 200) still matches and Java explains
// COVERING(I1 …) (sparse-index-tests.yamsql). Go's Placeholder carries no
// candidate-side ranges yet, so the implied query conservatively falls back
// to a base scan — correct rows, narrower reach. When placeholder extra
// ranges land, this test's expectation FLIPS: delete it and assert the
// COVERING plan instead.
func TestSparseIndexCandidate_ImpliedQueryFallsBack(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE T1 (id BIGINT, col1 BIGINT, PRIMARY KEY (id))
CREATE INDEX i1 AS SELECT col1 FROM t1 WHERE col1 < 200
`
	plan, err := PlanQueryForTest("SELECT col1 FROM t1 WHERE col1 < 100", schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "Scan(T1)") {
		t.Errorf("implied sparse query no longer plans a base scan:\n%s\n"+
			"— if the planner now matches sparse candidates via placeholder extra "+
			"ranges (Java's ValueIndexExpansionVisitor.java:146-158), this negative "+
			"pin has been RE-ARMED: replace it with a COVERING(I1 …) assertion and "+
			"prove range implication with a red-green over a NON-implied query", plan)
	}
}

// TestSparseIndexCandidate_OrderByNeverServedFromSparse pins the shortcut
// gate: OrderedIndexScanRule (and every other compensation-free shortcut
// behind candidatePreservesBaseRecordCardinality) must not swap a base-record
// scan for a SPARSE index — the index omits the records its predicate
// rejects. The live defect this pins: boolean-ddl.yamsql's `WHERE NULL` index
// is EMPTY, and `SELECT "col1" FROM T ORDER BY "col1"` planned
// IndexScan(IDX_NULL, [*]) — the whole table served from an empty index,
// 0 of 1 rows.
func TestSparseIndexCandidate_OrderByNeverServedFromSparse(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE T ("id" INTEGER, "col1" INTEGER, PRIMARY KEY ("id"))
CREATE INDEX idx_true  AS SELECT "col1" FROM T WHERE TRUE  ORDER BY "col1"
CREATE INDEX idx_false AS SELECT "col1" FROM T WHERE FALSE ORDER BY "col1"
CREATE INDEX idx_null  AS SELECT "col1" FROM T WHERE NULL  ORDER BY "col1"
`
	// Two query shapes, because two distinct paths served the empty index:
	// the bare ORDER BY through OrderedIndexScanRule's compensation-free
	// shortcut, and WHERE + ORDER BY through leaf-match absorption
	// (adjustMatchForSelect must bail on a non-tautology constant,
	// SelectExpression.java:617-620) followed by ordering-satisfying data
	// access.
	var plan string
	for _, q := range []string{
		`SELECT "col1" FROM T ORDER BY "col1"`,
		`SELECT "col1" FROM T WHERE "col1" = 5 ORDER BY "col1"`,
	} {
		var err error
		plan, err = PlanQueryForTest(q, schema, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, sparse := range []string{"IDX_FALSE", "IDX_NULL"} {
			if strings.Contains(plan, sparse) {
				t.Errorf("%s uses the sparse index %s:\n%s\n— its stored predicate rejects "+
					"records the scan then silently omits", q, sparse, plan)
			}
		}
	}
	// idx_true's WHERE TRUE predicate is a tautology: the candidate stays an
	// ordinary full value index and MAY serve the ordering
	// (ValueIndexExpansionVisitor.java:141)... but only through paths that
	// account for predicates. Either IDX_TRUE or an in-memory sort over the
	// base scan is a correct answer; the two empty indexes never are.
	if !strings.Contains(plan, "IDX_TRUE") && !strings.Contains(plan, "InMemorySort") {
		t.Errorf("plan neither uses IDX_TRUE nor sorts the base scan:\n%s", plan)
	}
}

// TestSparseIndexCandidate_TautologyPredicateStaysMatchable pins Java's :141
// gate: a WHERE TRUE index predicate is a tautology, is NOT attached to the
// candidate, and the index remains a normal, fully-matchable value index.
func TestSparseIndexCandidate_TautologyPredicateStaysMatchable(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE T1 (id BIGINT, col1 BIGINT, PRIMARY KEY (id))
CREATE INDEX i_true AS SELECT col1 FROM t1 WHERE TRUE
`
	plan, err := PlanQueryForTest("SELECT col1 FROM t1 WHERE col1 < 100", schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "I_TRUE") {
		t.Errorf("WHERE TRUE index not used:\n%s\n— a tautological index predicate "+
			"filters nothing (ValueIndexExpansionVisitor.java:141), so the candidate "+
			"must stay matchable like any full value index", plan)
	}
}

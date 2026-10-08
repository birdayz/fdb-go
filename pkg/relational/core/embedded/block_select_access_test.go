package embedded

import (
	"strings"
	"testing"
)

// A query block that is one Select matches the index as a whole, so its
// data-access compensation is a Select carrying the result value. That has to
// be explored and implemented like any other compensation (Java
// yieldUnknownExpression), not parked as a final.
func TestSelectBlockCompensationReachesTheIndex(t *testing.T) {
	t.Parallel()
	got, err := PlanQueryForTest("SELECT id FROM orders WHERE amount BETWEEN 100 AND 200", ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertPlanContains(t, got, "IndexScan(IDX_AMOUNT")
}

// A projected EXISTS reads its existential quantifier from the block's result
// value. A match over the FROM source alone leaves that quantifier unmatched,
// and the result compensation, rebuilt over the realized scan, cannot read it:
// such a match is impossible, so the existential leg stays in the plan.
func TestResultReadingAnUnmatchedQuantifierIsNotCompensated(t *testing.T) {
	t.Parallel()
	got, err := PlanQueryForTest(
		`SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h FROM t1 ORDER BY id`,
		derivedExistsSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertPlanContains(t, got, "FirstOrDefault")
}

// A CTE's column list is a block over the CTE body, and Go names both that
// block's input and the body's sort input after the scanned table. Reading the
// body's row through its sort must not rename the outer block's reads of its
// own, differently typed, input: every column of c is the body's one ID.
func TestBlockOverASortedBodyReadsItsOwnInput(t *testing.T) {
	t.Parallel()
	got, err := PlanQueryForTest(
		`WITH c(a, b, d) AS (SELECT id, id, id FROM t GROUP BY id, v ORDER BY v) SELECT COUNT(*) FROM c`,
		`CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))`, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertPlanContains(t, got, "{ID: _current.ID#0, ID_2: _current.ID#0, ID_3: _current.ID#0, _3: _current.V#1}")
	assertPlanContains(t, got, "{A: _current.ID#0, B: _current.ID_2#1, D: _current.ID_3#2}")
}

// A scalar subquery's block merges under its null-on-empty edge, so the
// correlated FlatMap over a sorted outer is its only implementation. The
// sort's own quantifier is no leg the subquery could read through the outer.
func TestScalarSubqueryOverASortedOuterPlans(t *testing.T) {
	t.Parallel()
	got, err := PlanQueryForTest(
		`SELECT name, (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id) FROM customers c ORDER BY name`,
		`CREATE TABLE customers (id BIGINT, name STRING, PRIMARY KEY (id))
		CREATE TABLE orders (id BIGINT, customer_id BIGINT, amount DOUBLE, PRIMARY KEY (id))`, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertPlanContains(t, got, "FlatMap(outer=InMemorySort(")
	assertPlanContains(t, got, "inner=DefaultOnEmpty(")
}

// An index definition's block reads its input with the row type the DDL path
// derives, which may disagree with the catalog's in nested nullability (an
// array's elements). That is still a read of the input row, not of a source
// below it.
func TestIndexBlockOverAnArrayTableBuilds(t *testing.T) {
	t.Parallel()
	if _, err := buildSchemaTemplateFromDDL(`CREATE TABLE t (id BIGINT, a BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id))
		CREATE INDEX ix_a ON t(a)`); err != nil {
		t.Fatal(err)
	}
}

// A non-partition residual over a per-partition top-K vector scan does not
// commute with the top-K, so the query must not plan. The block's
// compensation is a Select over that residual: reshaping rows over an unsafe
// residual is no safer than the residual itself.
func TestBlockCompensationOverAnUnsafeVectorResidualDoesNotPlan(t *testing.T) {
	t.Parallel()
	_, err := PlanQueryForTest(
		`SELECT id, region FROM docs WHERE id = 12 QUALIFY ROW_NUMBER() OVER (PARTITION BY zone, region ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) <= 1`,
		`CREATE TABLE docs (zone STRING, region STRING, id BIGINT, embedding VECTOR(3, DOUBLE), PRIMARY KEY (zone, region, id))
		CREATE VECTOR INDEX vec_idx USING HNSW ON docs(embedding) PARTITION BY (zone, region)`, nil)
	if err == nil || !strings.Contains(err.Error(), "not plannable") {
		t.Fatalf("a top-K-then-filter plan was produced (or the wrong error): %v", err)
	}
}

// A record IN in a block's WHERE explodes into a FlatMap probing the two-column
// index once per distinct row, and a scalar IN into the IN-join.
func TestBlockInListsReachTheirIndexes(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id))
		CREATE INDEX rvc_ab AS SELECT a, b FROM t ORDER BY a, b
		CREATE INDEX ia AS SELECT a FROM t ORDER BY a`
	for sql, want := range map[string]string{
		"SELECT id FROM t WHERE (a, b) IN ((1L, 2L), (1L, 2L), (3L, 4L))": "FlatMap(outer=Explode(array_distinct), inner=IndexScan(RVC_AB, [=, =]))",
		"SELECT id FROM t WHERE a IN (1, 3)":                              "InJoin(Map(IndexScan(IA, [=] COVERING)",
	} {
		got, err := PlanQueryForTest(sql, ddl, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertPlanContains(t, got, want)
	}
}

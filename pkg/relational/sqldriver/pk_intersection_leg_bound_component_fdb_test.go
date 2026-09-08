package sqldriver_test

// A primary-key intersection is sound only if its comparison key identifies a
// record within EVERY leg. Over PRIMARY KEY (pk1, pk2) with indexes (b, pk1)
// and (pk2), `WHERE b = 1 AND pk2 = 3` used to intersect the two covering scans
// on (pk1) alone — pk2 is equality-bound in the (pk2) leg, and the proof
// subtracted the UNION of the legs' equality-bound values from the primary key.
// The (b, pk1) leg holds several records per pk1 that differ only in pk2, so the
// merge emitted every b = 1 record whose pk1 also had SOME pk2 = 3 record: six
// rows for a query whose answer is one. Java 4.12.11.0 has the same defect
// (conformance/pk_intersection_leg_bound_key_java_probe_test.go); the Go proof
// is now per leg (cascades/intersector_primary_key.go,
// comparisonKeyIdentifiesRecordInEveryLeg).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDB_PkIntersectionLegBoundComponent(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_pkilbc")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_pkilbc")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE pkilbc "+
			"CREATE TABLE ti (pk1 BIGINT, pk2 BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (pk1, pk2)) "+
			"CREATE INDEX ti_a ON ti (a) "+
			"CREATE INDEX ti_b_pk1 ON ti (b, pk1) "+
			"CREATE INDEX ti_pk2 ON ti (pk2)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_pkilbc/s WITH TEMPLATE pkilbc")
	dsn := fmt.Sprintf("fdbsql:///testdb_pkilbc?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Every pk1 in 0..3 has a pk2 = 3 record; b = 1 records mostly sit at a
	// different pk2, so aligning the legs on pk1 alone matches four records
	// while only (3, 3) satisfies both predicates. a = 1 on (3, 3) and (0, 2)
	// gives the three-way shape one true row as well.
	mwjoMustExec(t, db, ctx, "INSERT INTO ti (pk1, pk2, a, b) VALUES "+
		"(0, 2, 1, 1), (0, 3, 0, 0), (1, 4, 0, 1), (1, 3, 0, 0), "+
		"(2, 0, 0, 1), (2, 3, 0, 7), (3, 3, 1, 1), (4, 1, 0, 1)")

	explain := mwjoExplainer(t, db, ctx)
	rowsOf := func(q string) []string {
		t.Helper()
		out, _, err := twinRows(ctx, db, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return out
	}

	cases := []struct {
		sql  string
		want []string
	}{
		{"SELECT pk1, pk2, a, b FROM ti WHERE b = 1 AND pk2 = 3", []string{"3|3|1|1"}},
		{"SELECT pk1, pk2, a, b FROM ti WHERE pk2 = 3 AND b = 1 ORDER BY pk1", []string{"3|3|1|1"}},
		{"SELECT pk1, pk2, a, b FROM ti WHERE pk2 = 3 AND b = 1 ORDER BY pk1, pk2", []string{"3|3|1|1"}},
		{"SELECT pk1 FROM ti WHERE b = 1 AND pk2 = 3", []string{"3"}},
		{"SELECT COUNT(*) FROM ti WHERE b = 1 AND pk2 = 3", []string{"1"}},
		{"SELECT pk1, pk2, a, b FROM ti WHERE b = 1 AND pk2 = 3 AND pk1 > -1", []string{"3|3|1|1"}},
		{"SELECT pk1, pk2, a, b FROM ti WHERE a = 1 AND b = 1 AND pk2 = 3", []string{"3|3|1|1"}},
		{"SELECT pk1, pk2, a, b FROM ti WHERE a = 1 AND pk2 = 3", []string{"3|3|1|1"}},
		{"SELECT pk1, pk2, a, b FROM ti WHERE b = 1 AND pk2 IN (3, 4) ORDER BY pk1", []string{"1|4|0|1", "3|3|1|1"}},
		{"SELECT pk1, pk2, a, b FROM ti WHERE b IN (1, 7) AND pk2 = 3 ORDER BY pk1", []string{"2|3|0|7", "3|3|1|1"}},
		// Control: two legs that fix NO primary-key component still intersect
		// soundly on (pk1, pk2).
		{"SELECT pk1, pk2, a, b FROM ti WHERE a = 1 AND b = 1 ORDER BY pk1", []string{"0|2|1|1", "3|3|1|1"}},
	}
	for _, c := range cases {
		got := rowsOf(c.sql)
		if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("wrong rows\n  sql:  %s\n  plan: %s\n  got:  %v\n  want: %v", c.sql, explain(c.sql), got, c.want)
		}
	}

	// The property, not the shape: any intersection the planner builds here must
	// compare on BOTH primary-key components, because no leg other than TI_PK2
	// fixes pk2 and TI_PK2 fixes nothing else. A future planner that widens the
	// comparison key instead of declining the merge passes this; one that
	// re-admits the (pk1)-only merge fails it.
	for _, c := range cases {
		plan, err := embedded.PlanPhysicalForTest(c.sql,
			"CREATE TABLE ti (pk1 BIGINT, pk2 BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (pk1, pk2)) "+
				"CREATE INDEX ti_a ON ti (a) CREATE INDEX ti_b_pk1 ON ti (b, pk1) CREATE INDEX ti_pk2 ON ti (pk2)", nil)
		if err != nil {
			t.Fatalf("plan %s: %v", c.sql, err)
		}
		for _, ip := range intersectionsIn(plan) {
			if n := len(ip.GetComparisonKeyValues()); n != 2 {
				t.Errorf("intersection compares on %d key(s), the primary key has 2 components\n  sql:  %s\n  plan: %s",
					n, c.sql, plan.Explain())
			}
		}
	}
}

// intersectionsIn walks the typed plan tree and returns every primary-key
// intersection in it (never by EXPLAIN text).
func intersectionsIn(plan plans.RecordQueryPlan) []*plans.RecordQueryIntersectionPlan {
	var out []*plans.RecordQueryIntersectionPlan
	var walk func(p plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if ip, ok := p.(*plans.RecordQueryIntersectionPlan); ok {
			out = append(out, ip)
		}
		for _, c := range p.GetChildren() {
			walk(c)
		}
	}
	walk(plan)
	return out
}

package sqldriver_test

// A primary-key intersection is sound only if its comparison key identifies a
// record within EVERY leg. Over PRIMARY KEY (pk1, pk2) with indexes (b, pk1)
// and (pk2), `WHERE b = 1 AND pk2 = 3` used to intersect the two covering scans
// on (pk1) alone — pk2 is equality-bound in the (pk2) leg, and the proof
// subtracted the UNION of the legs' equality-bound values from the primary key.
// The (b, pk1) leg holds several records per pk1 that differ only in pk2, so the
// merge emitted every b = 1 record whose pk1 also had SOME pk2 = 3 record: four
// rows on this fixture (six on the twin-table fixture that found it) for a
// query whose answer is one. Java 4.12.11.0 has the same defect
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
	// TI carries the legs that must NOT merge on (pk1): one fixes pk2, the
	// others sort it. TJ carries the accept arm: BOTH of its indexes fix pk2,
	// so the merge on (pk1) is sound there and must still be built.
	const ddl = "CREATE TABLE ti (pk1 BIGINT, pk2 BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (pk1, pk2)) " +
		"CREATE INDEX ti_a ON ti (a) " +
		"CREATE INDEX ti_b_pk1 ON ti (b, pk1) " +
		"CREATE INDEX ti_pk2 ON ti (pk2) " +
		"CREATE TABLE tj (pk1 BIGINT, pk2 BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (pk1, pk2)) " +
		"CREATE INDEX tj_a_pk2 ON tj (a, pk2) " +
		"CREATE INDEX tj_b_pk2 ON tj (b, pk2)"
	setup := openTestDB(t, "/testdb_pkilbc")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_pkilbc")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE pkilbc "+ddl)
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
	const rows = "(0, 2, 1, 1), (0, 3, 0, 0), (1, 4, 0, 1), (1, 3, 0, 0), " +
		"(2, 0, 0, 1), (2, 3, 0, 7), (3, 3, 1, 1), (4, 1, 0, 1), (5, 3, 1, 1), (6, 3, 1, 0)"
	mwjoMustExec(t, db, ctx, "INSERT INTO ti (pk1, pk2, a, b) VALUES "+rows)
	mwjoMustExec(t, db, ctx, "INSERT INTO tj (pk1, pk2, a, b) VALUES "+rows)

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
		table string // which table the query reads: "ti" (decline arms) or "tj" (accept arm)
		sql   string
		want  []string
	}{
		{"ti", "SELECT pk1, pk2, a, b FROM ti WHERE b = 1 AND pk2 = 3 ORDER BY pk1", []string{"3|3|1|1", "5|3|1|1"}},
		{"ti", "SELECT pk1, pk2, a, b FROM ti WHERE pk2 = 3 AND b = 1 ORDER BY pk1, pk2", []string{"3|3|1|1", "5|3|1|1"}},
		{"ti", "SELECT pk1 FROM ti WHERE b = 1 AND pk2 = 3 ORDER BY pk1", []string{"3", "5"}},
		{"ti", "SELECT COUNT(*) FROM ti WHERE b = 1 AND pk2 = 3", []string{"2"}},
		{"ti", "SELECT pk1, pk2, a, b FROM ti WHERE b = 1 AND pk2 = 3 AND pk1 > -1 ORDER BY pk1", []string{"3|3|1|1", "5|3|1|1"}},
		{"ti", "SELECT pk1, pk2, a, b FROM ti WHERE a = 1 AND b = 1 AND pk2 = 3 ORDER BY pk1", []string{"3|3|1|1", "5|3|1|1"}},
		{"ti", "SELECT pk1, pk2, a, b FROM ti WHERE a = 1 AND pk2 = 3 ORDER BY pk1", []string{"3|3|1|1", "5|3|1|1", "6|3|1|0"}},
		{"ti", "SELECT pk1, pk2, a, b FROM ti WHERE b = 1 AND pk2 IN (3, 4) ORDER BY pk1", []string{"1|4|0|1", "3|3|1|1", "5|3|1|1"}},
		{"ti", "SELECT pk1, pk2, a, b FROM ti WHERE b IN (1, 7) AND pk2 = 3 ORDER BY pk1", []string{"2|3|0|7", "3|3|1|1", "5|3|1|1"}},
		// Control: two legs that fix NO primary-key component still intersect
		// soundly on (pk1, pk2).
		{"ti", "SELECT pk1, pk2, a, b FROM ti WHERE a = 1 AND b = 1 ORDER BY pk1", []string{"0|2|1|1", "3|3|1|1", "5|3|1|1"}},
		// Accept arm: both legs fix pk2, so (pk1) identifies a record in each.
		{"tj", "SELECT pk1, pk2, a, b FROM tj WHERE a = 1 AND b = 1 AND pk2 = 3 ORDER BY pk1", []string{"3|3|1|1", "5|3|1|1"}},
		{"tj", "SELECT COUNT(*) FROM tj WHERE a = 1 AND b = 1 AND pk2 = 3", []string{"2"}},
	}
	for _, c := range cases {
		got := rowsOf(c.sql)
		if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("wrong rows\n  sql:  %s\n  plan: %s\n  got:  %v\n  want: %v", c.sql, explain(c.sql), got, c.want)
		}
	}

	// The property, not the shape: over TI no leg other than TI_PK2 fixes pk2
	// and TI_PK2 fixes nothing else, so any intersection built there must
	// compare on BOTH primary-key components — a planner that widens the
	// comparison key instead of declining the merge passes, one that re-admits
	// the (pk1)-only merge fails. Over TJ both legs fix pk2, so the sound
	// merge compares on (pk1) alone and MUST be built: that is the arm that
	// catches a proof declining every intersection whose legs share an equality.
	var tiIntersections, tjIntersections int
	for _, c := range cases {
		plan, err := embedded.PlanPhysicalForTest(c.sql, ddl, nil)
		if err != nil {
			t.Fatalf("plan %s: %v", c.sql, err)
		}
		for _, ip := range intersectionsIn(plan) {
			n := len(ip.GetComparisonKeyValues())
			switch c.table {
			case "ti":
				tiIntersections++
				if n != 2 {
					t.Errorf("TI intersection compares on %d key(s), the primary key has 2 components and no two legs fix the same one\n  sql:  %s\n  plan: %s",
						n, c.sql, plan.Explain())
				}
			case "tj":
				tjIntersections++
				if n != 1 {
					t.Errorf("TJ intersection compares on %d key(s); both legs fix pk2 so (pk1) alone is the sound key\n  sql:  %s\n  plan: %s",
						n, c.sql, plan.Explain())
				}
			}
		}
	}
	// Floors: the property loop asserts nothing over a plan set with no
	// intersections, and the fix DECLINES most of the TI shapes by design.
	if tiIntersections == 0 {
		t.Error("no intersection was built over TI at all — the control (a = 1 AND b = 1) is expected to build one; the plan-property arm is vacuous")
	}
	if tjIntersections == 0 {
		t.Error("no intersection was built over TJ — the per-leg proof is declining the SOUND merge whose legs all fix pk2")
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

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
// primaryKeyComponentsToCompare): a component may leave the key only when
// every leg fixes it to the same comparison. RFC-245 declined this shape; since
// RFC-247 the intersector compares on (pk1, pk2) — the order both legs deliver,
// the (pk2) leg trivially — and the merge is pinned per query below.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
	// others sort it — since RFC-247 they merge on (pk1, pk2) instead, the key
	// both legs deliver. TJ carries the accept arm: BOTH of its indexes fix pk2
	// to the same constant, so the merge on (pk1) is sound there and must
	// still be built.
	const ddl = "CREATE TABLE ti (pk1 BIGINT, pk2 BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (pk1, pk2)) " +
		"CREATE INDEX ti_a ON ti (a) " +
		"CREATE INDEX ti_b_pk1 ON ti (b, pk1) " +
		"CREATE INDEX ti_pk2 ON ti (pk2) " +
		"CREATE TABLE tj (pk1 BIGINT, pk2 BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (pk1, pk2)) " +
		"CREATE INDEX tj_a_pk2 ON tj (a, pk2) " +
		"CREATE INDEX tj_b_pk2 ON tj (b, pk2) " +
		// D drives the OR-union arm: the correlated inner of a LEFT JOIN is the
		// shape that reaches the union of index probes today.
		"CREATE TABLE d (did BIGINT, x BIGINT, y BIGINT, PRIMARY KEY (did))"
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
	// did=1 probes the two legs of the defect (b = 1, pk2 = 3); did=2 probes a
	// disjoint pair (b = 7, pk2 = 4); did=3 matches nothing.
	mwjoMustExec(t, db, ctx, "INSERT INTO d (did, x, y) VALUES (1, 1, 3), (2, 7, 4), (3, 9, 9)")

	explain := mwjoExplainer(t, db, ctx)
	rowsOf := func(q string) []string {
		t.Helper()
		out, err := mmRows(t, ctx, db, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return out
	}

	// merge names the plan the reproducer shapes MUST take since RFC-247 —
	// a positive assertion per query, not the conditional property arm below:
	// an intersection whose legs are exactly these indexes, comparing on
	// (PK1, PK2) in that order, in the stated direction. If the cost model
	// stops choosing it the arm fails here.
	type mergePin struct {
		legs    []string
		reverse bool
	}
	bPk1AndPk2 := &mergePin{legs: []string{"TI_B_PK1", "TI_PK2"}}
	bPk1AndPk2Reverse := &mergePin{legs: []string{"TI_B_PK1", "TI_PK2"}, reverse: true}
	cases := []struct {
		table string // which table the query reads: "ti" (widen arms) or "tj" (accept arm)
		sql   string
		want  []string
		merge *mergePin
	}{
		{"ti", "SELECT pk1, pk2, a, b FROM ti WHERE b = 1 AND pk2 = 3 ORDER BY pk1", []string{"3|3|1|1", "5|3|1|1"}, bPk1AndPk2},
		{"ti", "SELECT pk1, pk2, a, b FROM ti WHERE b = 1 AND pk2 = 3 ORDER BY pk1 DESC", []string{"5|3|1|1", "3|3|1|1"}, bPk1AndPk2Reverse},
		{"ti", "SELECT pk1, pk2, a, b FROM ti WHERE pk2 = 3 AND b = 1 ORDER BY pk1, pk2", []string{"3|3|1|1", "5|3|1|1"}, bPk1AndPk2},
		{"ti", "SELECT pk1 FROM ti WHERE b = 1 AND pk2 = 3 ORDER BY pk1", []string{"3", "5"}, bPk1AndPk2},
		{"ti", "SELECT COUNT(*) FROM ti WHERE b = 1 AND pk2 = 3", []string{"2"}, bPk1AndPk2},
		{"ti", "SELECT pk1, pk2, a, b FROM ti WHERE b = 1 AND pk2 = 3 AND pk1 > -1 ORDER BY pk1", []string{"3|3|1|1", "5|3|1|1"}, nil},
		{"ti", "SELECT pk1, pk2, a, b FROM ti WHERE a = 1 AND b = 1 AND pk2 = 3 ORDER BY pk1", []string{"3|3|1|1", "5|3|1|1"}, &mergePin{legs: []string{"TI_A", "TI_B_PK1", "TI_PK2"}}},
		{"ti", "SELECT pk1, pk2, a, b FROM ti WHERE a = 1 AND pk2 = 3 ORDER BY pk1", []string{"3|3|1|1", "5|3|1|1", "6|3|1|0"}, &mergePin{legs: []string{"TI_A", "TI_PK2"}}},
		{"ti", "SELECT pk1, pk2, a, b FROM ti WHERE b = 1 AND pk2 IN (3, 4) ORDER BY pk1", []string{"1|4|0|1", "3|3|1|1", "5|3|1|1"}, nil},
		{"ti", "SELECT pk1, pk2, a, b FROM ti WHERE b IN (1, 7) AND pk2 = 3 ORDER BY pk1", []string{"2|3|0|7", "3|3|1|1", "5|3|1|1"}, nil},
		// Control: two legs that fix NO primary-key component still intersect
		// soundly on (pk1, pk2).
		{"ti", "SELECT pk1, pk2, a, b FROM ti WHERE a = 1 AND b = 1 ORDER BY pk1", []string{"0|2|1|1", "3|3|1|1", "5|3|1|1"}, nil},
		// Accept arm: both legs fix pk2 to the same constant, so (pk1)
		// identifies a record in each.
		{"tj", "SELECT pk1, pk2, a, b FROM tj WHERE a = 1 AND b = 1 AND pk2 = 3 ORDER BY pk1", []string{"3|3|1|1", "5|3|1|1"}, nil},
		{"tj", "SELECT COUNT(*) FROM tj WHERE a = 1 AND b = 1 AND pk2 = 3", []string{"2"}, nil},
		// Union arm: the SAME two legs, (b, pk1) and (pk2), under OR. The union
		// dedups by the full primary key, so a component fixed in one leg only
		// cannot collapse two records; (3, 3) and (5, 3) satisfy both disjuncts
		// and must appear once each. Measured, because the union path's
		// soundness was otherwise established by reading alone.
		{
			"d", "SELECT d.did, t.pk1, t.pk2 FROM d LEFT JOIN ti AS t ON t.b = d.x OR t.pk2 = d.y ORDER BY d.did, t.pk1, t.pk2",
			[]string{"1|0|2", "1|0|3", "1|1|3", "1|1|4", "1|2|0", "1|2|3", "1|3|3", "1|4|1", "1|5|3", "1|6|3", "2|1|4", "2|2|3", "3|NULL|NULL"},
			nil,
		},
		{"d", "SELECT COUNT(*) FROM d LEFT JOIN ti AS t ON t.b = d.x OR t.pk2 = d.y", []string{"13"}, nil},
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
	// comparison key (RFC-247) passes, one that re-admits the (pk1)-only merge
	// fails. Over TJ both legs fix pk2, so the sound merge compares on (pk1)
	// alone and MUST be built: that is the arm that catches a proof declining
	// every intersection whose legs share an equality.
	var tiIntersections, tjIntersections, unions int
	for _, c := range cases {
		plan, err := embedded.PlanPhysicalForTest(c.sql, ddl, nil)
		if err != nil {
			t.Fatalf("plan %s: %v", c.sql, err)
		}
		if c.merge != nil {
			ips := intersectionsIn(plan)
			if len(ips) != 1 {
				t.Errorf("want exactly one intersection over %v, got %d\n  sql:  %s\n  plan: %s", c.merge.legs, len(ips), c.sql, plan.Explain())
			} else if legs := intersectionLegIndexes(ips[0]); strings.Join(legs, ",") != strings.Join(c.merge.legs, ",") ||
				strings.Join(comparisonKeyColumns(ips[0]), ",") != "PK1,PK2" || ips[0].IsReverse() != c.merge.reverse {
				t.Errorf("want Intersection over %v comparing on (PK1, PK2) reverse=%v; got legs %v, key %v, reverse=%v\n  sql:  %s\n  plan: %s",
					c.merge.legs, c.merge.reverse, legs, comparisonKeyColumns(ips[0]), ips[0].IsReverse(), c.sql, plan.Explain())
			}
		}
		if c.table == "d" {
			// The union arm proves nothing unless the plan actually unions the
			// two index probes; a nested-loop fallback answers the same rows.
			if n := unorderedUnionsIn(plan); n == 0 {
				t.Errorf("the OR arm no longer reaches an UnorderedUnion of the two index probes\n  sql:  %s\n  plan: %s", c.sql, plan.Explain())
			} else {
				unions += n
			}
			continue
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
	if unions == 0 {
		t.Error("no UnorderedUnion was built for the OR arms — the union path over the same two legs is unmeasured")
	}
}

// unorderedUnionsIn counts the UnorderedUnion plans in the typed plan tree.
func unorderedUnionsIn(plan plans.RecordQueryPlan) int {
	n := 0
	var walk func(p plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if _, ok := p.(*plans.RecordQueryUnorderedUnionPlan); ok {
			n++
		}
		for _, c := range p.GetChildren() {
			walk(c)
		}
	}
	walk(plan)
	return n
}

// intersectionLegIndexes names the index each leg of an intersection scans, in
// leg order; a leg that is not an index scan is named by its type.
func intersectionLegIndexes(ip *plans.RecordQueryIntersectionPlan) []string {
	var names []string
	for _, child := range ip.GetChildren() {
		switch leg := child.(type) {
		case *plans.RecordQueryIndexPlan:
			names = append(names, leg.GetIndexName())
		case *plans.RecordQueryCoveringIndexPlan:
			names = append(names, leg.GetIndexName())
		default:
			names = append(names, fmt.Sprintf("%T", child))
		}
	}
	return names
}

// comparisonKeyColumns renders an intersection's comparison key as column
// names in key order.
func comparisonKeyColumns(ip *plans.RecordQueryIntersectionPlan) []string {
	var names []string
	for _, kv := range ip.GetComparisonKeyValues() {
		if fv, ok := values.AsFieldValue(kv); ok {
			names = append(names, fv.DisplayName())
		} else {
			names = append(names, values.ExplainValue(kv))
		}
	}
	return names
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

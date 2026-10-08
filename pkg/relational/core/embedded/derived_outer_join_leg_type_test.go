package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestDerivedOuterJoinLegTypedByItsRow pins the row a join reads from a star
// derived table over an OUTER join. The dissolved box emits its legs' columns
// under their own names in FROM order (the FlatMap's row, and a FULL box's
// join row); the parent join's seed must type the derived leg the same way.
// Typed by the name model's qualified keys (W.ID, …) instead, reads of the
// leg that bound that row failed at execution (five of the FDB test's ten
// arms). A FULL body is the join that keeps its own leg; a LEFT or RIGHT body
// is one block with its parent, as an INNER one is: RewriteOuterJoinRule's
// canonical form survives REWRITING and SelectMergeRule dissolves it, as Java
// plans it (the leg flows the preserved row alone).
func TestDerivedOuterJoinLegTypedByItsRow(t *testing.T) {
	t.Parallel()
	// n.tags is the one NOT NULL column the DDL admits (an array), so it is the
	// column that shows the null-supplying side typed nullable.
	const ddl = `create table w(id bigint, f bigint, primary key(id)) ` +
		`create table h(id bigint, f bigint, primary key(id)) create table g(k bigint, v bigint, primary key(k)) ` +
		`create table n(k bigint, tags bigint array not null, primary key(k))`
	for _, tc := range []struct {
		body string
		want []string
		// nullableTags: the leg's TAGS column must be nullable (the
		// null-supplying side of an outer join) or NOT NULL (an inner join);
		// nil when the body has no TAGS column.
		nullableTags *bool
	}{
		{`SELECT * FROM w LEFT JOIN g ON g.k = w.id`, nil, nil},
		{`SELECT * FROM g RIGHT JOIN w ON g.k = w.id`, nil, nil},
		{`SELECT * FROM w FULL JOIN g ON g.k = w.id`, []string{"ID", "F", "K", "V"}, nil},
		{`SELECT * FROM w, g WHERE g.k = w.id`, nil, nil},
		{`SELECT * FROM w LEFT JOIN n ON n.k = w.id`, nil, nil},
		{`SELECT * FROM n RIGHT JOIN w ON n.k = w.id`, nil, nil},
		{`SELECT * FROM w, n WHERE n.k = w.id`, nil, nil},
		// FULL makes both sides null-supplying: n's TAGS is nullable on either
		// side.
		{`SELECT * FROM w FULL JOIN n ON n.k = w.id`, []string{"ID", "F", "K", "TAGS"}, ptrTo(true)},
		{`SELECT * FROM n FULL JOIN w ON n.k = w.id`, []string{"K", "TAGS", "ID", "F"}, ptrTo(true)},
		{`SELECT * FROM n LEFT JOIN w ON n.k = w.id`, nil, nil},
		{`SELECT * FROM w RIGHT JOIN n ON n.k = w.id`, nil, nil},
	} {
		t.Run(tc.body, func(t *testing.T) {
			t.Parallel()
			q := `SELECT a.id, d.id FROM (` + tc.body + `) AS a, h AS d WHERE d.f = 10`
			plan, err := PlanPhysicalForTest(q, ddl, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			var join plans.RecordQueryPlan
			var find func(p plans.RecordQueryPlan)
			find = func(p plans.RecordQueryPlan) {
				if join != nil {
					return
				}
				if _, ok := p.(*plans.RecordQueryNestedLoopJoinPlan); ok {
					join = p
					return
				}
				for _, c := range p.GetChildren() {
					find(c)
				}
			}
			find(plan)
			if join == nil {
				t.Fatalf("no join of the derived leg and d: %s", plan.Explain())
			}
			// The join reads the derived leg through the quantifier over it; that
			// quantifier's row is the leg's own row.
			var leg *values.RecordType
			var legNames string
			var legs []string
			for _, q := range join.GetQuantifiers() {
				qov, err := q.RequireFlowedObjectValue()
				if err != nil {
					t.Fatalf("join quantifier %v: %v", q.GetAlias(), err)
				}
				rt, ok := qov.FlowedType().(*values.RecordType)
				if !ok {
					continue
				}
				names := make([]string, len(rt.Fields))
				for i, f := range rt.Fields {
					names[i] = f.Name
				}
				legs = append(legs, strings.Join(names, ","))
				if len(rt.Fields) > 2 {
					leg, legNames = rt, strings.Join(names, ",")
				}
			}
			if tc.want == nil {
				if leg != nil {
					t.Fatalf("the body was not dissolved into the block: a join leg is typed %v: %s", leg, plan.Explain())
				}
				return
			}
			if legNames != strings.Join(tc.want, ",") {
				t.Fatalf("the join's legs are %v, want one typed by the derived row %v (its own row, not the name model's keys): %s",
					legs, tc.want, plan.Explain())
			}
			if tc.nullableTags != nil {
				for i, name := range tc.want {
					if name != "TAGS" {
						continue
					}
					if got := leg.Fields[i].FieldType.IsNullable(); got != *tc.nullableTags {
						t.Fatalf("the derived leg's TAGS is nullable=%v in %v, want %v", got, leg, *tc.nullableTags)
					}
				}
			}
		})
	}
}

func ptrTo(b bool) *bool { return &b }

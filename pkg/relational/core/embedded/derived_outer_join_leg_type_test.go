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
// arms). Every join kind the ordinal
// gate gates is driven: LEFT, RIGHT (the star order is the FROM order, g's
// columns first) and FULL, with an INNER body as the control.
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
		{`SELECT * FROM w LEFT JOIN g ON g.k = w.id`, []string{"ID", "F", "K", "V"}, nil},
		{`SELECT * FROM g RIGHT JOIN w ON g.k = w.id`, []string{"K", "V", "ID", "F"}, nil},
		{`SELECT * FROM w FULL JOIN g ON g.k = w.id`, []string{"ID", "F", "K", "V"}, nil},
		{`SELECT * FROM w, g WHERE g.k = w.id`, []string{"ID", "F", "K", "V"}, nil},
		{`SELECT * FROM w LEFT JOIN n ON n.k = w.id`, []string{"ID", "F", "K", "TAGS"}, ptrTo(true)},
		{`SELECT * FROM n RIGHT JOIN w ON n.k = w.id`, []string{"K", "TAGS", "ID", "F"}, ptrTo(true)},
		{`SELECT * FROM w, n WHERE n.k = w.id`, []string{"ID", "F", "K", "TAGS"}, ptrTo(false)},
		// FULL makes both sides null-supplying; the preserved side of a LEFT or
		// RIGHT join keeps its NOT NULL. n on FULL's left is the row that tells
		// FULL from LEFT: a FULL typed as a LEFT keeps n's TAGS NOT NULL.
		{`SELECT * FROM w FULL JOIN n ON n.k = w.id`, []string{"ID", "F", "K", "TAGS"}, ptrTo(true)},
		{`SELECT * FROM n FULL JOIN w ON n.k = w.id`, []string{"K", "TAGS", "ID", "F"}, ptrTo(true)},
		{`SELECT * FROM n LEFT JOIN w ON n.k = w.id`, []string{"K", "TAGS", "ID", "F"}, ptrTo(false)},
		{`SELECT * FROM w RIGHT JOIN n ON n.k = w.id`, []string{"ID", "F", "K", "TAGS"}, ptrTo(false)},
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
			rt, ok := join.GetResultValue().Type().(*values.RecordType)
			if !ok || len(rt.Fields) != len(tc.want)+2 {
				t.Fatalf("join row %v, want the leg's %d columns and d's two: %s",
					join.GetResultValue().Type(), len(tc.want), plan.Explain())
			}
			// The derived leg's columns lead or trail the join row, whichever
			// order the planner chose; find the run that is not d's.
			names := make([]string, len(rt.Fields))
			for i, f := range rt.Fields {
				names[i] = f.Name
			}
			start := 0
			if strings.Join(names[len(names)-len(tc.want):], ",") == strings.Join(tc.want, ",") {
				start = len(names) - len(tc.want)
			}
			leg := names[start : start+len(tc.want)]
			if strings.Join(leg, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("the derived leg's columns are %v in the join row %v, want %v (its own row, not the name model's keys)",
					leg, names, tc.want)
			}
			if tc.nullableTags != nil {
				for i, name := range tc.want {
					if name != "TAGS" {
						continue
					}
					if got := rt.Fields[start+i].FieldType.IsNullable(); got != *tc.nullableTags {
						t.Fatalf("the derived leg's TAGS is nullable=%v in %v, want %v", got, rt, *tc.nullableTags)
					}
				}
			}
		})
	}
}

func ptrTo(b bool) *bool { return &b }

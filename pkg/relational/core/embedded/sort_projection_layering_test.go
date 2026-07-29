package embedded

import (
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/parser"
	"fdb.dev/pkg/relational/core/query/logical"
)

// The SELECT-list projection is always built ABOVE the sort, never below it.
//
// This is a layering invariant of the logical builders, and it is load-bearing
// twice over. It is why the aggregate's private `[keys..., calls...]` row stays
// addressable to ORDER BY (buildSelectShell defers the reshaping projection past
// the sort — `postSortStripProj`), and it is what makes a whole family of
// "sort key over a projection output" handling in the translator unreachable:
// cascadesTranslator.translateSort once carried an `s.Input.(*logical.LogicalProject)`
// arm that resolved sort keys against the projection's output field NAMES. That
// arm was dead the day it was written, and the dead code hosted two of RFC-197's
// leaf-name identity decisions.
//
// The arm is gone. This test is what stands in its place: if a builder ever
// starts emitting Sort-over-Project, the shape must fail HERE, loudly, rather
// than silently arriving at a translator that no longer has a path for it and
// resolving the key by some other name-shaped route.
//
// Measured before removal, so the claim is not merely structural: a LOG probe at
// every `translateSort` entry over `go test ./pkg/relational/...` recorded the
// input's dynamic type as Filter, Scan, Aggregate, Join, CTE or Union — never
// Project — across every scenario in the tree.
func TestSortNeverSitsOverAProjection(t *testing.T) {
	t.Parallel()
	for _, sql := range []string{
		"SELECT a FROM t ORDER BY a",
		"SELECT a AS x FROM t ORDER BY x",
		"SELECT a, b FROM t WHERE b > 1 ORDER BY b, a",
		"SELECT a, SUM(b) FROM t GROUP BY a ORDER BY a",
		"SELECT a, SUM(b) AS s FROM t GROUP BY a ORDER BY s",
		"SELECT a, SUM(b) FROM t GROUP BY a ORDER BY SUM(b)",
		"SELECT a, COUNT(*) FROM t GROUP BY a HAVING COUNT(*) > 1 ORDER BY a",
		"SELECT a, b FROM t GROUP BY a, b ORDER BY b DESC, a",
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			sq := parseSelect(t, sql)
			op := buildLogicalPlanForSelect(sq)
			if op == nil {
				t.Fatalf("builder produced no plan for %q", sql)
			}
			walkLogical(op, func(n logical.LogicalOperator) {
				s, isSort := n.(*logical.LogicalSort)
				if !isSort {
					return
				}
				if p, isProj := s.Input.(*logical.LogicalProject); isProj {
					t.Errorf("Sort sits over a Project (%d projections) for %q\n%s\n\n"+
						"The SELECT-list projection must stay ABOVE the sort. A builder that "+
						"emits Sort-over-Project re-arms sort-key resolution against the "+
						"projection's output NAMES — the shape RFC-197 removed from "+
						"cascadesTranslator.translateSort as unreachable dead code. Restore the "+
						"layering, or the translator needs an ordinal-addressed pull-up before "+
						"this shape may exist.",
						len(p.Projections), sql, op.Explain(""))
				}
			})
		})
	}
}

// The positive half: for a grouped query the projection really is present and
// really is above the sort. Without this, the test above would pass just as
// happily on a builder that emitted no projection at all — an invariant that
// holds vacuously is not an invariant.
func TestGroupedSelectPutsItsProjectionAboveTheSort(t *testing.T) {
	t.Parallel()
	for _, sql := range []string{
		"SELECT a, SUM(b) FROM t GROUP BY a ORDER BY a",
		"SELECT a, SUM(b) AS s FROM t GROUP BY a ORDER BY s",
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			sq := parseSelect(t, sql)
			op := buildLogicalPlanForSelect(sq)
			if op == nil {
				t.Fatalf("builder produced no plan for %q", sql)
			}
			var sawProjectOverSort bool
			walkLogical(op, func(n logical.LogicalOperator) {
				p, isProj := n.(*logical.LogicalProject)
				if !isProj {
					return
				}
				if _, isSort := p.Input.(*logical.LogicalSort); isSort {
					sawProjectOverSort = true
				}
			})
			if !sawProjectOverSort {
				t.Fatalf("no Project directly over a Sort for %q — the deferred reshaping "+
					"projection (postSortStripProj) is what keeps the aggregate's private "+
					"[keys..., calls...] row addressable to ORDER BY\n%s", sql, op.Explain(""))
			}
		})
	}
}

// walkLogical visits op and every operator beneath it.
func walkLogical(op logical.LogicalOperator, visit func(logical.LogicalOperator)) {
	if op == nil {
		return
	}
	visit(op)
	for _, c := range op.Children() {
		walkLogical(c, visit)
	}
}

// assertNoSortOverProject is the shared assertion. Split out because the
// invariant is one invariant and it belongs to the LAYER, not to whichever
// builder a given test happens to drive.
func assertNoSortOverProject(t *testing.T, sql string, op logical.LogicalOperator) {
	t.Helper()
	walkLogical(op, func(n logical.LogicalOperator) {
		s, isSort := n.(*logical.LogicalSort)
		if !isSort {
			return
		}
		if p, isProj := s.Input.(*logical.LogicalProject); isProj {
			t.Errorf("Sort sits over a Project (%d projections) for %q\n%s\n\n"+
				"The SELECT-list projection must stay ABOVE the sort. "+
				"cascadesTranslator.translateSort REFUSES this shape (0AF00), because "+
				"it has no ordinal-addressed pull-up onto a projection's output and "+
				"the fall-through would resolve the sort key by leaf name in the wrong "+
				"ordinal domain. Restore the layering, or build the pull-up first.",
				len(p.Projections), sql, op.Explain(""))
		}
	})
}

// The layering invariant has SEVEN NewSort call sites in FOUR builder contexts,
// and the test above reaches exactly one of them.
//
//	buildSelectShell          logical_builder.go:694   ← the test above
//	visitOrderBy              plan_visitor.go:1616     ← here
//	union builders            logical_predicate.go:5771, :6936   ← here
//	buildCorrelatedScalar     logical_predicate.go:9246/:9487/:9510 ← here
//
// One covered site out of seven is not a defense of a layering invariant, it is
// a defense of one function. The three tests below close the other three
// contexts. They are deliberately separate functions rather than another SQL
// table in the first test: each drives a DIFFERENT entry point, and collapsing
// them would hide which builder a regression came from.

// buildViaPlanVisitor drives the production builder — the one the real
// connection uses — and returns its logical plan.
func buildViaPlanVisitor(t *testing.T, md *recordlayer.RecordMetaData, sql string) logical.LogicalOperator {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	sel := root.Statements().AllStatement()[0].SelectStatement()
	if sel == nil {
		t.Fatalf("not a SELECT statement: %q", sql)
	}
	op, err := NewPlanVisitor(md).VisitQuery(sel.Query())
	if err != nil {
		t.Fatalf("VisitQuery %q: %v", sql, err)
	}
	if op == nil {
		t.Fatalf("PlanVisitor produced no plan for %q", sql)
	}
	return op
}

// visitOrderBy (plan_visitor.go:1616) is the production ORDER BY builder — the
// one the real connection drives. It was entirely uncovered by the layering pin.
func TestSortNeverSitsOverAProjection_PlanVisitorPath(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	for _, sql := range []string{
		"SELECT order_id FROM Order ORDER BY order_id",
		"SELECT order_id AS x FROM Order ORDER BY x",
		"SELECT order_id, price FROM Order WHERE price > 1 ORDER BY price, order_id",
		"SELECT price, SUM(quantity) FROM Order GROUP BY price ORDER BY price",
		"SELECT price, SUM(quantity) AS s FROM Order GROUP BY price ORDER BY s",
		"SELECT price, COUNT(*) FROM Order GROUP BY price HAVING COUNT(*) > 1 ORDER BY price",
		"SELECT o.order_id, c.name FROM Order o, Customer c WHERE o.price = c.price ORDER BY c.name",
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			assertNoSortOverProject(t, sql, buildViaPlanVisitor(t, md, sql))
		})
	}
}

// The UNION builders (logical_predicate.go:5771, :6936) re-apply a set query's
// ORDER BY over the assembled union. A union leg carries its own SELECT-list
// projection, so this is the context where a Sort-over-Project is easiest to
// introduce by accident.
func TestSortNeverSitsOverAProjection_UnionBuilders(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	for _, sql := range []string{
		// Plain union builder (logical_predicate.go:6936).
		"SELECT order_id FROM Order UNION ALL SELECT customer_id FROM Customer ORDER BY order_id",
		"SELECT price FROM Order WHERE price > 1 UNION ALL SELECT price FROM Customer ORDER BY price DESC",
		// CTE union builder (logical_predicate.go:5771) — the same lift, reached
		// through the WITH-scoped entry point.
		"WITH d AS (SELECT price FROM Order) SELECT price FROM d UNION ALL SELECT price FROM Customer ORDER BY price",
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			assertNoSortOverProject(t, sql, buildViaPlanVisitor(t, md, sql))
		})
	}
}

// buildCorrelatedScalar (logical_predicate.go:9246/:9487/:9510) builds the
// inner plan of a correlated scalar subquery, and applies the subquery's own
// ORDER BY inside it. Three of the seven NewSort sites live here — the densest
// context, and the one furthest from anybody's attention.
func TestSortNeverSitsOverAProjection_CorrelatedScalarBuilder(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	for _, sql := range []string{
		// Ungrouped arm (:9510) — the sort runs over raw scan rows before LIMIT 1.
		"SELECT c.name, (SELECT o.price FROM Order o WHERE o.price = c.price ORDER BY o.order_id LIMIT 1) FROM Customer c",
		"SELECT c.name, (SELECT o.order_id FROM Order o WHERE o.price = c.price ORDER BY o.price DESC LIMIT 1) FROM Customer c",
		"SELECT c.name FROM Customer c WHERE c.price > (SELECT o.price FROM Order o WHERE o.order_id = c.customer_id ORDER BY o.price LIMIT 1)",
		// Aggregate arm (:9246) — the sort runs over the grouped output.
		"SELECT c.name, (SELECT SUM(o.price) FROM Order o WHERE o.price = c.price GROUP BY o.quantity ORDER BY SUM(o.price) LIMIT 1) FROM Customer c",
		// Group-key-only arm (:9487) — grouped output, no aggregate selected.
		"SELECT c.name, (SELECT o.quantity FROM Order o WHERE o.price = c.price GROUP BY o.quantity ORDER BY o.quantity LIMIT 1) FROM Customer c",
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			op := buildViaPlanVisitor(t, md, sql)
			assertNoSortOverProject(t, sql, op)
			// The correlated-scalar plans hang OFF the operator tree, not
			// inside it, so walkLogical alone never sees the three NewSort
			// sites this test exists for.
			var seenSubqueries int
			walkLogical(op, func(n logical.LogicalOperator) {
				for _, attached := range logical.AttachedPlans(n) {
					seenSubqueries++
					walkLogical(attached, func(inner logical.LogicalOperator) {
						assertNoSortOverProject(t, sql, inner)
					})
				}
			})
			if seenSubqueries == 0 {
				t.Fatalf("no correlated scalar subquery plan reached the assertion for %q — "+
					"this test would pass vacuously, which is how the builder it "+
					"targets stayed uncovered in the first place\n%s", sql, op.Explain(""))
			}
		})
	}
}

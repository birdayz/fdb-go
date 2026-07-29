package embedded

import (
	"testing"

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

package query

import (
	"testing"

	"fdb.dev/pkg/relational/core/query/logical"
)

// RFC-173 QP-REF-BIND item 2, commit 5b — the EXISTS-rider clusterArity
// poison lift. A cluster LEG whose body carries an EXISTS filter (a CTE /
// derived table `SELECT … WHERE EXISTS(…)`) poisoned clusterArity and failed
// ordinalEligible, so the WHOLE enclosing cluster stayed name-model — even
// though post-flattening the existential quantifier rides the merged select,
// which the 2+1 flatten's ordinal seed machinery already handles, and the
// leg's own output is the SOURCE row (the existential FlatMap's identity RV),
// a single-namespace row the seed types like any scan. The rider classes the
// 5a/5c work made safe (EXISTS subqueries — positional; uncorrelated scalar
// subqueries — root-context bindings) are now TRANSPARENT to both walks.

// TestRFC173Item2C5b_ExistsRiderLegGates pins the lift: the confirmed red
// shape (CTE leg with an EXISTS-filter body inside a 2-way inner cluster) now
// counts arity 2 and GATES ordinal.
func TestRFC173Item2C5b_ExistsRiderLegGates(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	body := &logical.LogicalFilter{
		Input:            scan("Customer", "c"),
		ExistsSubqueries: []logical.ExistsSubquery{{Plan: scan("Order", "x")}},
	}
	tr.cteScope["D"] = body
	j := logical.NewJoin(scan("Order", "o"), logical.NewScan("D", "d"), logical.JoinInner, "")

	if got := tr.clusterArity(j); got != 2 {
		t.Fatalf("clusterArity(join with EXISTS-rider leg) = %d, want 2 (the rider filter is transparent)", got)
	}
	dec := tr.ordinalWedgeGate(j)
	if !dec.Gated || dec.Arity != 2 {
		t.Fatalf("wedgeGate = gated=%v arity=%d (%q), want gated arity-2", dec.Gated, dec.Arity, dec.Reason)
	}
}

// TestRFC173Item2C5b_ScalarRiderLegGates pins the UNCORRELATED scalar rider:
// a filter-level scalar subquery is a root-context external binding
// (shape-agnostic — the 5c ruling), so it is transparent too.
func TestRFC173Item2C5b_ScalarRiderLegGates(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	body := &logical.LogicalFilter{
		Input:            scan("Customer", "c"),
		ScalarSubqueries: []logical.ScalarSubquery{{Plan: scan("Order", "x")}},
	}
	tr.cteScope["D"] = body
	j := logical.NewJoin(scan("Order", "o"), logical.NewScan("D", "d"), logical.JoinInner, "")

	if got := tr.clusterArity(j); got != 2 {
		t.Fatalf("clusterArity(join with scalar-rider leg) = %d, want 2", got)
	}
	if dec := tr.ordinalWedgeGate(j); !dec.Gated {
		t.Fatalf("wedgeGate not gated (%q), want gated", dec.Reason)
	}
}

// TestRFC173Item2C5b_CorrelatedScalarRiderStaysPoisoned is the NEGATIVE
// control: a projection carrying a CORRELATED scalar subquery (per-row
// evaluation — needs the W4b clusterPullUp rework, booked) still poisons.
func TestRFC173Item2C5b_CorrelatedScalarRiderStaysPoisoned(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	body := &logical.LogicalProject{
		Input: scan("Customer", "c"),
		CorrelatedScalarSubqueries: []logical.CorrelatedScalarSubquery{
			{InnerPlan: scan("Order", "x")},
		},
	}
	tr.cteScope["D"] = body
	j := logical.NewJoin(scan("Order", "o"), logical.NewScan("D", "d"), logical.JoinInner, "")

	if got := tr.clusterArity(j); got != arityPoison {
		t.Fatalf("clusterArity(join with correlated-scalar-rider leg) = %d, want poison %d", got, arityPoison)
	}
	if dec := tr.ordinalWedgeGate(j); dec.Gated {
		t.Fatalf("wedgeGate gated a correlated-scalar-rider leg (%q) — must stay name-model", dec.Reason)
	}
}

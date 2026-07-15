package query

import (
	"testing"

	"fdb.dev/pkg/relational/core/query/logical"
)

// A cluster LEG whose body carries an EXISTS filter (a CTE /
// derived table `SELECT … WHERE EXISTS(…)`) does NOT poison
// clusterArity or fail ordinalEligible: post-flattening the existential
// quantifier rides the merged select,
// which the 2+1 flatten's ordinal seed machinery already handles, and the
// leg's own output is the SOURCE row (the existential FlatMap's identity RV),
// a single-namespace row the seed types like any scan. EXISTS subqueries
// (positional) and uncorrelated scalar
// subqueries (root-context bindings) are TRANSPARENT to both walks.

// TestExistsRiderLegGates pins the rule: a
// CTE leg with an EXISTS-filter body inside a 2-way inner cluster
// counts arity 2 and GATES ordinal.
func TestExistsRiderLegGates(t *testing.T) {
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

// TestScalarRiderLegGates pins the UNCORRELATED scalar rider:
// a filter-level scalar subquery is a root-context external binding
// (shape-agnostic), so it is transparent too.
func TestScalarRiderLegGates(t *testing.T) {
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

// TestCorrelatedScalarRiderStaysPoisoned is the NEGATIVE
// control: a projection carrying a CORRELATED scalar subquery (per-row
// evaluation — needs a clusterPullUp extension, booked) still poisons.
func TestCorrelatedScalarRiderStaysPoisoned(t *testing.T) {
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

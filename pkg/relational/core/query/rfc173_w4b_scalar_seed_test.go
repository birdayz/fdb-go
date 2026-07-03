package query

// RFC-173 W4b — white-box pins for the correlated-scalar ordinal seed. The
// ordinal-vs-name-model choice is invisible in EXPLAIN (both flow the same
// rows), so these prove the SEED SHAPE directly; the FDB e2e
// (scalar_subq_ordinal_seed_fdb_test.go) proves execution + qualified/bare
// outer-ref resolution.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// TestRFC173W4b_ScalarSeed_Shape pins the ordinal seed a SINGLE-SOURCE outer
// produces: every outer column a baked ofOrdinal(QOV(outer), i), then ONE inner
// ofOrdinal(QOV(inner), 0) named EXACTLY <inner>.<scalarCol> and NULLABLE
// (LEFT-OUTER null-fill). AssertOrdinalJoinSeed (called inside the builder)
// enforces the pinned/single-accessor/full-leg-run invariants; here we re-check
// the W4b-specific inner shape.
func TestRFC173W4b_ScalarSeed_Shape(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	seed := tr.scalarSubqueryOrdinalSeed("C", scan("Customer", "c"), "SQ", "MAXORDER")
	if seed == nil {
		t.Fatal("single-source outer must ordinalize, got nil (declined)")
	}
	rc, ok := seed.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("seed = %T, want *RecordConstructorValue", seed)
	}
	if rc.AnchoredJoin {
		t.Fatal("W4b seed must be the ORDINAL seed, not the name-model anchored record")
	}
	// Every field is a baked ofOrdinal reference (AssertOrdinalJoinSeed already
	// verified this in the builder; re-assert defensively).
	values.AssertOrdinalJoinSeed(rc)

	outerType := tr.ordinalLegType(scan("Customer", "c"))
	wantOuter := len(outerType.Fields)
	if len(rc.Fields) != wantOuter+1 {
		t.Fatalf("seed has %d fields, want %d (outer %d cols + 1 inner scalar)", len(rc.Fields), wantOuter+1, wantOuter)
	}

	// The LAST field is the inner scalar leg: named <inner>.<scalarCol>, NULLABLE,
	// a baked ordinal-0 reference.
	last := rc.Fields[len(rc.Fields)-1]
	if last.Name != "SQ.MAXORDER" {
		t.Errorf("inner field name = %q, want SQ.MAXORDER (what replaceScalarSubqueryRef reads)", last.Name)
	}
	ifv, ok := last.Value.(*values.FieldValue)
	if !ok || ifv.Resolved == nil {
		t.Fatalf("inner field is %T, want a baked *FieldValue", last.Value)
	}
	if !ifv.Typ.IsNullable() {
		t.Errorf("inner scalar ordinal must be NULLABLE-wrapped (LEFT-OUTER null-fill), got %s", ifv.Typ)
	}
}

// TestRFC173W4b_ScalarSeed_OuterGate pins the OUTER-ONLY single-source gate: a
// single-table outer ordinalizes (clusterArity==1), a multi-table outer cluster
// stays name-model (clusterArity>1 — the caller declines the ordinal seed). This
// is the divergence from W4-LEFT (which gates both legs): ordinalizing a
// flattened outer cluster would erase its buried source names.
func TestRFC173W4b_ScalarSeed_OuterGate(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	// Single-source outer → clusterArity 1 → the caller ordinalizes.
	if got := tr.clusterArity(scan("Customer", "c")); got != 1 {
		t.Fatalf("single-table outer clusterArity = %d, want 1 (ordinalizes)", got)
	}
	// Multi-table outer cluster → clusterArity 2 → the caller keeps name-model.
	cluster := inner(scan("Customer", "c"), scan("Order", "o"))
	if got := tr.clusterArity(cluster); got == 1 {
		t.Fatalf("a 2-table outer cluster must NOT be single-source (clusterArity==1); got %d — it would ordinalize and erase buried names", got)
	}
	var _ logical.LogicalOperator = cluster
}

// TestRFC173W4b_ScalarSeed_InnerJoinGate pins the INNER-JOIN gate (the correction
// to the original "inner needs no gate" ruling). A join-inner scalar subquery
// names its first table under the same alias csq.InnerAlias carries, so the inner
// ordinal join's typed QOV(InnerAlias) collides with the seed's typed inner leg
// (the executor's widenLegTypesFromPlan DIVERGENT-baked-types panic). Such an
// inner MUST stay name-model. A single-table inner (even wrapped in an aggregate)
// ordinalizes.
func TestRFC173W4b_ScalarSeed_InnerJoinGate(t *testing.T) {
	t.Parallel()

	// Single-table inner (possibly aggregated/filtered/limited) → no join → ordinalizes.
	single := logical.NewLimit(scan("Order", "o"), 1, 0)
	if innerContainsJoin(single) {
		t.Error("a single-table inner (Limit(Scan)) must NOT be gated as a join-inner")
	}
	if innerContainsJoin(scan("Order", "o")) {
		t.Error("a bare single-table scan inner must NOT be gated as a join-inner")
	}

	// Join inner → the collision case → MUST be gated to name-model.
	joinInner := inner(scan("Order", "o"), scan("Customer", "c"))
	if !innerContainsJoin(joinInner) {
		t.Fatal("a join inner MUST be gated (its first-table alias collides with the seed's typed inner QOV)")
	}
	// A join buried under an aggregate (SELECT COUNT(*) FROM a JOIN b …) — the exact
	// shape that panicked, since clusterArity returns 1 for the aggregate without
	// recursing into the join.
	aggOverJoin := logical.NewAggregate(joinInner, nil, []string{"COUNT(*)"}, nil, "")
	if !innerContainsJoin(aggOverJoin) {
		t.Fatal("a join buried under an aggregate MUST be gated (clusterArity can't see it)")
	}
}

// TestRFC173W4b_ScalarSeed_ComputedGate pins the SECOND inner gate
// (innerScalarIsRowColumn): the scalar must be present in the inner row the
// ordinal leg adapter reads — an AGGREGATE output or a stored column. A COMPUTED
// scalar (whose column is a synthesized expression name, not a stored column and
// not an aggregate) is NOT, so it stays name-model (else the ordinal leg adapter
// rejects the name-model merge row). Aggregate detection recurses.
func TestRFC173W4b_ScalarSeed_ComputedGate(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	// An aggregate inner is always safe (collapses to a scalar-keyed row).
	agg := logical.NewAggregate(scan("Order", "o"), nil, []string{"COUNT(*)"}, nil, "")
	if !innerHasAggregate(agg) {
		t.Error("an aggregate inner must be detected as safe (scalar-keyed row)")
	}
	if !tr.innerScalarIsRowColumn(agg, "COUNT(*)") {
		t.Error("aggregate scalar must ordinalize (innerScalarIsRowColumn)")
	}
	if innerHasAggregate(scan("Order", "o")) {
		t.Error("a bare scan inner has no aggregate")
	}

	// A plain stored column of the single-source inner ordinalizes; a synthesized
	// computed name (not a stored column, no aggregate) stays name-model.
	orderCols := tr.legColumns(scan("Order", "o"))
	if len(orderCols) == 0 {
		t.Fatal("expected the Order table to have columns")
	}
	storedCol := strings.ToUpper(orderCols[0].Name)
	if !tr.innerScalarIsRowColumn(scan("Order", "o"), storedCol) {
		t.Errorf("plain stored column %q must ordinalize", storedCol)
	}
	if tr.innerScalarIsRowColumn(scan("Order", "o"), "UPPER(SOMECOL)") {
		t.Error("a computed scalar (synthesized name, not a stored column) must stay name-model")
	}
}

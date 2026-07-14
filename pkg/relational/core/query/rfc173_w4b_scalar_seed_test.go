package query

// RFC-173 W4b — white-box pins for the correlated-scalar ordinal seed. The
// ordinal-vs-name-model choice is invisible in EXPLAIN (both flow the same
// rows), so these prove the SEED SHAPE directly; the FDB e2e
// (scalar_subq_ordinal_seed_fdb_test.go) proves execution + qualified/bare
// outer-ref resolution.

import (
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

	seed := tr.scalarSubqueryOrdinalSeed("C", scan("Customer", "c"), values.UniqueCorrelationIdentifier(), "SQ", "MAXORDER")
	if seed == nil {
		t.Fatal("single-source outer must ordinalize, got nil (declined)")
	}
	rc, ok := seed.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("seed = %T, want *RecordConstructorValue", seed)
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

// TestRFC173W4b_ScalarSeed_OuterGate pins the single-source arity facts the
// dispatch routes on: a single-table outer is clusterArity==1 (THIS seed), a
// multi-table outer cluster is clusterArity>1 — routed to the shape-1
// clustered-outer path (gated clusters ordinalize with dotted leg naming;
// ungated ones keep the name-model fallback or decline).
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

// TestRFC173W4b_ScalarSeed_UniqueInnerCorrelation pins the shape-2 decouple
// (and buries the former InnerJoinGate test with its gate): the seed's inner
// leg is keyed by a FRESH unique correlation id, never the SQL alias — a
// JOIN-inner's own typed QOV(InnerAlias, N-field) would collide with an
// alias-keyed 1-field seed leg at the executor's widenLegTypesFromPlan (the
// DIVERGENT-baked-types tripwire that used to force join-inners to the name
// model). The RC field NAME keeps the SQL alias (the projection's read key);
// only the correlation is decoupled, and two seeds never share an id.
func TestRFC173W4b_ScalarSeed_UniqueInnerCorrelation(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	innerCorr := values.UniqueCorrelationIdentifier()
	seed := tr.scalarSubqueryOrdinalSeed("C", scan("Customer", "c"), innerCorr, "SQ", "MAXORDER")
	if seed == nil {
		t.Fatal("single-source outer must ordinalize")
	}
	rc := seed.(*values.RecordConstructorValue)
	last := rc.Fields[len(rc.Fields)-1]
	if last.Name != "SQ.MAXORDER" {
		t.Errorf("inner field name = %q, want SQ.MAXORDER (the SQL-alias read key)", last.Name)
	}
	qov := last.Value.(*values.FieldValue).Child.(*values.QuantifiedObjectValue)
	if qov.Correlation != innerCorr {
		t.Errorf("inner leg keyed by %s, want the fresh unique id %s", qov.Correlation, innerCorr)
	}
	if qov.Correlation.Name() == "SQ" {
		t.Error("inner leg keyed by the SQL alias — the JOIN-inner type collision returns")
	}

	// Two seeds must never share an inner correlation (uniqueness is the whole
	// decouple).
	seed2 := tr.scalarSubqueryOrdinalSeed("C", scan("Customer", "c"), values.UniqueCorrelationIdentifier(), "SQ", "MAXORDER")
	qov2 := seed2.(*values.RecordConstructorValue).Fields[len(rc.Fields)-1].Value.(*values.FieldValue).Child.(*values.QuantifiedObjectValue)
	if qov2.Correlation == qov.Correlation {
		t.Error("two seeds share an inner correlation id")
	}
}

// The former ComputedGate test is GONE with its subject (innerScalarIsRowColumn):
// RFC-173 W4b shape 3 removed that guard. A computed correlated scalar no longer
// "stays name-model" (where it resolved to a silent NULL) — it is materialized as
// the inner subquery's projected output and ordinalizes like any other scalar.
// End-to-end coverage: sqldriver's TestFDB_RFC173W4b_ComputedScalar (UPPER + arith
// computed scalars return correct rows, not NULL).

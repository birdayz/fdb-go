package query

// White-box pins for the correlated-scalar ordinal seed. The
// ordinal-vs-name-model choice is invisible in EXPLAIN (both flow the same
// rows), so these prove the SEED SHAPE directly; the FDB end-to-end scalar
// subquery seed test proves execution + qualified/bare outer-ref resolution.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// exactScalarInner models the materialized one-column output that reaches the
// seed builder in production. Passing a base scan while asking for a synthetic
// title such as MAXORDER is no longer a valid fixture: exact type resolution
// correctly declines a title absent from that scan's schema.
func exactScalarInner(t testing.TB, title string, typ values.Type) logical.LogicalOperator {
	t.Helper()
	return &logical.LogicalProject{
		Input:           scan("Order", "sq"),
		Projections:     []string{title},
		ProjectedValues: []values.Value{exactTestNamedField(t, "SQ_SOURCE", title, typ)},
	}
}

// TestScalarSeed_Shape pins the ordinal seed a SINGLE-SOURCE outer produces:
// every outer column a baked ofOrdinal(QOV(outer), i), then ONE inner
// ofOrdinal(QOV(inner), 0) named EXACTLY <inner>.<scalarCol> and NULLABLE
// (LEFT-OUTER null-fill). AssertOrdinalJoinSeed (called inside the builder)
// enforces the pinned/single-accessor/full-leg-run invariants; here we
// re-check the scalar-seed-specific inner shape.
func TestScalarSeed_Shape(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	inner := exactScalarInner(t, "MAXORDER", values.NotNullLong)

	seed := tr.scalarSubqueryOrdinalSeed("C", scan("Customer", "c"), inner, values.UniqueCorrelationIdentifier(), "SQ", "MAXORDER")
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
	ifv, ok := values.AsFieldValue(last.Value)
	if !ok {
		t.Fatalf("inner field is %T, want a baked *FieldValue", last.Value)
	}
	if !ifv.ResultType().IsNullable() {
		t.Errorf("inner scalar ordinal must be NULLABLE-wrapped (LEFT-OUTER null-fill), got %s", ifv.ResultType())
	}
	innerOwner, ok := values.AsQuantifiedObjectValue(ifv.ChildValue())
	if !ok {
		t.Fatalf("inner scalar owner = %T, want exact QOV", ifv.ChildValue())
	}
	if !innerOwner.Type().IsNullable() {
		t.Errorf("inner scalar source row must be NULLABLE for the LEFT-OUTER leg, got %s", innerOwner.Type())
	}
}

// A materialised aggregate is one exact positional field whose display title
// contains a dot inside an expression.  The dot is not a qualifier boundary;
// parsing `SUM(C.VAL)` as one made the scalar lookup search for `VAL)` and
// decline an otherwise exact ordinal seed.
func TestScalarSeed_AggregateDisplayTitleUsesTheOnlyExactSlot(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	inner := exactScalarInner(t, "SUM(C.VAL)", values.NullableLong)

	seed := tr.scalarSubqueryOrdinalSeed("C", scan("Customer", "c"), inner,
		values.UniqueCorrelationIdentifier(), "SQ", "SUM(C.VAL)")
	if seed == nil {
		t.Fatal("one-field aggregate projection must ordinalize by its exact slot, not parse its display title")
	}
	rc := seed.(*values.RecordConstructorValue)
	got := exactTestFieldView(t, rc.Fields[len(rc.Fields)-1].Value).ResultType()
	if got.Code() != values.TypeCodeLong || !got.IsNullable() {
		t.Fatalf("aggregate scalar slot type = %v, want nullable LONG", got)
	}
}

// TestScalarSeed_OuterGate pins the single-source arity facts the dispatch
// routes on: a single-table outer is clusterArity==1 (THIS seed), a
// multi-table outer cluster is clusterArity>1 — routed to the
// clustered-outer path (gated clusters ordinalize with dotted leg naming;
// ungated ones keep the name-model fallback or decline).
func TestScalarSeed_OuterGate(t *testing.T) {
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

// TestScalarSeed_UniqueInnerCorrelation pins the inner-correlation decouple
// (this replaces the former InnerJoinGate test along with the gate it
// pinned): the seed's inner leg is keyed by a FRESH unique correlation id,
// never the SQL alias — a JOIN-inner's own typed QOV(InnerAlias, N-field)
// would collide with an alias-keyed 1-field seed leg at the executor's
// widenLegTypesFromPlan (the divergent-baked-types tripwire that used to
// force join-inners to the name model). The RC field NAME keeps the SQL
// alias (the projection's read key); only the correlation is decoupled, and
// two seeds never share an id.
func TestScalarSeed_UniqueInnerCorrelation(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	inner := exactScalarInner(t, "MAXORDER", values.NotNullLong)

	innerCorr := values.UniqueCorrelationIdentifier()
	seed := tr.scalarSubqueryOrdinalSeed("C", scan("Customer", "c"), inner, innerCorr, "SQ", "MAXORDER")
	if seed == nil {
		t.Fatal("single-source outer must ordinalize")
	}
	rc := seed.(*values.RecordConstructorValue)
	last := rc.Fields[len(rc.Fields)-1]
	if last.Name != "SQ.MAXORDER" {
		t.Errorf("inner field name = %q, want SQ.MAXORDER (the SQL-alias read key)", last.Name)
	}
	innerField := exactTestFieldView(t, last.Value)
	qov, ok := values.AsQuantifiedObjectValue(innerField.ChildValue())
	if !ok {
		t.Fatalf("inner scalar owner = %T, want exact QOV", innerField.ChildValue())
	}
	if qov.Correlation() != innerCorr {
		t.Errorf("inner leg keyed by %s, want the fresh unique id %s", qov.Correlation(), innerCorr)
	}
	if qov.Correlation().Name() == "SQ" {
		t.Error("inner leg keyed by the SQL alias — the JOIN-inner type collision returns")
	}

	// Two seeds must never share an inner correlation (uniqueness is the whole
	// decouple).
	seed2 := tr.scalarSubqueryOrdinalSeed("C", scan("Customer", "c"), inner, values.UniqueCorrelationIdentifier(), "SQ", "MAXORDER")
	secondField := exactTestFieldView(t, seed2.(*values.RecordConstructorValue).Fields[len(rc.Fields)-1].Value)
	qov2, ok := values.AsQuantifiedObjectValue(secondField.ChildValue())
	if !ok {
		t.Fatalf("second inner scalar owner = %T, want exact QOV", secondField.ChildValue())
	}
	if qov2.Correlation() == qov.Correlation() {
		t.Error("two seeds share an inner correlation id")
	}
}

// The former ComputedGate test is GONE along with its subject
// (innerScalarIsRowColumn): that guard is removed. A computed correlated
// scalar no longer "stays name-model" (where it resolved to a silent NULL) —
// it is materialized as the inner subquery's projected output and
// ordinalizes like any other scalar. End-to-end coverage: sqldriver's
// computed-scalar FDB test (UPPER + arithmetic computed scalars return
// correct rows, not NULL).

// TestScalarSeed_InnerScalarTypeFlowsFromInner pins that the seed's inner
// scalar leg carries the INNER subquery's OWN column type rather than a
// placeholder. The type is known at the catalog, so discarding it here is what
// forced the metadata surface to re-derive the column's type by NAME — and a
// bare-name search across a join's leaf descriptors first-matches, so a scalar
// selecting one leg's column got typed from the OTHER leg's descriptor.
//
// Order.PRICE is int32 (INTEGER), deliberately NOT the int64/BIGINT an
// unresolved column falls back to downstream: an Unknown leg type is
// indistinguishable from a correct BIGINT answer at the metadata surface, so a
// BIGINT column could not express this defect.
func TestScalarSeed_InnerScalarTypeFlowsFromInner(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)

	innerOp := scan("Order", "sq")
	seed := tr.scalarSubqueryOrdinalSeed("C", scan("Customer", "c"), innerOp,
		values.UniqueCorrelationIdentifier(), "SQ", "PRICE")
	if seed == nil {
		t.Fatal("single-source outer must ordinalize")
	}
	rc := seed.(*values.RecordConstructorValue)
	last := rc.Fields[len(rc.Fields)-1]
	ifv := exactTestFieldView(t, last.Value)
	innerType := ifv.ResultType()

	if innerType == nil || innerType.Code() == values.TypeCodeUnknown {
		t.Fatalf("inner scalar leg typed %v — the catalog knows Order.PRICE, so an "+
			"Unknown here throws away a known type and forces a name-keyed "+
			"re-derivation downstream (the cross-leg same-name-different-type "+
			"wrong-metadata bug)", innerType)
	}
	// It must be the INNER's type, not merely "some type": compare against the
	// inner leg's own derived column.
	var want values.Type
	for _, c := range tr.legColumns(innerOp) {
		if c.Name == "PRICE" {
			want = c.FieldType
		}
	}
	if want == nil {
		t.Fatal("fixture drift: Order has no PRICE column to flow")
	}
	if innerType.Code() != want.Code() {
		t.Errorf("inner scalar leg type code = %v, want %v (Order.PRICE's own type)",
			innerType.Code(), want.Code())
	}
	// Nullability is independent of the flowed type and is always true here:
	// the join is LEFT-OUTER, so an unmatched outer row NULL-fills this slot.
	if !innerType.IsNullable() {
		t.Errorf("inner scalar ordinal must stay NULLABLE-wrapped after typing, got %s", innerType)
	}
}

package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// layoutOf builds a row layout — the flowed type a scan/index/fetch leg emits,
// and the layout its own column references index (RFC-197's ordinal DOMAIN).
func layoutOf(name string, cols ...string) *values.RecordType {
	fields := make([]values.Field, len(cols))
	for i, c := range cols {
		fields[i] = values.Field{Name: c, FieldType: values.UnknownType, Ordinal: i}
	}
	return values.NewRecordType(name, false, fields)
}

// innerLayout / outerLayout are the two join legs' row layouts, shared by the
// tests below so every leg's flowed type, its unique-key metadata and its
// predicate operands all speak about the same declared column order.
var (
	innerLayout = layoutOf("Inner", "ID", "OTHER", "STATUS", "ADDRESS", "CATEGORY", "TAGS", "TENANT", "ORDER")
	outerLayout = layoutOf("Outer", "FK", "TENANT_FK", "ORDER_FK", "CAT_FK", "TAGS_FK")
)

// fieldOfAliasIn builds the PRODUCTION shape of a join predicate's per-side
// comparand: a BAKED correlated reference carrying the ordinal of col within
// layout AND the domain token for layout — exactly what the SQL resolver mints
// for `alias.col` (expr.go's sourceColumnOrdinal derives the ordinal and the
// domain from the source's declared column list in the same breath).
//
// The unique-key proof is now stated against a layout rather than against a
// spelling, so a reference that cannot say which layout its ordinal indexes
// gets no answer — pinned by TestNestedLoopJoinPlan_HintCost_LazyOperand_Unaffected.
func fieldOfAliasIn(layout *values.RecordType, col, alias string) values.Value {
	ord, ok := layout.FieldIndex(col)
	if !ok {
		panic("test layout " + layout.RecordName + " does not declare " + col)
	}
	return values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(alias)),
		col, ord, values.UnknownType, values.OrdinalDomainOfType(layout),
	)
}

// pkOf builds the leaf-relative primary-key Values a scan carries: METADATA
// name carriers, lazy exactly as GetPrimaryKeyValues() stamps them. They are
// the one place a name is legitimate, and it dies at the boundary —
// innerLeafUniqueKeyOrdinals resolves each against the leg's stated layout
// once and matches ordinals from there on.
func pkOf(cols ...string) []values.Value {
	out := make([]values.Value, len(cols))
	for i, c := range cols {
		out[i] = &values.FieldValue{Field: c, Typ: values.UnknownType}
	}
	return out
}

func equalityJoinPredicate(lhs, rhs values.Value) predicates.QueryPredicate {
	return predicates.NewComparisonPredicate(lhs, predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: rhs,
	})
}

// TestNestedLoopJoinPlan_HintCost_UniqueKeyEqualityJoin_MatchesOuterCardinality
// pins the real production shape of the Finding 1 defect: a materialized
// NestedLoopJoin whose join predicate equality-binds the inner leg's FULL
// primary key must cost the same true cardinality (outerCard) the
// correlated-FlatMap shape of the SAME logical join would compute — not the
// flat-FilterSelectivity-inflated outerCard*innerCard*0.5.
func TestNestedLoopJoinPlan_HintCost_UniqueKeyEqualityJoin_MatchesOuterCardinality(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, outerLayout, false)
	innerPlan := NewRecordQueryScanPlan([]string{"Inner"}, innerLayout, false).
		WithPrimaryKey(pkOf("ID"))

	pred := equalityJoinPredicate(fieldOfAliasIn(innerLayout, "ID", "i"), fieldOfAliasIn(outerLayout, "FK", "o"))
	plan := NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan,
		[]predicates.QueryPredicate{pred},
		JoinInner, "o", "i", nil,
	)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); !ok || n != 1 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (1, true)", n, ok)
	}

	got := plan.HintCost([]properties.Cost{{Cardinality: 10, CPU: 1}, {Cardinality: 1000, CPU: 5}}, nil)
	if got.Cardinality != 10 {
		t.Fatalf("Cardinality = %v, want 10 (outerCard — a unique-key equality join returns at most one inner row per outer row)", got.Cardinality)
	}
}

// TestNestedLoopJoinPlan_HintCost_UniqueKeyThroughFetch confirms the
// detection sees through a Fetch wrapper (RecordQueryFetchFromPartialRecordPlan)
// over a unique index scan — the ordinary "unique index, non-covering" shape
// a real plan takes, exactly like isProvablePointProbe already sees through
// Fetch for the leaf-scan case.
//
// It also pins WHICH layout the key columns resolve against. The Fetch's child
// is a covering index scan flowing a PARTIAL record; the join predicate's
// `i.ID` reads the FETCH's output row, so the index's key column names are
// resolved in the Fetch's layout, not the child's. Resolving at the leaf would
// key the want set in a layout no operand speaks and the proof would silently
// stop firing.
func TestNestedLoopJoinPlan_HintCost_UniqueKeyThroughFetch(t *testing.T) {
	t.Parallel()

	// The covering index's own partial row: a DIFFERENT column order from the
	// record the Fetch resolves to.
	partialLayout := layoutOf("InnerPartial", "OTHER", "ID")

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, outerLayout, false)
	idxScan := NewRecordQueryIndexPlan("uq_idx", nil, []string{"Inner"}, partialLayout, false).
		WithIndexMetadata([]string{"ID"}, []string{"ID"}, true)
	innerPlan := NewRecordQueryFetchFromPartialRecordPlan(idxScan, nil, innerLayout, FetchIndexRecordsPrimaryKey)

	pred := equalityJoinPredicate(fieldOfAliasIn(innerLayout, "ID", "i"), fieldOfAliasIn(outerLayout, "FK", "o"))
	plan := NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan,
		[]predicates.QueryPredicate{pred},
		JoinInner, "o", "i", nil,
	)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); !ok || n != 1 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (1, true) — the key column resolves in the FETCH's layout, not the covering child's", n, ok)
	}
	got := plan.HintCost([]properties.Cost{{Cardinality: 10, CPU: 1}, {Cardinality: 1000, CPU: 5}}, nil)
	if got.Cardinality != 10 {
		t.Fatalf("Cardinality = %v, want 10", got.Cardinality)
	}
}

// TestNestedLoopJoinPlan_HintCost_CompositeUniqueKey_BothColumnsBound proves
// the fix composes correctly across a multi-column unique key: TWO join
// predicate conjuncts are consumed (one per key column), and their COMBINED
// selectivity is 1/innerCard exactly — not (1/innerCard) applied per
// conjunct, and not one conjunct getting the correction while the other
// falls through to FilterSelectivity.
func TestNestedLoopJoinPlan_HintCost_CompositeUniqueKey_BothColumnsBound(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, outerLayout, false)
	innerPlan := NewRecordQueryScanPlan([]string{"Inner"}, innerLayout, false).
		WithPrimaryKey(pkOf("TENANT", "ORDER"))

	preds := []predicates.QueryPredicate{
		equalityJoinPredicate(fieldOfAliasIn(innerLayout, "TENANT", "i"), fieldOfAliasIn(outerLayout, "TENANT_FK", "o")),
		equalityJoinPredicate(fieldOfAliasIn(innerLayout, "ORDER", "i"), fieldOfAliasIn(outerLayout, "ORDER_FK", "o")),
	}
	plan := NewRecordQueryNestedLoopJoinPlan(outerPlan, innerPlan, preds, JoinInner, "o", "i", nil)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); !ok || n != 2 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (2, true)", n, ok)
	}
	got := plan.HintCost([]properties.Cost{{Cardinality: 10}, {Cardinality: 1000}}, nil)
	if got.Cardinality != 10 {
		t.Fatalf("Cardinality = %v, want 10", got.Cardinality)
	}
}

// TestNestedLoopJoinPlan_HintCost_UniqueKeyPlusResidualPredicate proves the
// remaining (non-key) conjunct still pays its own FilterSelectivity factor on
// top of the exact 1/innerCard the key bind contributes — the correction
// explains only ITS OWN conjuncts, never any other residual predicate's.
func TestNestedLoopJoinPlan_HintCost_UniqueKeyPlusResidualPredicate(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, outerLayout, false)
	innerPlan := NewRecordQueryScanPlan([]string{"Inner"}, innerLayout, false).
		WithPrimaryKey(pkOf("ID"))

	keyPred := equalityJoinPredicate(fieldOfAliasIn(innerLayout, "ID", "i"), fieldOfAliasIn(outerLayout, "FK", "o"))
	residual := predicates.NewComparisonPredicate(fieldOfAliasIn(innerLayout, "STATUS", "i"), predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: &values.ConstantValue{Value: "ACTIVE"},
	})
	plan := NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan,
		[]predicates.QueryPredicate{keyPred, residual},
		JoinInner, "o", "i", nil,
	)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); !ok || n != 1 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (1, true)", n, ok)
	}
	outer := properties.Cost{Cardinality: 10, CPU: 1}
	inner := properties.Cost{Cardinality: 1000, CPU: 5}
	got := plan.HintCost([]properties.Cost{outer, inner}, nil)
	want := outer.Cardinality * properties.FilterSelectivity // inner cancels: 10*1000*(1/1000)*0.5
	if got.Cardinality != want {
		t.Fatalf("Cardinality = %v, want %v", got.Cardinality, want)
	}
}

// TestNestedLoopJoinPlan_HintCost_NonUniqueEqualityJoin_Unaffected pins the
// gate's other side: a non-unique index equality join must keep the flat
// FilterSelectivity fallback exactly as before — the correction must never
// fire for a join key that is not provably unique in the inner.
func TestNestedLoopJoinPlan_HintCost_NonUniqueEqualityJoin_Unaffected(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, outerLayout, false)
	innerPlan := NewRecordQueryIndexPlan("cat_idx", nil, []string{"Inner"}, innerLayout, false).
		WithIndexMetadata([]string{"CATEGORY"}, []string{"ID"}, false) // NOT unique

	pred := equalityJoinPredicate(fieldOfAliasIn(innerLayout, "CATEGORY", "i"), fieldOfAliasIn(outerLayout, "CAT_FK", "o"))
	plan := NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan,
		[]predicates.QueryPredicate{pred},
		JoinInner, "o", "i", nil,
	)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); ok || n != 0 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (0, false) — index is not unique", n, ok)
	}

	outer := properties.Cost{Cardinality: 10}
	inner := properties.Cost{Cardinality: 1000}
	got := plan.HintCost([]properties.Cost{outer, inner}, nil)
	want := outer.Cardinality * inner.Cardinality * properties.FilterSelectivity
	if got.Cardinality != want {
		t.Fatalf("Cardinality = %v, want %v (unchanged flat-selectivity formula)", got.Cardinality, want)
	}
}

// TestNestedLoopJoinPlan_HintCost_PartialCompositeKeyBind_Unaffected pins the
// other under-detection edge: a composite PRIMARY KEY with only ONE of two
// columns bound by the join predicate must NOT be treated as a point-probe
// bind — a partial prefix does not prove uniqueness.
func TestNestedLoopJoinPlan_HintCost_PartialCompositeKeyBind_Unaffected(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, outerLayout, false)
	innerPlan := NewRecordQueryScanPlan([]string{"Inner"}, innerLayout, false).
		WithPrimaryKey(pkOf("TENANT", "ORDER"))

	pred := equalityJoinPredicate(fieldOfAliasIn(innerLayout, "TENANT", "i"), fieldOfAliasIn(outerLayout, "TENANT_FK", "o"))
	plan := NewRecordQueryNestedLoopJoinPlan(outerPlan, innerPlan, []predicates.QueryPredicate{pred}, JoinInner, "o", "i", nil)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); ok || n != 0 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (0, false) — only one of two PK columns bound", n, ok)
	}
	outer := properties.Cost{Cardinality: 10}
	inner := properties.Cost{Cardinality: 1000}
	got := plan.HintCost([]properties.Cost{outer, inner}, nil)
	want := outer.Cardinality * inner.Cardinality * properties.FilterSelectivity
	if got.Cardinality != want {
		t.Fatalf("Cardinality = %v, want %v", got.Cardinality, want)
	}
}

// TestNestedLoopJoinPlan_HintCost_BothOperandsInner_Unaffected pins Hole 1: an
// equality whose BOTH operands read the inner (`i.id = i.other`) must NOT be
// treated as an outer-parameterized key bind — it is an intra-inner
// comparison the join re-evaluates for every (outer, inner) pair, not a
// per-outer-row point probe. Without the fix, the LHS alone
// (`i.id`, correlated to "i", matches the inner's PK) satisfied the old
// detection regardless of the RHS.
func TestNestedLoopJoinPlan_HintCost_BothOperandsInner_Unaffected(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, outerLayout, false)
	innerPlan := NewRecordQueryScanPlan([]string{"Inner"}, innerLayout, false).
		WithPrimaryKey(pkOf("ID"))

	// i.id = i.other — both sides correlated to "i", the inner alias.
	pred := equalityJoinPredicate(fieldOfAliasIn(innerLayout, "ID", "i"), fieldOfAliasIn(innerLayout, "OTHER", "i"))
	plan := NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan,
		[]predicates.QueryPredicate{pred},
		JoinInner, "o", "i", nil,
	)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); ok || n != 0 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (0, false) — both operands read the inner, not a per-outer-row bind", n, ok)
	}
	outer := properties.Cost{Cardinality: 10}
	inner := properties.Cost{Cardinality: 1000}
	got := plan.HintCost([]properties.Cost{outer, inner}, nil)
	want := outer.Cardinality * inner.Cardinality * properties.FilterSelectivity
	if got.Cardinality != want {
		t.Fatalf("Cardinality = %v, want %v (unchanged flat-selectivity formula)", got.Cardinality, want)
	}
}

// TestNestedLoopJoinPlan_HintCost_NestedFieldSameLeafName_Unaffected pins
// Hole 2: a FUSED baked nested reference (`i.address.id`, Field="ID" but
// Resolved carrying a two-accessor [ADDRESS, ID] path, Child the root QOV
// directly) must NOT be mistaken for a bind on a top-level primary-key column
// also named ID. Without the fix, correlatedInnerField read only fv.Field —
// the LEAF name — and fv.Child was already the bare root QOV, so this exact
// nested shape satisfied the old bare-child check.
//
// The fused path is given the inner leg's REAL domain, so what declines it is
// the multi-accessor shape itself (its root ordinal addresses ADDRESS, not the
// column it is named after) and not a missing domain token — the same reason
// values.OrdinalIn refuses to answer for a fused path at all.
func TestNestedLoopJoinPlan_HintCost_NestedFieldSameLeafName_Unaffected(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, outerLayout, false)
	innerPlan := NewRecordQueryScanPlan([]string{"Inner"}, innerLayout, false).
		WithPrimaryKey(pkOf("ID"))

	addrOrd, _ := innerLayout.FieldIndex("ADDRESS")
	nestedInnerID := &values.FieldValue{
		Field: "ID",
		Typ:   values.UnknownType,
		Child: &values.QuantifiedObjectValue{Correlation: values.NamedCorrelationIdentifier("i")},
		Resolved: &values.FieldPath{
			Accessors: []values.ResolvedAccessor{
				{Field: "ADDRESS", Ordinal: addrOrd},
				{Field: "ID", Ordinal: 0},
			},
			Domain: values.OrdinalDomainOfType(innerLayout),
		},
	}
	pred := equalityJoinPredicate(nestedInnerID, fieldOfAliasIn(outerLayout, "FK", "o"))
	plan := NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan,
		[]predicates.QueryPredicate{pred},
		JoinInner, "o", "i", nil,
	)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); ok || n != 0 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (0, false) — i.address.id is not a bind on top-level ID", n, ok)
	}
	outer := properties.Cost{Cardinality: 10}
	inner := properties.Cost{Cardinality: 1000}
	got := plan.HintCost([]properties.Cost{outer, inner}, nil)
	want := outer.Cardinality * inner.Cardinality * properties.FilterSelectivity
	if got.Cardinality != want {
		t.Fatalf("Cardinality = %v, want %v (unchanged flat-selectivity formula)", got.Cardinality, want)
	}
}

// TestNestedLoopJoinPlan_HintCost_SameLeafNameDifferentDomain_Unaffected is
// RFC-197 item 2's DIMENSION test for this site: the reference and the key
// column share a leaf name AND a correlation, and differ only in the layout
// the ordinal indexes.
//
// The old proof returned `fv.Field` as a bare string and the caller keyed its
// want set by it, so "ID" == "ID" was the whole argument — the operand below
// PROVED an at-most-one bind on the inner's primary key while actually reading
// slot 0 of a DIFFERENT column order, i.e. a different column.
//
// The other layout is deliberately built so ID sits at the SAME ordinal in
// both. Everything except the domain agrees — correlation, leaf name, ordinal
// — so the domain is the only element that can refuse the match, and a
// conversion that checked correlation and ordinal but dropped the domain would
// still fail this test. That is the failure mode RFC-197 revision 2 added the
// third element for: an ordinal conflation reads as authoritative, which is
// strictly worse than a name conflation that reads as suspect.
//
// A wrong at-most-one proof is not a worse plan, it is a cost model asserting
// a cardinality the join does not have — which is why this site fails closed.
func TestNestedLoopJoinPlan_HintCost_SameLeafNameDifferentDomain_Unaffected(t *testing.T) {
	t.Parallel()

	// A layout that also declares ID, at the SAME ordinal, among different
	// columns — so only the layout token separates the two references.
	otherLayout := layoutOf("Other", "ID", "SEQ")

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, outerLayout, false)
	innerPlan := NewRecordQueryScanPlan([]string{"Inner"}, innerLayout, false).
		WithPrimaryKey(pkOf("ID"))

	// Correlated to the inner alias, spelled ID, but its ordinal indexes
	// otherLayout — the shape a rebase produces when a reference keeps a
	// producer's ordinal while flowing under a leg with a different layout.
	misdomained := fieldOfAliasIn(otherLayout, "ID", "i")
	fv := misdomained.(*values.FieldValue)
	if fv.Field != "ID" {
		t.Fatalf("test setup: the operand must share the key column's leaf name, got %q", fv.Field)
	}
	innerOrd, _ := innerLayout.FieldIndex("ID")
	if fv.Resolved.Root().Ordinal != innerOrd {
		t.Fatalf("test setup: the operand must sit at the key column's ordinal (%d) so the DOMAIN is the only difference, got %d",
			innerOrd, fv.Resolved.Root().Ordinal)
	}

	pred := equalityJoinPredicate(misdomained, fieldOfAliasIn(outerLayout, "FK", "o"))
	plan := NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan,
		[]predicates.QueryPredicate{pred},
		JoinInner, "o", "i", nil,
	)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); ok || n != 0 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (0, false) — the operand's ordinal indexes another layout; only its NAME matches the key column", n, ok)
	}
	outer := properties.Cost{Cardinality: 10}
	inner := properties.Cost{Cardinality: 1000}
	got := plan.HintCost([]properties.Cost{outer, inner}, nil)
	want := outer.Cardinality * inner.Cardinality * properties.FilterSelectivity
	if got.Cardinality != want {
		t.Fatalf("Cardinality = %v, want %v (unchanged flat-selectivity formula)", got.Cardinality, want)
	}
}

// TestNestedLoopJoinPlan_HintCost_LazyOperand_Unaffected states the PRICE of
// the conversion plainly, so nobody has to rediscover it: a LAZY operand (no
// resolved accessor at all, a plan-time name carrier) can no longer prove the
// bind, where the old name comparison could.
//
// Every operand reaching this proof in the corpus is baked with a known domain
// (measured across the explaindiff corpus while converting the site), so this
// costs nothing today. It is pinned because a producer that starts handing this
// proof lazy references would silently lose the correction rather than fail,
// and "the cost model quietly stopped correcting" is exactly the kind of drift
// no other test in this file can see.
func TestNestedLoopJoinPlan_HintCost_LazyOperand_Unaffected(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, outerLayout, false)
	innerPlan := NewRecordQueryScanPlan([]string{"Inner"}, innerLayout, false).
		WithPrimaryKey(pkOf("ID"))

	lazyInnerID := &values.FieldValue{
		Field: "ID",
		Typ:   values.UnknownType,
		Child: &values.QuantifiedObjectValue{Correlation: values.NamedCorrelationIdentifier("i")},
	}
	pred := equalityJoinPredicate(lazyInnerID, fieldOfAliasIn(outerLayout, "FK", "o"))
	plan := NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan,
		[]predicates.QueryPredicate{pred},
		JoinInner, "o", "i", nil,
	)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); ok || n != 0 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (0, false) — a lazy reference carries no ordinal to prove the bind with", n, ok)
	}
}

// TestNestedLoopJoinPlan_HintCost_UntypedLeg_Unaffected pins the other
// fail-closed direction: a leg whose flowed type states no column order has no
// layout to resolve its key columns against, so the proof declines rather than
// resolving them somewhere else.
func TestNestedLoopJoinPlan_HintCost_UntypedLeg_Unaffected(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, outerLayout, false)
	innerPlan := NewRecordQueryScanPlan([]string{"Inner"}, values.UnknownType, false).
		WithPrimaryKey(pkOf("ID"))

	pred := equalityJoinPredicate(fieldOfAliasIn(innerLayout, "ID", "i"), fieldOfAliasIn(outerLayout, "FK", "o"))
	plan := NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan,
		[]predicates.QueryPredicate{pred},
		JoinInner, "o", "i", nil,
	)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); ok || n != 0 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (0, false) — an untyped leg declares no column order", n, ok)
	}
}

// TestNestedLoopJoinPlan_HintCost_UnsafeIndexOrderingNames_Unaffected pins
// Hole 3: a unique index whose names have been marked unsafe for name-based
// matching (WithOrderingKeyNamesUnavailable — the marker a function-key or
// nested-key index carries, e.g. a unique CARDINALITY(TAGS) or nested
// ADDR.CITY key) must NOT have its GetColumnNames() trusted as flat top-level
// key columns, even though IsUnique() is true and the leaf name matches.
func TestNestedLoopJoinPlan_HintCost_UnsafeIndexOrderingNames_Unaffected(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, outerLayout, false)
	innerPlan := NewRecordQueryIndexPlan("uq_tags_idx", nil, []string{"Inner"}, innerLayout, false).
		WithIndexMetadata([]string{"TAGS"}, []string{"ID"}, true). // unique
		WithOrderingKeyNamesUnavailable()                          // e.g. CARDINALITY(TAGS) — names unsafe

	pred := equalityJoinPredicate(fieldOfAliasIn(innerLayout, "TAGS", "i"), fieldOfAliasIn(outerLayout, "TAGS_FK", "o"))
	plan := NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan,
		[]predicates.QueryPredicate{pred},
		JoinInner, "o", "i", nil,
	)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); ok || n != 0 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (0, false) — index names are marked unsafe for name matching", n, ok)
	}
	outer := properties.Cost{Cardinality: 10}
	inner := properties.Cost{Cardinality: 1000}
	got := plan.HintCost([]properties.Cost{outer, inner}, nil)
	want := outer.Cardinality * inner.Cardinality * properties.FilterSelectivity
	if got.Cardinality != want {
		t.Fatalf("Cardinality = %v, want %v (unchanged flat-selectivity formula)", got.Cardinality, want)
	}
}

// TestNestedLoopJoinPlan_HintCost_FullOuterJoin_Unaffected pins Hole 4: a
// FULL OUTER join must NOT collapse to outerCard on a unique-key equality
// bind — FULL OUTER additionally preserves every unmatched INNER row
// (NULL-padded on the outer side), so a 10-row outer against a 1000-row inner
// cannot have cardinality 10 when most inner rows have no matching outer row.
func TestNestedLoopJoinPlan_HintCost_FullOuterJoin_Unaffected(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, outerLayout, false)
	innerPlan := NewRecordQueryScanPlan([]string{"Inner"}, innerLayout, false).
		WithPrimaryKey(pkOf("ID"))

	pred := equalityJoinPredicate(fieldOfAliasIn(innerLayout, "ID", "i"), fieldOfAliasIn(outerLayout, "FK", "o"))
	plan := NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan,
		[]predicates.QueryPredicate{pred},
		JoinFullOuter, "o", "i", nil,
	)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); ok || n != 0 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (0, false) — FULL OUTER preserves unmatched inner rows too", n, ok)
	}
	outer := properties.Cost{Cardinality: 10}
	inner := properties.Cost{Cardinality: 1000}
	got := plan.HintCost([]properties.Cost{outer, inner}, nil)
	want := outer.Cardinality * inner.Cardinality * properties.FilterSelectivity
	if got.Cardinality != want {
		t.Fatalf("Cardinality = %v, want %v (unchanged flat-selectivity formula, NOT outerCard)", got.Cardinality, want)
	}
}

// TestNestedLoopJoinPlan_HintCost_LeftOuterJoin_StillCorrected pins the other
// side of Hole 4's gate: a LEFT OUTER join on a unique-key equality bind DOES
// still collapse to outerCard, same as INNER — every outer row is preserved
// EXACTLY ONCE (0 or 1 inner matches, since the key is unique: a match emits
// one row, no match emits one NULL-padded row), so the flat-FilterSelectivity
// fallback would still be the ~500x overestimate the unique-key correction
// exists to fix. This must not regress when Hole 4's join-type gate narrows.
func TestNestedLoopJoinPlan_HintCost_LeftOuterJoin_StillCorrected(t *testing.T) {
	t.Parallel()

	outerPlan := NewRecordQueryScanPlan([]string{"Outer"}, outerLayout, false)
	innerPlan := NewRecordQueryScanPlan([]string{"Inner"}, innerLayout, false).
		WithPrimaryKey(pkOf("ID"))

	pred := equalityJoinPredicate(fieldOfAliasIn(innerLayout, "ID", "i"), fieldOfAliasIn(outerLayout, "FK", "o"))
	plan := NewRecordQueryNestedLoopJoinPlan(
		outerPlan, innerPlan,
		[]predicates.QueryPredicate{pred},
		JoinLeftOuter, "o", "i", nil,
	)

	if n, ok := NestedLoopJoinUniqueKeyConjuncts(plan); !ok || n != 1 {
		t.Fatalf("NestedLoopJoinUniqueKeyConjuncts = (%v, %v), want (1, true) — LEFT OUTER preserves each outer row exactly once on a unique-key bind", n, ok)
	}
	got := plan.HintCost([]properties.Cost{{Cardinality: 10, CPU: 1}, {Cardinality: 1000, CPU: 5}}, nil)
	if got.Cardinality != 10 {
		t.Fatalf("Cardinality = %v, want 10 (outerCard)", got.Cardinality)
	}
}

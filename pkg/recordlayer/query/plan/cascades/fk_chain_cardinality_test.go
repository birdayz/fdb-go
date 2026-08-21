package cascades

import (
	"fmt"
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustFKChain[T any](value T, err error) T {
	if err != nil {
		panic(fmt.Sprintf("construct FK-chain fixture: %v", err))
	}
	return value
}

// Regression tests for fk_chain_cardinality.go — the FK-chain cardinality
// cap that keeps compareJoinOrdering's per-hop selectivity estimate from
// exceeding what the data can physically produce, and makes the estimate
// agree regardless of which end of an FK chain drives.
//
// Chain modeled: t1(1) <- t2(20) <- t3(200) <- t4(2000), the exact shape from
// TestFDB_MultiwayJoinOrder_Nway (pkg/relational/sqldriver). Every table's PK
// column is named ID; the FK columns are T1_ID (on t2), T2_ID (on t3),
// T3_ID (on t4) — matching chainPred: "t2.t1_id=t1.id AND t3.t2_id=t2.id AND
// t4.t3_id=t3.id".

// fkChainLayouts is each modeled table's declared column order — the row
// layout its leaf flows, and the layout its own column references index
// (RFC-197's ordinal DOMAIN). Both sides of every proof in this file resolve
// against the SAME registry entry, which is what makes an ordinal comparison
// meaningful at all: a leaf whose flowed type declares no column order has no
// domain, and the identity proofs fail closed on it.
var fkChainLayouts = map[string]*values.RecordType{
	"T1": fkChainT1Layout(),
	"T2": fkChainLayout("T2", "ID", "T1_ID", "ADDRESS"),
	"T3": fkChainLayout("T3", "ID", "T2_ID"),
	"T4": fkChainLayout("T4", "ID", "T3_ID"),
	"X":  fkChainLayout("X", "ID", "GROUP"),
	"Y":  fkChainLayout("Y", "ID", "GROUP"),
}

func fkChainT1Layout() *values.RecordType {
	address := values.NewRecordType("T1_ADDRESS", false, []values.Field{
		{Name: "OTHER", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1},
	})
	return values.NewRecordType("T1", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "ADDRESS", FieldType: address, Ordinal: 1},
	})
}

func fkChainLayout(name string, cols ...string) *values.RecordType {
	fields := make([]values.Field, len(cols))
	for i, c := range cols {
		fields[i] = values.Field{Name: c, FieldType: values.NotNullLong, Ordinal: i}
	}
	return values.NewRecordType(name, false, fields)
}

// fkChainRowType is the flowed type a leaf over rt emits.
func fkChainRowType(rt string) values.Type {
	if l, ok := fkChainLayouts[rt]; ok {
		return l
	}
	panic("FK-chain fixture has no layout for " + rt)
}

func fkChainField(layout *values.RecordType, alias values.CorrelationIdentifier, field string) values.Value {
	ordinal, ok := layout.FieldIndexUnique(field)
	if !ok {
		panic("FK-chain fixture has no field " + field + " in " + layout.RecordName)
	}
	root := mustFKChain(values.NewQuantifiedObjectValue(alias, layout))
	resolved := mustFKChain(values.ResolveFieldOrdinals(root, []int{ordinal}))
	if _, ok := values.AsFieldValue(resolved); !ok {
		panic(fmt.Sprintf("FK-chain resolved field %s has unexpected type %T", field, resolved))
	}
	return resolved
}

// fkChainIDPK is the LAZY, name-only primary key GetPrimaryKeyValues() /
// GetCommonPrimaryKeyValues() stamp (primary_key_translation.go's "flat
// field-reference model", never evaluated). The name is METADATA and dies at
// the boundary: leafFieldIdentity resolves it against the leg's stated layout
// once, and every comparison after that is between ordinals.
func fkChainIDPK() []values.Value {
	return []values.Value{fkChainField(
		fkChainLayout("PK_METADATA", "ID"), values.UniqueCorrelationIdentifier(), "ID")}
}

// fkChainCorrelatedEq builds a ComparisonRange equal to `outerAlias.<field>`,
// BAKED against outerRT's declared column order — the production shape the SQL
// resolver mints (expr.go's sourceColumnOrdinal derives the ordinal and the
// domain from the source's own column list in one breath). outerRT is the
// table whose row outerAlias binds, and stating it is the point: an ordinal
// means nothing without the layout it indexes.
func fkChainCorrelatedEq(t *testing.T, outerRT string, outerAlias values.CorrelationIdentifier, field string) *predicates.ComparisonRange {
	t.Helper()
	operand := fkChainCorrelatedRef(t, outerRT, outerAlias, field)
	cmp := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: operand}
	res := predicates.EmptyComparisonRange().Merge(&cmp)
	if !res.Ok {
		t.Fatalf("failed to build correlated eq range")
	}
	return res.Range
}

// fkChainCorrelatedRef is fkChainCorrelatedEq's operand on its own. A column
// the layout does not declare stays LAZY — the shape a reference to something
// outside the outer's row would really have, and one the identity proof
// declines rather than matching by spelling.
func fkChainCorrelatedRef(t *testing.T, outerRT string, outerAlias values.CorrelationIdentifier, field string) values.Value {
	t.Helper()
	layout, known := fkChainLayouts[outerRT]
	if !known {
		t.Fatalf("unknown FK-chain outer row type %q", outerRT)
	}
	return fkChainField(layout, outerAlias, field)
}

func fkChainFullScan(rt string) plans.RecordQueryPlan {
	return mustFKChain(plans.NewRecordQueryScanPlan([]string{rt}, fkChainRowType(rt), false)).
		WithPrimaryKey(fkChainIDPK()).
		WithKeyComponentTypes([]values.Type{values.NotNullLong})
}

// fkChainPKProbe is a primary-key equality point probe against rt, correlated
// to outerAlias.<bindField> — the FK column name on the OUTER's own table
// (e.g. T4.T3_ID when probing T3 from an outer driven by T4), stated with that
// outer table's layout.
func fkChainPKProbe(t *testing.T, rt, outerRT string, outerAlias values.CorrelationIdentifier, bindField string) plans.RecordQueryPlan {
	t.Helper()
	return mustFKChain(plans.NewRecordQueryScanPlan([]string{rt}, fkChainRowType(rt), false)).
		WithPrimaryKey(fkChainIDPK()).
		WithScanComparisons([]*predicates.ComparisonRange{fkChainCorrelatedEq(t, outerRT, outerAlias, bindField)}).
		WithKeyComponentTypes([]values.Type{values.NotNullLong})
}

// fkChainFKProbe is a non-unique secondary-index equality probe against rt,
// correlated to outerAlias.ID — the PRECEDING table's own primary key. It
// models a plain SCALAR index (one entry per record — no FAN_OUT field), so
// it stamps distinctRecordsKnown/createsDuplicates=false exactly as
// abstract_data_access_rule.go does in production from the match candidate's
// createsDuplicates() signal — a scalar index never fans out, so
// ProducesDistinctRecords() must read true for the FK-chain cap to fire on
// it, same as production.
func fkChainFKProbe(t *testing.T, rt, idx, outerRT string, outerAlias values.CorrelationIdentifier) plans.RecordQueryPlan {
	t.Helper()
	return mustFKChain(plans.NewRecordQueryIndexPlan(idx,
		[]*predicates.ComparisonRange{fkChainCorrelatedEq(t, outerRT, outerAlias, "ID")},
		[]string{rt}, fkChainRowType(rt), false)).
		WithKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithIndexMetadata([]string{"FK"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithCommonPrimaryKey(fkChainIDPK()).
		WithDistinctRecordsSignal(false)
}

// fkChainFanOutFKProbe is fkChainFKProbe but models a FAN-OUT index (one
// entry per repeated/nested element, e.g. an index over a repeated field) —
// createsDuplicates=true, so the SAME physical record/PK can be emitted more
// than once per probe. See TestFKChainCardinalityCap_DeclinesOnFanOutIndex.
func fkChainFanOutFKProbe(t *testing.T, rt, idx, outerRT string, outerAlias values.CorrelationIdentifier) plans.RecordQueryPlan {
	t.Helper()
	return mustFKChain(plans.NewRecordQueryIndexPlan(idx,
		[]*predicates.ComparisonRange{fkChainCorrelatedEq(t, outerRT, outerAlias, "ID")},
		[]string{rt}, fkChainRowType(rt), false)).
		WithKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithIndexMetadata([]string{"FK"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithCommonPrimaryKey(fkChainIDPK()).
		WithDistinctRecordsSignal(true)
}

// fkChainRTPrefixedPK models the primary key shape
// TranslatePrimaryKeyToValues (primary_key_translation.go) actually stamps
// for a table whose declared PRIMARY KEY compiles to
// Concat(RecordTypeKey(), Field("ID")) — the normal encoding for every table
// in a non-intermingled multi-type SQL schema (cascades_generator.go's
// IndexCommonPrimaryKeyValues). Every OTHER helper in this file uses the bare
// fkChainIDPK() instead, which never exercises this shape — that gap is
// exactly what let a real production bug (innerFullyBindsThread failing
// closed on the RecordTypeValue component) hide behind a fully green test
// suite. See TestFKChainCardinalityCap_PropagatesAcrossRecordTypeKeyPrefixedPK.
func fkChainRTPrefixedPK() []values.Value {
	return []values.Value{
		values.NewRecordTypeValue(nil),
		fkChainField(fkChainLayout("PK_METADATA", "ID"), values.UniqueCorrelationIdentifier(), "ID"),
	}
}

// fkChainFKProbeRTPrefixed is fkChainFKProbe but stamps a
// RecordTypeValue-prefixed common primary key — the real shape
// GetCommonPrimaryKeyValues() returns for a production index scan (see
// fkChainRTPrefixedPK).
func fkChainFKProbeRTPrefixed(t *testing.T, rt, idx, outerRT string, outerAlias values.CorrelationIdentifier) plans.RecordQueryPlan {
	t.Helper()
	return mustFKChain(plans.NewRecordQueryIndexPlan(idx,
		[]*predicates.ComparisonRange{fkChainCorrelatedEq(t, outerRT, outerAlias, "ID")},
		[]string{rt}, fkChainRowType(rt), false)).
		WithKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithIndexMetadata([]string{"FK"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithCommonPrimaryKey(fkChainRTPrefixedPK()).
		WithDistinctRecordsSignal(false)
}

func fkChainFlat(outer, inner plans.RecordQueryPlan, outerAlias values.CorrelationIdentifier) plans.RecordQueryPlan {
	innerAlias := values.NamedCorrelationIdentifier("i")
	return mustFKChain(plans.NewRecordQueryFlatMapPlan(outer, inner,
		outerAlias, innerAlias,
		mustFKChain(values.NewQuantifiedObjectValue(innerAlias, planRowLayout(inner))), false))
}

func fkChainAlias(i int) values.CorrelationIdentifier {
	names := []string{"o0", "o1", "o2"}
	return values.NamedCorrelationIdentifier(names[i])
}

func fkChainStats() properties.MapStatistics {
	return properties.MapStatistics{PerType: map[string]float64{
		"T1": 1, "T2": 20, "T3": 200, "T4": 2000,
	}}
}

// buildForwardChain drives T1 (1 row, full scan) -> T2 -> T3 -> T4, each hop
// a NON-UNIQUE secondary-index equality probe on the child's FK column —
// mirroring qSmall's "SELECT t1.id FROM t1, t2, t3, t4 WHERE ..." plan shape.
func buildForwardChain(t *testing.T) plans.RecordQueryPlan {
	t.Helper()
	fwd1 := fkChainFlat(fkChainFullScan("T1"), fkChainFKProbe(t, "T2", "t2_by_t1", "T1", fkChainAlias(0)), fkChainAlias(0))
	fwd2 := fkChainFlat(fwd1, fkChainFKProbe(t, "T3", "t3_by_t2", "T2", fkChainAlias(1)), fkChainAlias(1))
	return fkChainFlat(fwd2, fkChainFKProbe(t, "T4", "t4_by_t3", "T3", fkChainAlias(2)), fkChainAlias(2))
}

// buildBackwardChain drives T4 (2000 rows, full scan) -> T3 -> T2 -> T1, each
// hop a PRIMARY-KEY equality point probe — mirroring qBig's
// "SELECT t1.id FROM t4, t3, t2, t1 WHERE ..." plan shape.
func buildBackwardChain(t *testing.T) plans.RecordQueryPlan {
	t.Helper()
	bwd1 := fkChainFlat(fkChainFullScan("T4"), fkChainPKProbe(t, "T3", "T4", fkChainAlias(0), "T3_ID"), fkChainAlias(0))
	bwd2 := fkChainFlat(bwd1, fkChainPKProbe(t, "T2", "T3", fkChainAlias(1), "T2_ID"), fkChainAlias(1))
	return fkChainFlat(bwd2, fkChainPKProbe(t, "T1", "T2", fkChainAlias(2), "T1_ID"), fkChainAlias(2))
}

// TestFKChainCardinalityCap_OrderInvariant pins the core property this file
// exists for: a join's output cardinality is a property of the logical FK
// chain, not of which end drives it. Forward (non-unique index probes) and
// backward (PK point probes) over the SAME chain must agree on the top-level
// join's estimated cardinality.
func TestFKChainCardinalityCap_OrderInvariant(t *testing.T) {
	t.Parallel()
	stats := fkChainStats()

	forward := concretePlanCost(buildForwardChain(t), stats, nil)
	backward := concretePlanCost(buildBackwardChain(t), stats, nil)

	if forward.Cardinality != backward.Cardinality {
		t.Fatalf("order-dependent cardinality: forward=%v backward=%v, want equal (true chain cardinality is 2000 either direction)",
			forward.Cardinality, backward.Cardinality)
	}
	if forward.Cardinality != 2000 {
		t.Fatalf("chain cardinality = %v, want the true value 2000 (every T4 row has exactly one ancestor chain)", forward.Cardinality)
	}
}

// TestFKChainCardinalityCap_NeverExceedsProvableBound is the impossible-8000
// regression: before the FK-chain cap, the forward direction's per-hop
// EqualityBoundSelectivity-compounded estimate reached 8000 at level 3 —
// exceeding T4's own 2000-row table size, a result no real query can ever
// produce. Every level's cap-target table size is a hard ceiling.
func TestFKChainCardinalityCap_NeverExceedsProvableBound(t *testing.T) {
	t.Parallel()
	stats := fkChainStats()

	fwd1 := fkChainFlat(fkChainFullScan("T1"), fkChainFKProbe(t, "T2", "t2_by_t1", "T1", fkChainAlias(0)), fkChainAlias(0))
	fwd2 := fkChainFlat(fwd1, fkChainFKProbe(t, "T3", "t3_by_t2", "T2", fkChainAlias(1)), fkChainAlias(1))
	fwd3 := fkChainFlat(fwd2, fkChainFKProbe(t, "T4", "t4_by_t3", "T3", fkChainAlias(2)), fkChainAlias(2))

	c1 := concretePlanCost(fwd1, stats, nil)
	c2 := concretePlanCost(fwd2, stats, nil)
	c3 := concretePlanCost(fwd3, stats, nil)

	if c1.Cardinality > 20 {
		t.Errorf("level1 (T1 join T2) cardinality %v exceeds T2's own size 20", c1.Cardinality)
	}
	if c2.Cardinality > 200 {
		t.Errorf("level2 (+T3) cardinality %v exceeds T3's own size 200", c2.Cardinality)
	}
	if c3.Cardinality > 2000 {
		t.Errorf("level3 (+T4) cardinality %v exceeds T4's own size 2000 (the impossible-8000 case)", c3.Cardinality)
	}
}

// TestFKChainCardinalityCap_DoesNotFireOnNonPKBind pins the documented
// counter-example that disproves the NAIVE, unconditional form of this cap:
// when outer rows are NOT unique on the join key (many outer rows sharing one
// bind value), summing per-probe results can legitimately exceed the probed
// table's size, so the cap must not fire. Here 1 outer row of Y probes X via
// Y's OWN NON-PK column ("GROUP", not Y's stamped primary key "ID") — the
// bind field name never matches Y's PK, so fkChainCardinalityCap must
// decline (ok=false), leaving the ordinary selectivity estimate in place
// rather than risking an unsound clamp.
func TestFKChainCardinalityCap_DoesNotFireOnNonPKBind(t *testing.T) {
	t.Parallel()

	outer := fkChainFullScan("Y")
	inner := mustFKChain(plans.NewRecordQueryIndexPlan("x_by_group",
		[]*predicates.ComparisonRange{fkChainCorrelatedEq(t, "Y", fkChainAlias(0), "GROUP")},
		[]string{"X"}, fkChainRowType("X"), false)).
		WithCommonPrimaryKey(fkChainIDPK())
	fm := fkChainFlat(outer, inner, fkChainAlias(0)).(*plans.RecordQueryFlatMapPlan)

	if cap, ok := fkChainCardinalityCap(fm, properties.MapStatistics{PerType: map[string]float64{"X": 500}}); ok {
		t.Fatalf("cap fired on a non-PK bind (cap=%v) — the disproven-unsound case; must decline (ok=false)", cap)
	}
}

// fkChainCorrelatedNestedEq is fkChainCorrelatedEq but builds a FUSED baked
// nested reference (Field=leaf, Child=the bare source QOV directly, Resolved
// carrying a TWO-accessor [parent, leaf] path) — the shape a baked
// `outerAlias.parent.leaf` reference takes, as opposed to a flat
// `outerAlias.leaf` top-level column.
//
// The fused path carries the OUTER's real domain, so what declines it is the
// multi-accessor shape itself — its root ordinal addresses PARENT, not the
// column it is named after — and not a missing domain token.
func fkChainCorrelatedNestedEq(t *testing.T, outerRT string, outerAlias values.CorrelationIdentifier, parent, leaf string) *predicates.ComparisonRange {
	t.Helper()
	layout, ok := fkChainLayouts[outerRT]
	if !ok {
		t.Fatalf("unknown FK-chain outer row type %q", outerRT)
	}
	root := mustFKChain(values.NewQuantifiedObjectValue(outerAlias, layout))
	parentRequest := mustFKChain(values.FieldByName(parent))
	leafRequest := mustFKChain(values.FieldByName(leaf))
	operand := mustFKChain(values.ResolveFieldAccess(
		root, []values.FieldRequest{parentRequest, leafRequest}))
	field, isField := values.AsFieldValue(operand)
	if !isField || field.Path().Len() != 2 {
		t.Fatalf("nested FK-chain operand = %T, want exact two-accessor FieldValue", operand)
	}
	cmp := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: operand}
	res := predicates.EmptyComparisonRange().Merge(&cmp)
	if !res.Ok {
		t.Fatalf("failed to build correlated nested eq range")
	}
	return res.Range
}

// TestFKChainCardinalityCap_DoesNotFireOnNestedFieldSameLeafName pins the
// nested-field analogue of the non-PK-bind counter-example above: the outer's
// OWN top-level primary key is flat "ID" (fkChainIDPK), but the inner's scan
// comparison binds a NESTED `outerAlias.ADDRESS.ID` — a fused baked reference
// whose LEAF name happens to also be "ID". correlatedFieldOf must not report
// this as a bind on the outer's flat top-level ID column merely because the
// leaf names collide: FieldValue.Field is a display leaf, not the whole
// accessor identity, and a two-accessor Resolved path is not the same column
// as the outer's actual single-accessor primary key. Mirrors
// plans/cost.go's TestNestedLoopJoinPlan_HintCost_NestedFieldSameLeafName_Unaffected,
// which pins the identical hole in the sibling unique-key join-cost proof.
func TestFKChainCardinalityCap_DoesNotFireOnNestedFieldSameLeafName(t *testing.T) {
	t.Parallel()

	outer := fkChainFullScan("T1") // PK: flat top-level "ID"
	inner := mustFKChain(plans.NewRecordQueryIndexPlan("t2_by_addr_id",
		[]*predicates.ComparisonRange{fkChainCorrelatedNestedEq(t, "T1", fkChainAlias(0), "ADDRESS", "ID")},
		[]string{"T2"}, fkChainRowType("T2"), false)).
		WithCommonPrimaryKey(fkChainIDPK()).
		WithDistinctRecordsSignal(false)
	fm := fkChainFlat(outer, inner, fkChainAlias(0)).(*plans.RecordQueryFlatMapPlan)

	if cap, ok := fkChainCardinalityCap(fm, properties.MapStatistics{PerType: map[string]float64{"T2": 500}}); ok {
		t.Fatalf("cap fired on a nested same-leaf-name bind (T1.ADDRESS.ID mistaken for T1's own top-level ID), cap=%v — must decline (ok=false)", cap)
	}
}

// TestFKChainCardinalityCap_PropagatesAcrossFetchAndTypeFilter checks the
// pass-through wrappers computePKThread/scanComparisonsOfLeaf/
// singleLeafRecordType see through: a real production FlatMap chain wraps a
// non-covering index probe in Fetch(IndexScan(...)), and a multi-type store
// adds TypeFilter. The cap must still fire through those wrappers exactly as
// it does over a bare leaf.
func TestFKChainCardinalityCap_PropagatesAcrossFetchAndTypeFilter(t *testing.T) {
	t.Parallel()
	stats := fkChainStats()

	outer := fkChainFullScan("T1")
	bareInner := fkChainFKProbe(t, "T2", "t2_by_t1", "T1", fkChainAlias(0))
	fetched := mustFKChain(plans.NewRecordQueryFetchFromPartialRecordPlan(
		bareInner, nil, fkChainRowType("T2"), plans.FetchIndexRecordsPrimaryKey))
	wrappedInner := mustFKChain(plans.NewRecordQueryTypeFilterPlan([]string{"T2"}, fetched))
	fm := fkChainFlat(outer, wrappedInner, fkChainAlias(0)).(*plans.RecordQueryFlatMapPlan)

	cap, ok := fkChainCardinalityCap(fm, stats)
	if !ok {
		t.Fatalf("cap did not fire through Fetch/TypeFilter wrappers")
	}
	if cap != 20 {
		t.Fatalf("cap = %v, want T2's table size 20", cap)
	}
}

// TestFKChainCardinalityCap_ManyOuterRowsSharingOneValue pins the same
// exclusion as TestFKChainCardinalityCap_DoesNotFireOnNonPKBind with the
// concrete numbers the file doc comment's disproven counter-example uses:
// 1000 outer rows of Y all sharing ONE value of a non-PK column probe a
// 500-row table X, matching 3 rows each — a TRUE output of 3000, which a cap
// at X's own size (500) would wrongly clamp. The cap must decline (ok=false)
// regardless of how large the outer's row count is; what disqualifies it is
// that the bind column is not Y's own primary key, so Y's rows are not
// provably unique on it.
func TestFKChainCardinalityCap_ManyOuterRowsSharingOneValue(t *testing.T) {
	t.Parallel()
	stats := properties.MapStatistics{PerType: map[string]float64{"Y": 1000, "X": 500}}

	outer := fkChainFullScan("Y") // 1000 rows, modeling all sharing one GROUP value
	inner := mustFKChain(plans.NewRecordQueryIndexPlan("x_by_group",
		[]*predicates.ComparisonRange{fkChainCorrelatedEq(t, "Y", fkChainAlias(0), "GROUP")},
		[]string{"X"}, fkChainRowType("X"), false)).
		WithCommonPrimaryKey(fkChainIDPK())
	fm := fkChainFlat(outer, inner, fkChainAlias(0)).(*plans.RecordQueryFlatMapPlan)

	if cap, ok := fkChainCardinalityCap(fm, stats); ok {
		t.Fatalf("cap fired (cap=%v) on 1000 outer rows sharing one non-PK bind value — "+
			"the disproven-unsound case (true output can be 3000, exceeding X's 500-row size)", cap)
	}
}

// TestFKChainCardinalityCap_PropagatesAcrossRecordTypeKeyPrefixedPK reproduces
// a real production bug found while verifying this file's cap against
// TestFDB_MultiwayJoinOrder_Nway: TranslatePrimaryKeyToValues
// (primary_key_translation.go) translates a real SQL table's declared
// PRIMARY KEY — Concat(RecordTypeKey(), Field("ID")), the normal shape for
// every table in a non-intermingled multi-type schema (see
// cascades_generator.go's IndexCommonPrimaryKeyValues) — into
// [RecordTypeValue, FieldValue{ID}], NOT the bare [FieldValue{ID}] every
// other helper in this file uses.
//
// A chain's FIRST hop roots its outer thread at a plain
// RecordQueryScanPlan (GetPrimaryKeyValues, which does not carry the prefix),
// so hop 1 was never affected. Every hop AFTER that roots its outer thread at
// the PRECEDING hop's inner leg — an IndexPlan, whose
// GetCommonPrimaryKeyValues DOES carry the RecordTypeValue prefix. Before
// innerFullyBindsThread learned to skip that component (it is a per-thread
// CONSTANT within a single record type, never a discriminating column),
// leafFieldName failed closed on it, so the cap could fire on hop 1 only —
// never on hop 2 or later, no matter how long the FK chain was. That is
// exactly what let TestFDB_MultiwayJoinOrder_Nway's forward direction keep
// computing an impossible 8000-row cardinality at hop 3 even after the cap
// was otherwise correctly generalized to outer-uniqueness (as opposed to
// inner-index-kind).
func TestFKChainCardinalityCap_PropagatesAcrossRecordTypeKeyPrefixedPK(t *testing.T) {
	t.Parallel()
	stats := fkChainStats()

	hop1 := fkChainFlat(fkChainFullScan("T1"), fkChainFKProbeRTPrefixed(t, "T2", "t2_by_t1", "T1", fkChainAlias(0)), fkChainAlias(0))
	hop2 := fkChainFlat(hop1, fkChainFKProbeRTPrefixed(t, "T3", "t3_by_t2", "T2", fkChainAlias(1)), fkChainAlias(1))

	cap, ok := fkChainCardinalityCap(hop2.(*plans.RecordQueryFlatMapPlan), stats)
	if !ok {
		t.Fatalf("cap did not fire on the SECOND hop of a RecordTypeValue-prefixed PK chain — " +
			"the real production shape (hop 1's outer thread is FlatMap-rooted, not scan-rooted)")
	}
	if cap != 200 {
		t.Fatalf("cap = %v, want T3's table size 200", cap)
	}

	// The point of the cap existing at all: hop2's OWN rolled-up Cardinality
	// must actually be clamped, not just the standalone fkChainCardinalityCap
	// probe above.
	c2 := concretePlanCost(hop2, stats, nil)
	if c2.Cardinality > 200 {
		t.Errorf("hop2 cardinality %v exceeds T3's own size 200 (cap not applied end-to-end)", c2.Cardinality)
	}
}

// TestFKChainCappedInnerCost_DerivationProperty pins the affine derivation:
// the cap scales only the row-dependent CPU and preserves the fixed work paid
// every time FlatMap opens the inner. It also pins malformed/overflow inputs
// to the conservative no-correction direction.
func TestFKChainCappedInnerCost_DerivationProperty(t *testing.T) {
	t.Parallel()

	const epsilon = 1e-9
	closeEnough := func(a, b float64) bool {
		d := a - b
		if d < 0 {
			d = -d
		}
		return d <= epsilon*(1+absF(a)+absF(b))
	}

	cases := []struct {
		name                          string
		outerCard, innerCard          float64
		innerCPU, cap, fixedCPU       float64
		wantApplied                   bool
		wantCardinality, wantInnerCPU float64
	}{
		{"zero intercept preserves proportional behavior", 40, 200, 278.1, 2000, 0, true, 50, 69.525},
		{"mixed fixed and variable CPU", 5, 100, 50, 60, 10, true, 12, 14.8},
		{"all CPU fixed survives a severe cap", 1000, 1000, 1000, 1, 1000, true, 0.001, 1000},
		{"zero cap still pays startup", 5, 100, 50, 0, 10, true, 0, 10},
		{"negative fixed component clamps to zero", 5, 100, 50, 60, -10, true, 12, 6},
		{"oversized fixed component clamps to all CPU", 5, 100, 50, 60, 500, true, 12, 50},
		{"unknown fixed component preserves all CPU", 5, 100, 50, 60, math.NaN(), true, 12, 50},
		{"saturated CPU remains conservative while cardinality binds", properties.MaxFiniteHeuristic, properties.MaxFiniteHeuristic, properties.MaxFiniteHeuristic, properties.MaxFiniteHeuristic / 2, 0, true, 0.5, properties.MaxFiniteHeuristic},
		{"not binding, cap exactly equals uncapped total", 10, 20, 5, 200, 0, false, 0, 0},
		{"not binding at saturation ceiling", properties.MaxFiniteHeuristic, properties.MaxFiniteHeuristic, 5, properties.MaxFiniteHeuristic, 0, false, 0, 0},
		{"empty outer executes no probes", 0, 20, 5, 1, 1, false, 0, 0},
		{"empty inner estimate cannot be rescaled", 10, 0, 5, 1, 1, false, 0, 0},
		{"negative cap is invalid", 10, 20, 5, -1, 1, false, 0, 0},
		{"NaN cap is invalid", 10, 20, 5, math.NaN(), 1, false, 0, 0},
		{"infinite cap is invalid", 10, 20, 5, math.Inf(1), 1, false, 0, 0},
		{"NaN cardinality is invalid", math.NaN(), 20, 5, 1, 1, false, 0, 0},
		{"infinite CPU is invalid", 10, 20, math.Inf(1), 1, 1, false, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			outer := properties.Cost{Cardinality: c.outerCard}
			inner := properties.Cost{Cardinality: c.innerCard, CPU: c.innerCPU}
			corrected, applied := fkChainCappedInnerCost(outer, inner, c.cap, c.fixedCPU)
			if applied != c.wantApplied {
				t.Fatalf("applied = %v, want %v", applied, c.wantApplied)
			}
			if !applied {
				return
			}
			if !closeEnough(corrected.Cardinality, c.wantCardinality) {
				t.Errorf("corrected.Cardinality = %v, want %v", corrected.Cardinality, c.wantCardinality)
			}
			if !closeEnough(corrected.CPU, c.wantInnerCPU) {
				t.Errorf("corrected.CPU = %v, want %v", corrected.CPU, c.wantInnerCPU)
			}
			if got := c.outerCard * corrected.Cardinality; !closeEnough(got, c.cap) {
				t.Errorf("outerCard*corrected.Cardinality = %v, want cap = %v", got, c.cap)
			}
		})
	}
}

func absF(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// TestFKChainCardinalityCap_CPUConsistentAcrossChain is the end-to-end
// regression for the CPU-consistency fix: hop3 of the forward chain (the
// exact shape TestFDB_MultiwayJoinOrder_Nway hits) must have its CPU derived
// from the SAME cap that clamps its Cardinality, not left at the
// pre-fix value computed from the uncapped (impossible) 8000-row estimate.
// Recomputes the expected Cost independently via fkChainCappedInnerCost +
// properties.FlatMapCost — the same two calls combineConcreteCost makes — so
// this pins the INVARIANT (cardinality and CPU agree on the same execution),
// not a hand-copied magic number.
func TestFKChainCardinalityCap_CPUConsistentAcrossChain(t *testing.T) {
	t.Parallel()
	stats := fkChainStats()

	fwd1 := fkChainFlat(fkChainFullScan("T1"), fkChainFKProbe(t, "T2", "t2_by_t1", "T1", fkChainAlias(0)), fkChainAlias(0))
	fwd2 := fkChainFlat(fwd1, fkChainFKProbe(t, "T3", "t3_by_t2", "T2", fkChainAlias(1)), fkChainAlias(1))
	fwd3 := fkChainFlat(fwd2, fkChainFKProbe(t, "T4", "t4_by_t3", "T3", fkChainAlias(2)), fkChainAlias(2))

	c2 := concretePlanCost(fwd2, stats, nil)
	t4Probe := fkChainFKProbe(t, "T4", "t4_by_t3", "T3", fkChainAlias(2))
	t4Cost := concretePlanCost(t4Probe, stats, nil)
	cap, ok := fkChainCardinalityCap(fwd3.(*plans.RecordQueryFlatMapPlan), stats)
	if !ok {
		t.Fatalf("cap did not fire on hop3 — precondition of this test is broken")
	}
	fixedCPU, derived := fkChainInnerFixedCPU(t4Probe, nil)
	if !derived {
		t.Fatalf("fixed CPU decomposition declined the reachable T4 probe")
	}
	corrected, applied := fkChainCappedInnerCost(c2, t4Cost, cap, fixedCPU)
	if !applied {
		t.Fatalf("cap did not bind on hop3 — precondition of this test is broken")
	}
	want := properties.FlatMapCost(c2, corrected)

	got := concretePlanCost(fwd3, stats, nil)
	if got.Cardinality != want.Cardinality || got.CPU != want.CPU {
		t.Fatalf("hop3 cost = %+v, want %+v (combineConcreteCost must derive CPU the same way this test does)", got, want)
	}

	// Regression guard against the pre-fix state: CPU derived from an
	// impossible 8000-row estimate was ~10062; the corrected CPU must be
	// substantially lower.
	if got.CPU > 3000 {
		t.Errorf("hop3 CPU = %v, want well under the pre-fix ~10062 (CPU not being derived from the cap)", got.CPU)
	}
	if got.Cardinality != 2000 {
		t.Errorf("hop3 Cardinality = %v, want 2000 (the true chain cardinality)", got.Cardinality)
	}
}

// TestFKChainCardinalityCap_CPUUnaffectedWhenCapDoesNotFire re-runs both
// disproven-counter-example shapes (non-PK bind; many outer rows sharing one
// value) and checks the CPU path, not just Cardinality: when the cap
// correctly declines, combineConcreteCost's Cost must be BIT-IDENTICAL to
// plain properties.FlatMapCost(outer, inner) — no scaling applied on either
// term. This is the CPU-side half of the soundness boundary
// TestFKChainCardinalityCap_DoesNotFireOnNonPKBind /
// TestFKChainCardinalityCap_ManyOuterRowsSharingOneValue already pin for
// Cardinality alone.
func TestFKChainCardinalityCap_CPUUnaffectedWhenCapDoesNotFire(t *testing.T) {
	t.Parallel()

	buildNonPKBind := func(t *testing.T, statsY, statsX float64) (plans.RecordQueryPlan, properties.MapStatistics) {
		t.Helper()
		outer := fkChainFullScan("Y")
		inner := mustFKChain(plans.NewRecordQueryIndexPlan("x_by_group",
			[]*predicates.ComparisonRange{fkChainCorrelatedEq(t, "Y", fkChainAlias(0), "GROUP")},
			[]string{"X"}, fkChainRowType("X"), false)).
			WithCommonPrimaryKey(fkChainIDPK())
		fm := fkChainFlat(outer, inner, fkChainAlias(0))
		return fm, properties.MapStatistics{PerType: map[string]float64{"Y": statsY, "X": statsX}}
	}

	cases := []struct {
		name  string
		build func(t *testing.T) (plans.RecordQueryPlan, properties.MapStatistics)
	}{
		{"non-PK bind, single outer row", func(t *testing.T) (plans.RecordQueryPlan, properties.MapStatistics) {
			return buildNonPKBind(t, 1, 500)
		}},
		{"non-PK bind, 1000 outer rows sharing one value", func(t *testing.T) (plans.RecordQueryPlan, properties.MapStatistics) {
			return buildNonPKBind(t, 1000, 500)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fm, stats := c.build(t)
			fmPlan := fm.(*plans.RecordQueryFlatMapPlan)
			if _, ok := fkChainCardinalityCap(fmPlan, stats); ok {
				t.Fatalf("cap fired — precondition of this test (a disproven-unsound shape) is broken")
			}
			outerCost := concretePlanCost(fmPlan.GetOuter(), stats, nil)
			innerCost := concretePlanCost(fmPlan.GetInner(), stats, nil)
			want := properties.FlatMapCost(outerCost, innerCost)
			got := concretePlanCost(fm, stats, nil)
			if got.Cardinality != want.Cardinality || got.CPU != want.CPU {
				t.Fatalf("cost = %+v, want unscaled FlatMapCost %+v — the cap must leave BOTH terms untouched when it declines", got, want)
			}
		})
	}
}

// The CURRENT inner access must itself partition physical records across
// distinct outer bind values. Guarding a fan-out index only after it becomes
// the next hop's outer thread is too late: one repeated-key record can already
// be returned by two probes in this hop, so their sum may exceed table size.
func TestFKChainCardinalityCap_DeclinesOnDuplicateProducingInnerIndex(t *testing.T) {
	t.Parallel()
	unstamped := func(t *testing.T) plans.RecordQueryPlan {
		t.Helper()
		return mustFKChain(plans.NewRecordQueryIndexPlan(
			"t2_by_t1_unstamped",
			[]*predicates.ComparisonRange{fkChainCorrelatedEq(t, "T1", fkChainAlias(0), "ID")},
			[]string{"T2"}, fkChainRowType("T2"), false,
		)).
			WithKeyComponentTypes([]values.Type{values.NotNullLong}).
			WithIndexMetadata([]string{"FK"}, []string{"ID"}, false).
			WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong}).
			WithCommonPrimaryKey(fkChainIDPK())
	}
	tests := []struct {
		name    string
		inner   func(*testing.T) plans.RecordQueryPlan
		wantCap bool
	}{
		{
			name: "explicit fan-out index",
			inner: func(t *testing.T) plans.RecordQueryPlan {
				return fkChainFanOutFKProbe(t, "T2", "t2_by_t1_fanout", "T1", fkChainAlias(0))
			},
		},
		{name: "unstamped distinct-record signal", inner: unstamped},
		{
			name: "explicit scalar index control",
			inner: func(t *testing.T) plans.RecordQueryPlan {
				return fkChainFKProbe(t, "T2", "t2_by_t1_scalar", "T1", fkChainAlias(0))
			},
			wantCap: true,
		},
	}
	stats := properties.MapStatistics{PerType: map[string]float64{"T1": 1000, "T2": 20}}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			inner := test.inner(t)
			flatMap := fkChainFlat(fkChainFullScan("T1"), inner, fkChainAlias(0))
			cap, gotCap := fkChainCardinalityCap(flatMap.(*plans.RecordQueryFlatMapPlan), stats)
			if gotCap != test.wantCap {
				t.Fatalf("fkChainCardinalityCap = (%v,%v), want proof=%v", cap, gotCap, test.wantCap)
			}
			if test.wantCap {
				if cap != 20 {
					t.Fatalf("cap = %v, want inner table size 20", cap)
				}
				return
			}
			outerCost := concretePlanCost(flatMap.(*plans.RecordQueryFlatMapPlan).GetOuter(), stats, nil)
			innerCost := concretePlanCost(inner, stats, nil)
			want := properties.FlatMapCost(outerCost, innerCost)
			if got := concretePlanCost(flatMap, stats, nil); got != want {
				t.Fatalf("duplicate-producing inner cost = %+v, want unclamped %+v", got, want)
			}
		})
	}
}

// TestFKChainCardinalityCap_DeclinesWhenOuterThreadedThroughFanOutIndex is
// HOLE 1's regression: fkChainCardinalityCap's soundness rests on every outer
// row's tracked bind value being genuinely UNIQUE across the accumulated
// outer chain. A fan-out index (createsDuplicates=true — one emitting
// multiple entries per record, e.g. an index over a repeated/nested field)
// can emit the SAME physical record and primary key more than once, so when
// T2 is threaded through such an index and then used as the OUTER for a
// further hop probing T3, repeated T2.ID values probe OVERLAPPING T3 rows —
// the true output can exceed T3's own table size (200), so the cap must
// decline rather than fire at 200.
//
// Both the direct unit (computePKThread on the fan-out-rooted hop) and the
// end-to-end cap on the next hop are asserted: the unit pins exactly the
// function this hole lives in, and the end-to-end assertion proves the unit
// result actually propagates to where an unsound cap would otherwise fire.
func TestFKChainCardinalityCap_DeclinesWhenOuterThreadedThroughFanOutIndex(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		t2Inner func(t *testing.T) plans.RecordQueryPlan
	}{
		{"explicit fan-out signal (createsDuplicates=true)", func(t *testing.T) plans.RecordQueryPlan {
			return fkChainFanOutFKProbe(t, "T2", "t2_by_t1_fanout", "T1", fkChainAlias(0))
		}},
		{"distinct-records signal never stamped (Java's empty-candidate default)", func(t *testing.T) plans.RecordQueryPlan {
			return mustFKChain(plans.NewRecordQueryIndexPlan("t2_by_t1_unstamped",
				[]*predicates.ComparisonRange{fkChainCorrelatedEq(t, "T1", fkChainAlias(0), "ID")},
				[]string{"T2"}, fkChainRowType("T2"), false)).
				WithCommonPrimaryKey(fkChainIDPK())
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			stats := fkChainStats()

			hop1 := fkChainFlat(fkChainFullScan("T1"), c.t2Inner(t), fkChainAlias(0))
			if computePKThread(hop1).ok {
				t.Fatalf("computePKThread(hop1) should be ok=false: hop1's inner leg's " +
					"distinct-records signal does not prove T2's rows are duplicate-free")
			}

			hop2 := fkChainFlat(hop1, fkChainFKProbe(t, "T3", "t3_by_t2", "T2", fkChainAlias(1)), fkChainAlias(1))
			if cap, ok := fkChainCardinalityCap(hop2.(*plans.RecordQueryFlatMapPlan), stats); ok {
				t.Fatalf("cap fired (cap=%v) on hop2 whose outer thread is rooted at a "+
					"not-provably-distinct T2 index — must decline: repeated T2 PKs can probe "+
					"overlapping T3 rows and the true output can exceed T3's table size (200)", cap)
			}
		})
	}
}

// TestFKChainCardinalityCap_DeclinesWhenProjectionReplacesTrackedPK is HOLE
// 2's regression: a RecordQueryProjectionPlan can REPLACE the tracked PK
// field with a computed/constant value while keeping its NAME — "ID" stays
// "ID" in the output schema, but it no longer distinguishes one T2 row from
// another. If the next hop's name-only check treated that "ID" as the same
// distinct underlying PK, every T2 row would bind the identical constant, so
// probes of T3 keyed off it are not partitioned by distinct row identity and
// the true output can exceed T3's table size (200) — the cap must decline.
func TestFKChainCardinalityCap_DeclinesWhenProjectionReplacesTrackedPK(t *testing.T) {
	t.Parallel()
	stats := fkChainStats()

	hop1 := fkChainFlat(fkChainFullScan("T1"), fkChainFKProbe(t, "T2", "t2_by_t1", "T1", fkChainAlias(0)), fkChainAlias(0))

	// A schema-legal projection that overwrites the tracked "ID" output with a
	// constant — the output column is still named "ID", but every row now
	// carries the SAME value, breaking the underlying distinctness the name
	// alone cannot reveal.
	brokenProjection := mustFKChain(plans.NewRecordQueryProjectionPlanWithAliases(
		[]values.Value{&values.ConstantValue{Value: int64(42), Typ: values.NotNullLong}},
		[]string{"ID"},
		hop1,
	))

	if computePKThread(brokenProjection).ok {
		t.Fatalf("computePKThread(brokenProjection) should be ok=false: the projection " +
			"replaces the tracked PK field with a constant, so \"ID\" no longer distinguishes T2 rows")
	}

	hop2 := fkChainFlat(brokenProjection, fkChainFKProbe(t, "T3", "t3_by_t2", "T2", fkChainAlias(1)), fkChainAlias(1))
	if cap, ok := fkChainCardinalityCap(hop2.(*plans.RecordQueryFlatMapPlan), stats); ok {
		t.Fatalf("cap fired (cap=%v) on hop2 whose outer thread passes through a PK-replacing "+
			"projection — must decline: every T2 row binds the SAME constant \"ID\", so T3 probes "+
			"can legitimately overlap and the true output can exceed T3's table size (200)", cap)
	}
}

// TestFKChainCardinalityCap_DeclinesWhenFlatMapResultValueDropsTrackedPK is
// the THIRD instance of hole 2's shape, found while auditing every path into
// pkThread with the same "output mapping can replace/drop/rename the tracked
// PK" risk: computeFlatMapPKThread re-roots a proven INNER pkThread through
// the FlatMap's OWN resultValue (Java's RecordQueryFlatMapPlan.resultValue,
// which can merge outer+inner fields, rename them, or drop the inner's PK
// outright), not through a Map or Projection. Every OTHER test in this file
// builds resultValue as a bare QuantifiedObjectValue over the inner alias —
// the identity fast path — so this is the only test that exercises a
// non-identity FlatMap resultValue at all.
func TestFKChainCardinalityCap_DeclinesWhenFlatMapResultValueDropsTrackedPK(t *testing.T) {
	t.Parallel()
	stats := fkChainStats()

	// hop1's OWN resultValue is a constant, not QOV("i") — it discards the
	// probed T2 row (and its PK) entirely from what the FlatMap actually
	// emits, even though the inner leg's own scan fully binds T1's PK.
	hop1 := mustFKChain(plans.NewRecordQueryFlatMapPlan(
		fkChainFullScan("T1"), fkChainFKProbe(t, "T2", "t2_by_t1", "T1", fkChainAlias(0)),
		fkChainAlias(0), values.NamedCorrelationIdentifier("i"),
		&values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}, false,
	))

	if computePKThread(hop1).ok {
		t.Fatalf("computePKThread(hop1) should be ok=false: hop1's OWN resultValue is a " +
			"constant, not the inner row it probed — the FlatMap's actual output does not " +
			"carry T2's tracked PK at all")
	}

	hop2 := fkChainFlat(hop1, fkChainFKProbe(t, "T3", "t3_by_t2", "T2", fkChainAlias(1)), fkChainAlias(1))
	if cap, ok := fkChainCardinalityCap(hop2.(*plans.RecordQueryFlatMapPlan), stats); ok {
		t.Fatalf("cap fired (cap=%v) on hop2 whose outer thread is hop1, whose OWN resultValue "+
			"drops the tracked PK — must decline: hop1's output no longer identifies distinct "+
			"T2 rows, so T3 probes off it can legitimately exceed T3's table size (200)", cap)
	}
}

// TestFKChainCardinalityCap_DeclinesOnSameLeafNameDifferentDomain is RFC-197
// item 2's DIMENSION test for this site: the bind operand and the outer's
// tracked primary-key column share a leaf name AND a correlation AND an
// ordinal, and differ only in the LAYOUT the ordinal indexes.
//
// The old proof returned `fv.Field` as a bare string and matched it against a
// set of PK NAMES, so "ID" == "ID" was the whole argument — this shape FIRED
// the cap, clamping the hop's cardinality on the strength of a bind that
// reads a column of a different row layout. Identity is (correlation, domain,
// ordinal); everything but the domain agrees here, so the domain is the only
// element that can refuse it, and a conversion that checked correlation and
// ordinal but dropped the domain would still fail this test.
//
// An unsound cap is not a worse plan: it is a cardinality estimate lower than
// what the query can actually produce, which is exactly what
// fkChainCardinalityCap's own doc comment establishes must never happen.
func TestFKChainCardinalityCap_DeclinesOnSameLeafNameDifferentDomain(t *testing.T) {
	t.Parallel()

	// The outer is T2, whose tracked PK column ID sits at ordinal 0. The bind
	// operand is spelled ID, correlated to the outer alias, and ALSO baked at
	// ordinal 0 — but in X's layout, a different column order entirely.
	outer := fkChainFullScan("T2")
	misdomained := fkChainCorrelatedRef(t, "X", fkChainAlias(0), "ID")
	fv, ok := values.AsFieldValue(misdomained)
	if !ok {
		t.Fatalf("test setup: operand = %T, want exact FieldValue", misdomained)
	}
	accessor, ok := fv.Path().Accessor(0)
	if !ok || fv.DisplayName() != "ID" || accessor.Ordinal() != 0 {
		t.Fatalf("test setup: the operand must share the PK column's leaf name and ordinal, got %q at %d",
			fv.DisplayName(), accessor.Ordinal())
	}
	if fv.Path().RootDomain() == values.OrdinalDomainOfType(fkChainRowType("T2")) {
		t.Fatal("test setup: the operand's domain must differ from the outer's, or nothing separates the two references")
	}

	cmp := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: misdomained}
	res := predicates.EmptyComparisonRange().Merge(&cmp)
	if !res.Ok {
		t.Fatal("failed to build the misdomained eq range")
	}
	inner := mustFKChain(plans.NewRecordQueryIndexPlan("t3_by_t2",
		[]*predicates.ComparisonRange{res.Range},
		[]string{"T3"}, fkChainRowType("T3"), false)).
		WithCommonPrimaryKey(fkChainIDPK()).
		WithDistinctRecordsSignal(false)
	fm := fkChainFlat(outer, inner, fkChainAlias(0)).(*plans.RecordQueryFlatMapPlan)

	if cap, ok := fkChainCardinalityCap(fm, fkChainStats()); ok {
		t.Fatalf("cap fired (cap=%v) on a bind whose ordinal indexes ANOTHER layout — only its "+
			"NAME matches the outer's tracked PK column; the cap must decline", cap)
	}
}

// TestFKChainCardinalityCap_FlatMapOuterLayoutComesFromResultValue pins the
// capability the identity conversion needed: a hop whose OUTER is a FlatMap —
// which is EVERY hop of a chain past the first — must expose the exact layout
// selected by its result value.
//
// planRowLayout derives it from the resultValue. Without that derivation the
// cap fires on hop 1 and silently stops firing on hops 2..n, which is exactly
// the order-dependent estimate this file exists to remove: the assertions
// below are the per-hop form of TestFKChainCardinalityCap_OrderInvariant, so
// a regression names the cause instead of only the symptom.
func TestFKChainCardinalityCap_FlatMapOuterLayoutComesFromResultValue(t *testing.T) {
	t.Parallel()
	stats := fkChainStats()

	hop1 := fkChainFlat(fkChainFullScan("T1"), fkChainFKProbe(t, "T2", "t2_by_t1", "T1", fkChainAlias(0)), fkChainAlias(0))
	// hop1 emits the INNER's rows (its resultValue is QOV over the inner
	// alias), so its layout and declared result type are both T2's.
	if got := planRowLayout(hop1); values.OrdinalDomainOfType(got) != values.OrdinalDomainOfType(fkChainRowType("T2")) {
		t.Fatalf("planRowLayout(hop1) = %v, want T2's layout (the FlatMap emits its inner leg's row)", got)
	}
	if !hop1.GetResultType().Equals(fkChainRowType("T2")) {
		t.Fatalf("FlatMap result type = %v, want exact T2 layout", hop1.GetResultType())
	}

	hop2 := fkChainFlat(hop1, fkChainFKProbe(t, "T3", "t3_by_t2", "T2", fkChainAlias(1)), fkChainAlias(1)).(*plans.RecordQueryFlatMapPlan)
	cap, ok := fkChainCardinalityCap(hop2, stats)
	if !ok {
		t.Fatal("cap did not fire on hop2 — the FlatMap outer's layout was not derived, so the " +
			"identity proof had no frontier to state itself in and failed closed")
	}
	if cap != 200 {
		t.Fatalf("cap = %v, want T3's table size 200", cap)
	}
}

// TestFKChainCardinalityCap_DeclinesOnSameLayoutOtherCorrelation is RFC-197
// item 2's CORRELATION dimension for innerFullyBindsThread — the element the
// rest of this file cannot vary on its own.
//
// Every other decline here is reached by something else: the misdomained bind
// is refused by the layout, the nested one by its multi-accessor shape, the
// non-PK one by its ordinal. So a conversion that compared domain and ordinal
// and simply ASSUMED the leg would pass the whole suite. A SELF-JOIN removes
// that cover: both legs scan T2, so there is ONE layout, and the inner's search
// binds `i.ID` — the inner's OWN copy of the column, at the same ordinal in the
// same domain as the outer's tracked primary key, differing only in which
// quantifier's row it reads.
//
// That bind proves nothing the cap needs. innerFullyBindsThread's whole
// argument is that each inner execution is keyed to ONE outer row, which
// requires the equality to be parameterized by the OUTER's primary key; an
// equality on the inner's own column is re-evaluated identically for every
// outer row and partitions nothing. Firing the cap on it clamps the hop below
// what the query can produce — the one failure this file's doc comment says
// must never happen.
func TestFKChainCardinalityCap_DeclinesOnSameLayoutOtherCorrelation(t *testing.T) {
	t.Parallel()

	outerAlias := fkChainAlias(0)
	innerAlias := values.NamedCorrelationIdentifier("i")

	// Self-join on T2: one layout, so the DOMAIN element cannot do the work.
	outer := fkChainFullScan("T2")
	selfBind := fkChainCorrelatedRef(t, "T2", innerAlias, "ID")
	outerBind := fkChainCorrelatedRef(t, "T2", outerAlias, "ID")
	selfFV, selfOK := values.AsFieldValue(selfBind)
	outerFV, outerOK := values.AsFieldValue(outerBind)
	if !selfOK || !outerOK {
		t.Fatalf("test setup: operands = (%T,%T), want exact FieldValues", selfBind, outerBind)
	}
	if selfFV.Path().RootDomain() != outerFV.Path().RootDomain() {
		t.Fatal("test setup: both operands must be baked in ONE layout so the DOMAIN cannot reject")
	}
	selfAccessor, selfAccessorOK := selfFV.Path().Accessor(0)
	outerAccessor, outerAccessorOK := outerFV.Path().Accessor(0)
	if !selfAccessorOK || !outerAccessorOK ||
		selfAccessor.Ordinal() != outerAccessor.Ordinal() || selfFV.DisplayName() != outerFV.DisplayName() {
		t.Fatal("test setup: both operands must share the ordinal and the leaf name, leaving only the correlation")
	}

	cmp := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: selfBind}
	res := predicates.EmptyComparisonRange().Merge(&cmp)
	if !res.Ok {
		t.Fatal("failed to build the self-correlated eq range")
	}
	inner := mustFKChain(plans.NewRecordQueryScanPlan([]string{"T2"}, fkChainRowType("T2"), false)).
		WithPrimaryKey(fkChainIDPK()).
		WithScanComparisons([]*predicates.ComparisonRange{res.Range}).
		WithKeyComponentTypes([]values.Type{values.NotNullLong})
	fm := mustFKChain(plans.NewRecordQueryFlatMapPlan(outer, inner, outerAlias, innerAlias,
		mustFKChain(values.NewQuantifiedObjectValue(innerAlias, fkChainRowType("T2"))), false))

	if cap, ok := fkChainCardinalityCap(fm, fkChainStats()); ok {
		t.Fatalf("cap fired (cap=%v) on an equality bound to the INNER leg's own column — "+
			"it is not parameterized by the outer row at all, so it partitions nothing and "+
			"cannot key each inner execution to one outer row", cap)
	}
}

// TestFKChainCardinalityCap_FiresOnSameLayoutOuterCorrelation is the accept
// companion in the SAME self-join geometry: it holds the layout, the ordinal
// and the leaf name identical to the case above and moves only the correlation
// back onto the outer leg.
//
// Without it the decline above could be satisfied by a conversion that simply
// stopped recognizing self-joins, which would silently drop the cap for every
// same-table chain rather than for the wrong leg.
func TestFKChainCardinalityCap_FiresOnSameLayoutOuterCorrelation(t *testing.T) {
	t.Parallel()

	outerAlias := fkChainAlias(0)
	innerAlias := values.NamedCorrelationIdentifier("i")

	outer := fkChainFullScan("T2")
	inner := mustFKChain(plans.NewRecordQueryScanPlan([]string{"T2"}, fkChainRowType("T2"), false)).
		WithPrimaryKey(fkChainIDPK()).
		WithScanComparisons([]*predicates.ComparisonRange{fkChainCorrelatedEq(t, "T2", outerAlias, "ID")}).
		WithKeyComponentTypes([]values.Type{values.NotNullLong})
	fm := mustFKChain(plans.NewRecordQueryFlatMapPlan(outer, inner, outerAlias, innerAlias,
		mustFKChain(values.NewQuantifiedObjectValue(innerAlias, fkChainRowType("T2"))), false))

	cap, ok := fkChainCardinalityCap(fm, fkChainStats())
	if !ok {
		t.Fatal("cap did not fire on a genuine outer-parameterized primary-key probe in a " +
			"self-join — the correlation check must reject the wrong leg, not the shape")
	}
	if cap != 20 {
		t.Fatalf("cap = %v, want T2's table size 20", cap)
	}
}

// TestPKThreadThroughResultValue_DeclinesSameLayoutOtherCorrelation is the
// CORRELATION dimension for the OTHER identity comparison in this file:
// findDirectFieldMapping, which decides whether a FlatMap's emitted record
// still carries the tracked primary key.
//
// A slot only preserves the thread if it is a DIRECT read of the tracked
// column off the CHILD. In a self-join both legs share a layout, so a slot
// reading the OUTER leg's ID agrees with the inner's tracked PK on domain,
// ordinal and leaf name, and differs only in which row it reads. Accepting it
// carries a pkThread forward over rows the key no longer identifies: the next
// hop then proves its 1:1 bind against a key that is not this stream's, and
// caps a fan-out away.
func TestPKThreadThroughResultValue_DeclinesSameLayoutOtherCorrelation(t *testing.T) {
	t.Parallel()

	childAlias := values.NamedCorrelationIdentifier("i")
	otherAlias := fkChainAlias(0)
	childLayout := fkChainRowType("T2")
	childThread := pkThread{
		recordType: "T2",
		pkValues:   fkChainIDPK(),
		pkTypes:    []values.Type{values.NotNullLong},
		ok:         true,
	}

	// The accept direction first, so the decline below cannot pass by refusing
	// everything.
	survived := pkThreadThroughResultValue(childThread, childAlias, childLayout,
		values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "KEY", Value: fkChainCorrelatedRef(t, "T2", childAlias, "ID")},
		))
	if !survived.ok {
		t.Fatal("a slot that IS a direct read of the child's tracked primary key did not " +
			"preserve the thread — the identity comparison over-declines")
	}

	// Same layout, same ordinal, same leaf name — the OUTER quantifier.
	crossed := pkThreadThroughResultValue(childThread, childAlias, childLayout,
		values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "KEY", Value: fkChainCorrelatedRef(t, "T2", otherAlias, "ID")},
		))
	if crossed.ok {
		t.Fatalf("a slot reading the OUTER leg's ID preserved the child's primary-key thread "+
			"(recordType=%q, %d component(s)) — ordinal 0 of two quantifiers are different "+
			"columns, and the emitted rows are not identified by the one this thread tracks",
			crossed.recordType, len(crossed.pkValues))
	}
}

func fkTypedEquality(
	t *testing.T,
	layout *values.RecordType,
	alias values.CorrelationIdentifier,
	field string,
) *predicates.ComparisonRange {
	t.Helper()
	_, ok := layout.FieldIndexUnique(field)
	if !ok {
		t.Fatalf("layout %s has no field %s", layout.RecordName, field)
	}
	operand := fkChainField(layout, alias, field)
	comparison := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: operand}
	merged := predicates.EmptyComparisonRange().Merge(&comparison)
	if !merged.Ok {
		t.Fatal("failed to construct typed correlated equality")
	}
	return merged.Range
}

// Distinct raw outer PK rows induce disjoint inner probes only when logical
// equality is injective from the outer key domain into the inner physical key
// domain. FLOAT/DOUBLE outer keys fail that theorem because -0/+0 (and raw
// NaNs) are distinct physical keys but one logical value; both outer rows can
// execute the same plural inner range set and return the same inner rows.
func TestFKChainCardinalityCap_LogicalEqualityProjectionMustBeInjective(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		sourceType values.Type
		targetType values.Type
		wantCap    bool
	}{
		{name: "LONG identity control", sourceType: values.NotNullLong, targetType: values.NotNullLong, wantCap: true},
		{name: "INT to DOUBLE exact control", sourceType: values.NotNullInt, targetType: values.NotNullDouble, wantCap: true},
		{name: "FLOAT signed-zero aliases", sourceType: values.NotNullFloat, targetType: values.NotNullFloat},
		{name: "DOUBLE signed-zero and NaN aliases", sourceType: values.NotNullDouble, targetType: values.NotNullDouble},
		{name: "LONG to DOUBLE rounding aliases", sourceType: values.NotNullLong, targetType: values.NotNullDouble},
		{name: "INT to FLOAT rounding aliases", sourceType: values.NotNullInt, targetType: values.NotNullFloat},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			outerLayout := values.NewRecordType("O", false, []values.Field{{
				Name: "K", FieldType: test.sourceType, Ordinal: 0,
			}})
			innerLayout := values.NewRecordType("I", false, []values.Field{{
				Name: "FK", FieldType: test.targetType, Ordinal: 0,
			}})
			outerAlias := values.NamedCorrelationIdentifier("o")
			innerAlias := values.NamedCorrelationIdentifier("i")
			outer := mustFKChain(plans.NewRecordQueryScanPlan([]string{"O"}, outerLayout, false)).
				WithPrimaryKey([]values.Value{fkChainField(outerLayout, values.UniqueCorrelationIdentifier(), "K")}).
				WithKeyComponentTypes([]values.Type{test.sourceType})
			inner := mustFKChain(plans.NewRecordQueryScanPlan([]string{"I"}, innerLayout, false)).
				WithPrimaryKey([]values.Value{fkChainField(innerLayout, values.UniqueCorrelationIdentifier(), "FK")}).
				WithScanComparisons([]*predicates.ComparisonRange{
					fkTypedEquality(t, outerLayout, outerAlias, "K"),
				}).
				WithKeyComponentTypes([]values.Type{test.targetType})
			flatMap := mustFKChain(plans.NewRecordQueryFlatMapPlan(
				outer, inner, outerAlias, innerAlias,
				mustFKChain(values.NewQuantifiedObjectValue(innerAlias, innerLayout)), false,
			))
			stats := properties.MapStatistics{PerType: map[string]float64{"O": 1000, "I": 20}}
			cap, gotCap := fkChainCardinalityCap(flatMap, stats)
			if gotCap != test.wantCap {
				t.Fatalf("fkChainCardinalityCap = (%v,%v), want proof=%v", cap, gotCap, test.wantCap)
			}
			if gotCap && cap != 20 {
				t.Fatalf("cap = %v, want inner table size 20", cap)
			}
			gotCost := concretePlanCost(flatMap, stats, nil)
			ordinary := properties.FlatMapCost(
				concretePlanCost(outer, stats, nil), concretePlanCost(inner, stats, nil),
			)
			if !test.wantCap && gotCost != ordinary {
				t.Fatalf("unsafe projection cost = %+v, want ordinary unclamped FlatMapCost %+v", gotCost, ordinary)
			}
			if test.wantCap && gotCost.Cardinality > cap {
				t.Fatalf("injective projection cardinality = %v, exceeds proven cap %v", gotCost.Cardinality, cap)
			}
			if test.name == "LONG identity control" {
				// All 1,000 outer rows still open an isolated full-PK probe.
				// Only 20 can return a row, so cardinality is capped while the
				// FetchCPU startup remains payable once per outer execution.
				if gotCost.Cardinality != 20 || gotCost.CPU != 1480.5 {
					t.Fatalf("point-probe capped cost = %+v, want {Cardinality:20 CPU:1480.5}", gotCost)
				}
				if ordinary.CPU != 1480.5 {
					t.Fatalf("ordinary point-probe CPU = %v, want 1480.5; cap must not reduce per-probe startup", ordinary.CPU)
				}
			}
		})
	}
}

// A correlated INT equality against the first component of a DOUBLE tuple key
// must cover both physical encodings when the key has a constrained suffix.
// The FK-table cap may reduce the average rows returned per outer execution,
// but every execution still opens that second signed-zero range. Fetch and
// TypeFilter wrap the fixed intercept in their own physical CPU multiplier;
// neither turns it into row-dependent work.
func TestFKChainCardinalityCap_PreservesSignedZeroRangeSeekThroughWrappers(t *testing.T) {
	t.Parallel()
	outerLayout := values.NewRecordType("O", false, []values.Field{
		{Name: "K", FieldType: values.NotNullInt, Ordinal: 0},
		{Name: "S", FieldType: values.NotNullLong, Ordinal: 1},
	})
	innerLayout := values.NewRecordType("I", false, []values.Field{
		{Name: "FK", FieldType: values.NotNullDouble, Ordinal: 0},
		{Name: "FS", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 2},
	})
	outerAlias := values.NamedCorrelationIdentifier("o")
	innerAlias := values.NamedCorrelationIdentifier("i")
	outer := mustFKChain(plans.NewRecordQueryScanPlan([]string{"O"}, outerLayout, false)).
		WithPrimaryKey([]values.Value{
			fkChainField(outerLayout, values.UniqueCorrelationIdentifier(), "K"),
			fkChainField(outerLayout, values.UniqueCorrelationIdentifier(), "S"),
		}).
		WithKeyComponentTypes([]values.Type{values.NotNullInt, values.NotNullLong})
	bareInner := mustFKChain(plans.NewRecordQueryIndexPlan(
		"i_by_fk_fs",
		[]*predicates.ComparisonRange{
			fkTypedEquality(t, outerLayout, outerAlias, "K"),
			fkTypedEquality(t, outerLayout, outerAlias, "S"),
		},
		[]string{"I"}, innerLayout, false,
	)).
		WithKeyComponentTypes([]values.Type{values.NotNullDouble, values.NotNullLong}).
		WithIndexMetadata([]string{"FK", "FS"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithCommonPrimaryKey([]values.Value{
			fkChainField(innerLayout, values.UniqueCorrelationIdentifier(), "ID"),
		}).
		WithDistinctRecordsSignal(false)

	shape := properties.PhysicalEqualityShapeForComparisons(
		bareInner.GetScanComparisons(), bareInner.GetKeyComponentTypes(), true,
	)
	if !shape.SuccessfulSeekUpperBoundExact || shape.SuccessfulSeekUpperBound != 2 {
		t.Fatalf("signed-zero inner shape seeks = (%d, exact=%v), want (2,true)",
			shape.SuccessfulSeekUpperBound, shape.SuccessfulSeekUpperBoundExact)
	}

	stats := properties.MapStatistics{PerType: map[string]float64{"O": 1000, "I": 20}}
	tests := []struct {
		name      string
		wrap      func(plans.RecordQueryPlan) plans.RecordQueryPlan
		wantFixed float64
	}{
		{
			name:      "bare index range set",
			wrap:      func(p plans.RecordQueryPlan) plans.RecordQueryPlan { return p },
			wantFixed: properties.PhysicalRangeSeekCost,
		},
		{
			name: "fetch",
			wrap: func(p plans.RecordQueryPlan) plans.RecordQueryPlan {
				return mustFKChain(plans.NewRecordQueryFetchFromPartialRecordPlan(
					p, nil, innerLayout, plans.FetchIndexRecordsPrimaryKey,
				))
			},
			wantFixed: properties.PhysicalRangeSeekCost * properties.PhysicalWrapperCostMultiplier,
		},
		{
			name: "type filter over fetch",
			wrap: func(p plans.RecordQueryPlan) plans.RecordQueryPlan {
				fetch := mustFKChain(plans.NewRecordQueryFetchFromPartialRecordPlan(
					p, nil, innerLayout, plans.FetchIndexRecordsPrimaryKey,
				))
				return mustFKChain(plans.NewRecordQueryTypeFilterPlan(
					[]string{"I"},
					fetch,
				))
			},
			wantFixed: properties.PhysicalRangeSeekCost *
				properties.PhysicalWrapperCostMultiplier * properties.PhysicalWrapperCostMultiplier,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			inner := test.wrap(bareInner)
			flatMap := mustFKChain(plans.NewRecordQueryFlatMapPlan(
				outer, inner, outerAlias, innerAlias,
				mustFKChain(values.NewQuantifiedObjectValue(innerAlias, innerLayout)), false,
			))
			cap, ok := fkChainCardinalityCap(flatMap, stats)
			if !ok || cap != 20 {
				t.Fatalf("fkChainCardinalityCap = (%v,%v), want (20,true)", cap, ok)
			}
			fixed, ok := fkChainInnerFixedCPU(inner, nil)
			if !ok || fixed != test.wantFixed {
				t.Fatalf("fixed inner CPU = (%v,%v), want (%v,true)", fixed, ok, test.wantFixed)
			}
			outerCost := concretePlanCost(outer, stats, nil)
			innerCost := concretePlanCost(inner, stats, nil)
			corrected, applied := fkChainCappedInnerCost(outerCost, innerCost, cap, fixed)
			if !applied {
				t.Fatal("table cap did not bind")
			}
			ratio := corrected.Cardinality / innerCost.Cardinality
			wantInnerCPU := test.wantFixed + (innerCost.CPU-test.wantFixed)*ratio
			if corrected.CPU != wantInnerCPU {
				t.Fatalf("corrected inner CPU = %v, want affine fixed+variable*r = %v", corrected.CPU, wantInnerCPU)
			}
			got := concretePlanCost(flatMap, stats, nil)
			want := properties.FlatMapCost(outerCost, corrected)
			if got != want {
				t.Fatalf("capped FlatMap cost = %+v, want %+v", got, want)
			}
		})
	}
}

// The structural PK slice and the physical type slice share primary-key
// component order. Display spellings do not participate in that alignment:
// two source slots can render with the same name and still carry different
// physical tuple domains. A name-keyed map collapsed this case and either
// declined the FK-chain proof or, after a last-write-wins refactor, could
// attach the wrong signed-zero theorem to a coordinate.
func TestAlignIndexPKTypesByCoordinate_DuplicateDisplayNamesStayPositional(t *testing.T) {
	t.Parallel()
	duplicateLayout := &values.RecordType{RecordName: "duplicate_display_names", Fields: []values.Field{
		{Name: "DUP", Ordinal: 0, FieldType: values.NotNullDouble},
		{Name: "DUP", Ordinal: 1, FieldType: values.NotNullFloat},
	}}
	root := mustFKChain(values.NewQuantifiedObjectValue(
		values.UniqueCorrelationIdentifier(), duplicateLayout))
	pk := []values.Value{
		mustFKChain(values.ResolveFieldOrdinals(root, []int{1})),
		mustFKChain(values.ResolveFieldOrdinals(root, []int{0})),
	}
	physicalTypes := []values.Type{values.NotNullFloat, values.NotNullDouble}
	aligned, ok := alignIndexPKTypesByCoordinate(
		pk, 2, physicalTypes,
	)
	if !ok {
		t.Fatal("coordinate-aligned duplicate display names were declined")
	}
	if len(aligned) != 2 || aligned[0] != values.NotNullFloat || aligned[1] != values.NotNullDouble {
		t.Fatalf("aligned types = %v, want [FLOAT DOUBLE] in PK-component order", aligned)
	}

	prefixed, ok := alignIndexPKTypesByCoordinate(
		append([]values.Value{values.NewRecordTypeValue(nil)}, pk...),
		2, physicalTypes,
	)
	if !ok || len(prefixed) != 3 || prefixed[0] != values.UnknownType ||
		prefixed[1] != values.NotNullFloat || prefixed[2] != values.NotNullDouble {
		t.Fatalf("record-type-prefixed alignment = %v ok=%v, want [UNKNOWN FLOAT DOUBLE]", prefixed, ok)
	}
}

// TestFKChainCardinalityCap_FlatMapLayoutArmDeclinesAndPicksOuter drives the
// OUTER branch and the two LOCAL declines -- a resultValue that is not a bare
// QOV, and one correlated to neither leg.
//
// It deliberately uses scan legs, and that is its limit: wherever a leg HAS a
// layout the arm and plain fall-through agree, so none of these three cases can
// tell them apart on its own. Measured, not assumed -- under a mutation that
// replaces the OUTER recursion with fall-through, this test still passes. The
// discriminating case is the nested one, and it lives in the sibling test
// below; these three pin the paths, that one pins the reason.
func TestFKChainCardinalityCap_FlatMapLayoutArmDeclinesAndPicksOuter(t *testing.T) {
	t.Parallel()

	outerAlias, innerAlias := fkChainAlias(0), values.NamedCorrelationIdentifier("i")
	outer := fkChainFullScan("T1")
	inner := fkChainFKProbe(t, "T2", "t2_by_t1", "T1", outerAlias)

	newFlat := func(rv values.Value) plans.RecordQueryPlan {
		return mustFKChain(plans.NewRecordQueryFlatMapPlan(
			outer, inner, outerAlias, innerAlias, rv, false))
	}

	// OUTER correlation: the emitted row is the outer leg's, so the layout is
	// T1's. This is a SYNTHETIC shape, not the production chain path --
	// fkChainFlat always correlates to the inner alias, and the hop-1 test above
	// pins hop1 emitting its INNER leg. The branch is reachable and worth
	// driving; it is just not the one a chain hop takes.
	outerQOV := mustFKChain(values.NewQuantifiedObjectValue(outerAlias, planRowLayout(outer)))
	if got := planRowLayout(newFlat(outerQOV)); values.OrdinalDomainOfType(got) != values.OrdinalDomainOfType(fkChainRowType("T1")) {
		t.Errorf("planRowLayout over an OUTER-correlated resultValue = %v, want T1's layout", got)
	}

	// Not a bare QOV: a value that still HAS a type, which is exactly why the
	// decline must be explicit. Returning that type would let the identity
	// proof rest on a row this file never named.
	notAQOV := values.NewNullValue(fkChainRowType("T2"))
	if notAQOV.Type() == nil {
		t.Fatal("fixture is vacuous: the non-QOV resultValue must carry a type, " +
			"otherwise this case cannot distinguish declining from having nothing to return")
	}
	if got := planRowLayout(newFlat(notAQOV)); got != nil {
		t.Errorf("planRowLayout over a non-QOV resultValue = %v, want nil (decline)", got)
	}

	// A QOV correlated to NEITHER leg: reaches the switch and falls past both
	// cases, which is the second nil and a different path from the one above.
	foreign := mustFKChain(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("elsewhere"), fkChainRowType("T2")))
	if got := planRowLayout(newFlat(foreign)); got != nil {
		t.Errorf("planRowLayout over a QOV correlated to neither leg = %v, want nil (decline)", got)
	}
}

// TestFKChainCardinalityCap_FlatMapDeclinePropagatesThroughNesting drives the
// recursion the sibling test above cannot reach: with scan legs the arm and
// plain fall-through agree, so nothing there discriminates them.
//
// It calls planRowLayout DIRECTLY. That is deliberate and it is also this
// test's limit: the fixtures below are NOT production shapes. The comment on
// planRowLayout explains why the transitive decline cannot arrive through the
// FK-cap entries at all. The assertions below pin what is pinnable: gate one
// (scanBindingOfLeaf excluding a FlatMap) is mutation-proven; the nil-layout
// rejection is over-determined and only the OUTCOME can be pinned, which the
// comment beside it says outright.
//
// Both recursions are driven, because the arm has two and a mutation to either
// must be caught: replacing the inner one with fall-through once left the whole
// target green.
func TestFKChainCardinalityCap_FlatMapDeclinePropagatesThroughNesting(t *testing.T) {
	t.Parallel()

	outerAlias, innerAlias := fkChainAlias(0), values.NamedCorrelationIdentifier("i")
	base := fkChainFullScan("T1")
	probe := fkChainFKProbe(t, "T2", "t2_by_t1", "T1", outerAlias)

	// A FlatMap that DECLINES: its resultValue is not a bare QOV.
	declining := mustFKChain(plans.NewRecordQueryFlatMapPlan(
		base, probe, outerAlias, innerAlias,
		values.NewNullValue(fkChainRowType("T2")), false))
	if got := planRowLayout(declining); got != nil {
		t.Fatalf("fixture is vacuous: the nested FlatMap must decline, got %v", got)
	}
	if declining.GetResultType() == nil {
		t.Fatal("fixture is vacuous: fall-through must have SOMETHING to return, " +
			"otherwise neither case below can distinguish the arm from fall-through")
	}

	// INNER leg. The wrapper selects its inner, which
	// is the declining FlatMap, so the decline must propagate.
	wrapAlias := fkChainAlias(1)
	viaInner := mustFKChain(plans.NewRecordQueryFlatMapPlan(
		base, declining, wrapAlias, innerAlias,
		mustFKChain(values.NewQuantifiedObjectValue(innerAlias, declining.GetResultType())), false))
	if got := planRowLayout(viaInner); got != nil {
		t.Errorf("planRowLayout over a FlatMap whose INNER declines = %v, want nil "+
			"(the decline must propagate through the recursion production takes)", got)
	}

	// OUTER leg -- the same property on the other recursion. Synthetic, but the
	// arm has two recursions and a mutation to either must be caught.
	viaOuter := mustFKChain(plans.NewRecordQueryFlatMapPlan(
		declining, probe, wrapAlias, innerAlias,
		mustFKChain(values.NewQuantifiedObjectValue(wrapAlias, declining.GetResultType())), false))
	if got := planRowLayout(viaOuter); got != nil {
		t.Errorf("planRowLayout over a FlatMap whose OUTER declines = %v, want nil "+
			"(the decline must propagate through nesting)", got)
	}

	// WHAT THE NEXT TWO ASSERTIONS ACTUALLY BUY, which is not the same thing for
	// each. The comment on planRowLayout says the transitive decline cannot
	// arrive through the FK-cap entries; these are what stand behind that.
	// scanBindingOfLeaf's exclusion of a FlatMap is genuinely pinned -- add a
	// FlatMap case and the first assertion reddens. The nil-layout rejection is
	// NOT pinned to any single guard, for the structural reason set out beside
	// it below: only the outcome can be asserted. An earlier version of this
	// block claimed both were pinned; that was measured false for the second and
	// the phrasing is deliberately not repeated here, so a grep for it stays at
	// zero.
	if _, ok := scanBindingOfLeaf(declining); ok {
		t.Error("scanBindingOfLeaf accepted a FlatMap: the inner-alias orientation " +
			"is no longer excluded, so planRowLayout's unreachability claim is stale")
	}
	if computePKThread(viaInner).ok {
		t.Error("computePKThread(viaInner) is ok: a FlatMap-inner now threads a PK, " +
			"so the transitive decline is reachable and the comment must be rewritten")
	}
	// GATE 2, the frontier check. Reaching it needs an outer that THREADS a PK
	// while declining a layout, which is exactly the RecordConstructor shape the
	// comment on planRowLayout names: pkThreadThroughResultValue accepts a
	// direct PK read, planRowLayout accepts only a bare QOV. Anything simpler
	// (a NullValue outer, say) dies at !outerThread.ok one frame above and the
	// assertion is then vacuous -- measured, not assumed.
	rcOuter := mustFKChain(plans.NewRecordQueryFlatMapPlan(
		base, probe, outerAlias, innerAlias,
		values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "ID", Value: fkChainCorrelatedRef(t, "T2", innerAlias, "ID")},
		), false))
	if !computePKThread(rcOuter).ok {
		t.Fatal("fixture is vacuous: the RecordConstructor outer must THREAD a PK, " +
			"otherwise the frontier gate is never reached and this assertion proves nothing")
	}
	if planRowLayout(rcOuter) != nil {
		t.Fatal("fixture is vacuous: the RecordConstructor outer must DECLINE a layout, " +
			"otherwise the frontier is known and the gate is never exercised")
	}
	// The probe must be baked against rcOuter's OWN emitted layout, not T1's.
	// rcOuter emits the one-column RecordConstructor row; baking against T1's
	// [ID, ADDRESS] domain would give correlatedFieldIdentity an independent
	// reason to reject, and the assertion below would then pass for the wrong
	// reason -- green even if a correctly baked outer started being accepted.
	rcLayout, isRecord := rcOuter.GetResultType().(*values.RecordType)
	if !isRecord {
		t.Fatalf("fixture is vacuous: rcOuter must emit a record layout to bake against, got %T",
			rcOuter.GetResultType())
	}
	wrapEq := predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: fkChainField(rcLayout, wrapAlias, "ID"),
	}
	wrapRange := predicates.EmptyComparisonRange().Merge(&wrapEq)
	if !wrapRange.Ok {
		t.Fatal("fixture is vacuous: could not build the correlated equality range")
	}
	wrapProbe := mustFKChain(plans.NewRecordQueryIndexPlan("t2_by_t1",
		[]*predicates.ComparisonRange{wrapRange.Range},
		[]string{"T2"}, fkChainRowType("T2"), false)).
		WithKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithIndexMetadata([]string{"FK"}, []string{"ID"}, false)
	viaRC := mustFKChain(plans.NewRecordQueryFlatMapPlan(
		rcOuter, wrapProbe, wrapAlias, innerAlias,
		mustFKChain(values.NewQuantifiedObjectValue(innerAlias, planRowLayout(wrapProbe))), false))
	// This pins the REJECTION, not one gate, and the distinction is structural
	// rather than a limit of this fixture. frontier.IsKnown() is false exactly
	// when the layout is not a *RecordType or has no fields; threadPKIdentity
	// succeeds only when it IS a *RecordType with an in-range ordinal. The two
	// are mutually exclusive, so a nil outerLayout is refused THREE times over
	// -- the frontier check, threadPKIdentity, and then the empty-wantKeys
	// return -- and the frontier guard is strictly subsumed by the others.
	// Measured: disabling it alone leaves the whole suite green. No fixture can
	// isolate that line, so claiming this assertion "pins the frontier gate"
	// would be unsupportable. What it does pin is the outcome, and that is not
	// vacuous: strip all three layers and this fires.
	if innerFullyBindsThread(viaRC, computePKThread(rcOuter)) {
		t.Error("innerFullyBindsThread accepted an outer whose layout is nil: every " +
			"path that used to fail closed on an unknown domain now admits it, so " +
			"planRowLayout's unreachability claim is stale")
	}
}

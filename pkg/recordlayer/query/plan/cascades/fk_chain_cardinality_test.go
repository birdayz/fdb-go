package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

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

func fkChainIDPK() []values.Value {
	return []values.Value{&values.FieldValue{Field: "ID", Typ: values.UnknownType}}
}

// fkChainCorrelatedEq builds a ComparisonRange equal to `outerAlias.<field>`.
func fkChainCorrelatedEq(t *testing.T, outerAlias values.CorrelationIdentifier, field string) *predicates.ComparisonRange {
	t.Helper()
	operand := &values.FieldValue{Field: field, Typ: values.UnknownType, Child: values.NewQuantifiedObjectValue(outerAlias)}
	cmp := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: operand}
	res := predicates.EmptyComparisonRange().Merge(&cmp)
	if !res.Ok {
		t.Fatalf("failed to build correlated eq range")
	}
	return res.Range
}

func fkChainFullScan(rt string) plans.RecordQueryPlan {
	return plans.NewRecordQueryScanPlan([]string{rt}, values.UnknownType, false).WithPrimaryKey(fkChainIDPK())
}

// fkChainPKProbe is a primary-key equality point probe against rt, correlated
// to outerAlias.<bindField> — the FK column name on the OUTER's own table
// (e.g. T4.T3_ID when probing T3 from an outer driven by T4).
func fkChainPKProbe(t *testing.T, rt string, outerAlias values.CorrelationIdentifier, bindField string) plans.RecordQueryPlan {
	t.Helper()
	return plans.NewRecordQueryScanPlan([]string{rt}, values.UnknownType, false).
		WithPrimaryKey(fkChainIDPK()).
		WithScanComparisons([]*predicates.ComparisonRange{fkChainCorrelatedEq(t, outerAlias, bindField)})
}

// fkChainFKProbe is a non-unique secondary-index equality probe against rt,
// correlated to outerAlias.ID — the PRECEDING table's own primary key. It
// models a plain SCALAR index (one entry per record — no FAN_OUT field), so
// it stamps distinctRecordsKnown/createsDuplicates=false exactly as
// abstract_data_access_rule.go does in production from the match candidate's
// createsDuplicates() signal — a scalar index never fans out, so
// ProducesDistinctRecords() must read true for the FK-chain cap to fire on
// it, same as production.
func fkChainFKProbe(t *testing.T, rt, idx string, outerAlias values.CorrelationIdentifier) plans.RecordQueryPlan {
	t.Helper()
	return plans.NewRecordQueryIndexPlan(idx,
		[]*predicates.ComparisonRange{fkChainCorrelatedEq(t, outerAlias, "ID")},
		[]string{rt}, values.UnknownType, false).
		WithCommonPrimaryKey(fkChainIDPK()).
		WithDistinctRecordsSignal(false)
}

// fkChainFanOutFKProbe is fkChainFKProbe but models a FAN-OUT index (one
// entry per repeated/nested element, e.g. an index over a repeated field) —
// createsDuplicates=true, so the SAME physical record/PK can be emitted more
// than once per probe. See TestFKChainCardinalityCap_DeclinesOnFanOutIndex.
func fkChainFanOutFKProbe(t *testing.T, rt, idx string, outerAlias values.CorrelationIdentifier) plans.RecordQueryPlan {
	t.Helper()
	return plans.NewRecordQueryIndexPlan(idx,
		[]*predicates.ComparisonRange{fkChainCorrelatedEq(t, outerAlias, "ID")},
		[]string{rt}, values.UnknownType, false).
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
		&values.FieldValue{Field: "ID", Typ: values.UnknownType},
	}
}

// fkChainFKProbeRTPrefixed is fkChainFKProbe but stamps a
// RecordTypeValue-prefixed common primary key — the real shape
// GetCommonPrimaryKeyValues() returns for a production index scan (see
// fkChainRTPrefixedPK).
func fkChainFKProbeRTPrefixed(t *testing.T, rt, idx string, outerAlias values.CorrelationIdentifier) plans.RecordQueryPlan {
	t.Helper()
	return plans.NewRecordQueryIndexPlan(idx,
		[]*predicates.ComparisonRange{fkChainCorrelatedEq(t, outerAlias, "ID")},
		[]string{rt}, values.UnknownType, false).
		WithCommonPrimaryKey(fkChainRTPrefixedPK()).
		WithDistinctRecordsSignal(false)
}

func fkChainFlat(outer, inner plans.RecordQueryPlan, outerAlias values.CorrelationIdentifier) plans.RecordQueryPlan {
	return plans.NewRecordQueryFlatMapPlan(outer, inner,
		outerAlias, values.NamedCorrelationIdentifier("i"),
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("i")), false)
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
	fwd1 := fkChainFlat(fkChainFullScan("T1"), fkChainFKProbe(t, "T2", "t2_by_t1", fkChainAlias(0)), fkChainAlias(0))
	fwd2 := fkChainFlat(fwd1, fkChainFKProbe(t, "T3", "t3_by_t2", fkChainAlias(1)), fkChainAlias(1))
	return fkChainFlat(fwd2, fkChainFKProbe(t, "T4", "t4_by_t3", fkChainAlias(2)), fkChainAlias(2))
}

// buildBackwardChain drives T4 (2000 rows, full scan) -> T3 -> T2 -> T1, each
// hop a PRIMARY-KEY equality point probe — mirroring qBig's
// "SELECT t1.id FROM t4, t3, t2, t1 WHERE ..." plan shape.
func buildBackwardChain(t *testing.T) plans.RecordQueryPlan {
	t.Helper()
	bwd1 := fkChainFlat(fkChainFullScan("T4"), fkChainPKProbe(t, "T3", fkChainAlias(0), "T3_ID"), fkChainAlias(0))
	bwd2 := fkChainFlat(bwd1, fkChainPKProbe(t, "T2", fkChainAlias(1), "T2_ID"), fkChainAlias(1))
	return fkChainFlat(bwd2, fkChainPKProbe(t, "T1", fkChainAlias(2), "T1_ID"), fkChainAlias(2))
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

	fwd1 := fkChainFlat(fkChainFullScan("T1"), fkChainFKProbe(t, "T2", "t2_by_t1", fkChainAlias(0)), fkChainAlias(0))
	fwd2 := fkChainFlat(fwd1, fkChainFKProbe(t, "T3", "t3_by_t2", fkChainAlias(1)), fkChainAlias(1))
	fwd3 := fkChainFlat(fwd2, fkChainFKProbe(t, "T4", "t4_by_t3", fkChainAlias(2)), fkChainAlias(2))

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
	inner := plans.NewRecordQueryIndexPlan("x_by_group",
		[]*predicates.ComparisonRange{fkChainCorrelatedEq(t, fkChainAlias(0), "GROUP")},
		[]string{"X"}, values.UnknownType, false).
		WithCommonPrimaryKey(fkChainIDPK())
	fm := plans.NewRecordQueryFlatMapPlan(outer, inner,
		fkChainAlias(0), values.NamedCorrelationIdentifier("i"),
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("i")), false)

	if cap, ok := fkChainCardinalityCap(fm, properties.MapStatistics{PerType: map[string]float64{"X": 500}}); ok {
		t.Fatalf("cap fired on a non-PK bind (cap=%v) — the disproven-unsound case; must decline (ok=false)", cap)
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
	bareInner := fkChainFKProbe(t, "T2", "t2_by_t1", fkChainAlias(0))
	fetched := plans.NewRecordQueryFetchFromPartialRecordPlan(bareInner, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey)
	wrappedInner := plans.NewRecordQueryTypeFilterPlan([]string{"T2"}, fetched)
	fm := plans.NewRecordQueryFlatMapPlan(outer, wrappedInner,
		fkChainAlias(0), values.NamedCorrelationIdentifier("i"),
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("i")), false)

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
	inner := plans.NewRecordQueryIndexPlan("x_by_group",
		[]*predicates.ComparisonRange{fkChainCorrelatedEq(t, fkChainAlias(0), "GROUP")},
		[]string{"X"}, values.UnknownType, false).
		WithCommonPrimaryKey(fkChainIDPK())
	fm := plans.NewRecordQueryFlatMapPlan(outer, inner,
		fkChainAlias(0), values.NamedCorrelationIdentifier("i"),
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("i")), false)

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

	hop1 := fkChainFlat(fkChainFullScan("T1"), fkChainFKProbeRTPrefixed(t, "T2", "t2_by_t1", fkChainAlias(0)), fkChainAlias(0))
	hop2 := fkChainFlat(hop1, fkChainFKProbeRTPrefixed(t, "T3", "t3_by_t2", fkChainAlias(1)), fkChainAlias(1))

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

// TestFKChainCappedInnerCost_DerivationProperty pins fkChainCappedInnerCost's
// derivation as a PROPERTY over a table of (outerCard, innerCard, innerCPU,
// cap) tuples, not one hand-picked example: whenever the cap actually binds
// (cap < outerCard*innerCard), the derived Cost must (1) make
// outerCard*corrected.Cardinality land exactly on cap — reproducing the same
// clamp fkChainCardinalityCap always guaranteed — and (2) scale CPU by the
// IDENTICAL ratio applied to Cardinality (corrected.CPU/inner.CPU ==
// corrected.Cardinality/inner.Cardinality), because scanLikeCost's CPU is an
// exactly linear, zero-intercept function of Cardinality (see
// fkChainCappedInnerCost's doc comment) — any OTHER ratio would silently
// reintroduce a cardinality/CPU mismatch under a different derivation.
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
		name                 string
		outerCard, innerCard float64
		innerCPU, cap        float64
		wantApplied          bool
	}{
		{"binding, matches production T4 hop", 40, 200, 278.1, 2000, true},
		{"binding, small numbers", 5, 100, 50, 60, true},
		{"binding, cap much smaller than uncapped total", 1000, 1000, 1000, 1, true},
		{"not binding, cap exactly equals uncapped total", 10, 20, 5, 200, false},
		{"not binding, cap exceeds uncapped total", 10, 20, 5, 10000, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			outer := properties.Cost{Cardinality: c.outerCard}
			inner := properties.Cost{Cardinality: c.innerCard, CPU: c.innerCPU}
			corrected, applied := fkChainCappedInnerCost(outer, inner, c.cap)
			if applied != c.wantApplied {
				t.Fatalf("applied = %v, want %v", applied, c.wantApplied)
			}
			if !applied {
				return
			}
			if got := c.outerCard * corrected.Cardinality; !closeEnough(got, c.cap) {
				t.Errorf("outerCard*corrected.Cardinality = %v, want cap = %v", got, c.cap)
			}
			wantRatio := corrected.Cardinality / c.innerCard
			gotRatio := corrected.CPU / c.innerCPU
			if !closeEnough(gotRatio, wantRatio) {
				t.Errorf("CPU ratio %v != Cardinality ratio %v — CPU and Cardinality no longer describe the same execution", gotRatio, wantRatio)
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

	fwd1 := fkChainFlat(fkChainFullScan("T1"), fkChainFKProbe(t, "T2", "t2_by_t1", fkChainAlias(0)), fkChainAlias(0))
	fwd2 := fkChainFlat(fwd1, fkChainFKProbe(t, "T3", "t3_by_t2", fkChainAlias(1)), fkChainAlias(1))
	fwd3 := fkChainFlat(fwd2, fkChainFKProbe(t, "T4", "t4_by_t3", fkChainAlias(2)), fkChainAlias(2))

	c2 := concretePlanCost(fwd2, stats, nil)
	t4Probe := fkChainFKProbe(t, "T4", "t4_by_t3", fkChainAlias(2))
	t4Cost := concretePlanCost(t4Probe, stats, nil)
	cap, ok := fkChainCardinalityCap(fwd3.(*plans.RecordQueryFlatMapPlan), stats)
	if !ok {
		t.Fatalf("cap did not fire on hop3 — precondition of this test is broken")
	}
	corrected, applied := fkChainCappedInnerCost(c2, t4Cost, cap)
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
		inner := plans.NewRecordQueryIndexPlan("x_by_group",
			[]*predicates.ComparisonRange{fkChainCorrelatedEq(t, fkChainAlias(0), "GROUP")},
			[]string{"X"}, values.UnknownType, false).
			WithCommonPrimaryKey(fkChainIDPK())
		fm := plans.NewRecordQueryFlatMapPlan(outer, inner,
			fkChainAlias(0), values.NamedCorrelationIdentifier("i"),
			values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("i")), false)
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
			return fkChainFanOutFKProbe(t, "T2", "t2_by_t1_fanout", fkChainAlias(0))
		}},
		{"distinct-records signal never stamped (Java's empty-candidate default)", func(t *testing.T) plans.RecordQueryPlan {
			return plans.NewRecordQueryIndexPlan("t2_by_t1_unstamped",
				[]*predicates.ComparisonRange{fkChainCorrelatedEq(t, fkChainAlias(0), "ID")},
				[]string{"T2"}, values.UnknownType, false).
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

			hop2 := fkChainFlat(hop1, fkChainFKProbe(t, "T3", "t3_by_t2", fkChainAlias(1)), fkChainAlias(1))
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

	hop1 := fkChainFlat(fkChainFullScan("T1"), fkChainFKProbe(t, "T2", "t2_by_t1", fkChainAlias(0)), fkChainAlias(0))

	// A schema-legal projection that overwrites the tracked "ID" output with a
	// constant — the output column is still named "ID", but every row now
	// carries the SAME value, breaking the underlying distinctness the name
	// alone cannot reveal.
	brokenProjection := plans.NewRecordQueryProjectionPlanWithAliases(
		[]values.Value{&values.ConstantValue{Value: int64(42), Typ: values.UnknownType}},
		[]string{"ID"},
		hop1,
	)

	if computePKThread(brokenProjection).ok {
		t.Fatalf("computePKThread(brokenProjection) should be ok=false: the projection " +
			"replaces the tracked PK field with a constant, so \"ID\" no longer distinguishes T2 rows")
	}

	hop2 := fkChainFlat(brokenProjection, fkChainFKProbe(t, "T3", "t3_by_t2", fkChainAlias(1)), fkChainAlias(1))
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
	hop1 := plans.NewRecordQueryFlatMapPlan(
		fkChainFullScan("T1"), fkChainFKProbe(t, "T2", "t2_by_t1", fkChainAlias(0)),
		fkChainAlias(0), values.NamedCorrelationIdentifier("i"),
		&values.ConstantValue{Value: int64(7), Typ: values.UnknownType}, false,
	)

	if computePKThread(hop1).ok {
		t.Fatalf("computePKThread(hop1) should be ok=false: hop1's OWN resultValue is a " +
			"constant, not the inner row it probed — the FlatMap's actual output does not " +
			"carry T2's tracked PK at all")
	}

	hop2 := fkChainFlat(hop1, fkChainFKProbe(t, "T3", "t3_by_t2", fkChainAlias(1)), fkChainAlias(1))
	if cap, ok := fkChainCardinalityCap(hop2.(*plans.RecordQueryFlatMapPlan), stats); ok {
		t.Fatalf("cap fired (cap=%v) on hop2 whose outer thread is hop1, whose OWN resultValue "+
			"drops the tracked PK — must decline: hop1's output no longer identifies distinct "+
			"T2 rows, so T3 probes off it can legitimately exceed T3's table size (200)", cap)
	}
}

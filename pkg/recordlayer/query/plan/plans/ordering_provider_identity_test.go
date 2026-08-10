package plans

// The ordering PROVIDERS state their keys as column IDENTITIES, not display
// names.
//
// Java's `Ordering` is a `PartiallyOrderedSet<Value>` and a
// `SetMultimap<Value, Binding>` keyed by `Value.equals` under the EMPTY alias
// map (Ordering.java:176-183, :336) — a rendered name is never consulted, and a
// provided key that could not be stated as a Value would simply not be in the
// set. Go's providers hold a metadata column-name LIST (an index's key columns,
// a scan's primary key), so a name has to be resolved exactly once, against the
// layout the plan flows, and it dies there.
//
// These tests pin that resolution at the four provider bodies that own it, on
// the two axes a name-keyed provider cannot express:
//
//   - the ORDINAL: which slot of the flowed row the key addresses;
//   - the DOMAIN: which layout that ordinal indexes, so an ordinal from one
//     layout can never answer for the same integer in another.
//
// A provider that hands out a bare name passes neither: OrderingIdentityOf
// declines on a lazy node, so an identity-keyed consumer cannot address the key
// at all and the ordering claim is dropped.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// providerLayout is the flowed row every test below resolves against: four
// columns, so an ordinal is genuinely discriminating (a two-column layout makes
// too many off-by-one slips test equal).
func providerLayout() *values.RecordType {
	return values.NewRecordType("SCORES", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "PLAYER", FieldType: values.NullableString, Ordinal: 1},
		{Name: "GAME", FieldType: values.NullableString, Ordinal: 2},
		{Name: "SCORE", FieldType: values.NullableLong, Ordinal: 3},
	})
}

// wantIdentity asserts that key carries the identity of column `ordinal` in
// `layout`, and says which of the two elements is missing when it does not.
func wantIdentity(t *testing.T, what string, key values.Value, layout values.Type, ordinal int) {
	t.Helper()
	domain := values.OrdinalDomainOfType(layout)
	if !domain.IsKnown() {
		t.Fatalf("test setup: %v has no layout token", layout)
	}
	ident, ok := values.OrderingIdentityOf(key)
	if !ok {
		t.Fatalf("%s: provided ordering key %q has NO column identity, so no "+
			"identity-keyed consumer can address it -- the provider is still "+
			"handing out a display name. Ordering satisfaction would fall back "+
			"to comparing renderings, which is what a name collision exploits.",
			what, values.ExplainValue(key))
	}
	if ident.Ordinal != ordinal {
		t.Fatalf("%s: provided key %q addresses ordinal %d, want %d",
			what, values.ExplainValue(key), ident.Ordinal, ordinal)
	}
	if ident.Domain != domain {
		t.Fatalf("%s: provided key %q states domain %v, want the flowed "+
			"layout's %v. An ordinal whose domain is wrong is worse than no "+
			"ordinal: it reads as authoritative while addressing another row.",
			what, values.ExplainValue(key), ident.Domain, domain)
	}
}

// TestIndexPlanRichOrderingKeysCarryIdentity pins the index scan's rich
// ordering: every key -- the index's own columns AND the trimmed primary-key
// suffix that continues them -- is addressed by its slot in the flowed row.
//
// The index here is (GAME, SCORE) over PK (ID), so the provided ordering is
// GAME, SCORE, ID at flowed ordinals 2, 3, 0. Those three integers are all
// different from the three KEY POSITIONS (0, 1, 2), which is the point: a
// provider that numbered its keys against its own key list instead of the
// flowed row would satisfy a positional assertion and fail this one.
func TestIndexPlanRichOrderingKeysCarryIdentity(t *testing.T) {
	t.Parallel()

	layout := providerLayout()
	plan := NewRecordQueryIndexPlan("idx", nil, []string{"SCORES"}, layout, false).
		WithKeyComponentTypes([]values.Type{values.NullableString, values.NullableLong}).
		WithIndexMetadata([]string{"GAME", "SCORE"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong})

	ordering := plan.HintRichOrdering()
	keys := ordering.GetKeys()
	if len(keys) != 3 {
		t.Fatalf("index rich ordering keys = %d, want 3 (GAME, SCORE, ID)", len(keys))
	}
	for i, want := range []int{2, 3, 0} {
		wantIdentity(t, "index rich ordering", keys[i], layout, want)
	}
}

// TestIndexPlanPlainOrderingKeysCarryIdentity pins the same for the PLAIN
// ordering derivation, which is a SEPARATE body over the same column list.
//
// Both are pinned because they feed different consumers -- the rich one reaches
// RichOrdering.Satisfies, the plain one reaches plan partitioning -- and a
// conversion that taught only one of them would leave the other comparing
// names while every test that looked at the rich side went green.
func TestIndexPlanPlainOrderingKeysCarryIdentity(t *testing.T) {
	t.Parallel()

	layout := providerLayout()
	plan := NewRecordQueryIndexPlan("idx", nil, []string{"SCORES"}, layout, false).
		WithKeyComponentTypes([]values.Type{values.NullableString, values.NullableLong}).
		WithIndexMetadata([]string{"GAME", "SCORE"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong})

	ordering := plan.HintOrdering()
	if !ordering.IsKnown || len(ordering.Keys) != 3 {
		t.Fatalf("index plain ordering = %+v, want 3 known keys", ordering)
	}
	for i, want := range []int{2, 3, 0} {
		wantIdentity(t, "index plain ordering", ordering.Keys[i], layout, want)
	}
}

// TestPKScanOrderingKeysCarryIdentity pins the primary-scan providers, both
// derivations, over a PK whose columns are NOT the leading columns of the
// flowed row -- so a provider that reported the PK's own position would pass a
// weaker test and fail this one.
func TestPKScanOrderingKeysCarryIdentity(t *testing.T) {
	t.Parallel()

	layout := providerLayout()
	plan := NewRecordQueryScanPlan([]string{"SCORES"}, layout, false).
		WithPrimaryKey([]values.Value{
			&values.FieldValue{Field: "GAME", Typ: values.NullableString},
			&values.FieldValue{Field: "ID", Typ: values.NotNullLong},
		}).
		WithKeyComponentTypes([]values.Type{values.NullableString, values.NotNullLong})

	plain := PKScanOrdering(plan)
	if !plain.IsKnown || len(plain.Keys) != 2 {
		t.Fatalf("PK scan plain ordering = %+v, want 2 known keys", plain)
	}
	for i, want := range []int{2, 0} {
		wantIdentity(t, "PK scan plain ordering", plain.Keys[i], layout, want)
	}

	rich := plan.HintRichOrdering()
	richKeys := rich.GetKeys()
	if len(richKeys) != 2 {
		t.Fatalf("PK scan rich ordering keys = %d, want 2", len(richKeys))
	}
	for i, want := range []int{2, 0} {
		wantIdentity(t, "PK scan rich ordering", richKeys[i], layout, want)
	}
}

// TestEqualityBoundPrefixKeysAlsoCarryIdentity pins the EQUALITY-BOUND arm of
// the rich derivation, which builds its keys in a different loop iteration than
// the sorted suffix.
//
// The distinction matters to a consumer: ImplementSortRule discounts a
// requested part that names an equality-bound key, and it can only do that if
// the fixed key is addressable by the same identity as a sorted one. A
// conversion that taught the sorted branch and not the fixed one would leave
// that discount keyed on a name.
func TestEqualityBoundPrefixKeysAlsoCarryIdentity(t *testing.T) {
	t.Parallel()

	layout := providerLayout()
	cmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(7))
	eq := predicates.EmptyComparisonRange().Merge(&cmp)
	if !eq.Ok {
		t.Fatalf("test setup: could not build an equality comparison range")
	}
	plan := NewRecordQueryIndexPlan(
		"idx", []*predicates.ComparisonRange{eq.Range},
		[]string{"SCORES"}, layout, false).
		WithKeyComponentTypes([]values.Type{values.NullableString, values.NullableLong}).
		WithIndexMetadata([]string{"GAME", "SCORE"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong})

	ordering := plan.HintRichOrdering()
	keys := ordering.GetKeys()
	if len(keys) != 3 {
		t.Fatalf("index rich ordering keys = %d, want 3 (GAME fixed, SCORE, ID)", len(keys))
	}
	eqBound := ordering.GetEqualityBoundValues()
	if len(eqBound) != 1 {
		t.Fatalf("equality-bound values = %d, want 1 (GAME)", len(eqBound))
	}
	for v := range eqBound {
		wantIdentity(t, "index equality-bound key", v, layout, 2)
	}
}

// TestUnresolvableProviderKeyStaysLazyRatherThanGuessing pins the fail-closed
// direction, and it is as load-bearing as the positive cases.
//
// A multi-record-type index flows a degraded row type with no single declared
// column order. There is no layout to resolve against, so the provider must
// hand out a key with NO identity -- an unaddressable key, which costs a sort --
// rather than an ordinal against a layout it cannot name. Minting a domain here
// to make the token non-zero would be the ordinal conflation the token exists
// to prevent, wearing a proof's clothes.
func TestUnresolvableProviderKeyStaysLazyRatherThanGuessing(t *testing.T) {
	t.Parallel()

	plan := NewRecordQueryIndexPlan("idx", nil, []string{"A", "B"}, values.UnknownType, false).
		WithKeyComponentTypes([]values.Type{values.NullableString}).
		WithIndexMetadata([]string{"GAME"}, nil, false)

	ordering := plan.HintRichOrdering()
	keys := ordering.GetKeys()
	if len(keys) != 1 {
		t.Fatalf("index rich ordering keys = %d, want 1", len(keys))
	}
	if ident, ok := values.OrderingIdentityOf(keys[0]); ok {
		t.Fatalf("provided key %q claims identity %+v against a layout with no "+
			"declared column order. An ordinal nothing can verify reads as "+
			"authoritative and is strictly worse than no ordinal at all.",
			values.ExplainValue(keys[0]), ident)
	}
}

// TestAggregateProvidersStateTheirOutputLayout pins the two aggregate ordering
// providers that had no domain at all.
//
// A provided ordering key that carries an ordinal without saying which layout
// the ordinal indexes is a key no consumer may compare: OrderingIdentityOf
// declines an unknown domain, because equal ordinals in different layouts is the
// conflation the token exists to refuse. Both providers below advertise an
// ordering over the row they EMIT, and both can name that row — so both must.
//
// Measured consequence of not stating it: the requested side (which does state
// it) could only ever meet these keys through the ordinal-free NAME rendering.
// That bridge carried 75 of the corpus's key resolutions and every one of them
// was an aggregate group key reaching these two providers; it drops to zero when
// they state their layout, with satisfaction unchanged. So this is a change of
// REPRESENTATION, and the test asserts the representation, not a plan shape.
func TestAggregateProvidersStateTheirOutputLayout(t *testing.T) {
	t.Parallel()

	t.Run("aggregate index scan", func(t *testing.T) {
		t.Parallel()

		plan := NewRecordQueryAggregateIndexPlan(
			NewRecordQueryIndexPlan("IDX_AGG", nil, []string{"ORDERS"}, values.UnknownType, false).
				WithKeyComponentTypes([]values.Type{values.NullableString, values.NullableString}),
			"ORDERS", values.UnknownType, "COUNT",
		).WithGroupColumns([]string{"REGION", "STATUS"}, "")

		ordering := plan.HintOrdering()
		if !ordering.IsKnown || len(ordering.Keys) != 2 {
			t.Fatalf("HintOrdering = %#v, want 2 known keys", ordering)
		}

		// The layout is the row the aggregate-index cursor writes:
		// [groupCols..., FUNC(col)] — wider than the group-key prefix, which is
		// the whole point. A domain derived from the group columns alone is a
		// DIFFERENT token, and the requested side derives its own from the full
		// output row.
		wantDomain := values.OrdinalDomainOfColumnNames(plan.OutputColumnNames())
		if !wantDomain.IsKnown() {
			t.Fatal("test setup: the output row must be nameable")
		}
		if prefixOnly := values.OrdinalDomainOfColumnNames(
			[]string{"REGION", "STATUS"}); prefixOnly == wantDomain {
			t.Fatal("test setup: the group-key prefix and the full output row " +
				"must yield DIFFERENT tokens, or the layout choice is untested")
		}

		for i, key := range ordering.Keys {
			ident, ok := values.OrderingIdentityOf(key)
			if !ok {
				t.Fatalf("group key %d (%q) states no identity. An aggregate "+
					"index's group column i IS slot i of the row it emits; a key "+
					"that does not say so can only be matched by its spelling.",
					i, values.ExplainValue(key))
			}
			if ident.Ordinal != i {
				t.Fatalf("group key %d states ordinal %d", i, ident.Ordinal)
			}
			if ident.Domain != wantDomain {
				t.Fatalf("group key %d states domain %v, want the OUTPUT row %v.\n\n"+
					"The requested side of an ORDER BY over this aggregate is "+
					"baked against the output row (GroupByOutputColumnNames is "+
					"the single authority for it). A key domained in anything "+
					"else — the group-key prefix, the underlying index's key "+
					"layout — renders identically and compares unequal.",
					i, ident.Domain, wantDomain)
			}
		}
	})

	t.Run("multi aggregate index intersection", func(t *testing.T) {
		t.Parallel()

		// The child rows are [REGION, SUM(V)] and [REGION, COUNT(*)]; the row
		// the PLAN emits is [REGION, SUM(V), COUNT(*)]. The comparison key is
		// child-relative by construction (the merge cursor evaluates it against
		// each stream), and the ordering the plan ADVERTISES is over its own
		// output — two different layouts that agree on the grouping ordinals.
		comparisonKey := []values.Value{
			values.NewFieldValueWithResolvedOrdinal("REGION", 0, values.UnknownType),
		}
		resultValue := values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "REGION", Value: comparisonKey[0]},
			values.RecordConstructorField{
				Name:  "SUM(V)",
				Value: values.NewFieldValueWithResolvedOrdinal("SUM(V)", 1, values.UnknownType),
			},
			values.RecordConstructorField{
				Name:  "COUNT(*)",
				Value: values.NewFieldValueWithResolvedOrdinal("COUNT(*)", 3, values.UnknownType),
			},
		)
		plan := NewRecordQueryMultiIntersectionOnValuesPlanFromQuantifiers(
			nil, comparisonKey, resultValue)
		if plan == nil {
			t.Fatal("test setup: plan construction declined")
		}

		ordering := plan.HintOrdering()
		if !ordering.IsKnown || len(ordering.Keys) != 1 {
			t.Fatalf("HintOrdering = %#v, want 1 known key", ordering)
		}
		ident, ok := values.OrderingIdentityOf(ordering.Keys[0])
		if !ok {
			t.Fatalf("the advertised key (%q) states no identity; the raw "+
				"comparison key carries an ordinal with no layout, and handing "+
				"it out as the advertised ordering makes it unaddressable",
				values.ExplainValue(ordering.Keys[0]))
		}
		wantDomain := values.OrdinalDomainOfColumnNames(
			[]string{"REGION", "SUM(V)", "COUNT(*)"})
		if ident.Domain != wantDomain {
			t.Fatalf("advertised key states domain %v, want the plan's OUTPUT "+
				"row %v.\n\n"+
				"The comparison key is CHILD-row relative; the ordering this "+
				"plan advertises describes the rows it EMITS, and only that row "+
				"is the one a requested ORDER BY key is baked against.",
				ident.Domain, wantDomain)
		}
		if ident.Ordinal != 0 {
			t.Fatalf("advertised key states ordinal %d, want 0 — the grouping "+
				"ordinals agree between the two layouts and the restatement must "+
				"preserve them", ident.Ordinal)
		}
	})
}

// TestInMemorySortHintOrderingStatesTheKeyValueNotItsRendering pins the fifth
// provider on the axis the four above do not reach: an in-memory sort does not
// resolve a metadata NAME at all — it is handed the key Value the executor will
// evaluate, and it used to throw that Value away and re-mint a lazy FieldValue
// from SortKey.Field.
//
// SortKey.Field is DISPLAY text. For anything but a bare column it is
// ExplainValue's rendering, correlation and `#ordinal` included ("q$7.AID#0"),
// which is a string no schema can produce. Storing it as a lazy Field made the
// sort advertise its ordering in terms of a RENDERING, and the match-domain
// identity has to decline a flat-dotted lazy name outright — `addr.city` (a
// nested path) and `Q.city` (an alias-qualified leaf) are the same string. The
// decline is safe but it is not free: it is read as "this ordering does not
// satisfy that grouping key", so streaming aggregation over an ordered input
// falls back to a sort it did not need.
func TestInMemorySortHintOrderingStatesTheKeyValueNotItsRendering(t *testing.T) {
	t.Parallel()

	// A CORRELATED leg reference — the shape whose SortKey.Field is the full
	// explain rendering rather than a bare column name.
	key := &values.FieldValue{
		Field:    "AID",
		Typ:      values.NullableLong,
		Child:    values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("q$7")),
		Resolved: values.NewFieldPathOfSingle("AID", 0, false),
	}
	rendered := values.ExplainValue(key)

	// CONTROL, so this test cannot pass for the wrong reason. Two facts have to
	// hold before the assertion below means anything: the rendering really is
	// the flat-dotted shape, and a lazy FieldValue carrying it really is
	// declined by the match-domain identity. If either stops being true the
	// defect is no longer expressible and this test would go green vacuously.
	if !strings.Contains(rendered, ".") || !strings.Contains(rendered, "#") {
		t.Fatalf("control: ExplainValue of a correlated key rendered %q, which "+
			"is not the dotted `#ordinal` shape this test is about — the "+
			"renderer changed and this pin no longer expresses the defect",
			rendered)
	}
	if _, ok := values.AccessorNamePath(&values.FieldValue{Field: rendered, Typ: values.UnknownType}); ok {
		t.Fatalf("control: AccessorNamePath ACCEPTED a lazy field carrying the "+
			"rendering %q. The decline this test exists to avoid is gone, so a "+
			"pass below would prove nothing", rendered)
	}
	// CONTROL: the key itself must be addressable, otherwise the assertion
	// below is satisfied by a value that is merely a different kind of broken.
	if _, ok := values.AccessorNamePath(key); !ok {
		t.Fatalf("control: AccessorNamePath declined the baked key itself, so " +
			"there is no identity for HintOrdering to state")
	}

	plan := &RecordQueryInMemorySortPlan{sortKeys: []SortKey{
		{Field: rendered, Desc: false, NullsFirst: true, ValueExpr: key},
	}}

	ordering := plan.HintOrdering()
	if !ordering.IsKnown || len(ordering.Keys) != 1 {
		t.Fatalf("HintOrdering = %#v, want 1 known key", ordering)
	}
	path, ok := values.AccessorNamePath(ordering.Keys[0])
	if !ok {
		t.Fatalf("the advertised sort key states no accessor path. "+
			"HintOrdering re-minted a lazy FieldValue from the DISPLAY string "+
			"%q instead of handing out the key Value it was given, so the "+
			"ordering is advertised as a rendering and every identity consumer "+
			"declines it", rendered)
	}
	if len(path) != 1 || path[0] != "AID" {
		t.Fatalf("advertised sort key states accessor path %v, want [AID] — "+
			"the correlation root is excluded from the path by construction",
			path)
	}
	if ordering.Keys[0] != values.Value(key) {
		t.Fatalf("advertised sort key is a COPY (%q), not the key Value the "+
			"executor evaluates. A restated key is a second identity for one "+
			"column, which is the conflation the ordinal exists to prevent",
			values.ExplainValue(ordering.Keys[0]))
	}
}

// TestInMemorySortHintOrderingIsUnknownWhenUnbaked pins the OTHER direction of
// the same change, and it replaces a test that pinned the OPPOSITE contract.
//
// The deleted test (TestInMemorySortHintOrderingKeepsDisplayNameWhenUnbaked)
// asserted that a SortKey with no ValueExpr still advertises a lazy FieldValue
// built from its display name. That made the ADVERTISER more permissive than the
// EXECUTOR of the same struct: in_memory_sort.go declares ValueExpr REQUIRED and
// the cursor rejects a nil one as a malformed plan (TestSortCursor_
// UnbakedKeyIsLoud), so the advertised ordering was a claim about a plan that
// cannot run. It also entrenched nothing testable — with a nil ValueExpr both the
// old always-lazy code and the fixed code take the same branch, so it stayed
// green under the mutation that reverts the fix while making the dead arm look
// covered.
//
// The contract now: an unbaked key yields an UNKNOWN ordering. Never a silent
// name mint — a rendered display string re-entered as an identity is declined by
// AccessorNamePath, so an ordering advertised that way is not comparable with a
// baked one, and two producer-dependent vocabularies is what makes satisfaction
// unreliable.
func TestInMemorySortHintOrderingIsUnknownWhenUnbaked(t *testing.T) {
	t.Parallel()

	plan := &RecordQueryInMemorySortPlan{sortKeys: []SortKey{
		{Field: "SCORE", Desc: true, NullsFirst: false},
	}}
	ordering := plan.HintOrdering()
	if ordering.IsKnown || len(ordering.Keys) != 0 {
		t.Fatalf("HintOrdering = %#v, want an UNKNOWN ordering for a nil ValueExpr. "+
			"Advertising anything here claims an order for a plan the sort cursor "+
			"rejects as malformed, and a key minted from the display string %q is a "+
			"rendering re-entered as an identity", ordering, "SCORE")
	}

	// A key that IS baked still advertises, so the assertion above is the nil
	// arm and not the whole function going dark.
	baked := values.NewFlatFieldValue("SCORE", values.UnknownType)
	ok := (&RecordQueryInMemorySortPlan{sortKeys: []SortKey{
		{Field: "SCORE", Desc: true, NullsFirst: false, ValueExpr: baked},
	}}).HintOrdering()
	if !ok.IsKnown || len(ok.Keys) != 1 || ok.Keys[0] != values.Value(baked) {
		t.Fatalf("control: a BAKED key advertised %#v, want the key Value itself. "+
			"Without this, the assertion above is satisfied by HintOrdering never "+
			"advertising anything at all", ok)
	}
	if !ok.Descending[0] || ok.NullsFirst[0] {
		t.Fatalf("control: direction lost on the baked key: Descending=%v NullsFirst=%v, want true/false",
			ok.Descending[0], ok.NullsFirst[0])
	}
}

// TestInMemorySortHintOrderingMintsNothingWhenUnbaked is the census-side half of
// the pin above: the nil arm must not record a lazy mint either.
//
// This is the fact the whole-corpus number rests on. The mint census counted
// 21,865 lazy-EXPLAIN-RENDERED mints from this one line; the corpus gate asserts
// that class is now 0. A silent re-mint here would put it straight back, and the
// ordering assertion above cannot see a mint that is recorded and then discarded.
func TestInMemorySortHintOrderingMintsNothingWhenUnbaked(t *testing.T) {
	// Not parallel: it owns the process census counters and the census gate flag
	// for its duration.
	was := values.LegIdentityCensusEnabled()
	values.SetLegIdentityCensusEnabled(true)
	values.ResetFieldValueMintCensus()
	t.Cleanup(func() {
		values.ResetFieldValueMintCensus()
		values.SetLegIdentityCensusEnabled(was)
	})

	// The exact shape that broke: a rendered Explain label in Field — dot and
	// `#ordinal` — which is what ClassifyFieldMint buckets as lazy-EXPLAIN-RENDERED.
	const rendered = "q$50765.AID#0"
	if got := values.ClassifyFieldMint(rendered, false); got != values.FieldMintLazyExplainRendered {
		t.Fatalf("control: %q classifies as %v, not lazy-EXPLAIN-RENDERED — the fixture no "+
			"longer expresses the class this test is about", rendered, got)
	}

	plan := &RecordQueryInMemorySortPlan{sortKeys: []SortKey{{Field: rendered}}}
	_ = plan.HintOrdering()

	total, counts := values.FieldValueMintCensus()
	if counts[values.FieldMintLazyExplainRendered] != 0 {
		t.Fatalf("HintOrdering minted %d lazy-EXPLAIN-RENDERED FieldValue(s) from a display "+
			"string; the corpus gate's ceiling of 0 on that class exists because this line "+
			"was its only producer", counts[values.FieldMintLazyExplainRendered])
	}
	if total != 0 {
		t.Fatalf("HintOrdering minted %d FieldValue(s) for an unbaked key, want none — "+
			"any mint here is a display rendering re-entered as an identity", total)
	}
}

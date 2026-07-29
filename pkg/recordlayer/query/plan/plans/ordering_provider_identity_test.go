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
		WithIndexMetadata([]string{"GAME", "SCORE"}, []string{"ID"}, false)

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
		WithIndexMetadata([]string{"GAME", "SCORE"}, []string{"ID"}, false)

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
		})

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
		WithIndexMetadata([]string{"GAME", "SCORE"}, []string{"ID"}, false)

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

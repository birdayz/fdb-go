package plans

// Completeness — "these coordinates are the WHOLE storage key" — is stamped by
// the ordering derivation, because that derivation is the site that decides how
// many coordinates to advertise and is therefore the only site that knows
// whether it dropped one.
//
// It used to be re-derived by the consumer from the plan's key component types.
// That is a second property, and two properties kept in agreement by hand
// disagree eventually. They disagree here: an expression-key index advertises no
// ordering at all, while its key component types are perfectly ordinary — so the
// type-only derivation called it complete, and the consumer that ANDs
// completeness with record-distinctness would then mark the plan strictly
// sorted on the strength of an ordering that does not exist.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func completenessRow() values.Type {
	return values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "A", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "D", FieldType: values.NullableDouble, Ordinal: 2},
	})
}

// completenessIndexPlan builds a forward index scan on one key column of the
// given type over PK (ID), with no scan bounds.
func completenessIndexPlan(t testing.TB, keyColumn string, keyType values.Type) *RecordQueryIndexPlan {
	t.Helper()
	return mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan(
			"IDX_"+keyColumn,
			[]*predicates.ComparisonRange{},
			[]string{"T"}, completenessRow(), false /* forward */)
	}).
		WithKeyComponentTypes([]values.Type{keyType}).
		WithIndexMetadata([]string{keyColumn}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes([]values.Type{values.NullableLong})
}

// TestStorageKeyCompleteness_UntruncatedIndexIsComplete is the control: an
// index whose key and primary-key suffix are both ordinary types advertises
// every coordinate, so the ordering IS the whole storage key.
func TestStorageKeyCompleteness_UntruncatedIndexIsComplete(t *testing.T) {
	t.Parallel()

	ordering := completenessIndexPlan(t, "A", values.NullableLong).HintRichOrdering()
	if got := len(ordering.GetKeys()); got != 2 {
		t.Fatalf("ordering has %d coordinates, want 2 (A, ID)", got)
	}
	if !ordering.StorageKeyIsComplete() {
		t.Fatal("an index advertising its full key + PK suffix must be storage-complete")
	}
}

// TestStorageKeyCompleteness_FloatTruncationIsIncomplete: the claim TERMINATES
// at a FLOAT coordinate, so the primary-key suffix behind it never reaches the
// advertised ordering. What is advertised is a strict prefix of the storage key,
// and a prefix does not make the key unique.
//
// The truncation and the key-component-type check REFUSE THIS SHAPE
// INDEPENDENTLY, which the assertions below state separately so the redundancy
// is visible rather than assumed. TestStorageKeyCompleteness_TruncationIsOverdetermined
// is the sentinel for the day they stop agreeing.
func TestStorageKeyCompleteness_FloatTruncationIsIncomplete(t *testing.T) {
	t.Parallel()

	plan := completenessIndexPlan(t, "D", values.NullableDouble)
	ordering := plan.HintRichOrdering()
	if got := len(ordering.GetKeys()); got != 0 {
		t.Fatalf("the claim must terminate AT the float, advertising no coordinate; got %d", got)
	}
	if ordering.StorageKeyIsComplete() {
		t.Fatal("an ordering truncated at a FLOAT coordinate is not the whole storage key")
	}
}

// TestStorageKeyCompleteness_TruncationIsOverdetermined records a MEASURED
// negative result, because a claim that a guard protects something is worth
// exactly as much as the evidence that it can fire.
//
// The producer refuses completeness for two independent reasons: the ordering
// derivation truncated a coordinate away, or the key component types say
// physical uniqueness is not logical uniqueness. On every index this package can
// construct, the second subsumes the first — truncation is driven by the FLOWED
// LAYOUT's column type and the type check by the KEY COMPONENT types, and for a
// real index those two name the same column's type. So no reachable shape
// isolates the truncation clause, and no test here can make it load-bearing.
//
// It is kept anyway: it is the fact the truncating site knows, stated where it
// is known, and the alternative is a producer whose answer silently depends on
// another source agreeing with it. But it is DEFENCE IN DEPTH, not a pinned
// guard, and this test fails the moment that stops being true — at which point
// the clause has become load-bearing and owes its own reproducer.
func TestStorageKeyCompleteness_TruncationIsOverdetermined(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		column  string
		keyType values.Type
	}{
		{"long_key", "A", values.NullableLong},
		{"double_key", "D", values.NullableDouble},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := completenessIndexPlan(t, tc.column, tc.keyType)
			typesAgree := properties.TupleKeyUniquenessMatchesLogicalEquality(
				plan.GetKeyComponentTypes(), len(plan.GetColumnNames())) &&
				properties.TupleKeyUniquenessMatchesLogicalEquality(
					plan.GetPrimaryKeyComponentTypes(), len(plan.GetPKColumnNames()))
			untruncated := len(plan.HintRichOrdering().GetKeys()) ==
				len(plan.GetColumnNames())+len(plan.GetPKColumnNames())
			if typesAgree != untruncated {
				t.Fatalf("truncation (%v) and the key-type check (%v) have DIVERGED. "+
					"The truncation clause in HintRichOrdering is now load-bearing on "+
					"its own and needs a mutation-proven pin of its own.",
					untruncated, typesAgree)
			}
		})
	}
}

// TestStorageKeyCompleteness_ExpressionKeyIndexIsIncomplete is the shape the
// two properties disagreed on. An expression-key index cannot synthesize
// ordering Values from its physical column names, so it advertises NOTHING —
// while its key component types are LONG and LONG and say nothing is wrong.
//
// A consumer reading the types alone concludes "complete" and, ANDed with
// record distinctness, marks the plan strictly sorted. There is no ordering for
// that claim to be about.
func TestStorageKeyCompleteness_ExpressionKeyIndexIsIncomplete(t *testing.T) {
	t.Parallel()

	plan := completenessIndexPlan(t, "A", values.NullableLong).
		WithOrderingKeyNamesUnavailable()

	// The type-only question — what the consumer used to ask — still answers
	// yes on both halves of this plan's key. It is not a wrong predicate; it is
	// the wrong question.
	if !properties.TupleKeyUniquenessMatchesLogicalEquality(
		plan.GetKeyComponentTypes(), len(plan.GetColumnNames())) ||
		!properties.TupleKeyUniquenessMatchesLogicalEquality(
			plan.GetPrimaryKeyComponentTypes(), len(plan.GetPKColumnNames())) {
		t.Fatal("this fixture is meant to have types that pass the type-only check; " +
			"if it no longer does, the divergence it pins is no longer expressed")
	}

	ordering := plan.HintRichOrdering()
	if len(ordering.GetKeys()) != 0 {
		t.Fatalf("expression-key index advertised %d coordinates, want none",
			len(ordering.GetKeys()))
	}
	if ordering.StorageKeyIsComplete() {
		t.Fatal("an ordering with NO coordinates cannot be the whole storage key")
	}
}

// TestStorageKeyCompleteness_ZeroCoordinateScanIsIncomplete is the SCAN-side
// half of the shape TestStorageKeyCompleteness_ExpressionKeyIndexIsIncomplete
// pins for index scans, and it is a NEGATIVE RESULT: it records the fact that
// makes an otherwise-live vacuity unreachable.
//
// The vacuity is in CoordinateBoundClaim: holdsOver is a for-all over the
// coordinates a claim was proved on, so a claim proved over NO coordinates holds
// over anything. WithStorageKeyComplete(true) applied to an ordering with no
// keys used to mint exactly that value; it now refuses (pinned in properties'
// TestStorageCompleteness_EmptyOrderingCannotBeComplete) — but the reason it was
// never reachable in the first place is HERE, at the producers, and nothing
// pinned this half:
//
//   - a coordinate list that collapses to empty returns EmptyOrdering() BEFORE
//     the stamp is applied, so the stamping call is never made; and
//   - the storageComplete formula would be false anyway, because `limit ==
//     len(resolved)` cannot hold when limit is 0 and the key is non-empty.
//
// Both are barriers, and this asserts the observable they jointly produce. If
// either is relaxed — a stamp moved above the guard, a formula rewritten to
// treat "nothing truncated" as vacuously satisfied — this goes red, which is
// exactly when the type-level guard stops being defence in depth and becomes
// load-bearing.
func TestStorageKeyCompleteness_ZeroCoordinateScanIsIncomplete(t *testing.T) {
	t.Parallel()

	// A DOUBLE leading primary-key column terminates the ordering claim at
	// position 0, so the scan advertises no coordinates at all.
	plan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, completenessRow(), false)
	}).
		WithPrimaryKey([]values.Value{testFieldAt(t, "D", 2, values.NullableDouble)}).
		WithKeyComponentTypes([]values.Type{values.NullableDouble})

	ordering := plan.HintRichOrdering()
	if got := len(ordering.GetKeys()); got != 0 {
		t.Fatalf("the fixture advertises %d coordinates, want none — a DOUBLE "+
			"leading PK column must terminate the claim at position 0, and if it "+
			"no longer does this test is pinning nothing", got)
	}
	if ordering.StorageKeyIsComplete() {
		t.Fatal("an ordering with NO coordinates reported itself as the WHOLE " +
			"storage key. That is the vacuous claim CoordinateBoundClaim's " +
			"empty-set guard exists to refuse, and reaching it here means a " +
			"producer now stamps completeness onto a coordinate list it emptied")
	}
	if ordering.IsDistinct() {
		t.Fatal("an ordering with NO coordinates reported itself DISTINCT")
	}
}

// TestStorageKeyCompleteness_DoesNotSurvivePrefixing binds completeness the same
// way distinctness is bound: to the coordinate set it was stamped over. A
// projection that carries only the leading coordinate forward leaves an ordering
// that is a strict prefix of the storage key, and it must not inherit the claim
// that the full ordering earned.
func TestStorageKeyCompleteness_DoesNotSurvivePrefixing(t *testing.T) {
	t.Parallel()

	full := completenessIndexPlan(t, "A", values.NullableLong).HintRichOrdering()
	if !full.StorageKeyIsComplete() {
		t.Fatal("precondition: the full (A, ID) ordering is storage-complete")
	}

	keys := full.GetKeys()
	upper := values.NamedCorrelationIdentifier("completeness_prefix")
	prefix, err := full.PullUpThroughValue(
		values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "A", Value: keys[0]},
		),
		upper)
	if err != nil {
		t.Fatalf("PullUpThroughValue: %v", err)
	}
	if prefix == nil {
		t.Fatal("PullUpThroughValue returned nil")
	}
	if got := len(prefix.GetKeys()); got != 1 {
		t.Fatalf("prefix ordering has %d coordinates, want 1", got)
	}
	if prefix.StorageKeyIsComplete() {
		t.Fatal("(A) inherited the (A, ID) completeness claim; the coordinate that " +
			"makes the key unique is exactly the one that was dropped")
	}
}

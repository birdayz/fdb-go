package recordlayer

import (
	"testing"

	"fdb.dev/gen"
)

// A deprecated index-type alias must behave EXACTLY like the type it names, in
// every place Go re-derives behaviour from the type string.
//
// Java never faces this question twice. The type picks a maintainer out of the
// factory registry once (`AtomicMutationIndexMaintainerFactory.TYPES` lists
// MIN_EVER and MAX_EVER right beside MIN_EVER_LONG and MAX_EVER_LONG), and every
// later question — idempotent? valid grouping? can it serve this aggregate? — is
// a method call on the maintainer already chosen. Go flattened that into five
// independent switches, so the alias has to be resolved five times and any one
// of them can be missed.
//
// Testing each switch against its own expected ANSWER is what leaves the gap:
// two of the five (canEvaluateAggregate and the grouping validator) could have
// their canonicalization deleted with the entire package suite green, because
// nothing asked about a bare-spelled index there at all. So the property pinned
// here is the DIFFERENTIAL — bare and canonical must agree — which is the real
// requirement, needs no per-switch expected value, and fails the moment any
// switch stops resolving the alias.
func TestIndexTypeAlias_BehavesIdenticallyToItsCanonicalType(t *testing.T) {
	t.Parallel()

	for _, pair := range []struct{ bare, canonical string }{
		{IndexTypeMinEver, IndexTypeMinEverLong},
		{IndexTypeMaxEver, IndexTypeMaxEverLong},
	} {
		t.Run(pair.bare, func(t *testing.T) {
			t.Parallel()

			mk := func(indexType string, root KeyExpression) *Index {
				idx := NewIndex("Order$alias", root)
				idx.Type = indexType
				return idx
			}
			// The valid single-value atomic-mutation shape: one aggregated column,
			// no grouping columns. The operand matches it structurally, which is
			// what isGroupPrefix accepts.
			grouped := func() KeyExpression { return Ungrouped(Field("price")) }

			// Switch 1 — Index.IsAtomicMutationIndex (index.go).
			if got, want := mk(pair.bare, grouped()).IsAtomicMutationIndex(),
				mk(pair.canonical, grouped()).IsAtomicMutationIndex(); got != want {
				t.Errorf("IsAtomicMutationIndex: %q=%t but %q=%t. A disagreement makes the alias a "+
					"value-scan candidate — an atomic index served by a value scan",
					pair.bare, got, pair.canonical, want)
			}

			// Switch 2 — isIndexIdempotent (online_indexer.go).
			if got, want := isIndexIdempotent(mk(pair.bare, grouped())),
				isIndexIdempotent(mk(pair.canonical, grouped())); got != want {
				t.Errorf("isIndexIdempotent: %q=%t but %q=%t. This selects the read isolation an "+
					"online index build uses", pair.bare, got, pair.canonical, want)
			}

			// Switch 3 — canEvaluateAggregate (aggregate_function.go). Swept over
			// several function names so the differential cannot pass by both
			// sides saying "no" to everything.
			for _, fnName := range []string{
				FunctionNameMinEver, FunctionNameMaxEver,
				FunctionNameMin, FunctionNameMax, FunctionNameSum, FunctionNameCount,
			} {
				fn := &IndexAggregateFunction{Name: fnName, Operand: Ungrouped(Field("price"))}
				got := canEvaluateAggregate(fn, mk(pair.bare, grouped()))
				want := canEvaluateAggregate(fn, mk(pair.canonical, grouped()))
				if got != want {
					t.Errorf("canEvaluateAggregate(%s): %q=%t but %q=%t. A false here means the "+
						"planner cannot find the index for an aggregate Java serves from it",
						fnName, pair.bare, got, pair.canonical, want)
				}
			}
			// The sweep must actually exercise a TRUE somewhere, or it proves
			// nothing but symmetric refusal.
			if !canEvaluateAggregate(
				&IndexAggregateFunction{Name: aggregateFunctionForEverType(pair.canonical), Operand: Ungrouped(Field("price"))},
				mk(pair.canonical, grouped())) {
				t.Fatalf("fixture: %q serves no aggregate at all, so the differential above is vacuous",
					pair.canonical)
			}

			// Switch 4 — the grouping validator in RecordMetaDataBuilder.Build
			// (metadata.go). Exercised on the PROGRAMMATIC path deliberately:
			// the proto path repairs an unwrapped root (Index.java:205-215), so
			// only a hand-built index can still reach the validator unwrapped.
			buildErr := func(indexType string, root KeyExpression) error {
				b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
				b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
				b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
				b.AddIndex("Order", mk(indexType, root))
				_, err := b.Build()
				return err
			}
			// Ungrouped root: Java's validator rejects both spellings, because
			// the registry routes both to the same factory and the same
			// IndexValidator.validateGrouping.
			bareErr := buildErr(pair.bare, Field("price"))
			canonErr := buildErr(pair.canonical, Field("price"))
			if (bareErr == nil) != (canonErr == nil) {
				t.Errorf("grouping validation of an UNGROUPED root: %q err=%v but %q err=%v. "+
					"Java routes both spellings to AtomicMutationIndexMaintainerFactory "+
					"(TYPES lists MIN_EVER/MAX_EVER) and runs one validator for both",
					pair.bare, bareErr, pair.canonical, canonErr)
			}
			if canonErr == nil {
				t.Fatal("fixture: an ungrouped atomic-mutation root was ACCEPTED, so the " +
					"differential above is vacuous — the validator is not firing at all")
			}
			// Grouped root: both must be accepted.
			if err := buildErr(pair.bare, grouped()); err != nil {
				t.Errorf("a correctly grouped %q index was rejected: %v", pair.bare, err)
			}
		})
	}
}

// aggregateFunctionForEverType names the aggregate a _LONG _EVER index serves,
// so the vacuity check above asks for the one function that must succeed.
func aggregateFunctionForEverType(indexType string) string {
	if indexType == IndexTypeMaxEverLong {
		return FunctionNameMaxEver
	}
	return FunctionNameMinEver
}

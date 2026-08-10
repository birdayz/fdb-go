package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RFC-229 step 0: WHICH grouping key a post-aggregate reference is comes from
// the two Values, not from a name both keys render the same way.
//
// These are UNIT pins because the corpus cannot reach the arm. Measured over the
// whole real-FDB //pkg/relational/sqldriver corpus with a probe at all three
// call sites, `groupKeyOrdinalByStructure` is consulted 13 times (4 decided, 9
// declined) and the structural answer NEVER differs from the name map's — and on
// the shape it exists for, two grouping keys sharing a leaf, it is consulted
// ZERO times, because those references arrive frontier-pinned from the binder in
// core/embedded/logical_predicate.go and the baker returns before any name
// lookup. So a corpus run cannot tell this decider from its absence, and a
// green suite is not evidence that it works. Every arm is driven from here.

// keyPath builds the resolved single-accessor path a group key / reference
// carries: `domain` is the layout the ordinal indexes, so two keys of the SAME
// table share it and only the quantifier root separates them.
func keyPath(field string, ordinal int, domain values.OrdinalDomain) *values.FieldPath {
	return values.NewFieldPathOfSingleInDomain(field, ordinal, false, domain)
}

func qualifiedKey(field string, ordinal int, domain values.OrdinalDomain, corr string) *values.FieldValue {
	return &values.FieldValue{
		Field:    field,
		Typ:      values.UnknownType,
		Child:    values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(corr)),
		Resolved: keyPath(field, ordinal, domain),
	}
}

func bareKey(field string, ordinal int, domain values.OrdinalDomain) *values.FieldValue {
	return &values.FieldValue{
		Field:    field,
		Typ:      values.UnknownType,
		Resolved: keyPath(field, ordinal, domain),
	}
}

// TestGroupKeyOrdinalByStructure_Arms drives each decision of the decider
// itself: the two-quantifier separation, the resolved-path separation, the
// ambiguity decline, and the two shapes it cannot speak for.
func TestGroupKeyOrdinalByStructure_Arms(t *testing.T) {
	t.Parallel()

	tbl := values.OrdinalDomainOfColumnNames([]string{"K", "O_K"})
	other := values.OrdinalDomainOfColumnNames([]string{"K"})

	// `GROUP BY o.k, i.k` over ONE table read through two quantifiers: the
	// resolved paths are identical (same layout, same ordinal) and the ROOT is
	// the only thing that tells the keys apart. This is the case the leaf name
	// cannot decide at all — both keys render "K".
	sameTable := []values.Value{
		qualifiedKey("K", 0, tbl, "O"),
		qualifiedKey("K", 0, tbl, "I"),
	}
	for want, corr := range map[int]string{0: "O", 1: "I"} {
		got, ok := groupKeyOrdinalByStructure(qualifiedKey("K", 0, tbl, corr), sameTable)
		if !ok || got != want {
			t.Errorf("two quantifiers over one table: %s.K resolved to (%d,%v), want (%d,true). "+
				"The quantifier root is the only separator here; without it both keys are \"K\".",
				corr, got, ok, want)
		}
	}

	// Two DIFFERENT tables: the resolved paths differ (distinct domains), so
	// SameColumnPath alone separates them even with the roots stripped.
	twoTables := []values.Value{
		bareKey("K", 0, tbl),
		bareKey("K", 0, other),
	}
	for want, dom := range map[int]values.OrdinalDomain{0: tbl, 1: other} {
		got, ok := groupKeyOrdinalByStructure(bareKey("K", 0, dom), twoTables)
		if !ok || got != want {
			t.Errorf("two layouts: resolved to (%d,%v), want (%d,true)", got, ok, want)
		}
	}

	// Same layout, different ordinal — the ordinary two-column GROUP BY.
	twoCols := []values.Value{bareKey("K", 0, tbl), bareKey("O_K", 1, tbl)}
	if got, ok := groupKeyOrdinalByStructure(bareKey("O_K", 1, tbl), twoCols); !ok || got != 1 {
		t.Errorf("two ordinals of one layout: got (%d,%v), want (1,true)", got, ok)
	}

	// DECLINES, all three of them. A declined match costs a rewrite; an
	// accepted wrong one binds the wrong column, so every ambiguity fails
	// closed and the caller's name channel stays in charge.
	declines := []struct {
		name string
		ref  values.Value
		keys []values.Value
	}{
		{
			"ambiguous: two keys are the same column of the same quantifier",
			bareKey("K", 0, tbl),
			[]values.Value{bareKey("K", 0, tbl), bareKey("K", 0, tbl)},
		},
		{
			// The ambiguity guard would decline this anyway if the roots
			// matched, so the ONE key it could bind to is the whole test: a
			// reference that lost its quantifier cannot be attributed to one of
			// several legs, and guessing is what the last-wins map already does.
			"a childless reference is not a wildcard for a qualified key",
			bareKey("K", 0, tbl),
			[]values.Value{qualifiedKey("K", 0, tbl, "O"), bareKey("O_K", 1, tbl)},
		},
		{
			"and not the other way round either",
			qualifiedKey("K", 0, tbl, "O"),
			[]values.Value{bareKey("K", 0, tbl), bareKey("O_K", 1, tbl)},
		},
		{
			"a LAZY reference states no path at all",
			&values.FieldValue{Field: "K", Typ: values.UnknownType},
			twoCols,
		},
	}
	for _, d := range declines {
		if got, ok := groupKeyOrdinalByStructure(d.ref, d.keys); ok {
			t.Errorf("%s: resolved to %d, want a decline", d.name, got)
		}
	}
}

// TestGroupByOutputBaker_StructureDecidesWhichSameLeafKey drives the two arms of
// groupByOutputBaker that consult the decider, with the name map in the LAST-WINS
// state that makes the name unable to answer: `GROUP BY o.k, i.k` renders both
// keys "K" through AggregateKeyColumnName, so keys["K"] holds the SECOND key's
// ordinal and a re-read of the first key recovers the wrong slot from it.
//
// The qualified aliases groupByOutputOrdinals also registers ("O.K"/"I.K") are a
// (correlation, leaf) pair encoded into a string — the channel RFC-197 removes —
// so they are deliberately absent from the maps here: this is the state the
// baker has to be correct in, and it is the state the corpus never puts it in.
func TestGroupByOutputBaker_StructureDecidesWhichSameLeafKey(t *testing.T) {
	t.Parallel()

	tbl := values.OrdinalDomainOfColumnNames([]string{"K", "O_K"})
	lastWins := map[string]int{"K": 1}
	aggOrds := map[string]int{"COUNT(*)": 2}

	t.Run("qualifier_stripped_arm", func(t *testing.T) {
		t.Parallel()
		groupKeys := []values.Value{
			qualifiedKey("K", 0, tbl, "O"),
			qualifiedKey("K", 0, tbl, "I"),
		}
		baker := groupByOutputBaker(groupKeys, lastWins, aggOrds)
		// The reference spells "O.K"; the map holds only the bare leaf, so the
		// baker strips the qualifier, finds the leaf unambiguous in the map,
		// and would take the last-wins slot 1 for a key whose slot is 0.
		got, isField := baker(qualifiedKey("K", 0, tbl, "O")).(*values.FieldValue)
		if !isField || got.Resolved == nil {
			t.Fatalf("O.K did not bake: %#v", got)
		}
		if o := got.Resolved.Root().Ordinal; o != 0 {
			t.Errorf("O.K baked to output slot %d, want 0. keys[\"K\"] is last-wins and holds "+
				"I.K's slot; the slot has to come from the group key's structure.", o)
		}
		if got, _ := baker(qualifiedKey("K", 0, tbl, "I")).(*values.FieldValue); got.Resolved.Root().Ordinal != 1 {
			t.Errorf("I.K baked to output slot %d, want 1", got.Resolved.Root().Ordinal)
		}
	})

	t.Run("direct_leaf_arm", func(t *testing.T) {
		t.Parallel()
		// Two childless keys of one layout: the reference names the leaf
		// directly, so the map hits without any stripping — and hits the wrong
		// slot, because "K" is registered once.
		groupKeys := []values.Value{bareKey("K", 0, tbl), bareKey("K", 3, tbl)}
		baker := groupByOutputBaker(groupKeys, lastWins, aggOrds)
		got, isField := baker(bareKey("K", 0, tbl)).(*values.FieldValue)
		if !isField || got.Resolved == nil {
			t.Fatalf("K did not bake: %#v", got)
		}
		if o := got.Resolved.Root().Ordinal; o != 0 {
			t.Errorf("the first key baked to output slot %d, want 0 (keys[\"K\"]=1 is last-wins)", o)
		}
	})

	t.Run("sort_key_arm", func(t *testing.T) {
		t.Parallel()
		// The ORDER BY consumer. It never sees the qualified spelling at all —
		// AggregateKeyColumnName strips the qualifier before the map is asked —
		// so the last-wins leaf is the ONLY thing standing between an
		// `ORDER BY o.k` and a sort on i.k.
		groupKeys := []values.Value{
			qualifiedKey("K", 0, tbl, "O"),
			qualifiedKey("K", 0, tbl, "I"),
		}
		for want, corr := range map[int]string{0: "O", 1: "I"} {
			got, hit := sortKeyAggregateOutputSlot(
				qualifiedKey("K", 0, tbl, corr), groupKeys, lastWins, aggOrds)
			if !hit || got != want {
				t.Errorf("ORDER BY %s.K resolved to slot (%d,%v), want (%d,true). "+
					"keyOrds[\"K\"] is last-wins; the sort would order by the other key.",
					corr, got, hit, want)
			}
		}
		// The aggregate arm still answers, and still answers second: a key the
		// group-key map does not claim falls through to it.
		if got, hit := sortKeyAggregateOutputSlot(
			values.NewFlatFieldValue("COUNT(*)", values.UnknownType), groupKeys, lastWins, aggOrds); !hit || got != 2 {
			t.Errorf("ORDER BY COUNT(*) resolved to (%d,%v), want (2,true)", got, hit)
		}
		// And a key that is neither is left for the callers below it.
		if _, hit := sortKeyAggregateOutputSlot(
			values.NewFlatFieldValue("ELSEWHERE", values.UnknownType), groupKeys, lastWins, aggOrds); hit {
			t.Error("a key that names no output column must not resolve to a slot")
		}
	})

	t.Run("no_group_keys_leaves_the_name_map_in_charge", func(t *testing.T) {
		t.Parallel()
		// The vacuity guard: with nothing to match structurally the baker must
		// still bake through the name map, or a nil grouping-key list would
		// silently turn every group-key re-read into a pass-through.
		baker := groupByOutputBaker(nil, lastWins, aggOrds)
		got, isField := baker(bareKey("K", 0, tbl)).(*values.FieldValue)
		if !isField || got.Resolved == nil || got.Resolved.Root().Ordinal != 1 {
			t.Fatalf("with no grouping keys the name map must still decide: %#v", got)
		}
	})
}

package cascades

// Two grouping keys that render the SAME accessor name path.
//
// `GROUP BY o.k, i.k` produces two grouping-key Values whose AccessorNamePath is
// ["K"] for both — the path excludes the QOV root, so the qualifier that tells
// them apart in the SQL text is not part of the identity this rule matches on.
// Everything downstream of that collapse is a coin flip:
//
//   - buildGroupKeySet turns the two keys into a ONE-entry set, so membership
//     answers "yes, that's a grouping key" for a reference spelled `k` without
//     knowing WHICH grouping key;
//   - rebindGroupKeyRefToInner then scans the key list for a path match, and a
//     scan that takes the first hit binds the reference to whichever key the
//     GROUP BY listed first.
//
// Bind the wrong one and the pushed predicate filters the pre-aggregate rows on
// a different column than the query asked for — wrong ROWS, silently, with a
// plan that looks fine. Both sites therefore decline: the name domain cannot
// make this identity decision, so it does not guess.
//
// WHY THIS IS A UNIT PIN AND NOT A yamsql SCENARIO. No SQL reaches the wrong
// pick today, and the two gates that stop it are independent of this rule:
//
//  1. A QUALIFIED reference (`HAVING i.k > 15`) arrives with the qualifier baked
//     INTO its accessor name — one accessor whose Field is the flat string
//     "I.K" — so its path key is "I.K", which equals no grouping key's "K" and
//     the predicate is never classified pushable at all. Measured over
//     //pkg/relational/...: 1094 rejections on exactly this mismatch against 20
//     rebind firings, and every one of those 20 had nkeys=1, so first-match was
//     never a choice.
//  2. An UNQUALIFIED reference (`HAVING k > 15`) is the only spelling that could
//     carry the bare path, and the resolver rejects it upstream with
//     `42702 Ambiguous reference K` — including when a SELECT alias supplies the
//     bare spelling (`SELECT i.k AS k ... HAVING k > 15`).
//
// Gate 1 is the qualified-name channel RFC-197 item 6 removes. The moment the
// qualifier segment stops being part of the accessor name, `i.k` renders ["K"],
// the predicate becomes pushable with two same-path keys in hand, and the pick
// is live. That is what these two tests exist to hold shut, and they are at the
// function boundary because that is where the shape is constructible now.
// TestFDB_GroupBySameLeafKeys_PushedHavingStaysAboveTheAggregate pins gate 2 and
// the end-to-end rows.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// qualifiedLeaf builds the plan-time shape of `<corr>.<leaf>`: a FieldValue for
// the leaf rooted at its quantifier. AccessorNamePath stops at that root, so two
// of these with different corr and the same leaf are path-equal.
func qualifiedLeaf(corr, leaf string) values.Value {
	return &values.FieldValue{
		Field: leaf,
		Typ:   values.UnknownType,
		Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(corr)),
	}
}

func TestPredicatePushesBelowGroupBy_TwoKeysOneNamePath_RefusesToPush(t *testing.T) {
	t.Parallel()

	// `GROUP BY o.k, i.k` — two keys, one renderable path.
	ambiguous := []values.Value{qualifiedLeaf("O", "K"), qualifiedLeaf("I", "K")}
	pred := predicates.NewComparisonPredicate(
		qualifiedLeaf("I", "K"),
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(15)),
	)

	if got := PredicatePushesBelowGroupBy(pred, ambiguous); got {
		t.Errorf("PredicatePushesBelowGroupBy(k > 15, [o.k, i.k]) = true, want false.\n"+
			"Both grouping keys render the accessor path %v, so the set they build has one "+
			"entry and membership cannot say WHICH key a reference denotes. Pushing here "+
			"hands rebindGroupKeyRefToInner an undecidable pick; the correct answer is to "+
			"leave the predicate a residual filter above the aggregate.", []string{"K"})
	}

	// The control: distinct paths still push, so the refusal above is the
	// AMBIGUITY and not this rule going dark for every multi-key GROUP BY.
	distinct := []values.Value{qualifiedLeaf("O", "K"), qualifiedLeaf("I", "J")}
	if got := PredicatePushesBelowGroupBy(pred, distinct); !got {
		t.Errorf("PredicatePushesBelowGroupBy(k > 15, [o.k, i.j]) = false, want true — "+
			"two grouping keys with DISTINCT paths must still push; got a rule that "+
			"refuses %d-key GROUP BY outright.", len(distinct))
	}
}

func TestRebindGroupKeyRefToInner_TwoKeysOneNamePath_LeavesTheRefAlone(t *testing.T) {
	t.Parallel()

	first := qualifiedLeaf("O", "K")
	second := qualifiedLeaf("I", "K")
	ref := qualifiedLeaf("I", "K")

	// Second line of defence, pinned independently of the decider above: even
	// handed an ambiguous key list directly, the rebind must not pick.
	got := rebindGroupKeyRefToInner([]values.Value{first, second})(ref)
	if got != ref {
		which := "the SECOND key (i.k)"
		if got == first {
			which = "the FIRST key (o.k) — the wrong one; the reference denotes i.k"
		}
		t.Errorf("rebindGroupKeyRefToInner([o.k, i.k])(i.k) rebound to %s, want the "+
			"reference returned UNCHANGED. Both keys answer to the path [\"K\"], so this "+
			"scan has no basis to prefer either; taking a match anyway makes the pushed "+
			"predicate filter on whichever column GROUP BY listed first.", which)
	}

	// Control: an UNambiguous list must still rebind, or the guard above is
	// satisfied by a function that rebinds nothing.
	other := qualifiedLeaf("I", "J")
	if got := rebindGroupKeyRefToInner([]values.Value{first, other})(ref); got != first {
		t.Errorf("rebindGroupKeyRefToInner([o.k, i.j])(i.k) = %v, want the o.k grouping "+
			"key — an unambiguous single path match must still rebind to that key's own "+
			"inner-row-addressed Value.", got)
	}
}

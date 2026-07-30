package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// legRowType is a join leg's own row layout — what its quantifier flows, and
// therefore the domain a leg-relative ordinal indexes.
func legRowType(cols ...string) *values.RecordType {
	fields := make([]values.Field, len(cols))
	for i, c := range cols {
		fields[i] = values.Field{Name: c, FieldType: values.UnknownType, Ordinal: i}
	}
	return &values.RecordType{Fields: fields}
}

// legRead is a BARE column read off a join leg: the leg's quantifier flows the
// leg's row type, and the read is SOURCE-RELATIVE baked at the column's ordinal
// in that type — the resolver's construction bind (expr.go:278-284), which is
// what reaches this rebase. A FrontierPinned bake reaching it is a planner bug
// the rebase asserts on, so the fixture must not build one.
func legRead(leg string, rt *values.RecordType, ordinal int) *values.FieldValue {
	qov := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(leg), rt)
	return values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		qov, rt.Fields[ordinal].Name, ordinal, values.UnknownType, values.OrdinalDomainOfType(rt),
	)
}

// legRC builds the positional RecordConstructorValue concat a FlatMap outer
// carries: each slot is a bare read off one leg, names bare (duplicates across
// legs allowed — the layout is positional, and the whole point is that two
// legs' same-named columns are different columns).
func legRC(reads ...*values.FieldValue) *values.RecordConstructorValue {
	fields := make([]values.RecordConstructorField, len(reads))
	for i, r := range reads {
		fields[i] = values.RecordConstructorField{Name: r.Field, Value: r}
	}
	return values.NewRecordConstructorValue(fields...)
}

// TestBuriedLegOrdinalLayout pins the COLUMN IDENTITY → global-ordinal
// derivation (WS-N slice 4, keyed by identity since RFC-197 item 3).
func TestBuriedLegOrdinalLayout(t *testing.T) {
	t.Parallel()

	// A(ID, FLAG) ++ B(ID, A_ID). Both legs declare an "ID", which is the whole
	// hazard: the retired key was "CORR.LEAF" built from the display name, and
	// only the alias prefix kept the two IDs apart. The identity keeps them
	// apart by the DOMAIN as well, so a leg that renamed its columns cannot
	// collide with a sibling either.
	aType := legRowType("ID", "FLAG")
	bType := legRowType("ID", "A_ID")
	aID, aFlag := legRead("A", aType, 0), legRead("A", aType, 1)
	bID, bAID := legRead("B", bType, 0), legRead("B", bType, 1)

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	fm := plans.NewRecordQueryFlatMapPlan(
		scan, scan,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"),
		legRC(aID, aFlag, bID, bAID),
		false,
	)
	layout := buriedLegOrdinalLayout(fm)
	if layout == nil {
		t.Fatal("RC-concat FlatMap outer must derive a layout")
	}
	for _, tc := range []struct {
		read *values.FieldValue
		want int
	}{{aID, 0}, {aFlag, 1}, {bID, 2}, {bAID, 3}} {
		id, ok := legSlotIdentity(tc.read)
		if !ok {
			t.Fatalf("a bare leg read must state an identity: %v", tc.read)
		}
		if got, hit := layout[id]; !hit || got != tc.want {
			t.Errorf("layout[%v] = %d (ok=%v), want %d", id, got, hit, tc.want)
		}
	}
	// The two legs' "ID" columns are DIFFERENT entries — same leaf name, same
	// leg-relative ordinal 0, separated only by the correlation. This is the
	// dimension the name-built key could express only through its alias prefix
	// and the reason the map is keyed by the triple.
	aid, _ := legSlotIdentity(aID)
	bid, _ := legSlotIdentity(bID)
	if aid == bid {
		t.Fatal("A.ID and B.ID share one identity — ordinal 0 of two legs are different columns")
	}

	// A LAZY slot states no identity and mints no key, so a layout of only lazy
	// slots is no layout at all. It used to mint a name-built key for every one
	// of them.
	lazySlot := values.NewFieldValue(
		values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("A"), aType),
		"ID", values.UnknownType,
	)
	fmLazy := plans.NewRecordQueryFlatMapPlan(
		scan, scan,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"),
		values.NewRecordConstructorValue(values.RecordConstructorField{Name: "ID", Value: lazySlot}),
		false,
	)
	if got := buriedLegOrdinalLayout(fmLazy); got != nil {
		t.Fatalf("a constructor of LAZY slots states no layout, got %v", got)
	}

	// Underivable: a FlatMap whose result value is not an RC concat.
	fmNoRC := plans.NewRecordQueryFlatMapPlan(
		scan, scan,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"),
		values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("A")), "ID", values.UnknownType),
		false,
	)
	if buriedLegOrdinalLayout(fmNoRC) != nil {
		t.Fatal("non-RC FlatMap outer must not claim a layout")
	}

	// The scan/NLJ-chain shape is no longer derived: planBuriedLegConcat has leg
	// windows and column NAMES, no per-slot value to take an identity from, so
	// every key it could mint would be a name. Declining costs the lazy
	// qualified mint, which is what an underivable outer already gets.
	rt := legRowType("X", "Y")
	typedScan := plans.NewRecordQueryScanPlan([]string{"T"}, rt, false)
	nlj := plans.NewRecordQueryNestedLoopJoinPlan(
		typedScan, typedScan, nil, plans.JoinInner, values.NamedCorrelationIdentifier("L"), values.NamedCorrelationIdentifier("R"),
		values.NewRecordConstructorValue(),
	)
	if got := buriedLegOrdinalLayout(nlj); got != nil {
		t.Fatalf("the NLJ-chain shape states only column NAMES and must decline, got %v", got)
	}
}

// TestRebaseOuterLegValue_OrdinalFirst pins the rebase arms' PRECEDENCE: a
// leg-matching reference is re-anchored onto the merged row's global ordinal
// when the layout answers by IDENTITY (Java's own move,
// PartitionSelectRule.java:296-303), and is handed back UNTOUCHED otherwise.
//
// "Otherwise" used to mean a lazy qualified mint — `QOV(merged)."LEG.COL"`,
// with the merged row's binder left to find it by that string. That mint is
// deleted, so every non-arm-1 outcome now returns the value itself. Each
// assertion below therefore compares against the INPUT VALUE rather than
// against a shape, which is a stronger statement than the one it replaced: it
// says nothing was invented, not merely that nothing was baked.
func TestRebaseOuterLegValue_OrdinalFirst(t *testing.T) {
	t.Parallel()
	mergedCorr := values.NamedCorrelationIdentifier("$m")
	aType := legRowType("A_ID", "FLAG")
	legRef := legRead("A", aType, 0)
	legID, ok := legSlotIdentity(legRef)
	if !ok {
		t.Fatal("test setup: a bare leg read must state an identity")
	}

	// Layout answers → born baked at the global ordinal.
	baked := rebaseOuterLegValue(legRef, []string{"A"}, mergedCorr, map[values.ColumnIdentity]int{legID: 3}, nil)
	fv, isFV := baked.(*values.FieldValue)
	if !isFV {
		t.Fatalf("got %T", baked)
	}
	if fv.Resolved == nil || fv.Resolved.Root().Ordinal != 3 {
		t.Fatalf("want baked ordinal 3, got resolved=%v", fv.Resolved)
	}
	if fv.Field != "A.A_ID" {
		t.Fatalf("display field: got %q, want A.A_ID", fv.Field)
	}
	if qov, isQ := fv.Child.(*values.QuantifiedObjectValue); !isQ || qov.Correlation != mergedCorr {
		t.Fatalf("child must be QOV($m), got %T", fv.Child)
	}

	// A layout entry for the SAME column of a DIFFERENT leg must not answer. The
	// other leg is a SELF-JOIN twin — the IDENTICAL row type, so the same leaf
	// name AND the same domain AND the same leg-relative ordinal — leaving the
	// CORRELATION as the only element that can refuse. Giving the twin a
	// different row type would let the domain do the refusing and the test would
	// pass with the correlation dropped entirely.
	otherLegID, _ := legSlotIdentity(legRead("B", aType, 0))
	miss := rebaseOuterLegValue(legRef, []string{"A"}, mergedCorr, map[values.ColumnIdentity]int{otherLegID: 3}, nil)
	if miss != values.Value(legRef) {
		t.Fatalf("a SELF-JOIN twin leg's identical column answered the lookup: the read "+
			"was rewritten to %v — ordinal 0 of two quantifiers are different columns, "+
			"and the correlation is the only element that can say so here. A miss must "+
			"hand the read back UNTOUCHED, on its own leg alias and its own ordinal, "+
			"which the runtime binder (executor.bindMergedOuterLegs) resolves against "+
			"that leg's own window.", miss)
	}

	// A LAZY reference states no identity, so it finds nothing rather than
	// finding whatever its display name spells — and, since the mint is gone,
	// finding nothing means it is returned as it arrived.
	lazyRef := values.NewFieldValue(
		values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("A"), aType),
		"A_ID", values.UnknownType,
	)
	lazyOut := rebaseOuterLegValue(lazyRef, []string{"A"}, mergedCorr, map[values.ColumnIdentity]int{legID: 3}, nil)
	if lazyOut != values.Value(lazyRef) {
		t.Fatalf("a lazy reference was rewritten to %v. It states no identity, so the "+
			"layout cannot key it and there is nothing left to key it BY except its "+
			"display name — which is the channel this arm no longer has.", lazyOut)
	}

	// No layout → the PASS-THROUGH. The read already names its leg and already
	// carries its ordinal in that leg's domain, so re-anchoring it onto the merge
	// correlation would trade a stated identity for a name.
	lazy2 := rebaseOuterLegValue(legRef, []string{"A"}, mergedCorr, nil, nil)
	if lazy2 != values.Value(legRef) {
		t.Fatalf("a nil layout rewrote the read to %v. With no merged layout to state "+
			"an ordinal in, the reference's OWN leg-local ordinal is the only honest "+
			"answer and it is already on the value.", lazy2)
	}

	// Non-matching leg → untouched.
	same := rebaseOuterLegValue(legRef, []string{"Z"}, mergedCorr, map[values.ColumnIdentity]int{legID: 3}, nil)
	if same != legRef {
		t.Fatal("non-matching leg must return the value unchanged")
	}
}

// TestMergedOuterLegAliasesCarriesBothNamespaces pins that the alias set the
// dotted rebase matches against contains the leg IDENTITIES, not only the
// source-alias text.
//
// rebaseOuterLegValue compares each entry against a reference's own QOV
// CORRELATION name, and those references are correlated to the select's
// QUANTIFIERS. The set was built from the select's parallel source-alias slice
// alone, upper-folded, which broke on two axes — and the verification set beside
// it (existLegCorrs) had already worked around one of them by adding the
// quantifier aliases explicitly, which is what makes the omission in the REBASE
// set an oversight rather than a design:
//
//   - the two channels can name different things. Measured over the FDB corpus,
//     the source-alias slice carries a re-minted identifier while the quantifier
//     keeps the user alias on 12 of ~80k firings (census witnesses "q$N vs E");
//   - the fold ran one way only. Entries were upper-folded while a reference's
//     correlation is verbatim, so a lowercase MINTED alias could not match its own
//     entry even when the two channels agreed.
//
// Neither miss is loud: the reference keeps pointing at a leg alias that is not
// bound inside the FlatMap, evaluates to NULL, and an EXISTS correlation that
// never matches drops rows.
//
// HOW A MATCH IS OBSERVED, and why it changed. The arm used to make a match
// visible by rewriting a matched read into a dotted `QOV(merged)."LEG.COL"`, so
// "was it rebased" could be read straight off the field name. That mint is
// deleted and the pass-through returns the value itself, so a matched read and
// an unmatched one now return the SAME thing on the no-layout path — the field
// name can no longer answer the question. The remaining arm that acts on a
// match is the MERGED RE-ANCHOR, so each case below hands the rebase a layout
// keyed by the read's own identity: a matched alias bakes the merged ordinal,
// an unmatched one leaves the read alone.
//
// That is not a weaker test. It is the same question asked through the one arm
// that still answers it — and it makes the alias set's remaining consumer
// explicit, which matters because that consumer is dead-in-effect on the corpus
// (the layout-bearing callers rebase through rebaseOuterLegRefsOrdinal).
func TestMergedOuterLegAliasesCarriesBothNamespaces(t *testing.T) {
	t.Parallel()
	mergedCorr := values.NamedCorrelationIdentifier("$m")
	rt := legRowType("K")

	// assertRebasedByAliasSet drives the rebase with a layout the read's own
	// identity keys, so a MATCHED alias re-anchors onto the merged ordinal and an
	// unmatched one returns the read untouched.
	assertRebasedByAliasSet := func(t *testing.T, set []string, ref *values.FieldValue, because string) {
		t.Helper()
		id, ok := legSlotIdentity(ref)
		if !ok {
			t.Fatal("test setup: a bare leg read must state an identity")
		}
		out := rebaseOuterLegValue(ref, set, mergedCorr, map[values.ColumnIdentity]int{id: 4}, nil)
		fv, isFV := out.(*values.FieldValue)
		if !isFV {
			t.Fatalf("rebase returned %T", out)
		}
		if fv == ref || fv.Resolved == nil || fv.Resolved.Root().Ordinal != 4 {
			t.Errorf("reference to leg %s was NOT re-anchored (alias set %v).\n  %s\n"+
				"  An un-rebased leg reference is unbound inside the FlatMap: it evaluates\n"+
				"  to NULL and the EXISTS drops rows.", ref.Child, set, because)
			return
		}
		if qov, isQ := fv.Child.(*values.QuantifiedObjectValue); !isQ || qov.Correlation != mergedCorr {
			t.Errorf("re-anchored child = %T, want QOV($m)", fv.Child)
		}
	}

	// AXIS 1 — the two channels disagree. The plan's leg identity is the user
	// alias "E"; the select's source-alias slice carries a re-minted "q$7".
	t.Run("stale source alias", func(t *testing.T) {
		t.Parallel()
		set := mergedOuterLegAliases("q$7", "D",
			values.NamedCorrelationIdentifier("E"), values.NamedCorrelationIdentifier("D"))
		assertRebasedByAliasSet(t, set, legRead("E", rt, 0),
			"The alias set carried only the stale source-alias text, so nothing in it "+
				"matched the reference's own correlation.")
	})

	// AXIS 2 — the channels AGREE on a lowercase machine mint, and the fold alone
	// broke the match. This is the axis the stale-alias case cannot expose.
	t.Run("minted lowercase alias", func(t *testing.T) {
		t.Parallel()
		minted := values.NamedCorrelationIdentifier("q$9")
		set := mergedOuterLegAliases("q$9", "D", minted, values.NamedCorrelationIdentifier("D"))
		assertRebasedByAliasSet(t, set, legRead("q$9", rt, 0),
			"Every entry used to be upper-folded while the reference's correlation is "+
				"verbatim, so a minted leg could not match its own entry.")
	})

	// The NEGATIVE control the two above need: an alias set that does NOT name
	// the read's leg must leave it alone. Without it both cases above would pass
	// against a rebase that re-anchored unconditionally, which is exactly the
	// failure a set-vs-comparison mismatch produces in the other direction.
	t.Run("unmatched alias is left alone", func(t *testing.T) {
		t.Parallel()
		ref := legRead("E", rt, 0)
		id, _ := legSlotIdentity(ref)
		set := mergedOuterLegAliases("Z", "D", values.NamedCorrelationIdentifier("Z"))
		out := rebaseOuterLegValue(ref, set, mergedCorr, map[values.ColumnIdentity]int{id: 4}, nil)
		if out != values.Value(ref) {
			t.Fatalf("a read whose leg the alias set does not name was rewritten to %v "+
				"(set %v). The set is what decides which correlations this arm may touch; "+
				"an arm that re-anchors regardless would move references belonging to "+
				"enclosing scopes onto a merged row that does not carry them.", out, set)
		}
	})

	// The fold must not be applied to the IDENTITY half: a quoted "Q$9" is a
	// different leg from a minted q$9, and folding the set would make one match
	// the other — the forgery the exact comparison exists to exclude.
	t.Run("identity half is not folded", func(t *testing.T) {
		t.Parallel()
		set := mergedOuterLegAliases("", "", values.NamedCorrelationIdentifier("q$9"))
		for _, e := range set {
			if e == "Q$9" {
				t.Fatalf("alias set = %v — the identity half was upper-folded, so a quoted "+
					"\"Q$9\" leg would match a minted q$9's entry", set)
			}
		}
		if len(set) != 1 || set[0] != "q$9" {
			t.Fatalf("alias set = %v, want exactly the verbatim identity", set)
		}
	})

	// And the TEXT half is still there and still folded — the dotted key namespace
	// depends on it, so removing the fold would be its own regression.
	t.Run("text half stays folded", func(t *testing.T) {
		t.Parallel()
		set := mergedOuterLegAliases("e", "d")
		want := map[string]bool{"E": true, "D": true}
		if len(set) != 2 {
			t.Fatalf("alias set = %v, want two upper-folded text entries", set)
		}
		for _, e := range set {
			if !want[e] {
				t.Errorf("alias set entry %q is not the upper fold of its source alias", e)
			}
		}
	})
}

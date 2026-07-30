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

// TestRebaseOuterLegValue_OrdinalFirst pins the slice-4 rebase arms: a
// leg-matching reference bakes to the merged row's global ordinal when the
// layout answers by IDENTITY, and keeps the lazy qualified mint otherwise.
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
	baked := rebaseOuterLegValue(legRef, []string{"A"}, mergedCorr, map[values.ColumnIdentity]int{legID: 3})
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
	miss := rebaseOuterLegValue(legRef, []string{"A"}, mergedCorr, map[values.ColumnIdentity]int{otherLegID: 3})
	mfv := miss.(*values.FieldValue)
	if mfv.Resolved != nil {
		t.Fatalf("a SELF-JOIN twin leg's identical column answered the lookup: got resolved=%v — "+
			"ordinal 0 of two quantifiers are different columns, and the correlation is the "+
			"only element that can say so here", mfv.Resolved)
	}
	if mfv.Field != "A.A_ID" {
		t.Fatalf("lazy field: got %q", mfv.Field)
	}

	// A LAZY reference states no identity, so it finds nothing rather than
	// finding whatever its display name spells.
	lazyRef := values.NewFieldValue(
		values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("A"), aType),
		"A_ID", values.UnknownType,
	)
	lazyOut := rebaseOuterLegValue(lazyRef, []string{"A"}, mergedCorr, map[values.ColumnIdentity]int{legID: 3})
	if lazyOut.(*values.FieldValue).Resolved != nil {
		t.Fatal("a lazy reference must not be baked by a layout it cannot key into")
	}

	// No layout → lazy qualified mint (pre-slice-4 behavior).
	lazy2 := rebaseOuterLegValue(legRef, []string{"A"}, mergedCorr, nil)
	if lazy2.(*values.FieldValue).Resolved != nil {
		t.Fatal("nil layout must stay lazy")
	}

	// Non-matching leg → untouched.
	same := rebaseOuterLegValue(legRef, []string{"Z"}, mergedCorr, map[values.ColumnIdentity]int{legID: 3})
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
func TestMergedOuterLegAliasesCarriesBothNamespaces(t *testing.T) {
	t.Parallel()
	mergedCorr := values.NamedCorrelationIdentifier("$m")
	rt := legRowType("K")

	// AXIS 1 — the two channels disagree. The plan's leg identity is the user
	// alias "E"; the select's source-alias slice carries a re-minted "q$7".
	t.Run("stale source alias", func(t *testing.T) {
		t.Parallel()
		set := mergedOuterLegAliases("q$7", "D",
			values.NamedCorrelationIdentifier("E"), values.NamedCorrelationIdentifier("D"))
		ref := legRead("E", rt, 0)
		out := rebaseOuterLegValue(ref, set, mergedCorr, nil)
		fv, isFV := out.(*values.FieldValue)
		if !isFV {
			t.Fatalf("rebase returned %T", out)
		}
		if fv.Field != "E.K" {
			t.Errorf("reference to leg E was NOT rebased (field %q, want E.K).\n"+
				"  The alias set carried only the stale source-alias text, so nothing in it\n"+
				"  matched the reference's own correlation. An un-rebased leg reference is\n"+
				"  unbound inside the FlatMap: it evaluates to NULL and the EXISTS drops rows.",
				fv.Field)
		}
		if qov, isQ := fv.Child.(*values.QuantifiedObjectValue); !isQ || qov.Correlation != mergedCorr {
			t.Errorf("rebased child = %T, want QOV($m)", fv.Child)
		}
	})

	// AXIS 2 — the channels AGREE on a lowercase machine mint, and the fold alone
	// broke the match. This is the axis the stale-alias case cannot expose.
	t.Run("minted lowercase alias", func(t *testing.T) {
		t.Parallel()
		minted := values.NamedCorrelationIdentifier("q$9")
		set := mergedOuterLegAliases("q$9", "D", minted, values.NamedCorrelationIdentifier("D"))
		ref := legRead("q$9", rt, 0)
		out := rebaseOuterLegValue(ref, set, mergedCorr, nil)
		fv := out.(*values.FieldValue)
		if fv.Field != "q$9.K" {
			t.Errorf("reference to minted leg q$9 was NOT rebased (field %q, want q$9.K).\n"+
				"  Every entry used to be upper-folded while the reference's correlation is\n"+
				"  verbatim, so a minted leg could not match its own entry.", fv.Field)
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

package executor

// Executor pins for span derivation over a lateral-unnest seed whose element
// is MIXED into the merged row (carried through a partition collapse), and
// for the NLJ build's behavior when it has no legRVs to derive windows from.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestMixedElementSpanSynthesis pins the terminal synthesis: a
// SINGLE-accessor pinned ref over a merge quantifier whose referenced slot is
// a bare NON-RECORD QOV (the gathered unnest's mixed element after the
// partition collapsed {source, Explode}) resolves to a synthesized 1-field
// element leg — alias = the SLOT QOV's correlation (the AS alias, never the
// merge alias), the sole column named from the enclosing RC field. Without
// legRVs the shape must DECLINE (fail-safe, never mis-windowed); with a
// RECORD slot the merge-leg run resolution is UNCHANGED (the load-bearing
// non-record guard).
func TestMixedElementSpanSynthesis(t *testing.T) {
	t.Parallel()

	legS := values.NewRecordType("", false, []values.Field{
		{Name: "SID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "ARR", FieldType: values.NotNullLong, Ordinal: 1},
	})
	qovS := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("S"), legS)
	elemQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("EL"), values.NotNullLong)

	mergedType := values.NewRecordType("", false, []values.Field{
		{Name: values.OrdinalFieldName(0), FieldType: legS, Ordinal: 0},
		{Name: values.OrdinalFieldName(1), FieldType: values.NotNullLong, Ordinal: 1},
	})
	mergeQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("m"), mergedType)

	elemRef, err := values.NewFieldValueOfOrdinal(mergeQOV, 1)
	if err != nil {
		t.Fatalf("bake element slot: %v", err)
	}
	top := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "SID", Value: s3FusedRef(t, mergeQOV, 0, 0)},
		values.RecordConstructorField{Name: "ARR", Value: s3FusedRef(t, mergeQOV, 0, 1)},
		values.RecordConstructorField{Name: "EL", Value: elemRef},
	)
	legRVs := map[values.CorrelationIdentifier]values.Value{
		values.NamedCorrelationIdentifier("m"): values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: values.OrdinalFieldName(0), Value: qovS},
			values.RecordConstructorField{Name: values.OrdinalFieldName(1), Value: elemQOV},
		),
	}

	spans, _, ok := ordinalJoinSpansOf(top, legRVs)
	if !ok {
		t.Fatal("the mixed-element translated top must derive windows")
	}
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2 (the S run + the synthesized element leg)", len(spans))
	}
	if spans[0].Alias.Name() != "S" || spans[0].Width != 2 {
		t.Fatalf("span 0 = %s width %d, want the full S run (width 2)", spans[0].Alias, spans[0].Width)
	}
	el := spans[1]
	if el.Alias.Name() != "EL" {
		t.Fatalf("element span alias = %s, want the SLOT QOV's correlation EL (never the merge alias)", el.Alias)
	}
	if el.Width != 1 || len(el.LegType.Fields) != 1 || el.LegType.Fields[0].Name != "EL" {
		t.Fatalf("element leg = %+v, want the 1-field synthesis named from the enclosing RC field", el.LegType)
	}

	// Fail-safe: no legRVs → the fused outer refs cannot resolve → decline.
	if _, _, ok := ordinalJoinSpansOf(top, nil); ok {
		t.Fatal("the translated top must DECLINE without legRVs")
	}

	// The non-record guard: a RECORD slot referenced single-accessor is a
	// merge-leg run, NOT an element — the pristine positional-merge RC's own
	// consumers keep resolving unchanged (partial coverage here, so the probe
	// declines rather than synthesizing a bogus leg).
	recRef, err := values.NewFieldValueOfOrdinal(mergeQOV, 0)
	if err != nil {
		t.Fatalf("bake record slot: %v", err)
	}
	topRec := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "R", Value: recRef},
		values.RecordConstructorField{Name: "EL", Value: elemRef},
	)
	if _, _, ok := ordinalJoinSpansOf(topRec, legRVs); ok {
		t.Fatal("a RECORD slot must not synthesize an element leg (partial merge-run coverage declines)")
	}
}

// TestNLJBuildNilLegRVsWindows pins a known dimension: the NLJ build derives
// its spans WITHOUT legRVs (newOrdinalJoinBuild), so a TRANSLATED top (fused
// post-merge refs) builds with WindowsOK=false — the emitted row's Type
// carries no leg-window boundaries of its own; downstream consumers recover
// them separately via downstreamLegWindows. This is a LOUD decline today
// (pinned as such) rather than a silent wrong read; if this ever produces a
// wrong ANSWER instead of declining, that's the signal to add NLJ-side legRV
// recovery (the FlatMap already has it).
func TestNLJBuildNilLegRVsWindows(t *testing.T) {
	t.Parallel()

	legS := values.NewRecordType("", false, []values.Field{
		{Name: "SID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	mergedType := values.NewRecordType("", false, []values.Field{
		{Name: values.OrdinalFieldName(0), FieldType: legS, Ordinal: 0},
	})
	mergeQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("m"), mergedType)
	legB := values.NewRecordType("", false, []values.Field{
		{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	qovB := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("B"), legB)
	b0, err := values.NewFieldValueOfOrdinal(qovB, 0)
	if err != nil {
		t.Fatalf("bake B#0: %v", err)
	}
	top := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "SID", Value: s3FusedRef(t, mergeQOV, 0, 0)},
		values.RecordConstructorField{Name: "BID", Value: b0},
	)

	build, err := newOrdinalJoinBuild(top, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if build == nil || !build.Enabled {
		t.Fatal("a baked translated top must build ordinal (positional authority intact)")
	}
	if build.WindowsOK {
		t.Fatal("the NLJ build has no legRVs — a translated top's windows must NOT derive at build (they are recovered downstream via downstreamLegWindows)")
	}
}

// TestSpanWindowCrossAgreement_PlainSeed pins that the executor's
// ordinalJoinSpans and the planner's values.OrdinalSeedLegWindows derive
// IDENTICAL leg layout (offsets, widths, names, merged-type field order) for
// a pristine 3-leg seed, and both DECLINE the same non-pristine shape (a
// translated top whose reference fused to a multi-accessor path through a
// merge quantifier). The two derivations are separate implementations of the
// same contract; if they drift, one of them reads the wrong slot.
func TestSpanWindowCrossAgreement_PlainSeed(t *testing.T) {
	t.Parallel()
	mk := func(alias string, cols ...string) *values.QuantifiedObjectValue {
		fields := make([]values.Field, len(cols))
		for i, c := range cols {
			fields[i] = values.Field{Name: c, FieldType: values.NotNullLong, Ordinal: i}
		}
		return values.NewQuantifiedObjectValueOfType(
			values.NamedCorrelationIdentifier(alias),
			values.NewRecordType("", false, fields))
	}
	bake := func(qov *values.QuantifiedObjectValue, i int) values.RecordConstructorField {
		fv, err := values.NewFieldValueOfOrdinal(qov, i)
		if err != nil {
			t.Fatalf("bake: %v", err)
		}
		return values.RecordConstructorField{Name: fv.Field, Value: fv}
	}
	a := mk("A", "AID", "AV")
	b := mk("B", "BID")
	c := mk("C", "CID", "CV", "CW")
	rc := values.NewRawRecordConstructorValue(
		bake(a, 0), bake(a, 1), bake(b, 0), bake(c, 0), bake(c, 1), bake(c, 2),
	)

	spans, mergedFromSpans, ok := ordinalJoinSpans(rc)
	if !ok {
		t.Fatal("executor spans must derive for the pristine 3-leg seed")
	}
	windows, mergedFromWindows := values.OrdinalSeedLegWindows(rc)
	if windows == nil {
		t.Fatal("values windows must derive for the pristine 3-leg seed")
	}
	if len(spans) != len(windows) {
		t.Fatalf("leg count disagreement: %d spans vs %d windows", len(spans), len(windows))
	}
	for _, s := range spans {
		w, present := windows[s.Alias]
		if !present {
			t.Fatalf("leg %s in spans but not windows", s.Alias)
		}
		if w.Offset != s.Offset || len(w.Typ.Fields) != s.Width {
			t.Fatalf("leg %s LAYOUT DISAGREEMENT: window (offset %d, width %d) vs span (offset %d, width %d) — the rebase and the build would read different slots",
				s.Alias, w.Offset, len(w.Typ.Fields), s.Offset, s.Width)
		}
	}
	if len(mergedFromSpans.Fields) != len(mergedFromWindows.Fields) {
		t.Fatalf("merged-type width disagreement: %d vs %d", len(mergedFromSpans.Fields), len(mergedFromWindows.Fields))
	}
	for i := range mergedFromSpans.Fields {
		if mergedFromSpans.Fields[i].Name != mergedFromWindows.Fields[i].Name {
			t.Fatalf("merged field %d name disagreement: %q vs %q",
				i, mergedFromSpans.Fields[i].Name, mergedFromWindows.Fields[i].Name)
		}
	}

	// Both must DECLINE the same non-pristine shape (a translated top whose
	// ref FUSED to a multi-accessor path through a merge quantifier).
	mergedType := values.NewRecordType("", false, []values.Field{
		{Name: values.OrdinalFieldName(0), FieldType: a.Typ, Ordinal: 0},
	})
	mergeQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("m"), mergedType)
	nonPristine := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "AID", Value: s3FusedRef(t, mergeQOV, 0, 0)},
		values.RecordConstructorField{Name: "AV", Value: s3FusedRef(t, mergeQOV, 0, 1)},
	)
	_, _, spansOK := ordinalJoinSpans(nonPristine)
	winDeclined, _ := values.OrdinalSeedLegWindows(nonPristine)
	if spansOK || winDeclined != nil {
		t.Fatalf("both authorities must DECLINE the fused top: spans ok=%v windows derived=%v", spansOK, winDeclined != nil)
	}
}

// mixedSeedOuter builds a 2-column baked outer leg QOV keyed by the given alias.
func mixedSeedOuter(alias string) *values.QuantifiedObjectValue {
	return values.NewQuantifiedObjectValueOfType(
		values.NamedCorrelationIdentifier(alias),
		values.NewRecordType("", false, []values.Field{
			{Name: alias + "ID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: alias + "V", FieldType: values.NotNullLong, Ordinal: 1},
		}))
}

// bakeOrdinal bakes a frontier-pinned ofOrdinal field over a leg QOV.
func bakeOrdinal(t *testing.T, qov *values.QuantifiedObjectValue, i int) values.RecordConstructorField {
	t.Helper()
	fv, err := values.NewFieldValueOfOrdinal(qov, i)
	if err != nil {
		t.Fatalf("bake: %v", err)
	}
	return values.RecordConstructorField{Name: fv.Field, Value: fv}
}

// assertSpanWindowAgreement asserts the executor's ordinalJoinSpans and the
// planner's values.OrdinalSeedLegWindows agree BIT-FOR-BIT on the seed rc: same
// accept/decline, and — when accepted — identical leg count, per-leg offset,
// per-leg type (names AND types), and merged type (names AND types). A
// fixture that only checks one happy shape by name would let the two walks
// drift on the accept boundary or on field types while staying green — a
// sentinel that goes green on a measured divergence isn't one.
func assertSpanWindowAgreement(t *testing.T, label string, rc *values.RecordConstructorValue, wantAccept bool) {
	t.Helper()
	assertSpanWindowAgreementVia(t, label, rc, wantAccept, false)
}

// assertSpanWindowAgreementNested is the same assertion over the NESTED-accepting
// pair (ordinalJoinSpansAcceptingNested / values.OrdinalSeedLegWindowsAcceptingNested).
//
// Two entry points means two accept boundaries, and an agreement fixture that
// only walked one of them would let the other drift completely — which is the
// exact failure the single fixture was built to prevent for the single walk.
func assertSpanWindowAgreementNested(t *testing.T, label string, rc *values.RecordConstructorValue, wantAccept bool) {
	t.Helper()
	assertSpanWindowAgreementVia(t, label, rc, wantAccept, true)
}

func assertSpanWindowAgreementVia(t *testing.T, label string, rc *values.RecordConstructorValue, wantAccept, nested bool) {
	t.Helper()
	spansFn, winFn := ordinalJoinSpans, values.OrdinalSeedLegWindows
	if nested {
		spansFn, winFn = ordinalJoinSpansAcceptingNested, values.OrdinalSeedLegWindowsAcceptingNested
	}
	spans, mergedFromSpans, spansOK := spansFn(rc)
	windows, mergedFromWindows := winFn(rc)
	winOK := windows != nil
	if spansOK != winOK {
		t.Fatalf("%s: ACCEPT DISAGREEMENT — executor spans ok=%v, values windows ok=%v", label, spansOK, winOK)
	}
	if spansOK != wantAccept {
		t.Fatalf("%s: want accept=%v, got %v", label, wantAccept, spansOK)
	}
	if !wantAccept {
		return
	}
	// The windows map keys every ADDRESSABLE name (a box run's SUBS included —
	// the box's name means its rightmost LEAF); spans are RUN-level. Compare
	// per addressable name: a plain run 1:1, a box run per sub-leg.
	// Assumes the LEAF-NAMED box regime (a sub leg carries the run alias, so
	// the values twin REPLACES the run window): a $BOX-minted fixture would
	// retain the whole-run window under the minted key and mis-count here —
	// extend the accounting before adding such a fixture.
	wantWindows := 0
	for _, s := range spans {
		if len(s.LegType.Legs) > 0 {
			wantWindows += len(s.LegType.Legs)
			for _, sub := range s.LegType.Legs {
				w, present := windows[sub.Alias]
				if !present {
					t.Fatalf("%s: box-run sub %s in spans' Legs but not windows", label, sub.Name)
				}
				if w.Kind != sub.Kind {
					t.Fatalf("%s: box-run sub %s KIND DISAGREEMENT: window %v vs span sub %v",
						label, sub.Name, w.Kind, sub.Kind)
				}
				// Width is a SLOT count on both sides. For a flat sub-leg that is
				// its column count; for a NESTED one it is 1, however wide the
				// sub-leg's row is — the window's Typ carries the columns.
				wantSubWidth := len(w.Typ.Fields)
				if w.Kind == values.LegKindNested {
					wantSubWidth = 1
				}
				if w.Offset != s.Offset+sub.Start || wantSubWidth != sub.Width {
					t.Fatalf("%s: box-run sub %s LAYOUT DISAGREEMENT: window (offset %d, slots %d) vs span sub (offset %d, width %d)",
						label, sub.Name, w.Offset, wantSubWidth, s.Offset+sub.Start, sub.Width)
				}
			}
			// THE TWO BOX REGIMES, and the accounting now covers both.
			//
			// This block used to assume the LEAF-NAMED regime only — a sub-leg
			// carrying the RUN's own alias, where finalizeSeedWindows REPLACES the
			// run's window with the leaf's narrower sub-window, so the run
			// contributes no window of its own. Its own comment said to extend the
			// accounting before adding a fixture of the other kind, and RFC-200
			// §1(b) is exactly that fixture: a nested sub-leg with its OWN alias,
			// which is ADDED BESIDE the run's window rather than replacing it.
			//
			// So: count the run's own window unless some sub-leg claimed its alias.
			replaced := false
			for _, sub := range s.LegType.Legs {
				if values.SameLeg(sub.Alias, s.Alias) {
					replaced = true
				}
			}
			if !replaced {
				wantWindows++
				rw, present := windows[s.Alias]
				if !present {
					t.Fatalf("%s: box run %s is not REPLACED by a leaf sub-window and yet "+
						"has no window of its own — a run whose subs are all separately "+
						"aliased must remain addressable under its own alias", label, s.Alias)
				}
				if rw.Offset != s.Offset {
					t.Fatalf("%s: box run %s window offset %d vs span offset %d",
						label, s.Alias, rw.Offset, s.Offset)
				}
			}
			continue
		}
		wantWindows++
		w, present := windows[s.Alias]
		if !present {
			t.Fatalf("%s: leg %s in spans but not windows", label, s.Alias)
		}
		// THE KIND IS PART OF THE LAYOUT. A span and a window that agree on offset
		// and width while disagreeing on kind are two sides reading the same slot
		// with different arithmetic — the flat side adds a leg-local ordinal to the
		// offset, the nested side descends into it — which is a wrong-column read
		// with every numeric field matching.
		if w.Kind != s.Kind {
			t.Fatalf("%s: leg %s KIND DISAGREEMENT: window %v vs span %v",
				label, s.Alias, w.Kind, s.Kind)
		}
		// A nested leg occupies ONE slot however wide its row is, so the span's
		// Width is 1 while the window's Typ carries the leg's real columns. Width
		// is a SLOT count on both sides; equating it with the column count is only
		// valid for a flat run.
		wantWidth := len(w.Typ.Fields)
		if w.Kind == values.LegKindNested {
			wantWidth = 1
		}
		if w.Offset != s.Offset || wantWidth != s.Width {
			t.Fatalf("%s: leg %s LAYOUT DISAGREEMENT: window (offset %d, slots %d) vs span (offset %d, width %d)",
				label, s.Alias, w.Offset, wantWidth, s.Offset, s.Width)
		}
		for i := range s.LegType.Fields {
			sf, wf := s.LegType.Fields[i], w.Typ.Fields[i]
			if !strings.EqualFold(sf.Name, wf.Name) || sf.FieldType.String() != wf.FieldType.String() {
				t.Fatalf("%s: leg %s field %d TYPE DISAGREEMENT: span {%s %s} vs window {%s %s}",
					label, s.Alias, i, sf.Name, sf.FieldType, wf.Name, wf.FieldType)
			}
		}
	}
	if len(windows) != wantWindows {
		t.Fatalf("%s: window count disagreement: %d windows vs %d addressable names from spans", label, len(windows), wantWindows)
	}
	if len(mergedFromSpans.Fields) != len(mergedFromWindows.Fields) {
		t.Fatalf("%s: merged-type width disagreement: %d vs %d", label, len(mergedFromSpans.Fields), len(mergedFromWindows.Fields))
	}
	for i := range mergedFromSpans.Fields {
		sf, wf := mergedFromSpans.Fields[i], mergedFromWindows.Fields[i]
		if sf.Name != wf.Name || sf.FieldType.String() != wf.FieldType.String() {
			t.Fatalf("%s: merged field %d DISAGREEMENT: span {%s %s} vs window {%s %s}",
				label, i, sf.Name, sf.FieldType, wf.Name, wf.FieldType)
		}
	}
	// The merged type's LEGS are the dotted-read bridges' contract — the two
	// derivations must agree on the full boundary list (name, start, width,
	// order). The dimension whose absence let the values twin drop a box
	// run's rightmost-leaf boundary while the executor emitted it.
	if len(mergedFromSpans.Legs) != len(mergedFromWindows.Legs) {
		t.Fatalf("%s: merged-type LEGS count disagreement: spans %v vs windows %v", label, mergedFromSpans.Legs, mergedFromWindows.Legs)
	}
	for i := range mergedFromSpans.Legs {
		if mergedFromSpans.Legs[i] != mergedFromWindows.Legs[i] {
			t.Fatalf("%s: merged-type LEG %d DISAGREEMENT: span %+v vs window %+v", label, i, mergedFromSpans.Legs[i], mergedFromWindows.Legs[i])
		}
	}
}

// TestSpanWindowCrossAgreement_MixedSeed pins the cross-agreement invariant
// for the MIXED single-source lateral-unnest seed (a baked outer prefix + a
// trailing bare-QOV whole-object element). The executor's
// ordinalJoinSpans/unnestMixedSeedSpans and the planner's
// values.OrdinalSeedLegWindows must agree BIT-FOR-BIT — accept/decline, leg
// layout, AND field types. Without this invariant the executor could accept
// the mixed seed while the planner declined it, forcing the translator to
// PREDICT the executor's routing instead of relying on a shared contract.
//
// It locks the ACCEPT BOUNDARY, not just one happy shape: the two walks must
// AGREE on a multi-leg outer prefix (a multi-source gathered lateral unnest,
// now accepted by both together) and BOTH DECLINE a single-leg pristine seed
// — shapes where the two derivations were once measured to drift (planner
// accepted, executor declined) before their accept boundaries were extended
// in lockstep.
func TestSpanWindowCrossAgreement_MixedSeed(t *testing.T) {
	t.Parallel()
	tLeg := mixedSeedOuter("T")

	// ACCEPT: multi-col outer + a bare-QOV scalar element (the whole-object element
	// the seed cannot ofOrdinal-bake), keyed by the AS alias X.
	scalarElem := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("X"), values.NotNullLong)
	assertSpanWindowAgreement(t, "mixed/scalar-element", values.NewRawRecordConstructorValue(
		bakeOrdinal(t, tLeg, 0), bakeOrdinal(t, tLeg, 1),
		values.RecordConstructorField{Name: "X", Value: scalarElem},
	), true)

	// ACCEPT: a STRUCT array element maps to UnknownType (NOT a *RecordType), so it
	// flows the same mixed path as a scalar (unnestArrayElementType / isMixedSeedElement).
	structElem := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("W"), values.UnknownType)
	assertSpanWindowAgreement(t, "mixed/struct-element", values.NewRawRecordConstructorValue(
		bakeOrdinal(t, tLeg, 0), bakeOrdinal(t, tLeg, 1),
		values.RecordConstructorField{Name: "W", Value: structElem},
	), true)

	// ACCEPT: a multi-LEG outer prefix (T then B, BOTH FULLY covered) + a scalar
	// element — the MULTI-SOURCE gathered lateral unnest `FROM T, B, T.arr AS X`,
	// the grouped-gather input a GROUP BY un-collapse reads over this shape. Both
	// walks now derive [T-window, B-window, element-window] and MUST agree bit-for-bit
	// (accept, per-leg offsets, field types) — `assertSpanWindowAgreement(true)` proves
	// it. Extending the ONE authority (OrdinalSeedLegWindows) and its executor twin
	// (unnestMixedSeedSpans) TOGETHER, re-pinned here, is how the invariant stays
	// worth guarding: the un-collapse groups over exactly this seed and
	// positionally bakes its group keys via these windows. A DUPLICATE alias in the
	// prefix (a split run) still declines — the pristine-run discipline is unchanged.
	bLeg := mixedSeedOuter("B")
	assertSpanWindowAgreement(t, "accept/multi-leg-prefix", values.NewRawRecordConstructorValue(
		bakeOrdinal(t, tLeg, 0), bakeOrdinal(t, tLeg, 1),
		bakeOrdinal(t, bLeg, 0), bakeOrdinal(t, bLeg, 1),
		values.RecordConstructorField{Name: "X", Value: scalarElem},
	), true)

	// ACCEPT: the ELEMENT-ANYWHERE accepts the generalization opened — the element
	// SPLITS the leg run, distinct legs on BOTH sides. This is the exact layout the
	// un-collapse's `FROM T, T.arr AS X, B GROUP BY X` (+ SUM(T.col)) rests on: the
	// walk windows the leading legs, steps over the element's 1-field slot, then
	// windows the trailing legs. Both walks MUST derive identical [T-window, element,
	// B-window] — this pins the accept boundary at the bit-for-bit unit level where the
	// invariant lives (an E2E test would only incidentally catch a drift here).
	assertSpanWindowAgreement(t, "accept/mid-list-element", values.NewRawRecordConstructorValue(
		bakeOrdinal(t, tLeg, 0), bakeOrdinal(t, tLeg, 1),
		values.RecordConstructorField{Name: "X", Value: scalarElem},
		bakeOrdinal(t, bLeg, 0), bakeOrdinal(t, bLeg, 1),
	), true)
	// ACCEPT: a LEADING element (element at position 0, a leg after) — the other new
	// element-anywhere boundary (`FROM T.arr AS X, B`). Both walks derive [element,
	// B-window].
	assertSpanWindowAgreement(t, "accept/leading-element", values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "X", Value: scalarElem},
		bakeOrdinal(t, bLeg, 0), bakeOrdinal(t, bLeg, 1),
	), true)

	// A single baked leg, no element: ordinalJoinSpansOf bails on len(spans) < 2,
	// unnestMixedSeedSpans on the non-QOV trailing field; values now bails on
	// len(windows) < 2. BOTH decline (a folded projection, not the pristine concat).
	assertSpanWindowAgreement(t, "decline/single-leg-pristine", values.NewRawRecordConstructorValue(
		bakeOrdinal(t, tLeg, 0), bakeOrdinal(t, tLeg, 1),
	), false)

	// A SPLIT RUN — a leg alias RECURS (`[A.0, B.0, A.0]`, 1-field legs). Each
	// 1-field run is trivially full-coverage, so the executor's run-LIST would
	// accept it without the seen-alias reject; values' run-MAP declines on the
	// dup-alias check. This pins the truly-bit-for-bit reject (the last drift
	// residual): BOTH decline now. Unreachable from SQL (a seed is a concat of
	// DISTINCT contiguous legs) but the invariant is that the two walks agree.
	a1 := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("A"),
		values.NewRecordType("", false, []values.Field{{Name: "AID", FieldType: values.NotNullLong, Ordinal: 0}}))
	b1 := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("B"),
		values.NewRecordType("", false, []values.Field{{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0}}))
	assertSpanWindowAgreement(t, "decline/split-run", values.NewRawRecordConstructorValue(
		bakeOrdinal(t, a1, 0), bakeOrdinal(t, b1, 0), bakeOrdinal(t, a1, 0),
	), false)

	// A RECORD-typed trailing bare QOV (a positional-merge RC's shape): the
	// non-record guard (isMixedSeedElement / unnestMixedSeedSpans) excludes it. BOTH decline.
	recElem := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("R"),
		values.NewRecordType("", false, []values.Field{{Name: "RC0", FieldType: values.NotNullLong, Ordinal: 0}}))
	assertSpanWindowAgreement(t, "decline/record-element", values.NewRawRecordConstructorValue(
		bakeOrdinal(t, tLeg, 0), bakeOrdinal(t, tLeg, 1),
		values.RecordConstructorField{Name: "R", Value: recElem},
	), false)

	// A lone element (no baked outer prefix): fewer than 2 fields / empty windows. BOTH decline.
	assertSpanWindowAgreement(t, "decline/lone-element", values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "X", Value: scalarElem},
	), false)
}

// TestSpanWindowCrossAgreement_BoxLeg pins the cross-agreement
// invariant for a seed whose leg is a CLUSTERED BOX (RecordType.Legs carries
// the buried-leaf boundaries): the executor's run-level spans emit EVERY sub
// of the box run into the merged type's Legs, and the values twin must land
// on the identical boundary list — including the RIGHTMOST leaf's entry
// under the box's own name (the box is NAMED by that leaf, so the
// alias-keyed window must be the LEAF's narrow slice, never the whole
// concat: a run-wide window FieldIndexes across the concat and first-matches
// an earlier buried leg's duplicate name). This shape once caught a real bug:
// values.OrdinalSeedLegWindows skipped a boundary whenever `leg.Name ==
// alias`, silently dropping the rightmost leaf's window in exactly this case.
func TestSpanWindowCrossAgreement_BoxLeg(t *testing.T) {
	t.Parallel()
	// Plain preserved leg A (2 cols) + a clustered box leg named E whose
	// concat buries B (2 cols, one name DUPLICATING an A column) before its
	// rightmost leaf E (1 col).
	aLeg := mixedSeedOuter("A")
	boxTyp := values.NewRecordType("", false, []values.Field{
		{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "AID", FieldType: values.NotNullLong, Ordinal: 1}, // dup of A's column name
		{Name: "EID", FieldType: values.NotNullLong, Ordinal: 2},
	})
	// A leg STATES its identity (Alias), not merely its text: identity is what the
	// window derivations compare, so a fixture that supplies only Name models a
	// pre-identity producer and would make this agreement check vacuous.
	boxTyp.Legs = []values.RecordTypeLeg{
		{Kind: values.LegKindFlatRun, Alias: values.NamedCorrelationIdentifier("B"), Name: "B", Start: 0, Width: 2},
		{Kind: values.LegKindFlatRun, Alias: values.NamedCorrelationIdentifier("E"), Name: "E", Start: 2, Width: 1},
	}
	eBox := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("E"), boxTyp)
	seed := values.NewRawRecordConstructorValue(
		bakeOrdinal(t, aLeg, 0), bakeOrdinal(t, aLeg, 1),
		bakeOrdinal(t, eBox, 0), bakeOrdinal(t, eBox, 1), bakeOrdinal(t, eBox, 2),
	)
	assertSpanWindowAgreement(t, "box-leg", seed, true)

	// The alias-keyed window is the rightmost LEAF's slice.
	windows, merged := values.OrdinalSeedLegWindows(seed)
	if w := windows[values.NamedCorrelationIdentifier("E")]; w.Offset != 4 || len(w.Typ.Fields) != 1 || w.Typ.Fields[0].Name != "EID" {
		t.Fatalf("windows[E] = (offset %d, %v) — want the rightmost LEAF window (offset 4, [EID])", w.Offset, w.Typ.Fields)
	}
	if w := windows[values.NamedCorrelationIdentifier("B")]; w.Offset != 2 || len(w.Typ.Fields) != 2 {
		t.Fatalf("windows[B] = (offset %d, width %d) — want the buried sub-window (offset 2, width 2)", w.Offset, len(w.Typ.Fields))
	}
	wantLegs := []values.RecordTypeLeg{
		{Kind: values.LegKindFlatRun, Alias: values.NamedCorrelationIdentifier("A"), Name: "A", Start: 0, Width: 2},
		{Kind: values.LegKindFlatRun, Alias: values.NamedCorrelationIdentifier("B"), Name: "B", Start: 2, Width: 2},
		{Kind: values.LegKindFlatRun, Alias: values.NamedCorrelationIdentifier("E"), Name: "E", Start: 4, Width: 1},
	}
	if len(merged.Legs) != len(wantLegs) {
		t.Fatalf("merged Legs = %v, want %v", merged.Legs, wantLegs)
	}
	for i, want := range wantLegs {
		if merged.Legs[i] != want {
			t.Fatalf("merged Legs[%d] = %+v, want %+v", i, merged.Legs[i], want)
		}
	}
}

// mergeSlotQOV is one collapsed lower quantifier of a positional merge: a bare
// QuantifiedObjectValue holding that quantifier's WHOLE row.
func mergeSlotQOV(alias string, cols ...string) *values.QuantifiedObjectValue {
	fields := make([]values.Field, len(cols))
	for i, c := range cols {
		fields[i] = values.Field{Name: c, FieldType: values.NotNullLong, Ordinal: i}
	}
	return values.NewQuantifiedObjectValueOfType(
		values.NamedCorrelationIdentifier(alias),
		values.NewRecordType("", false, fields))
}

// positionalMergeOf builds the merge row exactly as positionalMergeCase does:
// values.OrdinalFieldName(i) per slot, in position order, one bare QOV each.
func positionalMergeOf(slots ...values.Value) *values.RecordConstructorValue {
	fields := make([]values.RecordConstructorField, len(slots))
	for i, v := range slots {
		fields[i] = values.RecordConstructorField{Name: values.OrdinalFieldName(i), Value: v}
	}
	return values.NewRawRecordConstructorValue(fields...)
}

// TestSpanWindowCrossAgreement_NestedMerge is the NESTED matrix RFC-200 owes:
// the two walks must agree on (Alias, Kind, Offset, Typ) for a positional merge
// and DECLINE identically for every shape neither can address.
//
// It also pins the boundary that makes the whole design safe — the NARROW pair
// still declines every one of these. Two walks of the same wrong model agree
// perfectly, so agreement alone proves nothing about correctness; what it proves
// is that a change to one side cannot silently move only that side.
func TestSpanWindowCrossAgreement_NestedMerge(t *testing.T) {
	t.Parallel()

	a := mergeSlotQOV("A", "AID", "AV")
	b := mergeSlotQOV("B", "BID")
	// A merge slot that states NO type at all. The corpus has 750 of these, every
	// distinct witness an unnest ELEMENT alias whose array element type Go does
	// not infer that far. It keeps the ELEMENT treatment — a synthesized 1-field
	// flat window — and the reason this fixture exists is the trap it guards:
	// routing the per-slot test through IsMixedSeedElementType would classify a
	// nil-typed slot as NESTED (that predicate answers false for nil), yielding a
	// window with a nil Typ that panics at the first FieldIndex.
	untyped := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("U"))

	for _, tc := range []struct {
		name       string
		rc         *values.RecordConstructorValue
		wantAccept bool
	}{
		{
			name:       "a pristine 2-slot positional merge",
			rc:         positionalMergeOf(a, b),
			wantAccept: true,
		},
		{
			name:       "a MIXED merge: two record slots and one untyped element slot",
			rc:         positionalMergeOf(a, untyped, b),
			wantAccept: true,
		},
		{
			// IsPositionalMergeRC requires the auto-generated `_i` names in POSITION
			// ORDER. A named 2-slot RC of bare typed leg QOVs is shape-adjacent and is
			// NOT a merge row — the distinction is what keeps the nested branch keyed
			// on the full structural recognizer rather than on the looser "every field
			// is a bare typed QOV", which is a pin this repo already carries because
			// admitting such a constructor once gave multi-column legs one-column
			// windows.
			name: "a NAMED 2-slot RC of bare typed leg QOVs is not a merge row",
			rc: values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "A", Value: a},
				values.RecordConstructorField{Name: "B", Value: b},
			),
			wantAccept: false,
		},
		{
			// Same quantifier twice: IsPositionalMergeRC requires DISTINCT ones,
			// because two windows filed under one identity is one window and a
			// silently dropped leg.
			name:       "a merge row over the SAME quantifier twice",
			rc:         positionalMergeOf(a, a),
			wantAccept: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertSpanWindowAgreementNested(t, tc.name, tc.rc, tc.wantAccept)
			// THE NARROW PAIR IS FROZEN. Every shape above declines on both narrow
			// walks, accepted or not by the nested pair — which is what makes the
			// nested acceptance a separate opt-in rather than a widening, and what
			// makes the per-consumer proof for the narrow entry's other call sites
			// hold: their answer is unchanged on every input they can see.
			assertSpanWindowAgreement(t, tc.name+" [narrow]", tc.rc, false)
		})
	}
}

// The nested windows the merge row yields, checked by VALUE rather than only for
// agreement.
//
// Cross-agreement is necessary and not sufficient: two walks of the same wrong
// model agree perfectly. This states what the model actually has to be — Offset
// is the SLOT INDEX, Typ is the leg's OWN record type by EXTRACTION, and Width
// on the merged leg table is 1.
func TestNestedMergeWindows_OffsetIsTheSlotAndTypIsTheLegsOwnRow(t *testing.T) {
	t.Parallel()

	a := mergeSlotQOV("A", "AID", "AV")
	b := mergeSlotQOV("B", "BID")
	rc := positionalMergeOf(a, b)

	windows, merged := values.OrdinalSeedLegWindowsAcceptingNested(rc)
	if windows == nil {
		t.Fatal("the nested entry must accept a positional merge row")
	}

	wa := windows[values.NamedCorrelationIdentifier("A")]
	if wa.Kind != values.LegKindNested || wa.Offset != 0 {
		t.Fatalf("leg A window = (kind %v, offset %d), want (nested, 0) — Offset is the "+
			"FIELD INDEX of the slot holding the whole leg row, not the first column of "+
			"a run", wa.Kind, wa.Offset)
	}
	// Typ is an EXTRACTION, not a one-field wrapper describing the slot. A wrapper
	// would decline every leg-local ordinal >= 1 at the readers' bound check and
	// resolve ordinal 0 against the wrapper — a silent wrong-column read on
	// exactly the shape that check exists to catch.
	if len(wa.Typ.Fields) != 2 || wa.Typ.Fields[1].Name != "AV" {
		t.Fatalf("leg A window Typ = %v, want the LEG's own 2-column row [AID AV]. A "+
			"one-field wrapper here turns the readers' leg-local bound check into a "+
			"silent wrong-column read.", wa.Typ.Fields)
	}
	wb := windows[values.NamedCorrelationIdentifier("B")]
	if wb.Kind != values.LegKindNested || wb.Offset != 1 || len(wb.Typ.Fields) != 1 {
		t.Fatalf("leg B window = (kind %v, offset %d, %d cols), want (nested, 1, 1) — "+
			"slot 1 holds B's WHOLE row, so the offset does not advance by A's column "+
			"count", wb.Kind, wb.Offset, len(wb.Typ.Fields))
	}

	if len(merged.Fields) != 2 {
		t.Fatalf("the merged row has %d fields, want 2 — one SLOT per collapsed "+
			"quantifier, which is what makes nesting free", len(merged.Fields))
	}
	for _, leg := range merged.Legs {
		if leg.Kind != values.LegKindNested || leg.Width != 1 {
			t.Fatalf("merged leg %s = (kind %v, width %d), want (nested, 1). Width is a "+
				"SLOT count: every consumer computes Start+Width as a slot range into the "+
				"carrying type's Fields, so a nested leg claiming its column count would "+
				"put its range over its neighbours.", leg.Name, leg.Kind, leg.Width)
		}
	}
}

// nestedSubLegSeed builds the shape the OPT-IN SITES ACTUALLY RECEIVE: a
// pristine flat seed whose leg RUN carries a LegKindNested boundary in its
// Typ.Legs.
//
// This is RFC-200 §1(b), and it is the live path. §1(a) — the whole-RC
// positional merge handed to the derivation directly — is what the head
// recognizer accepts, but no opt-in site ever passes one: all three read
// `step1RV`, which reconstructFoldStep1Seed builds as a FLAT concat of
// `ofOrdinal(QOV(leg, rt), j)` per slot. The merge only appears as `rt` — the
// leg's own row type — and therefore only ever reaches the layout authority as a
// nested boundary inside a carrying run's leg table.
//
// The distinction matters because the two enter the derivation at completely
// different places: §1(a) at positionalMergeWindows, §1(b) at
// finalizeSeedWindows' sub-window loop. A matrix that only covered §1(a) tested
// the branch the corpus does not take.
//
// Leg A is a plain 2-column run. Leg M is a 3-slot run whose slot 1 holds a
// whole 2-column sub-leg row, declared nested in M's leg table.
func nestedSubLegSeed(t *testing.T) (*values.RecordConstructorValue, *values.RecordType) {
	t.Helper()
	subRow := values.NewRecordType("", false, []values.Field{
		{Name: "SID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "SV", FieldType: values.NotNullLong, Ordinal: 1},
	})
	mRow := values.NewRecordType("", false, []values.Field{
		{Name: "M0", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: values.OrdinalFieldName(1), FieldType: subRow, Ordinal: 1},
		{Name: "M2", FieldType: values.NotNullLong, Ordinal: 2},
	})
	mRow.Legs = []values.RecordTypeLeg{
		values.NewRecordTypeLeg(values.LegKindNested,
			values.NamedCorrelationIdentifier("S"), "S", 1, 1),
	}
	a := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("A"),
		values.NewRecordType("", false, []values.Field{
			{Name: "AID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "AV", FieldType: values.NotNullLong, Ordinal: 1},
		}))
	m := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("M"), mRow)
	return values.NewRawRecordConstructorValue(
		bakeOrdinal(t, a, 0), bakeOrdinal(t, a, 1),
		bakeOrdinal(t, m, 0), bakeOrdinal(t, m, 1), bakeOrdinal(t, m, 2),
	), subRow
}

// TestSpanWindowCrossAgreement_NestedSubLeg is RFC-200 §1(b)'s matrix — the
// shape the three opt-in sites actually receive.
func TestSpanWindowCrossAgreement_NestedSubLeg(t *testing.T) {
	t.Parallel()
	seed, subRow := nestedSubLegSeed(t)

	// Both nested-accepting walks agree, and both narrow walks DECLINE — the
	// narrow entry's fail-closed refusal of a seed carrying a nested leg.
	assertSpanWindowAgreementNested(t, "nested sub-leg", seed, true)
	assertSpanWindowAgreement(t, "nested sub-leg [narrow]", seed, false)

	windows, merged, runs := values.OrdinalSeedLegLayout(seed)
	if windows == nil {
		t.Fatal("the nested entry must accept a seed whose leg run carries a nested boundary")
	}
	// TWO tiles: leg A at 0, the carrying run M at 2. The nested sub-window is
	// ADDED beside them, not a tile.
	if len(runs) != 2 {
		t.Fatalf("the seed reports %d tiles, want 2 — a nested SUB-window is addressable "+
			"but does not tile the row", len(runs))
	}

	// THE SUB-WINDOW, by value. Offset is the carrying run's offset plus the
	// leg's slot — 2 + 1 — and Typ is the sub-leg's OWN row by EXTRACTION.
	ws := windows[values.NamedCorrelationIdentifier("S")]
	if ws.Kind != values.LegKindNested || ws.Offset != 3 {
		t.Fatalf("sub-window S = (kind %v, offset %d), want (nested, 3) = run offset 2 + "+
			"slot 1", ws.Kind, ws.Offset)
	}
	if len(ws.Typ.Fields) != 2 || ws.Typ.Fields[1].Name != "SV" {
		t.Fatalf("sub-window S Typ = %v, want the SUB-LEG's own 2-column row. An "+
			"EXTRACTION, not a one-field wrapper describing the slot: a wrapper declines "+
			"every leg-local ordinal >= 1 at the readers' bound check and resolves "+
			"ordinal 0 against itself.", ws.Typ.Fields)
	}
	if ws.Typ != subRow {
		t.Fatalf("sub-window S Typ is not the declared sub-row — it was rebuilt rather " +
			"than extracted, which is how a slice-shaped copy creeps back in")
	}

	// THE MERGED LEG TABLE, which the executor's runtime binders read. The nested
	// leg's Width is 1: a SLOT count, because every consumer computes Start+Width
	// as a slot range into the carrying type's Fields.
	var found bool
	for _, leg := range merged.Legs {
		if leg.Alias != values.NamedCorrelationIdentifier("S") {
			continue
		}
		found = true
		if leg.Kind != values.LegKindNested || leg.Start != 3 || leg.Width != 1 {
			t.Fatalf("merged leg S = (kind %v, start %d, width %d), want (nested, 3, 1)",
				leg.Kind, leg.Start, leg.Width)
		}
	}
	if !found {
		t.Fatal("the merged leg table has no entry for the nested sub-leg S — the " +
			"executor's runtime binders read this table, so a missing entry is a leg " +
			"that cannot be bound at all")
	}
}

// The NONDETERMINISM guard (RFC-200 G-F4): a sub-leg whose own row carries leg
// boundaries must decline DETERMINISTICALLY.
//
// finalizeSeedWindows RANGES the map it inserts into, and Go's spec leaves it
// unspecified whether an entry added during iteration is visited. Without the
// check at the INSERTION, the head-of-loop guard would decline this seed on runs
// where the range happens to reach the inserted sub-window and accept it on runs
// where it does not — accept/decline decided by map iteration order.
//
// Run repeatedly on purpose: one iteration cannot observe an order-dependent
// answer, and this is the one defect class where a single green run is not
// evidence.
func TestNestedSubLeg_ALegsCarryingSubRowDeclinesDeterministically(t *testing.T) {
	t.Parallel()

	subRow := values.NewRecordType("", false, []values.Field{
		{Name: "SID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "SV", FieldType: values.NotNullLong, Ordinal: 1},
	})
	// The sub-leg's OWN row carries a leg table — two steps from the merged row,
	// which neither walk can express.
	subRow.Legs = []values.RecordTypeLeg{
		values.NewRecordTypeLeg(values.LegKindFlatRun,
			values.NamedCorrelationIdentifier("DEEP"), "DEEP", 0, 2),
	}
	mRow := values.NewRecordType("", false, []values.Field{
		{Name: "M0", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: values.OrdinalFieldName(1), FieldType: subRow, Ordinal: 1},
		{Name: "M2", FieldType: values.NotNullLong, Ordinal: 2},
	})
	mRow.Legs = []values.RecordTypeLeg{
		values.NewRecordTypeLeg(values.LegKindNested,
			values.NamedCorrelationIdentifier("S"), "S", 1, 1),
	}
	a := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("A"),
		values.NewRecordType("", false, []values.Field{
			{Name: "AID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "AV", FieldType: values.NotNullLong, Ordinal: 1},
		}))
	m := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("M"), mRow)
	seed := values.NewRawRecordConstructorValue(
		bakeOrdinal(t, a, 0), bakeOrdinal(t, a, 1),
		bakeOrdinal(t, m, 0), bakeOrdinal(t, m, 1), bakeOrdinal(t, m, 2),
	)

	for i := 0; i < 200; i++ {
		if w, _ := values.OrdinalSeedLegWindowsAcceptingNested(seed); w != nil {
			t.Fatalf("iteration %d ACCEPTED a seed whose nested sub-leg carries its own "+
				"leg table.\n"+
				"  Those boundaries are TWO steps from the merged row and this authority "+
				"has no two-step window for them, so the honest answer is no ordinal "+
				"layout at all.\n"+
				"  If this fails on SOME iterations and not others, the decline has moved "+
				"back to the head of finalizeSeedWindows' loop — which ranges the map it "+
				"inserts into, so whether the inserted sub-window is visited is "+
				"unspecified and ACCEPT/DECLINE becomes map-iteration-order dependent. "+
				"That is nondeterministic PLANNING: the same query gets different plans "+
				"on different runs.", i)
		}
		// The executor twin must refuse it too, on every iteration.
		if _, _, ok := ordinalJoinSpansAcceptingNested(seed); ok {
			t.Fatalf("iteration %d: the executor twin ACCEPTED what the planner declined", i)
		}
	}
}

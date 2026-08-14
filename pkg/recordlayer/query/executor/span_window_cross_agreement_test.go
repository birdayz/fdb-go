package executor

// Executor pins for span derivation over a lateral-unnest seed whose element
// is MIXED into the merged row (carried through a partition collapse), and
// for the NLJ build's behavior when it has no legRVs to derive windows from.

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func spanMustQOV(t testing.TB, alias string, typ values.Type) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(alias), typ)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue(%s): %v", alias, err)
	}
	return qov
}

func spanMustSeedValue(t testing.TB, child values.Value, ordinal int) values.Value {
	t.Helper()
	field, err := values.ResolveOrdinalSeedField(child, ordinal)
	if err != nil {
		t.Fatalf("ResolveOrdinalSeedField(%d): %v", ordinal, err)
	}
	return field
}

func spanFusedRef(t testing.TB, mergeQOV values.QuantifiedObjectValue, slot, legOrd int) values.FieldValue {
	t.Helper()
	resolved, err := values.ResolveFieldOrdinals(mergeQOV, []int{slot, legOrd})
	if err != nil {
		t.Fatalf("ResolveFieldOrdinals([%d,%d]): %v", slot, legOrd, err)
	}
	field, ok := values.AsFieldValue(resolved)
	if !ok || field.Path() == nil || field.Path().Len() != 2 || field.Path().IsFrontierPinned() {
		t.Fatalf("fused reference = %T path %v, want exact unpinned two-accessor FieldValue", resolved, field)
	}
	return field
}

// TestMixedElementSpanSynthesis pins the RFC-232 replacement for inferred
// mixed-element spans. The phase's explicit OrdinalLayout retains the source
// record field-by-field and the scalar UNNEST element by object path. Omitting
// the element window is loud even though a same-typed carrier slot exists, and
// claiming that scalar slot for a record source is rejected at construction.
func TestMixedElementSpanSynthesis(t *testing.T) {
	t.Parallel()

	legS := values.NewRecordType("", false, []values.Field{
		{Name: "SID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "ARR", FieldType: values.NotNullLong, Ordinal: 1},
	})
	qovS := spanMustQOV(t, "S", legS)
	elemQOV := spanMustQOV(t, "EL", values.NotNullLong)
	carrierType := values.NewRecordType("", false, []values.Field{
		{Name: "SID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "ARR", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "EL", FieldType: values.NotNullLong, Ordinal: 2},
	})
	tiles := []values.OrdinalTileSpec{{Start: 0, Width: 3, Kind: values.OrdinalTileFlat}}
	sourceWindow := values.OrdinalWindowSpec{Source: qovS, FieldPaths: [][]int{{0}, {1}}}
	elementWindow := values.OrdinalWindowSpec{Source: elemQOV, ObjectPath: []int{2}}
	newLayout := func(windows []values.OrdinalWindowSpec) values.OrdinalLayout {
		t.Helper()
		layout, err := values.NewOrdinalLayoutForCarrierType(carrierType, tiles, windows)
		if err != nil {
			t.Fatalf("NewOrdinalLayoutForCarrierType: %v", err)
		}
		return layout
	}
	rowFor := func(layout values.OrdinalLayout) *PositionalRow {
		t.Helper()
		row, err := NewLayoutPositionalRow(carrierType, layout)
		if err != nil {
			t.Fatalf("NewLayoutPositionalRow: %v", err)
		}
		copy(row.Slots, []any{int64(7), int64(70), int64(99)})
		return row
	}

	full := newLayout([]values.OrdinalWindowSpec{sourceWindow, elementWindow})
	ctx, err := ordinalLayoutRowContext(full, rowFor(full), nil, nil)
	if err != nil {
		t.Fatalf("ordinalLayoutRowContext: %v", err)
	}
	arr := mustTestFieldOrdinal(t, qovS, 1)
	if got, evalErr := arr.Evaluate(ctx); evalErr != nil || got != int64(70) {
		t.Fatalf("S.ARR = (%v, %v), want (70, nil)", got, evalErr)
	}
	if got, evalErr := elemQOV.Evaluate(ctx); evalErr != nil || got != int64(99) {
		t.Fatalf("EL object = (%v, %v), want (99, nil)", got, evalErr)
	}

	// Mutation control: with no EL window, the exact object binder must not
	// fall back to carrier slot 2 (or another same-typed slot).
	sourceOnly := newLayout([]values.OrdinalWindowSpec{sourceWindow})
	sourceOnlyCtx, err := ordinalLayoutRowContext(sourceOnly, rowFor(sourceOnly), nil, nil)
	if err != nil {
		t.Fatalf("source-only ordinalLayoutRowContext: %v", err)
	}
	got, evalErr := elemQOV.Evaluate(sourceOnlyCtx)
	var coded interface {
		Code() values.ResolutionErrorCode
	}
	if got != nil || !errors.As(evalErr, &coded) || coded.Code() != values.UnboundCorrelation {
		t.Fatalf("EL without an explicit window = (%v, %v), want UnboundCorrelation", got, evalErr)
	}

	// Exact-shape control: the scalar EL slot cannot be declared as a whole
	// record object merely because an old span synthesizer would have guessed a
	// one-field wrapper.
	recordElem := spanMustQOV(t, "REC_EL", legS)
	invalid, invalidErr := values.NewOrdinalLayoutForCarrierType(carrierType, tiles, []values.OrdinalWindowSpec{
		sourceWindow,
		{Source: recordElem, ObjectPath: []int{2}},
	})
	if invalid != nil || invalidErr == nil {
		t.Fatalf("record source over scalar EL slot = (%v, %v), want exact layout rejection", invalid, invalidErr)
	}
	var invalidCode interface {
		Code() values.ResolutionErrorCode
	}
	if !errors.As(invalidErr, &invalidCode) || invalidCode.Code() != values.LayoutTypeMismatch {
		t.Fatalf("record-over-scalar error = %v, want LayoutTypeMismatch", invalidErr)
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
	mergeQOV := spanMustQOV(t, "m", mergedType)
	legB := values.NewRecordType("", false, []values.Field{
		{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	qovB := spanMustQOV(t, "B", legB)
	b0 := spanMustSeedValue(t, qovB, 0)
	top := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "SID", Value: spanFusedRef(t, mergeQOV, 0, 0)},
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
	mk := func(alias string, cols ...string) values.QuantifiedObjectValue {
		fields := make([]values.Field, len(cols))
		for i, c := range cols {
			fields[i] = values.Field{Name: c, FieldType: values.NotNullLong, Ordinal: i}
		}
		return spanMustQOV(t, alias, values.NewRecordType("", false, fields))
	}
	bake := func(qov values.QuantifiedObjectValue, i int) values.RecordConstructorField {
		return bakeOrdinal(t, qov, i)
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
		{Name: values.OrdinalFieldName(0), FieldType: a.FlowedType(), Ordinal: 0},
	})
	mergeQOV := spanMustQOV(t, "m", mergedType)
	nonPristine := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "AID", Value: spanFusedRef(t, mergeQOV, 0, 0)},
		values.RecordConstructorField{Name: "AV", Value: spanFusedRef(t, mergeQOV, 0, 1)},
	)
	_, _, spansOK := ordinalJoinSpans(nonPristine)
	winDeclined, _ := values.OrdinalSeedLegWindows(nonPristine)
	if spansOK || winDeclined != nil {
		t.Fatalf("both authorities must DECLINE the fused top: spans ok=%v windows derived=%v", spansOK, winDeclined != nil)
	}
}

// mixedSeedOuter builds a 2-column baked outer leg QOV keyed by the given alias.
func mixedSeedOuter(t testing.TB, alias string) values.QuantifiedObjectValue {
	t.Helper()
	return spanMustQOV(t, alias, values.NewRecordType("", false, []values.Field{
		{Name: alias + "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: alias + "V", FieldType: values.NotNullLong, Ordinal: 1},
	}))
}

// bakeOrdinal bakes a frontier-pinned ofOrdinal field over a leg QOV.
func bakeOrdinal(t testing.TB, qov values.QuantifiedObjectValue, i int) values.RecordConstructorField {
	t.Helper()
	value := spanMustSeedValue(t, qov, i)
	fv, ok := values.AsFieldValue(value)
	if !ok {
		t.Fatalf("ResolveOrdinalSeedField(%s,%d) = %T, want exact FieldValue", qov.Correlation(), i, value)
	}
	return values.RecordConstructorField{Name: fv.DisplayName(), Value: fv}
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
	tLeg := mixedSeedOuter(t, "T")

	// ACCEPT: multi-col outer + a bare-QOV scalar element (the whole-object element
	// the seed cannot ofOrdinal-bake), keyed by the AS alias X.
	scalarElem := spanMustQOV(t, "X", values.NotNullLong)
	assertSpanWindowAgreement(t, "mixed/scalar-element", values.NewRawRecordConstructorValue(
		bakeOrdinal(t, tLeg, 0), bakeOrdinal(t, tLeg, 1),
		values.RecordConstructorField{Name: "X", Value: scalarElem},
	), true)

	// ACCEPT: an exact opaque collection element is still NON-record, so it flows
	// the same mixed path as a scalar (isMixedSeedElement).
	structElem := spanMustQOV(t, "W", values.NewArrayType(false, values.NotNullLong))
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
	bLeg := mixedSeedOuter(t, "B")
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
	a1 := spanMustQOV(t, "A",
		values.NewRecordType("", false, []values.Field{{Name: "AID", FieldType: values.NotNullLong, Ordinal: 0}}))
	b1 := spanMustQOV(t, "B",
		values.NewRecordType("", false, []values.Field{{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0}}))
	assertSpanWindowAgreement(t, "decline/split-run", values.NewRawRecordConstructorValue(
		bakeOrdinal(t, a1, 0), bakeOrdinal(t, b1, 0), bakeOrdinal(t, a1, 0),
	), false)

	// A RECORD-typed trailing bare QOV (a positional-merge RC's shape): the
	// non-record guard (isMixedSeedElement / unnestMixedSeedSpans) excludes it. BOTH decline.
	recElem := spanMustQOV(t, "R",
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

// TestSpanWindowCrossAgreement_BoxLeg pins agreement between the explicit
// retained-result layout factory and an independently stated executor layout
// for the old clustered-box shape. A, buried B, and rightmost E are separate
// exact sources; no RecordType.Legs table participates. E's field window must
// name carrier slot 4, never B's earlier duplicate at slot 3.
func TestSpanWindowCrossAgreement_BoxLeg(t *testing.T) {
	t.Parallel()
	aType := values.NewRecordType("", false, []values.Field{
		{Name: "AID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "AV", FieldType: values.NotNullLong, Ordinal: 1},
	})
	bType := values.NewRecordType("", false, []values.Field{
		{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1},
	})
	eType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	a := spanMustQOV(t, "A", aType)
	b := spanMustQOV(t, "B", bType)
	e := spanMustQOV(t, "E", eType)
	seed := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "AID", Value: mustTestFieldOrdinal(t, a, 0)},
		values.RecordConstructorField{Name: "AV", Value: mustTestFieldOrdinal(t, a, 1)},
		values.RecordConstructorField{Name: "BID", Value: mustTestFieldOrdinal(t, b, 0)},
		values.RecordConstructorField{Name: "ID", Value: mustTestFieldOrdinal(t, b, 1)},
		values.RecordConstructorField{Name: "ID", Value: mustTestFieldOrdinal(t, e, 0)},
	)
	derived, err := values.NewFlatOrdinalLayoutForRetainedResult(seed, nil)
	if err != nil {
		t.Fatalf("NewFlatOrdinalLayoutForRetainedResult: %v", err)
	}
	carrierType := seed.Type().(*values.RecordType)
	explicit, err := values.NewOrdinalLayoutForCarrierType(
		carrierType,
		[]values.OrdinalTileSpec{{Start: 0, Width: 5, Kind: values.OrdinalTileFlat}},
		[]values.OrdinalWindowSpec{
			{Source: a, FieldPaths: [][]int{{0}, {1}}},
			{Source: b, FieldPaths: [][]int{{2}, {3}}},
			{Source: e, FieldPaths: [][]int{{4}}},
		},
	)
	if err != nil {
		t.Fatalf("explicit box replacement layout: %v", err)
	}
	if !derived.RawEqual(explicit) {
		t.Fatal("retained-result and explicit executor layouts disagree on the A/B/E field windows")
	}
	if len(carrierType.Legs) != 0 {
		t.Fatalf("fixture accidentally restored legacy RecordType.Legs authority: %v", carrierType.Legs)
	}

	row, err := NewLayoutPositionalRow(carrierType, explicit)
	if err != nil {
		t.Fatalf("NewLayoutPositionalRow: %v", err)
	}
	copy(row.Slots, []any{int64(10), int64(11), int64(20), int64(21), int64(30)})
	ctx, err := ordinalLayoutRowContext(explicit, row, nil, nil)
	if err != nil {
		t.Fatalf("ordinalLayoutRowContext: %v", err)
	}
	if got, evalErr := mustTestFieldOrdinal(t, e, 0).Evaluate(ctx); evalErr != nil || got != int64(30) {
		t.Fatalf("E.ID = (%v, %v), want rightmost slot 4 value 30, never buried B.ID=21", got, evalErr)
	}
	if got, evalErr := mustTestFieldOrdinal(t, b, 1).Evaluate(ctx); evalErr != nil || got != int64(21) {
		t.Fatalf("B.ID = (%v, %v), want buried source slot 3 value 21", got, evalErr)
	}
}

// mergeSlotQOV is one collapsed lower quantifier of a positional merge: a bare
// QuantifiedObjectValue holding that quantifier's WHOLE row.
func mergeSlotQOV(t testing.TB, alias string, cols ...string) values.QuantifiedObjectValue {
	t.Helper()
	fields := make([]values.Field, len(cols))
	for i, c := range cols {
		fields[i] = values.Field{Name: c, FieldType: values.NotNullLong, Ordinal: i}
	}
	return spanMustQOV(t, alias, values.NewRecordType("", false, fields))
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

	a := mergeSlotQOV(t, "A", "AID", "AV")
	b := mergeSlotQOV(t, "B", "BID")
	// An exact non-record slot keeps the ELEMENT treatment — a synthesized
	// one-field flat window — while RFC-232 prevents an unresolved QOV from being
	// published into this machinery in the first place.
	element := spanMustQOV(t, "U", values.NotNullLong)

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
			name:       "a MIXED merge: two record slots and one scalar element slot",
			rc:         positionalMergeOf(a, element, b),
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

	a := mergeSlotQOV(t, "A", "AID", "AV")
	b := mergeSlotQOV(t, "B", "BID")
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

// TestSpanWindowCrossAgreement_NestedSubLeg pins the RFC-232 nested layout
// directly. The carrier contains A's two flat fields and M's three fields, one
// of which is a nested S row. Tiles describe the physical nesting; source
// windows independently retain A, M, and S. No boundary is inferred from a
// logical RecordType side table.
func TestSpanWindowCrossAgreement_NestedSubLeg(t *testing.T) {
	t.Parallel()
	subRow := values.NewRecordType("", false, []values.Field{
		{Name: "SID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "SV", FieldType: values.NotNullLong, Ordinal: 1},
	})
	aType := values.NewRecordType("", false, []values.Field{
		{Name: "AID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "AV", FieldType: values.NotNullLong, Ordinal: 1},
	})
	mType := values.NewRecordType("", false, []values.Field{
		{Name: "M0", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: values.OrdinalFieldName(1), FieldType: subRow, Ordinal: 1},
		{Name: "M2", FieldType: values.NotNullLong, Ordinal: 2},
	})
	carrierType := values.NewRecordType("", false, []values.Field{
		{Name: "AID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "AV", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "M0", FieldType: values.NotNullLong, Ordinal: 2},
		{Name: values.OrdinalFieldName(1), FieldType: subRow, Ordinal: 3},
		{Name: "M2", FieldType: values.NotNullLong, Ordinal: 4},
	})
	a := spanMustQOV(t, "A", aType)
	m := spanMustQOV(t, "M", mType)
	s := spanMustQOV(t, "S", subRow)
	tiles := []values.OrdinalTileSpec{
		{Start: 0, Width: 3, Kind: values.OrdinalTileFlat},
		{Start: 3, Width: 1, Kind: values.OrdinalTileNested},
		{Start: 4, Width: 1, Kind: values.OrdinalTileFlat},
		{Parent: []int{3}, Start: 0, Width: 2, Kind: values.OrdinalTileFlat},
	}
	windows := []values.OrdinalWindowSpec{
		{Source: a, FieldPaths: [][]int{{0}, {1}}},
		{Source: m, FieldPaths: [][]int{{2}, {3}, {4}}},
		{Source: s, ObjectPath: []int{3}},
	}
	layout, err := values.NewOrdinalLayoutForCarrierType(carrierType, tiles, windows)
	if err != nil {
		t.Fatalf("nested NewOrdinalLayoutForCarrierType: %v", err)
	}
	if len(carrierType.Legs) != 0 || len(mType.Legs) != 0 || len(subRow.Legs) != 0 {
		t.Fatal("fixture restored legacy RecordType.Legs instead of declaring an OrdinalLayout")
	}

	nested := NewPositionalRow(subRow)
	copy(nested.Slots, []any{int64(30), int64(31)})
	row, err := NewLayoutPositionalRow(carrierType, layout)
	if err != nil {
		t.Fatalf("NewLayoutPositionalRow: %v", err)
	}
	copy(row.Slots, []any{int64(1), int64(2), int64(20), nested, int64(22)})
	ctx, err := ordinalLayoutRowContext(layout, row, nil, nil)
	if err != nil {
		t.Fatalf("ordinalLayoutRowContext: %v", err)
	}
	if got, evalErr := mustTestFieldOrdinal(t, s, 1).Evaluate(ctx); evalErr != nil || got != int64(31) {
		t.Fatalf("S.SV = (%v, %v), want nested slot [3,1] value 31", got, evalErr)
	}
	mNestedSID, err := values.ResolveFieldOrdinals(m, []int{1, 0})
	if err != nil {
		t.Fatalf("resolve M._1.SID: %v", err)
	}
	if got, evalErr := mNestedSID.Evaluate(ctx); evalErr != nil || got != int64(30) {
		t.Fatalf("M._1.SID = (%v, %v), want nested field retained through M window", got, evalErr)
	}

	// Mutation control: retaining M's nested value does not implicitly bind S.
	// Source identity comes only from the explicit S window.
	withoutS, err := values.NewOrdinalLayoutForCarrierType(carrierType, tiles, windows[:2])
	if err != nil {
		t.Fatalf("layout without S: %v", err)
	}
	withoutSRow, err := NewLayoutPositionalRow(carrierType, withoutS)
	if err != nil {
		t.Fatalf("row without S: %v", err)
	}
	copy(withoutSRow.Slots, row.Slots)
	withoutSCtx, err := ordinalLayoutRowContext(withoutS, withoutSRow, nil, nil)
	if err != nil {
		t.Fatalf("context without S: %v", err)
	}
	got, evalErr := mustTestFieldOrdinal(t, s, 1).Evaluate(withoutSCtx)
	var coded interface {
		Code() values.ResolutionErrorCode
	}
	if got != nil || !errors.As(evalErr, &coded) || coded.Code() != values.UnboundCorrelation {
		t.Fatalf("S without its explicit window = (%v, %v), want UnboundCorrelation", got, evalErr)
	}
}

// TestNestedSubLeg_ALegsCarryingSubRowDeclinesDeterministically retains the
// legacy regression name while pinning its RFC-232 disposition. Two-level
// nesting is representable when every physical partition and source window is
// explicit. An incomplete layout is rejected deterministically at construction;
// there is no RecordType.Legs map walk whose iteration order can alter the
// answer.
func TestNestedSubLeg_ALegsCarryingSubRowDeclinesDeterministically(t *testing.T) {
	t.Parallel()

	deepRow := values.NewRecordType("", false, []values.Field{
		{Name: "D", FieldType: values.NotNullLong, Ordinal: 0},
	})
	subRow := values.NewRecordType("", false, []values.Field{
		{Name: "SID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "DEEP", FieldType: deepRow, Ordinal: 1},
	})
	carrierType := values.NewRecordType("", false, []values.Field{
		{Name: "SUB", FieldType: subRow, Ordinal: 0},
	})
	deep := spanMustQOV(t, "DEEP", deepRow)
	deepWindow := []values.OrdinalWindowSpec{
		{Source: deep, ObjectPath: []int{0, 1}},
	}

	// Mutation control: declaring the outer nested tile without the required
	// child partitions is never repaired or inferred from logical type metadata.
	incomplete := []values.OrdinalTileSpec{
		{Start: 0, Width: 1, Kind: values.OrdinalTileNested},
	}
	for i := 0; i < 200; i++ {
		layout, layoutErr := values.NewOrdinalLayoutForCarrierType(carrierType, incomplete, deepWindow)
		var coded interface {
			Code() values.ResolutionErrorCode
		}
		if layout != nil || !errors.As(layoutErr, &coded) || coded.Code() != values.LayoutTileGap {
			t.Fatalf("iteration %d incomplete nested layout = (%v, %v), want nil LayoutTileGap", i, layout, layoutErr)
		}
	}

	complete := []values.OrdinalTileSpec{
		{Start: 0, Width: 1, Kind: values.OrdinalTileNested},
		{Parent: []int{0}, Start: 0, Width: 1, Kind: values.OrdinalTileFlat},
		{Parent: []int{0}, Start: 1, Width: 1, Kind: values.OrdinalTileNested},
		{Parent: []int{0, 1}, Start: 0, Width: 1, Kind: values.OrdinalTileFlat},
	}
	layout, err := values.NewOrdinalLayoutForCarrierType(carrierType, complete, deepWindow)
	if err != nil {
		t.Fatalf("complete two-level nested layout: %v", err)
	}
	if len(carrierType.Legs) != 0 || len(subRow.Legs) != 0 || len(deepRow.Legs) != 0 {
		t.Fatal("fixture restored legacy RecordType.Legs authority")
	}
	deepValue := NewPositionalRow(deepRow)
	deepValue.Set(0, int64(42))
	subValue := NewPositionalRow(subRow)
	subValue.Set(0, int64(7))
	subValue.Set(1, deepValue)
	carrier, err := NewLayoutPositionalRow(carrierType, layout)
	if err != nil {
		t.Fatalf("NewLayoutPositionalRow: %v", err)
	}
	carrier.Set(0, subValue)
	ctx, err := ordinalLayoutRowContext(layout, carrier, nil, nil)
	if err != nil {
		t.Fatalf("ordinalLayoutRowContext: %v", err)
	}
	if got, evalErr := deep.Evaluate(ctx); evalErr != nil || got != deepValue {
		t.Fatalf("DEEP object = (%v, %v), want exact row at path [0,1]", got, evalErr)
	}
	if got, evalErr := mustTestFieldOrdinal(t, deep, 0).Evaluate(ctx); evalErr != nil || got != int64(42) {
		t.Fatalf("DEEP.D = (%v, %v), want two-level nested value 42", got, evalErr)
	}
}

package executor

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// --- fixtures --------------------------------------------------------------

// ojLegTypeAV is leg A's type [ID, V]; ojLegTypeBW is leg B's type [ID, W].
// The duplicated ID across legs is deliberate: two joined tables can both have
// an "ID" column, and the ordinal join must keep both as distinct slots by
// position rather than colliding on the name — the axis every wrong-slot
// test below turns on.
func ojLegTypeAV() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
	})
}

func ojLegTypeBW() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "W", FieldType: values.NotNullLong, Ordinal: 1},
	})
}

// buildOrdinalJoinRC builds the canonical ordinal join RC over the given typed
// leg QOVs — the exact shape the planner's translator emits for a join seed:
// for each leg, in order, ofOrdinal(QOV(leg), 0..n-1), field name = the leg
// column name, duplicates across legs preserved verbatim
// (NewRawRecordConstructorValue).
func buildOrdinalJoinRC(t *testing.T, qovs ...values.QuantifiedObjectValue) *values.RecordConstructorValue {
	t.Helper()
	var fields []values.RecordConstructorField
	for _, qov := range qovs {
		rt, isRT := qov.Type().(*values.RecordType)
		if !isRT {
			t.Fatalf("buildOrdinalJoinRC: leg %s flows %T, want *RecordType", qov.Correlation(), qov.Type())
		}
		for i := range rt.Fields {
			fv, err := values.ResolveOrdinalSeedField(qov, i)
			if err != nil {
				t.Fatalf("buildOrdinalJoinRC: ResolveOrdinalSeedField(%s, %d): %v", qov.Correlation(), i, err)
			}
			view, ok := values.AsFieldValue(fv)
			if !ok {
				t.Fatalf("ResolveOrdinalSeedField returned %T, want exact FieldValue", fv)
			}
			fields = append(fields, values.RecordConstructorField{Name: view.DisplayName(), Value: fv})
		}
	}
	return values.NewRawRecordConstructorValue(fields...)
}

// ojMergedRow hand-builds the merged positional row [A.ID=1, A.V=10, B.ID=2,
// B.W=20] over the given merged type — the fixture every window test reads.
func ojMergedRow(t *testing.T, mergedType *values.RecordType) *PositionalRow {
	t.Helper()
	row := NewPositionalRow(mergedType)
	for i, v := range []any{int64(1), int64(10), int64(2), int64(20)} {
		if !row.Set(i, v) {
			t.Fatalf("ojMergedRow: Set(%d) out of range — merged type has %d fields", i, len(mergedType.Fields))
		}
	}
	return row
}

// stubBinder binds correlations to arbitrary values. A key PRESENT with a nil
// value binds the leg to nil — the sanctioned NULL-leg expression: a
// nil-bound leg evaluates as NULL, never an error.
type stubBinder map[values.CorrelationIdentifier]any

func (b stubBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	v, found := b[id]
	return v, found
}

// mustPanicLoud runs fn and asserts it panics (with every extra want
// substring present in the panic message, if any) — a malformed ordinal seed
// is a planner bug and must be LOUD, never a silent name-model demotion.
func mustPanicLoud(t *testing.T, fn func(), want ...string) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatal("want a panic, got none — a malformed ordinal seed silently passed")
		}
		msg := fmt.Sprint(r)
		for _, w := range want {
			if !strings.Contains(msg, w) {
				t.Fatalf("panic message %q must contain %q", msg, w)
			}
		}
	}()
	fn()
}

// --- raw RC constructor ------------------------------------------------------

// TestRawRecordConstructor_DupNamesVerbatim pins the dedicated raw RC
// constructor: duplicate field names survive VERBATIM (the
// ordinal join RC's cross-leg duplicates are positional, not name-addressed),
// while NewRecordConstructorValue — the name-model constructor — still renames
// (ID, ID_2), unchanged.
func TestRawRecordConstructor_DupNamesVerbatim(t *testing.T) {
	t.Parallel()
	fields := []values.RecordConstructorField{
		{Name: "ID", Value: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}},
		{Name: "ID", Value: &values.ConstantValue{Value: int64(2), Typ: values.NotNullLong}},
	}

	raw := values.NewRawRecordConstructorValue(fields...)
	if raw.Fields[0].Name != "ID" || raw.Fields[1].Name != "ID" {
		t.Fatalf("raw RC names = [%q, %q], want [ID, ID] — duplicates must survive verbatim", raw.Fields[0].Name, raw.Fields[1].Name)
	}

	// Control: the name-model constructor still renames.
	named := values.NewRecordConstructorValue(fields...)
	if named.Fields[0].Name != "ID" || named.Fields[1].Name != "ID_2" {
		t.Fatalf("name-model RC names = [%q, %q], want [ID, ID_2] — the _N dedup is name-model behavior and must stay", named.Fields[0].Name, named.Fields[1].Name)
	}
}

// --- ordinalJoinSpans --------------------------------------------------------

// TestOrdinalJoinSpans_HappyPath pins the strict deriver on a 2+1
// seed: spans (offsets 0/2, widths 2/1) derive from the RC — the single
// authority — and the merged type keeps the concatenated names in order,
// duplicate ID across legs preserved.
func TestOrdinalJoinSpans_HappyPath(t *testing.T) {
	t.Parallel()
	corrA := values.NamedCorrelationIdentifier("a")
	corrB := values.NamedCorrelationIdentifier("b")
	qovA := mustTestQOV(t, corrA, ojLegTypeAV())
	legB := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	qovB := mustTestQOV(t, corrB, legB)

	rc := buildOrdinalJoinRC(t, qovA, qovB)
	spans, mergedType, ok := ordinalJoinSpans(rc)
	if !ok {
		t.Fatal("ordinalJoinSpans rejected a well-formed ordinal join RC")
	}
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	if spans[0].Alias != corrA || spans[0].Offset != 0 || spans[0].Width != 2 {
		t.Fatalf("leg A span = %+v, want {a, offset 0, width 2}", spans[0])
	}
	if spans[1].Alias != corrB || spans[1].Offset != 2 || spans[1].Width != 1 {
		t.Fatalf("leg B span = %+v, want {b, offset 2, width 1}", spans[1])
	}
	if len(spans[0].LegType.Fields) != 2 || len(spans[1].LegType.Fields) != 1 {
		t.Fatalf("leg types = %d/%d fields, want 2/1", len(spans[0].LegType.Fields), len(spans[1].LegType.Fields))
	}
	wantNames := []string{"ID", "V", "ID"}
	if len(mergedType.Fields) != len(wantNames) {
		t.Fatalf("merged type has %d fields, want %d", len(mergedType.Fields), len(wantNames))
	}
	for i, want := range wantNames {
		f := mergedType.Fields[i]
		if f.Name != want || f.Ordinal != i {
			t.Fatalf("merged field %d = {%q, ord %d}, want {%q, ord %d} — dup names across legs must survive in order", i, f.Name, f.Ordinal, want, i)
		}
		if f.FieldType == nil || !f.FieldType.Equals(values.NotNullLong) {
			t.Fatalf("merged field %d type = %v, want the baked FieldValue's type", i, f.FieldType)
		}
	}
}

// TestOrdinalJoinSpans_NameModelDeclines pins ok=false — NOT a panic
// — for everything the name model produces: a non-RC value, a lazy projection
// RC, and a lazy ANCHORED join RC. Those are ordinary shapes the name-model
// planner path produces; only a malformed ORDINAL seed is loud.
func TestOrdinalJoinSpans_NameModelDeclines(t *testing.T) {
	t.Parallel()
	qovA := mustTestQOV(t, values.NamedCorrelationIdentifier("a"), ojLegTypeAV())

	// Non-RC value.
	if _, _, ok := ordinalJoinSpans(&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}); ok {
		t.Fatal("a non-RC value must not be an ordinal join")
	}
	// Lazy projection RC (zero baked fields).
	lazyRC := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: mustTestFieldOrdinal(t, qovA, 0)},
		values.RecordConstructorField{Name: "V", Value: mustTestFieldOrdinal(t, qovA, 1)},
	)
	if _, _, ok := ordinalJoinSpans(lazyRC); ok {
		t.Fatal("a lazy RC must not be an ordinal join — the name model's RCs land here")
	}
}

// TestOrdinalJoinSpans_FoldedProjectionsDecline pins a boundary case: a
// pure-wrapper merge rewrites the select's result value into the parent
// projection's RC — which LEGITIMATELY contains baked leg references without
// being the seed. The cursor-side probe must DECLINE these (no windows — the
// output is a plain projection row), never panic; only the translator-side
// seed assert is loud.
func TestOrdinalJoinSpans_FoldedProjectionsDecline(t *testing.T) {
	t.Parallel()
	qovA := mustTestQOV(t, values.NamedCorrelationIdentifier("a"), ojLegTypeAV())
	bakedV := mustExecutorConstruct(values.ResolveOrdinalSeedField(qovA, 1))

	// `SELECT a.V FROM a JOIN b` post-merge: single run, partial coverage.
	folded := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "V", Value: bakedV},
	)
	if _, _, ok := ordinalJoinSpans(folded); ok {
		t.Fatal("a folded single-column projection over a gated join must DECLINE — it is not the seed concat")
	}

	// `SELECT a.V, 1 FROM a JOIN b` post-merge: baked ref mixed with a constant.
	mixed := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "V", Value: bakedV},
		values.RecordConstructorField{Name: "_1", Value: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}},
	)
	if _, _, ok := ordinalJoinSpans(mixed); ok {
		t.Fatal("a mixed baked/constant projection RC must DECLINE — legitimate fold, not a malformed seed")
	}

	// Both shapes DO read as ordinal-build plans via the deep probe — that is
	// the cursor-side detector split: ContainsBakedOrdinal says "evaluate with
	// leg bindings", ordinalJoinSpans says "leg windows apply downstream".
	if !values.ContainsBakedOrdinal(folded) || !values.ContainsBakedOrdinal(mixed) {
		t.Fatal("the deep probe must still detect folded projections as ordinal-build plans")
	}
}

// TestSeedAssert_MalformedPanics pins every malformation panic in
// values.AssertOrdinalJoinSeed — the SEED-TIME validator. The loudness lives at the
// translator seed, where the pristine shape is guaranteed by construction;
// the cursor-side ordinalJoinSpans probe DECLINES the same shapes, pinned
// separately, because post-merge result values legitimately mix/fold baked
// references.
func TestSeedAssert_MalformedPanics(t *testing.T) {
	t.Parallel()
	newQOV := func(name string, rt *values.RecordType) values.QuantifiedObjectValue {
		return mustTestQOV(t, values.NamedCorrelationIdentifier(name), rt)
	}
	baked := func(qov values.QuantifiedObjectValue, ord int) values.RecordConstructorField {
		t.Helper()
		fv, err := values.ResolveOrdinalSeedField(qov, ord)
		if err != nil {
			t.Fatalf("ResolveOrdinalSeedField(%s, %d): %v", qov.Correlation(), ord, err)
		}
		view, ok := values.AsFieldValue(fv)
		if !ok {
			t.Fatalf("ResolveOrdinalSeedField returned %T, want exact FieldValue", fv)
		}
		return values.RecordConstructorField{Name: view.DisplayName(), Value: fv}
	}

	t.Run("single run", func(t *testing.T) {
		t.Parallel()
		qovA := newQOV("a", ojLegTypeAV())
		rc := values.NewRawRecordConstructorValue(baked(qovA, 0), baked(qovA, 1))
		mustPanicLoud(t, func() { values.AssertOrdinalJoinSeed(rc) }, "at least two legs")
		if _, _, ok := ordinalJoinSpans(rc); ok {
			t.Fatal("the cursor-side probe must DECLINE this shape, not accept it")
		}
	})

	t.Run("three runs are legal (N-leg flat seeds)", func(t *testing.T) {
		t.Parallel()
		oneCol := func(name string) *values.RecordType {
			return values.NewRecordType("", false, []values.Field{{Name: name, FieldType: values.NotNullLong, Ordinal: 0}})
		}
		rc := values.NewRawRecordConstructorValue(
			baked(newQOV("a", oneCol("X")), 0),
			baked(newQOV("b", oneCol("Y")), 0),
			baked(newQOV("c", oneCol("Z")), 0),
		)
		values.AssertOrdinalJoinSeed(rc) // must NOT panic — a join seed may flatten any number of legs, not just two
		spans, mergedType, ok := ordinalJoinSpans(rc)
		if !ok || len(spans) != 3 {
			t.Fatalf("cursor-side probe must accept the 3-leg seed with 3 spans, got (%v, ok=%v)", spans, ok)
		}
		if len(mergedType.Fields) != 3 {
			t.Fatalf("merged type = %v, want 3 slots", mergedType)
		}
	})

	t.Run("ordinal gap", func(t *testing.T) {
		t.Parallel()
		wide := values.NewRecordType("", false, []values.Field{
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
			{Name: "X", FieldType: values.NotNullLong, Ordinal: 2},
		})
		qovA := newQOV("a", wide)
		qovB := newQOV("b", ojLegTypeBW())
		rc := values.NewRawRecordConstructorValue(
			baked(qovA, 0), baked(qovA, 2), // gap: 0,2
			baked(qovB, 0), baked(qovB, 1),
		)
		mustPanicLoud(t, func() { values.AssertOrdinalJoinSeed(rc) })
		if _, _, ok := ordinalJoinSpans(rc); ok {
			t.Fatal("the cursor-side probe must DECLINE this shape, not accept it")
		}
	})

	t.Run("partial leg coverage", func(t *testing.T) {
		t.Parallel()
		wide := values.NewRecordType("", false, []values.Field{
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
			{Name: "X", FieldType: values.NotNullLong, Ordinal: 2},
		})
		qovA := newQOV("a", wide)
		qovB := newQOV("b", ojLegTypeBW())
		rc := values.NewRawRecordConstructorValue(
			baked(qovA, 0), baked(qovA, 1), // leg type has 3 fields, run has 2
			baked(qovB, 0), baked(qovB, 1),
		)
		mustPanicLoud(t, func() { values.AssertOrdinalJoinSeed(rc) })
		if _, _, ok := ordinalJoinSpans(rc); ok {
			t.Fatal("the cursor-side probe must DECLINE this shape, not accept it")
		}
	})

	t.Run("mixed baked and lazy", func(t *testing.T) {
		t.Parallel()
		qovA := newQOV("a", ojLegTypeAV())
		rc := values.NewRawRecordConstructorValue(
			baked(qovA, 0),
			values.RecordConstructorField{Name: "V", Value: mustTestFieldOrdinal(t, qovA, 1)},
		)
		mustPanicLoud(t, func() { values.AssertOrdinalJoinSeed(rc) })
		if _, _, ok := ordinalJoinSpans(rc); ok {
			t.Fatal("the cursor-side probe must DECLINE this shape, not accept it")
		}
	})
}

// --- legWindowRow ------------------------------------------------------------

// TestLegWindowRow pins the window mechanics over a hand-built merged
// row: Get is leg-relative (window at offset 2 reads merged slot 2+i),
// out-of-range is (nil,false) — never a read into a sibling leg — and
// TypeNames reports the leg's columns. A leg-local plan-time bind (the LEG
// type's FieldIndexUnique) names exactly the leg's own columns, even when the merged
// type carries the same name at a DIFFERENT absolute slot (the wrong-slot
// axis).
func TestLegWindowRow(t *testing.T) {
	t.Parallel()
	corrA := values.NamedCorrelationIdentifier("a")
	corrB := values.NamedCorrelationIdentifier("b")
	qovA := mustTestQOV(t, corrA, ojLegTypeAV())
	qovB := mustTestQOV(t, corrB, ojLegTypeBW())
	rc := buildOrdinalJoinRC(t, qovA, qovB)
	spans, mergedType, ok := ordinalJoinSpans(rc)
	if !ok {
		t.Fatal("ordinalJoinSpans rejected the fixture RC")
	}
	merged := ojMergedRow(t, mergedType) // [A.ID=1, A.V=10, B.ID=2, B.W=20]

	winB := &legWindowRow{parent: merged, legType: spans[1].LegType, offset: spans[1].Offset, width: spans[1].Width}
	if v, found := winB.Get(0); !found || v != int64(2) {
		t.Fatalf("window B Get(0) = (%v, %v), want (2, true) — leg ordinal 0 is merged slot 2", v, found)
	}
	if v, found := winB.Get(1); !found || v != int64(20) {
		t.Fatalf("window B Get(1) = (%v, %v), want (20, true)", v, found)
	}
	if v, found := winB.Get(2); found || v != nil {
		t.Fatalf("window B Get(2) = (%v, %v), want (nil, false) — past the leg width, never a read into a sibling leg", v, found)
	}
	if v, found := winB.Get(-1); found || v != nil {
		t.Fatalf("window B Get(-1) = (%v, %v), want (nil, false)", v, found)
	}

	// The plan-time bind is leg-LOCAL: "ID" resolves against LEG B's type to
	// leg ordinal 0 (→ merged slot 2 = B's ID=2), even though the merged type
	// has "ID" at absolute slot 0 holding A's ID=1 — the wrong-slot axis. A's
	// "V" is not a column of B's leg type at all.
	if idx, found := spans[1].LegType.FieldIndexUnique("ID"); !found || idx != 0 {
		t.Fatalf("leg B FieldIndexUnique(ID) = (%d, %v), want (0, true) — B's own ID, not the merged slot 0", idx, found)
	}
	if idx, found := spans[1].LegType.FieldIndexUnique("W"); !found || idx != 1 {
		t.Fatalf("leg B FieldIndexUnique(W) = (%d, %v), want (1, true)", idx, found)
	}
	if _, found := spans[1].LegType.FieldIndexUnique("V"); found {
		t.Fatal("leg B FieldIndexUnique(V) must miss — V is leg A's column, not visible through B's window")
	}

	wantNames := []string{"ID", "W"}
	got := winB.TypeNames()
	if len(got) != len(wantNames) || got[0] != wantNames[0] || got[1] != wantNames[1] {
		t.Fatalf("window B TypeNames = %v, want %v — the LEG's names, for OrdinalResolutionError diagnostics", got, wantNames)
	}
}

// TestLegWindow_WrongSlotHazard is the red→green pin on the exact hazard the
// RFC-232 OrdinalLayout replaces legacy RecordType.Legs to prevent. B.W has
// source ordinal 1. The carrier's absolute slot 1 is A.V, so an evaluation
// phase whose layout does not bind B must fail LOUD rather than fall through to
// the carrier row. The full A/B layout maps B field 1 to carrier slot 3 and
// reads 20. Both halves use the same FieldValue; only the admitted immutable
// layout differs.
func TestLegWindow_WrongSlotHazard(t *testing.T) {
	t.Parallel()
	corrA := values.NamedCorrelationIdentifier("a")
	corrB := values.NamedCorrelationIdentifier("b")
	qovA := mustTestQOV(t, corrA, ojLegTypeAV())
	qovB := mustTestQOV(t, corrB, ojLegTypeBW())
	rc := buildOrdinalJoinRC(t, qovA, qovB)
	_, mergedType, ok := ordinalJoinSpans(rc)
	if !ok {
		t.Fatal("ordinalJoinSpans rejected the fixture RC")
	}

	// The discriminating probe is B.W: its source-relative ordinal is 1. A
	// leg-oblivious carrier read would land on absolute slot 1 = A.V=10.
	bwRef := mustTestFieldOrdinal(t, qovB, 1)

	newLayout := func(windows []values.OrdinalWindowSpec) values.OrdinalLayout {
		t.Helper()
		layout, err := values.NewOrdinalLayoutForCarrierType(
			mergedType,
			[]values.OrdinalTileSpec{{Start: 0, Width: 4, Kind: values.OrdinalTileFlat}},
			windows,
		)
		if err != nil {
			t.Fatalf("NewOrdinalLayoutForCarrierType: %v", err)
		}
		return layout
	}
	rowFor := func(layout values.OrdinalLayout) *PositionalRow {
		t.Helper()
		row, err := NewLayoutPositionalRow(mergedType, layout)
		if err != nil {
			t.Fatalf("NewLayoutPositionalRow: %v", err)
		}
		copy(row.Slots, []any{int64(1), int64(10), int64(2), int64(20)})
		return row
	}
	aWindow := values.OrdinalWindowSpec{Source: qovA, FieldPaths: [][]int{{0}, {1}}}
	bWindow := values.OrdinalWindowSpec{Source: qovB, FieldPaths: [][]int{{2}, {3}}}

	// (i) THE HAZARD, caught: the selected layout has no B window. Objects is
	// installed, so exact QOV evaluation cannot use the ambient positional
	// fallback and must report the absent binding.
	aOnly := newLayout([]values.OrdinalWindowSpec{aWindow})
	aOnlyCtx, err := ordinalLayoutRowContext(aOnly, rowFor(aOnly), nil, nil)
	if err != nil {
		t.Fatalf("A-only ordinalLayoutRowContext: %v", err)
	}
	if got, evalErr := bwRef.Evaluate(aOnlyCtx); evalErr == nil {
		t.Fatalf("B.W without a B layout window = %v, want loud unbound correlation (absolute slot 1 is A.V=10)", got)
	} else {
		var resolutionErr *values.ResolutionError
		if !errors.As(evalErr, &resolutionErr) || resolutionErr.Code() != values.UnboundCorrelation {
			t.Fatalf("hazard eval error = %v, want UnboundCorrelation", evalErr)
		}
	}

	// (ii) GREEN: the full layout maps B's source slot 1 to carrier slot 3.
	full := newLayout([]values.OrdinalWindowSpec{aWindow, bWindow})
	fullCtx, err := ordinalLayoutRowContext(full, rowFor(full), nil, nil)
	if err != nil {
		t.Fatalf("full ordinalLayoutRowContext: %v", err)
	}
	got, err := bwRef.Evaluate(fullCtx)
	if err != nil {
		t.Fatalf("windowed eval errored: %v", err)
	}
	if got != int64(20) {
		t.Fatalf("B.W through the leg window = %v, want 20 (B's W)", got)
	}
	// Secondary: B.ID's source ordinal 0 is likewise unbound in the A-only
	// layout and correct (2) through the full layout.
	bidRef := mustTestFieldOrdinal(t, qovB, 0)
	if got, evalErr := bidRef.Evaluate(aOnlyCtx); evalErr == nil {
		t.Fatalf("B.ID without a B layout window = %v, want loud unbound correlation", got)
	}
	if got, evalErr := bidRef.Evaluate(fullCtx); evalErr != nil || got != int64(2) {
		t.Fatalf("B.ID through the full layout = (%v, %v), want (2, nil)", got, evalErr)
	}

	// (iii) THE FUSED TWIN, also caught. A source-relative leg reference that
	// picks up a suffix (composeFieldOverField / the withChildren rebuild-fuse)
	// becomes a MULTI-accessor node whose ROOT ordinal stays leg-relative and
	// UNPINNED. That shape used to slip the old guard (which required a single
	// accessor) and read a FOREIGN leg's root slot, then descend into it —
	// silently. It must be held to the SAME correct-or-loud bar as its
	// single-accessor twin. Built here via the exported rebuild-fuse producer
	// (Java's withNewChild == ofFieldsAndFuseIfPossible), which yields the
	// identical node composeFieldOverField does.
	hdrType := values.NewRecordType("", false, []values.Field{{Name: "SUB", FieldType: values.NotNullLong, Ordinal: 0}})
	nestedB := mustTestQOV(t, corrB, values.NewRecordType("", false, []values.Field{
		{Name: "HDR", FieldType: hdrType, Ordinal: 0},
	}))
	fused := mustExecutorConstruct(values.ResolveFieldOrdinals(nestedB, []int{0, 0}))
	fusedView, ok := values.AsFieldValue(fused)
	if !ok || fusedView.Path() == nil {
		t.Fatalf("nested exact resolution returned %T, want exact FieldValue", fused)
	}
	if fusedView.Path().Len() != 2 || fusedView.Path().IsFrontierPinned() {
		t.Fatalf("fused node shape = (accessors %d, pinned %v), want (2, false) — the multi-accessor unpinned shape that slips the old single-accessor guard", fusedView.Path().Len(), fusedView.Path().IsFrontierPinned())
	}
	// The explicit Objects binder makes the fused twin obey the same rule: the
	// absent exact source is loud and cannot fall through to carrier slot 0.
	if got, evalErr := fused.Evaluate(aOnlyCtx); evalErr == nil {
		t.Fatalf("fused B.HDR.SUB without its layout window = %v, want loud unbound correlation", got)
	} else {
		var resolutionErr *values.ResolutionError
		if !errors.As(evalErr, &resolutionErr) || resolutionErr.Code() != values.UnboundCorrelation {
			t.Fatalf("fused hazard eval error = %v, want UnboundCorrelation", evalErr)
		}
	}

	// A machinery-pinned B#0 through the same layout reads the correct slot.
	bakedBID := mustExecutorConstruct(values.ResolveOrdinalSeedField(qovB, 0))
	got, err = bakedBID.Evaluate(fullCtx)
	if err != nil {
		t.Fatalf("baked windowed eval errored: %v", err)
	}
	if got != int64(2) {
		t.Fatalf("baked B#0 through the leg window = %v, want 2", got)
	}
}

// TestLegWindowBinder_Delegation pins the binder's non-span arms: an
// unknown alias delegates to base when non-nil, else (nil, false).
func TestLegWindowBinder_Delegation(t *testing.T) {
	t.Parallel()
	corrA := values.NamedCorrelationIdentifier("a")
	outer := values.NamedCorrelationIdentifier("outer")
	spans := []legSpan{{Alias: corrA, LegType: ojLegTypeAV(), Offset: 0, Width: 2}}
	row := NewPositionalRow(ojLegTypeAV())

	// Span alias: a window.
	b := &legWindowBinder{spans: spans, row: row}
	bound, found := b.GetCorrelationBinding(corrA)
	if !found {
		t.Fatal("span alias must bind")
	}
	if _, isWin := bound.(*legWindowRow); !isWin {
		t.Fatalf("span alias bound to %T, want *legWindowRow", bound)
	}
	// Unknown alias, nil base: unbound.
	if _, found := b.GetCorrelationBinding(outer); found {
		t.Fatal("unknown alias with nil base must be unbound")
	}
	// Unknown alias, base present: delegates (the outer-correlation path).
	withBase := &legWindowBinder{base: stubBinder{outer: int64(42)}, spans: spans, row: row}
	bound, found = withBase.GetCorrelationBinding(outer)
	if !found || bound != int64(42) {
		t.Fatalf("base delegation = (%v, %v), want (42, true)", bound, found)
	}
}

// --- evaluateOrdinalJoinRow ---------------------------------------------------

// TestEvaluateOrdinalJoinRow pins the merged-row build primitive:
// both legs bound to leg-local positional rows concatenate into the merged
// slots; a leg bound to nil — (nil, true), the sanctioned appendNullLeg
// expression — yields NULL for exactly that leg's slots (appendNullLeg is
// exactly evaluating the merged RC with the leg QOV bound to nil).
func TestEvaluateOrdinalJoinRow(t *testing.T) {
	t.Parallel()
	corrA := values.NamedCorrelationIdentifier("a")
	corrB := values.NamedCorrelationIdentifier("b")
	legA, legB := ojLegTypeAV(), ojLegTypeBW()
	qovA := mustTestQOV(t, corrA, legA)
	qovB := mustTestQOV(t, corrB, legB)
	rc := buildOrdinalJoinRC(t, qovA, qovB)
	_, mergedType, ok := ordinalJoinSpans(rc)
	if !ok {
		t.Fatal("ordinalJoinSpans rejected the fixture RC")
	}

	rowA := NewPositionalRow(legA)
	rowA.Set(0, int64(1))
	rowA.Set(1, int64(10))
	rowB := NewPositionalRow(legB)
	rowB.Set(0, int64(2))
	rowB.Set(1, int64(20))

	t.Run("both legs bound", func(t *testing.T) {
		t.Parallel()
		merged, err := evaluateOrdinalJoinRow(rc, mergedType, stubBinder{corrA: rowA, corrB: rowB}, nil)
		if err != nil {
			t.Fatalf("evaluateOrdinalJoinRow: %v", err)
		}
		if merged.Type != mergedType {
			t.Fatal("merged row must carry the single merged type")
		}
		want := []any{int64(1), int64(10), int64(2), int64(20)}
		for i, w := range want {
			if got, found := merged.Get(i); !found || got != w {
				t.Fatalf("merged slot %d = (%v, %v), want (%v, true) — legs concatenate", i, got, found, w)
			}
		}
	})

	t.Run("null leg B", func(t *testing.T) {
		t.Parallel()
		// The NULL-leg equivalence pin at the primitive level: leg B bound to
		// nil (present!) — the baked nodes' `return bound, nil` arm yields nil
		// and B's slots fall out NULL, A's intact. No per-row type, no
		// special-case appendNullLeg arm.
		merged, err := evaluateOrdinalJoinRow(rc, mergedType, stubBinder{corrA: rowA, corrB: nil}, nil)
		if err != nil {
			t.Fatalf("evaluateOrdinalJoinRow with null leg: %v", err)
		}
		want := []any{int64(1), int64(10), nil, nil}
		for i, w := range want {
			if got, found := merged.Get(i); !found || got != w {
				t.Fatalf("null-leg merged slot %d = (%v, %v), want (%v, true)", i, got, found, w)
			}
		}
	})

	// (A map-bound leg is a non-nil, non-OrdinalRow binding: for a FrontierPinned
	// baked node it is a frontier-contract violation — the same loud
	// *BakedNameContextError the garbage-leg pin below asserts (frontierContractGuard).
	// A nil-bound leg stays the sanctioned null leg → NULL.)

	t.Run("garbage leg binding is a loud frontier-contract violation", func(t *testing.T) {
		t.Parallel()
		// A leg bound to a non-nil, non-OrdinalRow value is a frontier-contract
		// violation — the executor must supply an ordinal row (or nil for a
		// null leg). A baked FrontierPinned node hitting such a binding is a
		// LOUD *values.BakedNameContextError (frontierContractGuard), never a
		// silent raw-object slot that would corrupt the merged row.
		type garbage struct{ x int }
		g := garbage{x: 9}
		_, err := evaluateOrdinalJoinRow(rc, mergedType, stubBinder{corrA: rowA, corrB: g}, nil)
		var bnce *values.BakedNameContextError
		if !errors.As(err, &bnce) {
			t.Fatalf("garbage leg binding = %v, want a loud *values.BakedNameContextError (frontier-contract violation)", err)
		}
	})

	t.Run("merged type not derived from RC errors", func(t *testing.T) {
		t.Parallel()
		// A field-count mismatch between the RC and the merged type means
		// the plan is malformed (a planner bug — the merged type must derive
		// from this RC via ordinalJoinSpans): fail the QUERY with a loud
		// error, never the process.
		_, err := evaluateOrdinalJoinRow(rc, legA, stubBinder{corrA: rowA, corrB: rowB}, nil)
		if err == nil {
			t.Fatal("want a loud malformed-plan error, got nil — a malformed ordinal seed silently passed")
		}
		if !strings.Contains(err.Error(), "malformed plan") {
			t.Fatalf("error = %v, want the malformed-plan diagnosis", err)
		}
	})
}

// --- adaptLegPositional --------------------------------------------------------

// TestAdaptLegPositional pins the row-FORMAT adapter at the join-input
// boundary: positional passthrough, per-name reordering into leg-type order
// (missing column → nil slot), nil row → all-nil row, and the LOUD zero-match
// tripwire (a dotted-key merge-shaped leg must never silently all-NULL).
func TestAdaptLegPositional(t *testing.T) {
	t.Parallel()
	legA := ojLegTypeAV()

	t.Run("positional passthrough", func(t *testing.T) {
		t.Parallel()
		pos := NewPositionalRow(legA)
		pos.Set(0, int64(7))
		got, err := adaptLegPositional(QueryResult{Positional: pos}, legA, values.CorrelationIdentifier{})
		if err != nil {
			t.Fatalf("passthrough errored: %v", err)
		}
		if got != values.OrdinalRow(pos) {
			t.Fatal("a leg that already carries a positional row must flow it through untouched")
		}
	})

	t.Run("datum synthesis by leg-type names", func(t *testing.T) {
		t.Parallel()
		got, err := adaptLegPositional(dmap(map[string]any{"ID": int64(7)}), legA, values.CorrelationIdentifier{}) // V missing
		if err != nil {
			t.Fatalf("synthesis errored: %v", err)
		}
		if v, found := got.Get(0); !found || v != int64(7) {
			t.Fatalf("synthesized slot 0 = (%v, %v), want (7, true)", v, found)
		}
		if v, found := got.Get(1); !found || v != nil {
			t.Fatalf("missing-key slot 1 = (%v, %v), want (nil, true) — SQL NULL, in range", v, found)
		}
		row, isRow := got.(*PositionalRow)
		if !isRow || row.Type != legA {
			t.Fatalf("synthesized row = %T (type %v), want *PositionalRow over the LEG type", got, legA)
		}
	})

	t.Run("all-null row with present keys stays silent", func(t *testing.T) {
		t.Parallel()
		// A genuine all-NULL row (keys present, nil values) is NOT the dotted
		// zero-match shape — it matches every column and synthesizes quietly.
		got, err := adaptLegPositional(dmap(map[string]any{"ID": nil, "V": nil}), legA, values.CorrelationIdentifier{})
		if err != nil {
			t.Fatalf("genuine all-NULL row must synthesize silently, got %v", err)
		}
		if v, found := got.Get(0); !found || v != nil {
			t.Fatalf("all-NULL slot 0 = (%v, %v), want (nil, true)", v, found)
		}
	})

	t.Run("zero-match non-empty datum is loud", func(t *testing.T) {
		t.Parallel()
		// A merge-shaped leg carries dotted-qualified keys the bare leg-type
		// names never match — silently all-NULLing it would be
		// indistinguishable from a legitimate all-NULL row. The planner
		// already gates a genuine join input from reaching this shape; this
		// pins the tripwire that would catch it if that gate ever failed.
		_, err := adaptLegPositional(dmap(map[string]any{"A.ID": int64(1), "A.V": int64(10)}), legA, values.CorrelationIdentifier{})
		if err == nil || !strings.Contains(err.Error(), "leg adapter") {
			t.Fatalf("dotted-key zero-match synthesis must be a loud leg-adapter error, got %v", err)
		}
	})

	t.Run("nil datum", func(t *testing.T) {
		t.Parallel()
		got, err := adaptLegPositional(QueryResult{}, legA, values.CorrelationIdentifier{})
		if err != nil {
			t.Fatalf("nil datum errored: %v", err)
		}
		for i := range legA.Fields {
			if v, found := got.Get(i); !found || v != nil {
				t.Fatalf("nil-datum slot %d = (%v, %v), want (nil, true)", i, v, found)
			}
		}
	})
}

// --- real-PositionalRow eval pin --------------------------------------------

// TestBakedEval_RealPositionalRow pins that a BAKED FieldValue evaluated
// directly against a real executor *PositionalRow (not a values-package test
// fake) reads the right slot through the OrdinalRow arm, and an out-of-range
// bake is a loud LayoutRuntimeShape ResolutionError — never a silent NULL.
func TestBakedEval_RealPositionalRow(t *testing.T) {
	t.Parallel()
	legB := ojLegTypeBW()
	qovB := mustTestQOV(t, values.NamedCorrelationIdentifier("b"), legB)
	row := NewPositionalRow(legB)
	row.Set(0, int64(2))
	row.Set(1, int64(20))

	baked1 := mustExecutorConstruct(values.ResolveOrdinalSeedField(qovB, 1))
	got, err := baked1.Evaluate(row)
	if err != nil {
		t.Fatalf("baked eval over a real PositionalRow: %v", err)
	}
	if got != int64(20) {
		t.Fatalf("baked B#1 over a real PositionalRow = %v, want 20", got)
	}

	// Out-of-range: bake against a WIDER type than the runtime row (the bake
	// itself is legal; the row is short) — loud runtime-shape error.
	wide := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "W", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "X", FieldType: values.NotNullLong, Ordinal: 2},
	})
	qovWide := mustTestQOV(t, values.NamedCorrelationIdentifier("wb"), wide)
	baked2 := mustExecutorConstruct(values.ResolveOrdinalSeedField(qovWide, 2))
	_, err = baked2.Evaluate(row)
	var resolutionErr *values.ResolutionError
	if !errors.As(err, &resolutionErr) || resolutionErr.Code() != values.LayoutRuntimeShape {
		t.Fatalf("out-of-range baked eval over a real PositionalRow must be LayoutRuntimeShape, got %v", err)
	}
	if resolutionErr.Path != "field.path[0]" || !strings.Contains(resolutionErr.Detail, "shorter") {
		t.Fatalf("runtime-shape diagnostic = path %q detail %q, want failing field step and short-row detail", resolutionErr.Path, resolutionErr.Detail)
	}
}

// TestLegWindow_OutOfRangeIsLoud pins that a leg window's
// out-of-range miss surfaces through the EXISTING eval machinery as a loud
// LayoutRuntimeShape ResolutionError — no silent NULL or neighbouring slot.
func TestLegWindow_OutOfRangeIsLoud(t *testing.T) {
	t.Parallel()
	corrA := values.NamedCorrelationIdentifier("a")
	corrB := values.NamedCorrelationIdentifier("b")
	qovA := mustTestQOV(t, corrA, ojLegTypeAV())
	qovB := mustTestQOV(t, corrB, ojLegTypeBW())
	rc := buildOrdinalJoinRC(t, qovA, qovB)
	spans, mergedType, ok := ordinalJoinSpans(rc)
	if !ok {
		t.Fatal("ordinalJoinSpans rejected the fixture RC")
	}
	merged := ojMergedRow(t, mergedType)
	binder := &legWindowBinder{spans: spans, row: merged}

	// The value was valid against a wider exact declaration, but is evaluated
	// against the two-slot B window. This models a stale producer/consumer
	// layout disagreement without making a malformed FieldValue representable.
	staleFields := make([]values.Field, 6)
	for i := range staleFields {
		staleFields[i] = values.Field{Name: values.OrdinalFieldName(i), FieldType: values.NotNullLong, Ordinal: i}
	}
	staleQOV := mustTestQOV(t, corrB, values.NewRecordType("", false, staleFields))
	stale := mustExecutorConstruct(values.ResolveOrdinalSeedField(staleQOV, 5))
	_, err := stale.Evaluate(&values.RowEvalContext{Correlations: binder})
	var resolutionErr *values.ResolutionError
	if !errors.As(err, &resolutionErr) || resolutionErr.Code() != values.LayoutRuntimeShape {
		t.Fatalf("out-of-window baked eval must be LayoutRuntimeShape, got %v", err)
	}
	if resolutionErr.Path != "field.path[0]" || !strings.Contains(resolutionErr.Detail, "shorter") {
		t.Fatalf("out-of-window diagnostic = path %q detail %q, want failing field step and short-row detail", resolutionErr.Path, resolutionErr.Detail)
	}
}

// TestAdaptLegPositional_IndexShapedFallsBack pins a layout hazard:
// a COVERING-INDEX leg's positional row is INDEX-shaped
// (value-columns-then-PK, e.g. [V, ID]) while the seed typed the leg in table
// order ([ID, V]) — SAME width, different layout. The passthrough must
// REJECT it (ordered per-slot name agreement) and fall back to Datum
// synthesis, or baked leg ordinals silently read the wrong slots. The
// aligned passthrough and the genuine width mismatch are pinned alongside.
func TestAdaptLegPositional_IndexShapedFallsBack(t *testing.T) {
	t.Parallel()
	legType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
	}}
	indexShaped := &values.RecordType{Fields: []values.Field{
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1},
	}}
	// Covering row: V=20 at slot 0, ID=1 at slot 1 (index-shaped).
	cov := &PositionalRow{Type: indexShaped, Slots: []any{int64(20), int64(1)}}
	qr := QueryResult{Positional: cov}

	row, err := adaptLegPositional(qr, legType, values.CorrelationIdentifier{})
	if err != nil {
		t.Fatalf("adaptLegPositional: %v", err)
	}
	// The adapted row must be LEG-shaped: slot 0 = ID = 1, resolved by NAME
	// against the index-shaped row (a passthrough would put V=20 there — the
	// silent misread).
	if v, ok := row.Get(0); !ok || v != int64(1) {
		t.Fatalf("adapted slot 0 = (%v, %v), want (1, true) — index-shaped passthrough would give 20", v, ok)
	}
	if v, ok := row.Get(1); !ok || v != int64(20) {
		t.Fatalf("adapted slot 1 = (%v, %v), want (20, true)", v, ok)
	}

	// ALIGNED positional: passthrough (same object).
	aligned := &PositionalRow{Type: legType, Slots: []any{int64(1), int64(20)}}
	row, err = adaptLegPositional(QueryResult{Positional: aligned}, legType, values.CorrelationIdentifier{})
	if err != nil {
		t.Fatalf("aligned adapt: %v", err)
	}
	if row != values.OrdinalRow(aligned) {
		t.Fatal("an aligned positional row must pass through untouched")
	}

	// Width mismatch (covering row narrower than the leg type): the missing
	// column resolves to nil (SQL NULL) — the row simply doesn't carry it.
	// Never an error — a legitimate plan shape.
	narrow := &PositionalRow{
		Type:  &values.RecordType{Fields: []values.Field{{Name: "V", FieldType: values.NotNullLong, Ordinal: 0}}},
		Slots: []any{int64(20)},
	}
	row, err = adaptLegPositional(QueryResult{Positional: narrow}, legType, values.CorrelationIdentifier{})
	if err != nil {
		t.Fatalf("narrow adapt: %v", err)
	}
	if v, _ := row.Get(0); v != nil {
		t.Fatalf("narrow slot 0 (ID absent from the narrow row) = %v, want nil (SQL NULL)", v)
	}
	if v, _ := row.Get(1); v != int64(20) {
		t.Fatalf("narrow slot 1 = %v, want 20 (V resolved by name)", v)
	}
}

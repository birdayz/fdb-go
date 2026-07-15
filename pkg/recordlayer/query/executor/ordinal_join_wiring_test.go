package executor

import (
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// This file pins the ordinal-join wiring: the ordinal-BIRTH state on the
// NLJ/flatMap cursors, the birth-time predicate context, and the downstream
// leg-window dispatch in executeFilter/executeProjection/
// executePredicatesFilter/executeMap. These tests hand-build the
// plans/cursors rather than driving them through a full query, so each wiring
// point can be pinned in isolation.

// --- shared fixtures ---------------------------------------------------------

// ojWiringLegs builds the wiring fixtures under UPPER-case aliases A/B (the
// executor-visible alias spelling): leg types A[ID,V] / B[ID,W] (dup ID across
// legs, to exercise duplicate-name handling), their typed QOVs, and the
// pristine seed RC.
func ojWiringLegs(t *testing.T) (legA, legB *values.RecordType, qovA, qovB *values.QuantifiedObjectValue, seed *values.RecordConstructorValue) {
	t.Helper()
	legA, legB = ojLegTypeAV(), ojLegTypeBW()
	qovA = values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("A"), legA)
	qovB = values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("B"), legB)
	seed = buildOrdinalJoinRC(t, qovA, qovB)
	return legA, legB, qovA, qovB, seed
}

// ojLegQR builds a scan-shaped leg row: a positional row over rt.
func ojLegQR(t *testing.T, rt *values.RecordType, vals ...any) QueryResult {
	t.Helper()
	if len(vals) != len(rt.Fields) {
		t.Fatalf("ojLegQR: %d values for a %d-field type", len(vals), len(rt.Fields))
	}
	pos := NewPositionalRow(rt)
	for i, v := range vals {
		pos.Set(i, v)
	}
	return QueryResult{Positional: pos}
}

// ojNameQR builds a box-output leg row over the given type. (Every row is a
// PositionalRow; this is an alias of ojLegQR kept for the callers that named
// it, to keep the "this leg's row came from a box/aggregate output, not a
// direct scan" framing visible at the call site.)
func ojNameQR(rt *values.RecordType, vals ...any) QueryResult {
	pos := NewPositionalRow(rt)
	for i, v := range vals {
		if i < len(pos.Slots) {
			pos.Set(i, v)
		}
	}
	return QueryResult{Positional: pos}
}

// ojEqPred builds lhs = rhs.
func ojEqPred(lhs, rhs values.Value) predicates.QueryPredicate {
	return &predicates.ComparisonPredicate{
		Operand:    lhs,
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: rhs},
	}
}

// ojAssertSlots asserts a positional row's slots exactly.
func ojAssertSlots(t *testing.T, pos *PositionalRow, want ...any) {
	t.Helper()
	if pos == nil {
		t.Fatal("row carries no positional row — expected an ordinal-birth emission")
	}
	if len(pos.Slots) != len(want) {
		t.Fatalf("positional row has %d slots, want %d", len(pos.Slots), len(want))
	}
	for i, w := range want {
		got, ok := pos.Get(i)
		if !ok || !reflect.DeepEqual(got, w) {
			t.Fatalf("slot %d = (%v, %v), want (%v, true)", i, got, ok, w)
		}
	}
}

// mustNLJCursor builds an nljCursor, failing the test on a birth-probe error.
// The shared constructor for the pre-existing cursor-mechanics tests (nil
// result value = name-model) and the ordinal-birth wiring tests below.
func mustNLJCursor(
	t *testing.T,
	outer recordlayer.RecordCursor[QueryResult],
	innerRows []QueryResult,
	joinType plans.JoinType,
	outerAlias, innerAlias string,
	preds []predicates.QueryPredicate,
	resultValue values.Value,
	evalCtx *EvaluationContext,
	st *recordlayer.ExecuteState,
) *nljCursor {
	t.Helper()
	c, err := newNLJCursor(outer, innerRows, joinType, outerAlias, innerAlias, preds, resultValue, evalCtx, st)
	if err != nil {
		t.Fatalf("newNLJCursor: %v", err)
	}
	return c
}

// --- ordinalJoinBirth constructor ---------------------------------------------

// TestOrdinalJoinBirth_Constructor pins the once-at-construction
// birth probe: disabled (nil) for nil/lazy result values, enabled with
// windows for the pristine seed, enabled WITHOUT windows for a folded
// projection RC, and a LOUD error for a baked non-RC shape (planner bug —
// never a silent name-model demotion).
func TestOrdinalJoinBirth_Constructor(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)

	t.Run("nil result value disabled", func(t *testing.T) {
		t.Parallel()
		birth, err := newOrdinalJoinBirth(nil, nil)
		if err != nil || birth != nil {
			t.Fatalf("nil rv = (%v, %v), want (nil, nil)", birth, err)
		}
		if birth.enabled() {
			t.Fatal("nil birth must read as disabled")
		}
	})

	t.Run("lazy RC disabled", func(t *testing.T) {
		t.Parallel()
		lazy := values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "ID", Value: values.NewFieldValue(qovA, "ID", values.NotNullLong)},
		)
		birth, err := newOrdinalJoinBirth(lazy, nil)
		if err != nil || birth != nil {
			t.Fatalf("lazy RC = (%v, %v), want (nil, nil) — the name model's RCs are not birth sites", birth, err)
		}
	})

	t.Run("pristine seed enabled with windows", func(t *testing.T) {
		t.Parallel()
		birth, err := newOrdinalJoinBirth(seed, nil)
		if err != nil {
			t.Fatalf("seed birth: %v", err)
		}
		if !birth.enabled() || !birth.WindowsOK {
			t.Fatalf("seed birth = enabled %v, windows %v, want both true", birth.enabled(), birth.WindowsOK)
		}
		if len(birth.Spans) != 2 || birth.Spans[0].Offset != 0 || birth.Spans[0].Width != 2 || birth.Spans[1].Offset != 2 || birth.Spans[1].Width != 2 {
			t.Fatalf("seed spans = %+v, want offsets 0/2 widths 2/2", birth.Spans)
		}
		wantNames := []string{"ID", "V", "ID", "W"}
		if len(birth.OutputType.Fields) != len(wantNames) {
			t.Fatalf("output type has %d fields, want %d", len(birth.OutputType.Fields), len(wantNames))
		}
		for i, w := range wantNames {
			if birth.OutputType.Fields[i].Name != w || birth.OutputType.Fields[i].Ordinal != i {
				t.Fatalf("output field %d = {%q, ord %d}, want {%q, ord %d} — dup names preserved verbatim", i, birth.OutputType.Fields[i].Name, birth.OutputType.Fields[i].Ordinal, w, i)
			}
		}
		for _, id := range []values.CorrelationIdentifier{qovA.Correlation, qovB.Correlation} {
			if _, present := birth.LegTypes[id]; !present {
				t.Fatalf("LegTypes missing leg %s", id)
			}
		}
		if got := len(birth.LegTypes[qovA.Correlation].Fields); got != len(legA.Fields) {
			t.Fatalf("leg A type has %d fields, want %d", got, len(legA.Fields))
		}
		if got := len(birth.LegTypes[qovB.Correlation].Fields); got != len(legB.Fields) {
			t.Fatalf("leg B type has %d fields, want %d", got, len(legB.Fields))
		}
	})

	t.Run("folded RC enabled without windows", func(t *testing.T) {
		t.Parallel()
		bakedAV, err := values.NewFieldValueOfOrdinal(qovA, 1)
		if err != nil {
			t.Fatalf("NewFieldValueOfOrdinal: %v", err)
		}
		folded := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "V", Value: bakedAV},
			values.RecordConstructorField{Name: "C", Value: &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}},
		)
		birth, err := newOrdinalJoinBirth(folded, nil)
		if err != nil {
			t.Fatalf("folded birth: %v", err)
		}
		if !birth.enabled() || birth.WindowsOK {
			t.Fatalf("folded birth = enabled %v, windows %v, want enabled without windows (plain projection row downstream)", birth.enabled(), birth.WindowsOK)
		}
		if _, present := birth.LegTypes[qovA.Correlation]; !present {
			t.Fatal("folded LegTypes must recover leg A from the baked reference")
		}
		if _, present := birth.LegTypes[qovB.Correlation]; present {
			t.Fatal("a leg folded away entirely must be ABSENT from LegTypes — no reference to it can exist")
		}
		if len(birth.OutputType.Fields) != 2 || birth.OutputType.Fields[0].Name != "V" || birth.OutputType.Fields[1].Name != "C" {
			t.Fatalf("folded output type = %v, want [V C]", typeFieldNames(birth.OutputType))
		}
	})

	t.Run("baked non-RC is a loud error", func(t *testing.T) {
		t.Parallel()
		bakedBare, err := values.NewFieldValueOfOrdinal(qovA, 0)
		if err != nil {
			t.Fatalf("NewFieldValueOfOrdinal: %v", err)
		}
		birth, err := newOrdinalJoinBirth(bakedBare, nil)
		if err == nil || birth != nil {
			t.Fatalf("baked non-RC = (%v, %v), want a loud error — an ordinal result value that is not an RC is a planner bug", birth, err)
		}
		if !strings.Contains(err.Error(), "ordinal join birth") {
			t.Fatalf("error %q must identify the ordinal join birth check", err)
		}
	})
}

// --- birth.evaluate -------------------------------------------------------------

// TestOrdinalJoinBirth_Evaluate pins the one-shot birth: positional
// legs flow verbatim into the merged slots; a NIL QueryResult pointer is the
// NULL leg (that side's slots come out NULL); a leg row built by name
// (ojNameQR, e.g. an aggregate/box output) is adapted by the leg type; a
// folded RV (baked ref + constant) evaluates correctly with leg bindings even
// without spans.
func TestOrdinalJoinBirth_Evaluate(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, _, seed := ojWiringLegs(t)
	birth, err := newOrdinalJoinBirth(seed, nil)
	if err != nil {
		t.Fatalf("seed birth: %v", err)
	}
	outerQR := ojLegQR(t, legA, int64(1), int64(10))
	innerQR := ojLegQR(t, legB, int64(2), int64(20))

	t.Run("both legs positional", func(t *testing.T) {
		t.Parallel()
		pos, err := birth.evaluate("A", "B", &outerQR, &innerQR, nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if pos.Type != birth.OutputType {
			t.Fatal("birthed row must carry the birth's single OutputType")
		}
		ojAssertSlots(t, pos, int64(1), int64(10), int64(2), int64(20))
	})

	t.Run("outer nil is the null leg", func(t *testing.T) {
		t.Parallel()
		pos, err := birth.evaluate("A", "B", nil, &innerQR, nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		ojAssertSlots(t, pos, nil, nil, int64(2), int64(20))
	})

	t.Run("inner nil is the null leg", func(t *testing.T) {
		t.Parallel()
		pos, err := birth.evaluate("A", "B", &outerQR, nil, nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		ojAssertSlots(t, pos, int64(1), int64(10), nil, nil)
	})

	t.Run("leg row built by name is adapted by leg type", func(t *testing.T) {
		t.Parallel()
		nameInner := ojNameQR(legB, int64(2), int64(20))
		pos, err := birth.evaluate("A", "B", &outerQR, &nameInner, nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		ojAssertSlots(t, pos, int64(1), int64(10), int64(2), int64(20))
	})

	t.Run("folded RV evaluates with leg bindings", func(t *testing.T) {
		t.Parallel()
		bakedAV, err := values.NewFieldValueOfOrdinal(qovA, 1)
		if err != nil {
			t.Fatalf("NewFieldValueOfOrdinal: %v", err)
		}
		folded := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "V", Value: bakedAV},
			values.RecordConstructorField{Name: "C", Value: &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}},
		)
		fb, err := newOrdinalJoinBirth(folded, nil)
		if err != nil {
			t.Fatalf("folded birth: %v", err)
		}
		// The inner leg is folded away (no LegTypes entry) AND its row was
		// built by name (ojNameQR): adaptLegPositional(qr, nil) — a
		// zero-width row nothing references.
		nameInner := ojNameQR(legB, int64(2), int64(20))
		pos, err := fb.evaluate("A", "B", &outerQR, &nameInner, nil)
		if err != nil {
			t.Fatalf("folded evaluate: %v", err)
		}
		ojAssertSlots(t, pos, int64(10), int64(7))
	})
}

// --- nljCursor end-to-end (no FDB) ----------------------------------------------

// TestNLJCursor_OrdinalBirth_InnerJoin drives an INNER join with the
// ordinal seed RV over two stub legs on the LINEAR path: emitted rows carry
// the correct positional merged row; a lazy leg predicate evaluates correctly
// through the birth-time predicate context (leg-relative against the adapted
// leg rows — correct even for the second leg); a BAKED predicate works
// through the leg bindings and is a loud error without them.
func TestNLJCursor_OrdinalBirth_InnerJoin(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)

	outerRows := []QueryResult{
		ojLegQR(t, legA, int64(1), int64(10)),
		ojLegQR(t, legA, int64(2), int64(20)),
	}
	innerRows := []QueryResult{
		ojLegQR(t, legB, int64(1), int64(100)),
		ojLegQR(t, legB, int64(3), int64(300)),
	}
	lazyPred := ojEqPred(
		values.NewCorrelatedFieldValueWithResolvedOrdinal(qovA, "ID", 0, values.NotNullLong),
		values.NewCorrelatedFieldValueWithResolvedOrdinal(qovB, "ID", 0, values.NotNullLong),
	)

	t.Run("lazy leg predicate + positional emission", func(t *testing.T) {
		t.Parallel()
		c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
			"A", "B", []predicates.QueryPredicate{lazyPred}, seed, EmptyEvaluationContext(), nil)
		defer c.Close()
		results := collectCursor(t, c)
		if len(results) != 1 {
			t.Fatalf("got %d rows, want 1 (only A.ID=1 matches B.ID=1)", len(results))
		}
		// The ordinal birth: the merged positional row.
		ojAssertSlots(t, results[0].Positional, int64(1), int64(10), int64(1), int64(100))
	})

	t.Run("baked predicate through leg bindings", func(t *testing.T) {
		t.Parallel()
		bakedBW, err := values.NewFieldValueOfOrdinal(qovB, 1)
		if err != nil {
			t.Fatalf("NewFieldValueOfOrdinal: %v", err)
		}
		bakedPred := ojEqPred(bakedBW, &values.ConstantValue{Value: int64(100), Typ: values.NotNullLong})
		c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
			"A", "B", []predicates.QueryPredicate{bakedPred}, seed, EmptyEvaluationContext(), nil)
		defer c.Close()
		results := collectCursor(t, c)
		// B.W=100 matches for BOTH outer rows (no leg-A condition).
		if len(results) != 2 {
			t.Fatalf("got %d rows, want 2 (baked B#1=100 matches once per outer row)", len(results))
		}
		ojAssertSlots(t, results[0].Positional, int64(1), int64(10), int64(1), int64(100))
		ojAssertSlots(t, results[1].Positional, int64(2), int64(20), int64(1), int64(100))

		// Even WITHOUT an explicit leg binder, the merged row carries its own
		// leg windows (concatLegPositionals → Type.Legs), so
		// passesJoinPredicatesLegs derives them (spansFromMergedLegs) and the
		// baked leg predicate resolves correctly. B#1=100 over B(1,100) is TRUE.
		combined := mergeRows(outerRows[0], innerRows[0], "A", "B")
		passes, perr := passesJoinPredicatesLegs(combined, []predicates.QueryPredicate{bakedPred}, EmptyEvaluationContext(), nil)
		if perr != nil {
			t.Fatalf("baked predicate over the merged row's own leg windows must resolve, got error %v", perr)
		}
		if !passes {
			t.Fatal("baked B#1=100 over B(1,100) must be TRUE via the merged row's leg windows")
		}
	})
}

// TestNLJCursor_OrdinalBirth_HashPath drives the ordinal birth
// through the HASH join path (≥100 inner rows + a single-column equijoin):
// same positional emission as the linear path.
func TestNLJCursor_OrdinalBirth_HashPath(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)

	innerRows := make([]QueryResult, 120)
	for i := range innerRows {
		innerRows[i] = ojLegQR(t, legB, int64(i), int64(i*10))
	}
	outerRows := []QueryResult{
		ojLegQR(t, legA, int64(5), int64(50)),
		ojLegQR(t, legA, int64(200), int64(2000)), // no hash match
	}
	pred := ojEqPred(
		values.NewCorrelatedFieldValueWithResolvedOrdinal(qovA, "ID", 0, values.NotNullLong),
		values.NewCorrelatedFieldValueWithResolvedOrdinal(qovB, "ID", 0, values.NotNullLong),
	)
	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
		"A", "B", []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
	defer c.Close()
	if c.hashIndex == nil {
		t.Fatal("hash index was not built — this test must exercise the hash path")
	}
	results := collectCursor(t, c)
	if len(results) != 1 {
		t.Fatalf("got %d rows, want 1 (outer ID=5 matches inner ID=5)", len(results))
	}
	ojAssertSlots(t, results[0].Positional, int64(5), int64(50), int64(5), int64(50))
	// Qualified leg references (baked QOV(alias).col) resolve through the
	// merged row's leg windows.
	if v, ok := legRead(results[0].Positional, "A", "ID"); !ok || v != int64(5) {
		t.Fatalf("A.ID = %v, want 5 (leg window)", v)
	}
	if v, ok := legRead(results[0].Positional, "B", "W"); !ok || v != int64(50) {
		t.Fatalf("B.W = %v, want 50 (leg window)", v)
	}
}

// TestNLJCursor_OrdinalBirth_LeftOuterNullLeg pins the
// unmatched-outer LEFT emission: the positional row carries the outer leg
// verbatim and the INNER slots NULL (the nil-pointer NULL leg).
func TestNLJCursor_OrdinalBirth_LeftOuterNullLeg(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)

	outerRows := []QueryResult{
		ojLegQR(t, legA, int64(1), int64(10)),
		ojLegQR(t, legA, int64(2), int64(20)), // unmatched
	}
	innerRows := []QueryResult{ojLegQR(t, legB, int64(1), int64(100))}
	pred := ojEqPred(
		values.NewCorrelatedFieldValueWithResolvedOrdinal(qovA, "ID", 0, values.NotNullLong),
		values.NewCorrelatedFieldValueWithResolvedOrdinal(qovB, "ID", 0, values.NotNullLong),
	)
	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinLeftOuter,
		"A", "B", []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
	defer c.Close()
	results := collectCursor(t, c)
	if len(results) != 2 {
		t.Fatalf("got %d rows, want 2 (matched + null-padded)", len(results))
	}
	ojAssertSlots(t, results[0].Positional, int64(1), int64(10), int64(1), int64(100))
	// The null-padded row: inner slots NULL.
	ojAssertSlots(t, results[1].Positional, int64(2), int64(20), nil, nil)
}

// TestNLJCursor_OrdinalBirth_FullDrain pins the FULL OUTER drain
// emission: an inner row that matched no outer row births with the OUTER
// slots NULL (the symmetric nil-pointer NULL leg).
func TestNLJCursor_OrdinalBirth_FullDrain(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)

	outerRows := []QueryResult{ojLegQR(t, legA, int64(1), int64(10))}
	innerRows := []QueryResult{
		ojLegQR(t, legB, int64(1), int64(100)),
		ojLegQR(t, legB, int64(3), int64(300)), // matches no outer row
	}
	pred := ojEqPred(
		values.NewCorrelatedFieldValueWithResolvedOrdinal(qovA, "ID", 0, values.NotNullLong),
		values.NewCorrelatedFieldValueWithResolvedOrdinal(qovB, "ID", 0, values.NotNullLong),
	)
	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinFullOuter,
		"A", "B", []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
	defer c.Close()
	results := collectCursor(t, c)
	if len(results) != 2 {
		t.Fatalf("got %d rows, want 2 (matched + drained unmatched-inner)", len(results))
	}
	ojAssertSlots(t, results[0].Positional, int64(1), int64(10), int64(1), int64(100))
	// The drain row: outer slots NULL.
	ojAssertSlots(t, results[1].Positional, nil, nil, int64(3), int64(300))
}

// TestNLJCursor_BothSeedShapesEmitPositional pins that BOTH join-seed
// shapes — the ordinal-birth RC and a plain lazy (un-baked) join RC — emit a
// leg-windowed Positional row: mergeRows is Positional-native, so even the
// plain-RC merge births a positional row via concatLegPositionals.
func TestNLJCursor_BothSeedShapesEmitPositional(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)
	outerRows := []QueryResult{
		ojLegQR(t, legA, int64(1), int64(10)),
		ojLegQR(t, legA, int64(2), int64(20)),
	}
	innerRows := []QueryResult{
		ojLegQR(t, legB, int64(1), int64(100)),
		ojLegQR(t, legB, int64(2), int64(200)),
	}
	pred := ojEqPred(
		values.NewCorrelatedFieldValueWithResolvedOrdinal(qovA, "ID", 0, values.NotNullLong),
		values.NewCorrelatedFieldValueWithResolvedOrdinal(qovB, "ID", 0, values.NotNullLong),
	)
	// A LAZY (un-baked) RC join seed — the shape a non-ordinal merge carries.
	lazy := &values.RecordConstructorValue{
		Fields: []values.RecordConstructorField{
			{Name: "ID", Value: values.NewFieldValue(qovA, "ID", values.NotNullLong)},
			{Name: "V", Value: values.NewFieldValue(qovA, "V", values.NotNullLong)},
			{Name: "ID_2", Value: values.NewFieldValue(qovB, "ID", values.NotNullLong)},
			{Name: "W", Value: values.NewFieldValue(qovB, "W", values.NotNullLong)},
		},
	}

	collect := func(rv values.Value) []QueryResult {
		c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
			"A", "B", []predicates.QueryPredicate{pred}, rv, EmptyEvaluationContext(), nil)
		defer c.Close()
		return collectCursor(t, c)
	}
	ordinal := collect(seed)
	lazyResults := collect(lazy)
	if len(ordinal) != 2 || len(lazyResults) != 2 {
		t.Fatalf("got %d ordinal / %d lazy-RC rows, want 2/2", len(ordinal), len(lazyResults))
	}
	for i := range ordinal {
		if ordinal[i].Positional == nil {
			t.Fatalf("ordinal row %d missing positional", i)
		}
		// mergeRows is Positional-NATIVE — a plain lazy-RC merge also emits a
		// leg-windowed Positional (concatLegPositionals, built by parallel
		// construction from the leg rows' own Positionals).
		if lazyResults[i].Positional == nil {
			t.Fatalf("lazy-RC row %d must carry a leg-concat positional row", i)
		}
	}
}

// --- flatMapCursor computeResult -------------------------------------------------

// TestFlatMap_ComputeResult_OrdinalBirth pins the ordinal-birth flatMap: the
// positional row births from the RC with leg bindings (both legs present, and
// the nil inner as the NULL leg).
func TestFlatMap_ComputeResult_OrdinalBirth(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)
	newCursor := func(t *testing.T) *flatMapCursor {
		t.Helper()
		c, err := newFlatMapCursor(nil, nil, nil, nil, EmptyEvaluationContext(),
			qovA.Correlation, qovB.Correlation, seed, false, recordlayer.ExecuteProperties{})
		if err != nil {
			t.Fatalf("newFlatMapCursor: %v", err)
		}
		return c
	}
	outerQR := ojLegQR(t, legA, int64(1), int64(10))
	innerQR := ojLegQR(t, legB, int64(2), int64(20))

	t.Run("both legs", func(t *testing.T) {
		t.Parallel()
		c := newCursor(t)
		got, err := c.computeResult(outerQR, innerQR)
		if err != nil {
			t.Fatalf("computeResult: %v", err)
		}
		ojAssertSlots(t, got.Positional, int64(1), int64(10), int64(2), int64(20))
	})

	t.Run("nil inner is the null leg", func(t *testing.T) {
		t.Parallel()
		c := newCursor(t)
		got, err := c.computeResultLegs(outerQR, nil)
		if err != nil {
			t.Fatalf("computeResultLegs: %v", err)
		}
		ojAssertSlots(t, got.Positional, int64(1), int64(10), nil, nil)
	})
}

// --- downstream dispatch -----------------------------------------------------------

// TestDownstreamLegWindows pins the construction-time spans probe:
// a direct ordinal-seed NLJ/FlatMap input yields spans; each enumerated
// PASSTHROUGH wrapper (sort, in-memory sort, limit, distinct, type filter,
// filter, predicates filter — the cursors that re-emit input rows verbatim)
// unwraps to the join; a non-join input, a folded-RV join, and a
// deliberately-EXCLUDED wrapper (FirstOrDefault fabricates a default row)
// yield false.
func TestDownstreamLegWindows(t *testing.T) {
	t.Parallel()
	_, _, qovA, qovB, seed := ojWiringLegs(t)
	nlj := plans.NewRecordQueryNestedLoopJoinPlan(nil, nil, nil, plans.JoinInner, "A", "B", seed)

	assertSpans := func(t *testing.T, label string, p plans.RecordQueryPlan) {
		t.Helper()
		spans, ok := downstreamLegWindows(p)
		if !ok {
			t.Fatalf("%s: downstreamLegWindows(%T) declined, want the join's spans", label, p)
		}
		if len(spans) != 2 || spans[0].Offset != 0 || spans[0].Width != 2 || spans[1].Offset != 2 || spans[1].Width != 2 {
			t.Fatalf("%s: spans = %+v, want offsets 0/2 widths 2/2", label, spans)
		}
	}

	t.Run("direct NLJ", func(t *testing.T) {
		t.Parallel()
		assertSpans(t, "nlj", nlj)
	})
	t.Run("direct FlatMap", func(t *testing.T) {
		t.Parallel()
		assertSpans(t, "flatmap", plans.NewRecordQueryFlatMapPlan(nil, nil, qovA.Correlation, qovB.Correlation, seed, false))
	})
	t.Run("passthrough wrappers", func(t *testing.T) {
		t.Parallel()
		wrappers := map[string]plans.RecordQueryPlan{
			"sort":              plans.NewRecordQuerySortPlan(nil, nlj),
			"in-memory sort":    plans.NewRecordQueryInMemorySortPlan(nlj, nil),
			"limit":             plans.NewRecordQueryLimitPlan(nlj, 10, 0),
			"distinct":          plans.NewRecordQueryDistinctPlan(nlj),
			"type filter":       plans.NewRecordQueryTypeFilterPlan(nil, nlj),
			"filter":            plans.NewRecordQueryFilterPlan(nil, nlj),
			"predicates filter": plans.NewRecordQueryPredicatesFilterPlan(nlj, nil),
		}
		for name, w := range wrappers {
			assertSpans(t, name, w)
		}
		// Nested passthroughs unwrap transitively.
		assertSpans(t, "limit(distinct(sort))", plans.NewRecordQueryLimitPlan(plans.NewRecordQueryDistinctPlan(plans.NewRecordQuerySortPlan(nil, nlj)), 5, 0))
	})
	t.Run("non-join input declines", func(t *testing.T) {
		t.Parallel()
		if _, ok := downstreamLegWindows(plans.NewRecordQueryMapPlan(nlj, seed)); ok {
			t.Fatal("a Map input must decline — it rewrites rows (and is itself a dispatch site)")
		}
		if _, ok := downstreamLegWindows(nil); ok {
			t.Fatal("a nil input must decline")
		}
	})
	t.Run("folded-RV join declines", func(t *testing.T) {
		t.Parallel()
		bakedAV, err := values.NewFieldValueOfOrdinal(qovA, 1)
		if err != nil {
			t.Fatalf("NewFieldValueOfOrdinal: %v", err)
		}
		folded := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "V", Value: bakedAV},
		)
		foldedNLJ := plans.NewRecordQueryNestedLoopJoinPlan(nil, nil, nil, plans.JoinInner, "A", "B", folded)
		if _, ok := downstreamLegWindows(foldedNLJ); ok {
			t.Fatal("a folded-RV join must decline — its output is a plain projection row, no leg windows")
		}
	})
	t.Run("excluded wrapper declines", func(t *testing.T) {
		t.Parallel()
		if _, ok := downstreamLegWindows(plans.NewRecordQueryFirstOrDefaultPlan(nlj, nil)); ok {
			t.Fatal("FirstOrDefault is deliberately NOT a passthrough (it fabricates a default row) — must decline")
		}
	})
}

// TestLegWindowRowContext pins the REAL downstream context builder against
// the wrong-slot hazard: a lazy SECOND-leg reference over the merged
// positional row resolves correctly through the leg windows, and an outer
// correlation still resolves via the base EvaluationContext binder.
func TestLegWindowRowContext(t *testing.T) {
	t.Parallel()
	_, _, _, qovB, seed := ojWiringLegs(t)
	spans, mergedType, ok := ordinalJoinSpans(seed)
	if !ok {
		t.Fatal("ordinalJoinSpans rejected the seed")
	}
	merged := ojMergedRow(t, mergedType) // [A.ID=1, A.V=10, B.ID=2, B.W=20]

	outerID := values.NamedCorrelationIdentifier("OUT")
	// An outer correlation binds a PositionalRow (production binds
	// qualifyOuterPositional), so FieldValue(QOV(OUT), "X") resolves "X" by name
	// against the ordinal row.
	ec := EmptyEvaluationContext().WithBinding(outerID, &PositionalRow{
		Type:  positionalTypeFromNames([]string{"X"}),
		Slots: []any{int64(42)},
	})
	rowCtx := legWindowRowContext(merged, ec, spans)

	// The hazard pin, through the REAL builder: B.W's source-relative
	// ordinal is 1 — the bare merged row would misread absolute slot 1
	// (A.V=10); the window reads merged slot 3 = 20.
	bwRef := values.NewCorrelatedFieldValueWithResolvedOrdinal(qovB, "W", 1, values.NotNullLong)
	got, err := bwRef.Evaluate(rowCtx)
	if err != nil {
		t.Fatalf("B.W through legWindowRowContext: %v", err)
	}
	if got != int64(20) {
		t.Fatalf("lazy B.W = %v, want 20 (B's W through the leg window, not A's V at absolute slot 1)", got)
	}
	// A baked second-leg reference reads the same correct slot.
	bakedBID, err := values.NewFieldValueOfOrdinal(qovB, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	if got, err := bakedBID.Evaluate(rowCtx); err != nil || got != int64(2) {
		t.Fatalf("baked B#0 = (%v, %v), want (2, nil)", got, err)
	}
	// An OUTER correlation delegates through the base EvaluationContext.
	outerRef := values.NewCorrelatedFieldValueWithResolvedOrdinal(values.NewQuantifiedObjectValue(outerID), "X", 0, values.NotNullLong)
	if got, err := outerRef.Evaluate(rowCtx); err != nil || got != int64(42) {
		t.Fatalf("outer correlation OUT.X = (%v, %v), want (42, nil) — must resolve via the base binder", got, err)
	}
}

// TestNLJ_FoldedRVDroppedLeg_PredTypes pins a correctness trap: a FOLDED
// result value can DROP a leg entirely while a baked cross-leg ON predicate
// still references it — LegTypes derived from the RV alone misses the
// dropped leg, so a leg row built by name (e.g. an aggregate/box shape,
// carrying no positional layout of its own) adapted with a nil leg type
// became a ZERO-WIDTH binding and the baked predicate blew up (loud
// OrdinalResolutionError on a legitimate plan). LegTypes must be collected
// from the result value AND the predicates.
func TestNLJ_FoldedRVDroppedLeg_PredTypes(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, _ := ojWiringLegs(t)

	// Folded RV: only leg A appears ({A.V baked, const}) — leg B dropped.
	bakedAV, err := values.NewFieldValueOfOrdinal(qovA, 1)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	foldedRV := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "V", Value: bakedAV},
		values.RecordConstructorField{Name: "_1", Value: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}},
	)

	// Baked cross-leg ON predicate: A.ID = B.ID — references the DROPPED leg.
	bakedAID, err := values.NewFieldValueOfOrdinal(qovA, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	bakedBID, err := values.NewFieldValueOfOrdinal(qovB, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	pred := ojEqPred(bakedAID, bakedBID)

	outerRows := []QueryResult{
		ojLegQR(t, legA, int64(1), int64(10)),
		ojLegQR(t, legA, int64(2), int64(20)),
	}
	// Leg B rows are built by name (an aggregate-box shape): the adapter must
	// synthesize them from the PREDICATE-derived leg type.
	innerRows := []QueryResult{
		ojNameQR(legB, int64(1), int64(100)),
		ojNameQR(legB, int64(3), int64(300)),
	}

	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
		"A", "B", []predicates.QueryPredicate{pred}, foldedRV, EmptyEvaluationContext(), nil)
	defer c.Close()
	results := collectCursor(t, c)
	if len(results) != 1 {
		t.Fatalf("got %d rows, want 1 (only A.ID=1 matches B.ID=1) — a dropped-leg zero-width binding misfilters or errors", len(results))
	}
	// The folded output row: [A.V, 1].
	ojAssertSlots(t, results[0].Positional, int64(10), int64(1))
}

// TestFlatMap_FoldedRVDroppedLeg_PlanTypes pins the FlatMap side of the same
// correctness trap: the correlated FlatMap implementation pushes the gated
// join's baked ON references INTO the inner plan, so a folded result value
// that DROPS the outer leg leaves the birth typeless for it even though the
// inner plan still references it — a leg row built by name (aggregate-box
// shape) then bound zero-width and the baked SARG died loudly on a
// legitimate plan. newFlatMapCursor must widen LegTypes from the inner
// plan's predicate surfaces.
func TestFlatMap_FoldedRVDroppedLeg_PlanTypes(t *testing.T) {
	t.Parallel()
	legA, _, qovA, qovB, _ := ojWiringLegs(t)
	outerCorr := values.NamedCorrelationIdentifier("A")

	// Folded RV: only leg B appears — the OUTER leg A dropped.
	bakedBW, err := values.NewFieldValueOfOrdinal(qovB, 1)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	foldedRV := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "W", Value: bakedBW},
	)

	// The inner plan carries the baked ON reference to the dropped OUTER leg
	// (a residual PredicatesFilter — the correlated-implementation shape).
	bakedAID, err := values.NewFieldValueOfOrdinal(qovA, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	bakedBID, err := values.NewFieldValueOfOrdinal(qovB, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	innerPlan := plans.NewRecordQueryPredicatesFilterPlan(
		plans.NewRecordQueryScanPlan(nil, values.UnknownType, false),
		[]predicates.QueryPredicate{ojEqPred(bakedBID, bakedAID)},
	)

	c, err := newFlatMapCursor(nil, nil, innerPlan, nil, EmptyEvaluationContext(),
		outerCorr, values.NamedCorrelationIdentifier("B"), foldedRV, false,
		recordlayer.ExecuteProperties{})
	if err != nil {
		t.Fatalf("newFlatMapCursor: %v", err)
	}
	// The birth must know the dropped OUTER leg's type from the inner plan's
	// baked reference…
	outerType := c.birth.legType(outerCorr)
	if outerType == nil {
		t.Fatal("LegTypes missing the RV-dropped OUTER leg — the inner plan's baked SARG/residual references it")
	}
	if len(outerType.Fields) != len(legA.Fields) {
		t.Fatalf("outer leg type has %d fields, want %d", len(outerType.Fields), len(legA.Fields))
	}
	// …so a leg row built by name (aggregate-box shape) adapts to a
	// full-width binding the baked reference can read — not the zero-width
	// death row.
	adapted, aerr := adaptLegPositional(ojNameQR(legA, int64(7), int64(70)), outerType)
	if aerr != nil {
		t.Fatalf("adaptLegPositional: %v", aerr)
	}
	if v, ok := adapted.Get(0); !ok || v != int64(7) {
		t.Fatalf("adapted outer slot 0 = (%v, %v), want (7, true) — zero-width binding means the widening failed", v, ok)
	}
}

// TestLegWindowBinder_BoxAliasReadsLeaf pins the leg-window binder's
// box-span precedence: a BOX span is NAMED by its rightmost LEAF, so a
// correlated baked reference through the box name must window that LEAF's
// slice — never the whole concat, where a leg-local ordinal would land on an
// earlier BURIED leg's slot (silently the wrong column). The fixture's
// buried leg B deliberately carries an "ID" column ahead of the leaf's own
// "ID": a whole-concat window for "E.ID" reads B's.
func TestLegWindowBinder_BoxAliasReadsLeaf(t *testing.T) {
	t.Parallel()
	corrA := values.NamedCorrelationIdentifier("A")
	corrE := values.NamedCorrelationIdentifier("E")
	legA := &values.RecordType{Fields: []values.Field{{Name: "AID", FieldType: values.NotNullLong, Ordinal: 0}}}
	boxTyp := &values.RecordType{
		Fields: []values.Field{
			{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1}, // buried B's ID — the first-match trap
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 2}, // the leaf E's own ID
		},
		Legs: []values.RecordTypeLeg{{Name: "B", Start: 0, Width: 2}, {Name: "E", Start: 2, Width: 1}},
	}
	qovA := values.NewQuantifiedObjectValueOfType(corrA, legA)
	qovE := values.NewQuantifiedObjectValueOfType(corrE, boxTyp)
	var fields []values.RecordConstructorField
	for i := 0; i < 1; i++ {
		fv, err := values.NewFieldValueOfOrdinal(qovA, i)
		if err != nil {
			t.Fatalf("bake A#%d: %v", i, err)
		}
		fields = append(fields, values.RecordConstructorField{Name: fv.Field, Value: fv})
	}
	for i := 0; i < 3; i++ {
		fv, err := values.NewFieldValueOfOrdinal(qovE, i)
		if err != nil {
			t.Fatalf("bake E#%d: %v", i, err)
		}
		fields = append(fields, values.RecordConstructorField{Name: fv.Field, Value: fv})
	}
	rc := values.NewRawRecordConstructorValue(fields...)
	spans, mergedType, ok := ordinalJoinSpans(rc)
	if !ok {
		t.Fatal("fixture RC rejected by ordinalJoinSpans")
	}
	row := NewPositionalRow(mergedType)
	row.Set(0, int64(10)) // a.aid
	row.Set(1, int64(20)) // b.bid
	row.Set(2, int64(21)) // b.id — the trap value
	row.Set(3, int64(30)) // e.id — the correct read

	// The production resolution: a source-relative BAKED reference over the
	// leg's QOV, bound through the legWindowBinder (a leg-local ordinal reads
	// its OWN window's slot).
	binder := &legWindowBinder{spans: spans, row: row}
	read := func(alias, col string, legOrd int) (any, bool) {
		fv := values.NewCorrelatedFieldValueWithResolvedOrdinal(
			values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(alias)),
			col, legOrd, values.UnknownType)
		v, err := fv.Evaluate(&values.RowEvalContext{Correlations: binder})
		if err != nil {
			return nil, false
		}
		return v, true
	}
	// E.ID: leg-local ordinal 0 within the LEAF's own 1-column window — must
	// read the leaf's slot (30), not the buried dup at the box's slot 1 (21).
	if v, found := read("E", "ID", 0); !found || v != int64(30) {
		t.Fatalf("E.ID = (%v, %v), want the rightmost LEAF's column (30), not the buried dup (21)", v, found)
	}
	// B.ID: leg-local ordinal 1 within the BURIED leg's window ([BID, ID]).
	if v, found := read("B", "ID", 1); !found || v != int64(21) {
		t.Fatalf("B.ID = (%v, %v), want the buried leg's column (21)", v, found)
	}
	if v, found := read("B", "BID", 0); !found || v != int64(20) {
		t.Fatalf("B.BID = (%v, %v), want 20", v, found)
	}
	if v, found := read("A", "AID", 0); !found || v != int64(10) {
		t.Fatalf("A.AID = (%v, %v), want 10", v, found)
	}
}

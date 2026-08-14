package executor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// This file pins the ordinal-join wiring: the ordinal-BUILD state on the
// NLJ/flatMap cursors, the build-time predicate context, and the downstream
// leg-window dispatch in executeFilter/executeProjection/
// executePredicatesFilter/executeMap. These tests hand-build the
// plans/cursors rather than driving them through a full query, so each wiring
// point can be pinned in isolation.

// --- shared fixtures ---------------------------------------------------------

func ojWiringMustConstruct[T any](t testing.TB, value T, err error) T {
	t.Helper()
	if err != nil {
		t.Fatalf("construct exact ordinal-wiring fixture: %v", err)
	}
	return value
}

func ojWiringMustQOV(
	t testing.TB,
	alias values.CorrelationIdentifier,
	typ values.Type,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(alias, typ)
	return ojWiringMustConstruct(t, qov, err)
}

func ojWiringMustField(t testing.TB, child values.Value, ordinals ...int) values.Value {
	t.Helper()
	field, err := values.ResolveFieldOrdinals(child, ordinals)
	return ojWiringMustConstruct(t, field, err)
}

func ojWiringMustSeedField(t testing.TB, child values.Value, ordinal int) values.Value {
	t.Helper()
	field, err := values.ResolveOrdinalSeedField(child, ordinal)
	return ojWiringMustConstruct(t, field, err)
}

func ojWiringLegTypeAV() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
	})
}

func ojWiringLegTypeBW() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "W", FieldType: values.NotNullLong, Ordinal: 1},
	})
}

func ojWiringBuildSeed(t testing.TB, qovs ...values.QuantifiedObjectValue) *values.RecordConstructorValue {
	t.Helper()
	var fields []values.RecordConstructorField
	for _, qov := range qovs {
		rt, ok := qov.FlowedType().(*values.RecordType)
		if !ok || rt == nil {
			t.Fatalf("seed leg %s flows %T, want exact RecordType", qov.Correlation(), qov.FlowedType())
		}
		for i := range rt.Fields {
			value := ojWiringMustSeedField(t, qov, i)
			field, ok := values.AsFieldValue(value)
			if !ok {
				t.Fatalf("seed field %s#%d = %T, want exact FieldValue", qov.Correlation(), i, value)
			}
			fields = append(fields, values.RecordConstructorField{Name: field.DisplayName(), Value: field})
		}
	}
	return values.NewRawRecordConstructorValue(fields...)
}

func ojCollectCursor(t testing.TB, c recordlayer.RecordCursor[QueryResult]) []QueryResult {
	t.Helper()
	var out []QueryResult
	for {
		result, err := c.OnNext(context.Background())
		if err != nil {
			t.Fatalf("OnNext: %v", err)
		}
		if !result.HasNext() {
			return out
		}
		out = append(out, result.GetValue())
	}
}

func ojWiringLegRead(t testing.TB, pos *PositionalRow, alias, column string) (any, bool) {
	t.Helper()
	if pos == nil || pos.Type == nil {
		return nil, false
	}
	for _, leg := range pos.Type.Legs {
		if !strings.EqualFold(leg.Name, alias) {
			continue
		}
		end := leg.Start + leg.Width
		if leg.Start < 0 || end > len(pos.Type.Fields) {
			return nil, false
		}
		for absolute := leg.Start; absolute < end; absolute++ {
			if !strings.EqualFold(pos.Type.Fields[absolute].Name, column) {
				continue
			}
			legType := &values.RecordType{Fields: append([]values.Field(nil), pos.Type.Fields[leg.Start:end]...)}
			for i := range legType.Fields {
				legType.Fields[i].Ordinal = i
			}
			qov := ojWiringMustQOV(t, values.NamedCorrelationIdentifier(alias), legType)
			field := ojWiringMustField(t, qov, absolute-leg.Start)
			rowCtx, err := frontierRowContext(pos, nil, false)
			if err != nil {
				return nil, false
			}
			value, err := field.Evaluate(rowCtx)
			return value, err == nil
		}
		return nil, false
	}
	return nil, false
}

// ojWiringLegs builds the wiring fixtures under UPPER-case aliases A/B (the
// executor-visible alias spelling): leg types A[ID,V] / B[ID,W] (dup ID across
// legs, to exercise duplicate-name handling), their typed QOVs, and the
// pristine seed RC.
func ojWiringLegs(t *testing.T) (legA, legB *values.RecordType, qovA, qovB values.QuantifiedObjectValue, seed *values.RecordConstructorValue) {
	t.Helper()
	legA, legB = ojWiringLegTypeAV(), ojWiringLegTypeBW()
	qovA = ojWiringMustQOV(t, values.NamedCorrelationIdentifier("A"), legA)
	qovB = ojWiringMustQOV(t, values.NamedCorrelationIdentifier("B"), legB)
	seed = ojWiringBuildSeed(t, qovA, qovB)
	return legA, legB, qovA, qovB, seed
}

func ojWiringOuterJoinSources(
	t testing.TB,
	joinType plans.JoinType,
) (legA, legB *values.RecordType, qovA, qovB values.QuantifiedObjectValue, seed *values.RecordConstructorValue) {
	t.Helper()
	legA, legB = ojWiringLegTypeAV(), ojWiringLegTypeBW()
	var flowedA values.Type = legA
	var flowedB values.Type = legB
	if joinType == plans.JoinFullOuter {
		flowedA = values.WithNullability(flowedA, true)
	}
	if joinType == plans.JoinLeftOuter || joinType == plans.JoinFullOuter {
		flowedB = values.WithNullability(flowedB, true)
	}
	qovA = ojWiringMustQOV(t, values.NamedCorrelationIdentifier("A"), flowedA)
	qovB = ojWiringMustQOV(t, values.NamedCorrelationIdentifier("B"), flowedB)
	seed = ojWiringBuildSeed(t, qovA, qovB)
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
		t.Fatal("row carries no positional row — expected an ordinal-build emission")
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

// mustNLJCursor builds an nljCursor, failing the test on a build-probe error.
// The shared constructor for the pre-existing cursor-mechanics tests (nil
// result value = name-model) and the ordinal-build wiring tests below.
func mustNLJCursor(
	t *testing.T,
	outer recordlayer.RecordCursor[QueryResult],
	innerRows []QueryResult,
	joinType plans.JoinType,
	outerAlias, innerAlias values.CorrelationIdentifier,
	preds []predicates.QueryPredicate,
	resultValue values.Value,
	evalCtx *EvaluationContext,
	st *recordlayer.ExecuteState,
) *nljCursor {
	t.Helper()
	c, err := newNLJCursor(
		outer, innerRows, joinType, outerAlias, innerAlias, preds, resultValue,
		nil, nil, evalCtx, st)
	if err != nil {
		t.Fatalf("newNLJCursor: %v", err)
	}
	return c
}

// --- ordinalJoinBuild constructor ---------------------------------------------

// TestOrdinalJoinBuild_Constructor pins the once-at-construction
// build probe: disabled (nil) for nil/lazy result values, enabled with
// windows for the pristine seed, enabled WITHOUT windows for a folded
// projection RC, and a LOUD error for a baked non-RC shape (planner bug —
// never a silent name-model demotion).
func TestOrdinalJoinBuild_Constructor(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)

	t.Run("nil result value disabled", func(t *testing.T) {
		t.Parallel()
		build, err := newOrdinalJoinBuild(nil, nil)
		if err != nil || build != nil {
			t.Fatalf("nil rv = (%v, %v), want (nil, nil)", build, err)
		}
		if build.enabled() {
			t.Fatal("nil build must read as disabled")
		}
	})

	t.Run("ordinary semantic RC disabled", func(t *testing.T) {
		t.Parallel()
		ordinary := values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "ID", Value: ojWiringMustField(t, qovA, 0)},
		)
		build, err := newOrdinalJoinBuild(ordinary, nil)
		if err != nil || build != nil {
			t.Fatalf("ordinary RC = (%v, %v), want (nil, nil) — semantic field reads are not ordinal build sites", build, err)
		}
	})

	t.Run("pristine seed enabled with windows", func(t *testing.T) {
		t.Parallel()
		build, err := newOrdinalJoinBuild(seed, nil)
		if err != nil {
			t.Fatalf("seed build: %v", err)
		}
		if !build.enabled() || !build.WindowsOK {
			t.Fatalf("seed build = enabled %v, windows %v, want both true", build.enabled(), build.WindowsOK)
		}
		if len(build.Spans) != 2 || build.Spans[0].Offset != 0 || build.Spans[0].Width != 2 || build.Spans[1].Offset != 2 || build.Spans[1].Width != 2 {
			t.Fatalf("seed spans = %+v, want offsets 0/2 widths 2/2", build.Spans)
		}
		wantNames := []string{"ID", "V", "ID", "W"}
		if len(build.OutputType.Fields) != len(wantNames) {
			t.Fatalf("output type has %d fields, want %d", len(build.OutputType.Fields), len(wantNames))
		}
		for i, w := range wantNames {
			if build.OutputType.Fields[i].Name != w || build.OutputType.Fields[i].Ordinal != i {
				t.Fatalf("output field %d = {%q, ord %d}, want {%q, ord %d} — dup names preserved verbatim", i, build.OutputType.Fields[i].Name, build.OutputType.Fields[i].Ordinal, w, i)
			}
		}
		for _, id := range []values.CorrelationIdentifier{qovA.Correlation(), qovB.Correlation()} {
			if _, present := build.LegTypes[id]; !present {
				t.Fatalf("LegTypes missing leg %s", id)
			}
		}
		if got := len(build.LegTypes[qovA.Correlation()].Fields); got != len(legA.Fields) {
			t.Fatalf("leg A type has %d fields, want %d", got, len(legA.Fields))
		}
		if got := len(build.LegTypes[qovB.Correlation()].Fields); got != len(legB.Fields) {
			t.Fatalf("leg B type has %d fields, want %d", got, len(legB.Fields))
		}
	})

	t.Run("folded RC enabled without windows", func(t *testing.T) {
		t.Parallel()
		bakedAV := ojWiringMustSeedField(t, qovA, 1)
		folded := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "V", Value: bakedAV},
			values.RecordConstructorField{Name: "C", Value: &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}},
		)
		build, err := newOrdinalJoinBuild(folded, nil)
		if err != nil {
			t.Fatalf("folded build: %v", err)
		}
		if !build.enabled() || build.WindowsOK {
			t.Fatalf("folded build = enabled %v, windows %v, want enabled without windows (plain projection row downstream)", build.enabled(), build.WindowsOK)
		}
		if _, present := build.LegTypes[qovA.Correlation()]; !present {
			t.Fatal("folded LegTypes must recover leg A from the baked reference")
		}
		if _, present := build.LegTypes[qovB.Correlation()]; present {
			t.Fatal("a leg folded away entirely must be ABSENT from LegTypes — no reference to it can exist")
		}
		if len(build.OutputType.Fields) != 2 || build.OutputType.Fields[0].Name != "V" || build.OutputType.Fields[1].Name != "C" {
			t.Fatalf("folded output type = %v, want [V C]", typeFieldNames(build.OutputType))
		}
	})

	// A BARE (non-RC) baked result value is a LEGITIMATE select output, not a
	// malformed plan: PartitionSelectRule's single-live-lower arm flows one leg's
	// whole row (PartitionSelectRule.java:281) and a later positional-merge round
	// translates that bare QOV into `ofOrdinal(QOV(merge), i)` (:319), which then
	// reaches RecordQueryFlatMapPlan verbatim (ImplementNestedLoopJoinRule.java:187,
	// 201,214). The build must stay ENABLED for it — see Bare. What must NOT
	// happen is a nil build: that routes the cursor down the non-build path, which
	// binds the outer by name instead of adapting it to the merge layout the
	// inner's pushed SARGs read by ordinal, and returns zero rows.
	t.Run("baked non-RC builds enabled on the Bare arm", func(t *testing.T) {
		t.Parallel()
		bakedBare := ojWiringMustSeedField(t, qovA, 0)
		build, err := newOrdinalJoinBuild(bakedBare, nil)
		if err != nil {
			t.Fatalf("bare baked build: %v — a bare baked result value is a legitimate select output", err)
		}
		if !build.enabled() {
			t.Fatal("bare baked build must be ENABLED: a nil build routes the cursor down the " +
				"non-build path, which binds the outer by NAME rather than adapting it to the " +
				"merge layout the inner's pushed-down SARGs read by ordinal — measured as zero " +
				"rows on TestFDB_CommaJoin3ProjectedExistsWithEquijoins")
		}
		if build.RC != nil || build.Bare != bakedBare {
			t.Fatalf("bare build = {RC: %v, Bare: %v}, want RC nil and Bare the result value — the two are mutually exclusive", build.RC, build.Bare)
		}
		if build.WindowsOK {
			t.Fatal("a bare result value publishes NO leg windows: its output is one flowed value, not a merge")
		}
		if _, present := build.LegTypes[qovA.Correlation()]; !present {
			t.Fatal("bare LegTypes must recover leg A from the baked reference — the leg still has to adapt")
		}
	})

	// The bare arm's OUTPUT SHAPE follows from what the value evaluated to, never
	// from a plan flag. A whole-leg reference yields the leg's ROW and that row
	// flows through as ITSELF; re-wrapping it in a 1-slot row would double-nest it
	// and a downstream ordinal read of column i would land on the whole record.
	t.Run("bare arm flows a row through and wraps a scalar", func(t *testing.T) {
		t.Parallel()
		mergedLeg := &values.RecordType{Fields: []values.Field{
			{Name: "_0", FieldType: legA, Ordinal: 0},
			{Name: "_1", FieldType: legB, Ordinal: 1},
		}}
		mergeQOV := ojWiringMustQOV(t, values.NamedCorrelationIdentifier("m"), mergedLeg)
		wholeLeg := ojWiringMustSeedField(t, mergeQOV, 0)
		build, err := newOrdinalJoinBuild(wholeLeg, nil)
		if err != nil {
			t.Fatalf("bare build: %v", err)
		}
		legRow := NewPositionalRow(legA)
		legRow.Slots[0] = int64(7)
		legRow.Slots[1] = int64(9)
		mergeRow := NewPositionalRow(mergedLeg)
		mergeRow.Slots[0] = legRow
		got, err := build.evaluateBound(&twoLegBinder{
			outerID: values.NamedCorrelationIdentifier("m"),
			outer:   mergeRow,
		})
		if err != nil {
			t.Fatalf("bare evaluate: %v", err)
		}
		if got != legRow {
			t.Fatalf("bare evaluate = %v, want the leg row ITSELF (%v) — wrapping it in a 1-slot row "+
				"double-nests it, and a downstream ordinal read of column i then lands on the whole record", got, legRow)
		}

		// A bare reference naming a COLUMN, not a leg, is a scalar output and wraps
		// into the 1-slot row every other scalar-output path uses.
		col := ojWiringMustSeedField(t, qovA, 1)
		scalarBuild, err := newOrdinalJoinBuild(col, nil)
		if err != nil {
			t.Fatalf("scalar bare build: %v", err)
		}
		aRow := NewPositionalRow(legA)
		aRow.Slots[0] = int64(1)
		aRow.Slots[1] = int64(42)
		scalarGot, err := scalarBuild.evaluateBound(&twoLegBinder{outerID: qovA.Correlation(), outer: aRow})
		if err != nil {
			t.Fatalf("scalar bare evaluate: %v", err)
		}
		if len(scalarGot.Slots) != 1 || scalarGot.Slots[0] != int64(42) {
			t.Fatalf("scalar bare evaluate = %v, want a 1-slot row holding 42", scalarGot.Slots)
		}
	})
}

// --- build.evaluate -------------------------------------------------------------

// TestOrdinalJoinBuild_Evaluate pins the one-shot build: positional
// legs flow verbatim into the merged slots; a NIL QueryResult pointer is the
// NULL leg (that side's slots come out NULL); a leg row built by name
// (ojNameQR, e.g. an aggregate/box output) is adapted by the leg type; a
// folded RV (baked ref + constant) evaluates correctly with leg bindings even
// without spans.
func TestOrdinalJoinBuild_Evaluate(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, _, seed := ojWiringLegs(t)
	build, err := newOrdinalJoinBuild(seed, nil)
	if err != nil {
		t.Fatalf("seed build: %v", err)
	}
	outerQR := ojLegQR(t, legA, int64(1), int64(10))
	innerQR := ojLegQR(t, legB, int64(2), int64(20))

	t.Run("both legs positional", func(t *testing.T) {
		t.Parallel()
		pos, err := build.evaluate(values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), &outerQR, &innerQR, nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if pos.Type != build.OutputType {
			t.Fatal("built row must carry the build's single OutputType")
		}
		ojAssertSlots(t, pos, int64(1), int64(10), int64(2), int64(20))
	})

	t.Run("outer nil is the null leg", func(t *testing.T) {
		t.Parallel()
		pos, err := build.evaluate(values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), nil, &innerQR, nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		ojAssertSlots(t, pos, nil, nil, int64(2), int64(20))
	})

	t.Run("inner nil is the null leg", func(t *testing.T) {
		t.Parallel()
		pos, err := build.evaluate(values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), &outerQR, nil, nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		ojAssertSlots(t, pos, int64(1), int64(10), nil, nil)
	})

	t.Run("leg row built by name is adapted by leg type", func(t *testing.T) {
		t.Parallel()
		nameInner := ojNameQR(legB, int64(2), int64(20))
		pos, err := build.evaluate(values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), &outerQR, &nameInner, nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		ojAssertSlots(t, pos, int64(1), int64(10), int64(2), int64(20))
	})

	t.Run("folded RV evaluates with leg bindings", func(t *testing.T) {
		t.Parallel()
		bakedAV := ojWiringMustSeedField(t, qovA, 1)
		folded := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "V", Value: bakedAV},
			values.RecordConstructorField{Name: "C", Value: &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}},
		)
		fb, err := newOrdinalJoinBuild(folded, nil)
		if err != nil {
			t.Fatalf("folded build: %v", err)
		}
		// The inner leg is folded away (no LegTypes entry) AND its row was
		// built by name (ojNameQR): adaptLegPositional(qr, nil) — a
		// zero-width row nothing references.
		nameInner := ojNameQR(legB, int64(2), int64(20))
		pos, err := fb.evaluate(values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), &outerQR, &nameInner, nil)
		if err != nil {
			t.Fatalf("folded evaluate: %v", err)
		}
		ojAssertSlots(t, pos, int64(10), int64(7))
	})
}

// --- nljCursor end-to-end (no FDB) ----------------------------------------------

// TestNLJCursor_OrdinalBuild_InnerJoin drives an INNER join with the
// ordinal seed RV over two stub legs on the LINEAR path: emitted rows carry
// the correct positional merged row; a lazy leg predicate evaluates correctly
// through the build-time predicate context (leg-relative against the adapted
// leg rows — correct even for the second leg); a BAKED predicate works
// through the leg bindings and is a loud error without them.
func TestNLJCursor_OrdinalBuild_InnerJoin(t *testing.T) {
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
		ojWiringMustField(t, qovA, 0),
		ojWiringMustField(t, qovB, 0),
	)

	t.Run("lazy leg predicate + positional emission", func(t *testing.T) {
		t.Parallel()
		c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
			values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), []predicates.QueryPredicate{lazyPred}, seed, EmptyEvaluationContext(), nil)
		defer c.Close()
		results := ojCollectCursor(t, c)
		if len(results) != 1 {
			t.Fatalf("got %d rows, want 1 (only A.ID=1 matches B.ID=1)", len(results))
		}
		// The ordinal build: the merged positional row.
		ojAssertSlots(t, results[0].Positional, int64(1), int64(10), int64(1), int64(100))
	})

	t.Run("baked predicate through leg bindings", func(t *testing.T) {
		t.Parallel()
		bakedBW := ojWiringMustField(t, qovB, 1)
		bakedPred := ojEqPred(bakedBW, &values.ConstantValue{Value: int64(100), Typ: values.NotNullLong})
		c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
			values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), []predicates.QueryPredicate{bakedPred}, seed, EmptyEvaluationContext(), nil)
		defer c.Close()
		results := ojCollectCursor(t, c)
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
		combined := mergeRows(outerRows[0], innerRows[0], values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"))
		passes, perr := passesJoinPredicatesLegs(combined, []predicates.QueryPredicate{bakedPred}, EmptyEvaluationContext(), nil)
		if perr != nil {
			t.Fatalf("baked predicate over the merged row's own leg windows must resolve, got error %v", perr)
		}
		if !passes {
			t.Fatal("baked B#1=100 over B(1,100) must be TRUE via the merged row's leg windows")
		}
	})
}

func TestJoinPredicateContexts_PreserveLocalExactLegOwnership(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)
	outer := ojLegQR(t, legA, int64(1), int64(10))
	inner := ojLegQR(t, legB, int64(2), int64(20))
	combined := mergeRows(outer, inner, qovA.Correlation(), qovB.Correlation())

	staleA := NewPositionalRow(legA)
	staleA.Slots[0], staleA.Slots[1] = int64(999), int64(9990)
	ambient, err := EmptyEvaluationContext().withQuantifiedBinding(qovA, staleA, false)
	if err != nil {
		t.Fatalf("ambient exact binding: %v", err)
	}

	assertLocalA := func(t *testing.T, ctx *values.RowEvalContext) {
		t.Helper()
		got, evalErr := ojWiringMustField(t, qovA, 0).Evaluate(ctx)
		if evalErr != nil || got != int64(1) {
			t.Fatalf("local A.ID = (%v, %v), want (1, nil); ambient exact A held 999", got, evalErr)
		}
	}
	assertForeignTypeRejected := func(t *testing.T, ctx *values.RowEvalContext) {
		t.Helper()
		foreignType := values.NewRecordType("FOREIGN_A", false, []values.Field{
			{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
			{Name: "V", Ordinal: 1, FieldType: values.NotNullLong},
		})
		foreign := ojWiringMustQOV(t, qovA.Correlation(), foreignType)
		got, evalErr := foreign.Evaluate(ctx)
		var coded interface {
			Code() values.ResolutionErrorCode
		}
		if got != nil || !errors.As(evalErr, &coded) || coded.Code() != values.CorrelationTypeConflict {
			t.Fatalf("same-alias foreign exact type = (%v, %v), want CorrelationTypeConflict", got, evalErr)
		}
	}

	t.Run("merged leg windows shadow ambient exact values", func(t *testing.T) {
		spans, _, ok := ordinalJoinSpans(seed)
		if !ok {
			t.Fatal("pristine seed did not expose leg windows")
		}
		ctx := legWindowRowContext(combined.Positional, ambient, spans)
		assertLocalA(t, ctx)
		assertForeignTypeRejected(t, ctx)
	})

	t.Run("build pair shadows ambient exact values", func(t *testing.T) {
		pair := &twoLegBinder{
			outerID: qovA.Correlation(), innerID: qovB.Correlation(),
			outer: outer.Positional, inner: inner.Positional,
			outerType: legA, innerType: legB,
			base: ambient,
		}
		ctx := &values.RowEvalContext{
			Positional: combined.Positional, Objects: pair, Correlations: pair,
		}
		assertLocalA(t, ctx)
		assertForeignTypeRejected(t, ctx)
	})
}

// TestNLJCursor_OrdinalBuild_HashPath drives the ordinal build
// through the HASH join path (≥100 inner rows + a single-column equijoin):
// same positional emission as the linear path.
func TestNLJCursor_OrdinalBuild_HashPath(t *testing.T) {
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
		ojWiringMustField(t, qovA, 0),
		ojWiringMustField(t, qovB, 0),
	)
	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
	defer c.Close()
	if c.hashIndex == nil {
		t.Fatal("hash index was not built — this test must exercise the hash path")
	}
	results := ojCollectCursor(t, c)
	if len(results) != 1 {
		t.Fatalf("got %d rows, want 1 (outer ID=5 matches inner ID=5)", len(results))
	}
	ojAssertSlots(t, results[0].Positional, int64(5), int64(50), int64(5), int64(50))
	// Qualified leg references (baked QOV(alias).col) resolve through the
	// merged row's leg windows.
	if v, ok := ojWiringLegRead(t, results[0].Positional, "A", "ID"); !ok || v != int64(5) {
		t.Fatalf("A.ID = %v, want 5 (leg window)", v)
	}
	if v, ok := ojWiringLegRead(t, results[0].Positional, "B", "W"); !ok || v != int64(50) {
		t.Fatalf("B.W = %v, want 50 (leg window)", v)
	}
}

// TestNLJCursor_OrdinalBuild_LeftOuterNullLeg pins the
// unmatched-outer LEFT emission: the positional row carries the outer leg
// verbatim and the INNER slots NULL (the nil-pointer NULL leg).
func TestNLJCursor_OrdinalBuild_LeftOuterNullLeg(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringOuterJoinSources(t, plans.JoinLeftOuter)

	outerRows := []QueryResult{
		ojLegQR(t, legA, int64(1), int64(10)),
		ojLegQR(t, legA, int64(2), int64(20)), // unmatched
	}
	innerRows := []QueryResult{ojLegQR(t, legB, int64(1), int64(100))}
	pred := ojEqPred(
		ojWiringMustField(t, qovA, 0),
		ojWiringMustField(t, qovB, 0),
	)
	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinLeftOuter,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
	defer c.Close()
	results := ojCollectCursor(t, c)
	if len(results) != 2 {
		t.Fatalf("got %d rows, want 2 (matched + null-padded)", len(results))
	}
	ojAssertSlots(t, results[0].Positional, int64(1), int64(10), int64(1), int64(100))
	// The null-padded row: inner slots NULL.
	ojAssertSlots(t, results[1].Positional, int64(2), int64(20), nil, nil)
}

// TestNLJCursor_OrdinalBuild_FullDrain pins the FULL OUTER drain
// emission: an inner row that matched no outer row builds with the OUTER
// slots NULL (the symmetric nil-pointer NULL leg).
func TestNLJCursor_OrdinalBuild_FullDrain(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringOuterJoinSources(t, plans.JoinFullOuter)

	outerRows := []QueryResult{ojLegQR(t, legA, int64(1), int64(10))}
	innerRows := []QueryResult{
		ojLegQR(t, legB, int64(1), int64(100)),
		ojLegQR(t, legB, int64(3), int64(300)), // matches no outer row
	}
	pred := ojEqPred(
		ojWiringMustField(t, qovA, 0),
		ojWiringMustField(t, qovB, 0),
	)
	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinFullOuter,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
	defer c.Close()
	results := ojCollectCursor(t, c)
	if len(results) != 2 {
		t.Fatalf("got %d rows, want 2 (matched + drained unmatched-inner)", len(results))
	}
	ojAssertSlots(t, results[0].Positional, int64(1), int64(10), int64(1), int64(100))
	// The drain row: outer slots NULL.
	ojAssertSlots(t, results[1].Positional, nil, nil, int64(3), int64(300))
}

func TestNLJCursor_OrdinalBuild_FullDrainHashCandidates(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringOuterJoinSources(t, plans.JoinFullOuter)

	const matchKey = int64(1)
	matchPositions := map[int]bool{3: true, 57: true, 99: true}
	innerRows := make([]QueryResult, 120)
	for i := range innerRows {
		key := int64(1_000 + i)
		if matchPositions[i] {
			key = matchKey
		}
		innerRows[i] = ojLegQR(t, legB, key, int64(i))
	}
	outerRows := []QueryResult{
		ojLegQR(t, legA, matchKey, int64(10)),
		ojLegQR(t, legA, int64(-1), int64(20)), // hash miss, emitted null-padded
	}
	pred := ojEqPred(
		ojWiringMustField(t, qovA, 0),
		ojWiringMustField(t, qovB, 0),
	)
	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinFullOuter,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
	defer c.Close()
	if c.hashIndex == nil {
		t.Fatal("fixture must build the hash index")
	}

	results := ojCollectCursor(t, c)
	const wantRows = 3 + 1 + 117
	if len(results) != wantRows {
		t.Fatalf("got %d rows, want %d (three matches + unmatched outer + unmatched-inner drain)",
			len(results), wantRows)
	}
	for resultIndex, innerIndex := range []int{3, 57, 99} {
		ojAssertSlots(t, results[resultIndex].Positional,
			matchKey, int64(10), matchKey, int64(innerIndex))
	}
	ojAssertSlots(t, results[3].Positional, int64(-1), int64(20), nil, nil)

	drainIndex := 4
	for innerIndex, innerRow := range innerRows {
		if matchPositions[innerIndex] {
			continue
		}
		key, _ := innerRow.Positional.Get(0)
		ojAssertSlots(t, results[drainIndex].Positional,
			nil, nil, key, int64(innerIndex))
		drainIndex++
	}
	if drainIndex != len(results) {
		t.Fatalf("validated %d rows, result has %d", drainIndex, len(results))
	}
}

// TestNLJCursor_BothSeedShapesEmitPositional pins that BOTH join-seed
// shapes — the ordinal-build RC and a plain ordinary (unpinned) join RC — emit a
// leg-windowed Positional row: mergeRows is Positional-native, so even the
// plain-RC merge builds a positional row via concatLegPositionals.
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
		ojWiringMustField(t, qovA, 0),
		ojWiringMustField(t, qovB, 0),
	)
	// An ordinary semantic RC is the exact non-ordinal counterpart.
	ordinary := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: ojWiringMustField(t, qovA, 0)},
		values.RecordConstructorField{Name: "V", Value: ojWiringMustField(t, qovA, 1)},
		values.RecordConstructorField{Name: "ID_2", Value: ojWiringMustField(t, qovB, 0)},
		values.RecordConstructorField{Name: "W", Value: ojWiringMustField(t, qovB, 1)},
	)

	collect := func(rv values.Value) []QueryResult {
		c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
			values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), []predicates.QueryPredicate{pred}, rv, EmptyEvaluationContext(), nil)
		defer c.Close()
		return ojCollectCursor(t, c)
	}
	ordinal := collect(seed)
	lazyResults := collect(ordinary)
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

// TestFlatMap_ComputeResult_OrdinalBuild pins the ordinal-build flatMap: the
// positional row builds from the RC with leg bindings (both legs present, and
// the nil inner as the NULL leg).
func TestFlatMap_ComputeResult_OrdinalBuild(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t)
	newCursor := func(t *testing.T) *flatMapCursor {
		t.Helper()
		c, err := newFlatMapCursorWithOuterProperties(nil, nil, nil, nil, EmptyEvaluationContext(),
			qovA.Correlation(), qovB.Correlation(), seed, recordlayer.ExecuteProperties{}, false)
		if err != nil {
			t.Fatalf("newFlatMapCursorWithOuterProperties: %v", err)
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

// TestSortKeyExactCarrierDispatch pins the only escape from joined-row leg
// windows: a key whose every QOV leaf is the pointer-exact selected carrier is
// evaluated against that carrier. Same-shaped current roots and ordinary named
// owners remain distinct, and a mixed tree cannot borrow the carrier path.
func TestSortKeyExactCarrierDispatch(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, _, _ := ojWiringLegs(t)
	mergedType := values.NewRecordType("", false, []values.Field{
		{Name: "A_ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "B_ID", FieldType: values.NotNullLong, Ordinal: 2},
		{Name: "W", FieldType: values.NotNullLong, Ordinal: 3},
	})
	carrier := mustExecutorCurrentLayout(t, mergedType, nil).Carrier()
	exact := ojWiringMustField(t, carrier, 1)
	originalExplain := values.ExplainValue(exact)

	if !valueReadsOnlyExactCarrier(exact, carrier) {
		t.Fatal("pointer-exact selected carrier key declined")
	}
	if got := values.ExplainValue(exact); got != originalExplain {
		t.Fatalf("carrier admission mutated the key: before=%q after=%q", originalExplain, got)
	}

	sameShapeCurrent := mustExecutorCurrentLayout(t, mergedType, nil).Carrier()
	if valueReadsOnlyExactCarrier(ojWiringMustField(t, sameShapeCurrent, 1), carrier) {
		t.Fatal("independently minted same-shaped current carrier admitted")
	}
	foreignNamed := ojWiringMustQOV(t, values.NamedCorrelationIdentifier("JOINED"), mergedType)
	if valueReadsOnlyExactCarrier(ojWiringMustField(t, foreignNamed, 1), carrier) {
		t.Fatal("foreign named carrier admitted")
	}
	wrongType := values.NewRecordType("", false, []values.Field{
		{Name: "A_ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NullableString, Ordinal: 1},
		{Name: "B_ID", FieldType: values.NotNullLong, Ordinal: 2},
		{Name: "W", FieldType: values.NotNullLong, Ordinal: 3},
	})
	wrongTypeCurrent := mustExecutorCurrentLayout(t, wrongType, nil).Carrier()
	if valueReadsOnlyExactCarrier(ojWiringMustField(t, wrongTypeCurrent, 1), carrier) {
		t.Fatal("same-correlation wrong-type carrier admitted")
	}

	// A foreign nested path is still foreign: matching a leaf name or path does
	// not authorize it to borrow the selected carrier.
	nestedType := values.NewRecordType("", false, []values.Field{
		{Name: "A", FieldType: legA, Ordinal: 0},
		{Name: "B", FieldType: legB, Ordinal: 1},
	})
	nestedCurrent := mustExecutorCurrentLayout(t, nestedType, nil).Carrier()
	nestedPath := ojWiringMustField(t, nestedCurrent, 0, 1)
	if valueReadsOnlyExactCarrier(nestedPath, carrier) {
		t.Fatal("foreign nested path admitted by leaf/path coincidence")
	}

	// Mixing one exact-carrier read with any other declared owner must retain
	// leg-window dispatch; all QOV leaves, not merely the first, are checked.
	mixed := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  exact,
		Right: ojWiringMustField(t, qovA, 1),
	}
	if valueReadsOnlyExactCarrier(mixed, carrier) {
		t.Fatal("mixed selected-carrier/leg key admitted")
	}
}

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
	nlj, err := plans.NewRecordQueryNestedLoopJoinPlan(nil, nil, nil, plans.JoinInner, values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), seed)
	nlj = ojWiringMustConstruct(t, nlj, err)

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
		flatMap, err := plans.NewRecordQueryFlatMapPlan(nil, nil, qovA.Correlation(), qovB.Correlation(), seed, false)
		assertSpans(t, "flatmap", ojWiringMustConstruct(t, flatMap, err))
	})
	t.Run("passthrough wrappers", func(t *testing.T) {
		t.Parallel()
		inMemorySort, err := plans.NewRecordQueryInMemorySortPlan(nlj, nil)
		inMemorySort = ojWiringMustConstruct(t, inMemorySort, err)
		limit, err := plans.NewRecordQueryLimitPlan(nlj, 10, 0)
		limit = ojWiringMustConstruct(t, limit, err)
		distinct, err := plans.NewRecordQueryDistinctPlan(nlj)
		distinct = ojWiringMustConstruct(t, distinct, err)
		pkDistinct, err := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(nlj)
		pkDistinct = ojWiringMustConstruct(t, pkDistinct, err)
		typeFilter, err := plans.NewRecordQueryTypeFilterPlan(nil, nlj)
		typeFilter = ojWiringMustConstruct(t, typeFilter, err)
		filter, err := plans.NewRecordQueryFilterPlan(nil, nlj)
		filter = ojWiringMustConstruct(t, filter, err)
		predicatesFilter, err := plans.NewRecordQueryPredicatesFilterPlan(nlj, nil)
		predicatesFilter = ojWiringMustConstruct(t, predicatesFilter, err)
		inJoin, err := plans.NewRecordQueryInJoinPlan(nlj, "iv", false, false)
		inJoin = ojWiringMustConstruct(t, inJoin, err)
		inUnion, err := plans.NewRecordQueryInUnionPlan(nlj, []string{"iv"}, nil, false)
		inUnion = ojWiringMustConstruct(t, inUnion, err)
		wrappers := map[string]plans.RecordQueryPlan{
			"in-memory sort":    inMemorySort,
			"limit":             limit,
			"distinct":          distinct,
			"PK distinct":       pkDistinct,
			"type filter":       typeFilter,
			"filter":            filter,
			"predicates filter": predicatesFilter,
			// Both lowerings of `... IN (…)` re-emit the inner join's merged rows
			// verbatim under a per-in-value binding (row count/order change, row
			// LAYOUT does not), so the join below is still the leg-window
			// authority — a source-relative sort key over the merged row resolves
			// through the windows instead of going loud.
			"in-join":  inJoin,
			"in-union": inUnion,
		}
		for name, w := range wrappers {
			assertSpans(t, name, w)
		}
		// Nested passthroughs unwrap transitively — incl. the real failing shape:
		// an in-memory sort ABOVE an in-join/in-union over the join.
		nestedDistinct, err := plans.NewRecordQueryDistinctPlan(inMemorySort)
		nestedDistinct = ojWiringMustConstruct(t, nestedDistinct, err)
		nestedLimit, err := plans.NewRecordQueryLimitPlan(nestedDistinct, 5, 0)
		assertSpans(t, "limit(distinct(in-memory sort))", ojWiringMustConstruct(t, nestedLimit, err))
		inJoinSort, err := plans.NewRecordQueryInMemorySortPlan(inJoin, nil)
		assertSpans(t, "in-memory sort(in-join)", ojWiringMustConstruct(t, inJoinSort, err))
		inUnionSort, err := plans.NewRecordQueryInMemorySortPlan(inUnion, nil)
		assertSpans(t, "in-memory sort(in-union)", ojWiringMustConstruct(t, inUnionSort, err))
	})
	t.Run("non-join input declines", func(t *testing.T) {
		t.Parallel()
		mapPlan, err := plans.NewRecordQueryMapPlan(nlj, seed)
		if _, ok := downstreamLegWindows(ojWiringMustConstruct(t, mapPlan, err)); ok {
			t.Fatal("a Map input must decline — it rewrites rows (and is itself a dispatch site)")
		}
		if _, ok := downstreamLegWindows(nil); ok {
			t.Fatal("a nil input must decline")
		}
	})
	t.Run("folded-RV join declines", func(t *testing.T) {
		t.Parallel()
		bakedAV := ojWiringMustSeedField(t, qovA, 1)
		folded := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "V", Value: bakedAV},
		)
		foldedNLJ, err := plans.NewRecordQueryNestedLoopJoinPlan(nil, nil, nil, plans.JoinInner, values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), folded)
		foldedNLJ = ojWiringMustConstruct(t, foldedNLJ, err)
		if _, ok := downstreamLegWindows(foldedNLJ); ok {
			t.Fatal("a folded-RV join must decline — its output is a plain projection row, no leg windows")
		}
	})
	t.Run("excluded wrapper declines", func(t *testing.T) {
		t.Parallel()
		firstOrDefault, err := plans.NewRecordQueryFirstOrDefaultPlan(nlj, nil)
		if _, ok := downstreamLegWindows(ojWiringMustConstruct(t, firstOrDefault, err)); ok {
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
	merged := NewPositionalRow(mergedType) // [A.ID=1, A.V=10, B.ID=2, B.W=20]
	for i, value := range []any{int64(1), int64(10), int64(2), int64(20)} {
		if !merged.Set(i, value) {
			t.Fatalf("merged fixture slot %d is out of range", i)
		}
	}

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
	bwRef := ojWiringMustField(t, qovB, 1)
	got, err := bwRef.Evaluate(rowCtx)
	if err != nil {
		t.Fatalf("B.W through legWindowRowContext: %v", err)
	}
	if got != int64(20) {
		t.Fatalf("lazy B.W = %v, want 20 (B's W through the leg window, not A's V at absolute slot 1)", got)
	}
	// A baked second-leg reference reads the same correct slot.
	bakedBID := ojWiringMustField(t, qovB, 0)
	if got, err := bakedBID.Evaluate(rowCtx); err != nil || got != int64(2) {
		t.Fatalf("baked B#0 = (%v, %v), want (2, nil)", got, err)
	}
	// An OUTER correlation delegates through the base EvaluationContext.
	outerQOV := ojWiringMustQOV(t, outerID,
		values.NewRecordType("", false, []values.Field{{Name: "X", FieldType: values.NotNullLong, Ordinal: 0}}))
	outerRef := ojWiringMustField(t, outerQOV, 0)
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
	bakedAV := ojWiringMustSeedField(t, qovA, 1)
	foldedRV := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "V", Value: bakedAV},
		values.RecordConstructorField{Name: "_1", Value: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}},
	)

	// Baked cross-leg ON predicate: A.ID = B.ID — references the DROPPED leg.
	bakedAID := ojWiringMustField(t, qovA, 0)
	bakedBID := ojWiringMustField(t, qovB, 0)
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
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), []predicates.QueryPredicate{pred}, foldedRV, EmptyEvaluationContext(), nil)
	defer c.Close()
	results := ojCollectCursor(t, c)
	if len(results) != 1 {
		t.Fatalf("got %d rows, want 1 (only A.ID=1 matches B.ID=1) — a dropped-leg zero-width binding misfilters or errors", len(results))
	}
	// The folded output row: [A.V, 1].
	ojAssertSlots(t, results[0].Positional, int64(10), int64(1))
}

// TestFlatMap_FoldedRVDroppedLeg_PlanTypes pins the FlatMap side of the same
// correctness trap: the correlated FlatMap implementation pushes the gated
// join's baked ON references INTO the inner plan, so a folded result value
// that DROPS the outer leg leaves the build typeless for it even though the
// inner plan still references it — a leg row built by name (aggregate-box
// shape) then bound zero-width and the baked SARG died loudly on a
// legitimate plan. newFlatMapCursorWithOuterProperties must widen LegTypes from the inner
// plan's predicate surfaces.
func TestFlatMap_FoldedRVDroppedLeg_PlanTypes(t *testing.T) {
	t.Parallel()
	legA, _, qovA, qovB, _ := ojWiringLegs(t)
	outerCorr := values.NamedCorrelationIdentifier("A")

	// Folded RV: only leg B appears — the OUTER leg A dropped.
	bakedBW := ojWiringMustSeedField(t, qovB, 1)
	foldedRV := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "W", Value: bakedBW},
	)

	// The inner plan carries the baked ON reference to the dropped OUTER leg
	// (a residual PredicatesFilter — the correlated-implementation shape).
	bakedAID := ojWiringMustField(t, qovA, 0)
	bakedBID := ojWiringMustField(t, qovB, 0)
	innerScan, err := plans.NewRecordQueryScanPlan(nil, qovB.FlowedType(), false)
	innerScan = ojWiringMustConstruct(t, innerScan, err)
	innerPlan, err := plans.NewRecordQueryPredicatesFilterPlan(
		innerScan,
		[]predicates.QueryPredicate{ojEqPred(bakedBID, bakedAID)},
	)
	innerPlan = ojWiringMustConstruct(t, innerPlan, err)

	c, err := newFlatMapCursorWithOuterProperties(nil, nil, innerPlan, nil, EmptyEvaluationContext(),
		outerCorr, values.NamedCorrelationIdentifier("B"), foldedRV,
		recordlayer.ExecuteProperties{}, false)
	if err != nil {
		t.Fatalf("newFlatMapCursorWithOuterProperties: %v", err)
	}
	// The build must know the dropped OUTER leg's type from the inner plan's
	// baked reference…
	outerType := c.build.legType(outerCorr)
	if outerType == nil {
		t.Fatal("LegTypes missing the RV-dropped OUTER leg — the inner plan's baked SARG/residual references it")
	}
	if len(outerType.Fields) != len(legA.Fields) {
		t.Fatalf("outer leg type has %d fields, want %d", len(outerType.Fields), len(legA.Fields))
	}
	// …so a leg row built by name (aggregate-box shape) adapts to a
	// full-width binding the baked reference can read — not the zero-width
	// death row.
	adapted, aerr := adaptLegPositional(ojNameQR(legA, int64(7), int64(70)), outerType, values.CorrelationIdentifier{})
	if aerr != nil {
		t.Fatalf("adaptLegPositional: %v", aerr)
	}
	if v, ok := adapted.Get(0); !ok || v != int64(7) {
		t.Fatalf("adapted outer slot 0 = (%v, %v), want (7, true) — zero-width binding means the widening failed", v, ok)
	}
}

// TestLegWindowBinder_BoxAliasReadsLeaf pins the RFC-232 replacement for box
// span precedence. The exact OrdinalLayout gives A, buried B, and leaf E
// independent source windows. E.ID therefore names the rightmost carrier slot,
// never B's earlier same-named slot. Omitting E's window is loud; runtime
// evaluation cannot fall back to ambient carrier positions.
func TestLegWindowBinder_BoxAliasReadsLeaf(t *testing.T) {
	t.Parallel()
	corrA := values.NamedCorrelationIdentifier("A")
	corrB := values.NamedCorrelationIdentifier("B")
	corrE := values.NamedCorrelationIdentifier("E")
	aType := exactTestRowType(values.Field{Name: "AID", FieldType: values.NotNullLong})
	bType := exactTestRowType(
		values.Field{Name: "BID", FieldType: values.NotNullLong},
		values.Field{Name: "ID", FieldType: values.NotNullLong},
	)
	eType := exactTestRowType(values.Field{Name: "ID", FieldType: values.NotNullLong})
	carrierType := exactTestRowType(
		values.Field{Name: "AID", FieldType: values.NotNullLong},
		values.Field{Name: "BID", FieldType: values.NotNullLong},
		values.Field{Name: "ID", FieldType: values.NotNullLong}, // buried B's duplicate
		values.Field{Name: "ID", FieldType: values.NotNullLong}, // leaf E's value
	)
	qovA := ojWiringMustQOV(t, corrA, aType)
	qovB := ojWiringMustQOV(t, corrB, bType)
	qovE := ojWiringMustQOV(t, corrE, eType)
	tiles := []values.OrdinalTileSpec{{Start: 0, Width: 4, Kind: values.OrdinalTileFlat}}
	windows := []values.OrdinalWindowSpec{
		{Source: qovA, FieldPaths: [][]int{{0}}},
		{Source: qovB, FieldPaths: [][]int{{1}, {2}}},
		{Source: qovE, FieldPaths: [][]int{{3}}},
	}
	newLayout := func(specs []values.OrdinalWindowSpec) values.OrdinalLayout {
		t.Helper()
		layout, err := values.NewOrdinalLayoutForCarrierType(carrierType, tiles, specs)
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
		copy(row.Slots, []any{int64(10), int64(20), int64(21), int64(30)})
		return row
	}

	full := newLayout(windows)
	if len(carrierType.Legs) != 0 || len(aType.Legs) != 0 || len(bType.Legs) != 0 || len(eType.Legs) != 0 {
		t.Fatal("fixture restored legacy RecordType.Legs authority")
	}
	ctx, err := ordinalLayoutRowContext(full, rowFor(full), nil, nil)
	if err != nil {
		t.Fatalf("ordinalLayoutRowContext: %v", err)
	}
	assertRead := func(label string, value values.Value, want int64) {
		t.Helper()
		got, evalErr := value.Evaluate(ctx)
		if evalErr != nil || got != want {
			t.Fatalf("%s = (%v, %v), want (%d, nil)", label, got, evalErr, want)
		}
	}
	assertRead("A.AID", ojWiringMustField(t, qovA, 0), 10)
	assertRead("B.BID", ojWiringMustField(t, qovB, 0), 20)
	assertRead("B.ID", ojWiringMustField(t, qovB, 1), 21)
	assertRead("E.ID", ojWiringMustField(t, qovE, 0), 30)

	// Mutation control: the same-shaped rightmost slot does not bind E without
	// E's exact window.
	withoutE := newLayout(windows[:2])
	withoutECtx, err := ordinalLayoutRowContext(withoutE, rowFor(withoutE), nil, nil)
	if err != nil {
		t.Fatalf("context without E: %v", err)
	}
	got, evalErr := ojWiringMustField(t, qovE, 0).Evaluate(withoutECtx)
	var coded interface {
		Code() values.ResolutionErrorCode
	}
	if got != nil || !errors.As(evalErr, &coded) || coded.Code() != values.UnboundCorrelation {
		t.Fatalf("E.ID without E window = (%v, %v), want UnboundCorrelation", got, evalErr)
	}
}

// TestOrdinalJoinBuild_BareArmNullLeg falsifies the Bare arm's `case nil`.
//
// That arm was reachable only through the join's own machinery, so nothing
// distinguished "NULL flows as a nil row" from "this branch is dead". A nil arm
// no test constructs is a claim, and the claim here is load-bearing: it is the
// NULL-leg extension for a LEFT/FULL join whose result value is a single baked
// reference rather than a record constructor. If it returned a 1-slot row holding
// nil instead of a nil row, a null-extended pair would emit a ROW where the join
// must emit nothing-of-that-leg, and the two paths would disagree about what NULL
// is.
//
// So both paths are asserted TOGETHER: the Bare arm's nil row and the RC arm's
// NULL-PADDED row over the same NULL binding. They are deliberately different
// shapes — the RC arm knows its output width and pads it, the Bare arm has one
// value and no width to pad — and pinning them side by side is what keeps that
// difference intentional.
func TestOrdinalJoinBuild_BareArmNullLeg(t *testing.T) {
	t.Parallel()
	legA, _, qovA, qovB, _ := ojWiringLegs(t)

	// The Bare shape: the join's result value is the WHOLE leg-A row, baked.
	bare := ojWiringMustSeedField(t, qovA, 0)
	build, err := newOrdinalJoinBuild(bare, nil)
	if err != nil {
		t.Fatalf("bare build: %v", err)
	}
	if build.Bare == nil {
		t.Fatalf("fixture: build took the RC arm (RC=%v), so this test would not "+
			"reach the Bare nil case at all", build.RC)
	}

	// A is the NULL leg: present in the binder with a nil row, which is the
	// sanctioned nil-binding the LEFT/FULL padding produces.
	nullA := &twoLegBinder{
		outerID: qovA.Correlation(), innerID: qovB.Correlation(),
		outer: nil, inner: NewPositionalRow(legA),
	}
	row, err := build.evaluateBound(nullA)
	if err != nil {
		t.Fatalf("bare arm over a NULL leg: %v", err)
	}
	if row != nil {
		t.Errorf("bare arm over a NULL leg returned row %v, want a NIL row.\n"+
			"  Wrapping NULL into a 1-slot row would make a null-extended pair emit a\n"+
			"  present row whose single slot is nil — a different answer from the RC\n"+
			"  path's, for the same binding.", row)
	}

	// The RC arm over the SAME NULL binding pads its known width instead. Both
	// behaviours are correct and they are not the same behaviour.
	rcBuild, err := newOrdinalJoinBuild(ojWiringBuildSeed(t, qovA, qovB), nil)
	if err != nil {
		t.Fatalf("rc build: %v", err)
	}
	rcRow, err := rcBuild.evaluateBound(nullA)
	if err != nil {
		t.Fatalf("rc arm over a NULL leg: %v", err)
	}
	if rcRow == nil {
		t.Fatal("rc arm over a NULL leg returned a nil row — the RC path knows its " +
			"output width and must pad it with NULLs, not vanish")
	}
	if len(rcRow.Slots) != len(rcBuild.OutputType.Fields) {
		t.Fatalf("rc arm padded row has %d slots, want %d (its full output width)",
			len(rcRow.Slots), len(rcBuild.OutputType.Fields))
	}
	// Leg A's slots are the NULL-padded ones; leg B's still carry its row.
	for i := 0; i < len(legA.Fields); i++ {
		if rcRow.Slots[i] != nil {
			t.Errorf("rc arm slot %d = %v over a NULL leg A, want nil", i, rcRow.Slots[i])
		}
	}
}

// TestOrdinalJoinBuild_WidthAgreementOnBothArms pins the leg width-agreement
// assert on the BARE arm as well as the RC arm.
//
// Every reference to one leg is supposed to be a copy of the single
// planner-constructed typed QOV, so two references disagreeing on that leg's
// width is a malformed plan. The assert used to live only inside the RC arm's
// bare-QOV loop, which meant the Bare arm — whose ONLY leg-type source is
// legTypesFromResultValue — asserted nothing, while
// widenLegTypesFromPredicates' doc pointed at "the RV-side collection the caller
// already ran" as the place the invariant is checked. It also meant that even on
// the RC path two BAKED references to one leg at different widths passed, because
// that walk overwrote last-wins.
//
// The assert now lives in the walk, so both arms are covered by one check. A
// silent last-wins here adapts a leg to the WRONG width: too narrow and a
// predicate's ordinal blows up loudly, too wide and it reads a neighbouring
// leg's slot, which does not.
func TestOrdinalJoinBuild_WidthAgreementOnBothArms(t *testing.T) {
	t.Parallel()
	legA := ojWiringLegTypeAV()
	// Two QOVs for the SAME leg at DIFFERENT widths — the malformed-plan shape.
	wide := ojWiringMustQOV(t, values.NamedCorrelationIdentifier("A"), legA)
	narrow := ojWiringMustQOV(t, values.NamedCorrelationIdentifier("A"),
		&values.RecordType{Fields: []values.Field{
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		}})
	if len(legA.Fields) == 1 {
		t.Fatal("fixture: the two leg types must differ in width or the assert cannot fire")
	}
	bakedWide := ojWiringMustSeedField(t, wide, 0)
	bakedNarrow := ojWiringMustSeedField(t, narrow, 0)

	// BARE arm: one value carrying both references. An arithmetic value is used
	// because the bare arm's shape is "not an RC" — the plan-level gate narrows
	// which non-RC shapes the planner may emit, but the executor's assert must not
	// depend on that narrowing holding.
	t.Run("bare arm", func(t *testing.T) {
		t.Parallel()
		bare := &values.ArithmeticValue{Op: values.OpAdd, Left: bakedWide, Right: bakedNarrow}
		if _, isRC := values.Value(bare).(*values.RecordConstructorValue); isRC {
			t.Fatal("fixture: the bare-arm value must not be an RC")
		}
		build, err := newOrdinalJoinBuild(bare, nil)
		if err == nil {
			t.Fatalf("bare arm accepted DIVERGENT widths for leg A (build=%+v).\n"+
				"  Its only leg-type source is legTypesFromResultValue; with no assert there\n"+
				"  the wider or narrower type wins by walk order and the leg adapts to a\n"+
				"  width the references disagree about.", build)
		}
		if !strings.Contains(err.Error(), "DIVERGENT") {
			t.Errorf("bare arm error = %q, want one naming the divergent widths", err)
		}
	})

	// RC arm with two BAKED references (not the bare-QOV pairing the old assert
	// covered): this is the case the RC path also used to let through.
	t.Run("rc arm two baked refs", func(t *testing.T) {
		t.Parallel()
		rc := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "X", Value: bakedWide},
			values.RecordConstructorField{Name: "Y", Value: bakedNarrow},
		)
		build, err := newOrdinalJoinBuild(rc, nil)
		if err == nil {
			t.Fatalf("RC arm accepted DIVERGENT widths for leg A across two BAKED "+
				"references (build=%+v) — the old assert only compared a bare QOV against "+
				"the baked collection, so this pairing overwrote last-wins", build)
		}
	})

	// The AGREEING case must still build: an assert that rejects the legitimate
	// shape is worse than none.
	t.Run("agreeing widths build", func(t *testing.T) {
		t.Parallel()
		alsoWideQOV := ojWiringMustQOV(t, values.NamedCorrelationIdentifier("A"), legA)
		alsoWide := ojWiringMustSeedField(t, alsoWideQOV, 1)
		rc := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "X", Value: bakedWide},
			values.RecordConstructorField{Name: "Y", Value: alsoWide},
		)
		build, err := newOrdinalJoinBuild(rc, nil)
		if err != nil {
			t.Fatalf("two references at the SAME width must build: %v", err)
		}
		if got := build.LegTypes[values.NamedCorrelationIdentifier("A")]; got == nil ||
			len(got.Fields) != len(legA.Fields) {
			t.Fatalf("leg A type = %v, want the %d-field type", got, len(legA.Fields))
		}
	})
}

func TestOrdinalJoinBuild_WidenPlanIgnoresLocalCurrentPhasesButRejectsNamedLegDrift(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, _ := ojWiringLegs(t)
	build, err := newOrdinalJoinBuild(ojWiringBuildSeed(t, qovA, qovB), nil)
	if err != nil {
		t.Fatalf("newOrdinalJoinBuild: %v", err)
	}

	currentTwoLayout := mustExecutorConstruct(values.NewOrdinalLayoutForCarrierType(
		values.NewRecordType("CURRENT_TWO", false, []values.Field{
			{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
			{Name: "V", Ordinal: 1, FieldType: values.NotNullLong},
		}), []values.OrdinalTileSpec{{Start: 0, Width: 2, Kind: values.OrdinalTileFlat}}, nil))
	currentTwo := currentTwoLayout.Carrier()
	currentFourLayout := mustExecutorConstruct(values.NewOrdinalLayoutForCarrierType(
		values.NewRecordType("CURRENT_FOUR", false, []values.Field{
			{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
			{Name: "V", Ordinal: 1, FieldType: values.NotNullLong},
			{Name: "X", Ordinal: 2, FieldType: values.NotNullLong},
			{Name: "Y", Ordinal: 3, FieldType: values.NotNullLong},
		}), []values.OrdinalTileSpec{{Start: 0, Width: 4, Kind: values.OrdinalTileFlat}}, nil))
	currentFour := currentFourLayout.Carrier()
	currentTwoID := mustExecutorConstruct(values.ResolveFieldOrdinals(currentTwo, []int{0}))
	currentFourID := mustExecutorConstruct(values.ResolveFieldOrdinals(currentFour, []int{0}))
	currentTwoScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan(
		nil, currentTwo.FlowedType(), false))
	currentFourScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan(
		nil, currentFour.FlowedType(), false))
	currentTwoFilter := mustExecutorConstruct(plans.NewRecordQueryPredicatesFilterPlan(
		currentTwoScan, []predicates.QueryPredicate{
			ojEqPred(currentTwoID, &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}),
		}))
	currentFourFilter := mustExecutorConstruct(plans.NewRecordQueryPredicatesFilterPlan(
		currentFourScan, []predicates.QueryPredicate{
			ojEqPred(currentFourID, &values.ConstantValue{Value: int64(2), Typ: values.NotNullLong}),
		}))
	localPhases := &bakedReferenceTreePlan{children: []plans.RecordQueryPlan{
		currentTwoFilter, currentFourFilter,
	}}
	if err := build.widenLegTypesFromPlan(localPhases); err != nil {
		t.Fatalf("unrelated local current phases were treated as one join leg: %v", err)
	}
	if _, present := build.LegTypes[values.CurrentCorrelation()]; present {
		t.Fatal("local current phase leaked into named FlatMap leg types")
	}

	wideA := ojWiringMustQOV(t, qovA.Correlation(), values.NewRecordType("A_WIDE", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "V", Ordinal: 1, FieldType: values.NotNullLong},
		{Name: "X", Ordinal: 2, FieldType: values.NotNullLong},
	}))
	wideAID := ojWiringMustSeedField(t, wideA, 0)
	namedDrift := mustExecutorConstruct(plans.NewRecordQueryPredicatesFilterPlan(
		mustExecutorConstruct(plans.NewRecordQueryScanPlan(nil, legB, false)),
		[]predicates.QueryPredicate{
			ojEqPred(wideAID, &values.ConstantValue{Value: int64(3), Typ: values.NotNullLong}),
		},
	))
	if err := build.widenLegTypesFromPlan(namedDrift); err == nil ||
		!strings.Contains(err.Error(), "DIVERGENT baked types") {
		t.Fatalf("named leg width drift error = %v, want divergence", err)
	}
	if got := build.LegTypes[qovA.Correlation()]; got == nil || len(got.Fields) != len(legA.Fields) {
		t.Fatalf("named leg A type was overwritten after rejected drift: %v", got)
	}
}

// bakedReferenceTreePlan is a test-only parent that lets the executor's
// physical-plan census visit two independently admitted predicate-filter
// phases. It adds no Value surface of its own.
type bakedReferenceTreePlan struct {
	plans.PlanExprBase
	children []plans.RecordQueryPlan
}

func (*bakedReferenceTreePlan) GetResultType() values.Type                           { return values.UnknownType }
func (p *bakedReferenceTreePlan) GetChildren() []plans.RecordQueryPlan               { return p.children }
func (*bakedReferenceTreePlan) EqualsPlanWithoutChildren(plans.RecordQueryPlan) bool { return false }
func (*bakedReferenceTreePlan) HashCodeWithoutChildren() uint64                      { return 0 }
func (*bakedReferenceTreePlan) Explain() string                                      { return "baked-reference-tree" }
func (p *bakedReferenceTreePlan) EqualsWithoutChildren(
	other expressions.RelationalExpression,
	_ *expressions.AliasMap,
) bool {
	o, ok := other.(*bakedReferenceTreePlan)
	return ok && p == o
}

func (p *bakedReferenceTreePlan) WithQuantifiers(
	_ []expressions.Quantifier,
) (expressions.RelationalExpression, error) {
	return p, nil
}

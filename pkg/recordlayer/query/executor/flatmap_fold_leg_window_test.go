package executor

// The white-box gate pins in the cascades package are all PLANNER-side: they
// prove the seed is built and gated. None of them reach the cursor's
// legWindowRowContext branch (computeResultLegs) — the code that actually
// resolves a heterogeneous fold projection against the merged row. A
// functional FDB test can't isolate that branch either: a merged row built
// with a name-keyed Datum would still resolve the fold's dotted reads
// correctly through the Datum, masking a broken leg-window branch. This test
// makes the positional path load-bearing by leaving the outer row's Datum
// EMPTY, so the fold's dotted AND QOV leg refs can only resolve through the
// positional row's leg windows. Correct output proves the branch fired; a
// regression back to a Datum-based read would yield NULLs and fail.

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestCursorResolvesFoldPositionally(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := ojWiringLegs(t) // A(ID,V), B(ID,W); seed = [A.ID,A.V,B.ID,B.W]

	// The step-1 NLJ carries the ordinal seed as its result value — this is what
	// downstreamLegWindows reads to derive the leg windows (A@0, B@2).
	scanA := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"A"}, legA, false))
	scanB := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"B"}, legB, false))
	step1 := mustExecutorConstruct(plans.NewRecordQueryNestedLoopJoinPlan(scanA, scanB, nil, plans.JoinInner, values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), seed))

	// Cross-agreement strengthening: the EXECUTOR twin (ordinalJoinSpansOf, via
	// downstreamLegWindows→joinPlanSpansTyped) yields A@0 / B@2 for this seed — the
	// same offsets the planner twin (OrdinalSeedLegWindows) gives, closing the
	// cross-agreement loop on this constructor.
	spans, ok := downstreamLegWindows(step1)
	if !ok || len(spans) != 2 {
		t.Fatalf("downstreamLegWindows over the seed NLJ = (%v, %v), want 2 leg spans", spans, ok)
	}
	off := map[string]int{}
	for _, s := range spans {
		off[s.Alias.Name()] = s.Offset
	}
	if off["A"] != 0 || off["B"] != 2 {
		t.Fatalf("executor twin offsets A@%d B@%d, want A@0 B@2", off["A"], off["B"])
	}

	// The folded projection: a HETEROGENEOUS mix over the SAME merged row — a
	// exact leg read (A.V), another exact read into the OTHER leg (B.W), and a
	// third QOV-based leg ref (QOV(A).ID). None can resolve via the Datum below.
	mergedCorr := values.NamedCorrelationIdentifier("M")
	foldRV := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "AV", Value: mustTestFieldOrdinal(t, qovA, 1)},
		values.RecordConstructorField{Name: "BW", Value: mustTestFieldOrdinal(t, qovB, 1)},
		values.RecordConstructorField{Name: "AIDQ", Value: mustTestFieldOrdinal(t, qovA, 0)},
	)

	existCorr := values.NamedCorrelationIdentifier("EX")
	c, err := newFlatMapCursorWithOuterProperties(
		recordlayer.FromList([]QueryResult{}), step1, scanB, nil,
		EmptyEvaluationContext(), mergedCorr, existCorr, foldRV,
		recordlayer.ExecuteProperties{}, false)
	if err != nil {
		t.Fatalf("newFlatMapCursorWithOuterProperties: %v", err)
	}
	defer c.Close()
	if !c.foldWindowsOK {
		t.Fatal("the cursor must recognise the gated ordinal outer (foldWindowsOK) — the leg-window authority")
	}

	// The merged positional row [A.ID=1, A.V=10, B.ID=2, B.W=20], with an EMPTY
	// Datum: the fold can ONLY resolve through the positional leg windows.
	mergedType, ok := c.outerPlanObject.FlowedType().(*values.RecordType)
	if !ok || mergedType == nil {
		t.Fatalf("outer plan object type = %T, want exact record", c.outerPlanObject.FlowedType())
	}
	pos := NewPositionalRow(mergedType)
	pos.Set(0, int64(1))
	pos.Set(1, int64(10))
	pos.Set(2, int64(2))
	pos.Set(3, int64(20))
	outerRow := QueryResult{Positional: pos}
	innerPos := NewPositionalRow(legB)
	innerPos.Slots[0], innerPos.Slots[1] = int64(2), int64(20)
	innerRow := QueryResult{Positional: innerPos}

	out, err := c.computeResultLegs(outerRow, &innerRow)
	if err != nil {
		t.Fatalf("computeResultLegs: %v", err)
	}
	got, isMap := rowMapOK(out)
	if !isMap {
		t.Fatalf("fold output Datum is %T, want a map", out.Positional)
	}
	wantType, ok := foldRV.Type().(*values.RecordType)
	if !ok || out.Positional == nil || out.Positional.Type == nil || !out.Positional.Type.Equals(wantType) {
		t.Fatalf("fold output type = %v, want the exact result-program type %v", out.Positional, wantType)
	}
	if _, exactErr := values.SnapshotExactType(out.Positional.Type); exactErr != nil {
		t.Fatalf("fold output type is not exact: %v", exactErr)
	}
	// Every column resolved from the POSITIONAL row through legWindowRowContext.
	// A revert of the computeResultLegs branch reads the empty Datum → NULLs here.
	if got["AV"] != int64(10) {
		t.Errorf("dotted A.V = %v, want 10 (positional leg window; a Datum-fallback revert yields nil)", got["AV"])
	}
	if got["BW"] != int64(20) {
		t.Errorf("dotted B.W = %v, want 20 (the OTHER leg's window — spanAwareRow splits the dotted name)", got["BW"])
	}
	if got["AIDQ"] != int64(1) {
		t.Errorf("QOV(A).ID = %v, want 1 (legWindowBinder resolves the QOV leg ref)", got["AIDQ"])
	}
}

// TestFlatMapComputedWholeRecordFlowsWithoutScalarWrapper pins the non-build
// counterpart of ordinalJoinBuild's bare-row arm. A translated N-way join may
// leave a non-identity FieldValue whose selected slot is an entire record row.
// The row is the FlatMap output; wrapping it in scalarPositionalRow would emit
// RECORD<_0 RECORD<...>> and disagree with the exact record output carrier.
func TestFlatMapComputedWholeRecordFlowsWithoutScalarWrapper(t *testing.T) {
	t.Parallel()
	leg := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
	})
	merged := values.NewRecordType("", false, []values.Field{
		{Name: "_0", FieldType: leg, Ordinal: 0},
	})
	outerAlias := values.NamedCorrelationIdentifier("M")
	outer := ojWiringMustQOV(t, outerAlias, merged)
	wholeLeg := ojWiringMustField(t, outer, 0)

	c := &flatMapCursor{
		evalCtx:     EmptyEvaluationContext(),
		outerAlias:  outerAlias,
		innerAlias:  values.NamedCorrelationIdentifier("EX"),
		resultValue: wholeLeg,
	}
	legRow := NewPositionalRow(leg)
	legRow.Slots[0], legRow.Slots[1] = int64(7), int64(9)
	outerRow := NewPositionalRow(merged)
	outerRow.Slots[0] = legRow

	got, err := c.computeResultLegs(QueryResult{Positional: outerRow}, nil)
	if err != nil {
		t.Fatalf("computeResultLegs: %v", err)
	}
	if got.Positional != legRow {
		t.Fatalf("computed record = %v, want the selected record row itself %v", got.Positional, legRow)
	}
	wrongRow := NewPositionalRow(values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	}))
	outerRow.Slots[0] = wrongRow
	_, err = c.computeResultLegs(QueryResult{Positional: outerRow}, nil)
	var resolutionErr *values.ResolutionError
	if !errors.As(err, &resolutionErr) || resolutionErr.Code() != values.LayoutTypeMismatch {
		t.Fatalf("wrong computed record type error = %v, want LayoutTypeMismatch", err)
	}
	outerRow.Slots[0] = legRow

	// Scalar computed values still use the one-slot transport; the record arm
	// must not turn every non-RC result into a row pass-through.
	c.resultValue = values.LiteralValue(int64(42))
	scalar, err := c.computeResultLegs(QueryResult{Positional: outerRow}, nil)
	if err != nil {
		t.Fatalf("scalar computeResultLegs: %v", err)
	}
	if scalar.Positional == nil || len(scalar.Positional.Slots) != 1 || scalar.Positional.Slots[0] != int64(42) {
		t.Fatalf("computed scalar row = %v, want one slot containing 42", scalar.Positional)
	}
}

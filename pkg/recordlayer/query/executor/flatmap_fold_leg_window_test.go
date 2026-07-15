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
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestCursorResolvesFoldPositionally(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, _, seed := ojWiringLegs(t) // A(ID,V), B(ID,W); seed = [A.ID,A.V,B.ID,B.W]

	// The step-1 NLJ carries the ordinal seed as its result value — this is what
	// downstreamLegWindows reads to derive the leg windows (A@0, B@2).
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, legA, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, legB, false)
	step1 := plans.NewRecordQueryNestedLoopJoinPlan(scanA, scanB, nil, plans.JoinInner, "A", "B", seed)

	// Cross-agreement strengthening: the EXECUTOR twin (ordinalJoinSpansOf, via
	// downstreamLegWindows→joinPlanSpans) yields A@0 / B@2 for this seed — the
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
	// dotted-frontier read (A.V), another dotted read into the OTHER leg (B.W),
	// and a QOV-based leg ref (QOV(A).ID). None can resolve via the Datum below.
	foldRV := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "AV", Value: values.NewFieldValueWithResolvedOrdinal("A.V", 1, values.NotNullLong)},
		values.RecordConstructorField{Name: "BW", Value: values.NewFieldValueWithResolvedOrdinal("B.W", 3, values.NotNullLong)},
		values.RecordConstructorField{Name: "AIDQ", Value: values.NewCorrelatedFieldValueWithResolvedOrdinal(qovA, "ID", 0, values.NotNullLong)},
	)

	mergedCorr := values.NamedCorrelationIdentifier("M")
	existCorr := values.NamedCorrelationIdentifier("EX")
	c, err := newFlatMapCursor(
		recordlayer.FromList([]QueryResult{}), step1, scanB, nil,
		EmptyEvaluationContext(), mergedCorr, existCorr, foldRV, false,
		recordlayer.ExecuteProperties{})
	if err != nil {
		t.Fatalf("newFlatMapCursor: %v", err)
	}
	defer c.Close()
	if !c.foldWindowsOK {
		t.Fatal("the cursor must recognise the gated ordinal outer (foldWindowsOK) — the leg-window authority")
	}

	// The merged positional row [A.ID=1, A.V=10, B.ID=2, B.W=20], with an EMPTY
	// Datum: the fold can ONLY resolve through the positional leg windows.
	mergedType := values.NewRecordType("", false, []values.Field{
		{Name: "A_ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "A_V", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "B_ID", FieldType: values.NotNullLong, Ordinal: 2},
		{Name: "B_W", FieldType: values.NotNullLong, Ordinal: 3},
	})
	pos := NewPositionalRow(mergedType)
	pos.Set(0, int64(1))
	pos.Set(1, int64(10))
	pos.Set(2, int64(2))
	pos.Set(3, int64(20))
	outerRow := QueryResult{Positional: pos}
	innerRow := dmap(map[string]any{})

	out, err := c.computeResultLegs(outerRow, &innerRow)
	if err != nil {
		t.Fatalf("computeResultLegs: %v", err)
	}
	got, isMap := rowMapOK(out)
	if !isMap {
		t.Fatalf("fold output Datum is %T, want a map", out.Positional)
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

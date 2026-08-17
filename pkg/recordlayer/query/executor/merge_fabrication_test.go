package executor

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"google.golang.org/protobuf/proto"
)

// TestMergeRows_LegWindowedQualifiedReads pins the split between mergeRows'
// exact carrier construction and the plan-selected OrdinalLayout that owns
// qualified reads. Type.Legs remains diagnostic shape only: the merged row is
// deliberately unreadable until the explicit C/CC2 windows are attached.
func TestMergeRows_LegWindowedQualifiedReads(t *testing.T) {
	t.Parallel()
	outerAlias := values.NamedCorrelationIdentifier("C")
	innerAlias := values.NamedCorrelationIdentifier("CC2")
	outerType := exactTestRowType(values.Field{Name: "AK", FieldType: values.NotNullLong})
	innerType := exactTestRowType(values.Field{Name: "CV", FieldType: values.NotNullLong})
	outer := QueryResult{Positional: &PositionalRow{Type: outerType, Slots: []any{int64(100)}}}
	inner := QueryResult{Positional: &PositionalRow{Type: innerType, Slots: []any{int64(900)}}}
	merged := mergeRows(outer, inner, outerAlias, innerAlias)
	outerQOV := mustTestQOV(t, outerAlias, outerType)
	innerQOV := mustTestQOV(t, innerAlias, innerType)

	outerOnly, err := values.NewOrdinalLayoutForCarrierType(
		merged.Positional.Type,
		[]values.OrdinalTileSpec{{Start: 0, Width: 2, Kind: values.OrdinalTileFlat}},
		[]values.OrdinalWindowSpec{{Source: outerQOV, FieldPaths: [][]int{{0}}}},
	)
	if err != nil {
		t.Fatalf("outer-only NewOrdinalLayoutForCarrierType: %v", err)
	}
	outerOnlyRow, err := merged.Positional.AttachOrdinalLayout(outerOnly, outerOnly.Carrier().FlowedType())
	if err != nil {
		t.Fatalf("outer-only AttachOrdinalLayout: %v", err)
	}
	outerOnlyCtx, err := ordinalLayoutRowContext(&values.OrdinalBinderStorage{}, outerOnly, outerOnlyRow, nil, nil)
	if err != nil {
		t.Fatalf("outer-only ordinalLayoutRowContext: %v", err)
	}
	innerField := mustTestFieldOrdinal(t, innerQOV, 0)
	got, evalErr := innerField.Evaluate(outerOnlyCtx)
	var coded interface {
		Code() values.ResolutionErrorCode
	}
	if got != nil || !errors.As(evalErr, &coded) || coded.Code() != values.UnboundCorrelation {
		t.Fatalf("omitted CC2.CV = (%v, %v), want UnboundCorrelation despite ambient slot 1", got, evalErr)
	}

	layout, err := values.NewOrdinalLayoutForCarrierType(
		merged.Positional.Type,
		[]values.OrdinalTileSpec{{Start: 0, Width: 2, Kind: values.OrdinalTileFlat}},
		[]values.OrdinalWindowSpec{
			{Source: outerQOV, FieldPaths: [][]int{{0}}},
			{Source: innerQOV, FieldPaths: [][]int{{1}}},
		},
	)
	if err != nil {
		t.Fatalf("NewOrdinalLayoutForCarrierType: %v", err)
	}
	merged.Positional, err = merged.Positional.AttachOrdinalLayout(layout, layout.Carrier().FlowedType())
	if err != nil {
		t.Fatalf("AttachOrdinalLayout: %v", err)
	}
	if v, ok := legRead(merged.Positional, "C", "AK"); !ok || v != int64(100) {
		t.Fatalf("C.AK = %v, want 100 (leg window)", v)
	}
	if v, ok := legRead(merged.Positional, "CC2", "CV"); !ok || v != int64(900) {
		t.Fatalf("CC2.CV = %v, want 900 (leg window)", v)
	}
}

// TestSortContinuation_PositionalRoundTripAndReject pins the continuation round-trip
// of the positional payload (the SOLE runtime row a resumed sort buffer must carry). It
// also pins the legacy-continuation regression fix: a pre-positional continuation (a legacy
// name-keyed JSON object, or a v2 [_, complete] array — both written by an older
// binary) carries no positional and is therefore UNRECONSTRUCTABLE in the ordinal
// model; decode must REJECT it loudly rather than silently decode to a
// nil-positional row (which resumes as all-NULL sort keys and wrong/misordered rows
// now that the name-keyed Datum fallback is deleted). (The retired QueryResult
// .Complete field is no longer round-tripped — slot 1 is an ignored dead placeholder.)
func TestSortContinuation_PositionalRoundTripAndReject(t *testing.T) {
	t.Parallel()

	r0 := dmap(map[string]any{"AK": int64(100)})
	r1 := dmap(map[string]any{"AK": int64(110)})
	buf := []QueryResult{r0, r1}
	enc, err := encodeSortContinuation(nil, buf, false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	_, decoded, _, err := decodeSortContinuation(enc, nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("row count lost in round-trip: %+v", decoded)
	}
	if rowVal(decoded[0], "AK") != int64(100) || rowVal(decoded[1], "AK") != int64(110) {
		t.Fatalf("positional lost in round-trip: %+v", decoded)
	}

	// A resumed row with NO positional payload must be rejected loudly, never
	// silently decoded to a nil-positional (all-NULL) row.
	for _, tc := range []struct {
		name    string
		message []byte
	}{
		{"legacy_object_payload", []byte(`{"AK":100}`)},       // pre-migration name-keyed datum
		{"v2_array_no_positional", []byte(`[null, false]`)},   // v2 [_, complete], no positional slot
		{"v2_array_with_datum", []byte(`[{"AK":100}, true]`)}, // v2 carrying the old datum
	} {
		t.Run(tc.name+"_rejected", func(t *testing.T) {
			t.Parallel()
			sr, _ := proto.Marshal(&gen.SortedRecord{Message: tc.message})
			legacy, _ := proto.Marshal(&gen.MemorySortContinuation{Records: [][]byte{sr}})
			_, _, _, err := decodeSortContinuation(legacy, nil)
			if err == nil {
				t.Fatal("pre-positional payload must be REJECTED (no positional) — a silently dropped row is wrong results with no error")
			}
			// RFC-180 H6: legacy shapes are rejected as not-this-binary's
			// format (bad arity or non-array payload), never resumed.
			if !strings.Contains(err.Error(), "want 3") && !strings.Contains(err.Error(), "failed to unmarshal sorted record") {
				t.Fatalf("decode error = %q, want strict-format rejection", err)
			}
		})
	}
}

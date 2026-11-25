package executor

// The merge's result value is baked positionally at PLAN time against a flat
// concatenation of its children, each assumed to span len(groupCols)+1 slots.
// Three files derive that width independently — the planning rule that bakes
// the ordinals, the cursor that sizes the absent-child filler, and the
// concatenation itself — and nothing made them agree.
//
// A disagreement does not crash and does not change the row count. It shifts
// every LATER child's aggregate by the difference, so each group reports
// another group's value in well-formed rows. That is the silent wrong-rows
// class, and the only defence against it is to check the width at merge time
// instead of assuming it. These tests pin that the check exists and is exact.

import (
	"context"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// childRow builds a child result of the given width, as an aggregate-index
// scan flows it: [groupCols..., aggregate].
func childRow(width int) QueryResult {
	fields := make([]values.Field, width)
	slots := make([]any, width)
	for i := range fields {
		fields[i] = values.Field{Ordinal: i, FieldType: values.NotNullLong}
		slots[i] = int64(i)
	}
	return QueryResult{Positional: &PositionalRow{
		Type:  &values.RecordType{Fields: fields},
		Slots: slots,
	}}
}

func TestMergeChildEvalArg_RejectsChildWiderThanTheBakedAssumption(t *testing.T) {
	t.Parallel()

	// Two children where the plan baked 2 slots each, but the SECOND flows 3.
	// The first child is well-formed, so a check that only looked at child 0 —
	// or only at the total length — would pass this.
	children := []QueryResult{childRow(2), childRow(3)}

	got, err := mergeChildEvalArg(children, 2)
	if err == nil {
		t.Fatalf("a child flowing 3 columns against a baked width of 2 was accepted "+
			"(result %v). Every later child's aggregate is then read from the wrong "+
			"slot and each group reports another group's value, with the row count "+
			"unchanged and every row well-formed — nothing downstream can detect it.", got)
	}
	if !strings.Contains(err.Error(), "child 1") {
		t.Fatalf("the error must name WHICH child disagreed, so the three independent "+
			"width derivations can be told apart; got: %v", err)
	}
}

func TestMergeChildEvalArg_RejectsChildNarrowerThanTheBakedAssumption(t *testing.T) {
	t.Parallel()

	// The opposite direction, which is the one an absent-child filler sized from
	// a stale width would produce.
	children := []QueryResult{childRow(3), childRow(2)}

	if _, err := mergeChildEvalArg(children, 3); err == nil {
		t.Fatal("a child flowing 2 columns against a baked width of 3 was accepted; " +
			"a SHORT child shifts later children left, which is exactly what a filler " +
			"sized from a stale width does")
	}
}

func TestMergeChildEvalArg_AcceptsTheExactBakedWidth(t *testing.T) {
	t.Parallel()

	// The control. The check must be exact, not a lower or upper bound: if it
	// only rejected one direction, or only flagged gross mismatches, the two
	// tests above could pass while real merges still drifted.
	children := []QueryResult{childRow(2), childRow(2), childRow(2)}

	arg, err := mergeChildEvalArg(children, 2)
	if err != nil {
		t.Fatalf("three children at exactly the baked width were rejected: %v", err)
	}
	row, ok := arg.(*PositionalRow)
	if !ok {
		t.Fatalf("expected a concatenated PositionalRow, got %T", arg)
	}
	if len(row.Type.Fields) != 6 {
		t.Fatalf("concatenation of 3 children x 2 slots must be 6 wide, got %d",
			len(row.Type.Fields))
	}
	// The ordinals the baked references read must be the flat 0..5, renumbered
	// across the concatenation rather than repeating each child's local 0,1.
	for i, f := range row.Type.Fields {
		if f.Ordinal != i {
			t.Fatalf("concatenated field %d carries ordinal %d; the result value's "+
				"baked references address the FLAT concatenation, so a child's local "+
				"ordinals must be renumbered", i, f.Ordinal)
		}
	}
}

func TestMultiIntersectionMergeCursor_PreservesPlanExactOutputType(t *testing.T) {
	t.Parallel()
	mergedType := &values.RecordType{RecordName: "merged_aggregate_rows", Fields: []values.Field{
		{Name: "G", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "SUM(V)", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "G", FieldType: values.NullableLong, Ordinal: 2},
		{Name: "COUNT(*)", FieldType: values.NullableLong, Ordinal: 3},
	}}
	root := mustTestQOV(t, values.UniqueCorrelationIdentifier(), mergedType)
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "G", Value: mustTestFieldOrdinal(t, root, 0)},
		values.RecordConstructorField{Name: "COUNT(*)", Value: mustTestFieldOrdinal(t, root, 3)},
	)
	outputType := resultValue.Type().(*values.RecordType)
	children := []QueryResult{
		{Positional: &PositionalRow{
			Type: values.NewRecordType("sum_row", false, []values.Field{
				{Name: "G", FieldType: values.NullableLong, Ordinal: 0},
				{Name: "SUM(V)", FieldType: values.NullableLong, Ordinal: 1},
			}),
			Slots: []any{int64(7), int64(42)},
		}},
		{Positional: &PositionalRow{
			Type: values.NewRecordType("count_row", false, []values.Field{
				{Name: "G", FieldType: values.NullableLong, Ordinal: 0},
				{Name: "COUNT(*)", FieldType: values.NullableLong, Ordinal: 1},
			}),
			Slots: []any{int64(7), int64(1)},
		}},
	}
	cursor := &multiIntersectionMergeCursor{
		inner:       recordlayer.FromList([][]QueryResult{children}),
		resultValue: resultValue,
		outputType:  outputType,
		childWidth:  2,
	}
	defer cursor.Close()
	result, err := cursor.OnNext(context.Background())
	if err != nil || !result.HasNext() {
		t.Fatalf("OnNext = (%v,%v), want a row", result, err)
	}
	row := result.GetValue().Positional
	if row == nil || !row.Type.Equals(outputType) {
		t.Fatalf("output type = %v, want exact plan type %v", row, outputType)
	}
	for i, field := range row.Type.Fields {
		if field.FieldType.Code() == values.TypeCodeUnknown {
			t.Fatalf("output field %d erased to UNKNOWN: %v", i, field)
		}
	}
}

func TestMergeChildEvalArg_OptOutRequiresTheExplicitSentinel(t *testing.T) {
	t.Parallel()

	children := []QueryResult{childRow(2), childRow(5)}

	// The escape hatch is a NEGATIVE sentinel, and it still works.
	if _, err := mergeChildEvalArg(children, mergeChildWidthUnchecked); err != nil {
		t.Fatalf("the explicit opt-out must disable the check, got: %v", err)
	}

	// The zero value must NOT be that escape. This is the half the previous
	// version of this test got backwards: it blessed 0 as "the documented
	// escape", which meant a caller that simply forgot to set childWidth
	// disarmed the assertion and every test stayed green. An assertion whose
	// default state is disarmed cannot catch the wiring bug it exists for.
	if _, err := mergeChildEvalArg(children, 0); err == nil {
		t.Fatal("an UNSTATED width was accepted. The zero value must fail closed: " +
			"deleting the cursor's childWidth wiring leaves the field at 0, and if 0 " +
			"means 'skip the check' that deletion is invisible to the whole suite. " +
			"Opting out must cost an explicit mergeChildWidthUnchecked.")
	}

	// Nor may any OTHER negative opt out. The sentinel is matched by equality,
	// not by sign: a -2 falling out of arithmetic on an unexpected grouping key
	// is a bug, and a guard spelled `> 0` would silently accept it as a request
	// to skip the check — the same fail-open shape this contract was inverted to
	// remove, one step further from the default.
	for _, w := range []int{-2, -7} {
		if _, err := mergeChildEvalArg(children, w); err == nil {
			t.Fatalf("width %d opted out of the check. Only the exact "+
				"mergeChildWidthUnchecked sentinel may disable it; an arbitrary negative "+
				"is arithmetic gone wrong and must be loud.", w)
		}
	}
}

package cascades

// A duplicate-named RecordType is built as a raw literal in this file because
// NewRecordType deliberately rejects duplicate field names. The shape is still
// real: a positional join/box run can concatenate two buried fields both named
// K. RFC-232 makes the resulting references exact QOV-rooted ordinal paths, so
// display-name ambiguity is no longer an input to rebasing. K#0 and K#1 must map
// to different merged slots, and a nested suffix must remain attached to the
// selected ordinal.

import (
	"fmt"
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// dupNamedLegFixture builds the concatenated box-run shape L = [K, K, Z],
// whose run starts at merged slot 10. kType is shared by both duplicate-named
// fields and by their corresponding merged slots.
func dupNamedLegFixture(
	t testing.TB,
	kType values.Type,
) (*values.RecordType, values.QuantifiedObjectValue) {
	t.Helper()
	leg := &values.RecordType{Fields: []values.Field{
		{Name: "K", FieldType: kType, Ordinal: 0},
		{Name: "K", FieldType: kType, Ordinal: 1},
		{Name: "Z", FieldType: values.NullableInt, Ordinal: 2},
	}}
	mergedFields := make([]values.Field, 16)
	for i := range mergedFields {
		mergedFields[i] = values.Field{
			Name:      fmt.Sprintf("M%d", i),
			FieldType: values.NullableInt,
			Ordinal:   i,
		}
	}
	mergedFields[10].FieldType = kType
	mergedFields[11].FieldType = kType
	mergedType := values.NewRecordType("", false, mergedFields)
	mergedQOV, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("M"), mergedType)
	mergedQOV = mustConstruct(t, mergedQOV, err)
	return leg, mergedQOV
}

func dupNamedLegWindows(leg *values.RecordType) map[values.CorrelationIdentifier]ordinalLegWindow {
	return map[values.CorrelationIdentifier]ordinalLegWindow{
		values.NamedCorrelationIdentifier("L"): {
			Kind:   values.LegKindFlatRun,
			Offset: 10,
			Typ:    leg,
			Alias:  values.NamedCorrelationIdentifier("L"),
		},
	}
}

func dupNamedLegReference(
	t testing.TB,
	leg *values.RecordType,
	ordinals []int,
	frontierPinned bool,
) values.Value {
	t.Helper()
	legQOV, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("L"), leg)
	legQOV = mustConstruct(t, legQOV, err)
	if frontierPinned {
		if len(ordinals) != 1 {
			t.Fatalf("frontier-pinned fixture path = %v, want one top-level ordinal", ordinals)
		}
		resolved, resolveErr := values.ResolveOrdinalSeedField(legQOV, ordinals[0])
		return mustConstruct(t, resolved, resolveErr)
	}
	resolved, resolveErr := values.ResolveFieldOrdinals(legQOV, ordinals)
	return mustConstruct(t, resolved, resolveErr)
}

// rebasedOrdinals drives the production symbol and returns its exact merged
// path. Interface recognition keeps the test independent of sealed concrete
// FieldValue representation.
func rebasedOrdinals(
	t testing.TB,
	ref values.Value,
	leg *values.RecordType,
	merged values.QuantifiedObjectValue,
) ([]int, bool) {
	t.Helper()
	out, ok := rebaseOuterLegValueOrdinal(ref, dupNamedLegWindows(leg), merged)
	if !ok {
		return nil, false
	}
	field, isField := values.AsFieldValue(out)
	if !isField || field.Path() == nil {
		t.Fatalf("rebase produced %T (%v), want an exact FieldValue", out, out)
	}
	return field.Path().Ordinals(), true
}

func requireDupNamedRebase(
	t testing.TB,
	ref values.Value,
	leg *values.RecordType,
	merged values.QuantifiedObjectValue,
	want []int,
) {
	t.Helper()
	got, ok := rebasedOrdinals(t, ref, leg, merged)
	if !ok {
		t.Fatalf("exact ordinal reference declined; want merged path %v", want)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rebased path = %v, want %v", got, want)
	}
}

// The two K fields have the same display name and type. Only their exact root
// ordinal distinguishes them, so mapping K#1 to slot 11 is the wrong-column
// regression pin: a first-match name lookup would instead produce slot 10.
func TestDupNamedLegWindow_ExactOrdinalSelectsSecondDuplicate(t *testing.T) {
	t.Parallel()

	leg, merged := dupNamedLegFixture(t, values.NullableInt)
	ref := dupNamedLegReference(t, leg, []int{1}, false)
	field, ok := values.AsFieldValue(ref)
	if !ok {
		t.Fatalf("source reference = %T, want exact FieldValue", ref)
	}
	if field.DisplayName() != "K" || !slices.Equal(field.Path().Ordinals(), []int{1}) {
		t.Fatalf("source reference = %s%v, want K[1]", field.DisplayName(), field.Path().Ordinals())
	}

	requireDupNamedRebase(t, ref, leg, merged, []int{11})
}

// Exact paths make the former name/multi/pinned dispatch distinction
// unnecessary. This test pins the two cases that remain easy to get subtly
// wrong: preserving a nested suffix, and honoring a physical seed pin's root
// ordinal even when its display name is duplicated.
func TestDupNamedLegWindow_ExactPathsPreserveSuffixAndPin(t *testing.T) {
	t.Parallel()

	t.Run("nested suffix follows the second duplicate", func(t *testing.T) {
		t.Parallel()
		nestedK := values.NewRecordType("", true, []values.Field{
			{Name: "SUB", FieldType: values.NullableInt, Ordinal: 0},
		})
		leg, merged := dupNamedLegFixture(t, nestedK)
		ref := dupNamedLegReference(t, leg, []int{1, 0}, false)
		requireDupNamedRebase(t, ref, leg, merged, []int{11, 0})
	})

	t.Run("frontier pin retains the second duplicate ordinal", func(t *testing.T) {
		t.Parallel()
		leg, merged := dupNamedLegFixture(t, values.NullableInt)
		ref := dupNamedLegReference(t, leg, []int{1}, true)
		field, ok := values.AsFieldValue(ref)
		if !ok || field.Path() == nil || !field.Path().IsFrontierPinned() {
			t.Fatalf("source reference = %T (%v), want frontier-pinned exact FieldValue", ref, ref)
		}
		requireDupNamedRebase(t, ref, leg, merged, []int{11})
	})
}

// The unique Z control proves the offset is applied to the whole leg, while
// K#0 proves duplicate names do not make the exact ordinal path collapse or
// decline. Together with K#1 above, they distinguish positional mapping from
// either a constant-slot bug or a display-name first match.
func TestDupNamedLegWindow_ControlsDistinguishFixtureFromFinding(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name string
		path []int
		want []int
	}{
		{name: "unique Z maps to its slot", path: []int{2}, want: []int{12}},
		{name: "first duplicate maps by ordinal zero", path: []int{0}, want: []int{10}},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			leg, merged := dupNamedLegFixture(t, values.NullableInt)
			ref := dupNamedLegReference(t, leg, testCase.path, false)
			requireDupNamedRebase(t, ref, leg, merged, testCase.want)
		})
	}
}

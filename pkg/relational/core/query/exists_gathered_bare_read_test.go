package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestRebaseLegRefsToBoxUsesExactFlatWindow(t *testing.T) {
	t.Parallel()

	legType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	}}
	mergedType := &values.RecordType{Fields: []values.Field{
		{Name: "PAD", FieldType: values.NullableString, Ordinal: 0},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1},
	}}
	legCorr := values.NamedCorrelationIdentifier("L")
	windows := map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow{
		legCorr: {Kind: values.LegKindFlatRun, Offset: 1, Typ: legType, Alias: legCorr},
	}
	box := exactTestQOV(t, "$box", mergedType)
	ref := exactTestField(t, exactTestQOV(t, "L", legType), 0)

	out, ok := rebaseLegRefsToBox(ref, windows, mergedType, box)
	if !ok {
		t.Fatal("exact leg reference did not rebase")
	}
	field := exactTestFieldView(t, out)
	owner, ownerOK := values.AsQuantifiedObjectValue(field.ChildValue())
	if !ownerOK || owner.Correlation() != values.NamedCorrelationIdentifier("$box") {
		t.Fatalf("rebased owner = %T, want exact box QOV", field.ChildValue())
	}
	if got := field.Path().Ordinals(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("rebased path = %v, want [1]", got)
	}

	wrongBox := exactTestQOV(t, "$wrong", &values.RecordType{Fields: []values.Field{
		{Name: "PAD", FieldType: values.NullableString, Ordinal: 0},
	}})
	if got, ok := rebaseLegRefsToBox(ref, windows, mergedType, wrongBox); ok || got != ref {
		t.Fatalf("missing box slot did not fail closed: got %T ok=%v", got, ok)
	}
}

func TestRebaseLegRefsToBoxKeysWindowsByCorrelationIdentity(t *testing.T) {
	t.Parallel()

	machine := values.NamedCorrelationIdentifier("q$7")
	legType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	}}
	windows := map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow{
		machine: {Kind: values.LegKindFlatRun, Offset: 0, Typ: legType, Alias: machine},
	}
	box := exactTestQOV(t, "$box", legType)
	ref := exactTestField(t, exactTestQOV(t, "q$7", legType), 0)
	out, ok := rebaseLegRefsToBox(ref, windows, legType, box)
	if !ok || out == ref {
		t.Fatalf("machine-minted exact correlation was not rebased: got %T ok=%v", out, ok)
	}

	// Mutation control: case-folding would incorrectly match this distinct
	// correlation to the lowercase window.
	upper := exactTestField(t, exactTestQOV(t, "Q$7", legType), 0)
	upperOut, upperOK := rebaseLegRefsToBox(upper, windows, legType, box)
	if !upperOK || upperOut != upper {
		t.Fatalf("distinct uppercase correlation matched lowercase window: got %T ok=%v", upperOut, upperOK)
	}
}

func TestRebaseLegRefsToBoxUsesNestedWindowPath(t *testing.T) {
	t.Parallel()

	legType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	}}
	mergedType := &values.RecordType{Fields: []values.Field{
		{Name: "PAD", FieldType: values.NullableString, Ordinal: 0},
		{Name: "NESTED", FieldType: legType, Ordinal: 1},
	}}
	legCorr := values.NamedCorrelationIdentifier("N")
	windows := map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow{
		legCorr: {Kind: values.LegKindNested, Offset: 1, Typ: legType, Alias: legCorr},
	}
	box := exactTestQOV(t, "$box", mergedType)
	ref := exactTestField(t, exactTestQOV(t, "N", legType), 0)
	out, ok := rebaseLegRefsToBox(ref, windows, mergedType, box)
	if !ok {
		t.Fatal("nested exact leg reference did not rebase")
	}
	if got := exactTestFieldView(t, out).Path().Ordinals(); len(got) != 2 || got[0] != 1 || got[1] != 0 {
		t.Fatalf("nested rebased path = %v, want [1 0]", got)
	}

	badWindows := map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow{
		legCorr: {Kind: values.LegKindUnset, Offset: 1, Typ: legType, Alias: legCorr},
	}
	if got, ok := rebaseLegRefsToBox(ref, badWindows, mergedType, box); ok || got != ref {
		t.Fatalf("unset window kind did not fail closed: got %T ok=%v", got, ok)
	}
}

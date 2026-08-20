package plandiff

// A STRUCT or ARRAY cell must reach the cross-engine comparison in the shape
// Java's JSON decode produces — a map[string]any and a []any — not as the Go
// POINTER the driver hands over.
//
// Before the two arms in coerceForComparison, both fell through to the
// pass-through default and were compared (and printed) as `0x…` heap
// addresses. That is the failure mode where an instrument goes blind rather
// than loud: a struct-valued column could never match Java, so it read as a
// permanent divergence, and a REAL struct divergence was indistinguishable
// from agreement because both render as an address that changes every run.
// It was found by a probe on CASE arms whose measurement could not be
// interpreted until the harness could render the cell.
//
// The arms are driven HERE rather than only through the corpus because a
// corpus run exercises only the cell types its fixtures happen to contain:
// nested structs, arrays of structs and an unnamed-attribute struct are all
// reachable through the driver (api.Array documents the nesting) and none is
// guaranteed to appear in any given corpus sweep. An arm whose first real
// firing is in a cross-engine report is an untested branch being read as a
// finding.

import (
	"reflect"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// ---- doubles ---------------------------------------------------------------

// fakeStructMD names attributes positionally from a list. A nil/short list is
// how the "metadata cannot name this attribute" arm is driven.
type fakeStructMD struct{ names []string }

func (m *fakeStructMD) TypeName() string    { return "FAKE" }
func (m *fakeStructMD) AttributeCount() int { return len(m.names) }
func (m *fakeStructMD) AttributeName(oneBased int) (string, error) {
	if oneBased < 1 || oneBased > len(m.names) {
		return "", api.NewErrorf(api.ErrCodeInvalidColumnReference, "no attribute %d", oneBased)
	}
	return m.names[oneBased-1], nil
}
func (m *fakeStructMD) AttributeType(int) (int, error)              { return 0, nil }
func (m *fakeStructMD) AttributeTypeName(int) (string, error)       { return "", nil }
func (m *fakeStructMD) AttributeDataType(int) (api.DataType, error) { return nil, nil }
func (m *fakeStructMD) AttributeNullable(int) (int, error)          { return 0, nil }

type fakeStruct struct {
	md    api.StructMetaData
	attrs []any
}

func (s *fakeStruct) MetaData() api.StructMetaData { return s.md }
func (s *fakeStruct) AttributeCount() int          { return len(s.attrs) }
func (s *fakeStruct) Attribute(oneBased int) (any, error) {
	if oneBased < 1 || oneBased > len(s.attrs) {
		return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference, "no attribute %d", oneBased)
	}
	return s.attrs[oneBased-1], nil
}
func (s *fakeStruct) AttributeByName(string) (any, error) { return nil, nil }
func (s *fakeStruct) Attributes() []any                   { return s.attrs }

type fakeArrayMD struct{}

func (m *fakeArrayMD) ElementType() int              { return 0 }
func (m *fakeArrayMD) ElementTypeName() string       { return "BIGINT" }
func (m *fakeArrayMD) ElementDataType() api.DataType { return nil }
func (m *fakeArrayMD) Nullable() int                 { return 0 }

type fakeArray struct{ elems []any }

func (a *fakeArray) MetaData() api.ArrayMetaData { return &fakeArrayMD{} }
func (a *fakeArray) BaseType() int               { return 0 }
func (a *fakeArray) BaseTypeName() string        { return "BIGINT" }
func (a *fakeArray) Length() int                 { return len(a.elems) }
func (a *fakeArray) Element(oneBased int) (any, error) {
	if oneBased < 1 || oneBased > len(a.elems) {
		return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference, "no element %d", oneBased)
	}
	return a.elems[oneBased-1], nil
}
func (a *fakeArray) Elements() []any { return a.elems }

func namedStruct(names []string, attrs ...any) *fakeStruct {
	return &fakeStruct{md: &fakeStructMD{names: names}, attrs: attrs}
}

// ---- the arms --------------------------------------------------------------

func TestCoerceForComparison_StructAndArrayCells(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   any
		want any
	}{
		{
			// The shape the CASE probe produced: `(5, 6)` with ordinal names.
			// Java's decode renders {"_0":5,"_1":6}; the numbers arrive from
			// JSON as float64, which is why the int64s must normalise.
			name: "struct of bigints matches Java's ordinal-named object",
			in:   namedStruct([]string{"_0", "_1"}, int64(5), int64(6)),
			want: map[string]any{"_0": float64(5), "_1": float64(6)},
		},
		{
			name: "declared field names are used, not positions",
			in:   namedStruct([]string{"A", "B"}, int64(1), "x"),
			want: map[string]any{"A": float64(1), "B": "x"},
		},
		{
			// Metadata that cannot name an attribute must NOT drop it: a
			// dropped attribute makes a 2-field struct compare equal to a
			// 1-field one, which is a silent false agreement.
			name: "an unnameable attribute falls back to its ordinal, never vanishes",
			in:   &fakeStruct{md: &fakeStructMD{names: []string{"A"}}, attrs: []any{int64(1), int64(2)}},
			want: map[string]any{"A": float64(1), "_1": float64(2)},
		},
		{
			name: "nil metadata still yields every attribute",
			in:   &fakeStruct{md: nil, attrs: []any{int64(7)}},
			want: map[string]any{"_0": float64(7)},
		},
		{
			name: "a NULL attribute stays nil rather than becoming a zero",
			in:   namedStruct([]string{"A", "B"}, nil, int64(0)),
			want: map[string]any{"A": nil, "B": float64(0)},
		},
		{
			name: "BYTES inside a struct take the same base64 encoding as at top level",
			in:   namedStruct([]string{"A"}, []byte{0x01, 0x02}),
			want: map[string]any{"A": base64Encode([]byte{0x01, 0x02})},
		},
		{
			name: "IEEE specials inside a struct take the same string encoding",
			in:   namedStruct([]string{"A"}, mustInf()),
			want: map[string]any{"A": "Infinity"},
		},
		{
			name: "a nested struct is coerced recursively",
			in:   namedStruct([]string{"OUT"}, namedStruct([]string{"IN"}, int64(3))),
			want: map[string]any{"OUT": map[string]any{"IN": float64(3)}},
		},
		{
			name: "an array of scalars matches Java's JSON list",
			in:   &fakeArray{elems: []any{int64(1), int64(2)}},
			want: []any{float64(1), float64(2)},
		},
		{
			name: "an empty array is an empty list, not nil",
			in:   &fakeArray{elems: nil},
			want: []any{},
		},
		{
			name: "an array of structs is coerced element-wise",
			in:   &fakeArray{elems: []any{namedStruct([]string{"A"}, int64(1))}},
			want: []any{map[string]any{"A": float64(1)}},
		},
		{
			name: "a struct holding an array recurses the other direction too",
			in:   namedStruct([]string{"L"}, &fakeArray{elems: []any{int64(9)}}),
			want: map[string]any{"L": []any{float64(9)}},
		},
		{
			name: "a NULL cell is still nil and never reaches either arm",
			in:   nil,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := coerceForComparison(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("coerceForComparison:\n  got  %#v\n  want %#v", got, tc.want)
			}
		})
	}
}

// TestCoerceForComparison_StructCellIsNeverAPointer is the guard that names the
// defect directly rather than only its repair. reflect.Ptr is what the harness
// used to emit for every struct and array cell, and it is what any FUTURE
// non-scalar driver type would emit if it were added without an arm here — so
// the assertion is on the KIND, not on the specific types above.
func TestCoerceForComparison_StructCellIsNeverAPointer(t *testing.T) {
	t.Parallel()
	for _, in := range []any{
		namedStruct([]string{"A"}, int64(1)),
		&fakeArray{elems: []any{int64(1)}},
		namedStruct([]string{"N"}, &fakeArray{elems: []any{namedStruct([]string{"A"}, int64(1))}}),
	} {
		got := coerceForComparison(in)
		if k := reflect.ValueOf(got).Kind(); k == reflect.Ptr {
			t.Fatalf("coerceForComparison(%T) returned a %v — a pointer renders as a heap address, "+
				"which no Java rendering can equal and which changes every run, so the cell reads as "+
				"a permanent divergence AND hides a real one", in, k)
		}
	}
}

func mustInf() float64 {
	// Written as a division rather than math.Inf so the test needs no extra
	// import for a single value.
	zero := 0.0
	return 1.0 / zero
}

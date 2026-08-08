// Pure-Go unit tests for STRUCT-column comparison in the diff layer.
// No FDB or Docker: they exercise normalizeValue/valueEqual directly.

package yamsql

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/rowstruct"
)

// theDriverStructTypeIsCoveredByTheStructArm is the load-bearing link between
// this file's fake and production. normalizeValue dispatches on the api.Struct
// INTERFACE, so the arm applies to a driver row value only if the driver's
// concrete struct type implements it. It does — and if that ever stops being
// true, a struct column silently falls back to the `%T:%v` default and renders
// as a pointer address again, which is exactly the unassertable state this arm
// was added to remove.
var theDriverStructTypeIsCoveredByTheStructArm api.Struct = (*rowstruct.MessageStruct)(nil)

// fakeStruct is a minimal api.Struct. Only Attributes is load-bearing for the
// comparator; the rest satisfy the interface.
type fakeStruct struct{ attrs []any }

func (f fakeStruct) MetaData() api.StructMetaData { return nil }
func (f fakeStruct) AttributeCount() int          { return len(f.attrs) }
func (f fakeStruct) Attributes() []any            { return f.attrs }
func (f fakeStruct) Attribute(i int) (any, error) { return f.attrs[i-1], nil }
func (f fakeStruct) AttributeByName(string) (any, error) {
	return nil, nil
}

// TestDiffRows_StructColumnComparesByValue pins that a STRUCT column is
// compared POSITIONALLY against a nested YAML sequence.
//
// Before the struct arm existed, a struct value fell to the `%T:%v` default and
// stringified as a POINTER ADDRESS, so every expectation mismatched and no
// scenario could assert a struct column at all. A row-count or row-order
// assertion cannot stand in for this: it is a wrong-COLUMN check.
func TestDiffRows_StructColumnComparesByValue(t *testing.T) {
	t.Parallel()

	actual := [][]any{
		{int64(3), fakeStruct{attrs: []any{int64(30), int64(3)}}, true},
		{int64(2), fakeStruct{attrs: []any{int64(40), int64(2)}}, false},
	}
	// The expected side is what the YAML decoder produces: plain ints inside a
	// nested []any, which must promote to int64 exactly as top-level values do.
	expected := [][]any{
		{3, []any{30, 3}, true},
		{2, []any{40, 2}, false},
	}
	if d := diffRows(expected, actual, false); d != "" {
		t.Fatalf("a matching struct column reported a diff:\n%s", d)
	}
}

// TestDiffRows_StructColumnMemberMismatchIsRed is the negative direction: the
// comparison must actually READ the members. Only the SECOND member differs,
// so a comparator that looked at the first attribute alone — or at the struct's
// length, or at its identity — would stay green here.
func TestDiffRows_StructColumnMemberMismatchIsRed(t *testing.T) {
	t.Parallel()

	actual := [][]any{{int64(3), fakeStruct{attrs: []any{int64(30), int64(3)}}}}
	expected := [][]any{{3, []any{30, 999}}}
	d := diffRows(expected, actual, false)
	if d == "" {
		t.Fatal("a struct column whose second member differs compared EQUAL — " +
			"the comparator is not reading struct members")
	}
	if !strings.Contains(d, "999") {
		t.Fatalf("the diff does not name the mismatching member:\n%s", d)
	}
}

// TestDiffRows_StructColumnArityMismatchIsRed pins the other way a struct can
// differ: same leading members, different member COUNT. A prefix comparison
// would pass this.
func TestDiffRows_StructColumnArityMismatchIsRed(t *testing.T) {
	t.Parallel()

	actual := [][]any{{fakeStruct{attrs: []any{int64(30), int64(3)}}}}
	expected := [][]any{{[]any{30}}}
	if d := diffRows(expected, actual, false); d == "" {
		t.Fatal("a 2-member struct compared EQUAL to a 1-member expectation")
	}
}

// TestDiffRows_UnorderedStructColumnDoesNotPanic covers the sort path: rowKey
// renders each value with %T:%v, and an unnormalised struct would key on its
// ADDRESS, making unordered comparison nondeterministic. Normalisation happens
// before sorting, so the key is the member values.
func TestDiffRows_UnorderedStructColumnDoesNotPanic(t *testing.T) {
	t.Parallel()

	actual := [][]any{
		{int64(2), fakeStruct{attrs: []any{int64(40)}}},
		{int64(1), fakeStruct{attrs: []any{int64(50)}}},
	}
	expected := [][]any{
		{1, []any{50}},
		{2, []any{40}},
	}
	if d := diffRows(expected, actual, true); d != "" {
		t.Fatalf("unordered struct comparison reported a diff:\n%s", d)
	}
}

package values

// The exact-snapshot error PATH names where in a type graph the refusal
// happened, and nothing pinned it.
//
// That mattered the moment the path stopped being threaded down the recursion
// as an eagerly concatenated string and started being built on the way OUT, one
// segment per frame, on the error branch only. The two constructions must
// produce identical text — otherwise a diagnostic that used to say
// `type.field[1].element` silently starts saying something else, and the only
// reader who would notice is someone debugging a refusal at 3am.
//
// The eager form cost three allocations per record field per snapshot to
// describe a location almost nothing reads: snapshotExactType was 14.7% of a
// planner sweep's samples with runtime.concatstring4 16.2% of that. These arms
// are what make removing it checkable.

import (
	"errors"
	"testing"
)

func exactPathOf(t *testing.T, typ Type) (string, ResolutionErrorCode) {
	t.Helper()
	handle, err := SnapshotExactType(typ)
	if err == nil {
		t.Fatalf("snapshot of %v succeeded; this fixture must REFUSE or the arm proves nothing (got %v)", typ, handle)
	}
	var resolution *ResolutionError
	if !errors.As(err, &resolution) {
		t.Fatalf("error %v is not a *ResolutionError, so it carries no path", err)
	}
	return resolution.Path, resolution.ErrorCode
}

func TestExactSnapshotErrorPathNamesTheOffendingNode(t *testing.T) {
	t.Parallel()

	// UnknownType is a placeholder, so it refuses wherever it appears — which
	// makes it the probe for "which node did we reach".
	rec := func(fields ...Field) *RecordType { return &RecordType{Fields: fields} }
	fld := func(name string, ord int, typ Type) Field {
		return Field{Name: name, Ordinal: ord, FieldType: typ}
	}

	for _, tc := range []struct {
		name string
		typ  Type
		want string
	}{
		{
			name: "root",
			typ:  UnknownType,
			want: "type",
		},
		{
			name: "first_field",
			typ:  rec(fld("A", 0, UnknownType)),
			want: "type.field[0]",
		},
		{
			// The INDEX has to be the offending field's, not the first one's —
			// an off-by-one here points a debugger at the wrong column.
			name: "later_field",
			typ: rec(
				fld("A", 0, NotNullLong),
				fld("B", 1, NotNullLong),
				fld("C", 2, UnknownType),
			),
			want: "type.field[2]",
		},
		{
			name: "array_element",
			typ:  NewArrayType(false, UnknownType),
			want: "type.element",
		},
		{
			// Two frames deep: the segments must appear OUTERMOST-FIRST. Built
			// on unwind, each frame prepends, so a reversed accumulation would
			// show up here as `.element.field[1]`.
			name: "field_then_element",
			typ: rec(
				fld("A", 0, NotNullLong),
				fld("B", 1, NewArrayType(false, UnknownType)),
			),
			want: "type.field[1].element",
		},
		{
			name: "relation_inner",
			typ:  &RelationType{InnerType: UnknownType},
			want: "type.inner",
		},
		{
			// Three frames, mixing all three segment kinds.
			name: "relation_field_element",
			typ: &RelationType{InnerType: rec(
				fld("A", 0, NotNullLong),
				fld("B", 1, NotNullLong),
				fld("C", 2, NewArrayType(false, UnknownType)),
			)},
			want: "type.inner.field[2].element",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, code := exactPathOf(t, tc.typ)
			if got != tc.want {
				t.Errorf("error path = %q, want %q", got, tc.want)
			}
			if code != TypeUnresolved {
				t.Errorf("error code = %v, want TypeUnresolved — the fixture must be"+
					" refused for the reason the arm assumes, or the path is about"+
					" a different failure", code)
			}
		})
	}
}

// A cycle is still detected, and detected at the node that closes it.
//
// Cycle detection moved off a map with a deferred delete onto an ancestor
// SLICE passed by value. The by-value pass is what gives the delete for free:
// a sibling branch sees the caller's length. If that were wrong — if the slice
// leaked between siblings — the second arm here would report a cycle that does
// not exist.
func TestExactSnapshotCycleDetection(t *testing.T) {
	t.Parallel()

	t.Run("self_reference_is_a_cycle", func(t *testing.T) {
		t.Parallel()
		cyclic := &RecordType{}
		cyclic.Fields = []Field{{Name: "SELF", Ordinal: 0, FieldType: cyclic}}
		path, code := exactPathOf(t, cyclic)
		if code != TypeCycle {
			t.Fatalf("code = %v, want TypeCycle", code)
		}
		if path != "type.field[0]" {
			t.Errorf("cycle path = %q, want %q", path, "type.field[0]")
		}
	})

	t.Run("repeated_sibling_is_not_a_cycle", func(t *testing.T) {
		t.Parallel()
		// ONE shared leaf reached down two different branches. It is an
		// ancestor of neither, so this must snapshot cleanly. A cycle set that
		// outlived a sibling's recursion would call this a cycle.
		shared := &RecordType{Fields: []Field{{Name: "X", Ordinal: 0, FieldType: NotNullLong}}}
		outer := &RecordType{Fields: []Field{
			{Name: "L", Ordinal: 0, FieldType: shared},
			{Name: "R", Ordinal: 1, FieldType: shared},
		}}
		if _, err := SnapshotExactType(outer); err != nil {
			t.Fatalf("a leaf shared by two SIBLING branches was refused: %v", err)
		}
	})

	t.Run("repeated_deeper_sibling_is_not_a_cycle", func(t *testing.T) {
		t.Parallel()
		// Same shape one level deeper, so the ancestor chain is non-empty when
		// the second branch is walked.
		shared := &RecordType{Fields: []Field{{Name: "X", Ordinal: 0, FieldType: NotNullLong}}}
		mid := &RecordType{Fields: []Field{
			{Name: "L", Ordinal: 0, FieldType: shared},
			{Name: "R", Ordinal: 1, FieldType: NewArrayType(false, shared)},
		}}
		outer := &RecordType{Fields: []Field{{Name: "M", Ordinal: 0, FieldType: mid}}}
		if _, err := SnapshotExactType(outer); err != nil {
			t.Fatalf("a leaf shared across nested sibling branches was refused: %v", err)
		}
	})
}

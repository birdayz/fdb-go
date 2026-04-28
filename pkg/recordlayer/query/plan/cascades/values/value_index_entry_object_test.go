package values

import "testing"

// fakeIndexEntry implements IndexEntryReader for tests — the real
// *recordlayer.IndexEntry can't be imported here (cycle).
type fakeIndexEntry struct {
	key, value []any
}

func (f *fakeIndexEntry) PrimaryKey() any { return f.key }
func (f *fakeIndexEntry) IndexValues() any {
	return f.value
}

func TestIndexEntryObjectValue_Type(t *testing.T) {
	t.Parallel()
	v := NewIndexEntryObjectValue(NamedCorrelationIdentifier("e"), TupleSourceKey, []int{0}, NotNullLong)
	if !v.Type().Equals(NotNullLong) {
		t.Fatalf("Type = %v, want NotNullLong", v.Type())
	}
}

func TestIndexEntryObjectValue_Children(t *testing.T) {
	t.Parallel()
	v := NewIndexEntryObjectValue(NamedCorrelationIdentifier("e"), TupleSourceKey, []int{0}, NotNullLong)
	if got := v.Children(); len(got) != 0 {
		t.Fatalf("Children = %v, want []", got)
	}
}

func TestIndexEntryObjectValue_Name(t *testing.T) {
	t.Parallel()
	v := NewIndexEntryObjectValue(NamedCorrelationIdentifier("e"), TupleSourceKey, []int{0}, NotNullLong)
	if got := v.Name(); got != "indexEntryObject" {
		t.Fatalf("Name = %q, want indexEntryObject", got)
	}
}

func TestIndexEntryObjectValue_EvaluateFromKey(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("e")
	v := NewIndexEntryObjectValue(alias, TupleSourceKey, []int{1}, NotNullLong)
	entry := &fakeIndexEntry{
		key:   []any{int64(10), int64(20), int64(30)},
		value: []any{},
	}
	ctx := map[CorrelationIdentifier]any{alias: entry}
	if got := v.Evaluate(ctx); got != int64(20) {
		t.Fatalf("Evaluate = %v, want 20", got)
	}
}

func TestIndexEntryObjectValue_EvaluateFromValue(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("e")
	v := NewIndexEntryObjectValue(alias, TupleSourceValue, []int{0}, NotNullString)
	entry := &fakeIndexEntry{
		key:   []any{int64(1)},
		value: []any{"payload"},
	}
	ctx := map[CorrelationIdentifier]any{alias: entry}
	if got := v.Evaluate(ctx); got != "payload" {
		t.Fatalf("Evaluate = %v, want 'payload'", got)
	}
}

func TestIndexEntryObjectValue_EvaluateNestedPath(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("e")
	v := NewIndexEntryObjectValue(alias, TupleSourceKey, []int{0, 1}, NotNullLong)
	entry := &fakeIndexEntry{
		key: []any{
			[]any{int64(100), int64(200)}, // nested tuple in KEY
			int64(999),
		},
	}
	ctx := map[CorrelationIdentifier]any{alias: entry}
	if got := v.Evaluate(ctx); got != int64(200) {
		t.Fatalf("Evaluate(nested) = %v, want 200", got)
	}
}

func TestIndexEntryObjectValue_EvaluateOutOfBoundsReturnsNil(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("e")
	v := NewIndexEntryObjectValue(alias, TupleSourceKey, []int{99}, NotNullLong)
	entry := &fakeIndexEntry{key: []any{int64(10)}}
	ctx := map[CorrelationIdentifier]any{alias: entry}
	if got := v.Evaluate(ctx); got != nil {
		t.Fatalf("Evaluate(OOB) = %v, want nil", got)
	}
}

func TestIndexEntryObjectValue_EvaluateNegativePathReturnsNil(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("e")
	v := NewIndexEntryObjectValue(alias, TupleSourceKey, []int{-1}, NotNullLong)
	entry := &fakeIndexEntry{key: []any{int64(10)}}
	ctx := map[CorrelationIdentifier]any{alias: entry}
	if got := v.Evaluate(ctx); got != nil {
		t.Fatalf("Evaluate(-1) = %v, want nil", got)
	}
}

func TestIndexEntryObjectValue_EvaluateMissingAliasReturnsNil(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("e")
	other := NamedCorrelationIdentifier("f")
	v := NewIndexEntryObjectValue(alias, TupleSourceKey, []int{0}, NotNullLong)
	entry := &fakeIndexEntry{key: []any{int64(10)}}
	ctx := map[CorrelationIdentifier]any{other: entry}
	if got := v.Evaluate(ctx); got != nil {
		t.Fatalf("Evaluate(missing alias) = %v, want nil", got)
	}
}

func TestIndexEntryObjectValue_EvaluateNonReaderReturnsNil(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("e")
	v := NewIndexEntryObjectValue(alias, TupleSourceKey, []int{0}, NotNullLong)
	ctx := map[CorrelationIdentifier]any{alias: "not-an-entry"}
	if got := v.Evaluate(ctx); got != nil {
		t.Fatalf("Evaluate(non-reader) = %v, want nil", got)
	}
}

func TestIndexEntryObjectValue_EvaluateNilCtxReturnsNil(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("e")
	v := NewIndexEntryObjectValue(alias, TupleSourceKey, []int{0}, NotNullLong)
	if got := v.Evaluate(nil); got != nil {
		t.Fatalf("Evaluate(nil) = %v, want nil", got)
	}
}

func TestIndexEntryObjectValue_GetCorrelatedToIsEmpty(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("e")
	v := NewIndexEntryObjectValue(alias, TupleSourceKey, []int{0}, NotNullLong)
	if got := v.GetCorrelatedTo(); len(got) != 0 {
		t.Fatalf("GetCorrelatedTo = %v, want empty", got)
	}
}

func TestIndexEntryObjectValue_OrdinalPathIsDefensiveCopy(t *testing.T) {
	t.Parallel()
	original := []int{0, 1, 2}
	alias := NamedCorrelationIdentifier("e")
	v := NewIndexEntryObjectValue(alias, TupleSourceKey, original, NotNullLong)
	// Mutate caller's slice.
	original[0] = 99
	if v.OrdinalPath[0] == 99 {
		t.Fatalf("OrdinalPath aliased caller's slice — not defensively copied")
	}
}

func TestTupleSource_String(t *testing.T) {
	t.Parallel()
	cases := map[TupleSource]string{
		TupleSourceKey:   "KEY",
		TupleSourceValue: "VALUE",
		TupleSourceOther: "OTHER",
		TupleSource(99):  "INVALID",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("TupleSource(%d).String() = %q, want %q", s, got, want)
		}
	}
}

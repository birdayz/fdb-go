package values

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// testIndexEntry is an index entry's two raw tuples; *recordlayer.IndexEntry is
// the production binding (pinned on real scanned entries in recordlayer's
// index_entry_reader_fdb_test.go).
type testIndexEntry struct{ key, value tuple.Tuple }

func (e *testIndexEntry) IndexEntryKey() tuple.Tuple   { return e.key }
func (e *testIndexEntry) IndexEntryValue() tuple.Tuple { return e.value }

type testEntryBindings map[CorrelationIdentifier]any

func (b testEntryBindings) GetCorrelationBinding(id CorrelationIdentifier) (any, bool) {
	v, ok := b[id]
	return v, ok
}

func mustIndexEntryObjectValue(t *testing.T, alias CorrelationIdentifier, source TupleSource, path []int, typ Type) *IndexEntryObjectValue {
	t.Helper()
	v, err := NewIndexEntryObjectValue(alias, source, path, typ)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestIndexEntryObjectValue_ConstructorRefusesStructuredTypes(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("e")
	for _, typ := range []Type{
		nil, UnknownType, NewRecordType("R", true, []Field{{Name: "A", FieldType: NullableLong}}),
		&ArrayType{ElementType: NullableLong, Nullable: true},
	} {
		if _, err := NewIndexEntryObjectValue(alias, TupleSourceKey, []int{0}, typ); err == nil {
			t.Errorf("result type %v was admitted; Java's constructor admits only a primitive, enum or UUID", typ)
		}
	}
	for _, typ := range []Type{NullableLong, NotNullString, NullableBytes, NullableUuid, NullableBoolean} {
		if _, err := NewIndexEntryObjectValue(alias, TupleSourceKey, []int{0}, typ); err != nil {
			t.Errorf("result type %v refused: %v", typ, err)
		}
	}
}

func TestIndexEntryObjectValue_LeafShape(t *testing.T) {
	t.Parallel()
	original := []int{0, 1, 2}
	v := mustIndexEntryObjectValue(t, NamedCorrelationIdentifier("e"), TupleSourceValue, original, NotNullLong)
	original[0] = 99
	if v.OrdinalPath[0] == 99 {
		t.Fatal("OrdinalPath aliases the caller's slice")
	}
	if !v.Type().Equals(NotNullLong) || len(v.Children()) != 0 || len(v.GetCorrelatedTo()) != 0 || v.Name() != "indexEntryObject" {
		t.Fatalf("leaf shape: type %v, children %v, correlated %v, name %q", v.Type(), v.Children(), v.GetCorrelatedTo(), v.Name())
	}
	if got := ExplainValue(v); got != "VALUE:[0, 1, 2]" {
		t.Fatalf("explain = %q, want Java's VALUE:[0, 1, 2]", got)
	}
}

func TestIndexEntryObjectValue_Evaluate(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("e")
	id := tuple.UUID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	stamp := tuple.Versionstamp{TransactionVersion: [10]byte{0, 0, 0, 0, 0, 0, 0, 7, 0, 1}, UserVersion: 0x0203}
	entry := &testIndexEntry{
		key:   tuple.Tuple{int64(10), tuple.Tuple{int64(100), nil, "x"}, id, float32(1.5), stamp, nil},
		value: tuple.Tuple{"payload", []byte{9}},
	}
	bindings := testEntryBindings{alias: entry}
	for _, c := range []struct {
		name   string
		source TupleSource
		path   []int
		typ    Type
		want   any
	}{
		{"key", TupleSourceKey, []int{0}, NullableLong, int64(10)},
		{"value", TupleSourceValue, []int{0}, NullableString, "payload"},
		// Every non-KEY source reads the VALUE tuple (IndexEntryObjectValue.eval).
		{"other_reads_value", TupleSourceOther, []int{1}, NullableBytes, []byte{9}},
		{"nested_tuple", TupleSourceKey, []int{1, 2}, NullableString, "x"},
		{"null_element", TupleSourceKey, []int{5}, NullableLong, nil},
		{"null_midway", TupleSourceKey, []int{5, 3}, NullableLong, nil},
		{"null_nested_element", TupleSourceKey, []int{1, 1}, NullableLong, nil},
		{"uuid_to_row_domain", TupleSourceKey, []int{2}, NullableUuid, [16]byte(id)},
		{"float_widened", TupleSourceKey, []int{3}, NullableFloat, float64(1.5)},
		{
			"versionstamp_to_bytes", TupleSourceKey,
			[]int{4},
			NullableVersion,
			[]byte{0, 0, 0, 0, 0, 0, 0, 7, 0, 1, 2, 3},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			v := mustIndexEntryObjectValue(t, alias, c.source, c.path, c.typ)
			for _, ctx := range []any{bindings, &RowEvalContext{Correlations: bindings}} {
				got, err := v.Evaluate(ctx)
				if err != nil {
					t.Fatalf("Evaluate(%T): %v", ctx, err)
				}
				if !valuesEqualForTest(got, c.want) {
					t.Fatalf("Evaluate(%T) = %#v, want %#v", ctx, got, c.want)
				}
			}
		})
	}
}

func valuesEqualForTest(a, b any) bool {
	ab, aBytes := a.([]byte)
	bb, bBytes := b.([]byte)
	if aBytes || bBytes {
		return aBytes && bBytes && string(ab) == string(bb)
	}
	return a == b
}

// Every way the entry cannot be read is an error: Java's requireNonNull on the
// binding, its IndexEntry cast, and getForOrdinalPath's out-of-bounds and
// non-tuple hops (a NULL there would drop rows).
func TestIndexEntryObjectValue_EvaluateErrors(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("e")
	entry := &testIndexEntry{key: tuple.Tuple{int64(10)}, value: tuple.Tuple{}}
	for _, c := range []struct {
		name string
		path []int
		ctx  any
	}{
		{"nil_context", []int{0}, nil},
		{"context_without_bindings", []int{0}, &RowEvalContext{}},
		{"unsupported_context", []int{0}, map[CorrelationIdentifier]any{alias: entry}},
		{"unbound_alias", []int{0}, testEntryBindings{NamedCorrelationIdentifier("f"): entry}},
		{"not_an_entry", []int{0}, testEntryBindings{alias: "not-an-entry"}},
		{"nil_entry", []int{0}, testEntryBindings{alias: (*testIndexEntry)(nil)}},
		{"out_of_bounds", []int{99}, testEntryBindings{alias: entry}},
		{"negative", []int{-1}, testEntryBindings{alias: entry}},
		{"non_tuple_hop", []int{0, 0}, testEntryBindings{alias: entry}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			v := mustIndexEntryObjectValue(t, alias, TupleSourceKey, c.path, NullableLong)
			if got, err := v.Evaluate(c.ctx); err == nil {
				t.Fatalf("Evaluate = %v with no error", got)
			}
		})
	}
}

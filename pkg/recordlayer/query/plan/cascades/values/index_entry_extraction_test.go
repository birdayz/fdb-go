package values

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/tupleordering"

	"github.com/stretchr/testify/require"
)

func extractionField(t *testing.T, base Value, names ...string) FieldValue {
	t.Helper()
	requests := make([]FieldRequest, len(names))
	for i, name := range names {
		r, err := FieldByName(name)
		require.NoError(t, err)
		requests[i] = r
	}
	v, err := ResolveFieldAccess(base, requests)
	require.NoError(t, err)
	fv, ok := AsFieldValue(v)
	require.True(t, ok, "%v is no FieldValue", v)
	return fv
}

func evalReader(t *testing.T, reader Value, key, value tuple.Tuple) any {
	t.Helper()
	got, err := reader.Evaluate(testEntryBindings{CurrentCorrelation(): &testIndexEntry{key: key, value: value}})
	require.NoError(t, err)
	return got
}

// TestExtractFromIndexEntry_SimpleField: a field of the base is read from its
// entry column (MatchSimpleFieldValueRule).
func TestExtractFromIndexEntry_SimpleField(t *testing.T) {
	t.Parallel()
	base := s3NestedQOV(t)
	w := extractionField(t, base, "W")
	field, reader, ok := ExtractFromIndexEntry(w, base.Correlation(), TupleSourceValue, []int{1})
	require.True(t, ok)
	require.Same(t, w, field)
	require.Equal(t, "VALUE:[1]", ExplainValue(reader))
	require.Equal(t, int64(9), evalReader(t, reader, tuple.Tuple{"k"}, tuple.Tuple{int64(8), int64(9)}))
}

// TestExtractFromIndexEntry_Declines: only fields of the base are extracted,
// and only into a type an entry leaf carries.
func TestExtractFromIndexEntry_Declines(t *testing.T) {
	t.Parallel()
	base := s3NestedQOV(t)
	other := mustQOV(t, NamedCorrelationIdentifier("other"), base.Type())
	for name, v := range map[string]Value{
		"other_quantifier":  extractionField(t, other, "W"),
		"constant":          LiteralValue(int64(3)),
		"ordered_constant":  NewToOrderedBytesValue(LiteralValue(int64(3)), OrderedBytesDescNullsLast),
		"record_typed_leaf": extractionField(t, base, "NESTED"),
		"bare_quantifier":   base,
	} {
		_, _, ok := ExtractFromIndexEntry(v, base.Correlation(), TupleSourceKey, []int{0})
		require.False(t, ok, name)
	}
}

// TestExtractFromIndexEntry_OrderedBytes: an order-function column covers its
// argument field, read back through FromOrderedBytes
// (CompensateToOrderedBytesValueRule).
func TestExtractFromIndexEntry_OrderedBytes(t *testing.T) {
	t.Parallel()
	base := s3NestedQOV(t)
	w := extractionField(t, base, "W")
	field, reader, ok := ExtractFromIndexEntry(NewToOrderedBytesValue(w, OrderedBytesDescNullsLast),
		base.Correlation(), TupleSourceKey, []int{0})
	require.True(t, ok)
	require.Same(t, w, field)
	from, isFrom := reader.(*FromOrderedBytesValue)
	require.True(t, isFrom, "reader %T", reader)
	require.Equal(t, OrderedBytesDescNullsLast, from.Direction)
	packed := tupleordering.Pack(tuple.Tuple{int64(-5)}, tupleordering.DescNullsLast)
	require.Equal(t, int64(-5), evalReader(t, reader, tuple.Tuple{packed, int64(1)}, nil))
}

// TestExtractFromIndexEntry_NestedField: a nested field is one path on the
// base, so it is extracted whole; a chain of FieldValues is not an admitted
// FieldValue and is not extracted.
func TestExtractFromIndexEntry_NestedField(t *testing.T) {
	t.Parallel()
	base := s3NestedQOV(t)
	y := extractionField(t, base, "NESTED", "Y")
	field, _, ok := ExtractFromIndexEntry(y, base.Correlation(), TupleSourceKey, []int{0})
	require.True(t, ok)
	require.Equal(t, []int{0, 1}, field.Path().Ordinals())

	inner, outer := bakedChain(t)
	chainBase, ok := AsQuantifiedObjectValue(inner.Child)
	require.True(t, ok)
	_, _, ok = ExtractFromIndexEntry(outer, chainBase.Correlation(), TupleSourceKey, []int{0})
	require.False(t, ok)
}

// TestIndexEntryRecordReader_BuildsTheRecord: covered fields, nested ones
// included, are read from the entry in the record's field order; uncovered
// ones are absent; the first source to cover a field wins.
func TestIndexEntryRecordReader_BuildsTheRecord(t *testing.T) {
	t.Parallel()
	base := s3NestedQOV(t)
	reader := &IndexEntryRecordReader{}
	y := extractionField(t, base, "NESTED", "Y")
	_, yReader, ok := ExtractFromIndexEntry(y, base.Correlation(), TupleSourceKey, []int{0})
	require.True(t, ok)
	require.True(t, reader.CoverField(y, yReader))
	_, yAgain, ok := ExtractFromIndexEntry(y, base.Correlation(), TupleSourceValue, []int{0})
	require.True(t, ok)
	require.True(t, reader.CoverField(y, yAgain))
	require.True(t, reader.Covers())

	record := reader.ToRecordValue(base.Type().(*RecordType))
	got := evalReader(t, record, tuple.Tuple{int64(7)}, tuple.Tuple{int64(99)})
	require.Equal(t, map[string]any{"NESTED": map[string]any{"X": nil, "Y": int64(7)}, "W": nil}, got)
}

// TestIndexEntryRecordReader_AbsentArrays: an uncovered non-null array is
// empty, a nullable one NULL (IndexEntryToRecordValueHelper.absent).
func TestIndexEntryRecordReader_AbsentArrays(t *testing.T) {
	t.Parallel()
	target := NewRecordType("", false, []Field{
		{Name: "A", FieldType: &ArrayType{ElementType: NotNullLong}, Ordinal: 0},
		{Name: "B", FieldType: &ArrayType{ElementType: NotNullLong, Nullable: true}, Ordinal: 1},
	})
	record := (&IndexEntryRecordReader{}).ToRecordValue(target)
	got, err := record.Evaluate(testEntryBindings{})
	require.NoError(t, err)
	m := got.(map[string]any)
	require.Empty(t, m["A"])
	require.NotNil(t, m["A"])
	require.Nil(t, m["B"])
}

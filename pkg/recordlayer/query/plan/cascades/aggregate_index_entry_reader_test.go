package cascades

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// aggregateReaderTarget is (g1, g2, agg), Java's TARGET_TYPE.
func aggregateReaderTarget() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "G1", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "G2", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "AGG", FieldType: values.NullableLong, Ordinal: 2},
	})
}

type aggregateReaderEntry struct{ key, value tuple.Tuple }

func (e aggregateReaderEntry) IndexEntryKey() tuple.Tuple   { return e.key }
func (e aggregateReaderEntry) IndexEntryValue() tuple.Tuple { return e.value }

type aggregateReaderBindings struct{ entry aggregateReaderEntry }

func (b aggregateReaderBindings) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	return b.entry, id == values.CurrentCorrelation()
}

func readAggregateEntry(t *testing.T, reader *values.RecordConstructorValue, key, value tuple.Tuple) []any {
	t.Helper()
	out := make([]any, len(reader.Fields))
	for i, field := range reader.Fields {
		v, err := field.Value.Evaluate(aggregateReaderBindings{aggregateReaderEntry{key, value}})
		if err != nil {
			t.Fatalf("field %s: %v", field.Name, err)
		}
		out[i] = v
	}
	return out
}

// TestAggregateIndexEntryToRecordValue ports AggregateIndexEntryToRecordValueTest:
// an ordinary entry reads grouping from KEY and the aggregate from VALUE, a
// permuted one reads every column from KEY with its trailing grouping columns
// behind the aggregate, and a permuted index of size zero still keeps the
// aggregate in the key.
func TestAggregateIndexEntryToRecordValue(t *testing.T) {
	t.Parallel()
	want := []any{int64(10), int64(20), int64(100)}
	for name, tc := range map[string]struct {
		permuted      bool
		permutedCount int
		key, value    tuple.Tuple
		sources       []string
	}{
		"ordinary":      {false, 0, tuple.Tuple{int64(10), int64(20)}, tuple.Tuple{int64(100)}, []string{"KEY:[0]", "KEY:[1]", "VALUE:[0]"}},
		"permuted":      {true, 1, tuple.Tuple{int64(10), int64(100), int64(20)}, tuple.Tuple{}, []string{"KEY:[0]", "KEY:[2]", "KEY:[1]"}},
		"permuted_zero": {true, 0, tuple.Tuple{int64(10), int64(20), int64(100)}, tuple.Tuple{}, []string{"KEY:[0]", "KEY:[1]", "KEY:[2]"}},
	} {
		reader, ok := aggregateIndexEntryToRecordValue(aggregateReaderTarget(), 2, 1, tc.permuted, tc.permutedCount)
		if !ok {
			t.Fatalf("%s: no reader", name)
		}
		for i, field := range reader.Fields {
			if got := values.ExplainValue(field.Value); got != tc.sources[i] {
				t.Errorf("%s: field %d reads %s, want %s", name, i, got, tc.sources[i])
			}
		}
		got := readAggregateEntry(t, reader, tc.key, tc.value)
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: record = %v, want %v", name, got, want)
				break
			}
		}
	}
}

// TestAggregateIndexEntryToRecordValue_RefusesAnUnreadableColumn: a column no
// entry leaf can carry has no reader, and the plan keeps its cursor's own
// decoding (Java's entryColumn throws there).
func TestAggregateIndexEntryToRecordValue_RefusesAnUnreadableColumn(t *testing.T) {
	t.Parallel()
	target := values.NewRecordType("", false, []values.Field{
		{Name: "G", FieldType: values.NewArrayType(true, values.NotNullLong), Ordinal: 0},
		{Name: "AGG", FieldType: values.NullableLong, Ordinal: 1},
	})
	if _, ok := aggregateIndexEntryToRecordValue(target, 1, 1, false, 0); ok {
		t.Fatal("an array grouping column was given a reader")
	}
}

package cascades

import "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

// aggregateIndexEntryToRecordValue is
// AggregateIndexMatchCandidate.indexEntryToRecordValue: one column per field
// of target, each reading the entry position the field was written to. For n
// grouping and m aggregate columns an ordinary index stores KEY(g1..gn)
// VALUE(agg1..aggm); a permuted one KEY(g1..g(n-p), agg1..aggm,
// g(n-p+1)..gn) VALUE() with p = permutedCount. Whether an index is permuted
// is its type: at p = 0 the aggregate is still in the key.
func aggregateIndexEntryToRecordValue(
	target *values.RecordType, groupingCount, groupedCount int, permuted bool, permutedCount int,
) (*values.RecordConstructorValue, bool) {
	if target == nil || len(target.Fields) != groupingCount+groupedCount ||
		permutedCount < 0 || permutedCount > groupingCount {
		return nil, false
	}
	fields := make([]values.RecordConstructorField, len(target.Fields))
	for i, field := range target.Fields {
		source, ordinal := values.TupleSourceKey, i
		switch {
		case permuted && i >= groupingCount:
			ordinal = i - permutedCount
		case permuted && i >= groupingCount-permutedCount:
			ordinal = i + groupedCount
		case !permuted && i >= groupingCount:
			source, ordinal = values.TupleSourceValue, i-groupingCount
		}
		leaf, err := values.NewIndexEntryObjectValue(values.CurrentCorrelation(), source, []int{ordinal}, field.FieldType)
		if err != nil {
			return nil, false
		}
		fields[i] = values.RecordConstructorField{Name: field.Name, Value: leaf}
	}
	return values.NewRecordConstructorValue(fields...), true
}

// indexEntryToRecordValue is the candidate's reader for an entry read into
// resultType, its grouping columns then its aggregate.
func (c *AggregateIndexMatchCandidate) indexEntryToRecordValue(resultType values.Type) *values.RecordConstructorValue {
	target, ok := resultType.(*values.RecordType)
	if !ok {
		return nil
	}
	groupingCount := len(c.groupCols)
	permutedCount := 0
	if c.permuted {
		permutedCount = groupingCount - c.physicalGroupingPrefixCount
	}
	reader, ok := aggregateIndexEntryToRecordValue(target, groupingCount, len(target.Fields)-groupingCount, c.permuted, permutedCount)
	if !ok {
		return nil
	}
	return reader
}

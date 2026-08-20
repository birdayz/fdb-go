package recordlayer

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// SQL MIN ignores NULLs; the PERMUTED_MIN index does not.
//
// The permuted subspace holds ONE entry per group, carrying the extremum over
// that group's index entries — and a record whose aggregated column is NULL
// produces an entry like any other, whose value component is NULL. NULL sorts
// before every value in tuple order, so for a MIN index it wins the comparison
// unconditionally: a group holding a single NULL stores NULL as its extremum
// however many real values sit beside it, and reads back as NULL.
//
// The repair is on the READ side, deliberately. The stored bytes are what Java
// writes — its PermutedMinMaxIndexMaintainer.updateIndexKeys has no NULL filter
// either, and getExtremum takes the first entry of the group scan — so changing
// what Go stores would break the property this port exists to hold: that Go and
// Java can share a cluster and read each other's indexes. Repairing at read time
// leaves every byte identical and makes only the ANSWER differ, which is the
// axis where being right matters more than matching.
//
// JAVA ANSWERS THIS WRONGLY TOO, and that is measured rather than inferred from
// the maintainer's shape: over a group holding {5, NULL, 9}, Java's grouped MIN
// returns NULL where Go now returns 5, while both engines agree exactly on MAX
// over the same fixture. So this is an upstream defect that Go is deliberately
// ahead of — DivergenceJavaWrongRowsGoCorrect — rather than a Go-side gap being
// closed. The measurement lives in
// conformance/permuted_min_null_group_java_probe_test.go and FAILS if Java ever
// starts agreeing, at which point this paragraph is what needs rewriting.
//
// MAX is untouched and must stay so. NULL sorts LOWEST, so a real value always
// beats it for MAX; a stored NULL extremum there means the group genuinely holds
// no non-NULL value, and NULL is already the correct answer.
//
// The cost is one range read per group whose stored extremum is NULL — that is,
// only for groups that actually contain a NULL — and the read is bounded to a
// single entry, seeking directly past the NULL run rather than scanning it.

// OrdinaryIndexScanFunc scans the ORDINARY (value) subspace of an index. The
// repair is expressed against this function rather than against a store because
// its two callers reach that subspace differently: the query executor goes
// through FDBRecordStore.ScanIndexByType, while the index maintainer already
// holds a standard-maintainer handle on the same subspace and has no store.
type OrdinaryIndexScanFunc func(scanRange TupleRange, props ScanProperties) RecordCursor[*IndexEntry]

// PermutedMinIgnoringNulls returns the smallest NON-NULL value of one group of a
// PERMUTED_MIN index, or nil when the group holds no non-NULL value at all (in
// which case SQL MIN is genuinely NULL).
//
// It reads the index's ORDINARY subspace, whose entries are
// (grouping..., grouped..., primaryKey...) in key order. Every NULL-valued entry
// of the group shares the prefix (groupKey..., NULL), so an exclusive low
// endpoint on that prefix seeks past the whole NULL run in one operation — the
// answer is then the first entry returned, whatever the group's NULL count.
//
// groupKey is the group in ORIGINAL column order (not the permuted order the
// secondary subspace stores). valueStart/valueEnd bound the aggregated columns
// within an ordinary entry's key.
func PermutedMinIgnoringNulls(
	ctx context.Context,
	scan OrdinaryIndexScanFunc,
	indexName string,
	groupKey tuple.Tuple,
	valueStart, valueEnd int,
	callerProps ExecuteProperties,
) (tuple.Tuple, error) {
	if scan == nil {
		return nil, fmt.Errorf("permuted MIN null repair on index %q: no scan function", indexName)
	}
	if valueStart < 0 || valueEnd <= valueStart {
		return nil, fmt.Errorf("permuted MIN null repair on index %q: invalid value span [%d,%d)",
			indexName, valueStart, valueEnd)
	}

	// Low is (groupKey..., NULL) EXCLUSIVE, which resolves to Strinc of the
	// packed prefix — the first key that cannot be a NULL-valued entry of this
	// group. Every non-NULL tuple type code is greater than NULL's 0x00, so no
	// real value is skipped by starting there.
	low := make(tuple.Tuple, 0, len(groupKey)+1)
	low = append(low, groupKey...)
	low = append(low, nil)
	scanRange := TupleRange{
		Low:          low,
		LowEndpoint:  EndpointTypeRangeExclusive,
		High:         groupKey,
		HighEndpoint: EndpointTypeRangeInclusive,
	}

	// The caller's properties are threaded through rather than rebuilt, so the
	// repair's read is CHARGED to the same budget the query is running under.
	//
	// A freshly-built ExecuteProperties carries no ScanState, and no ScanState
	// means no limit: the repair's extra read per NULL-extremum group would be
	// invisible to the statement's record, byte and time budgets. Thirty mixed
	// groups would then complete sixty index reads under a forty-record fail
	// limit — a resource ceiling silently exceeded, and exceeded by an amount
	// that scales with the DATA rather than with anything the caller wrote.
	//
	// Two fields are overridden rather than inherited, and each for its own
	// reason:
	//
	//	ReturnedRowLimit  the answer is ONE entry — the ordinary subspace is
	//	                  ordered by value within the group, so the first
	//	                  non-NULL entry carries the minimum. Inheriting the
	//	                  caller's limit would read more than needed.
	//	Skip              an inherited OFFSET would skip that one entry and the
	//	                  repair would report "no non-NULL value in the group",
	//	                  turning a paging offset into a wrong ANSWER.
	props := ScanProperties{ExecuteProperties: callerProps}
	props.ExecuteProperties.ReturnedRowLimit = 1
	props.ExecuteProperties.Skip = 0
	cursor := scan(scanRange, props)
	defer func() { _ = cursor.Close() }()

	result, err := cursor.OnNext(ctx)
	if err != nil {
		return nil, fmt.Errorf("permuted MIN null repair on index %q: %w", indexName, err)
	}
	if !result.HasNext() {
		// No non-NULL value in the group: SQL MIN is NULL, which is what the
		// stored extremum already said.
		return nil, nil
	}
	key := result.GetValue().Key
	if valueEnd > len(key) {
		return nil, fmt.Errorf(
			"permuted MIN null repair on index %q: entry key has %d columns, value span is [%d,%d)",
			indexName, len(key), valueStart, valueEnd)
	}
	return tuple.Tuple(key[valueStart:valueEnd]), nil
}

// OrdinaryIndexScanner returns the scan function the repair needs for an index
// reached through a record store — the query executor's route.
func OrdinaryIndexScanner(store *FDBRecordStore, index *Index) OrdinaryIndexScanFunc {
	if store == nil || index == nil {
		return nil
	}
	return func(scanRange TupleRange, props ScanProperties) RecordCursor[*IndexEntry] {
		return store.ScanIndexByType(index, IndexScanByValue, scanRange, nil, props)
	}
}

// IsPermutedMinIndex reports whether idx is the MIN flavour of the permuted
// index, the only one whose stored extremum can be a NULL that hides real
// values. Callers use it to keep the repair off the MAX flavour, where a stored
// NULL is the correct answer.
func IsPermutedMinIndex(idx *Index) bool {
	return idx != nil && idx.Type == IndexTypePermutedMin
}

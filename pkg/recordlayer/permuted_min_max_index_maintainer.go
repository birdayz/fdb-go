package recordlayer

import (
	"context"
	"fmt"
	"strconv"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// IndexOptionPermutedSize specifies how many trailing grouping fields are permuted
// to after the value in the secondary index subspace.
// Matches Java's IndexOptions.PERMUTED_SIZE_OPTION.
const IndexOptionPermutedSize = "permutedSize"

// permutedMinMaxIndexMaintainer maintains a PERMUTED_MIN or PERMUTED_MAX index.
//
// It extends the standard VALUE index with a secondary (permuted) subspace that
// stores one entry per group with the grouping suffix columns moved after the value.
// This allows enumerating extrema ordered by value, not by group.
//
// Primary subspace (IndexKey=2): standard VALUE index entries [group..., value..., pk...]
// Secondary subspace (IndexSecondarySpaceKey=3): permuted entries [groupPrefix, value, groupSuffix]
//
// Matches Java's PermutedMinMaxIndexMaintainer.
type permutedMinMaxIndexMaintainer struct {
	*StandardIndexMaintainer
	isMax             bool
	permutedSize      int
	secondarySubspace subspace.Subspace
}

func newPermutedMinMaxIndexMaintainer(
	index *Index,
	indexSubspace subspace.Subspace,
	secondarySubspace subspace.Subspace,
	tx fdb.Transaction,
	store indexStoreContext,
	isMax bool,
) *permutedMinMaxIndexMaintainer {
	permutedSize := 0
	if v, ok := index.Options[IndexOptionPermutedSize]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			permutedSize = n
		}
	}
	return &permutedMinMaxIndexMaintainer{
		StandardIndexMaintainer: newStandardIndexMaintainer(index, indexSubspace, tx, store),
		isMax:                   isMax,
		permutedSize:            permutedSize,
		secondarySubspace:       secondarySubspace,
	}
}

// getGroupingCount returns the number of leading grouping (GROUP BY) columns.
func (m *permutedMinMaxIndexMaintainer) getGroupingCount() int {
	if g, ok := m.index.RootExpression.(*GroupingKeyExpression); ok {
		return g.GetGroupingCount()
	}
	return keyExpressionColumnSize(m.index.RootExpression)
}

// shouldUpdateExtremum returns true if newValue should replace oldValue.
// For MAX: newValue > oldValue. For MIN: newValue < oldValue.
// Uses tuple byte comparison (same as FDB ordering).
func (m *permutedMinMaxIndexMaintainer) shouldUpdateExtremum(oldValue, newValue tuple.Tuple) bool {
	cmp := compareTuples(oldValue, newValue)
	if m.isMax {
		return cmp < 0 // old < new means new is bigger → replace old with new
	}
	return cmp > 0 // old > new means new is smaller → replace old with new
}

// compareTuples compares two tuples by their packed byte representation.
// Returns negative if a < b, positive if a > b, 0 if equal.
func compareTuples(a, b tuple.Tuple) int {
	ap := a.Pack()
	bp := b.Pack()
	for i := 0; i < len(ap) && i < len(bp); i++ {
		if ap[i] < bp[i] {
			return -1
		}
		if ap[i] > bp[i] {
			return 1
		}
	}
	if len(ap) < len(bp) {
		return -1
	}
	if len(ap) > len(bp) {
		return 1
	}
	return 0
}

// Update handles insert/delete/update with permuted subspace maintenance.
// Matches Java's PermutedMinMaxIndexMaintainer.updateIndexKeys().
func (m *permutedMinMaxIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	groupPrefixSize := m.getGroupingCount()
	totalSize := keyExpressionColumnSize(m.index.RootExpression)
	permutePosition := groupPrefixSize - m.permutedSize

	if oldRecord != nil && newRecord == nil {
		// DELETE path: first update primary, then fix permuted subspace.
		if err := m.StandardIndexMaintainer.Update(oldRecord, nil); err != nil {
			return err
		}

		oldEntries, err := m.evaluateIndex(oldRecord)
		if err != nil {
			return fmt.Errorf("evaluate index %q for old record (permuted delete): %w", m.index.Name, err)
		}

		entryPerGroup := m.extremumEntriesByGroup(oldEntries, groupPrefixSize, totalSize)
		for _, entry := range entryPerGroup {
			groupKey := entry.key[:groupPrefixSize]
			value := entry.key[groupPrefixSize:totalSize]
			groupPrefix := groupKey[:permutePosition]
			groupSuffix := groupKey[permutePosition:groupPrefixSize]

			permutedKey := m.buildPermutedKey(groupPrefix, value, groupSuffix)
			permutedKeyBytes := m.secondarySubspace.Pack(permutedKey)

			// Check if the deleted value is in the permuted subspace.
			existing := m.tx.Get(fdb.Key(permutedKeyBytes)).MustGet()
			if existing == nil {
				continue // Not the current extremum, nothing to do.
			}

			// Get the new extremum from the primary index.
			extremum, err := m.getExtremum(groupKey)
			if err != nil {
				return fmt.Errorf("get extremum for permuted delete: %w", err)
			}

			if extremum == nil {
				// No replacement, just remove.
				m.tx.Clear(fdb.Key(permutedKeyBytes))
			} else {
				remainingValue := tuple.Tuple(extremum[groupPrefixSize:totalSize])
				if !tuplesEqual(value, remainingValue) {
					newPermutedKey := m.buildPermutedKey(groupPrefix, remainingValue, groupSuffix)
					newPermutedKeyBytes := m.secondarySubspace.Pack(newPermutedKey)
					m.tx.Clear(fdb.Key(permutedKeyBytes))
					m.tx.Set(fdb.Key(newPermutedKeyBytes), tuple.Tuple{}.Pack())
				}
			}
		}
		return nil
	}

	if newRecord != nil {
		// INSERT or UPDATE path: update permuted subspace first, then primary.
		newEntries, err := m.evaluateIndex(newRecord)
		if err != nil {
			return fmt.Errorf("evaluate index %q for new record (permuted insert): %w", m.index.Name, err)
		}

		entryPerGroup := m.extremumEntriesByGroup(newEntries, groupPrefixSize, totalSize)
		for _, entry := range entryPerGroup {
			groupKey := entry.key[:groupPrefixSize]
			value := entry.key[groupPrefixSize:totalSize]
			groupPrefix := groupKey[:permutePosition]
			groupSuffix := groupKey[permutePosition:groupPrefixSize]

			extremum, err := m.getExtremum(groupKey)
			if err != nil {
				return fmt.Errorf("get extremum for permuted insert: %w", err)
			}

			addPermuted := false
			if extremum == nil {
				addPermuted = true // New group.
			} else {
				currentValue := tuple.Tuple(extremum[groupPrefixSize:totalSize])
				if m.shouldUpdateExtremum(currentValue, value) {
					addPermuted = true
					// Remove old permuted entry.
					oldPermutedKey := m.buildPermutedKey(groupPrefix, currentValue, groupSuffix)
					m.tx.Clear(fdb.Key(m.secondarySubspace.Pack(oldPermutedKey)))
				}
			}

			if addPermuted {
				newPermutedKey := m.buildPermutedKey(groupPrefix, value, groupSuffix)
				m.tx.Set(fdb.Key(m.secondarySubspace.Pack(newPermutedKey)), tuple.Tuple{}.Pack())
			}
		}

		// Now update the primary VALUE index.
		if oldRecord != nil {
			return m.StandardIndexMaintainer.Update(oldRecord, newRecord)
		}
		return m.StandardIndexMaintainer.Update(nil, newRecord)
	}

	return nil
}

// Scan scans the primary (standard VALUE) index subspace.
// This is the default BY_VALUE scan. For BY_GROUP, use ScanByGroup.
// Matches Java's PermutedMinMaxIndexMaintainer.scan() for BY_VALUE.
func (m *permutedMinMaxIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return m.StandardIndexMaintainer.Scan(scanRange, continuation, scanProperties)
}

// ScanByGroup scans the secondary (permuted) subspace.
// Returns entries with permuted key order: [groupPrefix, value, groupSuffix].
// Matches Java's PermutedMinMaxIndexMaintainer.scan() for BY_GROUP.
func (m *permutedMinMaxIndexMaintainer) ScanByGroup(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return newIndexCursor(m.index, m.secondarySubspace, m.tx, scanRange, continuation, scanProperties)
}

// DeleteWhere clears both primary and secondary subspaces.
// Matches Java's PermutedMinMaxIndexMaintainer.deleteWhere().
func (m *permutedMinMaxIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	if err := m.StandardIndexMaintainer.DeleteWhere(prefix); err != nil {
		return err
	}
	return deleteWhereRange(m.tx, m.secondarySubspace, prefix)
}

// getExtremum finds the current min/max entry for a given group key by scanning
// the primary index with limit 1. For MAX, scans reverse (last = largest).
// For MIN, scans forward (first = smallest).
// Returns the full index entry key or nil if no entries exist.
// Matches Java's PermutedMinMaxIndexMaintainer.getExtremum().
func (m *permutedMinMaxIndexMaintainer) getExtremum(groupKey tuple.Tuple) (tuple.Tuple, error) {
	scanRange := TupleRangeAllOf(groupKey)
	props := ScanProperties{
		ExecuteProperties: ExecuteProperties{
			ReturnedRowLimit: 1,
		},
		Reverse: m.isMax,
	}

	cursor := m.StandardIndexMaintainer.Scan(scanRange, nil, props)
	defer func() { _ = cursor.Close() }()

	result, err := cursor.OnNext(context.Background())
	if err != nil {
		return nil, err
	}
	if !result.HasNext() {
		return nil, nil
	}
	return result.GetValue().Key, nil
}

// extremumEntriesByGroup groups index entries by their grouping key prefix and
// keeps only the extremum (min or max) entry per group. This handles fan-out
// cases where a single record produces multiple entries for different groups.
// Matches Java's PermutedMinMaxIndexMaintainer.extremumEntriesByGroup().
func (m *permutedMinMaxIndexMaintainer) extremumEntriesByGroup(
	entries []indexEntry,
	groupPrefixSize, totalSize int,
) map[string]indexEntry {
	result := make(map[string]indexEntry, len(entries))
	for _, entry := range entries {
		groupKey := entry.key[:groupPrefixSize]
		groupKeyStr := string(groupKey.Pack())
		existing, ok := result[groupKeyStr]
		if !ok {
			result[groupKeyStr] = entry
		} else {
			existingValue := existing.key[groupPrefixSize:totalSize]
			entryValue := entry.key[groupPrefixSize:totalSize]
			if m.shouldUpdateExtremum(existingValue, entryValue) {
				result[groupKeyStr] = entry
			}
		}
	}
	return result
}

// buildPermutedKey constructs the permuted tuple: [groupPrefix..., value..., groupSuffix...].
func (m *permutedMinMaxIndexMaintainer) buildPermutedKey(groupPrefix, value, groupSuffix tuple.Tuple) tuple.Tuple {
	result := make(tuple.Tuple, 0, len(groupPrefix)+len(value)+len(groupSuffix))
	result = append(result, groupPrefix...)
	result = append(result, value...)
	result = append(result, groupSuffix...)
	return result
}

// evaluatePermutedMinMaxAggregate evaluates MIN or MAX aggregate via the permuted subspace.
// Scans BY_GROUP with the (possibly trimmed) range, filters entries that match the full
// range, and reduces to find the overall extremum value.
// Matches Java's PermutedMinMaxIndexMaintainer.evaluateAggregateFunction().
func evaluatePermutedMinMaxAggregate(
	ctx context.Context,
	fn *IndexAggregateFunction,
	m *permutedMinMaxIndexMaintainer,
	scanRange TupleRange,
	isolationLevel IsolationLevel,
) (tuple.Tuple, error) {
	groupPrefixSize := m.getGroupingCount()
	totalSize := keyExpressionColumnSize(m.index.RootExpression)
	valueStart := groupPrefixSize - m.permutedSize
	valueEnd := totalSize - m.permutedSize

	props := ScanProperties{
		ExecuteProperties: ExecuteProperties{
			IsolationLevel: isolationLevel,
		},
	}

	// Trim range to unpermuted prefix (permuted columns can't be range-filtered directly).
	unpermutedRange := trimToUnpermutedPrefix(scanRange, groupPrefixSize-m.permutedSize)
	cursor := m.ScanByGroup(unpermutedRange, nil, props)
	defer func() { _ = cursor.Close() }()

	needsFilter := !tupleRangesEqual(unpermutedRange, scanRange)

	var result tuple.Tuple
	for {
		r, err := cursor.OnNext(ctx)
		if err != nil {
			return nil, fmt.Errorf("evaluate permuted aggregate: %w", err)
		}
		if !r.HasNext() {
			break
		}

		entry := r.GetValue()
		if needsFilter {
			// Reconstruct unpermuted group key for range check.
			groupPrefix := entry.Key[:valueStart]
			groupSuffix := entry.Key[valueEnd:]
			group := make(tuple.Tuple, 0, len(groupPrefix)+len(groupSuffix))
			group = append(group, groupPrefix...)
			group = append(group, groupSuffix...)
			if !scanRange.Contains(group) {
				continue
			}
		}

		value := tuple.Tuple(entry.Key[valueStart:valueEnd])
		if result == nil {
			result = value
		} else if m.shouldUpdateExtremum(result, value) {
			result = value
		}
	}

	return result, nil
}

// trimToUnpermutedPrefix truncates a TupleRange to only the unpermuted prefix size.
// Matches Java's PermutedMinMaxIndexMaintainer.trimToUnpermutedPrefix().
func trimToUnpermutedPrefix(r TupleRange, unpermutedSize int) TupleRange {
	low := r.Low
	lowEP := r.LowEndpoint
	if lowEP != EndpointTypeTreeStart && low != nil && len(low) > unpermutedSize {
		low = low[:unpermutedSize]
		lowEP = EndpointTypeRangeInclusive
	}
	high := r.High
	highEP := r.HighEndpoint
	if highEP != EndpointTypeTreeEnd && high != nil && len(high) > unpermutedSize {
		high = high[:unpermutedSize]
		highEP = EndpointTypeRangeInclusive
	}
	return TupleRange{Low: low, High: high, LowEndpoint: lowEP, HighEndpoint: highEP}
}

// tupleRangesEqual returns true if two TupleRanges are identical.
func tupleRangesEqual(a, b TupleRange) bool {
	if a.LowEndpoint != b.LowEndpoint || a.HighEndpoint != b.HighEndpoint {
		return false
	}
	if !tuplesEqual(a.Low, b.Low) || !tuplesEqual(a.High, b.High) {
		return false
	}
	return true
}

// Contains checks if a tuple falls within the range.
func (r TupleRange) Contains(t tuple.Tuple) bool {
	if r.Low != nil {
		cmp := compareTuples(t, r.Low)
		switch r.LowEndpoint {
		case EndpointTypeRangeInclusive:
			if cmp < 0 {
				return false
			}
		case EndpointTypeRangeExclusive:
			if cmp <= 0 {
				return false
			}
		}
	}
	if r.High != nil {
		cmp := compareTuples(t, r.High)
		switch r.HighEndpoint {
		case EndpointTypeRangeInclusive:
			if cmp > 0 {
				return false
			}
		case EndpointTypeRangeExclusive:
			if cmp >= 0 {
				return false
			}
		}
	}
	return true
}

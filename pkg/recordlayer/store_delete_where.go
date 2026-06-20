package recordlayer

import (
	"fmt"
	"slices"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// DeleteRecordsWhere deletes all records whose primary key starts with the
// given prefix, along with all associated index entries, record versions,
// and record counts. This is a pure range-clear operation — no scanning.
//
// The prefix must align with every active index's key expression so that
// index entries can be cleared via range operations. Type-specific indexes
// for matching types are cleared entirely. Universal indexes must have
// leading key expression columns that match the PK columns covered by
// the prefix.
//
// Matches Java's FDBRecordStore.deleteRecordsWhereAsync().
func (store *FDBRecordStore) DeleteRecordsWhere(prefix tuple.Tuple) error {
	if len(prefix) == 0 {
		return fmt.Errorf("deleteRecordsWhere: prefix must be non-empty")
	}
	if err := store.validateRecordUpdateAllowed(); err != nil {
		return err
	}

	// Hold read lock for entire operation — matches Java's beginRead()/endRead()
	// wrapping RecordsWhereDeleter.run().
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()

	tx := store.context.Transaction()

	// Determine which record types match this prefix.
	// A record type matches if the prefix length <= its PK column count.
	matchingTypeNames := store.findMatchingRecordTypes(prefix)
	if len(matchingTypeNames) == 0 {
		return fmt.Errorf("deleteRecordsWhere: prefix length %d exceeds all record type PK sizes", len(prefix))
	}

	// Validate all active indexes and compute their delete prefixes.
	type indexAction struct {
		index  *Index
		prefix tuple.Tuple
	}
	var actions []indexAction

	for _, idx := range store.metaData.GetAllIndexes() {
		if store.getIndexStateLocked(idx.Name).IsDisabled() {
			continue
		}

		// Determine which record types this index covers.
		indexTypeNames := store.recordTypesForIndex(idx)
		isUniversal := len(indexTypeNames) == 0

		if !isUniversal {
			// Type-specific index: check if it covers any matching types.
			coversMatching := false
			for _, itn := range indexTypeNames {
				if slices.Contains(matchingTypeNames, itn) {
					coversMatching = true
					break
				}
			}
			if !coversMatching {
				continue // Index doesn't cover any types being deleted, skip.
			}

			if len(indexTypeNames) > 1 && !hasRecordTypeKeyPrefix(idx.RootExpression) {
				// Multi-type index without RecordTypeKey prefix: can't scope
				// the clear to a single type. Matches Java's
				// canDeleteWhereForIndexOnStoredTypes which throws
				// "Index X applies to more record types than just Y".
				return fmt.Errorf("deleteRecordsWhere: index %q applies to more record types than just the target; "+
					"add RecordTypeKey() prefix to enable scoped delete", idx.Name)
			}

			if len(indexTypeNames) > 1 {
				// Multi-type index with RecordTypeKey prefix: scope the clear
				// to entries for the matching type(s) using the PK prefix.
				// Matches Java's hasRecordTypePrefix branch in
				// canDeleteWhereForIndexOnStoredTypes.
				idxPrefix, ok := computeIndexDeletePrefix(idx, prefix, store.metaData, matchingTypeNames)
				if !ok {
					return fmt.Errorf("deleteRecordsWhere: multi-type index %q cannot be cleared with prefix %v", idx.Name, prefix)
				}
				actions = append(actions, indexAction{index: idx, prefix: idxPrefix})
			} else {
				// Single-type index: clear ALL entries for this index.
				actions = append(actions, indexAction{index: idx, prefix: tuple.Tuple{}})
			}
		} else {
			// Universal index: the PK prefix must match leading index
			// expression columns so we can do a range clear.
			idxPrefix, ok := computeIndexDeletePrefix(idx, prefix, store.metaData, matchingTypeNames)
			if !ok {
				return fmt.Errorf("deleteRecordsWhere: index %q cannot be cleared with prefix %v — "+
					"leading index expression does not match PK prefix", idx.Name, prefix)
			}
			actions = append(actions, indexAction{index: idx, prefix: idxPrefix})
		}
	}

	// Clear records subspace.
	if err := clearPrefixRange(tx, store.subspace.Sub(RecordKey), prefix); err != nil {
		return err
	}

	// Clear legacy record versions in the separate RecordVersionKey(8) subspace.
	// Only the legacy layout stores versions there; in the modern layout versions
	// are inline (pk+-1) within the RecordKey subspace cleared above. Matches Java's
	// deleteRecordsWhereAsync: `useOldVersionFormat() && isStoreRecordVersions()`.
	if store.useOldVersionFormat() && store.metaData.IsStoreRecordVersions() {
		if err := clearPrefixRange(tx, store.subspace.Sub(RecordVersionKey), prefix); err != nil {
			return err
		}
	}

	// Remove pending version mutations and local version cache entries for
	// the cleared ranges. Without this, orphaned SET_VERSIONSTAMPED_VALUE
	// mutations for deleted records' version keys would still be flushed
	// at commit. Matches Java's context.clear → removeVersionMutationRange().
	if err := store.removeVersionDataInPrefixRange(store.subspace.Sub(RecordKey), prefix); err != nil {
		return err
	}
	if store.useOldVersionFormat() && store.metaData.IsStoreRecordVersions() {
		if err := store.removeVersionDataInPrefixRange(store.subspace.Sub(RecordVersionKey), prefix); err != nil {
			return err
		}
	}

	// Clear record counts.
	countKeyExpr := store.metaData.GetRecordCountKey()
	if countKeyExpr != nil && !store.isRecordCountDisabled() {
		countSub := store.subspace.Sub(RecordCountKey)
		countColSize := countKeyExpr.ColumnSize()
		if len(prefix) == countColSize {
			// Delete exact count entry.
			tx.Clear(fdb.Key(countSub.Pack(prefix)))
		} else if len(prefix) < countColSize {
			// Delete range of count entries under this prefix.
			if err := clearPrefixRange(tx, countSub, prefix); err != nil {
				return err
			}
		}
		// If prefix > countColSize, the count key is coarser than the
		// prefix — we can't adjust it. This matches Java which simply
		// skips when the prefix doesn't align with the count key.
	}

	// Delete index entries via each maintainer.
	for _, action := range actions {
		maintainer, mErr := store.getIndexMaintainer(action.index)
		if mErr != nil {
			return mErr
		}
		if err := maintainer.DeleteWhere(action.prefix); err != nil {
			return err
		}

		// Also clear version mutations/cache for the index subspace range.
		idxSub := store.indexSubspace(action.index)
		if err := store.removeVersionDataInPrefixRange(idxSub, action.prefix); err != nil {
			return err
		}
	}

	return nil
}

// removeVersionDataInPrefixRange removes pending version mutations and local
// version cache entries whose key falls within the PrefixRange of sub.Pack(prefix).
func (store *FDBRecordStore) removeVersionDataInPrefixRange(sub subspace.Subspace, prefix tuple.Tuple) error {
	key := sub.Pack(prefix)
	pr, err := fdb.PrefixRange(key)
	if err != nil {
		return fmt.Errorf("removeVersionDataInPrefixRange: PrefixRange(%x): %w", key, err)
	}
	begin, end := pr.FDBRangeKeys()
	store.context.RemoveVersionMutationsInRange(begin.FDBKey(), end.FDBKey())
	store.context.RemoveLocalVersionsInRange(begin.FDBKey(), end.FDBKey())
	return nil
}

// findMatchingRecordTypes returns names of record types whose PK has
// enough columns for the given prefix AND whose record type key matches
// the prefix value (when PKs have a RecordTypeKey prefix).
//
// Matches Java's behavior where recordTypeKeyComparison narrows
// allRecordTypes to just the target type, preventing index clears
// from leaking to other types' indexes.
func (store *FDBRecordStore) findMatchingRecordTypes(prefix tuple.Tuple) []string {
	var names []string
	for _, rt := range store.metaData.RecordTypes() {
		pkColSize := rt.PrimaryKey.ColumnSize()
		if len(prefix) > pkColSize {
			continue
		}
		// If the PK starts with RecordTypeKey and the prefix has a value
		// for it, only include types whose type key matches the prefix.
		if len(prefix) >= 1 && hasRecordTypeKeyPrefix(rt.PrimaryKey) {
			typeKey := rt.GetRecordTypeKey()
			if !recordTypeKeyEquals(prefix[0], typeKey) {
				continue
			}
		}
		names = append(names, rt.Name)
	}
	return names
}

// recordTypeKeyEquals compares a prefix value against a record type key,
// handling Go's int type normalization. FDB tuple decoding produces int64,
// but RecordType.GetRecordTypeKey() may return int (from RecordTypeIndex).
func recordTypeKeyEquals(prefixVal, typeKey any) bool {
	if prefixVal == typeKey {
		return true
	}
	// Normalize both to int64 for comparison.
	pInt, pOk := toInt64Value(prefixVal)
	tInt, tOk := toInt64Value(typeKey)
	if pOk && tOk {
		return pInt == tInt
	}
	return false
}

func toInt64Value(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	default:
		return 0, false
	}
}

// hasRecordTypeKeyPrefix returns true if the expression starts with
// RecordTypeKeyExpression. Matches Java's Key.Expressions.hasRecordTypePrefix().
func hasRecordTypeKeyPrefix(expr KeyExpression) bool {
	switch e := expr.(type) {
	case *RecordTypeKeyExpression:
		return true
	case *CompositeKeyExpression:
		return len(e.expressions) > 0 && hasRecordTypeKeyPrefix(e.expressions[0])
	case *GroupingKeyExpression:
		return hasRecordTypeKeyPrefix(e.wholeKey)
	case *KeyWithValueExpression:
		return hasRecordTypeKeyPrefix(e.innerKey)
	default:
		return false
	}
}

// recordTypesForIndex returns the names of record types that have this
// index defined. Returns nil for universal indexes.
func (store *FDBRecordStore) recordTypesForIndex(idx *Index) []string {
	// Check if it's a universal index.
	for _, uIdx := range store.metaData.GetUniversalIndexes() {
		if uIdx.Name == idx.Name {
			return nil // Universal.
		}
	}
	// Find which record types have this index.
	var names []string
	for _, rt := range store.metaData.RecordTypes() {
		for _, rtIdx := range store.metaData.GetIndexesForRecordType(rt.Name) {
			if rtIdx.Name == idx.Name {
				names = append(names, rt.Name)
				break
			}
		}
	}
	return names
}

// computeIndexDeletePrefix computes the tuple prefix to use for clearing
// an index, given a primary key prefix. The index expression's leading
// columns must structurally match the PK's leading columns for the prefix
// length.
//
// For example:
//   - PK = Concat(RecordType(), Field("id")), prefix = (typeKey)
//   - Index expr = Concat(RecordType(), Field("price"))
//   - PK column 0 = RecordType, index column 0 = RecordType → match
//   - Index delete prefix = (typeKey) (first prefix value maps to first index column)
//
// Returns (prefix, true) if the mapping works, or (nil, false) if not.
func computeIndexDeletePrefix(idx *Index, prefix tuple.Tuple, md *RecordMetaData, matchingTypes []string) (tuple.Tuple, bool) {
	// Use the first matching record type's PK for comparison.
	// matchingTypes is the set of types whose data is being deleted —
	// their PK structure determines how the prefix maps to index columns.
	var samplePK KeyExpression
	for _, name := range matchingTypes {
		rt := md.GetRecordType(name)
		if rt != nil && rt.PrimaryKey != nil {
			samplePK = rt.PrimaryKey
			break
		}
	}
	if samplePK == nil {
		// Fallback: use any type (backwards compat for edge cases).
		for _, rt := range md.RecordTypes() {
			samplePK = rt.PrimaryKey
			break
		}
	}
	if samplePK == nil {
		return nil, false
	}

	pkComponents := normalizeKeyForPositions(samplePK)
	idxComponents := normalizeKeyForPositions(idx.RootExpression)

	// Check that for each PK component covered by the prefix,
	// the same component appears at the same position in the index expression.
	for i := range len(prefix) {
		if i >= len(pkComponents) || i >= len(idxComponents) {
			return nil, false
		}
		if !keyExpressionEquals(pkComponents[i], idxComponents[i]) {
			return nil, false
		}
	}

	return prefix, true
}

// clearPrefixRange clears all keys under sub.Pack(prefix) using PrefixRange
// to include the prefix key itself (important for ungrouped aggregate data).
func clearPrefixRange(tx fdb.WritableTransaction, sub subspace.Subspace, prefix tuple.Tuple) error {
	key := sub.Pack(prefix)
	pr, err := fdb.PrefixRange(key)
	if err != nil {
		return fmt.Errorf("clearPrefixRange: PrefixRange(%x): %w", key, err)
	}
	tx.ClearRange(pr)
	return nil
}

package recordlayer

import (
	"fmt"
	"slices"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
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

		// The record types whose data is being deleted AND which this index
		// covers. Alignment has to be checked against THESE primary keys, not
		// against an arbitrary one of the matching types: the question is how
		// this index's entries relate to the prefix, and only the types the
		// index is defined on have entries in it. Sampling one type out of the
		// full matching set read a primary key from an unrelated type — and
		// since the metadata's record types live in a map, WHICH unrelated type
		// varied from run to run.
		coveredTypeNames := matchingTypeNames
		if !isUniversal {
			coveredTypeNames = nil
			for _, itn := range indexTypeNames {
				if slices.Contains(matchingTypeNames, itn) {
					coveredTypeNames = append(coveredTypeNames, itn)
				}
			}
			if len(coveredTypeNames) == 0 {
				continue // Index doesn't cover any types being deleted, skip.
			}
		}

		// A sliding-window index has a SECOND thing to satisfy, and it is not
		// implied by the first. Java asks both, in one place and BEFORE any
		// clear is queued: SlidingWindowIndexMaintainer.canDeleteWhere
		// (:319-326) requires the delegate to accept the prefix AND the prefix
		// to satisfy the PARTITION key, and deleteRecordsWhereCheckIndexes
		// (FDBRecordStore.java:1997-2008) runs every such check in the deleter's
		// constructor, throwing before a single range is touched.
		//
		// Both halves matter here:
		//
		//   - PARTITION, NOT ARITY. The delegate's check is against the index's
		//     own key expression; keyspace 10 is keyed by the PARTITION key,
		//     which may be a different field of the same width. With a
		//     `quantity`-prefixed vector index and `PARTITION BY price`, a
		//     prefix of (7) means quantity=7 to the delegate and would be read
		//     as price=7 against the window — clearing one price partition
		//     while the deleted records are spread across all of them, and
		//     leaving entries, counts and boundaries that mis-promote later.
		//
		//   - BEFORE, NOT DURING. Refusing inside the maintainer's DeleteWhere
		//     is too late: the record, version and count clears are already
		//     queued on the transaction by then, so a caller that commits
		//     anyway loses the records while the index keeps them.
		if isSlidingWindowIndex(idx) {
			if err := checkSlidingWindowDeleteWhere(idx, prefix, store.metaData, coveredTypeNames); err != nil {
				return err
			}
		}

		if !isUniversal {

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
				idxPrefix, ok := computeIndexDeletePrefix(idx, prefix, store.metaData, coveredTypeNames)
				if !ok {
					return fmt.Errorf("deleteRecordsWhere: multi-type index %q cannot be cleared with prefix %v", idx.Name, prefix)
				}
				actions = append(actions, indexAction{index: idx, prefix: idxPrefix})
			} else {
				// Single-type index. Clearing ALL of it is correct ONLY when the
				// delete-where selects the whole record type — Java's
				// `indexMatcher == null` arm of canDeleteWhereForIndexOnStoredTypes
				// (FDBRecordStore.java:2050-2051), reached when the delete-where
				// component is exactly a RecordTypeKeyComparison and the derived
				// index prefix is empty by construction.
				//
				// For any narrower prefix Java instead requires the query to
				// match a PREFIX of the index's own key expression and THROWS
				// when it does not. Clearing the whole index there destroys
				// entries for records that still exist: with PK
				// (customer_id, order_id) and an index on `total`,
				// DeleteRecordsWhere((cust1)) would empty `total` for cust2 as
				// well, leaving their records unindexed and silently missing
				// from every query served by that index.
				idxPrefix, ok := computeSingleTypeIndexDeletePrefix(idx, prefix, store.metaData, coveredTypeNames)
				if !ok {
					return fmt.Errorf("deleteRecordsWhere: index %q cannot be cleared with prefix %v — "+
						"the prefix does not match the index's leading key expression columns, so the "+
						"clear cannot be scoped to the deleted records", idx.Name, prefix)
				}
				actions = append(actions, indexAction{index: idx, prefix: idxPrefix})
			}
		} else {
			// Universal index: the PK prefix must match leading index
			// expression columns so we can do a range clear.
			idxPrefix, ok := computeIndexDeletePrefix(idx, prefix, store.metaData, coveredTypeNames)
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

// recordTypeKeyEquals reports whether a caller-supplied prefix value selects
// the given record type key. It compares the values by the BYTES they encode
// to, which is the same question the stored keys answer: the prefix is going
// to be packed into a key range, and it matches this type exactly when their
// encodings agree. That also makes an int prefix match an int64 type key
// without a special case, since their encodings are identical.
//
// Comparing the interfaces directly is not an option: `prefixVal == typeKey`
// PANICS ("comparing uncomparable type []uint8") the moment either side is a
// []byte, which an explicit bytes record type key makes reachable from an
// ordinary DeleteRecordsWhere.
func recordTypeKeyEquals(prefixVal, typeKey any) bool {
	prefixID, ok := recordTypeKeyIdentity(prefixVal)
	if !ok {
		return false
	}
	typeID, ok := recordTypeKeyIdentity(typeKey)
	if !ok {
		return false
	}
	return prefixID == typeID
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
// Every covered type must align, not just one of them. Java reaches the same
// place from the other direction: deleteRecordsWhereCheckRecordTypes requires
// the query to equality-match EVERY record type's primary key and to produce
// the SAME evaluated prefix for each ("Primary key prefixes don't align"), so
// by the time canDeleteWhere runs there is only one prefix left to check. Go
// takes a positional tuple prefix rather than a query, so the per-type check
// lands here instead — and it must fail closed, since one misaligned type is
// enough to make the range clear delete the wrong entries.
func computeIndexDeletePrefix(idx *Index, prefix tuple.Tuple, md *RecordMetaData, coveredTypes []string) (tuple.Tuple, bool) {
	pks := deleteWherePrimaryKeys(md, coveredTypes)
	if len(pks) == 0 {
		return nil, false
	}

	idxComponents := normalizeKeyForPositions(idx.RootExpression)

	// Check that for each PK component covered by the prefix,
	// the same component appears at the same position in the index expression.
	for _, pk := range pks {
		pkComponents := normalizeKeyForPositions(pk)
		for i := range len(prefix) {
			if i >= len(pkComponents) || i >= len(idxComponents) {
				return nil, false
			}
			if !keyExpressionEquals(pkComponents[i], idxComponents[i]) {
				return nil, false
			}
		}
	}

	return prefix, true
}

// checkSlidingWindowDeleteWhere is the sliding-window half of Java's
// canDeleteWhere (SlidingWindowIndexMaintainer.java:319-326), run as a
// PREFLIGHT so a refusal costs nothing — see the call site for why both
// properties are load-bearing.
//
// Java expresses the structural half as
// `matcher.matchesSatisfyingQuery(partitionKey)` followed by
// StandardIndexMaintainer.canDeleteWhere, which together demand that the
// delete-where be an EQUALITY match on a prefix of the partition key. Go's
// delete-where is a positional primary-key prefix rather than a query, so the
// same demand is expressed positionally: every column the prefix covers must be
// the same key expression in the primary key and in the partition key.
func checkSlidingWindowDeleteWhere(idx *Index, prefix tuple.Tuple, md *RecordMetaData, coveredTypes []string) error {
	spec, err := idx.RowNumberWindowSpec()
	if err != nil {
		return fmt.Errorf("deleteRecordsWhere: sliding window index %q: %w", idx.Name, err)
	}
	partitionKey, err := spec.PartitionKey()
	if err != nil {
		return fmt.Errorf("deleteRecordsWhere: sliding window index %q: partition key: %w", idx.Name, err)
	}
	if partitionKey == nil {
		return &SlidingWindowDeleteWhereError{
			IndexName: idx.Name,
			Message: "the window is unpartitioned, so it keeps one entry list for the whole " +
				"index and no range of it corresponds to the requested prefix",
		}
	}

	pks := deleteWherePrimaryKeys(md, coveredTypes)
	if len(pks) == 0 {
		return &SlidingWindowDeleteWhereError{
			IndexName: idx.Name,
			Message:   "no primary key could be resolved for the record types being deleted",
		}
	}
	partitionComponents := normalizeKeyForPositions(partitionKey)

	for _, pk := range pks {
		// THE RECORD-TYPE KEY IS NOT A PARTITION COLUMN AND NEVER CAN BE. A
		// window partitions by FIELD PATHS (RowNumberWindowPredicate's
		// partition_fields), and a record-type key is not a field, so a
		// type-prefixed primary key always carries one leading column the
		// partition key cannot have.
		//
		// Comparing the raw prefix would therefore reject every type-prefixed
		// schema on its first column — including shapes that work and that Java
		// accepts, because Java strips the same column
		// (indexEvaluated = evaluated.subList(1, …), FDBRecordStore.java:1951)
		// before asking the index anything. computeSingleTypeIndexDeletePrefix
		// strips it too, so checking the unstripped prefix here would also make
		// the preflight disagree with the prefix the maintainer is then handed.
		pkComponents := normalizeKeyForPositions(pk)
		effective := prefix
		if hasRecordTypeKeyPrefix(pk) {
			if len(effective) == 0 {
				continue
			}
			effective = effective[1:]
			pkComponents = pkComponents[1:]
		}
		// An empty effective prefix is the WHOLE-TYPE delete — Java's
		// indexMatcher == null arm — where the whole index, and so the whole
		// keyspace-10 region, is cleared. There are no partition columns left to
		// agree about, and clearing everything is exactly right.
		if len(effective) == 0 {
			continue
		}
		if len(effective) > len(partitionComponents) {
			return &SlidingWindowDeleteWhereError{
				IndexName: idx.Name,
				Message: fmt.Sprintf("prefix %v reaches past the partition key, so the window "+
					"cannot be scoped to the deleted records", prefix),
			}
		}
		for i := range effective {
			if i >= len(pkComponents) {
				return &SlidingWindowDeleteWhereError{
					IndexName: idx.Name,
					Message: fmt.Sprintf("prefix %v reaches past the primary key, so the window "+
						"cannot be scoped to the deleted records", prefix),
				}
			}
			if !keyExpressionEquals(pkComponents[i], partitionComponents[i]) {
				return &SlidingWindowDeleteWhereError{
					IndexName: idx.Name,
					Message: fmt.Sprintf("prefix %v names primary-key columns that are not the "+
						"window's partition columns, so clearing a partition group would clear a "+
						"different set of records than the ones being deleted", prefix),
				}
			}
		}
	}
	return nil
}

// computeSingleTypeIndexDeletePrefix decides how much of a SINGLE-TYPE index a
// deleteRecordsWhere may clear, and refuses when the answer is "none of it
// safely".
//
// It is the Go shape of the three arms Java's canDeleteWhereForIndexOnStoredTypes
// (FDBRecordStore.java:2041-2056) takes for an index on stored types:
//
//   - WHOLE TYPE — the delete-where selects every record of the type. Java
//     spells this `indexMatcher == null`, i.e. the component was exactly a
//     RecordTypeKeyComparison; here that is a PK with a record-type-key prefix
//     and a prefix consisting of just that key. Clear the whole index.
//
//   - INDEX ROOT CARRIES THE TYPE KEY — Java's
//     `Key.Expressions.hasRecordTypePrefix(index.getRootExpression())` branch,
//     which matches the FULL prefix against the index root. Positions line up
//     one-for-one with the primary key's.
//
//   - OTHERWISE — Java trims the record-type key off both the matcher and the
//     evaluated prefix (`indexEvaluated = evaluated.subList(1, …)`) and matches
//     the remainder against the index root. So the PK is compared from column 1
//     while the index is compared from column 0.
//
// Any prefix column that does not line up makes the clear unscopeable, and the
// caller turns that into an error — which is what Java's `canDelete == false`
// does.
func computeSingleTypeIndexDeletePrefix(idx *Index, prefix tuple.Tuple, md *RecordMetaData, coveredTypes []string) (tuple.Tuple, bool) {
	pks := deleteWherePrimaryKeys(md, coveredTypes)
	if len(pks) != 1 {
		// A single-type index has exactly one covered type. Anything else means
		// the caller's classification and the metadata disagree; refusing is the
		// only safe answer.
		return nil, false
	}
	samplePK := pks[0]
	pkHasTypeKey := hasRecordTypeKeyPrefix(samplePK)

	// Arm 1: the delete-where names the record type and nothing else.
	if pkHasTypeKey && len(prefix) == 1 {
		return tuple.Tuple{}, true
	}

	pkOffset := 0
	if pkHasTypeKey && !hasRecordTypeKeyPrefix(idx.RootExpression) {
		// Arm 3: the index does not repeat the record-type key, so the prefix's
		// leading type-key column has no counterpart in the index and is
		// dropped from both the comparison and the resulting prefix.
		pkOffset = 1
	}

	pkComponents := normalizeKeyForPositions(samplePK)
	idxComponents := normalizeKeyForPositions(idx.RootExpression)

	remaining := prefix[pkOffset:]
	for i := range remaining {
		if i+pkOffset >= len(pkComponents) || i >= len(idxComponents) {
			return nil, false
		}
		if !keyExpressionEquals(pkComponents[i+pkOffset], idxComponents[i]) {
			return nil, false
		}
	}
	return remaining, true
}

// deleteWherePrimaryKeys returns the primary keys whose structure decides how a
// delete-where prefix maps onto an index's columns — one per covered record
// type, in the caller's order. A named type with no primary key is dropped
// rather than substituted for, so a caller that gets fewer keys than names can
// refuse instead of validating against a stand-in.
func deleteWherePrimaryKeys(md *RecordMetaData, coveredTypes []string) []KeyExpression {
	pks := make([]KeyExpression, 0, len(coveredTypes))
	for _, name := range coveredTypes {
		rt := md.GetRecordType(name)
		if rt != nil && rt.PrimaryKey != nil {
			pks = append(pks, rt.PrimaryKey)
		}
	}
	return pks
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

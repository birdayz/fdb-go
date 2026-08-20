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

		var (
			idxPrefix tuple.Tuple
			// pkOffset is how many leading primary-key columns idxPrefix skips.
			// The prefix alone does not say: (typeKey, quantity) and (quantity)
			// are both plausible readings of the same tuple, and only the offset
			// distinguishes them. The sliding-window check below needs it to know
			// WHICH primary-key column each element of idxPrefix came from.
			pkOffset int
			ok       bool
		)

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
				idxPrefix, ok = computeIndexDeletePrefix(idx, prefix, store.metaData, coveredTypeNames)
				if !ok {
					return fmt.Errorf("deleteRecordsWhere: multi-type index %q cannot be cleared with prefix %v", idx.Name, prefix)
				}
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
				idxPrefix, pkOffset, ok = computeSingleTypeIndexDeletePrefix(idx, prefix, store.metaData, coveredTypeNames)
				if !ok {
					return fmt.Errorf("deleteRecordsWhere: index %q cannot be cleared with prefix %v — "+
						"the prefix does not match the index's leading key expression columns, so the "+
						"clear cannot be scoped to the deleted records", idx.Name, prefix)
				}
			}
		} else {
			// Universal index: the PK prefix must match leading index
			// expression columns so we can do a range clear.
			idxPrefix, ok = computeIndexDeletePrefix(idx, prefix, store.metaData, coveredTypeNames)
			if !ok {
				return fmt.Errorf("deleteRecordsWhere: index %q cannot be cleared with prefix %v — "+
					"leading index expression does not match PK prefix", idx.Name, prefix)
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
		// Three properties, each of which failed on its own once:
		//
		//   - PARTITION, NOT ARITY. The delegate's check is against the index's
		//     own key expression; keyspace 10 is keyed by the PARTITION key,
		//     which may be a different field of the same width. With a
		//     `quantity`-prefixed vector index and `PARTITION BY price`, a
		//     prefix of (7) means quantity=7 to the delegate and would be read
		//     as price=7 against the window — clearing one price partition
		//     while the deleted records are spread across all of them.
		//
		//   - THE PREFIX THE MAINTAINER ACTUALLY GETS. This runs AFTER idxPrefix
		//     is computed, and on idxPrefix, because the two can differ: the
		//     record-type column is stripped only when the index root does not
		//     repeat it. Checking the raw prefix instead approved a delete whose
		//     action prefix was a different tuple, and the clear then missed the
		//     window entirely while the records and the graph went — succeeding,
		//     silently, with the bookkeeping left behind.
		//
		//   - BEFORE, NOT DURING. Refusing inside the maintainer's DeleteWhere
		//     is too late: the record, version and count clears are already
		//     queued on the transaction by then, so a caller that commits
		//     anyway loses the records while the index keeps them.
		if isSlidingWindowIndex(idx) {
			if err := checkSlidingWindowDeleteWhere(
				idx, prefix, idxPrefix, pkOffset, store.metaData, coveredTypeNames); err != nil {
				return err
			}
		}

		// And the general form of the same question, for EVERY maintainer:
		// Java's deleteRecordsWhereCheckIndexes asks canDeleteWhere of each one
		// in the deleter's constructor (FDBRecordStore.java:1997-2008), before
		// any range is touched. Go asked nothing, so three maintainers that
		// refuse a prefix — TEXT (ungrouped), SPFresh (grouped), and a vector
		// index whose prefix reaches past its split point — all raised that
		// refusal from inside DeleteWhere, by which time the records were
		// already cleared on this transaction.
		//
		// Constructing the maintainer here is deliberate and matches Java,
		// which builds every maintainer in that same constructor: an index whose
		// maintainer cannot be built is one whose entries cannot be cleared, and
		// discovering that before the clear is the whole point.
		maintainer, mErr := store.getIndexMaintainer(idx)
		if mErr != nil {
			return mErr
		}
		// Every maintainer answers. CanDeleteWhere is part of IndexMaintainer,
		// so there is no "did not implement it" case to fall through — which is
		// what Java's abstract canDeleteWhere buys, and what keeps a new index
		// type from reaching this path without having decided what its physical
		// keys are prefixed by. That is not hypothetical: while the check was
		// merely optional here, the bound was found missing on RANK, PERMUTED,
		// the aggregates and TEXT in turn, because "does not implement it" and
		// "can clear any prefix" are the same answer to a type assertion.
		if err := maintainer.CanDeleteWhere(idxPrefix); err != nil {
			return fmt.Errorf("deleteRecordsWhere: %w", err)
		}

		actions = append(actions, indexAction{index: idx, prefix: idxPrefix})
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
// RecordTypeKeyExpression. Matches Java's Key.Expressions.hasRecordTypePrefix
// (Key.java:365-382) arm for arm.
//
// The WIDTH GUARDS on the two wrappers are the subtle half, and they are not
// belt-and-braces. A wrapper can hide the record-type column entirely:
//
//   - Ungrouped(RecordTypeKey()) has grouping count ZERO, so every column
//     including the type key is aggregated away and no physical entry is keyed
//     by it. Java's `getGroupingCount() > 0` says "no type prefix", and a
//     type-only delete then derives an EMPTY index prefix, clearing the whole
//     index — which is right, because the index holds one entry for the type.
//     Reading the grouped-away column as a physical prefix instead derives a
//     one-column prefix naming no entry, and the delete Java accepts is refused.
//   - KeyWithValue(..., 0) is the same shape: with a split point of zero the
//     type key sits in the VALUE, not in the key.
//
// The NestingKeyExpression arm recurses without a guard, matching Java: a nest
// navigates into a submessage, and its child is what starts the key.
func hasRecordTypeKeyPrefix(expr KeyExpression) bool {
	switch e := expr.(type) {
	case *RecordTypeKeyExpression:
		return true
	case *CompositeKeyExpression:
		return len(e.expressions) > 0 && hasRecordTypeKeyPrefix(e.expressions[0])
	case *GroupingKeyExpression:
		return e.GetGroupingCount() > 0 && hasRecordTypeKeyPrefix(e.wholeKey)
	case *KeyWithValueExpression:
		return e.splitPoint > 0 && hasRecordTypeKeyPrefix(e.innerKey)
	case *NestingKeyExpression:
		return hasRecordTypeKeyPrefix(e.child)
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
// PREFLIGHT so a refusal costs nothing — see the call site for why each of its
// properties is load-bearing.
//
// Java expresses the structural half as
// `matcher.matchesSatisfyingQuery(partitionKey)` followed by
// StandardIndexMaintainer.canDeleteWhere, which together demand that the
// delete-where be an EQUALITY match on a prefix of the partition key. Go's
// delete-where is a positional primary-key prefix rather than a query, so the
// same demand is expressed positionally.
//
// It is asked of idxPrefix — the tuple the maintainer will actually be handed —
// and NOT of the caller's raw prefix, which can be a different tuple. pkOffset
// says which primary-key column idxPrefix[0] came from, and it has to be passed
// in rather than re-derived: (typeKey, quantity) and (quantity) are the same two
// elements read two ways, and only the offset tells them apart.
//
// rawPrefix is carried for the error messages alone; every decision is made on
// idxPrefix.
func checkSlidingWindowDeleteWhere(
	idx *Index,
	rawPrefix, idxPrefix tuple.Tuple,
	pkOffset int,
	md *RecordMetaData,
	coveredTypes []string,
) error {
	// An empty index prefix is the WHOLE-TYPE delete — Java's
	// indexMatcher == null arm — where the entire index, and so the entire
	// keyspace-10 region, is cleared.
	//
	// This comes FIRST, before the window is even inspected, because Java
	// reaches that arm by RETURNING TRUE without asking the maintainer anything
	// (FDBRecordStore.java:2050-2051). An unpartitioned window is refused for
	// every other prefix precisely because no range of its single entry list
	// corresponds to one — but "all of it" always does, so refusing here would
	// reject a delete Java performs, on the shape (a type-prefixed primary key,
	// an index root that omits the type column) most likely to carry one.
	if len(idxPrefix) == 0 {
		return nil
	}

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
	if len(idxPrefix) > len(partitionComponents) {
		return &SlidingWindowDeleteWhereError{
			IndexName: idx.Name,
			Message: fmt.Sprintf("prefix %v reaches past the partition key, so the window "+
				"cannot be scoped to the deleted records", rawPrefix),
		}
	}

	for _, pk := range pks {
		pkComponents := normalizeKeyForPositions(pk)
		for i := range idxPrefix {
			// idxPrefix[i] carries the value of primary-key column pkOffset+i,
			// and the window's subspace is keyed by partition column i. The
			// clear is only scoped to the deleted records when those are the
			// same expression.
			//
			// This is where a record-type key is refused rather than stripped: a
			// window partitions by FIELD PATHS, so partitionComponents can never
			// hold one, and a shape whose index root repeats the type key keeps
			// it in idxPrefix — making element 0 a column the window does not
			// have. Refusing is the honest answer; stripping it here would
			// approve a clear that then addresses the wrong subspace.
			if pkOffset+i >= len(pkComponents) {
				return &SlidingWindowDeleteWhereError{
					IndexName: idx.Name,
					Message: fmt.Sprintf("prefix %v reaches past the primary key, so the window "+
						"cannot be scoped to the deleted records", rawPrefix),
				}
			}
			if !keyExpressionEquals(pkComponents[pkOffset+i], partitionComponents[i]) {
				return &SlidingWindowDeleteWhereError{
					IndexName: idx.Name,
					Message: fmt.Sprintf("prefix %v names primary-key columns that are not the "+
						"window's partition columns, so clearing a partition group would clear a "+
						"different set of records than the ones being deleted", rawPrefix),
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
// The second result is the pkOffset: how many leading primary-key columns the
// returned prefix skips. Callers that need to know WHICH primary-key column
// each element came from cannot recover it from the tuple — (typeKey, quantity)
// and (quantity) are both plausible readings of the same two bytes — so it is
// returned rather than re-derived.
func computeSingleTypeIndexDeletePrefix(idx *Index, prefix tuple.Tuple, md *RecordMetaData, coveredTypes []string) (tuple.Tuple, int, bool) {
	pks := deleteWherePrimaryKeys(md, coveredTypes)
	if len(pks) != 1 {
		// A single-type index has exactly one covered type. Anything else means
		// the caller's classification and the metadata disagree; refusing is the
		// only safe answer.
		return nil, 0, false
	}
	samplePK := pks[0]
	pkHasTypeKey := hasRecordTypeKeyPrefix(samplePK)

	// Arm 1: the delete-where names the record type and nothing else. The
	// offset is 1 because the one column the caller gave is consumed here; the
	// empty prefix that comes back covers no primary-key column at all.
	//
	// It applies only when the INDEX ROOT omits the record-type key. Java's
	// canDeleteWhereForIndexOnStoredTypes tests
	// `hasRecordTypePrefix(index.getRootExpression())` FIRST
	// (FDBRecordStore.java:2044) and, when the root does carry it, asks the
	// maintainer with the full evaluated prefix — never reaching the
	// whole-index arm at all. Collapsing to an empty prefix here instead would
	// hand the maintainer a tuple that skips its capability check entirely: an
	// unpartitioned sliding window, which cannot serve any prefix, would have
	// its whole keyspace-10 region cleared without ever being asked.
	if pkHasTypeKey && len(prefix) == 1 && !hasRecordTypeKeyPrefix(idx.RootExpression) {
		return tuple.Tuple{}, 1, true
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
			return nil, 0, false
		}
		if !keyExpressionEquals(pkComponents[i+pkOffset], idxComponents[i]) {
			return nil, 0, false
		}
	}
	return remaining, pkOffset, true
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

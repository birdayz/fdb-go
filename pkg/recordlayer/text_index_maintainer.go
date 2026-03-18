package recordlayer

import (
	"fmt"
	"strconv"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

const (
	textIndexBunchSize = 20
)

// tokenizerVersionSubspaceTuple is the subspace key for storing per-record tokenizer versions.
// Matches Java's TextIndexMaintainer.TOKENIZER_VERSION_SUBSPACE_TUPLE = Tuple.from(0L).
var tokenizerVersionSubspaceTuple = tuple.Tuple{int64(0)}

// textIndexMaintainer maintains a TEXT index using a BunchedMap for token→position list storage.
// Matches Java's TextIndexMaintainer.
type textIndexMaintainer struct {
	index         *Index
	indexSubspace subspace.Subspace
	secSubspace   subspace.Subspace // secondary subspace for tokenizer version tracking
	tx            fdb.Transaction
	store         indexStoreContext

	tokenizer                   TextTokenizer
	tokenizerVersion            int
	addAggressiveConflictRanges bool
	omitPositionLists           bool
	bunchedMap                  *BunchedMap
}

func newTextIndexMaintainer(index *Index, indexSubspace subspace.Subspace, secSubspace subspace.Subspace, tx fdb.Transaction, store indexStoreContext) *textIndexMaintainer {
	tokenizer := getTextTokenizer(index)
	tokenizerVersion := getTextTokenizerVersion(index)

	return &textIndexMaintainer{
		index:                       index,
		indexSubspace:               indexSubspace,
		secSubspace:                 secSubspace,
		tx:                          tx,
		store:                       store,
		tokenizer:                   tokenizer,
		tokenizerVersion:            tokenizerVersion,
		addAggressiveConflictRanges: getTextAggressiveConflictRanges(index),
		omitPositionLists:           getTextOmitPositions(index),
		bunchedMap:                  NewBunchedMap(textIndexBunchSize),
	}
}

// getTextTokenizer gets the tokenizer for a TEXT index from the registry.
func getTextTokenizer(index *Index) TextTokenizer {
	name := index.Options[IndexOptionTextTokenizerName]
	return GetTextTokenizer(name)
}

// getTextTokenizerVersion gets the tokenizer version from index options.
func getTextTokenizerVersion(index *Index) int {
	versionStr := index.Options[IndexOptionTextTokenizerVersion]
	if versionStr == "" {
		return 0 // GLOBAL_MIN_VERSION
	}
	v, err := strconv.Atoi(versionStr)
	if err != nil {
		panic(fmt.Sprintf("tokenizer version could not be parsed as int: %q", versionStr))
	}
	return v
}

func getTextAggressiveConflictRanges(index *Index) bool {
	return index.Options[IndexOptionTextAddAggressiveConflictRanges] == "true"
}

func getTextOmitPositions(index *Index) bool {
	return index.Options[IndexOptionTextOmitPositions] == "true"
}

// textFieldPosition returns the position of the text field in the index expression.
// This is the first column after all grouping columns (or 0 if no grouping).
func textFieldPosition(expr KeyExpression) int {
	if g, ok := expr.(*GroupingKeyExpression); ok {
		return g.GetGroupingCount()
	}
	return 0
}

// getRecordTokenizerKey returns the FDB key for storing a record's tokenizer version.
func (m *textIndexMaintainer) getRecordTokenizerKey(primaryKey tuple.Tuple) fdb.Key {
	// secSubspace / TOKENIZER_VERSION_SUBSPACE_TUPLE / primaryKey
	versionSubspace := m.secSubspace.Sub(tokenizerVersionSubspaceTuple[0])
	return versionSubspace.Sub(tupleToTupleElements(primaryKey)...).Bytes()
}

// getRecordTokenizerVersion reads the stored tokenizer version for a record.
// Returns GLOBAL_MIN_VERSION (0) if not stored.
func (m *textIndexMaintainer) getRecordTokenizerVersion(primaryKey tuple.Tuple) int {
	key := m.getRecordTokenizerKey(primaryKey)
	rawVersion := m.tx.Get(key).MustGet()
	if rawVersion == nil {
		return 0 // GLOBAL_MIN_VERSION
	}
	t, err := tuple.Unpack(rawVersion)
	if err != nil {
		return 0
	}
	if len(t) == 0 {
		return 0
	}
	v, ok := t[0].(int64)
	if !ok {
		return 0
	}
	return int(v)
}

// writeRecordTokenizerVersion stores the current tokenizer version for a record.
func (m *textIndexMaintainer) writeRecordTokenizerVersion(primaryKey tuple.Tuple) {
	key := m.getRecordTokenizerKey(primaryKey)
	m.tx.Set(key, tuple.Tuple{int64(m.tokenizerVersion)}.Pack())
}

// clearRecordTokenizerVersion removes the stored tokenizer version for a record.
func (m *textIndexMaintainer) clearRecordTokenizerVersion(primaryKey tuple.Tuple) {
	key := m.getRecordTokenizerKey(primaryKey)
	m.tx.Clear(key)
}

// Update updates the TEXT index for a record change.
// Handles tokenizer version tracking and re-indexing on version change.
// Matches Java's TextIndexMaintainer.update().
func (m *textIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	if oldRecord == nil && newRecord != nil {
		// Insert: write tokenizer version, then index.
		m.writeRecordTokenizerVersion(newRecord.PrimaryKey)
		return m.updateStandard(nil, newRecord)
	} else if oldRecord != nil && newRecord == nil {
		// Delete: remove index entries, then clear tokenizer version.
		err := m.updateStandard(oldRecord, nil)
		if err != nil {
			return err
		}
		m.clearRecordTokenizerVersion(oldRecord.PrimaryKey)
		return nil
	} else if oldRecord != nil && newRecord != nil {
		// Update: check if tokenizer version changed.
		recordTokenizerVersion := m.getRecordTokenizerVersion(oldRecord.PrimaryKey)
		if recordTokenizerVersion == m.tokenizerVersion {
			// Same version: standard update.
			return m.updateStandard(oldRecord, newRecord)
		}
		// Version changed: re-index completely.
		// Delete with old version, insert with new version.
		if err := m.updateStandard(oldRecord, nil); err != nil {
			return err
		}
		m.writeRecordTokenizerVersion(newRecord.PrimaryKey)
		return m.updateStandard(nil, newRecord)
	}
	return nil // Both nil.
}

// updateStandard performs the standard index update (evaluate expression, update keys).
func (m *textIndexMaintainer) updateStandard(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	if oldRecord != nil {
		recordTokenizerVersion := m.getRecordTokenizerVersion(oldRecord.PrimaryKey)
		oldEntries, err := m.index.RootExpression.Evaluate(oldRecord, oldRecord.Record)
		if err != nil {
			return err
		}
		if err := m.updateIndexKeys(oldRecord, true, oldEntries, recordTokenizerVersion); err != nil {
			return err
		}
	}
	if newRecord != nil {
		newEntries, err := m.index.RootExpression.Evaluate(newRecord, newRecord.Record)
		if err != nil {
			return err
		}
		if err := m.updateIndexKeys(newRecord, false, newEntries, m.tokenizerVersion); err != nil {
			return err
		}
	}
	return nil
}

// updateIndexKeys tokenizes text and writes/removes BunchedMap entries for each token.
// Matches Java's TextIndexMaintainer.updateIndexKeys().
func (m *textIndexMaintainer) updateIndexKeys(record *FDBStoredRecord[proto.Message], remove bool, entries [][]any, recordTokenizerVersion int) error {
	if len(entries) == 0 {
		return nil
	}
	textPosition := textFieldPosition(m.index.RootExpression)

	for _, entry := range entries {
		if err := m.updateOneKey(record, remove, entry, textPosition, recordTokenizerVersion); err != nil {
			return err
		}
	}
	return nil
}

// updateOneKey processes a single index entry — tokenizes text and updates BunchedMap.
// Matches Java's TextIndexMaintainer.updateOneKeyAsync().
func (m *textIndexMaintainer) updateOneKey(record *FDBStoredRecord[proto.Message], remove bool, entry []any, textPosition int, recordTokenizerVersion int) error {
	// Build the full index entry key (with PK trimming).
	entryTuple := make(tuple.Tuple, len(entry))
	for i, v := range entry {
		entryTuple[i] = v
	}
	indexEntryKey, err := indexEntryKey(m.index, entryTuple, record.PrimaryKey)
	if err != nil {
		return err
	}

	// Extract text field value.
	if textPosition >= len(indexEntryKey) {
		return nil
	}
	textVal := indexEntryKey[textPosition]
	text, ok := textVal.(string)
	if !ok || text == "" {
		return nil
	}

	// Split into grouping key, text, and grouped key.
	var groupingKey tuple.Tuple
	if textPosition > 0 {
		groupingKey = make(tuple.Tuple, textPosition)
		copy(groupingKey, indexEntryKey[:textPosition])
	}
	groupedKey := make(tuple.Tuple, len(indexEntryKey)-textPosition-1)
	copy(groupedKey, indexEntryKey[textPosition+1:])

	// Tokenize the text.
	positionMap := m.tokenizer.TokenizeToMap(text, recordTokenizerVersion, TokenizerModeIndex)
	if len(positionMap) == 0 {
		return nil
	}

	// Add aggressive conflict ranges if configured.
	if m.addAggressiveConflictRanges {
		var begin, end fdb.KeyConvertible
		if groupingKey == nil {
			begin, end = m.indexSubspace.FDBRangeKeys()
		} else {
			sub := m.indexSubspace.Sub(tupleToTupleElements(groupingKey)...)
			begin, end = sub.FDBRangeKeys()
		}
		indexRange := fdb.KeyRange{Begin: begin, End: end}
		m.tx.AddReadConflictRange(indexRange)
		m.tx.AddWriteConflictRange(indexRange)
	}

	// For each token, update the BunchedMap.
	for token, positions := range positionMap {
		var subspaceTuple tuple.Tuple
		if groupingKey == nil {
			subspaceTuple = tuple.Tuple{token}
		} else {
			subspaceTuple = make(tuple.Tuple, 0, len(groupingKey)+1)
			subspaceTuple = append(subspaceTuple, groupingKey...)
			subspaceTuple = append(subspaceTuple, token)
		}
		mapSubspace := m.indexSubspace.Sub(tupleToTupleElements(subspaceTuple)...)

		if remove {
			_, _, err := m.bunchedMap.Remove(m.tx, mapSubspace, groupedKey)
			if err != nil {
				return fmt.Errorf("text index remove token %q: %w", token, err)
			}
		} else {
			value := positions
			if m.omitPositionLists {
				value = nil
			}
			_, _, err := m.bunchedMap.Put(m.tx, mapSubspace, groupedKey, value)
			if err != nil {
				return fmt.Errorf("text index put token %q: %w", token, err)
			}
		}
	}

	return nil
}

// UpdateWhileWriteOnly handles TEXT index updates during WRITE_ONLY state.
// TEXT indexes are idempotent (same token→position mapping for same text),
// so this is a pass-through to Update.
func (m *textIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return m.Update(oldRecord, newRecord)
}

// Scan scans the TEXT index. Only BY_TEXT_TOKEN scan type is supported.
// The scan range should cover the token(s) to search for.
// Matches Java's TextIndexMaintainer.scan().
func (m *textIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	textPosition := textFieldPosition(m.index.RootExpression)
	splitter := NewTextSubspaceSplitter(m.indexSubspace, textPosition+1)

	byteRange := scanRange.ToFDBRange(m.indexSubspace)

	// Adjust limit for skip (matching Java's clearSkipAndAdjustLimit).
	skip := scanProperties.ExecuteProperties.Skip
	adjustedLimit := scanProperties.ExecuteProperties.ReturnedRowLimit
	if skip > 0 && adjustedLimit > 0 {
		adjustedLimit += skip
	}

	// Determine the read transaction (snapshot vs serializable).
	var readTx fdb.ReadTransaction
	if scanProperties.ExecuteProperties.IsolationLevel.IsSnapshot() {
		readTx = m.tx.Snapshot()
	} else {
		readTx = m.tx
	}

	iterator := NewBunchedMapMultiIterator(
		readTx,
		m.indexSubspace,
		splitter,
		[]byte(byteRange.Begin.FDBKey()),
		[]byte(byteRange.End.FDBKey()),
		continuation,
		adjustedLimit,
		scanProperties.IsReverse(),
		TextIndexBunchedSerializerInstance(),
	)

	var cursor RecordCursor[*IndexEntry] = newTextCursor(iterator, m.index, scanProperties)
	if skip > 0 {
		cursor = SkipCursor(cursor, skip)
	}
	return cursor
}

// DeleteWhere clears all TEXT index data for records matching the prefix.
// For TEXT indexes, this can only be aligned with grouping keys — once text
// is tokenized, there's no efficient way to remove documents within the
// grouped part. Matches Java's TextIndexMaintainer.canDeleteWhere() which
// delegates to canDeleteGroup().
func (m *textIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	// Clear index entries using PrefixRange to include the exact prefix key
	// (matching Java's Range.startsWith pattern).
	if len(prefix) == 0 {
		r, err := fdb.PrefixRange(m.indexSubspace.Bytes())
		if err != nil {
			return fmt.Errorf("text index deleteWhere: %w", err)
		}
		m.tx.ClearRange(r)
	} else {
		sub := m.indexSubspace.Sub(tupleToTupleElements(prefix)...)
		r, err := fdb.PrefixRange(sub.Bytes())
		if err != nil {
			return fmt.Errorf("text index deleteWhere: %w", err)
		}
		m.tx.ClearRange(r)
	}

	// Clear tokenizer version entries in secondary subspace.
	if m.secSubspace != nil {
		r, err := fdb.PrefixRange(m.secSubspace.Bytes())
		if err != nil {
			return fmt.Errorf("text index deleteWhere: %w", err)
		}
		m.tx.ClearRange(r)
	}

	return nil
}

// Ensure textIndexMaintainer implements IndexMaintainer.
var _ IndexMaintainer = (*textIndexMaintainer)(nil)

// Portions derived from FoundationDB Record Layer (SplitHelper.java,
// FDBStoredSizes.java, RecordCoreException.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/internal/tuplefast"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// sizeInfo tracks the storage metrics for a record.
// Matches Java's SplitHelper.sizeInfo / FDBStoredSizes.
type sizeInfo struct {
	KeyCount        int
	KeySize         int
	ValueSize       int
	IsSplit         bool
	VersionedInline bool
}

// dryRunSaveWithSplitSizeInfo is SplitHelper.dryRunSaveWithSplitOnlySetSizeInfo,
// data portion: the keys and values saveWithSplit would write, counted and not
// written. A record longer than splitRecordSize is sized as its chunks
// whatever splitLongRecords says, as Java's dry run sizes it (the real save
// refuses it); a shorter one is its one key, suffixed as saveWithSplit
// suffixes it.
func dryRunSaveWithSplitSizeInfo(recordSubspace subspace.Subspace, primaryKey tuple.Tuple, serialized []byte,
	splitLongRecords, omitUnsplitRecordSuffix bool,
) sizeInfo {
	var s sizeInfo
	if len(serialized) > splitRecordSize {
		for index, offset := startSplitRecord, 0; offset < len(serialized); index, offset = index+1, offset+splitRecordSize {
			s.KeyCount++
			s.KeySize += len(recordSubspace.Pack(appendToTuple(primaryKey, index)))
			s.ValueSize += min(offset+splitRecordSize, len(serialized)) - offset
		}
		s.IsSplit = true
		return s
	}
	key := primaryKey
	if splitLongRecords || !omitUnsplitRecordSuffix {
		key = appendToTuple(primaryKey, unsplitRecord)
	}
	s.KeyCount = 1
	s.KeySize = len(recordSubspace.Pack(key))
	s.ValueSize = len(serialized)
	return s
}

// saveWithSplit saves a serialized record, splitting it across multiple KV pairs
// if it exceeds splitRecordSize and splitLongRecords is enabled.
// Incomplete primary keys require a context so chunk mutations can be buffered
// until commit and canceled by context clears. A nil context supports complete
// keys only. The store layer still manages inline record versions separately.
// Matches Java's SplitHelper.saveWithSplit() (data portion only).
func saveWithSplit(
	recordContext *FDBRecordContext,
	tx fdb.WritableTransaction,
	recordSubspace subspace.Subspace,
	primaryKey tuple.Tuple,
	serialized []byte,
	splitLongRecords bool,
	omitUnsplitRecordSuffix bool,
	oldsizeInfo *sizeInfo,
	sizes *sizeInfo,
) error {
	if len(primaryKey) == 0 {
		return fmt.Errorf("primary key must not be empty")
	}

	hasVersionInKey, err := primaryKey.HasIncompleteVersionstamp()
	if err != nil {
		return fmt.Errorf("split record primary key: %w", err)
	}
	if hasVersionInKey && recordContext == nil {
		return &RecordCoreArgumentError{Message: "incomplete split record key requires a record context"}
	}
	dataLen := len(serialized)

	if dataLen > splitRecordSize {
		if !splitLongRecords {
			// Java's SplitHelper.saveWithSplit: a RecordCoreException, which
			// the relational layer reports as UNKNOWN (XXXXX).
			return &RecordCoreError{Message: "Record is too long to be stored in a single value; consider split_long_records"}
		}

		// Clear previous record data
		if !hasVersionInKey {
			clearPreviousRecord(recordContext, tx, recordSubspace, primaryKey, splitLongRecords, oldsizeInfo)
		}

		if sizes != nil {
			*sizes = sizeInfo{}
		}
		// Split the record into chunks
		splitIndex := startSplitRecord
		offset := 0
		for offset < dataLen {
			end := offset + splitRecordSize
			if end > dataLen {
				end = dataLen
			}
			chunk := serialized[offset:end]

			keyTuple := appendToTuple(primaryKey, splitIndex)
			key, err := writeSplitValue(recordContext, tx, recordSubspace, keyTuple, chunk, hasVersionInKey)
			if err != nil {
				return err
			}
			if sizes != nil {
				sizes.KeyCount++
				sizes.KeySize += len(key)
				sizes.ValueSize += len(chunk)
			}

			splitIndex++
			offset = end
		}
		if sizes != nil {
			sizes.IsSplit = true
		}
		return nil
	}

	if !splitLongRecords && omitUnsplitRecordSuffix {
		// Legacy unsplit layout: the value lives at the bare key with no suffix.
		// Java does NOT clear the previous record here — it overwrites in place
		// (clearPreviousSplitRecord is skipped when previousSizeInfo is non-null and
		// not versioned-inline, which is always the case for a bare-key store, since
		// versions live in the separate subspace). The Set below overwrites the only
		// key this record can occupy. Matches SplitHelper.saveWithSplit's
		// `recordKey = key` branch.
		key, err := writeSplitValue(recordContext, tx, recordSubspace, primaryKey, serialized, hasVersionInKey)
		if err != nil {
			return err
		}
		if sizes != nil {
			sizes.KeyCount = 1
			sizes.KeySize = len(key)
			sizes.ValueSize = dataLen
			sizes.IsSplit = false
			sizes.VersionedInline = false
		}
		return nil
	}

	// Clear previous record data
	if !hasVersionInKey {
		clearPreviousRecord(recordContext, tx, recordSubspace, primaryKey, splitLongRecords, oldsizeInfo)
	}

	// Unsplit: single KV pair at suffix 0. Keep the complete-key path's
	// concatenated packing so ordinary record writes need no extra tuple slice.
	var key []byte
	if hasVersionInKey {
		key, err = writeSplitValue(recordContext, tx, recordSubspace, appendToTuple(primaryKey, unsplitRecord), serialized, true)
		if err != nil {
			return err
		}
	} else {
		key = tuple.PackConcatWithPrefix(recordSubspace.Bytes(), primaryKey, unsplitSuffix)
		tx.SetBytes(key, serialized)
	}
	if sizes != nil {
		sizes.KeyCount = 1
		sizes.KeySize = len(key)
		sizes.ValueSize = dataLen
		sizes.IsSplit = false
		sizes.VersionedInline = false
	}

	return nil
}

// writeSplitValue uses the same packed key for collision detection and deferred
// mutation storage. The four-byte versionstamp offset is included in Java's key
// size metric even though FDB does not persist it.
func writeSplitValue(recordContext *FDBRecordContext, tx fdb.WritableTransaction, ss subspace.Subspace, key tuple.Tuple, value []byte, incomplete bool) ([]byte, error) {
	if !incomplete {
		packed := ss.Pack(key)
		tx.SetBytes(packed, value)
		return packed, nil
	}
	packed, err := key.PackWithVersionstamp(ss.Bytes())
	if err != nil {
		return nil, fmt.Errorf("pack split record versionstamp: %w", err)
	}
	if previous := recordContext.AddVersionMutation(MutationTypeSetVersionstampedKey, packed, value); previous != nil {
		return nil, &RecordCoreInternalError{Message: "Key with version overwritten"}
	}
	return packed, nil
}

// clearPreviousRecord clears the old record's KV pairs before saving a new version.
// If oldsizeInfo indicates the previous record was split, clears the split range.
// Otherwise, clears just the unsplit key.
// Matches Java's SplitHelper behavior with clearBasedOnPrevioussizeInfo.
func clearPreviousRecord(
	recordContext *FDBRecordContext,
	tx fdb.WritableTransaction,
	recordSubspace subspace.Subspace,
	primaryKey tuple.Tuple,
	splitLongRecords bool,
	oldsizeInfo *sizeInfo,
) {
	// A later save without a version must not commit a previous save's
	// deferred version mutation. Java leaves the local-version cache intact.
	if recordContext != nil {
		versionKey := recordSubspace.Pack(appendToTuple(primaryKey, recordVersionSuffix))
		if _, ok := recordContext.GetLocalVersion(versionKey); ok {
			recordContext.RemoveVersionMutation(versionKey)
		}
	}
	if oldsizeInfo == nil {
		// No previous record — nothing to clear
		return
	}

	if oldsizeInfo.IsSplit || splitLongRecords {
		// Clear the entire primary key range (covers all suffixes: -1, 0, 1, 2, ...)
		// This is safe because the range only covers this primary key's data.
		clearRecordKeyRange(tx, recordSubspace, primaryKey)
	} else {
		// Only unsplit key exists — clear just that
		keyTuple := appendToTuple(primaryKey, unsplitRecord)
		tx.Clear(recordSubspace.Pack(keyTuple))

		// Also clear version key if it was stored inline
		if oldsizeInfo.VersionedInline {
			versionKeyTuple := appendToTuple(primaryKey, recordVersionSuffix)
			tx.Clear(recordSubspace.Pack(versionKeyTuple))
		}
	}
}

// loadWithSplit loads the entire record range, as Java's SingleKeyUnsplitter
// does, except for the legacy bare-key layout, which remains a point read.
func loadWithSplit(
	tx fdb.ReadTransaction,
	recordSubspace subspace.Subspace,
	primaryKey tuple.Tuple,
	splitLongRecords bool,
	omitUnsplitRecordSuffix bool,
	sizes *sizeInfo,
) ([]byte, error) {
	p := startLoadWithSplit(tx, recordSubspace, primaryKey, splitLongRecords, omitUnsplitRecordSuffix, nil)
	value, _, err := p.finish(sizes)
	return value, err
}

// pendingRecordLoad starts the range before waiting, preserving record-fetch
// and old-record-read pipelining. Inline versions are part of that same read.
type pendingRecordLoad struct {
	primaryKey tuple.Tuple
	prefix     []byte
	value      fdb.FutureByteSlice // legacy bare-key layout only
	rangeRead  fdb.RangeResult
	version    *FDBRecordVersion
	localSize  sizeInfo
	localErr   error
	asyncRange *pendingRecordRange
}

// RangeResult has no readiness operation. The worker owns raw reassembly and
// validation, stopping at the first corrupt key without waiting for later batches.
// Store deserialization, limit accounting and timers remain on the caller.
type pendingRecordRange struct {
	done    chan struct{}
	value   []byte
	version *FDBRecordVersion
	sizes   sizeInfo
	err     error
}

func (p *pendingRecordLoad) startReadiness() {
	if p.rangeRead == nil {
		return
	}
	r := &pendingRecordRange{done: make(chan struct{})}
	p.asyncRange = r
	// Copy the already-captured local version inputs; never access context state
	// from the worker. Closing done publishes its exclusively owned result.
	load := *p
	go func() {
		r.value, r.version, r.err = load.readRange(&r.sizes)
		close(r.done)
	}()
}

func (p *pendingRecordLoad) ready() bool {
	if p.value != nil {
		return p.value.IsReady()
	}
	select {
	case <-p.asyncRange.done:
		return true
	default:
		return false
	}
}

func recordRangeBounds(prefix []byte) (begin, end []byte) {
	n := len(prefix) + 1
	buf := make([]byte, 2*n)
	// Full capacities isolate endpoint appends/reslices. Retaining either
	// endpoint keeps the shared storage for both alive.
	begin, end = buf[:n:n], buf[n:2*n:2*n]
	copy(begin, prefix)
	copy(end, prefix)
	end[n-1] = 0xff
	return begin, end
}

func startLoadWithSplit(
	tx fdb.ReadTransaction,
	recordSubspace subspace.Subspace,
	primaryKey tuple.Tuple,
	splitLongRecords bool,
	omitUnsplitRecordSuffix bool,
	recordContext *FDBRecordContext,
) pendingRecordLoad {
	p := pendingRecordLoad{primaryKey: primaryKey, prefix: recordSubspace.Pack(primaryKey)}
	if !splitLongRecords && omitUnsplitRecordSuffix {
		p.value = tx.Get(fdb.Key(p.prefix))
		return p
	}
	// Subspace.range(), not PrefixRange: exclude the bare key and pk+ff.
	begin, end := recordRangeBounds(p.prefix)
	p.rangeRead = tx.GetRange(fdb.KeyRange{Begin: fdb.Key(begin), End: fdb.Key(end)}, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll})
	if recordContext != nil {
		versionKey := recordSubspace.Pack(appendToTuple(primaryKey, recordVersionSuffix))
		if local, ok := recordContext.GetLocalVersion(versionKey); ok {
			p.version, p.localErr = IncompleteVersion(local)
			p.localSize = sizeInfo{KeyCount: 1, KeySize: len(versionKey), ValueSize: VersionBytes + 1, VersionedInline: true}
		}
	}
	return p
}

func (p *pendingRecordLoad) finish(sizes *sizeInfo) ([]byte, *FDBRecordVersion, error) {
	if sizes == nil {
		sizes = &sizeInfo{}
	}
	if p.value != nil {
		value, err := p.value.Get()
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get legacy unsplit record: %w", err)
		}
		if value != nil {
			*sizes = sizeInfo{KeyCount: 1, KeySize: len(p.prefix), ValueSize: len(value)}
		}
		return value, nil, nil
	}
	if p.asyncRange != nil {
		<-p.asyncRange.done
		*sizes = p.asyncRange.sizes
		return p.asyncRange.value, p.asyncRange.version, p.asyncRange.err
	}
	return p.readRange(sizes)
}

func (p *pendingRecordLoad) readRange(sizes *sizeInfo) ([]byte, *FDBRecordVersion, error) {
	*sizes = p.localSize
	if p.localErr != nil {
		return nil, nil, p.localErr
	}
	var result []byte
	version := p.version
	var lastIndex int64
	iter := p.rangeRead.Iterator()
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read record range: %w", err)
		}
		subkey, err := tuplefast.SubspaceUnpack(kv.Key, len(p.prefix))
		if err != nil {
			return nil, nil, &RecordCoreArgumentError{Message: fmt.Sprintf("unable to unpack split key for %v: %v", p.primaryKey, err)}
		}
		if len(subkey) != 1 {
			return nil, nil, &RecordCoreError{Message: fmt.Sprintf("Expected only a single key extension for split record: %v", p.primaryKey)}
		}
		index, ok := subkey[0].(int64)
		if !ok {
			return nil, nil, &RecordCoreArgumentError{Message: fmt.Sprintf("split record suffix is not an integer: %v", subkey)}
		}
		switch {
		case index == unsplitRecord:
			if result != nil {
				return nil, nil, &RecordCoreError{Message: fmt.Sprintf("More than one unsplit value: %v", p.primaryKey)}
			}
			result = kv.Value
			sizes.IsSplit = false
		case index == lastIndex+1 || (lastIndex == recordVersionSuffix && index == startSplitRecord):
			if index == startSplitRecord {
				if result != nil {
					return nil, nil, &RecordCoreError{Message: fmt.Sprintf("Unsplit value followed by split: %v", p.primaryKey)}
				}
				result = kv.Value
				sizes.IsSplit = true
			} else {
				result = append(result, kv.Value...)
			}
			lastIndex = index
		case index == recordVersionSuffix:
			version, err = unpackVersion(kv.Value)
			if err != nil {
				return nil, nil, err
			}
			sizes.VersionedInline = true
		default:
			if lastIndex >= startSplitRecord {
				return nil, nil, &FoundSplitOutOfOrderError{Expected: lastIndex + 1, Found: index, KeyTuple: p.primaryKey}
			}
			return nil, nil, &FoundSplitWithoutStartError{NextIndex: index, KeyTuple: p.primaryKey}
		}
		sizes.KeyCount++
		sizes.KeySize += len(kv.Key)
		sizes.ValueSize += len(kv.Value)
	}
	if _, err := iter.Get(); err != nil {
		return nil, nil, fmt.Errorf("failed to read record range: %w", err)
	}
	if result == nil && version != nil {
		return nil, nil, &FoundSplitWithoutStartError{NextIndex: recordVersionSuffix, KeyTuple: p.primaryKey}
	}
	return result, version, nil
}

// deleteSplit deletes a record that may be split across multiple KV pairs.
// Returns true if a record was deleted.
// Matches Java's SplitHelper.deleteSplit().
func deleteSplit(
	tx fdb.WritableTransaction,
	recordSubspace subspace.Subspace,
	primaryKey tuple.Tuple,
	splitLongRecords bool,
	omitUnsplitRecordSuffix bool,
	oldsizeInfo *sizeInfo,
) bool {
	if len(primaryKey) == 0 {
		return false
	}

	if oldsizeInfo == nil {
		return false
	}

	if !splitLongRecords && omitUnsplitRecordSuffix {
		// Legacy unsplit layout: clear the bare key. Matches Java's deleteSplit
		// `context.ensureActive().clear(subspace.pack(key))`.
		tx.Clear(recordSubspace.Pack(primaryKey))
		return true
	}

	if oldsizeInfo.IsSplit || splitLongRecords {
		// Clear the entire primary key range
		clearRecordKeyRange(tx, recordSubspace, primaryKey)
	} else {
		// Clear unsplit key only
		keyTuple := appendToTuple(primaryKey, unsplitRecord)
		tx.Clear(recordSubspace.Pack(keyTuple))

		// Clear inline version if present
		if oldsizeInfo.VersionedInline {
			versionKeyTuple := appendToTuple(primaryKey, recordVersionSuffix)
			tx.Clear(recordSubspace.Pack(versionKeyTuple))
		}
	}

	return true
}

// recordExistsWithSplit checks if a record exists, handling both split and unsplit formats.
func recordExistsWithSplit(
	tx fdb.ReadTransaction,
	recordSubspace subspace.Subspace,
	primaryKey tuple.Tuple,
	splitLongRecords bool,
	omitUnsplitRecordSuffix bool,
) (bool, error) {
	if !splitLongRecords && omitUnsplitRecordSuffix {
		// Legacy unsplit layout: existence is the bare key. Matches Java's
		// SplitHelper.keyExists -> loadUnsplitLegacy(...).thenApply(Objects::nonNull).
		value, err := tx.Get(fdb.Key(recordSubspace.Pack(primaryKey))).Get()
		if err != nil {
			return false, fmt.Errorf("failed to check legacy record existence: %w", err)
		}
		return value != nil, nil
	}

	// Java keyExists uses Range.startsWith, including the bare key, and stops
	// after one physical key. It does not validate or load the whole record.
	r, err := fdb.PrefixRange(recordSubspace.Pack(primaryKey))
	if err != nil {
		return false, fmt.Errorf("record existence range: %w", err)
	}
	iter := tx.GetRange(r, fdb.RangeOptions{Limit: 1}).Iterator()
	found := iter.Advance()
	if _, err := iter.Get(); err != nil {
		return false, fmt.Errorf("failed to check record existence: %w", err)
	}
	return found, nil
}

// clearRecordKeyRange clears all keys for a primary key (version, unsplit, and all split chunks).
// Empty primaryKey is a no-op to prevent catastrophic data loss (would clear entire records subspace).
func clearRecordKeyRange(tx fdb.WritableTransaction, recordSubspace subspace.Subspace, primaryKey tuple.Tuple) {
	if len(primaryKey) == 0 {
		return
	}
	pkSubspace := recordSubspace.Sub(primaryKey...)
	begin, end := pkSubspace.FDBRangeKeys()
	tx.ClearRange(fdb.KeyRange{Begin: begin, End: end})
}

// appendToTuple creates a new tuple with the suffix appended (safe copy, no aliasing).
func appendToTuple(base tuple.Tuple, suffix int64) tuple.Tuple {
	result := make(tuple.Tuple, len(base)+1)
	copy(result, base)
	result[len(base)] = suffix
	return result
}

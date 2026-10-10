// Portions derived from FoundationDB Record Layer (VectorIndexMaintainer.java,
// IndexEntry.java, ListCursor.java, StandardIndexMaintainer.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Copyright 2023 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package vectorindex

import (
	"context"
	"encoding/binary"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/internal/tuplefast"
	"fdb.dev/pkg/recordlayer/vectorcodec"
	"google.golang.org/protobuf/proto"
)

// vectorIndexMaintainer maintains a VECTOR index using an HNSW graph.
// Wire-compatible with Java's VectorIndexMaintainer.
//
// Prefix partitioning: when the index uses a KeyWithValueExpression with
// splitPoint > 0 (e.g., KWV(Concat(Field("group"), Field("vec")), 1)),
// each unique prefix (the key portion) gets an independent HNSW graph
// stored under hnswSubspace.Sub(prefix...). This matches Java's behavior
// where grouped key expressions produce per-prefix HNSW graphs.
type vectorIndexMaintainer struct {
	recordlayer.StandardIndexMaintainer
	hnswSubspace subspace.Subspace
	hnswConfig   HNSWConfig

	// A GUARDIANN index keeps its own configuration, and its deferred-task
	// counts and merge lock in the index's secondary subspace.
	engine            VectorEngineKind
	guardiannConfig   guardiannConfig
	secondarySubspace subspace.Subspace
	taskCounts        vectorTaskCounts
}

func newVectorIndexMaintainer(
	index *recordlayer.Index,
	indexSubspace, hnswSubspace, secondarySubspace subspace.Subspace,
	tx fdb.WritableTransaction,
	store *recordlayer.FDBRecordStore,
) (*vectorIndexMaintainer, error) {
	// The configuration as Java's parseConfig reads it, Config's checks
	// included: a configuration Java refuses is refused here rather than built
	// with Go's own reading (for example m > mMax, a new node selecting more
	// neighbours than the pruning cap, or efRepair < m).
	engine, err := VectorEngineOf(index)
	if err != nil {
		return nil, err
	}
	if engine == VectorEngineGuardiann {
		gc, err := parseGuardiannConfig(index)
		if err != nil {
			return nil, fmt.Errorf("vector index %q: %w", index.Name, err)
		}
		return &vectorIndexMaintainer{
			StandardIndexMaintainer: *recordlayer.NewStandardIndexMaintainer(recordlayer.IndexMaintainerState{Index: index, IndexSubspace: indexSubspace, Transaction: tx, Store: store}),
			hnswSubspace:            hnswSubspace,
			engine:                  engine,
			guardiannConfig:         gc,
			secondarySubspace:       secondarySubspace,
			taskCounts:              newVectorTaskCounts(secondarySubspace),
		}, nil
	}
	config, err := parseHNSWConfig(index)
	if err != nil {
		return nil, fmt.Errorf("vector index %q: %w", index.Name, err)
	}
	return &vectorIndexMaintainer{
		StandardIndexMaintainer: *recordlayer.NewStandardIndexMaintainer(recordlayer.IndexMaintainerState{Index: index, IndexSubspace: indexSubspace, Transaction: tx, Store: store}),
		hnswSubspace:            hnswSubspace,
		hnswConfig:              config,
	}, nil
}

// HNSWConfigOf is the HNSW configuration a VECTOR index's options declare, as
// the maintainer reads it (parseHNSWConfig): Java's
// HnswVectorIndexEngine.parseConfig, with Go's forms for a plain VECTOR index.
func HNSWConfigOf(index *recordlayer.Index) (HNSWConfig, error) { return parseHNSWConfig(index) }

// getSubspaceForPrefix returns the HNSW subspace scoped to the given prefix.
// If the prefix is empty (no grouping), returns the base hnswSubspace.
// Each unique prefix value gets its own independent HNSW graph.
func (m *vectorIndexMaintainer) getSubspaceForPrefix(prefix tuple.Tuple) subspace.Subspace {
	if len(prefix) == 0 {
		return m.hnswSubspace
	}
	// Convert tuple elements to []TupleElement for Sub() variadic call.
	args := make([]tuple.TupleElement, len(prefix))
	for i, v := range prefix {
		args[i] = v
	}
	return m.hnswSubspace.Sub(args...)
}

// numDimensions is the index's vector width under either engine.
func (m *vectorIndexMaintainer) numDimensions() int {
	if m.engine == VectorEngineGuardiann {
		return m.guardiannConfig.numDimensions
	}
	return m.hnswConfig.NumDimensions
}

// getStorageForPrefix returns a fresh hnswStorage for the given prefix
// subspace: its parsed-node cache lives for ONE engine operation, as Java's
// HNSW node caches are created per insert / delete / search. A cache shared
// across operations would let a node a SNAPSHOT search fetched (no read
// conflict) serve a later serializable insert in the same transaction, which
// then commits without the conflict that read owes (RFC-257 WS-D section 1).
// FDB's read-your-writes cache still serves repeated reads in a transaction.
func (m *vectorIndexMaintainer) getStorageForPrefix(prefix tuple.Tuple) *hnswStorage {
	storage := newHNSWStorage(m.getSubspaceForPrefix(prefix), m.hnswConfig)
	storage.env = m.Store().Env()
	storage.timer = m.timer()
	return storage
}

// splitPrefixAndVector extracts the prefix (grouping key) and vector from an
// index entry. For KeyWithValueExpression indexes:
//   - entry.key = prefix columns (the key portion before splitPoint)
//   - entry.value = vector data (the value portion after splitPoint)
//
// For non-KWV indexes (e.g., Concat(Field("x"), Field("y"))):
//   - entry.key = the entire vector (no prefix)
//   - entry.value = nil
//
// NOTE: Java divergence — Java's VectorIndexMaintainer.getKeyWithValueExpression()
// at line 375 throws if the root expression is not a KeyWithValueExpression.
// We accept non-KWV expressions for backwards compatibility with existing tests
// that use Concat/Field directly as the root expression. This is more permissive
// than Java but functionally equivalent for non-grouped vector indexes.
func (m *vectorIndexMaintainer) splitPrefixAndVector(entry recordlayer.EvaluatedIndexEntry) (prefix tuple.Tuple, vector []float64, err error) {
	if len(entry.Value()) > 0 {
		// KeyWithValue index: key is prefix, value is vector.
		vec, verr := tupleToVector(entry.Value())
		return entry.Key(), vec, verr
	}
	// Non-KWV index: no prefix, entire key is the vector.
	vec, verr := tupleToVector(entry.Key())
	return nil, vec, verr
}

// vectorTypeOfEntry is the VectorType ordinal of an entry's serialized vector;
// a vector spelled as numeric tuple elements is DOUBLE.
func vectorTypeOfEntry(entry recordlayer.EvaluatedIndexEntry) byte {
	t := entry.Key()
	if len(entry.Value()) > 0 {
		t = entry.Value()
	}
	if len(t) == 1 {
		if b, ok := t[0].([]byte); ok && len(b) > 0 && b[0] <= vectorcodec.TypeDouble {
			return b[0]
		}
	}
	return vectorcodec.TypeDouble
}

// Update handles insert/delete/update for the VECTOR index.
// When the index has a prefix (via KeyWithValueExpression), each unique prefix
// value gets its own independent HNSW graph stored at a separate subspace.
//
// Primary keys are trimmed via Index.TrimPrimaryKey() before storing in the HNSW
// graph, matching Java's VectorIndexMaintainer.updateIndexKeys() which calls
// state.index.trimPrimaryKey(primaryKeyParts) at line 343.
//
// An entry the old and the new record both have (key and value equal, Java's
// IndexEntry.equals) is not applied: Java's VectorIndexMaintainer inherits
// StandardIndexMaintainer.update, which removes those common entries before
// updating (StandardIndexMaintainer.java:215-228, skipUpdateForUnchangedKeys),
// so a save that leaves the vector unchanged makes no graph call. Deleting and
// re-inserting such a node rewired its edges, could move the entry point and
// re-sampled the statistics, where Java's graph is untouched. That holds for a
// save this method applies with both records; two paths still delete and
// re-insert an unchanged node, in both engines: a save queued for a
// WRITE_ONLY_WITH_QUEUE index (SerializePendingWriteQueue sends both entries,
// and the replay deletes then inserts) and a windowed index's delegate, which
// the sliding window calls once with the old record and once with the new.
func (m *vectorIndexMaintainer) Update(oldRecord, newRecord *recordlayer.FDBStoredRecord[proto.Message]) error {
	var entries [2][]recordlayer.EvaluatedIndexEntry
	for i, record := range []*recordlayer.FDBStoredRecord[proto.Message]{oldRecord, newRecord} {
		if record == nil {
			continue
		}
		evaluated, err := m.FilteredIndexEntries(record)
		if err != nil {
			which := "old"
			if i == 1 {
				which = "new"
			}
			return fmt.Errorf("evaluate vector index %q for %s record: %w", m.Index().Name, which, err)
		}
		entries[i] = evaluated
	}
	if oldRecord != nil && newRecord != nil {
		oldEntries, newEntries, err := recordlayer.RemoveCommonEntries(m.Index(), entries[0], entries[1])
		if err != nil {
			return err
		}
		entries[0], entries[1] = oldEntries, newEntries
	}
	for i, list := range entries {
		for _, entry := range list {
			if err := m.applyIndexEntry(entry, i == 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// withPrefixWriteLock runs fn against the prefix's HNSW graph while holding the
// per-prefix write lock — the same scope as Java's
// doWithWriteLock(LockIdentifier(indexSubspace.subspace(prefixKey))). For an
// unprefixed index this is the index subspace itself.
func (m *vectorIndexMaintainer) withPrefixWriteLock(prefix tuple.Tuple, fn func(*hnswGraph) error) error {
	lockKey := string(m.getSubspaceForPrefix(prefix).Bytes())
	m.Store().AcquireWriteLock(lockKey)
	defer m.Store().ReleaseWriteLock(lockKey)
	storage := m.getStorageForPrefix(prefix)
	return fn(NewHNSWGraph(storage, m.hnswConfig))
}

// tupleToVector converts tuple elements to a float64 vector. Returns:
//   - (nil, nil)   for an absent/null vector — an empty tuple or a null component —
//     which the caller skips (matches Java, which skips a null vector field);
//   - (nil, error) for a NON-null but UNDECODABLE vector (bad serialized bytes,
//     non-numeric element). Java's RealVector.fromBytes throws here and fails the
//     write; Go previously returned nil → the maintainer silently skipped it,
//     saving the record UNINDEXED (a vector search would miss the row). Surfacing
//     the error makes the write fail, matching Java and avoiding silent index
//     incompleteness.
//   - (vec, nil)   for a valid vector.
//
// Handles both raw bytes (KeyWithValueExpression on a bytes field) and numeric
// tuple elements (expressions on int/float fields).
func tupleToVector(t tuple.Tuple) ([]float64, error) {
	if len(t) == 0 {
		return nil, nil // absent vector — skip
	}
	// Single bytes element: a serialized vector (KeyWithValueExpression(field, 0)
	// on a bytes proto field). A non-null but undecodable payload is an error.
	if len(t) == 1 {
		if b, ok := t[0].([]byte); ok {
			vec, err := deserializeVector(b)
			if err != nil {
				return nil, fmt.Errorf("vector index: undecodable serialized vector: %w", err)
			}
			return vec, nil
		}
	}
	vec := make([]float64, 0, len(t))
	for _, elem := range t {
		switch v := elem.(type) {
		case nil:
			// A null component → treat the whole vector as absent (skip), not a
			// partial/undefined vector. Matches Java's null-vector handling.
			return nil, nil
		case []byte:
			deserialized, err := deserializeVector(v)
			if err != nil {
				return nil, fmt.Errorf("vector index: undecodable serialized vector element: %w", err)
			}
			vec = append(vec, deserialized...)
		case float64:
			vec = append(vec, v)
		case float32:
			vec = append(vec, float64(v))
		case int64:
			vec = append(vec, float64(v))
		case int:
			vec = append(vec, float64(v))
		default:
			return nil, fmt.Errorf("vector index: non-numeric element %T in vector key", elem)
		}
	}
	return vec, nil
}

// UpdateWhileWriteOnly handles updates during WRITE_ONLY state as Update does:
// Java's vector index is idempotent (StandardIndexMaintainer.isIdempotent), so
// a write-only save updates the graph directly, and a build that later meets
// the indexed record finds its node present and leaves it (hnswGraph.Insert).
func (m *vectorIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *recordlayer.FDBStoredRecord[proto.Message]) error {
	return m.Update(oldRecord, newRecord)
}

// Scan rejects the TupleRange-based scan API for VECTOR indexes.
// Matches Java's VectorIndexMaintainer.scan(IndexScanType, TupleRange, ...) which
// throws IllegalStateException("index maintainer does not support this scan api").
// Use ScanByDistance for kNN search, or SearchKNN for direct results.
func (m *vectorIndexMaintainer) Scan(
	scanRange recordlayer.TupleRange,
	continuation []byte,
	scanProperties recordlayer.ScanProperties,
) recordlayer.RecordCursor[*recordlayer.IndexEntry] {
	return recordlayer.NewErrorCursor[*recordlayer.IndexEntry](fmt.Errorf("VECTOR index %q does not support TupleRange scan; use ScanVectorIndex with BY_DISTANCE", m.Index().Name))
}

// VectorScanBounds carries the parameters for a BY_DISTANCE kNN scan.
// Matches Java's VectorIndexScanBounds (query vector, k limit, efSearch, options).
type VectorScanBounds struct {
	QueryVector []float64 // The query vector for similarity search.
	K           int       // Number of nearest neighbors to return.
	EfSearch    int       // Search exploration factor (0 = auto from K).
}

// ScanByDistance performs a kNN search and returns results as a cursor of IndexEntry.
// Each IndexEntry has Key = primaryKey and Value = tuple{distance}.
// Matches Java's VectorIndexMaintainer.scan(VectorIndexScanBounds, ...) which
// returns a ListCursor of IndexEntry from kNearestNeighborsSearch.
//
// For prefix-partitioned indexes, the prefix is encoded as additional elements
// at the end of TupleRange.Low (after the query vector bytes).
func (m *vectorIndexMaintainer) ScanByDistance(
	scanRange recordlayer.TupleRange,
	continuation []byte,
	scanProperties recordlayer.ScanProperties,
) recordlayer.RecordCursor[*recordlayer.IndexEntry] {
	// Extract VectorScanBounds from TupleRange.
	// Convention: Low = tuple{queryVectorBytes, prefix...} (serialized vector as []byte, followed by optional prefix elements),
	//             High = tuple{k, efSearch} (int64 values).
	if scanRange.Low == nil || len(scanRange.Low) < 1 {
		return recordlayer.NewErrorCursor[*recordlayer.IndexEntry](fmt.Errorf("VECTOR BY_DISTANCE scan requires query vector in TupleRange.Low"))
	}

	vecBytes, ok := scanRange.Low[0].([]byte)
	if !ok {
		return recordlayer.NewErrorCursor[*recordlayer.IndexEntry](fmt.Errorf("VECTOR BY_DISTANCE scan: TupleRange.Low[0] must be []byte (serialized query vector)"))
	}

	queryVector, err := deserializeVector(vecBytes)
	if err != nil {
		return recordlayer.NewErrorCursor[*recordlayer.IndexEntry](fmt.Errorf("VECTOR BY_DISTANCE scan: invalid query vector: %w", err))
	}

	// Extract optional prefix from remaining Low elements.
	var prefix tuple.Tuple
	if len(scanRange.Low) > 1 {
		prefix = tuple.Tuple(scanRange.Low[1:])
	}

	k := 10 // default
	efSearch := 0
	if scanRange.High != nil {
		if len(scanRange.High) >= 1 {
			if kVal, ok := asInt64(scanRange.High[0]); ok && kVal > 0 {
				k = int(kVal)
			}
		}
		if len(scanRange.High) >= 2 {
			if efVal, ok := asInt64(scanRange.High[1]); ok && efVal > 0 {
				efSearch = int(efVal)
			}
		}
	}

	// Multi-partition fan-out (partial prefix) is dispatched inside
	// ScanVectorIndex — the shared chokepoint for both this entry point
	// and ScanVectorIndexWithPrefix — so it is not branched here.
	return m.ScanVectorIndex(prefix, queryVector, k, recordlayer.VectorIndexScanOptions{EfSearch: positiveEfSearch(efSearch)}, continuation, scanProperties)
}

// partitionSize returns the number of leading partition (key) columns of the
// vector index — the KeyWithValueExpression split point — or 0 if the index is
// not a KeyWithValueExpression (unpartitioned). Mirrors Java's
// prefixSize = getKeyWithValueExpression(rootExpression).getSplitPoint().
func (m *vectorIndexMaintainer) partitionSize() int {
	if kwv, ok := m.Index().RootExpression.(*recordlayer.KeyWithValueExpression); ok {
		return kwv.SplitPoint()
	}
	return 0
}

// ScanVectorIndex performs the actual kNN search and returns a cursor.
// prefix scopes the search to a specific prefix partition (nil for no prefix).
//
// Each IndexEntry matches Java's VectorIndexMaintainer.toIndexEntry():
//   - Key = (prefix..., trimmedPK...) — prefix prepended to the PK from HNSW
//   - Value = (vectorRawBytes) or (nil) — the vector data, or nil for RaBitQ
//
// Supports continuation-based pagination via VectorIndexScanContinuation protobuf,
// matching Java's continuation format.
func (m *vectorIndexMaintainer) ScanVectorIndex(
	prefix tuple.Tuple,
	queryVector []float64,
	k int,
	opts recordlayer.VectorIndexScanOptions,
	continuation []byte,
	scanProperties recordlayer.ScanProperties,
) recordlayer.RecordCursor[*recordlayer.IndexEntry] {
	// Multi-partition fan-out: when the index is partitioned (splitPoint > 0) but
	// the bound prefix is shorter than the full partition prefix, scan every
	// matching partition (prefix skip-scan + per-partition HNSW search), matching
	// Java's VectorIndexMaintainer.scan flatMapPipelined(prefixSkipScan,
	// scanSinglePartition) (RFC-046). Sited at this chokepoint so BOTH entry
	// points — ScanByDistance (executor) and ScanVectorIndexWithPrefix (direct
	// API) — fan out on a partial prefix instead of scanning one wrong subspace.
	if pSize := m.partitionSize(); pSize > 0 && len(prefix) < pSize {
		return m.newVectorMultiPartitionCursor(prefix, queryVector, k, opts, pSize, continuation, scanProperties)
	}

	// A continuation carries the whole materialized page: Java's
	// scanSinglePartition replays it with no search and no lock, so a resumed
	// scan reads nothing of the graph.
	var entries []*recordlayer.IndexEntry
	if len(continuation) == 0 {
		var err error
		if entries, err = m.searchOnePartition(m.ReadTransaction(scanProperties), prefix, queryVector, k, opts); err != nil {
			return recordlayer.NewErrorCursor[*recordlayer.IndexEntry](err)
		}
	}
	cursor, err := m.newVectorSearchCursor(entries, continuation, prefix)
	if err != nil {
		return recordlayer.NewErrorCursor[*recordlayer.IndexEntry](err)
	}
	return cursor
}

// searchOnePartition runs one HNSW kNN search for the single partition
// identified by the FULL partition prefix (nil/empty for an unpartitioned
// index) and returns the top-k entries in Java's toIndexEntry layout
// (Key = prefix...+trimmedPK, Value = vector bytes or nil). It is both the body of the
// single-partition scan and the per-partition inner of the multi-partition
// fan-out (RFC-046). Mirrors Java's VectorIndexMaintainer.kNearestNeighborSearch
// + toIndexEntry.
func (m *vectorIndexMaintainer) searchOnePartition(readTx fdb.ReadTransaction, prefix tuple.Tuple, queryVector []float64, k int, opts recordlayer.VectorIndexScanOptions) ([]*recordlayer.IndexEntry, error) {
	// Java's scan read-locks LockIdentifier(partitionSubspace), the key a
	// write to this partition write-locks (withPrefixWriteLock). The lock
	// covers the search that materializes the page, not the cursor's life
	// (RFC-257 WS-D section 1, declared in DIVERGENCES).
	lockKey := string(m.getSubspaceForPrefix(prefix).Bytes())
	m.Store().AcquireReadLock(lockKey)
	defer m.Store().ReleaseReadLock(lockKey)
	if m.engine == VectorEngineGuardiann {
		results, err := m.searchGuardiann(readTx, prefix, queryVector, k, opts)
		if err != nil {
			return nil, err
		}
		entries := make([]*recordlayer.IndexEntry, len(results))
		for i, r := range results {
			key := append(append(tuple.Tuple{}, prefix...), r.primaryKey...)
			value := tuple.Tuple{nil}
			if opts.ReturnVectors != nil && *opts.ReturnVectors || opts.ReturnVectors == nil && !m.guardiannConfig.useRaBitQ {
				value[0] = r.vector.encode()
			}
			entries[i] = recordlayer.NewIndexEntry(m.Index(), key, value, m.entryFullPK(key, prefix))
		}
		return entries, nil
	}
	if len(queryVector) != m.hnswConfig.NumDimensions {
		return nil, fmt.Errorf("VECTOR index %q expects %d dimensions, but query vector has %d",
			m.Index().Name, m.hnswConfig.NumDimensions, len(queryVector))
	}
	storage := m.getStorageForPrefix(prefix)
	graph := NewHNSWGraph(storage, m.hnswConfig)

	includeVectors := m.hnswConfig.Quantizer == nil
	if opts.ReturnVectors != nil {
		includeVectors = *opts.ReturnVectors
	}
	results, err := graph.searchWithVectors(readTx, queryVector, k, recordlayer.HNSWEfSearch(opts.EfSearch, k), includeVectors)
	if err != nil {
		return nil, err
	}

	// Convert to IndexEntry slice matching Java's toIndexEntry() format:
	// Key = (prefix..., trimmedPK...), Value = (vectorRawData | nil)
	entries := make([]*recordlayer.IndexEntry, len(results))
	for i, r := range results {
		// Build key: prepend prefix to the PK (which is already trimmed in HNSW).
		key := make(tuple.Tuple, 0, len(prefix)+len(r.PrimaryKey))
		key = append(key, prefix...)
		key = append(key, r.PrimaryKey...)

		value := tuple.Tuple{nil}
		if includeVectors {
			value[0] = r.Vector
		}

		entries[i] = recordlayer.NewIndexEntry(m.Index(), key, value, m.entryFullPK(key, prefix))
	}
	return entries, nil
}

// entryFullPK reconstructs and pins the full primary key for a vector index
// entry from its key alone. IndexEntry.PrimaryKey()'s default getEntryPrimaryKey
// assumes the value-index key layout (indexValues[colSize] + pk) and mis-extracts
// a vector entry's key (prefix + trimmedPK), so the PK must be set explicitly.
//
// Deriving from the key (not the HNSW search result) is what lets RESUMED entries
// — reconstructed from a continuation with no record in hand — pin the same PK as
// fresh entries. key == (prefix..., trimmedPK...), so the
// non-component-positions PK is the key with the partition prefix stripped, which
// equals the fresh path's hnswSearchResult.PrimaryKey.
func (m *vectorIndexMaintainer) entryFullPK(key, prefix tuple.Tuple) tuple.Tuple {
	if m.Index().HasPrimaryKeyComponentPositions() {
		return m.Index().EntryPrimaryKey(key)
	}
	if len(prefix) <= len(key) {
		return key[len(prefix):]
	}
	return key
}

// vectorSearchCursor is a cursor over vector search results that supports
// continuation tokens matching Java's VectorIndexScanContinuation protobuf.
//
// Java's approach: serialize ALL remaining result entries into the continuation.
// On resume, deserialize and replay from the saved list (no re-search needed).
// This matches VectorIndexMaintainer.java lines 182-197 (resume) and 516-528 (create).
type vectorSearchCursor struct {
	entries    []*recordlayer.IndexEntry
	allEntries []*recordlayer.IndexEntry // all entries for continuation encoding
	pos        int
	closed     bool
}

// newVectorSearchCursor creates a vector search cursor from search results.
// If continuation is non-nil, it is parsed as a VectorIndexScanContinuation
// protobuf (Java format). On resume, results are replayed from the continuation
// rather than re-searching. prefix is threaded through so resumed entries
// reconstruct the same pinned primary key as fresh ones.
//
// A continuation that fails to parse is an error, never a fresh restart: Java's
// VectorIndexMaintainer.Continuation.fromBytes throws
// RecordCoreException("error parsing continuation") and a silent restart would
// re-emit rows the caller already consumed.
func (m *vectorIndexMaintainer) newVectorSearchCursor(entries []*recordlayer.IndexEntry, continuation []byte, prefix tuple.Tuple) (*vectorSearchCursor, error) {
	if len(continuation) > 0 {
		// Resume from continuation: parse the proto and replay saved entries.
		resumed, innerPos, err := m.parseVectorScanContinuation(continuation, prefix)
		if err != nil {
			return nil, err
		}
		return &vectorSearchCursor{
			entries:    resumed,
			allEntries: resumed,
			pos:        innerPos,
		}, nil
	}
	return &vectorSearchCursor{
		entries:    entries,
		allEntries: entries,
		pos:        0,
	}, nil
}

func (c *vectorSearchCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[*recordlayer.IndexEntry], error) {
	// Honor a statement deadline / cancellation while emitting (RFC-106a). The
	// kNN search itself is bounded by k/efSearch, so this only guards the emit
	// loop; per-search cost is bounded by construction, not by a scan limit.
	if err := ctx.Err(); err != nil {
		return recordlayer.RecordCursorResult[*recordlayer.IndexEntry]{}, err
	}
	if c.closed || c.pos >= len(c.entries) {
		return recordlayer.NewResultNoNext[*recordlayer.IndexEntry](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
	}
	entry := c.entries[c.pos]
	c.pos++

	// Encode continuation matching Java's Continuation class.
	// Includes ALL entries (for replay on resume) + inner position.
	cont := encodeVectorScanContinuation(c.allEntries, c.pos)
	return recordlayer.NewResultWithValue(entry, recordlayer.NewBytesContinuation(cont)), nil
}

func (c *vectorSearchCursor) Close() error {
	c.closed = true
	return nil
}

func (c *vectorSearchCursor) IsClosed() bool { return c.closed }

// encodeVectorScanContinuation creates a VectorIndexScanContinuation protobuf.
// Matches Java's Continuation.toByteString() which serializes all entries +
// the inner ListCursor continuation.
func encodeVectorScanContinuation(entries []*recordlayer.IndexEntry, innerPos int) []byte {
	contProto := &gen.VectorIndexScanContinuation{}
	for _, e := range entries {
		contProto.IndexEntries = append(contProto.IndexEntries,
			&gen.VectorIndexScanContinuation_IndexEntry{
				Key:   e.Key.Pack(),
				Value: e.Value.Pack(),
			})
	}
	// Inner continuation: Java's ListCursor.Continuation.toBytes() is
	// ByteBuffer.allocate(Integer.BYTES).putInt(nextPosition) — a 4-byte
	// big-endian int, NOT a packed tuple. Wire compat with Java requires the
	// exact same encoding.
	inner := make([]byte, 4)
	binary.BigEndian.PutUint32(inner, uint32(innerPos))
	contProto.InnerContinuation = inner

	data, err := contProto.MarshalVT()
	if err != nil {
		return nil
	}
	return data
}

// parseVectorScanContinuation parses a VectorIndexScanContinuation protobuf.
// Returns the saved entries and the inner cursor position.
//
// A continuation that fails to parse is an error, never a fresh restart:
// Java's VectorIndexMaintainer.Continuation.fromBytes throws
// RecordCoreException("error parsing continuation"), and corrupt entry keys /
// inner positions fail Tuple.fromBytes / ByteBuffer.getInt inside
// scanSinglePartition — a silent restart would re-emit rows the caller
// already consumed.
//
// Resumed entries are reconstructed to be INDISTINGUISHABLE from fresh ones:
// Index and the pinned full primary key are restored —
// derived from the persisted key via entryFullPK — so IndexEntry.PrimaryKey()
// returns the correct key on a resumed page instead of an empty tuple (which
// would fetch the wrong record / skip the remaining nearest rows).
func (m *vectorIndexMaintainer) parseVectorScanContinuation(data []byte, prefix tuple.Tuple) ([]*recordlayer.IndexEntry, int, error) {
	var contProto gen.VectorIndexScanContinuation
	if err := recordlayer.UnmarshalVTAsJava(&contProto, data); err != nil {
		return nil, 0, &recordlayer.ContinuationParseError{RawBytes: data, Cause: err}
	}

	entries := make([]*recordlayer.IndexEntry, 0, len(contProto.IndexEntries))
	for i, ie := range contProto.IndexEntries {
		key, err := tuplefast.Unpack(ie.GetKey())
		if err != nil {
			return nil, 0, fmt.Errorf("VECTOR index %q continuation: entry %d key: %w", m.Index().Name, i, err)
		}
		value, err := tuplefast.Unpack(ie.GetValue())
		if err != nil {
			return nil, 0, fmt.Errorf("VECTOR index %q continuation: entry %d value: %w", m.Index().Name, i, err)
		}
		entries = append(entries, recordlayer.NewIndexEntry(m.Index(), key, value, m.entryFullPK(key, prefix)))
	}

	// Inner continuation: Java's ListCursor reads it via
	// ByteBuffer.wrap(continuation).getInt() — a 4-byte big-endian int; fewer
	// than 4 bytes throws BufferUnderflowException. Same strictness here.
	inner := contProto.GetInnerContinuation()
	if len(inner) < 4 {
		return nil, 0, fmt.Errorf("VECTOR index %q continuation: inner position has %d bytes, need 4", m.Index().Name, len(inner))
	}
	innerPos := int(int32(binary.BigEndian.Uint32(inner[:4])))
	if innerPos < 0 {
		return nil, 0, fmt.Errorf("VECTOR index %q continuation: negative inner position %d", m.Index().Name, innerPos)
	}

	return entries, innerPos, nil
}

// vectorMultiPartitionCursor fans a BY_DISTANCE scan out over all distinct
// partitions whose full partition prefix begins with partialPrefix, running one
// HNSW kNN search per partition and concatenating each partition's top-k. Ports
// Java's VectorIndexMaintainer.scan flatMapPipelined(prefixSkipScan,
// scanSinglePartition) for the partial-partition-prefix case (RFC-046).
//
// SQL semantics: ROW_NUMBER() OVER (PARTITION BY <keys> ...) <= k selects the
// top-k PER partition, so the union across partitions is intentionally unbounded
// — there is no global top-k re-merge. An outer SQL LIMIT, if present, rides in
// scanProperties.ExecuteProperties.ReturnedRowLimit and caps the TOTAL rows
// across partitions (Java's final skipThenLimit) — a quantity distinct from the
// per-partition k.
//
// Continuation is full cross-partition (Java-aligned): each delivered row emits
// FlatMapContinuation{OuterContinuation: pack(currentPrefix), InnerContinuation:
// <per-partition VectorIndexScanContinuation>}. On resume the saved partition is
// re-read first (its inner continuation replays the saved entries), then the
// skip-scan advances to the next distinct partition.
type vectorMultiPartitionCursor struct {
	m *vectorIndexMaintainer
	// scanProps carries the caller.s isolation to every partition this cursor
	// visits. Held as PROPERTIES, not as an already-resolved transaction:
	// resolving at construction would touch the store before the continuation
	// is even validated, and Java resolves inside scanSinglePartition — per
	// partition, at the point of the read — for the same reason.
	scanProps     recordlayer.ScanProperties
	queryVector   []float64
	k             int
	opts          recordlayer.VectorIndexScanOptions
	partialPrefix tuple.Tuple // the bound equality prefix (may be empty)
	partitionSize int

	// nextPartitionStart is the FDB key at/after which to look for the next
	// distinct partition prefix; rangeEnd bounds the skip-scan to partitions
	// under partialPrefix.
	nextPartitionStart fdb.Key
	rangeEnd           fdb.Key

	currentCursor *vectorSearchCursor
	currentPrefix tuple.Tuple
	// pendingInner seeds the first resumed partition's inner continuation.
	pendingInner []byte

	globalLimit      int
	totalDelivered   int
	lastContinuation []byte
	// errLatch pins a continuation-parse failure: once set, every OnNext
	// returns it, so a retried OnNext can never fall through to a fresh
	// search that would silently re-emit already-delivered rows.
	errLatch  error
	exhausted bool
	closed    bool
}

// newVectorMultiPartitionCursor builds the fan-out cursor and, when resuming,
// seeds the skip-scan to re-read the saved partition first.
func (m *vectorIndexMaintainer) newVectorMultiPartitionCursor(
	partialPrefix tuple.Tuple,
	queryVector []float64,
	k int,
	opts recordlayer.VectorIndexScanOptions,
	partitionSize int,
	continuation []byte,
	scanProperties recordlayer.ScanProperties,
) recordlayer.RecordCursor[*recordlayer.IndexEntry] {
	// Validate the query-vector dimension up front, before any partition is
	// matched. searchOnePartition checks this too, but only per matched
	// partition — so without this an invalid-length vector over a partial prefix
	// that matches NO partitions would return SourceExhausted instead of the
	// dimension error, unlike the full-prefix/unpartitioned paths which validate
	// before touching graph contents. Validate once here for
	// consistent input validation regardless of how many partitions match.
	if dims := m.numDimensions(); len(queryVector) != dims {
		return recordlayer.NewErrorCursor[*recordlayer.IndexEntry](fmt.Errorf("VECTOR index %q expects %d dimensions, but query vector has %d",
			m.Index().Name, dims, len(queryVector)))
	}

	// Enumeration subspace: partitions under partialPrefix (whole index if empty).
	base := m.getSubspaceForPrefix(partialPrefix)
	end, err := fdb.Strinc(base.Bytes())
	if err != nil {
		return recordlayer.NewErrorCursor[*recordlayer.IndexEntry](fmt.Errorf("VECTOR index %q multi-partition scan: %w", m.Index().Name, err))
	}

	c := &vectorMultiPartitionCursor{
		m:                  m,
		scanProps:          scanProperties,
		queryVector:        queryVector,
		k:                  k,
		opts:               opts,
		partialPrefix:      partialPrefix,
		partitionSize:      partitionSize,
		nextPartitionStart: fdb.Key(base.Bytes()),
		rangeEnd:           fdb.Key(end),
	}
	if scanProperties.ExecuteProperties.ReturnedRowLimit > 0 {
		c.globalLimit = scanProperties.ExecuteProperties.ReturnedRowLimit
	}

	// Resume: unpack the outer prefix and seed the skip-scan to re-read that
	// partition first (inclusive start), replaying its inner continuation. The
	// findNextPartition read then returns resumePrefix and advances past it, so
	// the next partition follows once the resumed one drains.
	//
	// A continuation that fails to parse is an error, never a fresh restart:
	// the Java analog (RecordCursor.flatMapPipelined over the partition
	// skip-scan) throws RecordCoreException("error parsing continuation"), and
	// a silent restart would re-emit rows the caller already consumed.
	if len(continuation) > 0 {
		var fm gen.FlatMapContinuation
		if uerr := recordlayer.UnmarshalVTAsJava(&fm, continuation); uerr != nil {
			return recordlayer.NewErrorCursor[*recordlayer.IndexEntry](&recordlayer.ContinuationParseError{RawBytes: continuation, Cause: uerr})
		}
		// OuterContinuation absent is a well-formed shape (Java flatMapPipelined:
		// no outer continuation → outer restarts from the beginning); this
		// cursor's own emitter always sets it, so only the corrupt-payload
		// cases below are reachable from Go-produced tokens.
		if fm.OuterContinuation != nil {
			resumePrefix, perr := tuplefast.Unpack(fm.OuterContinuation)
			if perr != nil {
				return recordlayer.NewErrorCursor[*recordlayer.IndexEntry](fmt.Errorf("VECTOR index %q multi-partition continuation: outer prefix: %w", m.Index().Name, perr))
			}
			c.nextPartitionStart = fdb.Key(m.getSubspaceForPrefix(resumePrefix).Bytes())
			c.pendingInner = fm.InnerContinuation
		}
	}
	return c
}

func (c *vectorMultiPartitionCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[*recordlayer.IndexEntry], error) {
	if c.errLatch != nil {
		return recordlayer.RecordCursorResult[*recordlayer.IndexEntry]{}, c.errLatch
	}
	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[*recordlayer.IndexEntry]{}, err
		}
		// Global row limit (outer SQL LIMIT) across all partitions. Return the
		// last emitted continuation (a valid mid-stream resume point), never an
		// end continuation (ReturnLimitReached must not be end).
		if c.globalLimit > 0 && c.totalDelivered >= c.globalLimit {
			cont := c.lastContinuation
			if len(cont) == 0 {
				cont = []byte{}
			}
			return recordlayer.NewResultNoNext[*recordlayer.IndexEntry](recordlayer.ReturnLimitReached, recordlayer.NewBytesContinuation(cont)), nil
		}

		// Drain the current partition's cursor.
		if c.currentCursor != nil {
			res, err := c.currentCursor.OnNext(ctx)
			if err != nil {
				return recordlayer.RecordCursorResult[*recordlayer.IndexEntry]{}, err
			}
			if res.HasNext() {
				c.totalDelivered++
				cont, werr := c.wrapContinuation(res.GetContinuation())
				if werr != nil {
					return recordlayer.RecordCursorResult[*recordlayer.IndexEntry]{}, werr
				}
				c.lastContinuation = cont
				return recordlayer.NewResultWithValue(res.GetValue(), recordlayer.NewBytesContinuation(cont)), nil
			}
			_ = c.currentCursor.Close()
			c.currentCursor = nil
		}

		if c.exhausted {
			return recordlayer.NewResultNoNext[*recordlayer.IndexEntry](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
		}

		// Advance to the next distinct partition.
		fullPrefix, found, err := c.findNextPartition()
		if err != nil {
			return recordlayer.RecordCursorResult[*recordlayer.IndexEntry]{}, err
		}
		if !found {
			c.exhausted = true
			return recordlayer.NewResultNoNext[*recordlayer.IndexEntry](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
		}
		c.currentPrefix = fullPrefix

		var entries []*recordlayer.IndexEntry
		var inner []byte
		// len() > 0, not != nil: an empty-but-non-nil InnerContinuation would pass
		// a nil check, make newVectorSearchCursor take the fresh path with nil
		// entries, and silently skip the resumed partition.
		if len(c.pendingInner) > 0 {
			// Resumed partition: replay from the saved inner continuation; no
			// fresh HNSW search needed (matches Java's replay-from-continuation).
			inner = c.pendingInner
			c.pendingInner = nil
		} else {
			entries, err = c.m.searchOnePartition(c.m.ReadTransaction(c.scanProps), fullPrefix, c.queryVector, c.k, c.opts)
			if err != nil {
				return recordlayer.RecordCursorResult[*recordlayer.IndexEntry]{}, err
			}
		}
		cursor, cerr := c.m.newVectorSearchCursor(entries, inner, fullPrefix)
		if cerr != nil {
			// Corrupt per-partition inner continuation. Latch the error so every
			// subsequent OnNext fails the same way instead of falling through to
			// a fresh search on retry (which would silently re-emit rows).
			c.errLatch = cerr
			return recordlayer.RecordCursorResult[*recordlayer.IndexEntry]{}, cerr
		}
		c.currentCursor = cursor
	}
}

// wrapContinuation wraps a per-partition continuation in a FlatMapContinuation
// carrying the current full partition prefix as the outer continuation.
//
// Errors are PROPAGATED, never swallowed into a nil result: a nil continuation
// on a HasNext row reads downstream as end-of-scan, so swallowing a marshal
// failure here would silently truncate results (return fewer rows than exist
// with no error) — a silent-data-loss path. In practice neither
// step fails — the inner is a BytesContinuation whose ToBytes is infallible, and
// FlatMapContinuation marshals two []byte fields — but we surface any failure as
// a cursor error rather than a short read.
func (c *vectorMultiPartitionCursor) wrapContinuation(inner recordlayer.RecordCursorContinuation) ([]byte, error) {
	innerBytes, err := inner.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("VECTOR index %q multi-partition continuation: inner: %w", c.m.Index().Name, err)
	}
	fm := &gen.FlatMapContinuation{
		OuterContinuation: c.currentPrefix.Pack(),
		InnerContinuation: innerBytes,
	}
	data, err := fm.MarshalVT()
	if err != nil {
		return nil, fmt.Errorf("VECTOR index %q multi-partition continuation: marshal: %w", c.m.Index().Name, err)
	}
	return data, nil
}

// findNextPartition reads one key at/after nextPartitionStart within the
// partialPrefix range, extracts the first partitionSize tuple elements as the
// next distinct full partition prefix, and advances nextPartitionStart past that
// partition's entire subspace. Mirrors Java's nextPrefixTuple / the multidim
// prefixSkipScanCursor.findNextPrefix.
func (c *vectorMultiPartitionCursor) findNextPartition() (tuple.Tuple, bool, error) {
	rng := fdb.KeyRange{Begin: c.nextPartitionStart, End: c.rangeEnd}
	kvs, err := c.m.Transaction().GetRange(rng, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
	if err != nil {
		return nil, false, fmt.Errorf("VECTOR index %q multi-partition skip-scan: %w", c.m.Index().Name, err)
	}
	if len(kvs) == 0 {
		return nil, false, nil
	}

	t, err := tuplefast.SubspaceUnpack(kvs[0].Key, len(c.m.hnswSubspace.Bytes()))
	if err != nil {
		return nil, false, fmt.Errorf("VECTOR index %q multi-partition skip-scan: unpack key: %w", c.m.Index().Name, err)
	}
	// A key under the HNSW subspace with fewer than partitionSize leading
	// elements is malformed — surface it as an error rather than silently
	// terminating the fan-out (which would skip every partition after it).
	// The write path always writes full partition prefixes, so this never fires
	// in practice.
	if len(t) < c.partitionSize {
		return nil, false, fmt.Errorf("VECTOR index %q multi-partition skip-scan: key has %d tuple elements, need partition prefix of %d",
			c.m.Index().Name, len(t), c.partitionSize)
	}
	prefix := make(tuple.Tuple, c.partitionSize)
	copy(prefix, t[:c.partitionSize])

	// Advance nextPartitionStart past this partition's entire subspace.
	prefixEnd, err := fdb.Strinc(c.m.getSubspaceForPrefix(prefix).Bytes())
	if err != nil {
		return nil, false, fmt.Errorf("VECTOR index %q multi-partition skip-scan: %w", c.m.Index().Name, err)
	}
	c.nextPartitionStart = fdb.Key(prefixEnd)
	return prefix, true, nil
}

func (c *vectorMultiPartitionCursor) Close() error {
	c.closed = true
	if c.currentCursor != nil {
		return c.currentCursor.Close()
	}
	return nil
}

func (c *vectorMultiPartitionCursor) IsClosed() bool { return c.closed }

// SearchKNN performs a k-nearest-neighbor search on the HNSW graph.
// prefix scopes the search to a specific prefix partition (nil for no prefix).
// Returns results sorted by distance (closest first).
//
// SNAPSHOT deliberately, and this is NOT the query leaf: SearchKNN is the
// direct store-level API (FDBRecordStore.SearchVectorIndex), reached by chaos
// verification and diagnostics rather than by a plan, and it carries no
// ScanProperties to derive an isolation from. The executor's leaf is
// ScanByDistance, which resolves isolation through Context().ReadTransaction
// like every other leaf. Giving this diagnostic path conflict ranges would
// make a verification pass contend with the workload it is verifying. If this
// API ever becomes reachable from a plan it must take ScanProperties and route
// through readTx — the exhaustiveness gate in the isolation test is what
// forces that conversation.
//
// The HNSW graph stores trimmed primary keys (via TrimPrimaryKey). This method
// reconstructs full primary keys using getEntryPrimaryKey so callers can use
// them directly with LoadRecord.
func (m *vectorIndexMaintainer) SearchKNN(prefix tuple.Tuple, queryVector []float64, k, efSearch int) ([]recordlayer.VectorSearchResult, error) {
	// Read-lock the partition a write to it write-locks, as Java's
	// VectorIndexMaintainer.scan does (LockIdentifier(partitionSubspace)).
	lockKey := string(m.getSubspaceForPrefix(prefix).Bytes())
	m.Store().AcquireReadLock(lockKey)
	defer m.Store().ReleaseReadLock(lockKey)

	if m.engine == VectorEngineGuardiann {
		results, err := m.searchGuardiann(m.Transaction().Snapshot(), prefix, queryVector, k, recordlayer.VectorIndexScanOptions{EfSearch: positiveEfSearch(efSearch)})
		if err != nil {
			return nil, err
		}
		out := make([]recordlayer.VectorSearchResult, len(results))
		for i, r := range results {
			key := append(append(tuple.Tuple{}, prefix...), r.primaryKey...)
			out[i] = recordlayer.VectorSearchResult{PrimaryKey: m.entryFullPK(key, prefix), Distance: r.distance}
		}
		return out, nil
	}
	// Guard: query vector dimension must match the index's configured dimensions.
	if len(queryVector) != m.hnswConfig.NumDimensions {
		return nil, fmt.Errorf("VECTOR index %q expects %d dimensions, but query vector has %d",
			m.Index().Name, m.hnswConfig.NumDimensions, len(queryVector))
	}
	// Guard: if index has a prefix (KWV splitPoint > 0), caller MUST provide one.
	// Searching without a prefix on a grouped index returns empty (queries the
	// base subspace which has no data), silently producing wrong results.
	if len(prefix) == 0 {
		if kwv, ok := m.Index().RootExpression.(*recordlayer.KeyWithValueExpression); ok && kwv.SplitPoint() > 0 {
			return nil, fmt.Errorf("VECTOR index %q is prefix-partitioned (splitPoint=%d): "+
				"use SearchVectorIndexWithPrefix to provide a prefix", m.Index().Name, kwv.SplitPoint())
		}
	}
	storage := m.getStorageForPrefix(prefix)
	graph := NewHNSWGraph(storage, m.hnswConfig)

	results, err := graph.Search(m.Transaction().Snapshot(), queryVector, k, recordlayer.HNSWEfSearch(positiveEfSearch(efSearch), k))
	if err != nil {
		return nil, err
	}

	vResults := make([]recordlayer.VectorSearchResult, len(results))
	for i, r := range results {
		// Reconstruct full PK from the trimmed PK stored in HNSW.
		// When primaryKeyComponentPositions is set (PK overlaps index expression),
		// some PK components are in the prefix (index key) and need reconstruction.
		// Otherwise, the PK in HNSW is already the full PK.
		var fullPK tuple.Tuple
		if m.Index().HasPrimaryKeyComponentPositions() {
			entryKey := make(tuple.Tuple, 0, len(prefix)+len(r.PrimaryKey))
			entryKey = append(entryKey, prefix...)
			entryKey = append(entryKey, r.PrimaryKey...)
			fullPK = m.Index().EntryPrimaryKey(entryKey)
		} else {
			fullPK = r.PrimaryKey
		}

		vResults[i] = recordlayer.VectorSearchResult{
			PrimaryKey: fullPK,
			Distance:   r.Distance,
		}
	}
	return vResults, nil
}

// DeleteWhere clears all HNSW graph data at or BELOW the given prefix.
// An empty prefix clears every graph in the index.
//
// The range clear is the whole point, and clearing the prefix's own graph is
// not enough. A grouped vector index keyed by (zone, category) stores each
// group at hnswSubspace.Sub(zone, category), so a delete-where on (zone) has to
// take every category under it. Clearing only the graph AT (zone) left those
// descendants behind: their records were deleted while their HNSW nodes stayed
// queryable, so a search returned primary keys that no longer resolve.
//
// Matches Java, which reaches StandardIndexMaintainer.deleteWhere through
// VectorIndexMaintainer.deleteWhere and clears
// Range.startsWith(indexSubspace.pack(prefix)) — everything under the packed
// prefix, descendants included.
// CanDeleteWhere is Java's VectorIndexMaintainer.canDeleteWhere (:358-363):
// beyond the delegate's own alignment check it requires
// `evaluated.size() <= getKeyWithValueExpression(root).getColumnSize()`, and
// KeyWithValueExpression.getColumnSize() is the SPLIT POINT — the number of
// key columns, not the whole expression's width.
//
// That bound is the one the store-level alignment check cannot supply. It
// normalises a KeyWithValue to its FULL inner key (prefix columns AND vector
// columns), so a prefix reaching into the vector columns aligns positionally
// and is accepted — while getSubspaceForPrefix then addresses a subspace one
// level deeper than any graph that exists. The clear hits nothing, returns
// success, and the deleted records' HNSW nodes stay queryable.
//
// A non-KeyWithValue root has no key columns at all: splitPrefixAndVector
// reads the whole key as the vector and indexes every record under the empty
// prefix, so any non-empty prefix is unclearable. Java refuses that root shape
// outright (getKeyWithValueExpression throws); Go accepts it for other
// operations — see "VECTOR index metadata validation" in DIVERGENCES.md — so
// the bound of zero is what expresses the same refusal here.
func (m *vectorIndexMaintainer) CanDeleteWhere(prefix tuple.Tuple) error {
	keyColumns := 0
	if kwv, ok := m.Index().RootExpression.(*recordlayer.KeyWithValueExpression); ok {
		keyColumns = kwv.ColumnSize()
	}
	if len(prefix) > keyColumns {
		return fmt.Errorf(
			"vector index %q: deleteWhere prefix has %d columns but the index has %d key column(s); "+
				"a longer prefix names a graph that does not exist, so the clear would silently "+
				"leave the deleted records' HNSW nodes in place",
			m.Index().Name, len(prefix), keyColumns,
		)
	}
	return nil
}

func (m *vectorIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	// Backstop for direct callers — Java's Verify.verify inside deleteWhere
	// (:366-369). DeleteRecordsWhere asks CanDeleteWhere before it clears.
	if err := m.CanDeleteWhere(prefix); err != nil {
		return err
	}
	sub := m.getSubspaceForPrefix(prefix)
	pr, err := fdb.PrefixRange(sub.Bytes())
	if err != nil {
		return fmt.Errorf("vector index %q: DeleteWhere PrefixRange(%x): %w", m.Index().Name, sub.Bytes(), err)
	}
	m.Transaction().ClearRange(pr)
	if m.engine == VectorEngineGuardiann {
		if err := m.taskCounts.clearPrefix(m.Transaction(), prefix); err != nil {
			return err
		}
		if err := addVectorDeleteWhereConflicts(m.Transaction(), m.secondarySubspace, prefix); err != nil {
			return err
		}
	}
	// No graph cache outlives an operation (getStorageForPrefix), so nothing
	// cached can resurrect the cleared nodes.
	return nil
}

// guardiannSearchConfig is GuardiannVectorIndexEngine.searchConfig.
func guardiannSearchConfigOf(o recordlayer.VectorIndexScanOptions) (guardiannSearchConfig, error) {
	c := defaultGuardiannSearchConfig()
	setF := func(dst *float64, v *float64) {
		if v != nil {
			*dst = *v
		}
	}
	setI := func(dst *int, v *int) {
		if v != nil {
			*dst = *v
		}
	}
	setF(&c.candidatePoolFactor, o.GuardiannCandidatePoolFactor)
	setI(&c.searchMaxClusters, o.GuardiannSearchMaxClusters)
	setI(&c.searchMinClustersBeforePruning, o.GuardiannSearchMinClustersBeforePruning)
	setF(&c.searchDistanceRatioCutoff, o.GuardiannSearchDistanceRatioCutoff)
	setI(&c.centroidEfRingSearch, o.GuardiannCentroidEfRingSearch)
	setI(&c.centroidEfOutwardSearch, o.GuardiannCentroidEfOutwardSearch)
	setI(&c.searchConcurrency, o.GuardiannSearchConcurrency)
	return c, c.validate()
}

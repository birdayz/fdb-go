package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// spfreshIndexMaintainer is the record-layer integration of the SPFresh
// vector index (RFC-094). Phase 094.1 scope, stated honestly:
//
//   - the index is BUILD-THEN-READ: BuildSPFreshIndex bulk-builds a
//     generation from the store's existing records and flips it readable;
//   - foreground record writes are REJECTED while the index is present
//     (Update/UpdateWhileWriteOnly error) — the conflict-free foreground
//     write path is phase 094.2, the rebalancer 094.3 (RFC-094 §13);
//   - ScanByDistance serves kNN queries through the two-level routing cache
//     with the same TupleRange/IndexEntry contract as the HNSW index, so the
//     executor and (in 094.6) the planner reuse the existing vector surface.
type spfreshIndexMaintainer struct {
	standardIndexMaintainer
	config SPFreshConfig
}

func newSPFreshIndexMaintainer(
	index *Index,
	indexSubspace subspace.Subspace,
	tx fdb.Transaction,
	store indexStoreContext,
) (*spfreshIndexMaintainer, error) {
	config := parseSPFreshConfig(index)
	if err := ValidateSPFreshConfig(config); err != nil {
		return nil, fmt.Errorf("spfresh index %q: %w", index.Name, err)
	}
	if index.primaryKeyComponentPositions != nil {
		// The index stores TrimPrimaryKey'd tails; with PK components shared
		// into the index key the tail is not the full primary key, and
		// pinning it on scan entries would make the executor LoadRecord the
		// wrong key (codex 094.1 r2). Full-PK reconstruction lands with the
		// grouped/prefixed support; reject the shape until then.
		return nil, fmt.Errorf("spfresh index %q: primary-key components shared with the index key are not supported in 094.1", index.Name)
	}
	return &spfreshIndexMaintainer{
		standardIndexMaintainer: *newStandardIndexMaintainer(index, indexSubspace, tx, store),
		config:                  config,
	}, nil
}

// spfreshCaches holds the per-process routing caches, one per (index
// subspace, generation) — the client-side state of RFC-094 §2. Browsing a
// retrained generation gets a fresh cache; superseded entries age out with
// the process (bounded: generations are rare).
var spfreshCaches sync.Map // string(subspace bytes)+gen -> *spfreshRoutingCache

func spfreshCacheFor(indexSubspace subspace.Subspace, generation int64) *spfreshRoutingCache {
	key := fmt.Sprintf("%x/%d", indexSubspace.Bytes(), generation)
	if c, ok := spfreshCaches.Load(key); ok {
		return c.(*spfreshRoutingCache)
	}
	c, _ := spfreshCaches.LoadOrStore(key, newSPFreshRoutingCache(0))
	return c.(*spfreshRoutingCache)
}

// Update rejects foreground writes in 094.1 (no fake checkboxes: the
// conflict-free write path with its state-row fencing is 094.2; silently
// dropping index maintenance would corrupt the index).
func (m *spfreshIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return fmt.Errorf("spfresh index %q: foreground record writes are not supported in phase 094.1 (build-then-read; RFC-094 §13) — rebuild the index to incorporate changes", m.index.Name)
}

// UpdateWhileWriteOnly: same contract in 094.1 (the build/foreground staging
// interleaving is 094.2).
func (m *spfreshIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return m.Update(oldRecord, newRecord)
}

// Scan: vector indexes have no meaningful BY_VALUE range scan (entries are
// quantized residuals under centroid-assigned keys); mirror the HNSW index's
// contract by rejecting it.
func (m *spfreshIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return &errorCursor[*IndexEntry]{err: fmt.Errorf("spfresh index %q: BY_VALUE scan not supported; use BY_DISTANCE", m.index.Name)}
}

// DeleteWhere clears the whole index keyspace under the prefix. For 094.1
// only the full clear (empty prefix) is meaningful (no grouped SPFresh yet).
func (m *spfreshIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	if len(prefix) != 0 {
		return fmt.Errorf("spfresh index %q: grouped DeleteWhere not supported in 094.1", m.index.Name)
	}
	r, err := fdb.PrefixRange(m.indexSubspace.Bytes())
	if err != nil {
		return err
	}
	m.tx.ClearRange(r)
	return nil
}

// ScanByDistance executes a kNN query. Same TupleRange convention as the
// HNSW maintainer: Low = (serialized query vector [, prefix...]),
// High = (k [, efSearch]); efSearch maps onto the adaptive fine-probe width.
// Each IndexEntry: Key = (trimmedPK...), Value = nil (codes are residuals;
// exact vectors live in the sidecar/records). Distances are exact
// (sidecar-re-ranked) and ascending.
func (m *spfreshIndexMaintainer) ScanByDistance(
	scanRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	if scanRange.Low == nil || len(scanRange.Low) < 1 {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("SPFresh BY_DISTANCE scan requires query vector in TupleRange.Low")}
	}
	vecBytes, ok := scanRange.Low[0].([]byte)
	if !ok {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("SPFresh BY_DISTANCE scan: TupleRange.Low[0] must be []byte")}
	}
	queryVector, err := deserializeVector(vecBytes)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("SPFresh BY_DISTANCE scan: invalid query vector: %w", err)}
	}
	if len(queryVector) != m.config.NumDimensions {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("spfresh index %q: query vector has %d dimensions, index has %d", m.index.Name, len(queryVector), m.config.NumDimensions)}
	}
	if len(scanRange.Low) > 1 {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("spfresh index %q: prefixed (grouped) scans not supported in 094.1", m.index.Name)}
	}
	k := 10
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

	results, err := m.searchCurrentGeneration(queryVector, k, efSearch)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: err}
	}
	entries := make([]*IndexEntry, len(results))
	for i, r := range results {
		// Index + pinned primaryKey make the entry usable through the normal
		// executor path (IndexEntry.PrimaryKey loads the record).
		entries[i] = &IndexEntry{Index: m.index, Key: r.PrimaryKey, Value: tuple.Tuple{nil}, primaryKey: r.PrimaryKey}
	}
	return FromListWithContinuation(entries, continuation)
}

// searchCurrentGeneration resolves the readable generation, refreshes/loads
// the per-process cache, and runs the search in the maintainer's transaction.
func (m *spfreshIndexMaintainer) searchCurrentGeneration(query []float64, k, efSearch int) ([]spfreshSearchResult, error) {
	// Resolve the generation (snapshot — queries never conflict).
	metaStorage := newSPFreshStorage(m.indexSubspace, 0) // gen 0: META access only
	gen, err := spfreshReadGenerationSnapshot(m.tx, metaStorage)
	if err != nil {
		return nil, fmt.Errorf("spfresh index %q: no readable generation (build the index first): %w", m.index.Name, err)
	}
	storage := newSPFreshStorage(m.indexSubspace, gen)
	cache := spfreshCacheFor(m.indexSubspace, gen)

	// Queries pay ZERO cache-maintenance I/O once loaded (RFC-094 §4 — the
	// per-query changelog read was the rev-2-NAK'd hot-key anti-pattern, and
	// it cost ~half the measured p50 at SIFT-100k). In 094.1 the topology is
	// static per generation: only a cold cache or a generation change reloads;
	// the incremental timer refresh arrives with the rebalancer in 094.3.
	if !cache.ready(gen) {
		if frerr := cache.fullReload(m.tx, storage, gen); frerr != nil {
			return nil, fmt.Errorf("spfresh index %q: routing reload: %w", m.index.Name, frerr)
		}
	}

	searcher := newSPFreshSearcher(storage, m.config, cache)
	if efSearch > 0 {
		// Map the HNSW-style efSearch knob onto the fine-probe width.
		searcher.kc = max(searcher.kc, efSearch)
		searcher.c = max(searcher.c, 4*k)
	}
	return searcher.search(m.tx, query, k)
}

// spfreshScanBatchSize is the records-per-transaction limit of the build's
// record-collection scan (one unbounded scan blows the 5 s tx limit and
// retries forever). Variable so the continuation path is exercised by a
// small deterministic regression, not only by the env-gated SIFT benchmark.
var spfreshScanBatchSize = 1000

// BuildSPFreshIndex bulk-builds an SPFresh index over the store's existing
// records (RFC-094 §8) and flips it readable. 094.1's build-then-read entry
// point; resumable builds via OnlineIndexer integration land with 094.2's
// staging interleaving.
func BuildSPFreshIndex(ctx context.Context, db *FDBDatabase, storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error), indexName string, seed int64) error {
	// Collect inputs: scan the records in CONTINUATION-BATCHED transactions —
	// one unbounded scan blows the 5 s FDB transaction limit at scale and the
	// retry loop restarts it from scratch forever (caught by the SIFT-100k
	// benchmark hanging in exactly that loop). Each tx reads up to
	// spfreshScanBatch records and hands its continuation to the next.
	scanBatch := spfreshScanBatchSize
	var inputs []spfreshBuildInput
	var index *Index
	var indexSubspace subspace.Subspace
	var config SPFreshConfig
	var continuation []byte
	for first := true; first || continuation != nil; first = false {
		// Per-attempt staging: the body may RETRY (1007/1020); appending to
		// the outer slices inside it would duplicate the batch.
		var batchInputs []spfreshBuildInput
		var nextContinuation []byte
		err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
			batchInputs = batchInputs[:0]
			nextContinuation = nil
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return serr
			}
			if index == nil {
				index = store.GetMetaData().GetIndex(indexName)
				if index == nil {
					return fmt.Errorf("spfresh build: index %q not found", indexName)
				}
				if index.Type != IndexTypeVectorSPFresh {
					return fmt.Errorf("spfresh build: index %q has type %q", indexName, index.Type)
				}
				config = parseSPFreshConfig(index)
				if verr := ValidateSPFreshConfig(config); verr != nil {
					return verr
				}
				indexSubspace = store.indexSubspace(index)
			}
			evaluator := newStandardIndexMaintainer(index, indexSubspace, rtx.Transaction(), store)

			props := ScanProperties{ExecuteProperties: ExecuteProperties{ReturnedRowLimit: scanBatch}}
			cursor := store.ScanRecords(continuation, props)
			defer func() { _ = cursor.Close() }()
			for {
				result, cerr := cursor.OnNext(ctx)
				if cerr != nil {
					return cerr
				}
				if !result.HasNext() {
					if !result.GetNoNextReason().IsSourceExhausted() {
						cont, cterr := result.GetContinuation().ToBytes()
						if cterr != nil {
							return cterr
						}
						nextContinuation = cont
					}
					break
				}
				rec := result.GetValue()
				entries, eerr := evaluator.evaluateIndex(rec)
				if eerr != nil {
					return eerr
				}
				for _, entry := range entries {
					vec, verr := spfreshEntryVector(index, entry)
					if verr != nil {
						return verr
					}
					if vec == nil {
						continue // absent/null vector: unindexed
					}
					trimmedPK, terr := index.TrimPrimaryKey(entry.primaryKey)
					if terr != nil {
						return terr
					}
					batchInputs = append(batchInputs, spfreshBuildInput{pk: trimmedPK, vec: vec})
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		inputs = append(inputs, batchInputs...)
		continuation = nextContinuation
	}
	if len(inputs) == 0 {
		return fmt.Errorf("spfresh build: no records with vectors for index %q", indexName)
	}

	// Builds target generation current+1 (1 for a first build): a REBUILD into
	// the live generation would mix stale postings with new ones and leave
	// process caches 'ready' on stale routing (Torvalds/codex 094.1 review).
	var oldGen int64
	if err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		g, gerr := spfreshReadGenerationSnapshot(rtx.Transaction(), newSPFreshStorage(indexSubspace, 0))
		if gerr != nil {
			if errors.Is(gerr, errSPFreshNotFound) {
				oldGen = 0
				return nil
			}
			return gerr
		}
		oldGen = g
		return nil
	}); err != nil {
		return err
	}
	storage := newSPFreshStorage(indexSubspace, oldGen+1)
	// Abandoned-build GC at the entry point (RFC-094 §3): a prior build into
	// this same target that aborted PRE-FLIP left build-state/cell residue a
	// fresh run would mistake for its own commit retries — publishing stale
	// centroids for the newly scanned inputs (codex 094.1 r2). The target is
	// not readable (the flip never happened), so clearing it is safe.
	if err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		r, rerr := storage.generationRange()
		if rerr != nil {
			return rerr
		}
		rtx.Transaction().ClearRange(r)
		return nil
	}); err != nil {
		return err
	}
	builder := newSPFreshBuilder(db, storage, config, fmt.Sprintf("build-%s", indexName))
	if berr := builder.build(ctx, inputs, seed); berr != nil {
		return berr
	}
	if oldGen > 0 {
		// Clear the superseded generation. 094.1 is build-then-read with no
		// concurrent writers; the staleness grace period for live readers
		// arrives with 094.3's horizon machinery (RFC-094 §3).
		if err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
			r, rerr := newSPFreshStorage(indexSubspace, oldGen).generationRange()
			if rerr != nil {
				return rerr
			}
			rtx.Transaction().ClearRange(r)
			return nil
		}); err != nil {
			return err
		}
	}

	// Record the build in the index's range set (the bulk build covered the
	// full record space in one pass), so MarkIndexReadable's unbuilt-range
	// check passes — the same completion bookkeeping OnlineIndexer performs.
	return spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		store, serr := storeBuilder(rtx)
		if serr != nil {
			return serr
		}
		rangeSet := NewIndexingRangeSet(store.subspace, index)
		_, ierr := rangeSet.InsertRange(rtx.Transaction(), nil, rangeSetFinalKey, false)
		return ierr
	})
}

// spfreshEntryVector extracts the vector from an evaluated index entry
// (KeyWithValue puts it in the value; plain expressions in the key) — the
// same convention the HNSW maintainer uses.
func spfreshEntryVector(index *Index, entry indexEntry) ([]float64, error) {
	if _, ok := index.RootExpression.(*KeyWithValueExpression); ok && len(entry.value) > 0 {
		return tupleToVector(entry.value)
	}
	return tupleToVector(entry.key)
}

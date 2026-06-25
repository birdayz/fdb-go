package recordlayer

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"math/rand"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// fragmentIterationType represents the phase of mutual index building.
// Matches Java's IndexingMutuallyByRecords.FragmentIterationType.
type fragmentIterationType int

const (
	// fragmentFull only builds fully unbuilt fragments (no partial).
	fragmentFull fragmentIterationType = iota
	// fragmentAny builds any fragment with missing ranges.
	fragmentAny
	// fragmentRecover means all phases exhausted — manual recovery needed.
	fragmentRecover
)

// mutualIndexBuilder builds indexes concurrently with other processes.
// Each builder divides the key space into fragments (by FDB shard boundaries)
// and iterates them in a unique random order (prime-step modular arithmetic).
// Matches Java's IndexingMutuallyByRecords.
type mutualIndexBuilder struct {
	indexer   *OnlineIndexer
	heartbeat *IndexingHeartbeat

	// Fragment state
	boundaries    [][]byte // raw FDB keys — shard boundaries within records subspace
	fragmentCount int
	fragmentStep  int // prime, not a divisor of fragmentCount
	fragmentFirst int // random starting fragment
	fragmentCur   int // current fragment index
	iterType      fragmentIterationType

	// Conflict avoidance (anyJumper equivalent from Java):
	// When InsertRange(requireEmpty=true) fails, another builder claimed this range.
	// Instead of failing the transaction, skip to the next fragment.
	lastConflictFragment int // fragment where last conflict occurred (-1 = none)
	sameRangeRetries     int // consecutive retries on the same range (infinite loop protection)
}

// newMutualIndexBuilder initializes the mutual builder.
// Fetches FDB shard boundaries to determine fragments.
func newMutualIndexBuilder(oi *OnlineIndexer) (*mutualIndexBuilder, error) {
	hb := NewIndexingHeartbeat(
		"MUTUAL_BY_RECORDS",
		oi.leaseLengthMs,
		true, // allowMutual
	)

	m := &mutualIndexBuilder{
		indexer:              oi,
		heartbeat:            hb,
		iterType:             fragmentFull,
		lastConflictFragment: -1,
	}

	// Compute fragment boundaries from FDB shard splits.
	if err := m.computeFragments(); err != nil {
		return nil, fmt.Errorf("compute fragments: %w", err)
	}

	return m, nil
}

// computeFragments fetches FDB shard boundaries for the records subspace
// and sets up the fragment iteration order.
//
// Matches Java's IndexingMutuallyByRecords.getPrimaryKeyBoundaries() which:
// 1. Calls store.getPrimaryKeyBoundaries() → LocalityUtil.getBoundaryKeys()
// 2. If empty (single-node FDB), injects [null, null] as endpoints
// 3. Always ensures at least 2 boundary points → at least 1 fragment
//
// On single-node FDB clusters (including testcontainers), getBoundaryKeys
// returns no results, so we degrade to 1 fragment covering the entire range.
func (m *mutualIndexBuilder) computeFragments() error {
	// Matches Java's IndexingMutuallyByRecords.getPrimaryKeyBoundaries():
	// 1. If pre-set boundaries provided, use them
	// 2. Otherwise auto-detect via LocalityGetBoundaryKeys (FDB shard splits)
	// 3. On single-node FDB, auto-detection returns empty → 1 fragment
	// 4. Always inject endpoints to ensure at least 2 boundary points

	var pkBoundaries [][]byte
	if len(m.indexer.mutualBoundaries) > 0 {
		pkBoundaries = m.indexer.mutualBoundaries
	} else {
		// Auto-detect shard boundaries via the FDB locality API. Works on BOTH
		// backends now (RFC-109: the libfdb_c backend exposes LocalityGetBoundaryKeys
		// too — a read of \xff/keyServers, byte-identical to the pure-Go client), so
		// mutual indexing parallelizes on libfdb_c instead of degrading to a single
		// fragment. On single-node FDB (incl. testcontainers) this returns no splits →
		// single fragment; an error (a backend without locality) is likewise treated
		// as "no boundaries".
		recordsSub := m.indexer.subspace.Sub(RecordKey)
		begin, end := recordsSub.FDBRangeKeys()
		rawKeys, err := m.indexer.db.LocalityGetBoundaryKeys(
			fdb.KeyRange{Begin: begin, End: end}, 0, 0,
		)
		if err == nil {
			// Convert absolute FDB keys to relative PK bytes by stripping
			// the records subspace prefix and tuple-unpacking.
			prefix := recordsSub.Bytes()
			for _, k := range rawKeys {
				if len(k) > len(prefix) {
					pkBoundaries = append(pkBoundaries, []byte(k)[len(prefix):])
				}
			}
		}
		// Error ignored — fall back to single fragment.
	}

	// Fragment boundaries are in the RangeSet key space (raw PK bytes).
	// RangeSet operates in [\x00, \xff). Matching Java where
	// getPrimaryKeyBoundaries returns Tuple boundaries and fragmentGet
	// converts null → {0x00}/{0xff}.
	m.boundaries = make([][]byte, 0, len(pkBoundaries)+2)
	m.boundaries = append(m.boundaries, rangeSetFirstKey) // \x00
	for _, b := range pkBoundaries {
		m.boundaries = append(m.boundaries, b)
	}
	m.boundaries = append(m.boundaries, rangeSetFinalKey) // \xff

	// Deduplicate consecutive equal boundaries.
	deduped := m.boundaries[:1]
	for i := 1; i < len(m.boundaries); i++ {
		if !bytes.Equal(m.boundaries[i], deduped[len(deduped)-1]) {
			deduped = append(deduped, m.boundaries[i])
		}
	}
	m.boundaries = deduped

	m.fragmentCount = len(m.boundaries) - 1
	if m.fragmentCount <= 0 {
		m.fragmentCount = 1
	}

	// Random starting point and step.
	// Step must be coprime with fragmentCount (use a prime that doesn't divide it).
	// Matches Java's getPrimeStep(fragmentNum, rn).
	rng := rand.New(rand.NewSource(rand.Int63()))
	m.fragmentFirst = rng.Intn(m.fragmentCount)
	m.fragmentCur = m.fragmentFirst
	m.fragmentStep = findCoprimeStep(m.fragmentCount, rng)

	return nil
}

// findCoprimeStep finds a step value coprime with n for uniform iteration.
// Uses small primes first, then random primes.
// Matches Java's random-prime-not-divisor-of(fragmentNum) approach.
func findCoprimeStep(n int, rng *rand.Rand) int {
	if n <= 1 {
		return 1
	}

	primes := []int{2, 3, 5, 7, 11, 13, 17, 19, 23, 29, 31, 37, 41, 43, 47}
	// Collect primes that don't divide n.
	var candidates []int
	for _, p := range primes {
		if n%p != 0 {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) > 0 {
		return candidates[rng.Intn(len(candidates))]
	}

	// n is divisible by all small primes — use a large prime.
	// This is extremely unlikely for realistic fragment counts.
	bigN := big.NewInt(int64(n))
	for p := int64(53); ; p += 2 {
		bp := big.NewInt(p)
		if bp.ProbablyPrime(10) && new(big.Int).Mod(bigN, bp).Int64() != 0 {
			return int(p)
		}
	}
}

// fragmentRange returns the byte range [begin, end) for the current fragment.
func (m *mutualIndexBuilder) fragmentRange() ([]byte, []byte) {
	if m.fragmentCount <= 1 {
		return m.boundaries[0], m.boundaries[len(m.boundaries)-1]
	}
	idx := m.fragmentCur
	if idx >= m.fragmentCount {
		idx = m.fragmentCount - 1
	}
	return m.boundaries[idx], m.boundaries[idx+1]
}

// fragmentAdvance moves to the next fragment using modular prime-step.
// Returns true if we've completed a full cycle.
func (m *mutualIndexBuilder) fragmentAdvance() bool {
	m.fragmentCur = (m.fragmentCur + m.fragmentStep) % m.fragmentCount
	return m.fragmentCur == m.fragmentFirst
}

// buildMutual runs the mutual index build: iterates fragments, builds missing
// ranges, advances through FULL → ANY phases.
// Returns (recordsProcessed, hasMore, error).
func (m *mutualIndexBuilder) buildMutual(ctx context.Context) (int64, bool, error) {
	var recordsProcessed int64
	var hasMore bool

	_, err := m.indexer.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		recordsProcessed = 0
		hasMore = false

		store, err := m.indexer.openStore(rtx)
		if err != nil {
			return nil, err
		}

		// Update heartbeat every transaction.
		if err := m.heartbeat.CheckAndUpdate(rtx.Transaction(), m.indexer.subspace, m.indexer.primaryIndex()); err != nil {
			return nil, err
		}

		primaryRangeSet := NewIndexingRangeSet(store.subspace, m.indexer.primaryIndex())

		// Try the current fragment first (may have remaining work from last call),
		// then advance through other fragments if needed.
		cyclesWithoutWork := 0
		for cyclesWithoutWork <= m.fragmentCount {
			fragBegin, fragEnd := m.fragmentRange()

			rangeToBuild, err := m.findRangeInFragment(rtx.Transaction(), primaryRangeSet, fragBegin, fragEnd)
			if err != nil {
				return nil, err
			}

			if rangeToBuild != nil {
				n, err := m.buildFragmentRange(ctx, store, primaryRangeSet, rangeToBuild)
				if err != nil {
					return nil, err
				}
				// Check if the range was contested (anyJumper: another builder claimed it).
				if m.lastConflictFragment == m.fragmentCur {
					m.lastConflictFragment = -1
					m.sameRangeRetries++
					// Infinite loop protection: if we've retried the same range 1000 times,
					// another builder is persistently contesting. Skip this fragment.
					// Matches Java's infiniteLoopProtection threshold.
					if m.sameRangeRetries > 1000 {
						return nil, fmt.Errorf("mutual indexer: infinite loop on fragment %d after 1000 retries", m.fragmentCur)
					}
					// Jump to next fragment instead of retrying the same one.
					cyclesWithoutWork++
					if m.fragmentAdvance() {
						m.iterType++
						if m.iterType >= fragmentRecover {
							missing, checkErr := primaryRangeSet.FirstMissingRange(rtx.Transaction())
							if checkErr != nil {
								return nil, checkErr
							}
							hasMore = missing != nil
							return nil, nil
						}
						cyclesWithoutWork = 0
					}
					continue
				}
				m.sameRangeRetries = 0
				recordsProcessed = n
				// Don't advance fragment — stay here for the next call
				// in case there's more work in this fragment.
				hasMore = true
				return nil, nil
			}

			// No work in this fragment — advance to the next one.
			cyclesWithoutWork++
			if m.fragmentAdvance() {
				// Completed a full cycle — advance phase.
				m.iterType++
				if m.iterType >= fragmentRecover {
					// All phases exhausted. Check if truly complete.
					missing, err := primaryRangeSet.FirstMissingRange(rtx.Transaction())
					if err != nil {
						return nil, err
					}
					hasMore = missing != nil
					return nil, nil
				}
				// Reset counter for the new phase.
				cyclesWithoutWork = 0
			}
		}

		// Exhausted all fragments — check if build is complete.
		missing, err := primaryRangeSet.FirstMissingRange(rtx.Transaction())
		if err != nil {
			return nil, err
		}
		hasMore = missing != nil
		return nil, nil
	})

	return recordsProcessed, hasMore, err
}

// findRangeInFragment finds a missing range within the fragment boundaries.
// In FULL phase, only returns fully unbuilt fragments.
// In ANY phase, returns any fragment with missing ranges.
func (m *mutualIndexBuilder) findRangeInFragment(tx fdb.WritableTransaction, rangeSet *IndexingRangeSet, fragBegin, fragEnd []byte) (*RangeSetRange, error) {
	missing, err := rangeSet.ListMissingRangesInBytes(tx, fragBegin, fragEnd)
	if err != nil {
		return nil, err
	}
	if len(missing) == 0 {
		return nil, nil
	}

	if m.iterType == fragmentFull {
		// FULL phase: only build if the ENTIRE fragment is unbuilt.
		// A single missing range spanning the whole fragment means fully unbuilt.
		if len(missing) == 1 && bytes.Compare(missing[0].Begin, fragBegin) <= 0 && bytes.Compare(missing[0].End, fragEnd) >= 0 {
			return &RangeSetRange{Begin: fragBegin, End: fragEnd}, nil
		}
		return nil, nil // Partially built — skip in FULL phase.
	}

	// ANY phase: return the first missing range within the fragment.
	return &missing[0], nil
}

// buildFragmentRange builds records within the given range, same as buildRange
// but scoped to a fragment. Reuses the indexer's existing buildRange logic.
func (m *mutualIndexBuilder) buildFragmentRange(ctx context.Context, store *FDBRecordStore, primaryRangeSet *IndexingRangeSet, r *RangeSetRange) (int64, error) {
	var rangeStart, rangeEnd tuple.Tuple
	lowEp := EndpointTypeRangeInclusive
	highEp := EndpointTypeRangeExclusive

	if bytes.Equal(r.Begin, rangeSetFirstKey) {
		lowEp = EndpointTypeTreeStart
	} else {
		var err error
		if rangeStart, err = fastUnpack(r.Begin); err != nil {
			return 0, fmt.Errorf("mutual indexer: unpack range begin: %w", err)
		}
	}
	// The end may be a tuple+0xff (RANGE_INCLUSIVE high) written by the typed-records preset.
	if bytes.Equal(r.End, rangeSetFinalKey) {
		highEp = EndpointTypeTreeEnd
	} else {
		var err error
		if rangeEnd, highEp, err = unpackRangeEndBoundary(r.End); err != nil {
			return 0, fmt.Errorf("mutual indexer: unpack range end: %w", err)
		}
	}

	scanProps := ForwardScan()
	scanProps.ExecuteProperties.ReturnedRowLimit = saturatingAdd(m.indexer.limit, 1)
	if m.indexer.allTargetIndexesIdempotent() {
		scanProps.ExecuteProperties.IsolationLevel = IsolationLevelSnapshot
	}

	cursor := store.ScanRecordsInRange(rangeStart, rangeEnd, lowEp, highEp, nil, scanProps)

	var scannedCount int
	var recordsProcessed int64
	var extraPK tuple.Tuple

	for rec, iterErr := range Seq2(cursor, ctx) {
		if iterErr != nil {
			return 0, iterErr
		}
		scannedCount++
		if scannedCount > m.indexer.limit {
			extraPK = rec.PrimaryKey
			break
		}

		for _, idx := range m.indexer.targetIndexes {
			if !m.indexer.shouldIndexRecordForIndex(rec, idx) {
				continue
			}
			maintainer, mErr := store.GetIndexMaintainer(idx)
			if mErr != nil {
				return 0, fmt.Errorf("index %q get maintainer: %w", idx.Name, mErr)
			}
			if err := maintainer.Update(nil, rec); err != nil {
				return 0, fmt.Errorf("index %q update: %w", idx.Name, err)
			}
		}
		recordsProcessed++
	}

	// Mark the built range. Use the ORIGINAL byte boundary r.End (not rangeEnd.Pack()):
	// it may be a tuple+0xff (RANGE_INCLUSIVE high) from the typed-records preset, and the
	// stripped pack would drop the 0xff and invert the range.
	var endKey []byte
	if extraPK != nil {
		endKey = extraPK.Pack()
	} else if !bytes.Equal(r.End, rangeSetFinalKey) {
		endKey = r.End
	} else {
		endKey = rangeSetFinalKey
	}

	beginKey := r.Begin
	if rangeStart != nil && bytes.Equal(beginKey, rangeSetFirstKey) {
		beginKey = rangeStart.Pack()
	}

	for _, idx := range m.indexer.targetIndexes {
		idxRangeSet := NewIndexingRangeSet(store.subspace, idx)
		inserted, err := idxRangeSet.InsertRange(store.context.Transaction(), beginKey, endKey, true)
		if err != nil {
			return 0, fmt.Errorf("insert range for index %q: %w", idx.Name, err)
		}
		if !inserted {
			// requireEmpty=true found existing entries — another builder already
			// claimed this range. Record the conflict and let the caller skip to
			// the next fragment (anyJumper pattern from Java).
			// Note: transaction-level conflicts (concurrent commits) are handled
			// by buildRangeWithRetries at the outer level.
			m.lastConflictFragment = m.fragmentCur
			return 0, nil
		}
	}

	// Track progress.
	if recordsProcessed > 0 {
		for _, idx := range m.indexer.targetIndexes {
			store.AddBuildProgress(idx, recordsProcessed)
		}
	}

	return recordsProcessed, nil
}

// cleanup removes this builder's heartbeat.
func (m *mutualIndexBuilder) cleanup(ctx context.Context) {
	_, _ = m.indexer.db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		m.heartbeat.Cleanup(rtx.Transaction(), m.indexer.subspace, m.indexer.primaryIndex())
		return nil, nil
	})
}

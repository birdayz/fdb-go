// Portions derived from FoundationDB Record Layer (
// IndexingMutuallyByRecords.java, RangeSet.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"time"

	"fdb.dev/pkg/recordlayer/internal/tuplefast"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
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

	// Fragment state. boundaries stays nil until the first claim reads the shard map.
	boundaries    [][]byte // \x00, packed primary keys, \xff
	fragmentCount int
	fragmentStep  int // prime, not a divisor of fragmentCount
	fragmentFirst int // random starting fragment
	fragmentCur   int // current fragment index
	iterType      fragmentIterationType

	// claimed is the range being built chunk by chunk until it has no missing range,
	// the range of Java's buildThisRangeOnly loop.
	claimed *RangeSetRange
}

// newMutualIndexBuilder initializes the mutual builder. Preset boundaries set the
// fragments now; otherwise the first claim reads them from the shard map.
func newMutualIndexBuilder(oi *OnlineIndexer) *mutualIndexBuilder {
	hb := oi.sessionHeartbeat
	if hb == nil {
		// DST env: seeds the indexer UUID (once per indexer) and the persisted
		// heartbeat timestamps.
		hb = oi.newHeartbeat("MUTUAL_BY_RECORDS", true, oi.db.Env())
	}

	m := &mutualIndexBuilder{
		indexer:   oi,
		heartbeat: hb,
		iterType:  fragmentFull,
	}
	if len(oi.mutualBoundaries) > 0 {
		m.setFragments(oi.mutualBoundaries)
	}
	return m
}

// detectBoundaries sets the fragments at the primary keys of the indexed records' shard
// boundaries (Java's getPrimaryKeyBoundaries); a backend without a shard map gives one.
func (m *mutualIndexBuilder) detectBoundaries(store *FDBRecordStore) error {
	records := TupleRangeAll.ToFDBRange(store.recordsSubspace)
	begin, end := records.Begin.FDBKey(), records.End.FDBKey()
	if relBegin, relEnd, ok := m.indexer.computeRecordsRange(); ok {
		prefix := store.recordsSubspace.Bytes()
		begin = fdb.Key(append(bytes.Clone(prefix), relBegin...))
		end = fdb.Key(append(bytes.Clone(prefix), relEnd...))
	}
	primaryKeys, err := store.primaryKeyBoundaries(begin, end)
	var unsupported *BackendCapabilityError
	if err != nil && !errors.As(err, &unsupported) {
		return err
	}
	packed := make([][]byte, len(primaryKeys))
	for i, primaryKey := range primaryKeys {
		packed[i] = primaryKey.Pack()
	}
	m.setFragments(packed)
	return nil
}

// setFragments sets the fragment edges and a random visit order. pkBoundaries are packed
// primary keys; the edges add \x00 and \xff, as Java's fragmentGet does for its null ends.
func (m *mutualIndexBuilder) setFragments(pkBoundaries [][]byte) {
	m.boundaries = make([][]byte, 0, len(pkBoundaries)+2)
	m.boundaries = append(m.boundaries, rangeSetFirstKey) // \x00
	m.boundaries = append(m.boundaries, pkBoundaries...)
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
	rng := rand.New(rand.NewSource(mutualFragmentSeed(m.indexer.db.Env())))
	m.fragmentFirst = rng.Intn(m.fragmentCount)
	m.fragmentCur = m.fragmentFirst
	m.fragmentStep = findCoprimeStep(m.fragmentCount, rng)
}

// mutualFragmentSeed picks the seed for a mutual build's fragment visit order.
//
// The order is not cosmetic under fault injection, which is the whole point of a simulation.
// A build that runs to completion indexes every fragment and lands on the same index whatever
// order it walked them in — that much is true. A build INTERRUPTED partway has recorded a
// different SUBSET of fragments in the persisted built-range set depending on where the walk
// had got to, and the range set is durable state a later builder resumes from. So the order
// decides persisted bytes exactly when a fault schedule is in play, and a seeded run has to be
// able to reproduce it.
//
// A nil env keeps the raw math/rand draw: this site has always used math/rand, and routing the
// production arm through the crypto-backed nil default would change production behaviour rather
// than make it more deterministic.
func mutualFragmentSeed(env *dst.Env) int64 {
	if env == nil {
		return rand.Int63()
	}
	var b [8]byte
	if _, err := env.Read(b[:]); err != nil {
		return rand.Int63()
	}
	// Clear the sign bit: rand.NewSource takes a seed, and a negative one is legal but makes
	// the seed's relationship to the drawn bytes needlessly hard to read in a repro.
	return int64(binary.BigEndian.Uint64(b[:]) &^ (1 << 63))
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

// buildMutual builds one chunk of the claimed range, claiming the next range to build
// when none is claimed. It returns the records scanned and whether work remains.
func (m *mutualIndexBuilder) buildMutual(ctx context.Context) (int64, bool, error) {
	for {
		if m.claimed == nil {
			more, err := m.claimNextRange(ctx)
			if err != nil || !more {
				return 0, more, err
			}
		}
		n, built, err := m.buildClaimedRange(ctx)
		if err != nil {
			return n, false, err
		}
		if built {
			return n, true, nil
		}
	}
}

// claimNextRange claims the next range to build in a read-only transaction, Java's buildRangeOnly.
// It reports false when nothing is missing, and keeps the claim only if the transaction commits.
func (m *mutualIndexBuilder) claimNextRange(ctx context.Context) (bool, error) {
	next := *m
	var more bool
	_, err := m.indexer.run(ctx, func(rtx *FDBRecordContext) (any, error) {
		attempt := *m
		m := &attempt
		defer func() { next = *m }()
		more = false

		store, err := m.indexer.openStore(rtx)
		if err != nil {
			return nil, err
		}
		if err := m.indexer.expectedIndexStatesOrThrow(store); err != nil {
			return nil, err
		}
		if m.boundaries == nil {
			if err := m.detectBoundaries(store); err != nil {
				return nil, err
			}
		}
		missing, err := NewIndexingRangeSet(store.subspace, m.indexer.primaryIndex()).ListMissingRanges(rtx.Transaction())
		if err != nil || len(missing) == 0 {
			return nil, err
		}
		more = true
		m.claimed, err = m.nextClaim(missing)
		return nil, err
	})
	if err == nil {
		*m = next
	}
	return more, err
}

// nextClaim walks the fragments for a range to build, Java's buildNextRangeOnly. Each walk back
// to the first fragment moves on a phase: FULL to ANY, then ANY to RECOVER's failure.
func (m *mutualIndexBuilder) nextClaim(missing []RangeSetRange) (*RangeSetRange, error) {
	for {
		begin, end := m.fragmentRange()
		var r *RangeSetRange
		if m.iterType == fragmentFull {
			r = fullyUnbuiltRange(missing, begin, end)
		} else {
			r = partlyUnbuiltRange(missing, begin, end)
		}
		if r != nil {
			return r, nil
		}
		if m.fragmentAdvance() {
			m.iterType++
			if m.iterType == fragmentRecover {
				return nil, &IndexingValidationError{Message: "Mutual indexing failure - third iteration"}
			}
		}
	}
}

// fullyUnbuiltRange returns the fragment [begin, end) when a missing range covers it, Java's
// fullyUnBuiltRange. missing is sorted and disjoint, as ListMissingRanges returns it.
func fullyUnbuiltRange(missing []RangeSetRange, begin, end []byte) *RangeSetRange {
	for _, r := range missing {
		if bytes.Compare(r.End, end) >= 0 {
			if bytes.Compare(r.Begin, begin) <= 0 && bytes.Compare(begin, end) < 0 {
				return &RangeSetRange{Begin: begin, End: end}
			}
			break
		}
	}
	return nil
}

// partlyUnbuiltRange returns the first non-empty overlap of a missing range with the fragment
// [begin, end), Java's partlyUnBuiltRange.
func partlyUnbuiltRange(missing []RangeSetRange, begin, end []byte) *RangeSetRange {
	for _, r := range missing {
		if bytes.Compare(r.Begin, end) > 0 || bytes.Compare(r.End, begin) < 0 {
			continue
		}
		lo, hi := r.Begin, r.End
		if bytes.Compare(lo, begin) < 0 {
			lo = begin
		}
		if bytes.Compare(hi, end) > 0 {
			hi = end
		}
		if bytes.Compare(lo, hi) < 0 {
			return &RangeSetRange{Begin: lo, End: hi}
		}
	}
	return nil
}

// buildClaimedRange builds the claim's first missing chunk, Java's buildThisRangeOnly. built is
// false when nothing in the claim was missing; a claim with nothing left is dropped.
func (m *mutualIndexBuilder) buildClaimedRange(ctx context.Context) (n int64, built bool, err error) {
	var exhausted bool
	var mergeRequests []*Index
	_, err = m.indexer.run(ctx, func(rtx *FDBRecordContext) (any, error) {
		n, built, exhausted, mergeRequests = 0, false, false, nil
		started := rtx.Env().Now()

		store, err := m.indexer.openStore(rtx)
		if err != nil {
			return nil, err
		}
		if err := m.indexer.validateBuildSession(store); err != nil {
			return nil, err
		}
		// validateBuildSession already renewed the shared admitted session. The
		// standalone internal builder still needs its own compatibility check.
		if m.heartbeat != m.indexer.sessionHeartbeat {
			for _, index := range m.indexer.targetIndexes {
				if err := m.indexer.renewSessionHeartbeat(store, index, m.heartbeat); err != nil {
					return nil, err
				}
			}
		}

		primaryRangeSet := NewIndexingRangeSet(store.subspace, m.indexer.primaryIndex())
		missing, err := primaryRangeSet.FirstMissingRangeInBytes(rtx.Transaction(), m.claimed.Begin, m.claimed.End)
		if err != nil {
			return nil, err
		}
		if missing == nil {
			exhausted = true
			return nil, nil
		}
		var more bool
		if n, more, err = m.buildFragmentRange(ctx, store, started, missing); err != nil {
			return nil, err
		}
		built = true
		// Missing ranges are disjoint, so one built to the claim's end was its last.
		exhausted = !more && bytes.Equal(missing.End, m.claimed.End)
		mergeRequests = store.GetIndexDeferredMaintenanceControl().GetMergeRequiredIndexes()
		return nil, nil
	})
	if err != nil {
		return n, false, err
	}
	if exhausted {
		m.claimed = nil
	}
	m.indexer.mergeRequiredIndexes = mergeRequests
	return n, built, nil
}

// buildFragmentRange indexes r from its start until the record limit or the transaction's time
// or write limit, and marks what it indexed built; more reports that it stopped before r's end.
func (m *mutualIndexBuilder) buildFragmentRange(ctx context.Context, store *FDBRecordStore, started time.Time, r *RangeSetRange) (int64, bool, error) {
	var rangeStart, rangeEnd tuple.Tuple
	lowEp := EndpointTypeRangeInclusive
	highEp := EndpointTypeRangeExclusive

	if bytes.Equal(r.Begin, rangeSetFirstKey) {
		lowEp = EndpointTypeTreeStart
	} else {
		var err error
		if rangeStart, err = tuplefast.Unpack(r.Begin); err != nil {
			return 0, false, fmt.Errorf("mutual indexer: unpack range begin: %w", err)
		}
	}
	// The end may be a tuple+0xff (RANGE_INCLUSIVE high) written by the typed-records preset.
	if bytes.Equal(r.End, rangeSetFinalKey) {
		highEp = EndpointTypeTreeEnd
	} else {
		var err error
		if rangeEnd, highEp, err = unpackRangeEndBoundary(r.End); err != nil {
			return 0, false, fmt.Errorf("mutual indexer: unpack range end: %w", err)
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
	// reachedLimits: the last record indexed took the transaction past its
	// time or write limit, so the next record ends the range.
	var reachedLimits bool

	for rec, iterErr := range Seq2(cursor, ctx) {
		if iterErr != nil {
			return 0, false, iterErr
		}
		scannedCount++
		if scannedCount > m.indexer.limit || reachedLimits {
			extraPK = rec.PrimaryKey
			break
		}

		indexed, err := m.indexer.indexScannedRecord(store, rec)
		if err != nil {
			return 0, false, err
		}
		if indexed {
			if reachedLimits, err = m.indexer.transactionReachedLimits(store.context, started); err != nil {
				return 0, false, err
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
		if err := insertIndexBuildRange(idxRangeSet, store.context.Transaction(), idx, beginKey, endKey); err != nil {
			return 0, false, err
		}

	}

	// Track progress.
	if recordsProcessed > 0 {
		for _, idx := range m.indexer.targetIndexes {
			store.AddBuildProgress(idx, recordsProcessed)
		}
	}

	return recordsProcessed, extraPK != nil, nil
}

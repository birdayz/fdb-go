package recordlayer

import (
	"fmt"
	"strconv"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// RankQuerier is the public interface for rank-based queries on a RANK index maintainer.
type RankQuerier interface {
	RankForScore(score tuple.Tuple, nullsAreLow bool) (*int64, error)
	ScoreForRank(rank tuple.Tuple) (tuple.Tuple, error)
}

// rankIndexMaintainer maintains a RANK index.
// A RANK index has two subspaces:
//   - Primary (B-tree): standard VALUE index on [group..., score...]
//   - Secondary: a RankedSet per group for rank/select queries
//
// Matches Java's rankIndexMaintainer.
type rankIndexMaintainer struct {
	standardIndexMaintainer                   // embedded — primary B-tree operations
	secondarySubspace       subspace.Subspace // ranked sets per group
	rankedSetConfig         rankedSetConfig
}

// Index option keys for RANK indexes.
// Matches Java's IndexOptions.RANK_*.
const (
	IndexOptionRankNLevels         = "rankNLevels"
	IndexOptionRankHashFunction    = "rankHashFunction"
	IndexOptionRankCountDuplicates = "rankCountDuplicates"
)

func newRankIndexMaintainer(
	index *Index,
	indexSubspace, secondarySubspace subspace.Subspace,
	tx fdb.Transaction,
	store indexStoreContext,
) *rankIndexMaintainer {
	return &rankIndexMaintainer{
		standardIndexMaintainer: *newStandardIndexMaintainer(index, indexSubspace, tx, store),
		secondarySubspace:       secondarySubspace,
		rankedSetConfig:         parseRankedSetConfig(index),
	}
}

// parseRankedSetConfig reads RankedSet configuration from index options.
// Matches Java's RankedSetIndexHelper.getConfig().
func parseRankedSetConfig(index *Index) rankedSetConfig {
	config := defaultRankedSetConfig
	if v, ok := index.Options[IndexOptionRankNLevels]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			config.NLevels = n
		}
	}
	if v, ok := index.Options[IndexOptionRankHashFunction]; ok {
		switch v {
		case "CRC":
			config.HashFunction = crcHash
		default:
			config.HashFunction = jdkArrayHash
		}
	}
	if v, ok := index.Options[IndexOptionRankCountDuplicates]; ok {
		config.CountDuplicates = v == "true"
	}
	return config
}

// DeleteWhere clears both the primary B-tree and secondary ranked set entries
// for the given prefix. Matches Java's rankIndexMaintainer.deleteWhere().
func (m *rankIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	// Clear primary (B-tree) entries.
	if err := m.standardIndexMaintainer.DeleteWhere(prefix); err != nil {
		return err
	}
	// Clear secondary (ranked set) entries.
	key := m.secondarySubspace.Pack(prefix)
	pr, err := fdb.PrefixRange(key)
	if err != nil {
		return fmt.Errorf("rankIndexMaintainer.DeleteWhere: PrefixRange(%x): %w", key, err)
	}
	m.tx.ClearRange(pr)
	return nil
}

// Update handles insert/delete/update for the RANK index.
// Maintains both the primary B-tree and the secondary ranked set.
// Acquires write lock because the ranked set does read-modify-write on the
// skip list structure — concurrent updates cause lost updates.
// Matches Java where per-record index updates are serialized via the
// CompletableFuture pipeline.
func (m *rankIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	lockKey := string(m.secondarySubspace.Bytes())
	m.store.AcquireWriteLock(lockKey)
	defer m.store.ReleaseWriteLock(lockKey)
	var oldEntries, newEntries []indexEntry

	if oldRecord != nil {
		entries, err := m.evaluateIndex(oldRecord)
		if err != nil {
			return fmt.Errorf("evaluate index %q for old record: %w", m.index.Name, err)
		}
		oldEntries = entries
	}
	if newRecord != nil {
		entries, err := m.evaluateIndex(newRecord)
		if err != nil {
			return fmt.Errorf("evaluate index %q for new record: %w", m.index.Name, err)
		}
		newEntries = entries
	}

	if oldEntries != nil && newEntries != nil {
		var err error
		oldEntries, newEntries, err = removeCommonEntries(m.index, oldEntries, newEntries)
		if err != nil {
			return err
		}
	}

	isWriteOnlyOrUniquePending := m.store != nil &&
		(m.store.isIndexWriteOnly(m.index) || m.store.isIndexReadableUniquePending(m.index))

	// Process removes: clear B-tree + update ranked set.
	for i := range oldEntries {
		oldEntryKey, err := indexEntryKey(m.index, oldEntries[i].key, oldEntries[i].primaryKey)
		if err != nil {
			return err
		}
		m.tx.ClearBytes(m.indexSubspace.Pack(oldEntryKey))
		if isWriteOnlyOrUniquePending && m.index.IsUnique() && m.store != nil {
			if err := m.store.removeUniquenessViolations(m.index, oldEntries[i].key, oldEntries[i].primaryKey); err != nil {
				return err
			}
		}
		if err := m.updateRankedSet(oldEntries[i], true); err != nil {
			return err
		}
	}

	// Process inserts: set B-tree + update ranked set.
	valueBytes := tuple.Tuple{}.Pack()
	for i := range newEntries {
		entryTupleKey, err := indexEntryKey(m.index, newEntries[i].key, newEntries[i].primaryKey)
		if err != nil {
			return err
		}
		keyBytes := m.indexSubspace.Pack(entryTupleKey)

		if err := checkKeyValueSizes(m.index, newEntries[i].primaryKey, keyBytes, valueBytes); err != nil {
			return err
		}
		if m.index.IsUnique() && !indexKeyContainsNull(newEntries[i].key) {
			if err := m.checkUniqueness(newEntries[i]); err != nil {
				return err
			}
		}

		m.tx.SetBytes(keyBytes, valueBytes)

		if err := m.updateRankedSet(newEntries[i], false); err != nil {
			return err
		}
	}

	return nil
}

// UpdateWhileWriteOnly handles updates during WRITE_ONLY state.
// RANK is idempotent when !CountDuplicates.
func (m *rankIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	if !m.rankedSetConfig.CountDuplicates {
		return m.Update(oldRecord, newRecord) // idempotent
	}
	// Non-idempotent: check range set before updating.
	// Use oldRecord's PK when available (for deletes), fall back to newRecord.
	// Matches Java's rankIndexMaintainer.updateWriteOnlyByRecords().
	var checkRecord *FDBStoredRecord[proto.Message]
	if oldRecord != nil {
		checkRecord = oldRecord
	} else {
		checkRecord = newRecord
	}
	if checkRecord != nil && m.store != nil {
		inRange, err := m.store.isKeyInIndexBuildRange(m.index, checkRecord.PrimaryKey)
		if err != nil {
			return err
		}
		if !inRange {
			return nil // PK not yet built — skip
		}
	}
	return m.Update(oldRecord, newRecord)
}

// Scan scans the primary B-tree (BY_VALUE).
// Inherited from standardIndexMaintainer — no override needed.

// ScanByRank converts a rank range to a score range, then scans BY_VALUE.
// The rank range tuples have format [group..., rank] where rank is an int64.
// Matches Java's rankIndexMaintainer.scan(BY_RANK, ...).
func (m *rankIndexMaintainer) ScanByRank(
	rankRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	scoreRange, err := m.rankRangeToScoreRange(rankRange)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: err}
	}
	if scoreRange == nil {
		return Empty[*IndexEntry]()
	}
	return m.standardIndexMaintainer.Scan(*scoreRange, continuation, scanProperties)
}

// updateRankedSet adds or removes a score from the ranked set for the entry's group.
// On remove with !CountDuplicates, only removes if no other B-tree entry has this score.
// Matches Java's RankedSetIndexHelper.updateRankedSet().
func (m *rankIndexMaintainer) updateRankedSet(entry indexEntry, remove bool) error {
	groupPrefixSize := m.getGroupingCount()

	var rankSubspace subspace.Subspace
	var scoreKey tuple.Tuple

	if groupPrefixSize > 0 && groupPrefixSize <= len(entry.key) {
		groupPrefix := tuple.Tuple(entry.key[:groupPrefixSize])
		rankSubspace = m.secondarySubspace.Sub(groupPrefix...)
		scoreKey = tuple.Tuple(entry.key[groupPrefixSize:])
	} else {
		rankSubspace = m.secondarySubspace
		scoreKey = entry.key
	}

	rankedSet := newRankedSet(rankSubspace, m.rankedSetConfig)
	score := scoreKey.Pack()

	// Init if needed — use snapshot check to avoid conflicts with atomic mutations.
	needed, err := rankedSet.InitNeeded(m.tx.Snapshot())
	if err != nil {
		return err
	}
	if needed {
		if err := rankedSet.Init(m.tx); err != nil {
			return err
		}
	}

	if remove {
		if m.rankedSetConfig.CountDuplicates {
			exists, err := rankedSet.Remove(m.tx, score)
			if err != nil {
				return err
			}
			if !exists && m.store != nil && !m.store.isIndexWriteOnly(m.index) {
				return fmt.Errorf("rank index %q: score not present in ranked set", m.index.Name)
			}
		} else {
			// Only remove from ranked set if no other record has this score.
			// Check the B-tree (after our clear) for remaining entries.
			valueKey := entry.key
			prefixBytes := m.indexSubspace.Pack(valueKey)
			r, err := fdb.PrefixRange(prefixBytes)
			if err != nil {
				return err
			}
			kvs, err := m.tx.GetRange(r, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
			if err != nil {
				return err
			}
			if len(kvs) == 0 {
				exists, err := rankedSet.Remove(m.tx, score)
				if err != nil {
					return err
				}
				if !exists && m.store != nil && !m.store.isIndexWriteOnly(m.index) {
					return fmt.Errorf("rank index %q: score not present in ranked set", m.index.Name)
				}
			}
		}
	} else {
		if _, err := rankedSet.Add(m.tx, score); err != nil {
			return err
		}
	}
	return nil
}

// rankRangeToScoreRange converts rank endpoints to score endpoints.
// Returns nil if the range is empty. Matches Java's RankedSetIndexHelper.rankRangeToScoreRange().
func (m *rankIndexMaintainer) rankRangeToScoreRange(rankRange TupleRange) (*TupleRange, error) {
	groupPrefixSize := m.getGroupingCount()

	var prefix tuple.Tuple
	var rankSubspace subspace.Subspace

	if groupPrefixSize > 0 {
		if rankRange.Low == nil || len(rankRange.Low) < groupPrefixSize ||
			rankRange.High == nil || len(rankRange.High) < groupPrefixSize {
			return nil, fmt.Errorf("rank scan range must include group (size %d)", groupPrefixSize)
		}
		prefix = tuple.Tuple(rankRange.Low[:groupPrefixSize])
		highPrefix := tuple.Tuple(rankRange.High[:groupPrefixSize])
		if !tuplesEqual(prefix, highPrefix) {
			return nil, fmt.Errorf("rank scan range crosses groups")
		}
		rankSubspace = m.secondarySubspace.Sub(prefix...)
	} else {
		rankSubspace = m.secondarySubspace
	}

	lowRank, err := extractRankValue(groupPrefixSize, rankRange.Low)
	if err != nil {
		return nil, err
	}
	highRank, err := extractRankValue(groupPrefixSize, rankRange.High)
	if err != nil {
		return nil, err
	}

	startFromBeginning := lowRank == nil || *lowRank < 0
	lowEndpoint := rankRange.LowEndpoint
	if startFromBeginning {
		lowEndpoint = EndpointTypeRangeInclusive
	}

	highEndpoint := rankRange.HighEndpoint
	if highRank != nil {
		if *highRank < 0 || (highEndpoint == EndpointTypeRangeExclusive && *highRank == 0) {
			return nil, nil // Empty range
		}
	}
	if highRank != nil && lowRank != nil &&
		highEndpoint == EndpointTypeRangeExclusive &&
		lowEndpoint == EndpointTypeRangeExclusive &&
		*highRank == *lowRank {
		return nil, nil // Exclusively empty
	}

	if startFromBeginning && highRank == nil {
		result := TupleRangeAllOf(prefix)
		return &result, nil
	}

	rankedSet := newRankedSet(rankSubspace, m.rankedSetConfig)

	// Init if needed.
	needed, err := rankedSet.InitNeeded(m.tx.Snapshot())
	if err != nil {
		return nil, err
	}
	if needed {
		if err := rankedSet.Init(m.tx); err != nil {
			return nil, err
		}
	}

	// Prefetch sparse upper skip-list levels for the upcoming GetNth calls.
	rankedSet.PreloadForLookup(m.tx)

	// Convert low rank to score.
	var lowScore tuple.Tuple
	lowRankVal := int64(0)
	if !startFromBeginning {
		lowRankVal = *lowRank
	}
	lowScoreBytes, err := rankedSet.GetNth(m.tx, lowRankVal)
	if err != nil {
		return nil, err
	}
	if lowScoreBytes != nil {
		lowScore, err = fastUnpack(lowScoreBytes)
		if err != nil {
			return nil, fmt.Errorf("unpack low score: %w", err)
		}
	}
	if lowScore == nil {
		return nil, nil // Low rank past end.
	}

	// Convert high rank to score.
	var highScore tuple.Tuple
	if highRank != nil {
		highScoreBytes, err := rankedSet.GetNth(m.tx, *highRank)
		if err != nil {
			return nil, err
		}
		if highScoreBytes != nil {
			highScore, err = fastUnpack(highScoreBytes)
			if err != nil {
				return nil, fmt.Errorf("unpack high score: %w", err)
			}
		}
	}

	adjustedHighEndpoint := highEndpoint
	if highScore == nil {
		if prefix != nil {
			adjustedHighEndpoint = EndpointTypeRangeInclusive
		} else {
			adjustedHighEndpoint = EndpointTypeTreeEnd
		}
	}

	scoreRange := TupleRange{
		Low:          lowScore,
		High:         highScore,
		LowEndpoint:  lowEndpoint,
		HighEndpoint: adjustedHighEndpoint,
	}

	// Prepend group prefix to score range.
	if prefix != nil {
		scoreRange = scoreRange.Prepend(prefix)
	}

	return &scoreRange, nil
}

// getGroupingCount returns the number of grouping columns in the index expression.
func (m *rankIndexMaintainer) getGroupingCount() int {
	if g, ok := m.index.RootExpression.(*GroupingKeyExpression); ok {
		return g.GetGroupingCount()
	}
	return 0
}

// RankForScore returns the rank of a given score in the ranked set for the
// specified group. Returns nil if the score is not present and nullIfMissing is true.
func (m *rankIndexMaintainer) RankForScore(groupAndScore tuple.Tuple, nullIfMissing bool) (*int64, error) {
	groupPrefixSize := m.getGroupingCount()

	var rankSubspace subspace.Subspace
	var scoreTuple tuple.Tuple

	if groupPrefixSize > 0 && groupPrefixSize <= len(groupAndScore) {
		prefix := tuple.Tuple(groupAndScore[:groupPrefixSize])
		rankSubspace = m.secondarySubspace.Sub(prefix...)
		scoreTuple = tuple.Tuple(groupAndScore[groupPrefixSize:])
	} else {
		rankSubspace = m.secondarySubspace
		scoreTuple = groupAndScore
	}

	rankedSet := newRankedSet(rankSubspace, m.rankedSetConfig)
	rankedSet.PreloadForLookup(m.tx)
	return rankedSet.Rank(m.tx, scoreTuple.Pack(), nullIfMissing)
}

// ScoreForRank returns the score at the given rank in the ranked set for the
// specified group. Returns nil if rank is out of bounds.
func (m *rankIndexMaintainer) ScoreForRank(groupAndRank tuple.Tuple) (tuple.Tuple, error) {
	groupPrefixSize := m.getGroupingCount()

	var rankSubspace subspace.Subspace
	var rank int64

	if groupPrefixSize > 0 && groupPrefixSize < len(groupAndRank) {
		prefix := tuple.Tuple(groupAndRank[:groupPrefixSize])
		rankSubspace = m.secondarySubspace.Sub(prefix...)
		rankVal, ok := groupAndRank[groupPrefixSize].(int64)
		if !ok {
			return nil, fmt.Errorf("rank index: expected int64 rank, got %T", groupAndRank[groupPrefixSize])
		}
		rank = rankVal
	} else if len(groupAndRank) > 0 {
		rankSubspace = m.secondarySubspace
		rankVal, ok := groupAndRank[0].(int64)
		if !ok {
			return nil, fmt.Errorf("rank index: expected int64 rank, got %T", groupAndRank[0])
		}
		rank = rankVal
	} else {
		return nil, fmt.Errorf("rank index: empty group and rank tuple")
	}

	rankedSet := newRankedSet(rankSubspace, m.rankedSetConfig)
	rankedSet.PreloadForLookup(m.tx)
	scoreBytes, err := rankedSet.GetNth(m.tx, rank)
	if err != nil {
		return nil, err
	}
	if scoreBytes == nil {
		return nil, nil
	}
	return fastUnpack(scoreBytes)
}

// extractRankValue extracts the rank value from a scan range tuple.
// The rank is at position groupPrefixSize (after the group prefix).
// Returns error if the tuple has unexpected extra elements.
// Matches Java's RankedSetIndexHelper.extractRank().
func extractRankValue(groupPrefixSize int, maybeTuple tuple.Tuple) (*int64, error) {
	if maybeTuple == nil {
		return nil, nil
	}
	if len(maybeTuple) == groupPrefixSize+1 {
		switch v := maybeTuple[groupPrefixSize].(type) {
		case int64:
			return &v, nil
		case int:
			r := int64(v)
			return &r, nil
		}
		return nil, nil
	}
	if len(maybeTuple) <= groupPrefixSize {
		return nil, nil
	}
	return nil, fmt.Errorf("ranked set range bound is not correct size: groupPrefixSize=%d, tuple=%v", groupPrefixSize, maybeTuple)
}

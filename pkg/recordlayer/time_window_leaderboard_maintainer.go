package recordlayer

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// timeWindowLeaderboardIndexMaintainer maintains a TIME_WINDOW_LEADERBOARD index.
// Like RANK, it has two subspaces:
//   - Primary (B-tree): standard VALUE index with leaderboard subspace key prepended
//   - Secondary: directory proto at root, sub-directory protos per group, ranked sets per (leaderboard, group)
//
// Matches Java's TimeWindowLeaderboardIndexMaintainer.
type timeWindowLeaderboardIndexMaintainer struct {
	standardIndexMaintainer
	secondarySubspace subspace.Subspace
	rankedSetConfig   rankedSetConfig
}

func newTimeWindowLeaderboardIndexMaintainer(
	index *Index,
	indexSubspace, secondarySubspace subspace.Subspace,
	tx fdb.Transaction,
	store indexStoreContext,
) *timeWindowLeaderboardIndexMaintainer {
	return &timeWindowLeaderboardIndexMaintainer{
		standardIndexMaintainer: *newStandardIndexMaintainer(index, indexSubspace, tx, store),
		secondarySubspace:       secondarySubspace,
		rankedSetConfig:         parseRankedSetConfig(index),
	}
}

// Update handles insert/delete/update for the TIME_WINDOW_LEADERBOARD index.
// For each record, finds the best score per leaderboard and updates B-tree + RankedSet.
// Matches Java's TimeWindowLeaderboardIndexMaintainer.updateIndexKeys().
func (m *timeWindowLeaderboardIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
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

	// commonKeys override: all or nothing.
	if oldEntries != nil && newEntries != nil {
		if indexEntriesEqual(oldEntries, newEntries) {
			return nil // completely unchanged
		}
		// ANY change → redo everything (don't remove common entries).
	}

	dir, err := loadLeaderboardDirectory(m.tx, m.secondarySubspace)
	if err != nil {
		return err
	}
	if dir == nil {
		return nil // no leaderboards configured
	}

	// Process old entries (remove).
	if err := m.updateLeaderboardEntries(dir, oldEntries, true); err != nil {
		return err
	}

	// Process new entries (insert).
	if err := m.updateLeaderboardEntries(dir, newEntries, false); err != nil {
		return err
	}

	return nil
}

// updateLeaderboardEntries processes insert or remove for a set of index entries
// across all leaderboards.
func (m *timeWindowLeaderboardIndexMaintainer) updateLeaderboardEntries(
	dir *leaderboardDirectory,
	entries []indexEntry,
	remove bool,
) error {
	if len(entries) == 0 {
		return nil
	}

	groupPrefixSize := m.getGroupingCount()

	// Group and order scores.
	groupedScores, latestTimestamp, err := m.groupOrderedScoreIndexKeys(dir, entries, groupPrefixSize)
	if err != nil {
		return err
	}

	allLeaderboards := dir.allLeaderboards()
	valueBytes := tuple.Tuple{}.Pack()

	for _, lb := range allLeaderboards {
		for _, gs := range groupedScores {
			// Find best contained score for this leaderboard.
			bestScore := m.bestContainedScore(lb, gs.scores)
			if bestScore == nil {
				continue
			}

			leaderboardGroupKey := make(tuple.Tuple, 0, len(lb.SubspaceKey)+len(gs.groupKey))
			leaderboardGroupKey = append(leaderboardGroupKey, lb.SubspaceKey...)
			leaderboardGroupKey = append(leaderboardGroupKey, gs.groupKey...)

			// Build entry key: [leaderboardSubspaceKey, group..., score..., trimmedPK...]
			entryKey, err := indexEntryKey(m.index, tupleConcat(leaderboardGroupKey, bestScore.scoreKey), bestScore.entry.primaryKey)
			if err != nil {
				return err
			}

			// Update B-tree.
			keyBytes := m.indexSubspace.Pack(entryKey)
			if remove {
				m.tx.Clear(fdb.Key(keyBytes))
			} else {
				if err := checkKeyValueSizes(m.index, bestScore.entry.primaryKey, keyBytes, valueBytes); err != nil {
					return err
				}
				m.tx.Set(fdb.Key(keyBytes), valueBytes)
			}

			// Update RankedSet.
			config := m.rankedSetConfig
			if lb.NLevels > 0 {
				config.NLevels = lb.NLevels
			}
			if err := m.updateRankedSet(leaderboardGroupKey, bestScore.entry, bestScore.scoreKey, remove, config); err != nil {
				return err
			}
		}
	}

	// Track latest timestamp via atomic MAX.
	if !remove && latestTimestamp != nil {
		m.tx.Max(m.indexSubspace.FDBKey(), encodeSignedLong(*latestTimestamp))
	}

	return nil
}

// updateRankedSet adds or removes a score from the ranked set for the given leaderboard+group.
func (m *timeWindowLeaderboardIndexMaintainer) updateRankedSet(
	leaderboardGroupKey tuple.Tuple,
	entry indexEntry,
	scoreKey tuple.Tuple,
	remove bool,
	config rankedSetConfig,
) error {
	rankSubspace := m.secondarySubspace.Sub(leaderboardGroupKey...)
	rankedSet := newRankedSet(rankSubspace, config)
	score := scoreKey.Pack()

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
			if _, err := rankedSet.Remove(m.tx, score); err != nil {
				return err
			}
		} else {
			// Only remove from ranked set if no other record has this score.
			fullKey, err := indexEntryKey(m.index, tupleConcat(leaderboardGroupKey, scoreKey), nil)
			if err != nil {
				// Fall back: just remove.
				if _, err := rankedSet.Remove(m.tx, score); err != nil {
					return err
				}
				return nil
			}
			prefixBytes := m.indexSubspace.Pack(fullKey)
			r, err := fdb.PrefixRange(prefixBytes)
			if err != nil {
				return err
			}
			kvs, err := m.tx.GetRange(r, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
			if err != nil {
				return err
			}
			if len(kvs) == 0 {
				if _, err := rankedSet.Remove(m.tx, score); err != nil {
					return err
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

// groupedScores holds a group key and its ordered score entries.
type groupedScores struct {
	groupKey tuple.Tuple
	scores   []orderedScoreIndexKey
}

// groupOrderedScoreIndexKeys groups entries by their group key and orders scores.
// Returns grouped scores and the latest timestamp seen.
// Matches Java's groupOrderedScoreIndexKeys().
func (m *timeWindowLeaderboardIndexMaintainer) groupOrderedScoreIndexKeys(
	dir *leaderboardDirectory,
	entries []indexEntry,
	groupPrefixSize int,
) ([]groupedScores, *int64, error) {
	grouped := make(map[string]*groupedScores)
	var latestTimestamp *int64

	for i := range entries {
		entry := entries[i]
		scoreKey := entry.key
		var groupKey tuple.Tuple

		if groupPrefixSize > 0 && groupPrefixSize <= len(scoreKey) {
			groupKey = tuple.Tuple(scoreKey[:groupPrefixSize])
			scoreKey = tuple.Tuple(scoreKey[groupPrefixSize:])
		}

		// Resolve per-group highScoreFirst.
		highScoreFirst, err := m.isHighScoreFirst(dir, groupKey)
		if err != nil {
			return nil, nil, err
		}

		if highScoreFirst {
			scoreKey = negateScore(scoreKey, 0)
		}

		// Extract timestamp (position 1 in score key).
		if len(scoreKey) > 1 {
			if ts, ok := asInt64(scoreKey[1]); ok {
				if latestTimestamp == nil || ts > *latestTimestamp {
					latestTimestamp = &ts
				}
			}
		}

		gk := string(groupKey.Pack())
		if _, exists := grouped[gk]; !exists {
			grouped[gk] = &groupedScores{groupKey: groupKey}
		}
		grouped[gk].scores = append(grouped[gk].scores, orderedScoreIndexKey{
			entry:    entry,
			scoreKey: scoreKey,
		})
	}

	// Sort each group's scores ascending.
	var result []groupedScores
	for _, gs := range grouped {
		sort.Slice(gs.scores, func(i, j int) bool {
			return tupleCompare(gs.scores[i].scoreKey, gs.scores[j].scoreKey) < 0
		})
		result = append(result, *gs)
	}
	return result, latestTimestamp, nil
}

// bestContainedScore finds the first (best) score whose timestamp falls within
// the leaderboard's time window.
func (m *timeWindowLeaderboardIndexMaintainer) bestContainedScore(
	lb *leaderboard,
	scores []orderedScoreIndexKey,
) *orderedScoreIndexKey {
	for i := range scores {
		if len(scores[i].scoreKey) > 1 {
			if ts, ok := asInt64(scores[i].scoreKey[1]); ok {
				if lb.containsTimestamp(ts) {
					return &scores[i]
				}
			}
		}
	}
	return nil
}

// UpdateWhileWriteOnly handles updates during WRITE_ONLY state.
// Matches Java's TIME_WINDOW_LEADERBOARD idempotency: !CountDuplicates → idempotent.
func (m *timeWindowLeaderboardIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	if !m.rankedSetConfig.CountDuplicates {
		return m.Update(oldRecord, newRecord)
	}
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
			return nil
		}
	}
	return m.Update(oldRecord, newRecord)
}

// DeleteWhere clears both B-tree and RankedSet entries for all leaderboards
// matching the given prefix.
// Matches Java's TimeWindowLeaderboardIndexMaintainer.deleteWhere().
func (m *timeWindowLeaderboardIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	dir, err := loadLeaderboardDirectory(m.tx, m.secondarySubspace)
	if err != nil {
		return err
	}
	if dir == nil {
		return nil
	}

	for _, lb := range dir.allLeaderboards() {
		leaderboardGroupKey := make(tuple.Tuple, 0, len(lb.SubspaceKey)+len(prefix))
		leaderboardGroupKey = append(leaderboardGroupKey, lb.SubspaceKey...)
		leaderboardGroupKey = append(leaderboardGroupKey, prefix...)

		// Clear B-tree entries using strinc (not Subspace.range()).
		indexKey := m.indexSubspace.Pack(leaderboardGroupKey)
		inc, err := fdb.Strinc(indexKey)
		if err == nil {
			m.tx.ClearRange(fdb.KeyRange{Begin: fdb.Key(indexKey), End: fdb.Key(inc)})
		}

		// Clear ranked set entries.
		ranksetKey := m.secondarySubspace.Pack(leaderboardGroupKey)
		rinc, err := fdb.Strinc(ranksetKey)
		if err == nil {
			m.tx.ClearRange(fdb.KeyRange{Begin: fdb.Key(ranksetKey), End: fdb.Key(rinc)})
		}
	}
	return nil
}

// Scan dispatches to the appropriate scan based on the scan type embedded in the range.
// For standard scans (called via ScanIndex), scans the all-time leaderboard.
// For typed scans (called via ScanIndexByType), handles BY_TIME_WINDOW, BY_RANK, BY_VALUE.
func (m *timeWindowLeaderboardIndexMaintainer) Scan(
	scanRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	// Standard scan → all-time leaderboard BY_VALUE.
	return m.scanWithTimeWindow(AllTimeLeaderboardType, 0, scanRange, false, continuation, scanProperties)
}

// ScanByTimeWindow scans a specific time window.
// The scanRange contains the score range, leaderboard type and timestamp are provided separately.
// Matches Java's scan(BY_TIME_WINDOW, ...).
func (m *timeWindowLeaderboardIndexMaintainer) ScanByTimeWindow(
	leaderboardType int,
	leaderboardTimestamp int64,
	scanRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	return m.scanWithTimeWindow(leaderboardType, leaderboardTimestamp, scanRange, false, continuation, scanProperties)
}

// ScanByRank converts a rank range to a score range and scans.
// Matches Java's scan(BY_RANK, ...) for TIME_WINDOW_LEADERBOARD.
func (m *timeWindowLeaderboardIndexMaintainer) ScanByRankInTimeWindow(
	leaderboardType int,
	leaderboardTimestamp int64,
	rankRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	return m.scanWithTimeWindow(leaderboardType, leaderboardTimestamp, rankRange, true, continuation, scanProperties)
}

// scanWithTimeWindow is the core scan implementation.
func (m *timeWindowLeaderboardIndexMaintainer) scanWithTimeWindow(
	leaderboardType int,
	leaderboardTimestamp int64,
	scanRange TupleRange,
	isRankScan bool,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	dir, err := loadLeaderboardDirectory(m.tx, m.secondarySubspace)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: err}
	}
	if dir == nil {
		return Empty[*IndexEntry]()
	}

	lb := dir.oldestLeaderboardMatching(leaderboardType, leaderboardTimestamp)
	if lb == nil {
		return Empty[*IndexEntry]()
	}

	scoreRange := scanRange
	if isRankScan {
		// Convert rank range to score range.
		converted, err := m.rankRangeToScoreRange(lb, scanRange)
		if err != nil {
			return &errorCursor[*IndexEntry]{err: err}
		}
		if converted == nil {
			return Empty[*IndexEntry]()
		}
		scoreRange = *converted
	}

	// Determine highScoreFirst.
	// Java checks both low and high group tuples and only resolves per-group
	// highScoreFirst when both are non-nil and equal. Otherwise falls back to
	// directory default. For BY_RANK scans, highScoreFirst is always false.
	highScoreFirst := false
	groupPrefixSize := m.getGroupingCount()
	if !isRankScan {
		highScoreFirst = dir.HighScoreFirst
		var lowGroup, highGroup tuple.Tuple
		if scoreRange.Low != nil && len(scoreRange.Low) > groupPrefixSize {
			lowGroup = tuple.Tuple(scoreRange.Low[:groupPrefixSize])
		}
		if scoreRange.High != nil && len(scoreRange.High) > groupPrefixSize {
			highGroup = tuple.Tuple(scoreRange.High[:groupPrefixSize])
		}
		if lowGroup != nil && highGroup != nil && tuplesEqual(lowGroup, highGroup) {
			hsf, err := m.isHighScoreFirst(dir, lowGroup)
			if err != nil {
				return &errorCursor[*IndexEntry]{err: err}
			}
			highScoreFirst = hsf
		}
	}

	// Apply score negation and reverse for highScoreFirst.
	actualRange := scoreRange
	actualProps := scanProperties
	if highScoreFirst && !isRankScan {
		actualRange = negateScoreRange(scoreRange, groupPrefixSize)
		actualProps.Reverse = !actualProps.Reverse
	}

	// Prepend leaderboard subspace key.
	leaderboardRange := prependLeaderboardKey(actualRange, lb.SubspaceKey)

	// Scan B-tree.
	rawCursor := m.standardIndexMaintainer.Scan(leaderboardRange, continuation, actualProps)

	// Post-process: remove leaderboard key prefix and un-negate if needed.
	return MapErrCursor(rawCursor, func(entry *IndexEntry) (*IndexEntry, error) {
		return m.getIndexEntry(entry, dir, groupPrefixSize)
	})
}

// getIndexEntry post-processes a raw scan result by removing the leaderboard subspace key
// prefix and un-negating scores if highScoreFirst.
// Matches Java's TimeWindowLeaderboardIndexMaintainer.getIndexEntry().
func (m *timeWindowLeaderboardIndexMaintainer) getIndexEntry(
	rawEntry *IndexEntry,
	dir *leaderboardDirectory,
	groupPrefixSize int,
) (*IndexEntry, error) {
	if len(rawEntry.Key) < 1 {
		return rawEntry, nil
	}

	// Remove leaderboard subspace key (first element).
	rawKey := rawEntry.Key[1:]

	// Determine per-group highScoreFirst.
	var highScoreFirst bool
	if groupPrefixSize > 0 && len(rawKey) > groupPrefixSize {
		group := tuple.Tuple(rawKey[:groupPrefixSize])
		hsf, err := m.isHighScoreFirst(dir, group)
		if err != nil {
			return nil, err
		}
		highScoreFirst = hsf
	} else {
		highScoreFirst = dir.HighScoreFirst
	}

	key := rawKey
	if highScoreFirst {
		key = negateScore(rawKey, groupPrefixSize)
	}

	return &IndexEntry{
		Index: rawEntry.Index,
		Key:   key,
		Value: rawEntry.Value,
	}, nil
}

// rankRangeToScoreRange converts rank endpoints to score endpoints within a specific leaderboard.
func (m *timeWindowLeaderboardIndexMaintainer) rankRangeToScoreRange(
	lb *leaderboard,
	rankRange TupleRange,
) (*TupleRange, error) {
	groupPrefixSize := m.getGroupingCount()

	var prefix tuple.Tuple
	var leaderboardGroupKey tuple.Tuple

	if groupPrefixSize > 0 {
		if rankRange.Low == nil || len(rankRange.Low) < groupPrefixSize ||
			rankRange.High == nil || len(rankRange.High) < groupPrefixSize {
			return nil, fmt.Errorf("rank scan range must include group (size %d)", groupPrefixSize)
		}
		prefix = tuple.Tuple(rankRange.Low[:groupPrefixSize])
		leaderboardGroupKey = make(tuple.Tuple, 0, len(lb.SubspaceKey)+len(prefix))
		leaderboardGroupKey = append(leaderboardGroupKey, lb.SubspaceKey...)
		leaderboardGroupKey = append(leaderboardGroupKey, prefix...)
	} else {
		leaderboardGroupKey = lb.SubspaceKey
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
			return nil, nil
		}
	}

	if startFromBeginning && highRank == nil {
		result := TupleRangeAllOf(prefix)
		return &result, nil
	}

	rankSubspace := m.secondarySubspace.Sub(leaderboardGroupKey...)
	config := m.rankedSetConfig
	if lb.NLevels > 0 {
		config.NLevels = lb.NLevels
	}
	rankedSet := newRankedSet(rankSubspace, config)

	needed, err := rankedSet.InitNeeded(m.tx.Snapshot())
	if err != nil {
		return nil, err
	}
	if needed {
		if err := rankedSet.Init(m.tx); err != nil {
			return nil, err
		}
	}

	lowRankVal := int64(0)
	if !startFromBeginning {
		lowRankVal = *lowRank
	}
	lowScoreBytes, err := rankedSet.GetNth(m.tx, lowRankVal)
	if err != nil {
		return nil, err
	}
	if lowScoreBytes == nil {
		return nil, nil
	}
	lowScore, err := tuple.Unpack(lowScoreBytes)
	if err != nil {
		return nil, fmt.Errorf("unpack low score: %w", err)
	}

	var highScore tuple.Tuple
	if highRank != nil {
		highScoreBytes, err := rankedSet.GetNth(m.tx, *highRank)
		if err != nil {
			return nil, err
		}
		if highScoreBytes != nil {
			highScore, err = tuple.Unpack(highScoreBytes)
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

	if prefix != nil {
		scoreRange = scoreRange.Prepend(prefix)
	}

	return &scoreRange, nil
}

// getGroupingCount returns the number of grouping columns in the index expression.
func (m *timeWindowLeaderboardIndexMaintainer) getGroupingCount() int {
	if g, ok := m.index.RootExpression.(*GroupingKeyExpression); ok {
		return g.GetGroupingCount()
	}
	return 0
}

// PerformWindowUpdate executes a window update operation on the leaderboard directory.
// store is needed for RebuildIndex when rebuild is triggered.
// Matches Java's TimeWindowLeaderboardIndexMaintainer.performOperation(WindowUpdate).
func (m *timeWindowLeaderboardIndexMaintainer) PerformWindowUpdate(update *TimeWindowLeaderboardWindowUpdate, store *FDBRecordStore) error {
	dir, err := loadLeaderboardDirectory(m.tx, m.secondarySubspace)
	if err != nil {
		return err
	}

	changed := false
	rebuild := false

	isRebuildConditional := func() bool {
		return update.Rebuild == TimeWindowRebuildIfOverlappingChanged
	}

	// Initialize or validate directory. Matches Java's UpdateState.setDirectory().
	if dir != nil && dir.HighScoreFirst != update.HighScoreFirst {
		if update.Rebuild == TimeWindowRebuildNever {
			return fmt.Errorf("cannot change highScoreFirst without a rebuild")
		}
		dir = nil // Force new directory + rebuild.
	}
	if dir == nil {
		dir = &leaderboardDirectory{
			HighScoreFirst: update.HighScoreFirst,
			leaderboards:   make(map[int][]*leaderboard),
			subdirectories: make(map[string]*leaderboardSubDirectory),
		}
		if isRebuildConditional() {
			rebuild = true
		}
		changed = true
	}

	dir.UpdateTimestamp = update.UpdateTimestamp

	// Delete expired windows.
	for _, lb := range dir.allLeaderboards() {
		if lb.Type != AllTimeLeaderboardType && update.DeleteBefore >= lb.EndTimestamp {
			indexKey := m.indexSubspace.Pack(lb.SubspaceKey)
			if inc, err := fdb.Strinc(indexKey); err == nil {
				m.tx.ClearRange(fdb.KeyRange{Begin: fdb.Key(indexKey), End: fdb.Key(inc)})
			}
			rankKey := m.secondarySubspace.Pack(lb.SubspaceKey)
			if inc, err := fdb.Strinc(rankKey); err == nil {
				m.tx.ClearRange(fdb.KeyRange{Begin: fdb.Key(rankKey), End: fdb.Key(inc)})
			}
			dir.removeLeaderboard(lb)
			changed = true
		}
	}

	nlevels := update.NLevels
	if nlevels <= 0 {
		nlevels = defaultRankedSetConfig.NLevels
	}

	// Create all-time leaderboard if requested.
	// Java also triggers conditional rebuild when adding all-time.
	if update.AllTime {
		if dir.findLeaderboard(AllTimeLeaderboardType, math.MinInt64, math.MaxInt64) == nil {
			dir.addLeaderboard(AllTimeLeaderboardType, math.MinInt64, math.MaxInt64, nlevels)
			if isRebuildConditional() {
				rebuild = true
			}
			changed = true
		}
	}

	// Create time windows from specs.
	var earliestAddedStart *int64
	for _, spec := range update.Specs {
		for i := 0; i < spec.Count; i++ {
			start := spec.BaseTimestamp + spec.StartIncrement*int64(i)
			end := start + spec.Duration
			if dir.findLeaderboard(spec.Type, start, end) == nil {
				dir.addLeaderboard(spec.Type, start, end, nlevels)
				changed = true
				if earliestAddedStart == nil || start < *earliestAddedStart {
					earliestAddedStart = &start
				}
			}
		}
	}

	// Check overlapping records for conditional rebuild.
	if update.Rebuild == TimeWindowRebuildAlways {
		rebuild = true
	} else if isRebuildConditional() && !rebuild && earliestAddedStart != nil {
		latestBytes, err := m.tx.Get(m.indexSubspace.FDBKey()).Get()
		if err != nil {
			return err
		}
		if latestBytes != nil && len(latestBytes) == 8 {
			unsigned := binary.LittleEndian.Uint64(latestBytes)
			latestTimestamp := int64(unsigned) + math.MinInt64
			if latestTimestamp >= *earliestAddedStart {
				rebuild = true
			}
		}
	}

	// Execute rebuild: delete all data, save directory, then rebuild index.
	// Matches Java's UpdateState.save(): deleteWhere → saveDirectory → store.rebuildIndex.
	if rebuild {
		if err := m.DeleteWhere(nil); err != nil {
			return err
		}
	}
	if changed || rebuild {
		if err := saveLeaderboardDirectory(m.tx, m.secondarySubspace, dir); err != nil {
			return err
		}
	}
	if rebuild && store != nil {
		if err := store.RebuildIndex(m.index); err != nil {
			return err
		}
	}

	return nil
}

// LoadDirectory loads the leaderboard directory for inspection.
func (m *timeWindowLeaderboardIndexMaintainer) LoadDirectory() (*leaderboardDirectory, error) {
	return loadLeaderboardDirectory(m.tx, m.secondarySubspace)
}

// EvaluateRecordFunction evaluates a record function (rank, time_window_rank, etc.)
// for a specific record within the leaderboard.
// Matches Java's TimeWindowLeaderboardIndexMaintainer.evaluateRecordFunction().
func (m *timeWindowLeaderboardIndexMaintainer) EvaluateRecordFunction(
	fn *IndexRecordFunction,
	record *FDBStoredRecord[proto.Message],
) (*int64, error) {
	switch fn.Name {
	case FunctionNameRank:
		// All-time leaderboard.
		return m.timeWindowRank(record, AllTimeLeaderboardType, 0)
	case FunctionNameTimeWindowRank:
		// Requires TimeWindowForFunction embedded in fn — for now,
		// extract type and timestamp from fn metadata if available.
		// Fall back to all-time.
		return m.timeWindowRank(record, AllTimeLeaderboardType, 0)
	default:
		return nil, fmt.Errorf("leaderboard index %q: unsupported record function %q", m.index.Name, fn.Name)
	}
}

// timeWindowRank computes the rank of a record's score within a specific time window.
// Matches Java's TimeWindowLeaderboardIndexMaintainer.timeWindowRankAndEntry().
func (m *timeWindowLeaderboardIndexMaintainer) timeWindowRank(
	record *FDBStoredRecord[proto.Message],
	leaderboardType int,
	leaderboardTimestamp int64,
) (*int64, error) {
	dir, err := loadLeaderboardDirectory(m.tx, m.secondarySubspace)
	if err != nil {
		return nil, err
	}
	if dir == nil {
		return nil, nil
	}

	lb := dir.oldestLeaderboardMatching(leaderboardType, leaderboardTimestamp)
	if lb == nil {
		return nil, nil
	}

	// Evaluate index entries for this record.
	entries, err := m.evaluateIndex(record)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}

	groupPrefixSize := m.getGroupingCount()

	// Group and order scores.
	grouped, _, err := m.groupOrderedScoreIndexKeys(dir, entries, groupPrefixSize)
	if err != nil {
		return nil, err
	}
	if len(grouped) == 0 {
		return nil, nil
	}
	if len(grouped) > 1 {
		return nil, fmt.Errorf("record has more than one group of scores")
	}

	gs := grouped[0]
	bestScore := m.bestContainedScore(lb, gs.scores)
	if bestScore == nil {
		return nil, nil
	}

	// Look up the rank in the ranked set.
	leaderboardGroupKey := make(tuple.Tuple, 0, len(lb.SubspaceKey)+len(gs.groupKey))
	leaderboardGroupKey = append(leaderboardGroupKey, lb.SubspaceKey...)
	leaderboardGroupKey = append(leaderboardGroupKey, gs.groupKey...)

	config := m.rankedSetConfig
	if lb.NLevels > 0 {
		config.NLevels = lb.NLevels
	}
	rankSubspace := m.secondarySubspace.Sub(leaderboardGroupKey...)
	rankedSet := newRankedSet(rankSubspace, config)

	scoreBytes := bestScore.scoreKey.Pack()
	return rankedSet.Rank(m.tx, scoreBytes, true)
}

// Aggregate function name constants for TIME_WINDOW_LEADERBOARD.
const (
	FunctionNameTimeWindowCount              = "time_window_count"
	FunctionNameScoreForTimeWindowRank       = "score_for_time_window_rank"
	FunctionNameScoreForTimeWindowRankElseSkip = "score_for_time_window_rank_else_skip"
	FunctionNameTimeWindowRankForScore       = "time_window_rank_for_score"
)

// CanEvaluateTimeWindowAggregate checks if this maintainer supports a given aggregate function.
func (m *timeWindowLeaderboardIndexMaintainer) CanEvaluateTimeWindowAggregate(fn *IndexAggregateFunction) bool {
	switch fn.Name {
	case FunctionNameTimeWindowCount,
		FunctionNameScoreForTimeWindowRank,
		FunctionNameScoreForTimeWindowRankElseSkip,
		FunctionNameTimeWindowRankForScore:
		return keyExpressionEquals(m.index.RootExpression, fn.Operand)
	default:
		return false
	}
}

// EvaluateTimeWindowAggregate evaluates an aggregate function within a time window.
// The range tuple format is: (type, timestamp, groupKey..., values...).
// Matches Java's TimeWindowLeaderboardIndexMaintainer.evaluateAggregateFunction().
func (m *timeWindowLeaderboardIndexMaintainer) EvaluateTimeWindowAggregate(
	fn *IndexAggregateFunction,
	rangeTuple tuple.Tuple,
) (tuple.Tuple, error) {
	if len(rangeTuple) < 2 {
		return nil, fmt.Errorf("time window aggregate range must have at least (type, timestamp)")
	}

	leaderboardType, _ := asInt64(rangeTuple[0])
	leaderboardTimestamp, _ := asInt64(rangeTuple[1])
	groupingCount := m.getGroupingCount()
	var groupKey tuple.Tuple
	var values tuple.Tuple
	if groupingCount > 0 && len(rangeTuple) > 2+groupingCount {
		groupKey = tuple.Tuple(rangeTuple[2 : 2+groupingCount])
		values = tuple.Tuple(rangeTuple[2+groupingCount:])
	} else if len(rangeTuple) > 2 {
		values = tuple.Tuple(rangeTuple[2:])
	}

	dir, err := loadLeaderboardDirectory(m.tx, m.secondarySubspace)
	if err != nil {
		return nil, err
	}
	if dir == nil {
		return nil, fmt.Errorf("no leaderboard directory")
	}

	lb := dir.oldestLeaderboardMatching(int(leaderboardType), leaderboardTimestamp)
	if lb == nil {
		return nil, fmt.Errorf("no leaderboard matching type=%d timestamp=%d", leaderboardType, leaderboardTimestamp)
	}

	leaderboardGroupKey := make(tuple.Tuple, 0, len(lb.SubspaceKey)+len(groupKey))
	leaderboardGroupKey = append(leaderboardGroupKey, lb.SubspaceKey...)
	leaderboardGroupKey = append(leaderboardGroupKey, groupKey...)

	config := m.rankedSetConfig
	if lb.NLevels > 0 {
		config.NLevels = lb.NLevels
	}
	rankSubspace := m.secondarySubspace.Sub(leaderboardGroupKey...)
	rankedSet := newRankedSet(rankSubspace, config)

	switch fn.Name {
	case FunctionNameTimeWindowCount:
		size, err := rankedSet.Size(m.tx)
		if err != nil {
			return nil, err
		}
		return tuple.Tuple{size}, nil

	case FunctionNameScoreForTimeWindowRank, FunctionNameScoreForTimeWindowRankElseSkip:
		if len(values) == 0 {
			return nil, fmt.Errorf("score_for_time_window_rank requires rank value")
		}
		rankVal, ok := asInt64(values[0])
		if !ok {
			return nil, fmt.Errorf("score_for_time_window_rank: rank must be int64")
		}
		scoreBytes, err := rankedSet.GetNth(m.tx, rankVal)
		if err != nil {
			return nil, err
		}
		if scoreBytes == nil {
			if fn.Name == FunctionNameScoreForTimeWindowRankElseSkip {
				return nil, nil // Skip sentinel
			}
			return nil, fmt.Errorf("rank %d out of range", rankVal)
		}
		score, err := tuple.Unpack(scoreBytes)
		if err != nil {
			return nil, err
		}
		// Un-negate if highScoreFirst.
		highScoreFirst, err := m.isHighScoreFirst(dir, groupKey)
		if err != nil {
			return nil, err
		}
		if highScoreFirst {
			score = negateScore(score, 0)
		}
		return score, nil

	case FunctionNameTimeWindowRankForScore:
		// If highScoreFirst, negate the score values before ranking.
		highScoreFirst, err := m.isHighScoreFirst(dir, groupKey)
		if err != nil {
			return nil, err
		}
		scoreValues := values
		if highScoreFirst {
			scoreValues = negateScore(values, 0)
		}
		scoreBytes := scoreValues.Pack()
		rank, err := rankedSet.Rank(m.tx, scoreBytes, false)
		if err != nil {
			return nil, err
		}
		if rank == nil {
			return nil, nil
		}
		return tuple.Tuple{*rank}, nil

	default:
		return nil, fmt.Errorf("unsupported aggregate function %q", fn.Name)
	}
}

// indexEntriesEqual checks if two index entry slices are identical.
func indexEntriesEqual(a, b []indexEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !tuplesEqual(a[i].key, b[i].key) || !tuplesEqual(a[i].primaryKey, b[i].primaryKey) {
			return false
		}
	}
	return true
}

// tupleCompare compares two tuples lexicographically by their packed bytes.
func tupleCompare(a, b tuple.Tuple) int {
	ab := a.Pack()
	bb := b.Pack()
	if len(ab) < len(bb) {
		for i := range ab {
			if ab[i] < bb[i] {
				return -1
			}
			if ab[i] > bb[i] {
				return 1
			}
		}
		return -1
	}
	for i := range bb {
		if ab[i] < bb[i] {
			return -1
		}
		if ab[i] > bb[i] {
			return 1
		}
	}
	if len(ab) > len(bb) {
		return 1
	}
	return 0
}

// asInt64 extracts an int64 from a tuple element.
func asInt64(v any) (int64, bool) {
	switch val := v.(type) {
	case int64:
		return val, true
	case int:
		return int64(val), true
	case int32:
		return int64(val), true
	default:
		return 0, false
	}
}

// tupleConcat appends two tuples into a new tuple.
func tupleConcat(a, b tuple.Tuple) tuple.Tuple {
	result := make(tuple.Tuple, 0, len(a)+len(b))
	result = append(result, a...)
	result = append(result, b...)
	return result
}

package chaos

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
)

// verifyRankIndexes checks RANK indexes:
//  1. B-tree entries match model (same as VALUE index verification)
//  2. For each distinct score in the B-tree, RankForScore and ScoreForRank are consistent
//  3. Ranked set total size matches the number of distinct scores in B-tree
func verifyRankIndexes(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel) []Violation {
	var violations []Violation
	md := model.metadata

	for _, idx := range md.GetAllIndexes() {
		if idx.Type != recordlayer.IndexTypeRank {
			continue
		}

		// --- Part 1: B-tree entry verification (identical to VALUE) ---
		expected := make(map[string]tuple.Tuple)

		for _, rec := range model.Records {
			if !model.indexAppliesToType(idx, rec.TypeName) {
				continue
			}

			storedRec := &recordlayer.FDBStoredRecord[proto.Message]{
				PrimaryKey: rec.PrimaryKey,
				RecordType: md.GetRecordType(rec.TypeName),
				Record:     rec.Message,
			}

			tuples, err := idx.RootExpression.Evaluate(storedRec, rec.Message)
			if err != nil {
				violations = append(violations, Violation{
					Invariant:  "rank_index_eval_error",
					PrimaryKey: rec.PrimaryKey,
					Expected:   fmt.Sprintf("index %q evaluable", idx.Name),
					Actual:     err.Error(),
				})
				continue
			}

			trimmedPK, err := idx.TrimPrimaryKey(rec.PrimaryKey)
			if err != nil {
				violations = append(violations, Violation{
					Invariant:  "rank_index_trim_pk_error",
					PrimaryKey: rec.PrimaryKey,
					Expected:   fmt.Sprintf("index %q pk trimmable", idx.Name),
					Actual:     err.Error(),
				})
				continue
			}

			for _, values := range tuples {
				entryKey := make(tuple.Tuple, 0, len(values)+len(trimmedPK))
				for _, v := range values {
					entryKey = append(entryKey, v)
				}
				entryKey = append(entryKey, trimmedPK...)
				expected[string(entryKey.Pack())] = entryKey
			}
		}

		// Scan actual B-tree entries.
		actual := make(map[string]tuple.Tuple)
		idxCursor := store.ScanIndex(idx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())

		for {
			result, err := idxCursor.OnNext(ctx)
			if err != nil {
				violations = append(violations, Violation{
					Invariant: "rank_index_scan_error",
					Expected:  fmt.Sprintf("index %q scannable", idx.Name),
					Actual:    err.Error(),
				})
				break
			}
			if !result.HasNext() {
				break
			}
			entry := result.GetValue()
			actual[string(entry.Key.Pack())] = entry.Key
		}
		_ = idxCursor.Close()

		for key, entryKey := range expected {
			if _, ok := actual[key]; !ok {
				violations = append(violations, Violation{
					Invariant: "rank_btree_entry_missing",
					Expected:  fmt.Sprintf("index %q entry %v", idx.Name, entryKey),
					Actual:    "not in store",
				})
			}
		}

		for key, entryKey := range actual {
			if _, ok := expected[key]; !ok {
				violations = append(violations, Violation{
					Invariant: "rank_btree_entry_orphan",
					Expected:  fmt.Sprintf("index %q: no entry %v", idx.Name, entryKey),
					Actual:    "exists in store but not in model",
				})
			}
		}

		if len(expected) != len(actual) {
			violations = append(violations, Violation{
				Invariant: "rank_btree_entry_count",
				Expected:  fmt.Sprintf("index %q: %d entries", idx.Name, len(expected)),
				Actual:    fmt.Sprintf("%d entries", len(actual)),
			})
		}

		// --- Part 2: Ranked set consistency ---
		maintainer, mErr := store.GetIndexMaintainer(idx)
		if mErr != nil {
			violations = append(violations, Violation{
				Invariant: "rank_index_get_maintainer",
				Expected:  "no error",
				Actual:    mErr.Error(),
			})
			continue
		}
		rankMaintainer, ok := maintainer.(recordlayer.RankQuerier)
		if !ok {
			violations = append(violations, Violation{
				Invariant: "rank_index_maintainer_type",
				Expected:  "RankQuerier",
				Actual:    fmt.Sprintf("%T", maintainer),
			})
			continue
		}

		// Compute distinct scores and their counts from model records.
		distinctScores := make(map[string]tuple.Tuple)
		scoreCounts := make(map[string]int)

		for _, rec := range model.Records {
			if !model.indexAppliesToType(idx, rec.TypeName) {
				continue
			}
			storedRec := &recordlayer.FDBStoredRecord[proto.Message]{
				PrimaryKey: rec.PrimaryKey,
				RecordType: md.GetRecordType(rec.TypeName),
				Record:     rec.Message,
			}
			tuples, err := idx.RootExpression.Evaluate(storedRec, rec.Message)
			if err != nil {
				continue
			}
			for _, values := range tuples {
				scoreTuple := make(tuple.Tuple, len(values))
				for i, v := range values {
					scoreTuple[i] = v
				}
				pk := string(scoreTuple.Pack())
				distinctScores[pk] = scoreTuple
				scoreCounts[pk]++
			}
		}

		countDuplicates := idx.Options[recordlayer.IndexOptionRankCountDuplicates] == "true"

		type scoreInfo struct {
			score tuple.Tuple
			count int
		}
		var sortedScores []scoreInfo
		for pk, s := range distinctScores {
			sortedScores = append(sortedScores, scoreInfo{score: s, count: scoreCounts[pk]})
		}
		for i := 0; i < len(sortedScores); i++ {
			for j := i + 1; j < len(sortedScores); j++ {
				if string(sortedScores[i].score.Pack()) > string(sortedScores[j].score.Pack()) {
					sortedScores[i], sortedScores[j] = sortedScores[j], sortedScores[i]
				}
			}
		}

		// Verify RankForScore for each score.
		cumulativeRank := 0
		for _, si := range sortedScores {
			expectedRank := cumulativeRank

			actualRank, err := rankMaintainer.RankForScore(si.score, true)
			if err != nil {
				violations = append(violations, Violation{
					Invariant: "rank_for_score_error",
					Expected:  fmt.Sprintf("index %q score %v rankable", idx.Name, si.score),
					Actual:    err.Error(),
				})
			} else if actualRank == nil {
				violations = append(violations, Violation{
					Invariant: "rank_for_score_missing",
					Expected:  fmt.Sprintf("index %q score %v rank=%d", idx.Name, si.score, expectedRank),
					Actual:    "nil (not in ranked set)",
				})
			} else if *actualRank != int64(expectedRank) {
				violations = append(violations, Violation{
					Invariant: "rank_for_score_mismatch",
					Expected:  fmt.Sprintf("index %q score %v rank=%d", idx.Name, si.score, expectedRank),
					Actual:    fmt.Sprintf("rank=%d", *actualRank),
				})
			}

			if countDuplicates {
				cumulativeRank += si.count
			} else {
				cumulativeRank++
			}
		}

		// Verify ScoreForRank at rank boundaries.
		rankPos := 0
		for _, si := range sortedScores {
			actualScore, err := rankMaintainer.ScoreForRank(tuple.Tuple{int64(rankPos)})
			if err != nil {
				violations = append(violations, Violation{
					Invariant: "score_for_rank_error",
					Expected:  fmt.Sprintf("index %q rank %d resolvable", idx.Name, rankPos),
					Actual:    err.Error(),
				})
			} else if actualScore == nil {
				violations = append(violations, Violation{
					Invariant: "score_for_rank_missing",
					Expected:  fmt.Sprintf("index %q rank %d = score %v", idx.Name, rankPos, si.score),
					Actual:    "nil",
				})
			} else if string(actualScore.Pack()) != string(si.score.Pack()) {
				violations = append(violations, Violation{
					Invariant: "score_for_rank_mismatch",
					Expected:  fmt.Sprintf("index %q rank %d = score %v", idx.Name, rankPos, si.score),
					Actual:    fmt.Sprintf("score %v", actualScore),
				})
			}

			if countDuplicates {
				rankPos += si.count
			} else {
				rankPos++
			}
		}

		// Verify total size: ScoreForRank(totalSize) should return nil.
		totalSize := rankPos
		outOfBoundsScore, err := rankMaintainer.ScoreForRank(tuple.Tuple{int64(totalSize)})
		if err != nil {
			violations = append(violations, Violation{
				Invariant: "rank_size_check_error",
				Expected:  fmt.Sprintf("index %q out-of-bounds rank %d check", idx.Name, totalSize),
				Actual:    err.Error(),
			})
		} else if outOfBoundsScore != nil {
			violations = append(violations, Violation{
				Invariant: "rank_size_too_large",
				Expected:  fmt.Sprintf("index %q rank %d out of bounds (nil)", idx.Name, totalSize),
				Actual:    fmt.Sprintf("score %v (extra entry in ranked set)", outOfBoundsScore),
			})
		}
	}

	return violations
}

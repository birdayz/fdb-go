package chaos

import (
	"context"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// leaderboardWindowUpdater is a duck-typed interface for the unexported
// timeWindowLeaderboardIndexMaintainer.PerformWindowUpdate method.
type leaderboardWindowUpdater interface {
	PerformWindowUpdate(update *recordlayer.TimeWindowLeaderboardWindowUpdate, store *recordlayer.FDBRecordStore) error
}

// buildLeaderboardMetadata creates metadata with a TIME_WINDOW_LEADERBOARD index
// on Order using Concat(Field("price"), Field("quantity")). Price is the score,
// quantity is the timestamp. No grouping.
func buildLeaderboardMetadata() (*recordlayer.RecordMetaData, *recordlayer.Index) {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())

	idx := recordlayer.NewTimeWindowLeaderboardIndex("order_score_leaderboard",
		recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")))
	builder.AddIndex("Order", idx)

	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build leaderboard metadata: " + err.Error())
	}
	return md, idx
}

// setupAllTimeWindow sets up an all-time leaderboard window for the given scenario.
// Uses the clean DB (no fault injection) to ensure setup always succeeds.
// Must be called before any chaos operations that touch the leaderboard index.
func setupAllTimeWindow(t testing.TB, s *Scenario, idx *recordlayer.Index) {
	t.Helper()
	ctx := context.Background()
	_, err := s.cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(s.metadata).
			SetSubspace(s.sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		maintainer, mErr := store.GetIndexMaintainer(idx)
		if mErr != nil {
			return nil, mErr
		}
		lm, ok := maintainer.(leaderboardWindowUpdater)
		if !ok {
			t.Fatalf("chaos: index maintainer %T does not implement leaderboardWindowUpdater", maintainer)
		}
		return nil, lm.PerformWindowUpdate(&recordlayer.TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 0,
			AllTime:         true,
			Rebuild:         recordlayer.TimeWindowRebuildIfOverlappingChanged,
		}, store)
	})
	if err != nil {
		t.Fatalf("chaos: setupAllTimeWindow: %v", err)
	}
}

// verifyLeaderboardEntries scans the all-time leaderboard and verifies:
// 1. Entry count matches the model's record count
// 2. Each entry's primary key exists in the model (no orphan entries)
// 3. Each model record has a corresponding entry (no missing entries)
// This catches leaderboard-specific corruption that generic Verify() skips.
func verifyLeaderboardEntries(t testing.TB, s *Scenario, idx *recordlayer.Index) {
	t.Helper()
	ctx := context.Background()

	type entryInfo struct {
		pk    tuple.Tuple
		score int64
	}

	result, err := s.cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(s.metadata).
			SetSubspace(s.sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		entries, err := recordlayer.AsList(ctx, store.ScanTimeWindowLeaderboard(
			idx, recordlayer.IndexScanByTimeWindow,
			recordlayer.AllTimeLeaderboardType, 0,
			recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}

		infos := make([]entryInfo, len(entries))
		for i, e := range entries {
			pk := e.PrimaryKey()
			var score int64
			if len(e.Key) > 0 {
				switch v := e.Key[0].(type) {
				case int64:
					score = v
				case int:
					score = int64(v)
				}
			}
			infos[i] = entryInfo{pk: pk, score: score}
		}
		return infos, nil
	})
	if err != nil {
		t.Fatalf("chaos: verifyLeaderboardEntries: %v", err)
	}

	entries := result.([]entryInfo)
	expectedCount := int(s.model.Count())
	if len(entries) != expectedCount {
		t.Fatalf("chaos: leaderboard entry count mismatch at op %d (seed=%d): expected=%d actual=%d",
			s.opIndex, s.seed, expectedCount, len(entries))
	}

	// Check each entry's PK exists in the model.
	modelPKs := make(map[string]bool)
	for pkKey := range s.model.Records {
		modelPKs[pkKey] = true
	}
	for _, e := range entries {
		if !modelPKs[string(e.pk.Pack())] {
			t.Fatalf("chaos: orphan leaderboard entry pk=%v score=%d at op %d (seed=%d)",
				e.pk, e.score, s.opIndex, s.seed)
		}
	}
}

// verifyAll runs both the standard model verification and the leaderboard-specific
// entry count verification.
func verifyAll(t testing.TB, s *Scenario, idx *recordlayer.Index) {
	t.Helper()
	s.Verify()
	verifyLeaderboardEntries(t, s, idx)
}

// TestLeaderboardBasicVerify tests the TIME_WINDOW_LEADERBOARD index with no
// fault injection. Validates the verification framework itself works.
func TestLeaderboardBasicVerify(t *testing.T) {
	t.Parallel()
	md, idx := buildLeaderboardMetadata()
	s := NewScenario(t, testRealDB, md)
	setupAllTimeWindow(t, s, idx)

	// Timestamp (quantity) must be within the all-time window [MinInt64, MaxInt64).
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000)})
	verifyAll(t, s, idx)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(2000)})
	verifyAll(t, s, idx)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300), Quantity: proto.Int32(3000)})
	verifyAll(t, s, idx)

	// Delete one and re-verify.
	s.DeleteRecord(tuple.Tuple{int64(2)})
	verifyAll(t, s, idx)

	// Re-insert with different score.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(50), Quantity: proto.Int32(1500)})
	verifyAll(t, s, idx)
}

// TestLeaderboardCommitUnknown injects commit-unknown on saves with a
// TIME_WINDOW_LEADERBOARD index. The leaderboard uses removeCommonEntries-style
// all-or-nothing comparison (indexEntriesEqual) — if old==new, skip entirely.
// On retry: old record matches new record → no-op. Should be idempotent.
func TestLeaderboardCommitUnknown(t *testing.T) {
	t.Parallel()
	md, idx := buildLeaderboardMetadata()
	s := NewScenario(t, testRealDB, md)
	setupAllTimeWindow(t, s, idx)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000)})
	verifyAll(t, s, idx)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(2000)})
	verifyAll(t, s, idx)

	// Third save, no fault.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(50), Quantity: proto.Int32(3000)})
	verifyAll(t, s, idx)
}

// TestLeaderboardCommitUnknownOverwrite injects commit-unknown on a record
// overwrite that changes the score. This is the dangerous scenario:
// First commit: remove old score entry, add new score entry.
// Retry: old record now has the new score → indexEntriesEqual → no-op.
func TestLeaderboardCommitUnknownOverwrite(t *testing.T) {
	t.Parallel()
	md, idx := buildLeaderboardMetadata()
	s := NewScenario(t, testRealDB, md)
	setupAllTimeWindow(t, s, idx)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(2000)})
	verifyAll(t, s, idx)

	// Overwrite pk=1: price 100→500 with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500), Quantity: proto.Int32(1000)})
	verifyAll(t, s, idx)

	// Overwrite pk=2: price 200→50, changing timestamp too.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(50), Quantity: proto.Int32(4000)})
	verifyAll(t, s, idx)
}

// TestLeaderboardCommitUnknownDelete injects commit-unknown on deletes.
// First commit: clear B-tree entry, remove score from ranked set.
// Retry: old=nil (already deleted) → no entries to process → no-op.
func TestLeaderboardCommitUnknownDelete(t *testing.T) {
	t.Parallel()
	md, idx := buildLeaderboardMetadata()
	s := NewScenario(t, testRealDB, md)
	setupAllTimeWindow(t, s, idx)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(2000)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300), Quantity: proto.Int32(3000)})
	verifyAll(t, s, idx)

	// Delete pk=2 with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(2)})
	verifyAll(t, s, idx)

	// Delete pk=1 with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	verifyAll(t, s, idx)
}

// TestLeaderboardDuplicateScores tests multiple records with the same score
// (price) but different timestamps. With !CountDuplicates (default), the ranked
// set has one entry per distinct (negated) score key. The B-tree has one entry per record.
func TestLeaderboardDuplicateScores(t *testing.T) {
	t.Parallel()
	md, idx := buildLeaderboardMetadata()
	s := NewScenario(t, testRealDB, md)
	setupAllTimeWindow(t, s, idx)

	// Three records with the same score=100 but different timestamps.
	// Each gets a different score key because the timestamp is part of the key.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100), Quantity: proto.Int32(2000)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100), Quantity: proto.Int32(3000)})
	verifyAll(t, s, idx)

	// Delete one.
	s.DeleteRecord(tuple.Tuple{int64(2)})
	verifyAll(t, s, idx)

	// Delete another.
	s.DeleteRecord(tuple.Tuple{int64(1)})
	verifyAll(t, s, idx)

	// Delete last.
	s.DeleteRecord(tuple.Tuple{int64(3)})
	verifyAll(t, s, idx)
}

// TestLeaderboardDuplicateScoresCommitUnknown tests duplicate scores with
// commit-unknown faults.
func TestLeaderboardDuplicateScoresCommitUnknown(t *testing.T) {
	t.Parallel()
	md, idx := buildLeaderboardMetadata()
	s := NewScenario(t, testRealDB, md)
	setupAllTimeWindow(t, s, idx)

	// Setup: three records with similar scores.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100), Quantity: proto.Int32(2000)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200), Quantity: proto.Int32(3000)})
	verifyAll(t, s, idx)

	// Save duplicate score with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(100), Quantity: proto.Int32(4000)})
	verifyAll(t, s, idx)

	// Delete one of the duplicates with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	verifyAll(t, s, idx)

	// Overwrite pk=2 from score=100→300 with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(300), Quantity: proto.Int32(2000)})
	verifyAll(t, s, idx)
}

// TestLeaderboardDeleteAllRecords verifies DeleteAllRecords clears leaderboard entries.
func TestLeaderboardDeleteAllRecords(t *testing.T) {
	t.Parallel()
	md, idx := buildLeaderboardMetadata()
	s := NewScenario(t, testRealDB, md)
	setupAllTimeWindow(t, s, idx)

	for i := int64(1); i <= 10; i++ {
		s.SaveRecord(&gen.Order{
			OrderId:  proto.Int64(i),
			Price:    proto.Int32(int32(i * 10)),
			Quantity: proto.Int32(int32(i * 1000)),
		})
	}
	verifyAll(t, s, idx)

	s.DeleteAllRecords()
	verifyAll(t, s, idx)

	// Re-setup windows — DeleteAllRecords clears the secondary subspace
	// which includes the leaderboard directory proto.
	setupAllTimeWindow(t, s, idx)

	// Re-add after delete-all.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(99), Price: proto.Int32(42), Quantity: proto.Int32(5000)})
	verifyAll(t, s, idx)
}

// TestLeaderboardRandomFaults runs many random operations with continuous fault
// injection and periodic verification.
func TestLeaderboardRandomFaults(t *testing.T) {
	t.Parallel()
	md, idx := buildLeaderboardMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(77777), WithFaults(FaultsRetryHeavy))
	setupAllTimeWindow(t, s, idx)

	const numOps = 200
	const verifyEvery = 20
	maxPK := int64(30)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			// 70% saves with varying scores and timestamps.
			s.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(pk),
				Price:    proto.Int32(s.Rng.Int32N(10) * 100),
				Quantity: proto.Int32(s.Rng.Int32N(10000)),
			})
		} else {
			// 30% deletes.
			s.DeleteRecord(tuple.Tuple{pk})
		}

		if (i+1)%verifyEvery == 0 {
			verifyAll(t, s, idx)
		}
	}
	verifyAll(t, s, idx)

	t.Logf("completed %d ops with seed=%d, %d faults injected",
		numOps, s.Seed(), len(s.FaultLog()))
}

// TestLeaderboardHeavyFaultStress runs with a very high fault injection rate
// to maximize the chance of catching non-idempotent behavior.
func TestLeaderboardHeavyFaultStress(t *testing.T) {
	t.Parallel()
	md, idx := buildLeaderboardMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(88888), WithFaults(FaultsRetryVeryHeavy))
	setupAllTimeWindow(t, s, idx)

	const numOps = 300
	const verifyEvery = 10
	maxPK := int64(20) // smaller key space = more overwrites

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.6 {
			s.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(pk),
				Price:    proto.Int32(s.Rng.Int32N(5) * 100), // small score space
				Quantity: proto.Int32(s.Rng.Int32N(5000)),
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}

		if (i+1)%verifyEvery == 0 {
			verifyAll(t, s, idx)
		}
	}
	verifyAll(t, s, idx)

	t.Logf("completed %d ops with seed=%d, %d faults injected",
		numOps, s.Seed(), len(s.FaultLog()))
}

// TestLeaderboardOverwriteChangesScore verifies that overwriting a record with
// a different score correctly updates the leaderboard entry.
func TestLeaderboardOverwriteChangesScore(t *testing.T) {
	t.Parallel()
	md, idx := buildLeaderboardMetadata()
	s := NewScenario(t, testRealDB, md)
	setupAllTimeWindow(t, s, idx)

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(2000)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300), Quantity: proto.Int32(3000)})
	verifyAll(t, s, idx)

	// Move pk=1 from lowest to highest score.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(400), Quantity: proto.Int32(1000)})
	verifyAll(t, s, idx)

	// Move pk=2 down to lowest.
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(50), Quantity: proto.Int32(2000)})
	verifyAll(t, s, idx)

	// Overwrite with same values (no-op path in indexEntriesEqual).
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300), Quantity: proto.Int32(3000)})
	verifyAll(t, s, idx)
}

// TestLeaderboardMultipleWindows tests with both an all-time and a bounded
// time window. Records outside the bounded window should still appear in
// the all-time leaderboard but not in the bounded one.
func TestLeaderboardMultipleWindows(t *testing.T) {
	t.Parallel()
	md, idx := buildLeaderboardMetadata()
	s := NewScenario(t, testRealDB, md)

	// Set up both all-time and a bounded window [1000, 2000).
	ctx := context.Background()
	_, err := s.cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(s.metadata).
			SetSubspace(s.sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		maintainer, mErr := store.GetIndexMaintainer(idx)
		if mErr != nil {
			return nil, mErr
		}
		lm := maintainer.(leaderboardWindowUpdater)
		return nil, lm.PerformWindowUpdate(&recordlayer.TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 0,
			AllTime:         true,
			Specs: []recordlayer.TimeWindowSpec{
				{Type: 1, BaseTimestamp: 1000, Duration: 1000, Count: 1},
			},
			Rebuild: recordlayer.TimeWindowRebuildIfOverlappingChanged,
		}, store)
	})
	if err != nil {
		t.Fatalf("chaos: setup multiple windows: %v", err)
	}

	// Record with ts=1500 (inside bounded window).
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1500)})
	// Record with ts=500 (outside bounded window, but inside all-time).
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(500)})
	// Record with ts=2500 (outside bounded window, but inside all-time).
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300), Quantity: proto.Int32(2500)})

	s.Verify()

	// All-time should have 3 entries.
	verifyLeaderboardEntries(t, s, idx)

	// Bounded window type=1 at ts=1500 should have 1 entry.
	result, err := s.cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(s.metadata).
			SetSubspace(s.sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanTimeWindowLeaderboard(
			idx, recordlayer.IndexScanByTimeWindow, 1, 1500,
			recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		return len(entries), nil
	})
	if err != nil {
		t.Fatalf("chaos: scan bounded window: %v", err)
	}
	boundedCount := result.(int)
	if boundedCount != 1 {
		t.Fatalf("chaos: bounded window entry count mismatch: expected=1 actual=%d", boundedCount)
	}
}

// TestLeaderboardMultipleWindowsCommitUnknown tests fault injection with
// multiple time windows. Records should land in the correct windows even
// under retry.
func TestLeaderboardMultipleWindowsCommitUnknown(t *testing.T) {
	t.Parallel()
	md, idx := buildLeaderboardMetadata()
	s := NewScenario(t, testRealDB, md)

	// Set up all-time + bounded [1000, 2000).
	ctx := context.Background()
	_, err := s.cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(s.metadata).
			SetSubspace(s.sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		maintainer, mErr := store.GetIndexMaintainer(idx)
		if mErr != nil {
			return nil, mErr
		}
		lm := maintainer.(leaderboardWindowUpdater)
		return nil, lm.PerformWindowUpdate(&recordlayer.TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 0,
			AllTime:         true,
			Specs: []recordlayer.TimeWindowSpec{
				{Type: 1, BaseTimestamp: 1000, Duration: 1000, Count: 1},
			},
			Rebuild: recordlayer.TimeWindowRebuildIfOverlappingChanged,
		}, store)
	})
	if err != nil {
		t.Fatalf("chaos: setup multiple windows: %v", err)
	}

	// Save records with commit-unknown across both windows.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1500)})
	verifyAll(t, s, idx)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(500)})
	verifyAll(t, s, idx)

	// Overwrite pk=1 to move it out of the bounded window.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(150), Quantity: proto.Int32(500)})
	verifyAll(t, s, idx)

	// Delete pk=2 with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(2)})
	verifyAll(t, s, idx)
}

// TestLeaderboardHighScoreFirst tests with HighScoreFirst=true, which negates
// scores for ranked set ordering. The all-time leaderboard should still contain
// the correct number of entries.
func TestLeaderboardHighScoreFirst(t *testing.T) {
	t.Parallel()
	md, idx := buildLeaderboardMetadata()
	s := NewScenario(t, testRealDB, md)

	// Set up all-time window with HighScoreFirst.
	ctx := context.Background()
	_, err := s.cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(s.metadata).
			SetSubspace(s.sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		maintainer, mErr := store.GetIndexMaintainer(idx)
		if mErr != nil {
			return nil, mErr
		}
		lm := maintainer.(leaderboardWindowUpdater)
		return nil, lm.PerformWindowUpdate(&recordlayer.TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 0,
			HighScoreFirst:  true,
			AllTime:         true,
			Rebuild:         recordlayer.TimeWindowRebuildIfOverlappingChanged,
		}, store)
	})
	if err != nil {
		t.Fatalf("chaos: setup highScoreFirst window: %v", err)
	}

	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(1000)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200), Quantity: proto.Int32(2000)})
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(300), Quantity: proto.Int32(3000)})
	verifyAll(t, s, idx)

	// Overwrite with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(400), Quantity: proto.Int32(1000)})
	verifyAll(t, s, idx)

	// Delete with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(3)})
	verifyAll(t, s, idx)
}

// TestLeaderboardHighScoreFirstStress runs random operations with HighScoreFirst
// and heavy faults.
func TestLeaderboardHighScoreFirstStress(t *testing.T) {
	t.Parallel()
	md, idx := buildLeaderboardMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(44444), WithFaults(FaultsRetryVeryHeavy))

	// Set up all-time window with HighScoreFirst.
	ctx := context.Background()
	_, err := s.cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(s.metadata).
			SetSubspace(s.sub).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		maintainer, mErr := store.GetIndexMaintainer(idx)
		if mErr != nil {
			return nil, mErr
		}
		lm := maintainer.(leaderboardWindowUpdater)
		return nil, lm.PerformWindowUpdate(&recordlayer.TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 0,
			HighScoreFirst:  true,
			AllTime:         true,
			Rebuild:         recordlayer.TimeWindowRebuildIfOverlappingChanged,
		}, store)
	})
	if err != nil {
		t.Fatalf("chaos: setup highScoreFirst window: %v", err)
	}

	const numOps = 200
	const verifyEvery = 20
	maxPK := int64(20)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.6 {
			s.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(pk),
				Price:    proto.Int32(s.Rng.Int32N(5) * 100),
				Quantity: proto.Int32(s.Rng.Int32N(5000)),
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}

		if (i+1)%verifyEvery == 0 {
			verifyAll(t, s, idx)
		}
	}
	verifyAll(t, s, idx)

	t.Logf("completed %d ops with seed=%d, %d faults injected",
		numOps, s.Seed(), len(s.FaultLog()))
}

// TestLeaderboardAllFaultTypes runs with all fault types (commit-unknown,
// conflict, transaction-too-old) at moderate rates.
func TestLeaderboardAllFaultTypes(t *testing.T) {
	t.Parallel()
	md, idx := buildLeaderboardMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(55555), WithFaults(FaultsAll))
	setupAllTimeWindow(t, s, idx)

	const numOps = 200
	const verifyEvery = 20
	maxPK := int64(25)

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			s.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(pk),
				Price:    proto.Int32(s.Rng.Int32N(10) * 100),
				Quantity: proto.Int32(s.Rng.Int32N(10000)),
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}

		if (i+1)%verifyEvery == 0 {
			verifyAll(t, s, idx)
		}
	}
	verifyAll(t, s, idx)

	t.Logf("completed %d ops with seed=%d, %d faults injected",
		numOps, s.Seed(), len(s.FaultLog()))
}

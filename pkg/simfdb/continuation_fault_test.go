package simfdb_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/simfdb"
)

// TestSimFDB_ContinuationResumeAcrossFaultedWrite is RFC-199 Tier 2's continuation-under-INJECTED-fault
// case: a scan mints a continuation on page 1, then a between-page write to un-scanned data commits
// through a targeted fault, and the scan resumes from the continuation bytes in a fresh transaction.
// The resume must reflect the fault's TRUE outcome — SimFDB gives true rollback, so a not_committed
// (1020) leaves the data untouched, and a commit_unknown (1021) leaves it durable or not depending on
// which of the error's two real branches occurred — with no duplicate or lost record in any case.
// This pairs the continuation path with the fault path, which only SimFDB's deterministic
// true-rollback backend can replay reproducibly.
//
// The between-page write uses a RAW, single-commit transaction (not db.Run) on purpose: 1020/1021/1007
// are all retryable, so db.Run would retry the fault away. The raw commit surfaces the fault's outcome
// exactly once, which is what makes the branches observable.
func TestSimFDB_ContinuationResumeAcrossFaultedWrite(t *testing.T) {
	t.Parallel()

	// pk 10 is un-scanned at the page-1 continuation (page 1 is 0..4); it is what the faulted write
	// deletes. want10 is whether it should survive the fault.
	// commit_unknown_result has TWO branches and both are real FDB, so both are cases here: the
	// caller sees the same 1021 either way and the data either landed or did not.
	cases := []struct {
		name     string
		inject   int
		wantCode int
		want10   bool // pk 10 present in the resumed result?
	}{
		{"not_committed(1020) rolls the delete back — pk 10 survives", 1020, 1020, true},
		{"commit_unknown(1021), applied branch — pk 10 is gone", simfdb.CommitUnknownApplied, 1021, false},
		{"commit_unknown(1021), discarded branch — pk 10 survives", simfdb.CommitUnknownDiscarded, 1021, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sim, db, sub := newInjectableSim(tc.name)
			md := buildOrderMetadata(t)
			ctx := context.Background()

			// Seed pk 0..19.
			if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := openStore(rtx, md, sub)
				if err != nil {
					return nil, err
				}
				for i := 0; i < 20; i++ {
					if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i)), Price: proto.Int32(int32(i))}); err != nil {
						return nil, err
					}
				}
				return nil, nil
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}

			scanPage := func(cont []byte) (pks []int64, next []byte) {
				if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					store, err := openStore(rtx, md, sub)
					if err != nil {
						return nil, err
					}
					cur := store.ScanRecords(cont, recordlayer.ScanProperties{
						ExecuteProperties:   recordlayer.ExecuteProperties{ReturnedRowLimit: 5},
						CursorStreamingMode: recordlayer.StreamingModeWantAll,
					})
					recs, c, err := recordlayer.AsListWithContinuation(ctx, cur)
					if err != nil {
						return nil, err
					}
					for _, r := range recs {
						pks = append(pks, r.Record.(*gen.Order).GetOrderId())
					}
					next = c
					return nil, nil
				}); err != nil {
					t.Fatalf("scan page: %v", err)
				}
				return pks, next
			}

			// Page 1: 0..4, capture the continuation.
			page1, cont := scanPage(nil)
			if len(page1) != 5 || page1[0] != 0 || page1[4] != 4 {
				t.Fatalf("page1 = %v, want [0 1 2 3 4]", page1)
			}
			if len(cont) == 0 {
				t.Fatal("no continuation after page 1")
			}

			// FAULTED between-page write: delete pk 10 in a raw, single-commit transaction that the
			// injected fault hits. 1020 fires before apply (rolled back); 1021 fires after (durable).
			tx, err := db.CreateWritableTransaction()
			if err != nil {
				t.Fatalf("create txn: %v", err)
			}
			store, err := openStore(recordlayer.NewFDBRecordContext(tx, db.Env()), md, sub)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			if _, err := store.DeleteRecord(tuple.Tuple{int64(10)}); err != nil {
				t.Fatalf("delete: %v", err)
			}
			sim.InjectOnce(tc.inject)
			errCommit := tx.Commit().Get()
			var fe fdb.Error
			if !errors.As(errCommit, &fe) || fe.Code != tc.wantCode {
				t.Fatalf("commit = %v, want code %d", errCommit, tc.wantCode)
			}

			// Resume from the continuation to exhaustion.
			all := append([]int64(nil), page1...)
			for len(cont) > 0 {
				pks, next := scanPage(cont)
				all = append(all, pks...)
				cont = next
			}

			// Expected: 0..19, minus pk 10 iff the delete was applied.
			var want []int64
			for i := int64(0); i < 20; i++ {
				if i == 10 && !tc.want10 {
					continue
				}
				want = append(want, i)
			}
			if len(all) != len(want) {
				t.Fatalf("resumed scan = %v (%d rows), want %v (%d)", all, len(all), want, len(want))
			}
			for i := range want {
				if all[i] != want[i] {
					t.Fatalf("at index %d: got %d, want %d (full: %v)", i, all[i], want[i], all)
				}
			}
		})
	}
}

// TestSimFDB_ContinuationResumeAcrossFaultedInsert is the insert complement: a between-page INSERT of
// an un-scanned tail key commits through a targeted fault, and the resume must reflect the fault's
// true outcome — a rolled-back (1020) insert never appears, an applied 1021 does, a discarded 1021
// does not — with the resumed sequence otherwise intact. Insert (a phantom in the tail) is the
// mirror of delete and a distinct continuation-resume path from the delete case above.
func TestSimFDB_ContinuationResumeAcrossFaultedInsert(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		inject   int
		wantCode int
		want25   bool // newly-inserted pk 25 present in the resumed result?
	}{
		{"not_committed(1020) rolls the insert back — pk 25 never appears", 1020, 1020, false},
		{"commit_unknown(1021), applied branch — pk 25 appears in the tail", simfdb.CommitUnknownApplied, 1021, true},
		{"commit_unknown(1021), discarded branch — pk 25 never appears", simfdb.CommitUnknownDiscarded, 1021, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sim, db, sub := newInjectableSim("ins-" + tc.name)
			md := buildOrderMetadata(t)
			ctx := context.Background()

			if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := openStore(rtx, md, sub)
				if err != nil {
					return nil, err
				}
				for i := 0; i < 20; i++ {
					if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i)), Price: proto.Int32(int32(i))}); err != nil {
						return nil, err
					}
				}
				return nil, nil
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}

			scanPage := func(cont []byte) (pks []int64, next []byte) {
				if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					store, err := openStore(rtx, md, sub)
					if err != nil {
						return nil, err
					}
					cur := store.ScanRecords(cont, recordlayer.ScanProperties{
						ExecuteProperties:   recordlayer.ExecuteProperties{ReturnedRowLimit: 5},
						CursorStreamingMode: recordlayer.StreamingModeWantAll,
					})
					recs, c, err := recordlayer.AsListWithContinuation(ctx, cur)
					if err != nil {
						return nil, err
					}
					for _, r := range recs {
						pks = append(pks, r.Record.(*gen.Order).GetOrderId())
					}
					next = c
					return nil, nil
				}); err != nil {
					t.Fatalf("scan page: %v", err)
				}
				return pks, next
			}

			page1, cont := scanPage(nil)
			if len(page1) != 5 || page1[4] != 4 {
				t.Fatalf("page1 = %v, want [0 1 2 3 4]", page1)
			}

			// FAULTED between-page insert of pk 25 (an un-scanned tail key).
			tx, err := db.CreateWritableTransaction()
			if err != nil {
				t.Fatalf("create txn: %v", err)
			}
			store, err := openStore(recordlayer.NewFDBRecordContext(tx, db.Env()), md, sub)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(25), Price: proto.Int32(25)}); err != nil {
				t.Fatalf("save: %v", err)
			}
			sim.InjectOnce(tc.inject)
			errCommit := tx.Commit().Get()
			var fe fdb.Error
			if !errors.As(errCommit, &fe) || fe.Code != tc.wantCode {
				t.Fatalf("commit = %v, want code %d", errCommit, tc.wantCode)
			}

			all := append([]int64(nil), page1...)
			for len(cont) > 0 {
				pks, next := scanPage(cont)
				all = append(all, pks...)
				cont = next
			}

			want := make([]int64, 0, 21)
			for i := int64(0); i < 20; i++ {
				want = append(want, i)
			}
			if tc.want25 {
				want = append(want, 25)
			}
			if len(all) != len(want) {
				t.Fatalf("resumed scan = %v (%d rows), want %v (%d)", all, len(all), want, len(want))
			}
			for i := range want {
				if all[i] != want[i] {
					t.Fatalf("at index %d: got %d, want %d (full: %v)", i, all[i], want[i], all)
				}
			}
		})
	}
}

// TestSimFDB_ContinuationResumeUnderRetryingTransaction resumes a scan inside a db.Run whose commit is
// hit by an injected fault, so the record layer's retry loop re-runs the whole resume closure at a
// FRESH read version. The continuation token — minted at the page-1 version — must resume correctly
// across that retry (the "does the token survive a resume-transaction fault + retry" surface). The
// resume closure deletes an ALREADY-SCANNED prefix key (pk 0) so its commit is non-empty (a read-only
// commit short-circuits and no fault would fire); because that key is in the scanned prefix, the
// resumed tail is invariant to whether the delete rolled back (1020) or applied (1021), so
// page1 ++ tail == 0..19 either way. A retry MUST occur (attempts >= 2), proving the fault fired.
func TestSimFDB_ContinuationResumeUnderRetryingTransaction(t *testing.T) {
	t.Parallel()
	for _, faultCode := range []int{1020, 1021} {
		faultCode := faultCode
		t.Run(fmt.Sprintf("resume-commit-faults-%d", faultCode), func(t *testing.T) {
			t.Parallel()
			sim, db, sub := newInjectableSim(fmt.Sprintf("retry-%d", faultCode))
			md := buildOrderMetadata(t)
			ctx := context.Background()

			if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := openStore(rtx, md, sub)
				if err != nil {
					return nil, err
				}
				for i := 0; i < 20; i++ {
					if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i)), Price: proto.Int32(int32(i))}); err != nil {
						return nil, err
					}
				}
				return nil, nil
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}

			// Page 1 (read-only): 0..4, capture the continuation.
			var page1 []int64
			var cont []byte
			if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := openStore(rtx, md, sub)
				if err != nil {
					return nil, err
				}
				cur := store.ScanRecords(nil, recordlayer.ScanProperties{
					ExecuteProperties:   recordlayer.ExecuteProperties{ReturnedRowLimit: 5},
					CursorStreamingMode: recordlayer.StreamingModeWantAll,
				})
				recs, c, err := recordlayer.AsListWithContinuation(ctx, cur)
				if err != nil {
					return nil, err
				}
				for _, r := range recs {
					page1 = append(page1, r.Record.(*gen.Order).GetOrderId())
				}
				cont = c
				return nil, nil
			}); err != nil {
				t.Fatalf("page1: %v", err)
			}
			if len(page1) != 5 || len(cont) == 0 {
				t.Fatalf("page1 = %v, cont len %d", page1, len(cont))
			}

			// Resume the tail inside a db.Run whose first commit is faulted, forcing a retry.
			sim.InjectOnce(faultCode)
			var tail []int64
			attempts := 0
			if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				attempts++
				store, err := openStore(rtx, md, sub)
				if err != nil {
					return nil, err
				}
				cur := store.ScanRecords(cont, recordlayer.ScanProperties{
					ExecuteProperties:   recordlayer.ExecuteProperties{ReturnedRowLimit: 1000},
					CursorStreamingMode: recordlayer.StreamingModeWantAll,
				})
				recs, _, err := recordlayer.AsListWithContinuation(ctx, cur)
				if err != nil {
					return nil, err
				}
				tail = tail[:0] // reset per attempt — keep only the final successful attempt's rows
				for _, r := range recs {
					tail = append(tail, r.Record.(*gen.Order).GetOrderId())
				}
				// Non-empty mutation on an already-scanned key so the commit isn't short-circuited
				// and the tail stays invariant to it.
				if _, err := store.DeleteRecord(tuple.Tuple{int64(0)}); err != nil {
					return nil, err
				}
				return nil, nil
			}); err != nil {
				t.Fatalf("resume under fault %d: %v", faultCode, err)
			}
			if attempts < 2 {
				t.Fatalf("expected the injected fault to force a retry, but the resume committed in %d attempt(s)", attempts)
			}

			all := append(append([]int64(nil), page1...), tail...)
			var want []int64
			for i := int64(0); i < 20; i++ {
				want = append(want, i)
			}
			if len(all) != len(want) {
				t.Fatalf("page1++tail = %v (%d rows), want 0..19 (%d)", all, len(all), len(want))
			}
			for i := range want {
				if all[i] != want[i] {
					t.Fatalf("at index %d: got %d, want %d (full: %v)", i, all[i], want[i], all)
				}
			}
		})
	}
}

// newInjectableSim builds a SimFDB-backed record database and returns the raw SimDB too, so a test
// can call InjectOnce to fire a targeted commit fault. Faults are otherwise off (BUGGIFY disabled).
func newInjectableSim(name string) (*simfdb.SimDB, *recordlayer.FDBDatabase, subspace.Subspace) {
	env := dst.NewSim(1)
	env.Buggify = dst.DisabledBuggifier()
	sim := simfdb.New(env)
	db := recordlayer.NewFDBDatabaseWithBackend(sim).SetEnv(env)
	return sim, db, subspace.FromBytes(tuple.Tuple{"cont-fault", name}.Pack())
}

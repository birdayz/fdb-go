package simfdb_test

import (
	"context"
	"errors"
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

// TestSimFDB_ContinuationResumeAcrossFaultedWrite is RFC-179 Tier 2's continuation-under-INJECTED-fault
// case: a scan mints a continuation on page 1, then a between-page write to un-scanned data commits
// through a targeted fault, and the scan resumes from the continuation bytes in a fresh transaction.
// The resume must reflect the fault's TRUE outcome — SimFDB gives true rollback, so a not_committed
// (1020) leaves the data untouched and a commit_unknown (1021) leaves the write durable — with no
// duplicate or lost record either way. This pairs the continuation path with the fault path, which
// only SimFDB's deterministic true-rollback backend can replay reproducibly.
//
// The between-page write uses a RAW, single-commit transaction (not db.Run) on purpose: 1020/1021/1007
// are all retryable, so db.Run would retry the fault away. The raw commit surfaces the fault's outcome
// exactly once, which is what makes the two cases (rolled back vs applied) observable.
func TestSimFDB_ContinuationResumeAcrossFaultedWrite(t *testing.T) {
	t.Parallel()

	// pk 10 is un-scanned at the page-1 continuation (page 1 is 0..4); it is what the faulted write
	// deletes. want10 is whether it should survive the fault.
	cases := []struct {
		name   string
		fault  int
		want10 bool // pk 10 present in the resumed result?
	}{
		{"not_committed(1020) rolls the delete back — pk 10 survives", 1020, true},
		{"commit_unknown(1021) applied the delete — pk 10 is gone", 1021, false},
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
			store, err := openStore(recordlayer.NewFDBRecordContext(tx), md, sub)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			if _, err := store.DeleteRecord(tuple.Tuple{int64(10)}); err != nil {
				t.Fatalf("delete: %v", err)
			}
			sim.InjectOnce(tc.fault)
			errCommit := tx.Commit().Get()
			var fe fdb.Error
			if !errors.As(errCommit, &fe) || fe.Code != tc.fault {
				t.Fatalf("commit = %v, want injected code %d", errCommit, tc.fault)
			}

			// Resume from the continuation to exhaustion.
			all := append([]int64(nil), page1...)
			for len(cont) > 0 {
				pks, next := scanPage(cont)
				all = append(all, pks...)
				cont = next
			}

			// Expected: 0..19, minus pk 10 iff the delete was applied (1021).
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

// newInjectableSim builds a SimFDB-backed record database and returns the raw SimDB too, so a test
// can call InjectOnce to fire a targeted commit fault. Faults are otherwise off (BUGGIFY disabled).
func newInjectableSim(name string) (*simfdb.SimDB, *recordlayer.FDBDatabase, subspace.Subspace) {
	env := dst.NewSim(1)
	env.Buggify = dst.DisabledBuggifier()
	sim := simfdb.New(env)
	db := recordlayer.NewFDBDatabaseWithBackend(sim).SetEnv(env)
	return sim, db, subspace.FromBytes(tuple.Tuple{"cont-fault", name}.Pack())
}

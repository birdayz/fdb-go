package recordlayer

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/simfdb"
	"google.golang.org/protobuf/proto"
)

// GetSnapshotRecordCount's COUNT-index fallback asks for IsolationLevelSnapshot —
// Java's evaluateAggregateFunction(..., IsolationLevel.SNAPSHOT, ...)
// (FDBRecordStore.java:2320-2322) — and the aggregate-index scan behind it must
// actually perform a snapshot read: Java reaches that scan through
// AtomicMutationIndexMaintainer.evaluateAggregateFunction
// (AtomicMutationIndexMaintainer.java:209-217) -> StandardIndexMaintainer.scan ->
// KeyValueCursorBase.java:358, which is literally
// context.readTransaction(scanProperties.getExecuteProperties().getIsolationLevel().isSnapshot()).
//
// The aggregate cursor used to ignore that and always read non-snapshot, so the count
// added a read conflict range over the index. The consequence is not a wrong number —
// the count was right — it is a caller whose UNRELATED write is rejected with
// not_committed (1020) because some other transaction happened to touch the index in
// between. A count whose contract is "does not conflict" that silently conflicts is
// invisible until it shows up as contention under load, which is why the pin measures
// the COMMIT, not the count.
//
// Both directions are asserted. A cursor that hardcoded snapshot would pass the
// snapshot arm and fail the serializable one, so neither hardcoding survives.
//
// This is the opposite requirement from the store's records-emptiness probe
// (recordsRangeEmpty, store_builder.go), which is deliberately NON-snapshot so that a
// concurrent insert conflicts and invalidates its "empty, so build the index inline"
// decision. That probe wants the conflict; this read must not have one.
func TestAggregateIndexScanHonorsIsolationLevel(t *testing.T) {
	t.Parallel()

	buildMetaData := func(t *testing.T) *RecordMetaData {
		t.Helper()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		// A universal COUNT index and NO record-count key: that combination is
		// exactly what routes GetSnapshotRecordCount through the fallback rather
		// than through the maintained counters.
		builder.AddUniversalIndex(NewCountIndex("count_universal", Ungrouped(EmptyKey())))
		md, err := builder.Build()
		if err != nil {
			t.Fatalf("build metadata: %v", err)
		}
		if md.GetRecordCountKey() != nil {
			t.Fatalf("this fixture must have no record-count key, or the count is answered " +
				"by the maintained counters and the fallback under test is never reached")
		}
		return md
	}

	for _, arm := range []struct {
		name string
		// read performs the count under the isolation level being pinned and
		// returns how many records it counted.
		read         func(t *testing.T, store *FDBRecordStore) int64
		wantConflict bool
	}{
		{
			// The end-to-end shape from the brief: the public API, which always
			// asks for snapshot.
			name: "GetSnapshotRecordCount/snapshot",
			read: func(t *testing.T, store *FDBRecordStore) int64 {
				t.Helper()
				n, err := store.GetSnapshotRecordCount(tuple.Tuple{})
				if err != nil {
					t.Fatalf("GetSnapshotRecordCount: %v", err)
				}
				return n
			},
			wantConflict: false,
		},
		{
			// The other direction: a serializable aggregate evaluation must
			// still take its conflict range. Without this arm a cursor that
			// hardcoded Snapshot() would look correct.
			name: "EvaluateAggregateFunction/serializable",
			read: func(t *testing.T, store *FDBRecordStore) int64 {
				t.Helper()
				result, err := store.EvaluateAggregateFunction(context.Background(), nil,
					NewCountAggregateFunction(GroupAll(EmptyKey())),
					TupleRangeAll, IsolationLevelSerializable)
				if err != nil {
					t.Fatalf("EvaluateAggregateFunction: %v", err)
				}
				return result[0].(int64)
			},
			wantConflict: true,
		},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := dst.NewSim(209)
			env.Buggify = dst.DisabledBuggifier()
			db := NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
			sub := subspace.FromBytes(tuple.Tuple{"countiso", arm.name}.Pack())
			md := buildMetaData(t)

			if _, err := db.Run(ctx, func(rctx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rctx).SetMetaDataProvider(md).SetSubspace(sub).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 5; i++ {
					if _, err := store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i), Price: proto.Int32(int32(i)),
					}); err != nil {
						return nil, err
					}
				}
				return nil, nil
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}

			// The READER: a standalone transaction, no retry loop — a retry would
			// swallow the very conflict being measured.
			readerTx, err := db.CreateWritableTransaction()
			if err != nil {
				t.Fatalf("reader transaction: %v", err)
			}
			readerCtx := NewFDBRecordContext(readerTx, db.Env())
			readerStore, err := NewStoreBuilder().
				SetContext(readerCtx).SetMetaDataProvider(md).SetSubspace(sub).Open()
			if err != nil {
				t.Fatalf("reader store: %v", err)
			}

			if n := arm.read(t, readerStore); n != 5 {
				t.Fatalf("count = %d, want 5: a count that read nothing takes no conflict "+
					"range for the trivial reason and this subtest would then pass or fail "+
					"for reasons unrelated to the isolation level", n)
			}

			// The WRITER: a separate committed transaction that bumps the COUNT
			// index the reader just scanned.
			if _, err := db.Run(ctx, func(rctx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rctx).SetMetaDataProvider(md).SetSubspace(sub).Open()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(6), Price: proto.Int32(60),
				})
				return nil, err
			}); err != nil {
				t.Fatalf("concurrent write: %v", err)
			}

			// The reader now writes something unrelated and commits. Its own write
			// touches the count index too, but write-write does not conflict in
			// FDB: the ONLY thing that can reject this commit is the read conflict
			// range the count did or did not take.
			if _, err := readerStore.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(99), Name: proto.String("reader"),
			}); err != nil {
				t.Fatalf("reader write: %v", err)
			}
			commitErr := readerCtx.Commit()

			conflicted := commitErr != nil
			if conflicted {
				var fe fdb.Error
				if !errors.As(commitErr, &fe) || fe.Code != 1020 {
					t.Fatalf("reader commit failed with %v (%T), want either success or "+
						"not_committed (1020) — anything else means this subtest is "+
						"measuring a different failure than the conflict range",
						commitErr, commitErr)
				}
			}
			if conflicted != arm.wantConflict {
				if arm.wantConflict {
					t.Fatalf("the aggregate-index scan read at SERIALIZABLE and its transaction " +
						"COMMITTED after a concurrent update of the scanned COUNT index: the " +
						"scan took no read conflict range, so it reads at snapshot isolation " +
						"regardless of ExecuteProperties")
				}
				t.Fatalf("the COUNT-index fallback behind GetSnapshotRecordCount asked for "+
					"SNAPSHOT and its transaction was CONFLICTED by a concurrent index "+
					"update: the aggregate cursor is reading non-snapshot, so a count that "+
					"promises not to conflict rejects the caller's unrelated write with "+
					"not_committed (1020): %v", commitErr)
			}
		})
	}
}

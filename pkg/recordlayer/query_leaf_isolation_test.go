package recordlayer

// RFC-198 open question 2, answered behaviorally: does any read a SELECT plan
// reaches bypass ExecuteProperties.IsolationLevel?
//
// The question matters because Decision 2 makes in-transaction SQL reads
// SERIALIZABLE, and "serializable" is not a property of the isolation FIELD —
// it is a property of every leaf that consults it. An unconditional Snapshot()
// call reached from a SELECT plan would take no read conflict range and would
// be a silent hole: reads that look isolated, a commit that succeeds, and a
// lost update with no error. The RFC's survey found the unconditional
// Snapshot() sites (range_set.go, bunched_map.go, index_state.go, database.go,
// aggregate_function.go) to be index-maintenance, store-state or ranked-set
// internals rather than query leaves — but "not reached from a SELECT plan" is
// a reachability claim, and reachability claims get a test.
//
// This is that test, and it is BEHAVIORAL rather than a source survey: for
// each leaf the query path scans through, the same scan is run under both
// isolation levels and the outcome that actually matters is measured — whether
// a concurrent write into the scanned range conflicts the reader's commit.
// SERIALIZABLE must conflict (the read took a conflict range); SNAPSHOT must
// not (it took none). A leaf that ignored the level would produce the same
// answer for both, and both directions are asserted, so ignoring it in EITHER
// direction fails.
//
// What gets re-armed if this test ever fails: the SQL layer's promise that an
// explicit transaction's reads are serializable (RFC-198 Decision 2) rests on
// exactly these leaves. A leaf that stops consulting the level makes
// in-transaction SELECTs silently snapshot-isolated on that access path, and
// the lost update criterion 2 pins comes back for any query the planner routes
// through it.

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

// leafScan drains one query leaf over the seeded store. Each entry is a read
// path a SELECT plan reaches: the record scan behind a full table scan, the
// index scan behind an index access path, and the primary-key-only scan behind
// a covering plan that needs keys and not records.
type leafScan struct {
	name string
	// scan drains the leaf with the given properties, returning how many
	// elements it saw (asserted non-zero, so an empty scan can never be
	// mistaken for "no conflict range because nothing was read").
	scan func(t *testing.T, store *FDBRecordStore, index *Index, props ScanProperties) int
}

func queryLeafScans() []leafScan {
	return []leafScan{
		{
			name: "record_scan",
			scan: func(t *testing.T, store *FDBRecordStore, _ *Index, props ScanProperties) int {
				return drainLeaf(t, store.ScanRecords(nil, props))
			},
		},
		{
			name: "index_scan",
			scan: func(t *testing.T, store *FDBRecordStore, index *Index, props ScanProperties) int {
				return drainLeaf(t, store.ScanIndex(index, TupleRangeAll, nil, props))
			},
		},
		{
			name: "record_key_scan",
			scan: func(t *testing.T, store *FDBRecordStore, _ *Index, props ScanProperties) int {
				return drainLeaf(t, store.ScanRecordKeys(nil, props))
			},
		},
	}
}

func drainLeaf[T any](t *testing.T, cursor RecordCursor[T]) int {
	t.Helper()
	defer cursor.Close() //nolint:errcheck
	n := 0
	for {
		res, err := cursor.OnNext(context.Background())
		if err != nil {
			t.Fatalf("leaf scan: %v", err)
		}
		if !res.HasNext() {
			return n
		}
		n++
	}
}

// TestQueryLeavesConsultIsolationLevel is RFC-198 OQ-2's pin.
func TestQueryLeavesConsultIsolationLevel(t *testing.T) {
	t.Parallel()

	const indexName = "Order$price"
	buildMetaData := func(t *testing.T) (*RecordMetaData, *Index) {
		t.Helper()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		index := NewIndex(indexName, Field("price"))
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		if err != nil {
			t.Fatalf("build metadata: %v", err)
		}
		return md, index
	}

	for _, leaf := range queryLeafScans() {
		for _, level := range []struct {
			name         string
			isolation    IsolationLevel
			wantConflict bool
		}{
			{"serializable", IsolationLevelSerializable, true},
			{"snapshot", IsolationLevelSnapshot, false},
		} {
			t.Run(leaf.name+"/"+level.name, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				env := dst.NewSim(41)
				env.Buggify = dst.DisabledBuggifier()
				sim := simfdb.New(env)
				db := NewFDBDatabaseWithBackend(sim).SetEnv(env)
				sub := subspace.FromBytes(tuple.Tuple{"oq2", leaf.name, level.name}.Pack())
				md, index := buildMetaData(t)

				// Seed rows 1..5 and, separately, the row the reader will
				// WRITE. The write target is a record type the scans below
				// never touch through the same keys the concurrent writer
				// uses, so the only thing that can conflict the reader's
				// commit is its READ.
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

				// The READER: a standalone transaction (no retry loop — a
				// retry would mask the conflict this test is measuring).
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

				props := NewScanProperties(ExecuteProperties{IsolationLevel: level.isolation})
				if n := leaf.scan(t, readerStore, index, props); n == 0 {
					t.Fatalf("the %s leaf read nothing: a scan that saw no rows takes no "+
						"conflict range for the trivial reason, and this subtest would "+
						"then pass or fail for reasons unrelated to the isolation level",
						leaf.name)
				}

				// The WRITER: a separate, committed transaction that changes a
				// row inside the range the reader just scanned — both the
				// record key and the index entry move.
				if _, err := db.Run(ctx, func(rctx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rctx).SetMetaDataProvider(md).SetSubspace(sub).Open()
					if err != nil {
						return nil, err
					}
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(3), Price: proto.Int32(3000),
					})
					return nil, err
				}); err != nil {
					t.Fatalf("concurrent write: %v", err)
				}

				// The reader writes a row of its own and commits. Its write
				// set does not overlap the writer's, so the ONLY thing that
				// can conflict this commit is the read conflict range the
				// scan above did or did not take.
				if _, err := readerStore.SaveRecord(&gen.Order{
					OrderId: proto.Int64(99), Price: proto.Int32(99),
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
				if conflicted != level.wantConflict {
					if level.wantConflict {
						t.Fatalf("the %s leaf read at SERIALIZABLE and its transaction "+
							"COMMITTED after a concurrent write into the scanned range: "+
							"the leaf took no read conflict range, so it is reading at "+
							"snapshot isolation regardless of ExecuteProperties. Every "+
							"in-transaction SELECT the planner routes through this leaf "+
							"is silently non-serializable and the lost update RFC-198 "+
							"Decision 2 closes is back (open question 2)", leaf.name)
					}
					t.Fatalf("the %s leaf read at SNAPSHOT and its transaction was "+
						"CONFLICTED by a concurrent write into the scanned range: the "+
						"leaf took a read conflict range at snapshot isolation, so it "+
						"is ignoring ExecuteProperties.IsolationLevel in the other "+
						"direction — snapshot reads that conflict make every "+
						"maintenance path that relies on them (index build, store "+
						"state) contend where it must not (open question 2): %v",
						leaf.name, commitErr)
				}
			})
		}
	}
}

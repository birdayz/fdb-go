package vectorindex

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/simfdb"
	"google.golang.org/protobuf/proto"
)

// vectorLeafFixture carries the kNN indexes the seeded store holds.
type vectorLeafFixture struct {
	vectorIndex  *recordlayer.Index // Order$vec — the HNSW kNN access path
	spfreshIndex *recordlayer.Index // Order$spf — the SPFresh kNN access path
}

// vectorLeafScan drains one kNN query leaf over the seeded store.
type vectorLeafScan struct {
	name string
	scan func(t *testing.T, store *recordlayer.FDBRecordStore, fx vectorLeafFixture, props recordlayer.ScanProperties) int
}

// vectorLeafScans is the vector leaves recordlayer's
// leafIsolationDeclaration names for IndexTypeVector and
// IndexTypeVectorSPFresh.
func vectorLeafScans() []vectorLeafScan {
	return []vectorLeafScan{
		{
			// The VECTOR leaf. A kNN scan is a query leaf like any other — the
			// executor forwards the statement's ScanProperties to it — and its
			// HNSW traversal reads the graph through the transaction. Java
			// derives that read's isolation from ScanProperties exactly as the
			// generic leaf cursor does (VectorIndexMaintainer.java:201-202 is
			// character-for-character KeyValueCursorBase.java:358), so a
			// serializable kNN scan must take conflict ranges.
			name: "vector_scan_by_distance",
			scan: func(t *testing.T, store *recordlayer.FDBRecordStore, fx vectorLeafFixture, props recordlayer.ScanProperties) int {
				return drainVectorLeaf(t, store.ScanIndexByType(fx.vectorIndex, recordlayer.IndexScanByDistance,
					vectorKNNRange(vectorLeafQuery, 5), nil, props))
			},
		},
		{
			// SPFresh is a SEPARATE implementation of the same BY_DISTANCE
			// contract — its own routing, postings and re-rank reads, none of
			// which the HNSW leaf above executes a line of. Declaring it
			// "covered" by that leaf was a false coverage claim: a regression
			// in SPFresh's isolation would have left the whole suite green.
			name: "spfresh_scan_by_distance",
			scan: func(t *testing.T, store *recordlayer.FDBRecordStore, fx vectorLeafFixture, props recordlayer.ScanProperties) int {
				return drainVectorLeaf(t, store.ScanIndexByType(fx.spfreshIndex, recordlayer.IndexScanByDistance,
					vectorKNNRange(vectorLeafQuery, 5), nil, props))
			},
		},
		{
			// The ordered-stream path is a THIRD read path, not a variant of
			// the one above: it widens on demand through its own frontier and
			// was the site that discarded its ScanProperties entirely.
			name: "spfresh_ordered_stream",
			scan: func(t *testing.T, store *recordlayer.FDBRecordStore, fx vectorLeafFixture, props recordlayer.ScanProperties) int {
				return drainVectorLeaf(t, store.ScanIndexByType(fx.spfreshIndex,
					recordlayer.IndexScanByDistanceOrderedStream,
					vectorKNNRange(vectorLeafQuery, 5), nil, props))
			},
		},
	}
}

// vectorLeafQuery is the kNN probe point. It sits next to the seeded rows so
// the search returns them and the concurrent writer's move is inside the
// neighbourhood the reader actually traversed.
var vectorLeafQuery = []float64{3, 3}

// vectorKNNRange builds the BY_DISTANCE TupleRange the vector maintainers
// agree on: Low = (serialized query vector), High = (k).
func vectorKNNRange(query []float64, k int64) recordlayer.TupleRange {
	return recordlayer.TupleRange{
		Low:          tuple.Tuple{serializeVector(query)},
		High:         tuple.Tuple{k},
		LowEndpoint:  recordlayer.EndpointTypeRangeInclusive,
		HighEndpoint: recordlayer.EndpointTypeRangeInclusive,
	}
}

func drainVectorLeaf[T any](t *testing.T, cursor recordlayer.RecordCursor[T]) int {
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

// TestVectorQueryLeavesConsultIsolationLevel is recordlayer's
// TestQueryLeavesConsultIsolationLevel (RFC-198 OQ-2) over the vector leaves:
// each kNN read path, run under both isolation levels, must take a read
// conflict range exactly when the scan is serializable.
func TestVectorQueryLeavesConsultIsolationLevel(t *testing.T) {
	t.Parallel()

	const indexName = "Order$price"
	buildMetaData := func(t *testing.T) (*recordlayer.RecordMetaData, vectorLeafFixture) {
		t.Helper()
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		index := recordlayer.NewIndex(indexName, recordlayer.Field("price"))
		builder.AddIndex("Order", index)
		// (price, quantity) as a 2-D vector: the same records carry both the
		// value index and the kNN access path, so one seeded store serves
		// every leaf and the concurrent write below moves a row in BOTH.
		vecIndex := recordlayer.NewVectorIndex("Order$vec", recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")), 2)
		builder.AddIndex("Order", vecIndex)
		// The SPFresh index over the same 2-D coordinates. A separate index
		// type with a separate implementation of the BY_DISTANCE contract, so
		// it needs its own fixture — sharing the HNSW one would be the very
		// false-coverage claim this fixture exists to remove.
		spfIndex := &recordlayer.Index{
			Name:           "Order$spf",
			Type:           recordlayer.IndexTypeVectorSPFresh,
			RootExpression: recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")),
			Options: map[string]string{
				recordlayer.IndexOptionSPFreshNumDimensions: "2",
			},
		}
		builder.AddIndex("Order", spfIndex)
		md, err := builder.Build()
		if err != nil {
			t.Fatalf("build metadata: %v", err)
		}
		return md, vectorLeafFixture{vectorIndex: vecIndex, spfreshIndex: spfIndex}
	}

	for _, leaf := range vectorLeafScans() {
		for _, level := range []struct {
			name         string
			isolation    recordlayer.IsolationLevel
			wantConflict bool
		}{
			{"serializable", recordlayer.IsolationLevelSerializable, true},
			{"snapshot", recordlayer.IsolationLevelSnapshot, false},
		} {
			t.Run(leaf.name+"/"+level.name, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				env := dst.NewSim(41)
				env.Buggify = dst.DisabledBuggifier()
				sim := simfdb.New(env)
				db := recordlayer.NewFDBDatabaseWithBackend(sim).SetEnv(env)
				sub := subspace.FromBytes(tuple.Tuple{"oq2-vector", leaf.name, level.name}.Pack())
				md, fx := buildMetaData(t)

				// Seed rows 1..5 and, separately, the row the reader will
				// WRITE. The write target is a record type the scans below
				// never touch through the same keys the concurrent writer
				// uses, so the only thing that can conflict the reader's
				// commit is its READ.
				if _, err := db.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
					store, err := recordlayer.NewStoreBuilder().
						SetContext(rctx).SetMetaDataProvider(md).SetSubspace(sub).CreateOrOpen()
					if err != nil {
						return nil, err
					}
					for i := int64(1); i <= 5; i++ {
						if _, err := store.SaveRecord(&gen.Order{
							OrderId: proto.Int64(i), Price: proto.Int32(int32(i)), Quantity: proto.Int32(int32(i)),
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
				readerCtx := recordlayer.NewFDBRecordContext(readerTx, db.Env())
				readerStore, err := recordlayer.NewStoreBuilder().
					SetContext(readerCtx).SetMetaDataProvider(md).SetSubspace(sub).Open()
				if err != nil {
					t.Fatalf("reader store: %v", err)
				}

				props := recordlayer.NewScanProperties(recordlayer.ExecuteProperties{IsolationLevel: level.isolation})
				if n := leaf.scan(t, readerStore, fx, props); n == 0 {
					t.Fatalf("the %s leaf read nothing: a scan that saw no rows takes no "+
						"conflict range for the trivial reason, and this subtest would "+
						"then pass or fail for reasons unrelated to the isolation level",
						leaf.name)
				}

				// The WRITER: a separate, committed transaction that changes a
				// row inside the range the reader just scanned — both the
				// record key and the index entry move.
				if _, err := db.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
					store, err := recordlayer.NewStoreBuilder().
						SetContext(rctx).SetMetaDataProvider(md).SetSubspace(sub).Open()
					if err != nil {
						return nil, err
					}
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(3), Price: proto.Int32(3000), Quantity: proto.Int32(3000),
					})
					return nil, err
				}); err != nil {
					t.Fatalf("concurrent write: %v", err)
				}

				// The reader writes a row of its own and commits. It writes a
				// CUSTOMER — a record type carrying no indexes at all — so its
				// write set cannot overlap the writer's through index
				// maintenance. Writing another Order would: the vector index
				// makes every Order save mutate the shared HNSW graph, and the
				// two transactions would then conflict on their WRITES no
				// matter what the reader read, which reports as a pass on
				// every serializable arm and a failure on every snapshot one.
				// The ONLY thing that may conflict this commit is the read
				// conflict range the scan above did or did not take.
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

// TestVectorLeafNamesMatchCoreDeclaration pins the leaf names recordlayer's
// leafIsolationDeclaration claims for the vector types, which that package
// cannot resolve itself: renaming or dropping a leaf here must fail.
func TestVectorLeafNamesMatchCoreDeclaration(t *testing.T) {
	t.Parallel()
	want := map[string]bool{"vector_scan_by_distance": true, "spfresh_scan_by_distance": true, "spfresh_ordered_stream": true}
	got := map[string]bool{}
	for _, leaf := range vectorLeafScans() {
		got[leaf.name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("leaf %q, claimed by recordlayer's leafIsolationDeclaration, is not run", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("vector leaves %v, want exactly %v (update leafIsolationDeclaration)", got, want)
	}
}

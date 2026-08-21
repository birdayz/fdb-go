package chaos

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// TestFDB_LoadIndexBuildStatePropagatesCountReadFailure pins the half of
// IndexBuildState.loadIndexBuildStateAsync that says NOTHING about missing indexes.
//
// Java catches exactly AggregateFunctionNotSupportedException around
// getSnapshotRecordCount (IndexBuildState.java:76-82) — "very likely it is because
// there is no suitable COUNT type index defined" — and reports an unknown total.
// Every other failure escapes: an I/O error on the count read is not evidence that
// the store has no COUNT index, and reporting it as one turns a transient read
// failure into a build that permanently claims to have no denominator.
//
// The fault is scoped to the record-count subspace so the store still opens and
// still reads its build progress; only the count read fails. That is what makes the
// assertion about the swallow rather than about whichever read happens first.
func TestFDB_LoadIndexBuildStatePropagatesCountReadFailure(t *testing.T) {
	t.Parallel()
	ctx, cancelCtx := chaosRunContext(0)
	defer cancelCtx()

	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	priceIndex := recordlayer.NewIndex("n1_price_idx", recordlayer.Field("price"))
	builder.AddIndex("Order", priceIndex)
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}

	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	cleanDB := recordlayer.NewFDBDatabase(testRealDB)

	// Populate, then put the index into the WRITE_ONLY state that is the only one
	// loadIndexBuildStateAsync looks past.
	_, err = cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(sub).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		for i := int64(1); i <= 4; i++ {
			if _, err := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(i),
				Price:   proto.Int32(int32(i * 10)),
			}); err != nil {
				return nil, err
			}
		}
		_, err = store.ClearAndMarkIndexWriteOnly("n1_price_idx")
		return nil, err
	})
	if err != nil {
		t.Fatalf("populate: %v", err)
	}

	chaosT := NewChaosTransactor(testRealDB, FaultsNone, 1)
	chaosDB := recordlayer.NewFDBDatabaseWithTransactor(chaosT, testRealDB)

	loadState := func() (*recordlayer.IndexBuildState, error) {
		result, err := chaosDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(sub).Open()
			if err != nil {
				return nil, err
			}
			return recordlayer.LoadIndexBuildState(store, priceIndex)
		})
		if err != nil {
			return nil, err
		}
		return result.(*recordlayer.IndexBuildState), nil
	}

	// Control: without the fault the count read succeeds and the denominator is
	// real. Without this the failing case below could pass on a store that never
	// reads the count subspace at all.
	state, err := loadState()
	if err != nil {
		t.Fatalf("control LoadIndexBuildState: %v", err)
	}
	if state.State != recordlayer.IndexStateWriteOnly {
		t.Fatalf("control: index state = %v, want WRITE_ONLY", state.State)
	}
	if state.RecordsInTotal == nil || *state.RecordsInTotal != 4 {
		t.Fatalf("control: RecordsInTotal = %v, want 4", state.RecordsInTotal)
	}

	// Fail exactly the count read.
	countPrefix := sub.Sub(recordlayer.RecordCountKey).Bytes()
	chaosT.InjectReadErrorOnce(countPrefix)

	state, err = loadState()
	if err == nil {
		t.Fatalf("LoadIndexBuildState swallowed an injected read failure and returned %+v; "+
			"only AggregateFunctionNotSupportedError means \"there is no COUNT index\"", state)
	}
	if errors.As(err, new(*recordlayer.AggregateFunctionNotSupportedError)) {
		t.Fatalf("a read failure was reclassified as a missing COUNT index: %v", err)
	}
	if !errors.Is(err, fdb.Error{Code: 1510}) {
		t.Fatalf("LoadIndexBuildState returned %v, want the injected read error to propagate", err)
	}
	if len(chaosT.Log) != 1 || chaosT.Log[0].Fault != FaultReadError {
		t.Fatalf("fault log = %+v, want exactly one FaultReadError", chaosT.Log)
	}
}

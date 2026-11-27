package executor

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"fdb.dev/internal/pb/demopb"
	"fdb.dev/internal/testutil/allocs"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

var testDB *recordlayer.FDBDatabase

// These are the exact physical row types emitted by the record-to-positional
// conversion used by the integration store.  Keeping the helpers next to the
// store fixture makes every plan/value in this file name its real row shape;
// the old tests passed nil/Unknown and therefore could not prove that an
// ordinal was resolved against the row that execution actually emits.
func integrationOrderType() *values.RecordType {
	return PositionalTypeForRecordLayout((&demopb.Order{}).ProtoReflect().Descriptor(), false)
}

// orderUpdate is the transform assigning an Order column, resolved as the SQL
// builder resolves a SET column (values.ResolveAssignmentColumn over the
// descriptor's user identifiers).
func orderUpdate(column string, v values.Value) expressions.UpdateTransform {
	fields := (&demopb.Order{}).ProtoReflect().Descriptor().Fields()
	names := make([]string, fields.Len())
	for i := range names {
		names[i] = values.FieldNameForProtoField(fields.Get(i))
	}
	idx, hits := values.ResolveAssignmentColumn(names, column)
	if hits != 1 {
		panic(fmt.Sprintf("orderUpdate: column %q resolves %d times", column, hits))
	}
	return expressions.UpdateTransform{FieldNames: []string{names[idx]}, FieldOrdinals: []int{idx}, NewValue: v}
}

func integrationCustomerType() *values.RecordType {
	return PositionalTypeForRecordLayout((&demopb.Customer{}).ProtoReflect().Descriptor(), false)
}

func integrationTypedRecordType() *values.RecordType {
	return PositionalTypeForRecordLayout((&demopb.TypedRecord{}).ProtoReflect().Descriptor(), false)
}

func integrationField(t testing.TB, owner plans.RecordQueryPlan, ordinal int) values.Value {
	t.Helper()
	if owner == nil || owner.GetResultValue() == nil {
		t.Fatal("integration field owner has no exact result value")
	}
	return mustTestFieldOrdinal(t, owner.GetResultValue(), ordinal)
}

func integrationJoinResult(
	t testing.TB,
	outer, inner plans.RecordQueryPlan,
	outerAlias, innerAlias values.CorrelationIdentifier,
	joinType plans.JoinType,
) values.Value {
	t.Helper()
	if !values.SameLeg(outerAlias, outerAlias) || !values.SameLeg(innerAlias, innerAlias) ||
		values.SameLeg(outerAlias, innerAlias) {
		t.Fatalf("join result requires two stated, distinct source aliases: outer=%q inner=%q", outerAlias, innerAlias)
	}
	outerType, ok := outer.GetResultType().(*values.RecordType)
	if !ok || outerType == nil {
		t.Fatalf("join outer type = %T, want exact record", outer.GetResultType())
	}
	innerType, ok := inner.GetResultType().(*values.RecordType)
	if !ok || innerType == nil {
		t.Fatalf("join inner type = %T, want exact record", inner.GetResultType())
	}
	fields := make([]values.RecordConstructorField, 0, len(outerType.Fields)+len(innerType.Fields))
	appendFields := func(alias values.CorrelationIdentifier, source *values.RecordType, nullSupplying bool) {
		var flowed values.Type = source
		if nullSupplying {
			flowed = values.WithNullability(flowed, true)
		}
		qov := mustTestQOV(t, alias, flowed)
		for ordinal, field := range source.Fields {
			resolved, err := values.ResolveOrdinalSeedField(qov, ordinal)
			if err != nil {
				t.Fatalf("resolve join source %s field %d: %v", alias, ordinal, err)
			}
			fields = append(fields, values.RecordConstructorField{Name: field.Name, Value: resolved})
		}
	}
	appendFields(outerAlias, outerType, joinType == plans.JoinFullOuter)
	appendFields(innerAlias, innerType, joinType == plans.JoinLeftOuter || joinType == plans.JoinFullOuter)
	return values.NewRawRecordConstructorValue(fields...)
}

func TestMain(m *testing.M) { os.Exit(runTests(m)) }

func runTests(m *testing.M) (code int) {
	// Allocation-only children run one exact test without FDB client/container
	// background goroutines contributing to process-wide MemStats.
	if allocs.IsChildProcess() {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithAPIVersion(720),
	)
	if err != nil {
		panic("failed to start FDB container: " + err.Error())
	}

	defer func() {
		if err := foundationdbtc.Terminate(context.Background(), container); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}()

	clusterFile, err := container.ClusterFile(ctx)
	if err != nil {
		panic("failed to get cluster file: " + err.Error())
	}

	tmpFile, err := os.CreateTemp("", "fdb_executor_test_*.txt")
	if err != nil {
		panic(err.Error())
	}
	defer os.Remove(tmpFile.Name())

	_, _ = tmpFile.WriteString(clusterFile)
	_ = tmpFile.Close()

	fdb.MustAPIVersion(720)
	db, err := fdb.OpenDatabase(tmpFile.Name())
	if err != nil {
		panic("failed to open FDB: " + err.Error())
	}
	testDB = recordlayer.NewFDBDatabase(db)

	code = m.Run()

	return code
}

func testSubspace(t *testing.T) subspace.Subspace {
	return subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
}

func setupStore(t *testing.T, extraIndexes ...*recordlayer.Index) *recordlayer.FDBRecordStore {
	t.Helper()
	ctx := context.Background()
	ks := testSubspace(t)

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", recordlayer.NewIndex("order_price_idx", recordlayer.Field("price")))
	for _, index := range extraIndexes {
		builder.AddIndex("Order", index)
	}
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}

	var store *recordlayer.FDBRecordStore
	_, err = testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		var err error
		store, err = recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		return nil, err
	})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	// The subspace is keyed by t.Name() alone, which REPEATS across
	// `-count=N` reruns inside one binary (same container): a test that
	// inserts fixed primary keys would collide with its previous
	// iteration's rows ("record already exists"). Wipe the test's records
	// at the end of each iteration so every run starts from an empty store.
	t.Cleanup(func() {
		_, cerr := testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
			s, oerr := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if oerr != nil {
				return nil, oerr
			}
			return nil, s.DeleteAllRecords()
		})
		if cerr != nil {
			t.Errorf("cleanup: wiping test subspace: %v", cerr)
		}
	})
	return store
}

func insertOrders(t *testing.T, store *recordlayer.FDBRecordStore, orders ...*demopb.Order) {
	t.Helper()
	ctx := context.Background()
	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}
		for _, o := range orders {
			if _, err := s.SaveRecord(o); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("insert orders: %v", err)
	}
}

// ---------- Ordinal output rows over real FDB ----------
// internal/relational/resultset reads these rows by ordinal and tests its accessors.

// collectSlots drains cursor and returns each row's positional slots.
func collectSlots(t *testing.T, ctx context.Context, cursor recordlayer.RecordCursor[QueryResult]) [][]any {
	t.Helper()
	defer cursor.Close()
	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	rows := make([][]any, len(results))
	for i, r := range results {
		if r.Positional == nil {
			t.Fatalf("row %d has no positional output row", i)
		}
		rows[i] = r.Positional.Slots
	}
	return rows
}

func insertCustomers(t *testing.T, store *recordlayer.FDBRecordStore, customers ...*demopb.Customer) {
	t.Helper()
	ctx := context.Background()
	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}
		for _, c := range customers {
			if _, err := s.SaveRecord(c); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("insert customers: %v", err)
	}
}

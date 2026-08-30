package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

var testDB *recordlayer.FDBDatabase

// These are the exact physical row types emitted by the record-to-positional
// conversion used by the integration store.  Keeping the helpers next to the
// store fixture makes every plan/value in this file name its real row shape;
// the old tests passed nil/Unknown and therefore could not prove that an
// ordinal was resolved against the row that execution actually emits.
func integrationOrderType() *values.RecordType {
	return PositionalTypeForRecordLayout((&gen.Order{}).ProtoReflect().Descriptor(), false)
}

func integrationCustomerType() *values.RecordType {
	return PositionalTypeForRecordLayout((&gen.Customer{}).ProtoReflect().Descriptor(), false)
}

func integrationTypedRecordType() *values.RecordType {
	return PositionalTypeForRecordLayout((&gen.TypedRecord{}).ProtoReflect().Descriptor(), false)
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

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithAPIVersion(720),
	)
	if err != nil {
		panic("failed to start FDB container: " + err.Error())
	}

	clusterFile, err := container.ClusterFile(ctx)
	if err != nil {
		panic("failed to get cluster file: " + err.Error())
	}

	tmpFile, err := os.CreateTemp("", "fdb_executor_test_*.txt")
	if err != nil {
		panic(err.Error())
	}
	_, _ = tmpFile.WriteString(clusterFile)
	_ = tmpFile.Close()

	fdb.MustAPIVersion(720)
	db, err := fdb.OpenDatabase(tmpFile.Name())
	if err != nil {
		panic("failed to open FDB: " + err.Error())
	}
	testDB = recordlayer.NewFDBDatabase(db)

	code := m.Run()

	_ = container.Terminate(context.Background())
	_ = os.Remove(tmpFile.Name())
	os.Exit(code)
}

func testSubspace(t *testing.T) subspace.Subspace {
	return subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
}

func setupStore(t *testing.T) *recordlayer.FDBRecordStore {
	t.Helper()
	ctx := context.Background()
	ks := testSubspace(t)

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", recordlayer.NewIndex("order_price_idx", recordlayer.Field("price")))
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

func insertOrders(t *testing.T, store *recordlayer.FDBRecordStore, orders ...*gen.Order) {
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

// TestIntegration_ScanPlan_AllRecords tests scanning all records from a real FDB store.
func TestIntegration_ScanPlan_AllRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store, &gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
	}, &gen.Order{
		OrderId: proto.Int64(2),
		Price:   proto.Int32(200),
	}, &gen.Order{
		OrderId: proto.Int64(3),
		Price:   proto.Int32(300),
	})

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan(nil, values.NewAnyRecordType(false), false))
		cursor, err := ExecutePlan(ctx, scan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) < 3 {
			t.Fatalf("scan returned %d results, want >= 3", len(results))
		}

		for _, r := range results {
			if r.Record == nil {
				t.Fatal("result has nil Record")
			}
			if r.PrimaryKey == nil {
				t.Fatal("result has nil PrimaryKey")
			}
			datum, ok := rowMapOK(r)
			if !ok {
				t.Fatalf("datum type = %T, want map[string]any", r.Positional)
			}
			if datum["order_id"] == nil {
				t.Error("ORDER_ID is nil in datum")
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ScanPlan_TypeFilter tests scanning with a type filter.
func TestIntegration_ScanPlan_TypeFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store, &gen.Order{
		OrderId: proto.Int64(10),
		Price:   proto.Int32(999),
	})

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan(nil, values.NewAnyRecordType(false), false))
		typeFilter := mustExecutorConstruct(plans.NewRecordQueryTypeFilterPlan([]string{"Order"}, scan))

		cursor, err := ExecutePlan(ctx, typeFilter, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) == 0 {
			t.Fatal("type filter returned 0 results, want >= 1")
		}
		for _, r := range results {
			if r.Record.RecordType.Name != "Order" {
				t.Errorf("record type = %q, want Order", r.Record.RecordType.Name)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_FilterPlan tests filtering records by a predicate.
func TestIntegration_FilterPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store, &gen.Order{
		OrderId: proto.Int64(100),
		Price:   proto.Int32(50),
	}, &gen.Order{
		OrderId: proto.Int64(101),
		Price:   proto.Int32(500),
	})

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scan, 2),
					predicates.Comparison{
						Type:    predicates.ComparisonGreaterThan,
						Operand: values.LiteralValue(int64(100)),
					},
				),
			},
			scan,
		))

		cursor, err := ExecutePlan(ctx, filter, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("filter returned %d results, want 1 (price > 100)", len(results))
		}
		price, _ := rowMap(results[0])["price"]
		if price != int64(500) {
			t.Errorf("price = %v, want 500", price)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_SortLimitPlan tests sort + limit against real records.
func TestIntegration_SortLimitPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(201), Price: proto.Int32(300)},
		&gen.Order{OrderId: proto.Int64(202), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(203), Price: proto.Int32(200)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		sorted := mustExecutorConstruct(plans.NewRecordQueryInMemorySortPlan(
			scan,
			[]plans.SortKey{{Field: "price", ValueExpr: integrationField(t, scan, 2), Desc: false}},
		))
		limited := mustExecutorConstruct(plans.NewRecordQueryLimitPlan(sorted, 2, 0))

		cursor, err := ExecutePlan(ctx, limited, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("got %d results, want 2", len(results))
		}

		p1, _ := rowMap(results[0])["price"].(int64)
		p2, _ := rowMap(results[1])["price"].(int64)
		if p1 > p2 {
			t.Errorf("results not sorted ASC: price[0]=%d > price[1]=%d", p1, p2)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_SortContinuation_BytesKeyStraddle_F33 pins F33 end-to-end: an
// ORDER BY on a BYTES column (Order.vector_data) whose in-memory sort buffer
// straddles page boundaries — forced by a ScannedRecordsLimit that stops the inner
// scan mid-buffer, so the partial buffer rides an encodeSortContinuation and is
// restored by decodeSortContinuation on resume — returns rows in the CORRECT byte
// order. Before F33 the buffered []byte sort keys resumed as base64 STRINGS (JSON),
// so the comparator sorted them in base64-string order (wrong) instead of
// lexicographic byte order. The byte values below deliberately order differently
// under bytes.Compare than their base64 strings, and include high bytes (0xFF) that
// JSON base64-encodes, so a string-typed resume misorders them.
func TestIntegration_SortContinuation_BytesKeyStraddle_F33(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(1), VectorData: []byte{0x02, 0xff}},
		&gen.Order{OrderId: proto.Int64(2), VectorData: []byte{0x01}},
		&gen.Order{OrderId: proto.Int64(3), VectorData: []byte{0x03, 0x00}},
		&gen.Order{OrderId: proto.Int64(4), VectorData: []byte{0x00, 0xaa}},
		&gen.Order{OrderId: proto.Int64(5), VectorData: []byte{0x02, 0x01}},
		&gen.Order{OrderId: proto.Int64(6), VectorData: []byte{0x01, 0xff}},
	)

	// Expected ASC lexicographic byte order of vector_data.
	wantOrder := [][]byte{
		{0x00, 0xaa}, {0x01}, {0x01, 0xff}, {0x02, 0x01}, {0x02, 0xff}, {0x03, 0x00},
	}

	// vector_data is Order proto field 8 → positional ordinal 7 (0-based proto
	// declaration order: order_id0, flower1, price2, tags3, quantity4, coord_x5,
	// coord_y6, vector_data7 — the sibling PRICE tests use ordinal 2).
	const vectorDataOrdinal = 7

	var got [][]byte
	var continuation []byte
	pages := 0
	for {
		pages++
		if pages > 50 {
			t.Fatal("resume loop did not converge (over 50 pages)")
		}
		exhausted, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			s, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
				SetSubspace(testSubspace(t)).Open()
			if err != nil {
				return nil, err
			}
			scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
			sorted := mustExecutorConstruct(plans.NewRecordQueryInMemorySortPlan(
				scan,
				[]plans.SortKey{{
					Field:     "vector_data",
					ValueExpr: integrationField(t, scan, vectorDataOrdinal),
					Desc:      false,
				}},
			))
			// Stop the inner scan after 2 records per transaction so the sort buffer
			// straddles page boundaries and rides the continuation.
			props := recordlayer.DefaultExecuteProperties().WithScannedRecordsLimit(2)
			cursor, err := ExecutePlan(ctx, sorted, s, EmptyEvaluationContext(), continuation, props)
			if err != nil {
				return nil, err
			}
			defer cursor.Close()

			var nextCont []byte
			done := false
			for {
				res, oerr := cursor.OnNext(ctx)
				if oerr != nil {
					return nil, oerr
				}
				if res.HasNext() {
					v, ok := res.GetValue().Positional.Get(vectorDataOrdinal)
					if !ok {
						return nil, fmt.Errorf("no slot at ordinal %d", vectorDataOrdinal)
					}
					b, isBytes := v.([]byte)
					if !isBytes {
						return nil, fmt.Errorf("vector_data slot = %#v (%T), want []byte — a JSON-lossy resume flips it to a base64 string", v, v)
					}
					got = append(got, b)
					continue
				}
				if res.GetNoNextReason().IsSourceExhausted() {
					done = true
				} else {
					cb, cerr := res.GetContinuation().ToBytes()
					if cerr != nil {
						return nil, cerr
					}
					nextCont = cb
				}
				break
			}
			continuation = nextCont
			return done, nil
		})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if exhausted.(bool) {
			break
		}
	}

	// The straddle must actually have occurred (more than one page) or the test
	// never exercised the sort continuation encode/decode.
	if pages < 2 {
		t.Fatalf("sort completed in %d page(s); expected multiple pages (the scan limit did not force a straddle)", pages)
	}
	if len(got) != len(wantOrder) {
		t.Fatalf("got %d rows, want %d: %v", len(got), len(wantOrder), got)
	}
	for i, want := range wantOrder {
		if !bytes.Equal(got[i], want) {
			t.Errorf("row %d vector_data = %x, want %x (sort order corrupted across the resume)", i, got[i], want)
		}
	}
}

// TestIntegration_SortContinuation_ArrayStructStraddle_F53 pins F53 end-to-end:
// an ORDER BY over a table with an ARRAY column (Order.tags, []any in the row
// domain) and a STRUCT column (Order.flower, a raw proto.Message) whose
// in-memory sort buffer straddles page boundaries — forced by a
// ScannedRecordsLimit that stops the inner scan mid-buffer, so the buffered
// rows ride encodeSortContinuation and are restored by decodeSortContinuation
// (with the store-metadata descriptor resolver) on resume. Before F53 the FIRST
// checkpoint failed hard with "continuation: cannot encode value of type
// []any" — a completely legitimate `SELECT * ... ORDER BY` could not resume at
// all. The resumed rows must carry the tags array ([]any of strings, not a
// lossy blob) and the flower struct (a proto message field-for-field equal to
// what was stored) intact.
func TestIntegration_SortContinuation_ArrayStructStraddle_F53(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	type wantRow struct {
		price      int64
		tags       []string
		flowerType string
	}
	// Inserted out of price order so the sort actually reorders.
	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(300), Tags: []string{"c1", "c2"}, Flower: &gen.Flower{Type: proto.String("rose"), Color: gen.Color_RED.Enum()}},
		&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100), Tags: []string{"a1"}, Flower: &gen.Flower{Type: proto.String("lily"), Color: gen.Color_BLUE.Enum()}},
		&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(500), Tags: []string{"e1", "e2", "e3"}, Flower: &gen.Flower{Type: proto.String("tulip"), Color: gen.Color_YELLOW.Enum()}},
		&gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(200), Tags: []string{"b1", "b2"}, Flower: &gen.Flower{Type: proto.String("iris"), Color: gen.Color_PINK.Enum()}},
		&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(400), Tags: []string{"d1"}, Flower: &gen.Flower{Type: proto.String("daisy"), Color: gen.Color_RED.Enum()}},
	)
	want := []wantRow{
		{100, []string{"a1"}, "lily"},
		{200, []string{"b1", "b2"}, "iris"},
		{300, []string{"c1", "c2"}, "rose"},
		{400, []string{"d1"}, "daisy"},
		{500, []string{"e1", "e2", "e3"}, "tulip"},
	}

	// Order proto declaration order: order_id(0), flower(1), price(2), tags(3).
	const flowerOrdinal, priceOrdinal, tagsOrdinal = 1, 2, 3

	var got []wantRow
	var continuation []byte
	pages := 0
	for {
		pages++
		if pages > 50 {
			t.Fatal("resume loop did not converge (over 50 pages)")
		}
		exhausted, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			s, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
				SetSubspace(testSubspace(t)).Open()
			if err != nil {
				return nil, err
			}
			scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
			sorted := mustExecutorConstruct(plans.NewRecordQueryInMemorySortPlan(
				scan,
				[]plans.SortKey{{
					Field:     "price",
					ValueExpr: integrationField(t, scan, priceOrdinal),
					Desc:      false,
				}},
			))
			// Stop the inner scan after 2 records per transaction so the sort buffer
			// (with its ARRAY + STRUCT slots) straddles page boundaries and rides
			// the continuation.
			props := recordlayer.DefaultExecuteProperties().WithScannedRecordsLimit(2)
			cursor, err := ExecutePlan(ctx, sorted, s, EmptyEvaluationContext(), continuation, props)
			if err != nil {
				return nil, err
			}
			defer cursor.Close()

			var nextCont []byte
			done := false
			for {
				res, oerr := cursor.OnNext(ctx)
				if oerr != nil {
					return nil, oerr
				}
				if res.HasNext() {
					pos := res.GetValue().Positional
					pv, ok := pos.Get(priceOrdinal)
					if !ok {
						return nil, fmt.Errorf("no slot at price ordinal %d", priceOrdinal)
					}
					price, isInt := pv.(int64)
					if !isInt {
						return nil, fmt.Errorf("price slot = %#v (%T), want int64", pv, pv)
					}
					tv, ok := pos.Get(tagsOrdinal)
					if !ok {
						return nil, fmt.Errorf("no slot at tags ordinal %d", tagsOrdinal)
					}
					tagsAny, isList := tv.([]any)
					if !isList {
						return nil, fmt.Errorf("tags slot = %#v (%T), want []any — the ARRAY column must survive the sort-continuation resume", tv, tv)
					}
					tags := make([]string, len(tagsAny))
					for i, e := range tagsAny {
						str, isStr := e.(string)
						if !isStr {
							return nil, fmt.Errorf("tags[%d] = %#v (%T), want string", i, e, e)
						}
						tags[i] = str
					}
					fv, ok := pos.Get(flowerOrdinal)
					if !ok {
						return nil, fmt.Errorf("no slot at flower ordinal %d", flowerOrdinal)
					}
					// Rows carry *gen.Flower on BOTH sides of a checkpoint: the
					// restore rebuilds the ENCODED representation (a generated
					// row encodes flag 0 → protoregistry), so the resumed slot
					// keeps the generated concrete type. That identity is
					// load-bearing — the %T-keyed group/dedup paths
					// (computeGroupKey, packedDedupKey) would otherwise never
					// match a restored row against an equal fresh one.
					fm, isFlower := fv.(*gen.Flower)
					if !isFlower {
						return nil, fmt.Errorf("flower slot = %#v (%T), want *gen.Flower — the STRUCT column must survive the sort-continuation resume with its generated type", fv, fv)
					}
					got = append(got, wantRow{
						price:      price,
						tags:       tags,
						flowerType: fm.GetType(),
					})
					continue
				}
				if res.GetNoNextReason().IsSourceExhausted() {
					done = true
				} else {
					cb, cerr := res.GetContinuation().ToBytes()
					if cerr != nil {
						return nil, cerr
					}
					nextCont = cb
				}
				break
			}
			continuation = nextCont
			return done, nil
		})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if exhausted.(bool) {
			break
		}
	}

	// The straddle must actually have occurred (more than one page) or the test
	// never exercised the sort continuation encode/decode with ARRAY/STRUCT slots.
	if pages < 2 {
		t.Fatalf("sort completed in %d page(s); expected multiple pages (the scan limit did not force a straddle)", pages)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.price != w.price {
			t.Errorf("row %d price = %d, want %d (sort order corrupted across the resume)", i, g.price, w.price)
		}
		if len(g.tags) != len(w.tags) {
			t.Errorf("row %d tags = %v, want %v", i, g.tags, w.tags)
		} else {
			for j := range w.tags {
				if g.tags[j] != w.tags[j] {
					t.Errorf("row %d tags[%d] = %q, want %q (ARRAY column corrupted across the resume)", i, j, g.tags[j], w.tags[j])
				}
			}
		}
		if g.flowerType != w.flowerType {
			t.Errorf("row %d flower.type = %q, want %q (STRUCT column corrupted across the resume)", i, g.flowerType, w.flowerType)
		}
	}
}

// TestIntegration_DeletePlan tests deleting a record via the executor.
func TestIntegration_DeletePlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store, &gen.Order{
		OrderId: proto.Int64(500),
		Price:   proto.Int32(42),
	})

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		del := mustExecutorConstruct(plans.NewRecordQueryDeletePlan(scan, "Order"))

		cursor, err := ExecutePlan(ctx, del, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("delete returned %d results, want 1", len(results))
		}

		rec, err := s.LoadRecord(tuple.Tuple{int64(500)})
		if err != nil {
			t.Fatalf("LoadRecord: %v", err)
		}
		if rec != nil {
			t.Error("record still exists after delete")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_IndexScan tests index scan via ComparisonRange.
func TestIntegration_IndexScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(301), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(302), Price: proto.Int32(150)},
		&gen.Order{OrderId: proto.Int64(303), Price: proto.Int32(250)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		eqRange := predicates.EmptyComparisonRange()
		comp := &predicates.Comparison{
			Type:    predicates.ComparisonGreaterThanEq,
			Operand: values.LiteralValue(int64(100)),
		}
		res := eqRange.Merge(comp)
		if !res.Ok {
			t.Fatal("merge failed")
		}

		indexPlan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			[]*predicates.ComparisonRange{res.Range},
			[]string{"Order"},
			integrationOrderType(),
			false,
		)).WithKeyComponentTypes([]values.Type{values.NullableInt})

		cursor, err := ExecutePlan(ctx, indexPlan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("index scan returned %d results, want 2 (price >= 100)", len(results))
		}
		for _, r := range results {
			price, _ := rowMap(r)["price"].(int64)
			if price < 100 {
				t.Errorf("index scan returned price=%d, should be >= 100", price)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_UpdatePlan tests updating records via the executor.
func TestIntegration_UpdatePlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(601), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(602), Price: proto.Int32(200)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scan, 0),
					predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: values.LiteralValue(int64(601)),
					},
				),
			},
			scan,
		))
		update := mustExecutorConstruct(plans.NewRecordQueryUpdatePlan(filter, "Order", []expressions.UpdateTransform{
			{FieldPath: "price", NewValue: values.LiteralValue(int64(999))},
		}))

		cursor, err := ExecutePlan(ctx, update, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("update returned %d results, want 1", len(results))
		}

		rec, err := s.LoadRecord(tuple.Tuple{int64(601)})
		if err != nil {
			t.Fatalf("LoadRecord: %v", err)
		}
		if rec == nil {
			t.Fatal("record not found after update")
		}
		updated := rec.Record.(*gen.Order)
		if updated.GetPrice() != 999 {
			t.Errorf("price after update = %d, want 999", updated.GetPrice())
		}

		untouched, err := s.LoadRecord(tuple.Tuple{int64(602)})
		if err != nil {
			t.Fatalf("LoadRecord: %v", err)
		}
		if untouched == nil {
			t.Fatal("untouched record not found")
		}
		other := untouched.Record.(*gen.Order)
		if other.GetPrice() != 200 {
			t.Errorf("untouched order price = %d, want 200", other.GetPrice())
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_UpdatePlan_ExactTargetOwnerBinding pins the two names that
// legitimately address an UPDATE input row. The selected physical child owns
// a generated quantifier edge, while SQL SET expressions retain the target
// record name. Both must bind the same exact row without erasing nested values.
func TestIntegration_UpdatePlan_ExactTargetOwnerBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)
	wantFlower := &gen.Flower{Type: proto.String("rose"), Color: gen.Color_BLUE.Enum()}

	insertOrders(t, store,
		&gen.Order{
			OrderId: proto.Int64(603),
			Price:   proto.Int32(100),
			Flower:  proto.Clone(wantFlower).(*gen.Flower),
		},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		rowType := integrationOrderType()
		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, rowType, false))
		target := mustTestQOV(t, values.NamedCorrelationIdentifier("Order"), rowType)
		update := mustExecutorConstruct(plans.NewRecordQueryUpdatePlan(scan, "Order", []expressions.UpdateTransform{
			{
				FieldPath: "price",
				NewValue: &values.ArithmeticValue{
					Op:    values.OpAdd,
					Left:  mustTestFieldOrdinal(t, target, 2),
					Right: values.LiteralValue(int64(7)),
				},
			},
			{
				FieldPath: "flower",
				NewValue:  mustTestFieldOrdinal(t, target, 1),
			},
		}))

		cursor, err := ExecutePlan(ctx, update, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()
		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("update returned %d results, want 1", len(results))
		}

		rec, err := s.LoadRecord(tuple.Tuple{int64(603)})
		if err != nil {
			t.Fatalf("LoadRecord: %v", err)
		}
		updated := rec.Record.(*gen.Order)
		if updated.GetPrice() != 107 {
			t.Errorf("price after target-owned arithmetic = %d, want 107", updated.GetPrice())
		}
		if !proto.Equal(updated.GetFlower(), wantFlower) {
			t.Errorf("nested FLOWER identity update = %v, want %v", updated.GetFlower(), wantFlower)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_UpdatePlan_ExactTargetOwnerRejectsForeignViews proves that
// adding the SQL target owner is not a name-based fallback. A same-spelled
// owner with another exact type conflicts, and an unrelated owner remains
// unbound; neither failed transform may stage a write.
func TestIntegration_UpdatePlan_ExactTargetOwnerRejectsForeignViews(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		value    func(testing.TB, *values.RecordType) values.Value
		wantCode values.ResolutionErrorCode
	}{
		{
			name: "same spelling wrong exact type",
			value: func(t testing.TB, _ *values.RecordType) values.Value {
				wrongType := exactTestRowType(values.Field{Name: "price", FieldType: values.NullableLong})
				wrongOwner := mustTestQOV(t, values.NamedCorrelationIdentifier("Order"), wrongType)
				return mustTestFieldOrdinal(t, wrongOwner, 0)
			},
			wantCode: values.CorrelationTypeConflict,
		},
		{
			name: "foreign owner",
			value: func(t testing.TB, rowType *values.RecordType) values.Value {
				foreignOwner := mustTestQOV(t, values.NamedCorrelationIdentifier("Other"), rowType)
				return mustTestFieldOrdinal(t, foreignOwner, 2)
			},
			wantCode: values.UnboundCorrelation,
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := setupStore(t)
			id := int64(604 + i)
			insertOrders(t, store, &gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(100)})

			_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				s, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
					SetSubspace(testSubspace(t)).Open()
				if err != nil {
					return nil, err
				}

				rowType := integrationOrderType()
				scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, rowType, false))
				update := mustExecutorConstruct(plans.NewRecordQueryUpdatePlan(scan, "Order", []expressions.UpdateTransform{
					{FieldPath: "price", NewValue: test.value(t, rowType)},
				}))

				cursor, execErr := ExecutePlan(ctx, update, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
				if cursor != nil {
					defer cursor.Close()
					if execErr == nil {
						_, execErr = CollectAll(ctx, cursor)
					}
				}
				var resolutionErr *values.ResolutionError
				if !errors.As(execErr, &resolutionErr) || resolutionErr.Code() != test.wantCode {
					t.Fatalf("ExecutePlan error = %v, want resolution code %d", execErr, test.wantCode)
				}

				rec, err := s.LoadRecord(tuple.Tuple{id})
				if err != nil {
					t.Fatalf("LoadRecord: %v", err)
				}
				if got := rec.Record.(*gen.Order).GetPrice(); got != 100 {
					t.Fatalf("failed transform staged PRICE=%d, want unchanged 100", got)
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestIntegration_ScanDatum_Shape verifies that scan datum maps contain
// the correct keys and values from proto deserialization.
func TestIntegration_ScanDatum_Shape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(701), Price: proto.Int32(42)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))

		cursor, err := ExecutePlan(ctx, scan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("scan returned %d results, want 1", len(results))
		}

		datum, _ := rowMapOK(results[0])
		if datum["order_id"] != int64(701) {
			t.Errorf("ORDER_ID = %v, want 701", datum["order_id"])
		}
		if datum["price"] != int64(42) {
			t.Errorf("PRICE = %v, want 42", datum["price"])
		}
		// The ordinal row carries every descriptor field as a slot; an
		// unset field reads as nil (SQL NULL) — present-nil, not key-absent.
		if datum["flower"] != nil {
			t.Errorf("FLOWER (unset) = %v, want nil (SQL NULL)", datum["flower"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_IndexScan_Equality tests index scan with equality match.
func TestIntegration_IndexScan_Equality(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(901), Price: proto.Int32(77)},
		&gen.Order{OrderId: proto.Int64(902), Price: proto.Int32(88)},
		&gen.Order{OrderId: proto.Int64(903), Price: proto.Int32(77)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		eqRange := predicates.EmptyComparisonRange()
		comp := &predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.LiteralValue(int64(77)),
		}
		res := eqRange.Merge(comp)
		if !res.Ok {
			t.Fatal("merge failed")
		}

		indexPlan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			[]*predicates.ComparisonRange{res.Range},
			[]string{"Order"},
			integrationOrderType(),
			false,
		)).WithKeyComponentTypes([]values.Type{values.NullableInt})

		cursor, err := ExecutePlan(ctx, indexPlan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("index equality scan returned %d results, want 2 (price == 77)", len(results))
		}
		for _, r := range results {
			price, _ := rowMap(r)["price"].(int64)
			if price != 77 {
				t.Errorf("index scan returned price=%d, want 77", price)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_IndexScan_BoundedRange tests index scan with both lower and upper bounds.
func TestIntegration_IndexScan_BoundedRange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(1001), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(1002), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(1003), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(1004), Price: proto.Int32(150)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		cr := predicates.EmptyComparisonRange()
		lowRes := cr.Merge(&predicates.Comparison{
			Type:    predicates.ComparisonGreaterThanEq,
			Operand: values.LiteralValue(int64(50)),
		})
		if !lowRes.Ok {
			t.Fatal("merge low failed")
		}
		highRes := lowRes.Range.Merge(&predicates.Comparison{
			Type:    predicates.ComparisonLessThan,
			Operand: values.LiteralValue(int64(150)),
		})
		if !highRes.Ok {
			t.Fatal("merge high failed")
		}

		indexPlan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			[]*predicates.ComparisonRange{highRes.Range},
			[]string{"Order"},
			integrationOrderType(),
			false,
		)).WithKeyComponentTypes([]values.Type{values.NullableInt})

		cursor, err := ExecutePlan(ctx, indexPlan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("bounded range scan returned %d results, want 2 (50 <= price < 150)", len(results))
		}
		for _, r := range results {
			price, _ := rowMap(r)["price"].(int64)
			if price < 50 || price >= 150 {
				t.Errorf("price=%d outside [50, 150)", price)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_FilterSortLimit_Pipeline tests a realistic pipeline:
// scan → filter → sort → limit.
func TestIntegration_FilterSortLimit_Pipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(1101), Price: proto.Int32(500)},
		&gen.Order{OrderId: proto.Int64(1102), Price: proto.Int32(300)},
		&gen.Order{OrderId: proto.Int64(1103), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(1104), Price: proto.Int32(400)},
		&gen.Order{OrderId: proto.Int64(1105), Price: proto.Int32(200)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scan, 2),
					predicates.Comparison{
						Type:    predicates.ComparisonGreaterThan,
						Operand: values.LiteralValue(int64(150)),
					},
				),
			},
			scan,
		))
		sorted := mustExecutorConstruct(plans.NewRecordQueryInMemorySortPlan(
			filter,
			[]plans.SortKey{{Field: "price", ValueExpr: integrationField(t, scan, 2), Desc: true}},
		))
		limited := mustExecutorConstruct(plans.NewRecordQueryLimitPlan(sorted, 2, 0))

		cursor, err := ExecutePlan(ctx, limited, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("pipeline returned %d results, want 2 (top-2 by price DESC where price > 150)", len(results))
		}

		p1, _ := rowMap(results[0])["price"].(int64)
		p2, _ := rowMap(results[1])["price"].(int64)
		if p1 != 500 || p2 != 400 {
			t.Errorf("prices = [%d, %d], want [500, 400]", p1, p2)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ---------- ResultSet integration tests ----------

// TestIntegration_ResultSet_TypedAccess executes a scan plan against real FDB,
// wraps the cursor in RecordLayerResultSet, and verifies typed column access.
func TestIntegration_ResultSet_TypedAccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(2001), Price: proto.Int32(77)},
		&gen.Order{OrderId: proto.Int64(2002), Price: proto.Int32(88)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		// Project ORDER_ID, PRICE so the executor emits an ordinal output row aligned
		// to the result-set columns — the name-keyed Datum no longer backs the read.
		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		proj := mustExecutorConstruct(plans.NewRecordQueryProjectionPlan(
			[]values.Value{integrationField(t, scan, 0), integrationField(t, scan, 2)},
			scan,
		))
		cursor, err := ExecutePlan(ctx, proj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}

		cols := []ColumnDef{
			{Name: "order_id", TypeName: "BIGINT", Nullable: api.ColumnNoNulls},
			{Name: "price", TypeName: "BIGINT", Nullable: api.ColumnNullable},
		}
		rs := NewRecordLayerResultSet(ctx, cursor, cols)
		defer rs.Close()

		md := rs.MetaData()
		if md.ColumnCount() != 2 {
			t.Fatalf("ColumnCount = %d, want 2", md.ColumnCount())
		}
		name, _ := md.ColumnName(1)
		if name != "order_id" {
			t.Errorf("ColumnName(1) = %q, want ORDER_ID", name)
		}

		var ids []int64
		for rs.Next() {
			id, err := rs.Long(1)
			if err != nil {
				t.Fatalf("Long(1): %v", err)
			}
			if rs.WasNull() {
				t.Error("ORDER_ID should not be null")
			}

			price, err := rs.Long(2)
			if err != nil {
				t.Fatalf("Long(2): %v", err)
			}
			if rs.WasNull() {
				t.Error("PRICE should not be null for inserted orders")
			}
			_ = price
			ids = append(ids, id)
		}
		if rs.Err() != nil {
			t.Fatalf("Err: %v", rs.Err())
		}
		if len(ids) < 2 {
			t.Fatalf("got %d rows, want >= 2", len(ids))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ResultSet_StringCoercion verifies String() works on
// int64 values from real FDB records.
func TestIntegration_ResultSet_StringCoercion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(3001), Price: proto.Int32(42)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		proj := mustExecutorConstruct(plans.NewRecordQueryProjectionPlan(
			[]values.Value{integrationField(t, scan, 2)},
			scan,
		))
		cursor, err := ExecutePlan(ctx, proj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}

		cols := []ColumnDef{
			{Name: "price", TypeName: "BIGINT"},
		}
		rs := NewRecordLayerResultSet(ctx, cursor, cols)
		defer rs.Close()

		if !rs.Next() {
			t.Fatal("expected a row")
		}

		s2, err := rs.String(1)
		if err != nil {
			t.Fatalf("String(1): %v", err)
		}
		if s2 != "42" {
			t.Errorf("String(1) = %q, want '42'", s2)
		}

		d, err := rs.Double(1)
		if err != nil {
			t.Fatalf("Double(1): %v", err)
		}
		if d != 42.0 {
			t.Errorf("Double(1) = %v, want 42.0", d)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ResultSet_FilterPipeline tests the full executor→ResultSet
// pipeline with a filter + sort + limit plan.
func TestIntegration_ResultSet_FilterPipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(4001), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(4002), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(4003), Price: proto.Int32(90)},
		&gen.Order{OrderId: proto.Int64(4004), Price: proto.Int32(30)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scan, 2),
					predicates.Comparison{
						Type:    predicates.ComparisonGreaterThanEq,
						Operand: values.LiteralValue(int64(30)),
					},
				),
			},
			scan,
		))
		sorted := mustExecutorConstruct(plans.NewRecordQueryInMemorySortPlan(
			filter,
			[]plans.SortKey{{Field: "price", ValueExpr: integrationField(t, scan, 2), Desc: false}},
		))
		// Project PRICE, ORDER_ID so the output row is ordinal-aligned to the columns.
		proj := mustExecutorConstruct(plans.NewRecordQueryProjectionPlan(
			[]values.Value{integrationField(t, scan, 2), integrationField(t, scan, 0)},
			sorted,
		))

		cursor, err := ExecutePlan(ctx, proj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}

		cols := []ColumnDef{
			{Name: "price", TypeName: "BIGINT"},
			{Name: "order_id", TypeName: "BIGINT"},
		}
		rs := NewRecordLayerResultSet(ctx, cursor, cols)
		defer rs.Close()

		var prices []int64
		for rs.Next() {
			p, err := rs.Long(1)
			if err != nil {
				t.Fatalf("Long(1): %v", err)
			}
			prices = append(prices, p)
		}
		if rs.Err() != nil {
			t.Fatalf("Err: %v", rs.Err())
		}
		if len(prices) != 3 {
			t.Fatalf("got %d rows, want 3 (prices >= 30)", len(prices))
		}
		for i := 1; i < len(prices); i++ {
			if prices[i] < prices[i-1] {
				t.Errorf("prices not ascending: %v", prices)
				break
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ResultSet_ByName verifies column-by-name access with
// real FDB data.
func TestIntegration_ResultSet_ByName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(5001), Price: proto.Int32(99)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		proj := mustExecutorConstruct(plans.NewRecordQueryProjectionPlan(
			[]values.Value{integrationField(t, scan, 0), integrationField(t, scan, 2)},
			scan,
		))
		cursor, err := ExecutePlan(ctx, proj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}

		cols := []ColumnDef{
			{Name: "order_id", TypeName: "BIGINT"},
			{Name: "price", TypeName: "BIGINT"},
		}
		rs := NewRecordLayerResultSet(ctx, cursor, cols)
		defer rs.Close()

		if !rs.Next() {
			t.Fatal("expected a row")
		}

		id, err := rs.LongByName("order_id")
		if err != nil {
			t.Fatalf("LongByName ORDER_ID: %v", err)
		}
		if id != 5001 {
			t.Errorf("ORDER_ID = %d, want 5001", id)
		}

		price, err := rs.LongByName("price")
		if err != nil {
			t.Fatalf("LongByName PRICE: %v", err)
		}
		if price != 99 {
			t.Errorf("PRICE = %d, want 99", price)
		}

		_, err = rs.LongByName("NONEXISTENT")
		if err == nil {
			t.Fatal("expected error for nonexistent column")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func insertCustomers(t *testing.T, store *recordlayer.FDBRecordStore, customers ...*gen.Customer) {
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

// TestIntegration_ProjectionPlan tests projecting specific columns from scan results.
func TestIntegration_ProjectionPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(6001), Price: proto.Int32(111)},
		&gen.Order{OrderId: proto.Int64(6002), Price: proto.Int32(222)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		proj := mustExecutorConstruct(plans.NewRecordQueryProjectionPlan(
			[]values.Value{
				integrationField(t, scan, 2),
			},
			scan,
		))

		cursor, err := ExecutePlan(ctx, proj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("projection returned %d results, want 2", len(results))
		}

		for _, r := range results {
			datum, _ := rowMapOK(r)
			if _, exists := datum["price"]; !exists {
				t.Error("projected datum should contain PRICE")
			}
			if _, exists := datum["order_id"]; exists {
				t.Error("projected datum should NOT contain ORDER_ID")
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ProjectionPlan_MultiColumn tests multi-column projection.
func TestIntegration_ProjectionPlan_MultiColumn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(6101), Price: proto.Int32(50)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		proj := mustExecutorConstruct(plans.NewRecordQueryProjectionPlan(
			[]values.Value{
				integrationField(t, scan, 0),
				integrationField(t, scan, 2),
			},
			scan,
		))

		cursor, err := ExecutePlan(ctx, proj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("got %d results, want 1", len(results))
		}

		datum, _ := rowMapOK(results[0])
		if datum["order_id"] != int64(6101) {
			t.Errorf("ORDER_ID = %v, want 6101", datum["order_id"])
		}
		if datum["price"] != int64(50) {
			t.Errorf("PRICE = %v, want 50", datum["price"])
		}
		if len(datum) != 2 {
			t.Errorf("datum has %d keys, want exactly 2 (ORDER_ID, PRICE)", len(datum))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_DistinctPlan tests deduplication by primary key.
func TestIntegration_DistinctPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(7001), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(7002), Price: proto.Int32(200)},
		&gen.Order{OrderId: proto.Int64(7003), Price: proto.Int32(300)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		distinct := mustExecutorConstruct(plans.NewRecordQueryDistinctPlan(scan))

		cursor, err := ExecutePlan(ctx, distinct, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("distinct returned %d results, want 3 (all unique PKs)", len(results))
		}

		seen := make(map[string]struct{})
		for _, r := range results {
			key := string(r.PrimaryKey.Pack())
			if _, dup := seen[key]; dup {
				t.Errorf("duplicate PK in distinct results: %v", r.PrimaryKey)
			}
			seen[key] = struct{}{}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ParameterBinding_Filter tests filter with prepared-statement
// parameter against real FDB records.
func TestIntegration_ParameterBinding_Filter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(8001), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(8002), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(8003), Price: proto.Int32(90)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scan, 2),
					predicates.Comparison{
						Type:    predicates.ComparisonGreaterThan,
						Operand: values.NewParameterValue(1),
					},
				),
			},
			scan,
		))

		evalCtx := EmptyEvaluationContext().WithParams([]any{int64(40)})
		cursor, err := ExecutePlan(ctx, filter, s, evalCtx, nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("parameter filter returned %d results, want 2 (price > 40)", len(results))
		}
		for _, r := range results {
			price, _ := rowMap(r)["price"].(int64)
			if price <= 40 {
				t.Errorf("price=%d should be > 40", price)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ParameterBinding_IndexScan tests index scan with a parameter
// in the comparison range.
func TestIntegration_ParameterBinding_IndexScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(8101), Price: proto.Int32(25)},
		&gen.Order{OrderId: proto.Int64(8102), Price: proto.Int32(75)},
		&gen.Order{OrderId: proto.Int64(8103), Price: proto.Int32(125)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		cr := predicates.EmptyComparisonRange()
		res := cr.Merge(&predicates.Comparison{
			Type:    predicates.ComparisonGreaterThanEq,
			Operand: values.NewParameterValue(1),
		})
		if !res.Ok {
			t.Fatal("merge failed")
		}

		indexPlan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			[]*predicates.ComparisonRange{res.Range},
			[]string{"Order"},
			integrationOrderType(),
			false,
		)).WithKeyComponentTypes([]values.Type{values.NullableInt})

		evalCtx := EmptyEvaluationContext().WithParams([]any{int64(50)})
		cursor, err := ExecutePlan(ctx, indexPlan, s, evalCtx, nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("param index scan returned %d results, want 2 (price >= 50)", len(results))
		}
		for _, r := range results {
			price, _ := rowMap(r)["price"].(int64)
			if price < 50 {
				t.Errorf("price=%d, should be >= 50", price)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_NestedLoopJoin_CrossJoin tests a cross join between
// Orders and Customers (no join predicate).
func TestIntegration_NestedLoopJoin_CrossJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(9001), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(9002), Price: proto.Int32(20)},
	)
	insertCustomers(t, store,
		&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Alice")},
		&gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("Bob")},
		&gen.Customer{CustomerId: proto.Int64(3), Name: proto.String("Carol")},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		outerScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		innerScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Customer"}, integrationCustomerType(), false))
		outerAlias := values.NamedCorrelationIdentifier("ORDER")
		innerAlias := values.NamedCorrelationIdentifier("CUSTOMER")
		nlj := mustExecutorConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
			outerScan, innerScan,
			nil,
			plans.JoinInner,
			outerAlias, innerAlias,
			integrationJoinResult(t, outerScan, innerScan, outerAlias, innerAlias, plans.JoinInner),
		))

		cursor, err := ExecutePlan(ctx, nlj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 6 {
			t.Fatalf("cross join returned %d results, want 6 (2 orders × 3 customers)", len(results))
		}

		for _, r := range results {
			datum, _ := rowMapOK(r)
			if datum["order_id"] == nil {
				t.Error("ORDER_ID missing from joined row")
			}
			if datum["customer_id"] == nil {
				t.Error("CUSTOMER_ID missing from joined row")
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_NestedLoopJoin_WithPredicate tests NLJ with a filter predicate
// on the outer table's PRICE column.
func TestIntegration_NestedLoopJoin_WithPredicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(9101), Price: proto.Int32(100), Quantity: proto.Int32(5)},
		&gen.Order{OrderId: proto.Int64(9102), Price: proto.Int32(200), Quantity: proto.Int32(10)},
	)
	insertCustomers(t, store,
		&gen.Customer{CustomerId: proto.Int64(19101), Name: proto.String("Dan")},
		&gen.Customer{CustomerId: proto.Int64(19102), Name: proto.String("Eve")},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		outerScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		innerScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Customer"}, integrationCustomerType(), false))
		outerAlias := values.NamedCorrelationIdentifier("ORDER")
		innerAlias := values.NamedCorrelationIdentifier("CUSTOMER")

		nlj := mustExecutorConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
			outerScan, innerScan,
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, outerScan, 4),
					predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: values.LiteralValue(int64(5)),
					},
				),
			},
			plans.JoinInner,
			outerAlias, innerAlias,
			integrationJoinResult(t, outerScan, innerScan, outerAlias, innerAlias, plans.JoinInner),
		))

		cursor, err := ExecutePlan(ctx, nlj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("predicate join returned %d results, want 2 (quantity=5 order × 2 customers)", len(results))
		}
		for _, r := range results {
			datum, _ := rowMapOK(r)
			if datum["order_id"] != int64(9101) {
				t.Errorf("ORDER_ID = %v, want 9101 (quantity=5)", datum["order_id"])
			}
			if datum["customer_id"] == nil {
				t.Error("CUSTOMER_ID missing from joined row")
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_NestedLoopJoin_LeftOuter tests left outer join — unmatched
// outer rows are preserved.
func TestIntegration_NestedLoopJoin_LeftOuter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(9201), Price: proto.Int32(100), Quantity: proto.Int32(5)},
		&gen.Order{OrderId: proto.Int64(9202), Price: proto.Int32(200), Quantity: proto.Int32(10)},
	)
	insertCustomers(t, store,
		&gen.Customer{CustomerId: proto.Int64(19201), Name: proto.String("Frank")},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		outerScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		innerScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Customer"}, integrationCustomerType(), false))
		outerAlias := values.NamedCorrelationIdentifier("ORDER")
		innerAlias := values.NamedCorrelationIdentifier("CUSTOMER")

		nlj := mustExecutorConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
			outerScan, innerScan,
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, outerScan, 4),
					predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: values.LiteralValue(int64(5)),
					},
				),
			},
			plans.JoinLeftOuter,
			outerAlias, innerAlias,
			integrationJoinResult(t, outerScan, innerScan, outerAlias, innerAlias, plans.JoinLeftOuter),
		))

		cursor, err := ExecutePlan(ctx, nlj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("left outer join returned %d results, want 2 (1 matched + 1 unmatched)", len(results))
		}

		matchedFound := false
		unmatchedFound := false
		for _, r := range results {
			datum, _ := rowMapOK(r)
			orderID := datum["order_id"].(int64)
			if orderID == 9201 {
				if datum["customer_id"] == nil {
					t.Error("matched row should have CUSTOMER_ID from inner")
				}
				matchedFound = true
			} else if orderID == 9202 {
				unmatchedFound = true
			}
		}
		if !matchedFound {
			t.Error("expected matched row (order 9201 quantity=5)")
		}
		if !unmatchedFound {
			t.Error("expected unmatched outer row (order 9202 quantity=10)")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_UpdatePlan_WithParameter tests UPDATE with a parameterized
// SET value against real FDB.
func TestIntegration_UpdatePlan_WithParameter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(8201), Price: proto.Int32(100)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scan, 0),
					predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: values.LiteralValue(int64(8201)),
					},
				),
			},
			scan,
		))
		update := mustExecutorConstruct(plans.NewRecordQueryUpdatePlan(filter, "Order", []expressions.UpdateTransform{
			{FieldPath: "price", NewValue: values.NewParameterValue(1)},
		}))

		evalCtx := EmptyEvaluationContext().WithParams([]any{int64(777)})
		cursor, err := ExecutePlan(ctx, update, s, evalCtx, nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("update returned %d results, want 1", len(results))
		}

		rec, err := s.LoadRecord(tuple.Tuple{int64(8201)})
		if err != nil {
			t.Fatalf("LoadRecord: %v", err)
		}
		if rec == nil {
			t.Fatal("record not found after update")
		}
		updated := rec.Record.(*gen.Order)
		if updated.GetPrice() != 777 {
			t.Errorf("price after param update = %d, want 777", updated.GetPrice())
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_UnionPlan tests UNION ALL of two type-compatible scans
// against real FDB. A union of Order and Customer used to construct only
// because both inputs hid behind UnknownType; exact set-plan admission now
// correctly rejects that malformed shape before execution.
func TestIntegration_UnionPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(10001), Price: proto.Int32(100)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		firstScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		secondScan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		union := mustExecutorConstruct(plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{firstScan, secondScan}))

		cursor, err := ExecutePlan(ctx, union, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("union returned %d results, want 2 (the one Order from each UNION ALL arm)", len(results))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_IntersectionPlan tests N-way intersection using PK overlap.
func TestIntegration_IntersectionPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(11001), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(11002), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(11003), Price: proto.Int32(90)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan1 := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		scan2 := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		intersection := mustExecutorConstruct(plans.NewRecordQueryIntersectionPlan(
			[]plans.RecordQueryPlan{scan1, scan2},
			nil,
		))

		cursor, err := ExecutePlan(ctx, intersection, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("intersection returned %d results, want 3 (same scan twice → all 3 overlap)", len(results))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_StreamingAggregation_CountAndSum tests streaming aggregation with
// COUNT and SUM against real FDB data.
func TestIntegration_StreamingAggregation_CountAndSum(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(12001), Price: proto.Int32(100), Quantity: proto.Int32(2)},
		&gen.Order{OrderId: proto.Int64(12002), Price: proto.Int32(100), Quantity: proto.Int32(3)},
		&gen.Order{OrderId: proto.Int64(12003), Price: proto.Int32(200), Quantity: proto.Int32(1)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		agg := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			[]values.Value{integrationField(t, scan, 2)},
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount, Operand: integrationField(t, scan, 0), OperandName: "ORDER_ID"},
				{Function: expressions.AggSum, Operand: integrationField(t, scan, 4), OperandName: "QUANTITY"},
			},
		))

		cursor, err := ExecutePlan(ctx, agg, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("aggregation returned %d groups, want 2 (price=100, price=200)", len(results))
		}

		for _, r := range results {
			datum, _ := rowMapOK(r)
			price := datum["price"].(int64)
			count := datum["COUNT(ORDER_ID)"].(int64)
			sumQty := datum["SUM(QUANTITY)"].(int64)

			switch price {
			case 100:
				if count != 2 {
					t.Errorf("price=100 count=%d, want 2", count)
				}
				if sumQty != 5 {
					t.Errorf("price=100 sum(qty)=%v, want 5", sumQty)
				}
			case 200:
				if count != 1 {
					t.Errorf("price=200 count=%d, want 1", count)
				}
				if sumQty != 1 {
					t.Errorf("price=200 sum(qty)=%v, want 1", sumQty)
				}
			default:
				t.Errorf("unexpected price group: %d", price)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_Aggregation_NoGroupBy tests COUNT without GROUP BY.
func TestIntegration_Aggregation_NoGroupBy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(13001), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(13002), Price: proto.Int32(20)},
		&gen.Order{OrderId: proto.Int64(13003), Price: proto.Int32(30)},
		&gen.Order{OrderId: proto.Int64(13004), Price: proto.Int32(40)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		agg := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			nil,
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount, Operand: integrationField(t, scan, 0), OperandName: "ORDER_ID"},
				{Function: expressions.AggMin, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
				{Function: expressions.AggMax, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
			},
		))

		cursor, err := ExecutePlan(ctx, agg, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("no-group agg returned %d results, want 1", len(results))
		}

		datum, _ := rowMapOK(results[0])
		if datum["COUNT(ORDER_ID)"] != int64(4) {
			t.Errorf("COUNT = %v, want 4", datum["COUNT(ORDER_ID)"])
		}
		if datum["MIN(PRICE)"] != int64(10) {
			t.Errorf("MIN = %v, want 10", datum["MIN(PRICE)"])
		}
		if datum["MAX(PRICE)"] != int64(40) {
			t.Errorf("MAX = %v, want 40", datum["MAX(PRICE)"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_IndexScan_Reverse tests reverse-order index scan.
func TestIntegration_IndexScan_Reverse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(15001), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(15002), Price: proto.Int32(200)},
		&gen.Order{OrderId: proto.Int64(15003), Price: proto.Int32(300)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		indexPlan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			nil,
			[]string{"Order"},
			integrationOrderType(),
			true, // reverse
		)).WithKeyComponentTypes([]values.Type{values.NullableInt})

		cursor, err := ExecutePlan(ctx, indexPlan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("reverse index scan returned %d results, want 3", len(results))
		}

		prices := make([]int64, len(results))
		for i, r := range results {
			prices[i] = rowMap(r)["price"].(int64)
		}
		if prices[0] != 300 || prices[1] != 200 || prices[2] != 100 {
			t.Errorf("reverse scan prices = %v, want [300 200 100]", prices)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_LimitWithOffset tests LIMIT with OFFSET > 0.
func TestIntegration_LimitWithOffset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(15101), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(15102), Price: proto.Int32(20)},
		&gen.Order{OrderId: proto.Int64(15103), Price: proto.Int32(30)},
		&gen.Order{OrderId: proto.Int64(15104), Price: proto.Int32(40)},
		&gen.Order{OrderId: proto.Int64(15105), Price: proto.Int32(50)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		sorted := mustExecutorConstruct(plans.NewRecordQueryInMemorySortPlan(
			scan,
			[]plans.SortKey{{Field: "price", ValueExpr: integrationField(t, scan, 2), Desc: false}},
		))
		// OFFSET 2, LIMIT 2 — skip first 2 (price=10,20), take next 2 (price=30,40)
		limited := mustExecutorConstruct(plans.NewRecordQueryLimitPlan(sorted, 2, 2))

		cursor, err := ExecutePlan(ctx, limited, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("limit+offset returned %d results, want 2", len(results))
		}

		d0, _ := rowMapOK(results[0])
		d1, _ := rowMapOK(results[1])
		if d0["price"] != int64(30) {
			t.Errorf("first result price = %v, want 30", d0["price"])
		}
		if d1["price"] != int64(40) {
			t.Errorf("second result price = %v, want 40", d1["price"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_Aggregation_MinMaxAvg tests MIN, MAX, and AVG aggregate functions.
func TestIntegration_Aggregation_MinMaxAvg(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(15201), Price: proto.Int32(100), Quantity: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(15202), Price: proto.Int32(200), Quantity: proto.Int32(20)},
		&gen.Order{OrderId: proto.Int64(15203), Price: proto.Int32(300), Quantity: proto.Int32(30)},
		&gen.Order{OrderId: proto.Int64(15204), Price: proto.Int32(400), Quantity: proto.Int32(40)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		agg := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			nil, // no grouping keys — aggregate over all
			[]expressions.AggregateSpec{
				{Function: expressions.AggMin, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
				{Function: expressions.AggMax, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
				{Function: expressions.AggAvg, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
			},
		))

		cursor, err := ExecutePlan(ctx, agg, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("aggregation returned %d groups, want 1", len(results))
		}

		d, _ := rowMapOK(results[0])
		if d["MIN(PRICE)"] != int64(100) {
			t.Errorf("MIN(PRICE) = %v, want 100", d["MIN(PRICE)"])
		}
		if d["MAX(PRICE)"] != int64(400) {
			t.Errorf("MAX(PRICE) = %v, want 400", d["MAX(PRICE)"])
		}
		avg, ok := d["AVG(PRICE)"].(float64)
		if !ok || avg != 250.0 {
			t.Errorf("AVG(PRICE) = %v, want 250.0", d["AVG(PRICE)"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_DeletePlan_WithFilter tests DELETE with a filter predicate.
func TestIntegration_DeletePlan_WithFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(15301), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(15302), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(15303), Price: proto.Int32(150)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scan, 2),
					predicates.Comparison{
						Type:    predicates.ComparisonGreaterThan,
						Operand: values.LiteralValue(int64(75)),
					},
				),
			},
			scan,
		))
		del := mustExecutorConstruct(plans.NewRecordQueryDeletePlan(filter, "Order"))

		cursor, err := ExecutePlan(ctx, del, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		deleted, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(deleted) != 2 {
			t.Fatalf("delete returned %d results, want 2 (price > 75)", len(deleted))
		}

		// Verify only the low-price order remains.
		scanAll := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		cursor2, err := ExecutePlan(ctx, scanAll, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("verify scan: %v", err)
		}
		defer cursor2.Close()
		remaining, err := CollectAll(ctx, cursor2)
		if err != nil {
			t.Fatalf("verify CollectAll: %v", err)
		}
		if len(remaining) != 1 {
			t.Fatalf("remaining = %d, want 1", len(remaining))
		}
		price, _ := rowMap(remaining[0])["price"].(int64)
		if price != 50 {
			t.Errorf("remaining order price = %d, want 50", price)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ParameterBinding_Delete tests DELETE WHERE with a parameterized predicate.
func TestIntegration_ParameterBinding_Delete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(15401), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(15402), Price: proto.Int32(20)},
		&gen.Order{OrderId: proto.Int64(15403), Price: proto.Int32(30)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scan, 0),
					predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: &values.ParameterValue{Ordinal: 1},
					},
				),
			},
			scan,
		))
		del := mustExecutorConstruct(plans.NewRecordQueryDeletePlan(filter, "Order"))

		evalCtx := EmptyEvaluationContext().WithParams([]any{int64(15402)})
		cursor, err := ExecutePlan(ctx, del, s, evalCtx, nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		deleted, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(deleted) != 1 {
			t.Fatalf("delete returned %d results, want 1", len(deleted))
		}

		// Verify 2 orders remain.
		scanAll := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		cursor2, err := ExecutePlan(ctx, scanAll, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("verify scan: %v", err)
		}
		defer cursor2.Close()
		remaining, err := CollectAll(ctx, cursor2)
		if err != nil {
			t.Fatalf("verify CollectAll: %v", err)
		}
		if len(remaining) != 2 {
			t.Fatalf("remaining = %d, want 2", len(remaining))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_Aggregation_GroupBy_MultiFunc tests grouped aggregation with multiple functions.
func TestIntegration_Aggregation_GroupBy_MultiFunc(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(15501), Price: proto.Int32(100), Quantity: proto.Int32(1)},
		&gen.Order{OrderId: proto.Int64(15502), Price: proto.Int32(100), Quantity: proto.Int32(2)},
		&gen.Order{OrderId: proto.Int64(15503), Price: proto.Int32(200), Quantity: proto.Int32(3)},
		&gen.Order{OrderId: proto.Int64(15504), Price: proto.Int32(200), Quantity: proto.Int32(7)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		agg := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			[]values.Value{integrationField(t, scan, 2)},
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount, Operand: integrationField(t, scan, 0), OperandName: "ORDER_ID"},
				{Function: expressions.AggSum, Operand: integrationField(t, scan, 4), OperandName: "QUANTITY"},
				{Function: expressions.AggMin, Operand: integrationField(t, scan, 4), OperandName: "QUANTITY"},
				{Function: expressions.AggMax, Operand: integrationField(t, scan, 4), OperandName: "QUANTITY"},
			},
		))

		cursor, err := ExecutePlan(ctx, agg, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("grouped agg returned %d groups, want 2", len(results))
		}

		byPrice := make(map[int64]map[string]any)
		for _, r := range results {
			d, _ := rowMapOK(r)
			p := d["price"].(int64)
			byPrice[p] = d
		}

		g100 := byPrice[100]
		if g100 == nil {
			t.Fatal("no group for PRICE=100")
		}
		if g100["COUNT(ORDER_ID)"] != int64(2) {
			t.Errorf("COUNT(ORDER_ID) for price=100: %v, want 2", g100["COUNT(ORDER_ID)"])
		}
		if g100["SUM(QUANTITY)"] != int64(3) {
			t.Errorf("SUM(QUANTITY) for price=100: %v, want 3", g100["SUM(QUANTITY)"])
		}
		if g100["MIN(QUANTITY)"] != int64(1) {
			t.Errorf("MIN(QUANTITY) for price=100: %v, want 1", g100["MIN(QUANTITY)"])
		}
		if g100["MAX(QUANTITY)"] != int64(2) {
			t.Errorf("MAX(QUANTITY) for price=100: %v, want 2", g100["MAX(QUANTITY)"])
		}

		g200 := byPrice[200]
		if g200 == nil {
			t.Fatal("no group for PRICE=200")
		}
		if g200["COUNT(ORDER_ID)"] != int64(2) {
			t.Errorf("COUNT(ORDER_ID) for price=200: %v, want 2", g200["COUNT(ORDER_ID)"])
		}
		if g200["SUM(QUANTITY)"] != int64(10) {
			t.Errorf("SUM(QUANTITY) for price=200: %v, want 10", g200["SUM(QUANTITY)"])
		}
		if g200["MIN(QUANTITY)"] != int64(3) {
			t.Errorf("MIN(QUANTITY) for price=200: %v, want 3", g200["MIN(QUANTITY)"])
		}
		if g200["MAX(QUANTITY)"] != int64(7) {
			t.Errorf("MAX(QUANTITY) for price=200: %v, want 7", g200["MAX(QUANTITY)"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_FilterSortProjection_Pipeline tests the full pipeline:
// scan → filter → sort → project → limit (all plan types chained).
func TestIntegration_FilterSortProjection_Pipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(14001), Price: proto.Int32(500), Quantity: proto.Int32(5)},
		&gen.Order{OrderId: proto.Int64(14002), Price: proto.Int32(300), Quantity: proto.Int32(3)},
		&gen.Order{OrderId: proto.Int64(14003), Price: proto.Int32(100), Quantity: proto.Int32(1)},
		&gen.Order{OrderId: proto.Int64(14004), Price: proto.Int32(400), Quantity: proto.Int32(4)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scan, 2),
					predicates.Comparison{
						Type:    predicates.ComparisonGreaterThan,
						Operand: values.LiteralValue(int64(200)),
					},
				),
			},
			scan,
		))
		sorted := mustExecutorConstruct(plans.NewRecordQueryInMemorySortPlan(
			filter,
			[]plans.SortKey{{Field: "price", ValueExpr: integrationField(t, scan, 2), Desc: false}},
		))
		proj := mustExecutorConstruct(plans.NewRecordQueryProjectionPlan(
			[]values.Value{
				integrationField(t, scan, 2),
				integrationField(t, scan, 4),
			},
			sorted,
		))
		limited := mustExecutorConstruct(plans.NewRecordQueryLimitPlan(proj, 2, 0))

		cursor, err := ExecutePlan(ctx, limited, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("pipeline returned %d results, want 2", len(results))
		}

		d0, _ := rowMapOK(results[0])
		d1, _ := rowMapOK(results[1])
		if d0["price"] != int64(300) || d0["quantity"] != int64(3) {
			t.Errorf("first row = %v, want PRICE=300/QUANTITY=3", d0)
		}
		if d1["price"] != int64(400) || d1["quantity"] != int64(4) {
			t.Errorf("second row = %v, want PRICE=400/QUANTITY=4", d1)
		}
		if _, exists := d0["order_id"]; exists {
			t.Error("ORDER_ID should be excluded by projection")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_InsertPlan_DuplicateError mirrors Java's
// testInsertExistingRecordThrowsException: inserting a record
// that already exists must return RecordAlreadyExistsError.
func TestIntegration_InsertPlan_DuplicateError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store, &gen.Order{
		OrderId: proto.Int64(17101), Price: proto.Int32(42),
	})

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		ins := mustExecutorConstruct(plans.NewRecordQueryInsertPlan(scan, "Order", integrationOrderType()))

		_, err = ExecutePlan(ctx, ins, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err == nil {
			t.Fatal("expected error on duplicate insert, got nil")
		}
		var alreadyExists *recordlayer.RecordAlreadyExistsError
		if !errors.As(err, &alreadyExists) {
			t.Fatalf("expected RecordAlreadyExistsError, got %T: %v", err, err)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_InsertPlan_ValuesExplode proves the INSERT VALUES
// Cascades shape: RecordQueryInsertPlan over a RecordQueryExplodePlan
// of an array of literal RecordConstructorValues. The explode streams
// computed-row datums (no stored Record), and executeInsert's
// Datum→message bridge materializes each as a target-type record. This
// is the executor end of the INSERT VALUES path (RFC-035, Gap C).
func TestIntegration_InsertPlan_ValuesExplode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	// VALUES (7001, 42), (7002, 43) — order_id is int64, price is int32.
	// The int32 column is fed an int64 literal; goToProtoValue narrows it.
	mkRow := func(id, price int64) *values.RecordConstructorValue {
		return values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "order_id", Value: &values.ConstantValue{Value: id, Typ: values.NullableLong}},
			values.RecordConstructorField{Name: "price", Value: &values.ConstantValue{Value: price, Typ: values.NullableLong}},
		)
	}
	firstRow := mkRow(7001, 42)
	arr := values.NewArrayConstructorValue(firstRow.Type(), []values.Value{firstRow, mkRow(7002, 43)})
	explode := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(arr))
	ins := mustExecutorConstruct(plans.NewRecordQueryInsertPlan(explode, "Order", integrationOrderType()))
	probe, err := ExecutePlan(ctx, explode, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("execute VALUES explode: %v", err)
	}
	probeRows, err := CollectAll(ctx, probe)
	_ = probe.Close()
	if err != nil {
		t.Fatalf("collect VALUES explode: %v", err)
	}
	if len(probeRows) != 2 {
		t.Fatalf("VALUES explode emitted %d rows, want 2", len(probeRows))
	}
	for i, row := range probeRows {
		if row.Positional == nil || !row.Positional.Type.Equals(explode.GetResultType()) {
			t.Fatalf("VALUES explode row %d type = %v, want declared %v", i, row.Positional, explode.GetResultType())
		}
	}

	_, err = testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}
		cursor, err := ExecutePlan(ctx, ins, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			return nil, err
		}
		defer cursor.Close()
		inserted, err := CollectAll(ctx, cursor)
		if err != nil {
			return nil, err
		}
		if len(inserted) != 2 {
			t.Fatalf("INSERT VALUES emitted %d rows, want 2", len(inserted))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("execute INSERT VALUES: %v", err)
	}

	// Verify both rows persisted with correct values via a scan.
	_, err = testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}
		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		cursor, err := ExecutePlan(ctx, scan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			return nil, err
		}
		defer cursor.Close()
		rows, err := CollectAll(ctx, cursor)
		if err != nil {
			return nil, err
		}
		got := map[int64]int64{}
		for _, r := range rows {
			o, ok := r.Record.Record.(*gen.Order)
			if !ok {
				t.Fatalf("scanned record type = %T, want *gen.Order", r.Record.Record)
			}
			got[o.GetOrderId()] = int64(o.GetPrice())
		}
		if got[7001] != 42 || got[7002] != 43 {
			t.Fatalf("persisted rows = %v, want {7001:42, 7002:43}", got)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("verify scan: %v", err)
	}
}

// TestIntegration_Aggregation_EmptyInput tests aggregation over an
// empty result set: COUNT→0, SUM→0, MIN/MAX→nil.
func TestIntegration_Aggregation_EmptyInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		agg := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			nil,
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount, Operand: integrationField(t, scan, 0), OperandName: "ORDER_ID"},
				{Function: expressions.AggSum, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
				{Function: expressions.AggMin, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
				{Function: expressions.AggMax, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
			},
		))

		cursor, err := ExecutePlan(ctx, agg, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("aggregation over empty input returned %d rows, want 1", len(results))
		}
		d, _ := rowMapOK(results[0])
		if d["COUNT(ORDER_ID)"] != int64(0) {
			t.Errorf("COUNT(ORDER_ID) = %v, want 0", d["COUNT(ORDER_ID)"])
		}
		if d["SUM(PRICE)"] != nil {
			t.Errorf("SUM(PRICE) = %v, want nil (SQL NULL for empty set)", d["SUM(PRICE)"])
		}
		if d["MIN(PRICE)"] != nil {
			t.Errorf("MIN(PRICE) = %v, want nil", d["MIN(PRICE)"])
		}
		if d["MAX(PRICE)"] != nil {
			t.Errorf("MAX(PRICE) = %v, want nil", d["MAX(PRICE)"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_UpdatePlan_MultipleFields tests updating two
// fields in a single UPDATE plan.
func TestIntegration_UpdatePlan_MultipleFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(17201), Price: proto.Int32(100), Quantity: proto.Int32(5)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scan, 0),
					predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: values.LiteralValue(int64(17201)),
					},
				),
			},
			scan,
		))
		update := mustExecutorConstruct(plans.NewRecordQueryUpdatePlan(filter, "Order", []expressions.UpdateTransform{
			{FieldPath: "price", NewValue: values.LiteralValue(int64(999))},
			{FieldPath: "quantity", NewValue: values.LiteralValue(int64(42))},
		}))

		cursor, err := ExecutePlan(ctx, update, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("update returned %d results, want 1", len(results))
		}

		rec, err := s.LoadRecord(tuple.Tuple{int64(17201)})
		if err != nil {
			t.Fatalf("LoadRecord: %v", err)
		}
		if rec == nil {
			t.Fatal("record not found after update")
		}
		updated := rec.Record.(*gen.Order)
		if updated.GetPrice() != 999 {
			t.Errorf("price after update = %d, want 999", updated.GetPrice())
		}
		if updated.GetQuantity() != 42 {
			t.Errorf("quantity after update = %d, want 42", updated.GetQuantity())
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_IndexScan_EqualityRange tests an index scan with
// an exact equality comparison range.
func TestIntegration_IndexScan_EqualityRange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(17301), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(17302), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(17303), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(17304), Price: proto.Int32(200)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		eqRange := predicates.EmptyComparisonRange()
		comp := &predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.LiteralValue(int64(100)),
		}
		res := eqRange.Merge(comp)
		if !res.Ok {
			t.Fatal("merge failed")
		}

		idxScan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			[]*predicates.ComparisonRange{res.Range},
			[]string{"Order"},
			integrationOrderType(),
			false,
		)).WithKeyComponentTypes([]values.Type{values.NullableInt})

		cursor, err := ExecutePlan(ctx, idxScan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("equality index scan returned %d results, want 2", len(results))
		}
		for _, r := range results {
			d, _ := rowMapOK(r)
			if d["price"] != int64(100) {
				t.Errorf("PRICE = %v, want 100", d["price"])
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_SortPlan_MultiKey tests sorting by two fields:
// primary sort by PRICE ascending, secondary sort by QUANTITY descending.
func TestIntegration_SortPlan_MultiKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(17401), Price: proto.Int32(100), Quantity: proto.Int32(3)},
		&gen.Order{OrderId: proto.Int64(17402), Price: proto.Int32(200), Quantity: proto.Int32(1)},
		&gen.Order{OrderId: proto.Int64(17403), Price: proto.Int32(100), Quantity: proto.Int32(7)},
		&gen.Order{OrderId: proto.Int64(17404), Price: proto.Int32(200), Quantity: proto.Int32(9)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		sorted := mustExecutorConstruct(plans.NewRecordQueryInMemorySortPlan(
			scan,
			[]plans.SortKey{
				{Field: "price", ValueExpr: integrationField(t, scan, 2), Desc: false},
				{Field: "quantity", ValueExpr: integrationField(t, scan, 4), Desc: true},
			},
		))

		cursor, err := ExecutePlan(ctx, sorted, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 4 {
			t.Fatalf("sort returned %d results, want 4", len(results))
		}

		d0, _ := rowMapOK(results[0])
		d1, _ := rowMapOK(results[1])
		d2, _ := rowMapOK(results[2])
		d3, _ := rowMapOK(results[3])
		if d0["price"] != int64(100) || d0["quantity"] != int64(7) {
			t.Errorf("row 0: PRICE=%v QUANTITY=%v, want 100/7", d0["price"], d0["quantity"])
		}
		if d1["price"] != int64(100) || d1["quantity"] != int64(3) {
			t.Errorf("row 1: PRICE=%v QUANTITY=%v, want 100/3", d1["price"], d1["quantity"])
		}
		if d2["price"] != int64(200) || d2["quantity"] != int64(9) {
			t.Errorf("row 2: PRICE=%v QUANTITY=%v, want 200/9", d2["price"], d2["quantity"])
		}
		if d3["price"] != int64(200) || d3["quantity"] != int64(1) {
			t.Errorf("row 3: PRICE=%v QUANTITY=%v, want 200/1", d3["price"], d3["quantity"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_UnionPlan_DisjointLegs tests UNION of two
// non-overlapping filter legs — output is the bag union.
func TestIntegration_UnionPlan_DisjointLegs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(17501), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(17502), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(17503), Price: proto.Int32(150)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scanA := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filterLow := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scanA, 2),
					predicates.Comparison{
						Type:    predicates.ComparisonLessThan,
						Operand: values.LiteralValue(int64(100)),
					},
				),
			},
			scanA,
		))

		scanB := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filterHigh := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scanB, 2),
					predicates.Comparison{
						Type:    predicates.ComparisonGreaterThan,
						Operand: values.LiteralValue(int64(100)),
					},
				),
			},
			scanB,
		))

		union := mustExecutorConstruct(plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{filterLow, filterHigh}))

		cursor, err := ExecutePlan(ctx, union, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("union returned %d results, want 2 (disjoint legs)", len(results))
		}

		prices := map[int64]bool{}
		for _, r := range results {
			d, _ := rowMapOK(r)
			prices[d["price"].(int64)] = true
		}
		if !prices[50] {
			t.Error("expected PRICE=50 in union output")
		}
		if !prices[150] {
			t.Error("expected PRICE=150 in union output")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_FilterPlan_NoMatch tests that filtering with no
// matching records returns an empty cursor.
func TestIntegration_FilterPlan_NoMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(17601), Price: proto.Int32(10)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scan, 2),
					predicates.Comparison{
						Type:    predicates.ComparisonEquals,
						Operand: values.LiteralValue(int64(99999)),
					},
				),
			},
			scan,
		))

		cursor, err := ExecutePlan(ctx, filter, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("filter with no match returned %d results, want 0", len(results))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_DeletePlan_AllRecords tests deleting all records
// via an unfiltered scan→delete pipeline.
func TestIntegration_DeletePlan_AllRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(17701), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(17702), Price: proto.Int32(20)},
		&gen.Order{OrderId: proto.Int64(17703), Price: proto.Int32(30)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		del := mustExecutorConstruct(plans.NewRecordQueryDeletePlan(scan, "Order"))

		cursor, err := ExecutePlan(ctx, del, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("delete returned %d results, want 3", len(results))
		}

		for _, pk := range []int64{17701, 17702, 17703} {
			rec, err := s.LoadRecord(tuple.Tuple{pk})
			if err != nil {
				t.Fatalf("LoadRecord(%d): %v", pk, err)
			}
			if rec != nil {
				t.Errorf("record %d still exists after delete", pk)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_TypeFilter_MixedRecordTypes stores both Order and
// TypedRecord in the same subspace and verifies that TypeFilter
// correctly separates them during scan.
func TestIntegration_TypeFilter_MixedRecordTypes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		if _, err := s.SaveRecord(&gen.Order{OrderId: proto.Int64(18001), Price: proto.Int32(100)}); err != nil {
			return nil, err
		}
		if _, err := s.SaveRecord(&gen.Order{OrderId: proto.Int64(18002), Price: proto.Int32(200)}); err != nil {
			return nil, err
		}
		if _, err := s.SaveRecord(&gen.TypedRecord{Id: proto.Int64(28001), ValString: proto.String("hello")}); err != nil {
			return nil, err
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("insert mixed records: %v", err)
	}

	_, err = testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		orderRecordType := s.GetMetaData().GetRecordType("Order")
		if orderRecordType == nil || orderRecordType.Descriptor == nil {
			return nil, fmt.Errorf("missing Order record descriptor")
		}
		orderRowType := PositionalTypeForRecordLayout(
			orderRecordType.Descriptor, s.GetMetaData().IsStoreRecordVersions())
		// Deliberately scan the shared multi-type stream while publishing the
		// exact row contract the enclosing Order TypeFilter guarantees. The raw
		// scan encounters TypedRecord rows too; they must be discarded before the
		// Order layout is attached, while surviving Order rows remain exact.
		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan(
			[]string{"Order", "TypedRecord"}, orderRowType, false,
		))
		typeFilter := mustExecutorConstruct(plans.NewRecordQueryTypeFilterPlan([]string{"Order"}, scan))

		cursor, err := ExecutePlan(ctx, typeFilter, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("TypeFilter(Order) returned %d results, want 2", len(results))
		}
		for _, r := range results {
			d, _ := rowMapOK(r)
			if _, ok := d["order_id"]; !ok {
				t.Errorf("expected ORDER_ID in type-filtered result, got %v", d)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_ScanPlan_UnsetFieldsOmitted verifies that proto
// fields not set on a record are omitted from the datum map
// (protoToMap only includes set fields).
func TestIntegration_ScanPlan_UnsetFieldsOmitted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(18101), Price: proto.Int32(50)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))

		cursor, err := ExecutePlan(ctx, scan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("scan returned %d results, want 1", len(results))
		}

		d, _ := rowMapOK(results[0])
		if d["order_id"] != int64(18101) {
			t.Errorf("ORDER_ID = %v, want 18101", d["order_id"])
		}
		if d["price"] != int64(50) {
			t.Errorf("PRICE = %v, want 50", d["price"])
		}
		// An unset field is a present-nil slot (SQL NULL), not key-absent.
		if d["quantity"] != nil {
			t.Errorf("QUANTITY (unset) = %v, want nil (SQL NULL)", d["quantity"])
		}
		if d["customer_id"] != nil {
			t.Errorf("CUSTOMER_ID (unset) = %v, want nil (SQL NULL)", d["customer_id"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_FilterPlan_IsNull tests filtering for records where
// an optional field IS NULL (not set). A field missing from the datum
// map evaluates to nil; equality comparison with nil should not match.
func TestIntegration_FilterPlan_IsNull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(18201), Price: proto.Int32(100), Quantity: proto.Int32(5)},
		&gen.Order{OrderId: proto.Int64(18202), Price: proto.Int32(200)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					integrationField(t, scan, 4),
					predicates.Comparison{Type: predicates.ComparisonIsNull},
				),
			},
			scan,
		))

		cursor, err := ExecutePlan(ctx, filter, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("IS NULL filter returned %d results, want 1", len(results))
		}
		d, _ := rowMapOK(results[0])
		if d["order_id"] != int64(18202) {
			t.Errorf("ORDER_ID = %v, want 18202", d["order_id"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_Aggregation_AVG verifies AVG computation including
// floating-point result.
func TestIntegration_Aggregation_AVG(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(18301), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(18302), Price: proto.Int32(20)},
		&gen.Order{OrderId: proto.Int64(18303), Price: proto.Int32(30)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		agg := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(
			scan,
			nil,
			[]expressions.AggregateSpec{
				{Function: expressions.AggAvg, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
			},
		))

		cursor, err := ExecutePlan(ctx, agg, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("aggregation returned %d rows, want 1", len(results))
		}
		d, _ := rowMapOK(results[0])
		avg, ok := d["AVG(PRICE)"].(float64)
		if !ok {
			t.Fatalf("AVG(PRICE) type = %T, want float64", d["AVG(PRICE)"])
		}
		if avg != 20.0 {
			t.Errorf("AVG(PRICE) = %v, want 20.0", avg)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_StreamingAggregation_SortedInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(19001), Quantity: proto.Int32(1), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(19002), Quantity: proto.Int32(1), Price: proto.Int32(200)},
		&gen.Order{OrderId: proto.Int64(19003), Quantity: proto.Int32(2), Price: proto.Int32(300)},
		&gen.Order{OrderId: proto.Int64(19004), Quantity: proto.Int32(2), Price: proto.Int32(400)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		sort := mustExecutorConstruct(plans.NewRecordQueryInMemorySortPlan(scan, []plans.SortKey{
			{Field: "quantity", ValueExpr: integrationField(t, scan, 4), Desc: false},
		}))
		agg := mustExecutorConstruct(plans.NewRecordQueryStreamingAggregationPlan(
			sort,
			[]values.Value{integrationField(t, scan, 4)},
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: int64(1), Typ: values.NullableLong}, OperandName: "CONSTANT"},
				{Function: expressions.AggSum, Operand: integrationField(t, scan, 2), OperandName: "PRICE"},
			},
		))

		cursor, err := ExecutePlan(ctx, agg, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("streaming agg returned %d rows, want 2", len(results))
		}

		qtyCounts := map[int64]int64{}
		qtySums := map[int64]int64{}
		for _, r := range results {
			d, _ := rowMapOK(r)
			qty, ok := d["quantity"].(int64)
			if !ok {
				t.Fatalf("QUANTITY type = %T (value = %v), datum keys = %v", d["quantity"], d["quantity"], d)
			}
			cnt := d["COUNT(CONSTANT)"].(int64)
			sum := d["SUM(PRICE)"].(int64)
			qtyCounts[qty] = cnt
			qtySums[qty] = sum
		}
		if qtyCounts[1] != 2 || qtyCounts[2] != 2 {
			t.Errorf("counts: %v", qtyCounts)
		}
		if qtySums[1] != 300.0 || qtySums[2] != 700.0 {
			t.Errorf("sums: %v", qtySums)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_ProjectionOverJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(19101), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(19102), Price: proto.Int32(75)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan1 := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		scan2 := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		outerAlias := values.NamedCorrelationIdentifier("OUTER_ORDER")
		innerAlias := values.NamedCorrelationIdentifier("INNER_ORDER")
		nlj := mustExecutorConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
			scan1, scan2, nil, plans.JoinInner,
			outerAlias, innerAlias,
			integrationJoinResult(t, scan1, scan2, outerAlias, innerAlias, plans.JoinInner),
		))
		proj := mustExecutorConstruct(plans.NewRecordQueryProjectionPlan(
			[]values.Value{
				integrationField(t, nlj, 0),
				integrationField(t, nlj, 2),
			},
			nlj,
		))

		cursor, err := ExecutePlan(ctx, proj, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 4 {
			t.Fatalf("projection over cross join: %d rows, want 4 (2×2)", len(results))
		}
		for _, r := range results {
			d, _ := rowMapOK(r)
			if _, ok := d["order_id"]; !ok {
				t.Fatalf("missing ORDER_ID in projected datum: %v", d)
			}
			if _, ok := d["price"]; !ok {
				t.Fatalf("missing PRICE in projected datum: %v", d)
			}
			if _, ok := d["quantity"]; ok {
				t.Fatalf("QUANTITY should be projected out: %v", d)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_SortPlan_Reverse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(19201), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(19202), Price: proto.Int32(30)},
		&gen.Order{OrderId: proto.Int64(19203), Price: proto.Int32(20)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		sort := mustExecutorConstruct(plans.NewRecordQueryInMemorySortPlan(scan, []plans.SortKey{
			{Field: "price", ValueExpr: integrationField(t, scan, 2), Desc: true},
		}))

		cursor, err := ExecutePlan(ctx, sort, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("sort returned %d rows, want 3", len(results))
		}
		prices := make([]int64, len(results))
		for i, r := range results {
			d, _ := rowMapOK(r)
			prices[i] = d["price"].(int64)
		}
		if prices[0] != int64(30) || prices[1] != int64(20) || prices[2] != int64(10) {
			t.Errorf("expected [30 20 10], got %v", prices)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_FilterPlan_CompoundPredicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(19301), Price: proto.Int32(50), Quantity: proto.Int32(1)},
		&gen.Order{OrderId: proto.Int64(19302), Price: proto.Int32(150), Quantity: proto.Int32(1)},
		&gen.Order{OrderId: proto.Int64(19303), Price: proto.Int32(50), Quantity: proto.Int32(2)},
		&gen.Order{OrderId: proto.Int64(19304), Price: proto.Int32(150), Quantity: proto.Int32(2)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		pricePred := predicates.NewComparisonPredicate(
			integrationField(t, scan, 2),
			predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(100)),
		)
		qtyPred := predicates.NewComparisonPredicate(
			integrationField(t, scan, 4),
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
		)
		andPred := predicates.NewAnd(pricePred, qtyPred)
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan([]predicates.QueryPredicate{andPred}, scan))

		cursor, err := ExecutePlan(ctx, filter, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("compound filter returned %d rows, want 1 (PRICE>100 AND QUANTITY=1)", len(results))
		}
		d, _ := rowMapOK(results[0])
		if d["order_id"] != int64(19302) {
			t.Errorf("ORDER_ID = %v, want 19302", d["order_id"])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_LimitOverSort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(19401), Price: proto.Int32(100)},
		&gen.Order{OrderId: proto.Int64(19402), Price: proto.Int32(200)},
		&gen.Order{OrderId: proto.Int64(19403), Price: proto.Int32(300)},
		&gen.Order{OrderId: proto.Int64(19404), Price: proto.Int32(400)},
		&gen.Order{OrderId: proto.Int64(19405), Price: proto.Int32(500)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		sort := mustExecutorConstruct(plans.NewRecordQueryInMemorySortPlan(scan, []plans.SortKey{
			{Field: "price", ValueExpr: integrationField(t, scan, 2), Desc: true},
		}))
		limit := mustExecutorConstruct(plans.NewRecordQueryLimitPlan(sort, int64(3), int64(0)))

		cursor, err := ExecutePlan(ctx, limit, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("limit over sort returned %d rows, want 3", len(results))
		}
		prices := []int64{}
		for _, r := range results {
			d, _ := rowMapOK(r)
			prices = append(prices, d["price"].(int64))
		}
		if prices[0] != 500 || prices[1] != 400 || prices[2] != 300 {
			t.Errorf("expected top-3 DESC [500 400 300], got %v", prices)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_UpdatePlan_ClearField(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(19501), Price: proto.Int32(100), Quantity: proto.Int32(5)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		update := mustExecutorConstruct(plans.NewRecordQueryUpdatePlan(scan, "Order", []expressions.UpdateTransform{
			{FieldPath: "quantity", NewValue: values.LiteralValue(nil)},
		}))

		cursor, err := ExecutePlan(ctx, update, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("update returned %d results, want 1", len(results))
		}

		rec, err := s.LoadRecord(tuple.Tuple{int64(19501)})
		if err != nil {
			t.Fatalf("LoadRecord: %v", err)
		}
		updated := rec.Record.(*gen.Order)
		if updated.Quantity != nil {
			t.Errorf("quantity should be cleared (nil), got %v", updated.GetQuantity())
		}
		if updated.GetPrice() != 100 {
			t.Errorf("price should be unchanged at 100, got %d", updated.GetPrice())
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_FilterPlan_OrPredicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(19601), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(19602), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(19603), Price: proto.Int32(100)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
		lowPred := predicates.NewComparisonPredicate(
			integrationField(t, scan, 2),
			predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(20)),
		)
		highPred := predicates.NewComparisonPredicate(
			integrationField(t, scan, 2),
			predicates.NewLiteralComparison(predicates.ComparisonGreaterThanEq, int64(100)),
		)
		orPred := predicates.NewOr(lowPred, highPred)
		filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan([]predicates.QueryPredicate{orPred}, scan))

		cursor, err := ExecutePlan(ctx, filter, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("OR filter returned %d rows, want 2 (PRICE<20 OR PRICE>=100)", len(results))
		}
		ids := map[int64]bool{}
		for _, r := range results {
			d, _ := rowMapOK(r)
			ids[d["order_id"].(int64)] = true
		}
		if !ids[19601] || !ids[19603] {
			t.Errorf("expected orders 19601 and 19603, got %v", ids)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_IndexScan_FullRange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(19701), Price: proto.Int32(10)},
		&gen.Order{OrderId: proto.Int64(19702), Price: proto.Int32(20)},
		&gen.Order{OrderId: proto.Int64(19703), Price: proto.Int32(30)},
	)

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		idxPlan := mustExecutorConstruct(plans.NewRecordQueryIndexPlan(
			"order_price_idx",
			nil,
			[]string{"Order"},
			integrationOrderType(),
			false,
		)).WithKeyComponentTypes([]values.Type{values.NullableInt})

		cursor, err := ExecutePlan(ctx, idxPlan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		results, err := CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("CollectAll: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("full index scan returned %d rows, want 3", len(results))
		}
		prices := []int64{}
		for _, r := range results {
			d, _ := rowMapOK(r)
			prices = append(prices, d["price"].(int64))
		}
		if prices[0] != 10 || prices[1] != 20 || prices[2] != 30 {
			t.Errorf("expected ASC order [10 20 30], got %v", prices)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_LoadByKeys_Resume pins the resumable key-list shape of
// LoadByKeys (Java RecordQueryLoadByKeysPlan.executePlan resumes via
// RecordCursor.fromList(keys, continuation)): reading one row, then
// re-executing with the emitted continuation, must yield ONLY the
// remaining keys' records. The eager buffered form dropped the incoming
// continuation while emitting resumable-looking list tokens, so a
// resume replayed every row from 0 — duplicates. Missing keys are
// skipped like Java's .filter(Objects::nonNull).
func TestIntegration_LoadByKeys_Resume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store, &gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
	}, &gen.Order{
		OrderId: proto.Int64(2),
		Price:   proto.Int32(200),
	}, &gen.Order{
		OrderId: proto.Int64(3),
		Price:   proto.Int32(300),
	})

	// Key 99 has no record — skipped without consuming an output slot.
	keys := []tuple.Tuple{{int64(1)}, {int64(99)}, {int64(2)}, {int64(3)}}
	p := mustExecutorConstruct(plans.NewRecordQueryLoadByKeysPlanFromKeys(keys, integrationOrderType()))

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}

		cursor, err := ExecutePlan(ctx, p, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		defer cursor.Close()

		first, err := cursor.OnNext(ctx)
		if err != nil {
			t.Fatalf("OnNext: %v", err)
		}
		if !first.HasNext() {
			t.Fatal("expected a first row")
		}
		if got := first.GetValue().PrimaryKey; len(got) != 1 || got[0] != int64(1) {
			t.Fatalf("first row pk = %v, want [1]", got)
		}
		contBytes, err := first.GetContinuation().ToBytes()
		if err != nil {
			t.Fatalf("continuation: %v", err)
		}
		if len(contBytes) == 0 {
			t.Fatal("expected a resumable continuation after row 1")
		}

		resumed, err := ExecutePlan(ctx, p, s, EmptyEvaluationContext(), contBytes, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("ExecutePlan(resume): %v", err)
		}
		defer resumed.Close()
		rest, err := CollectAll(ctx, resumed)
		if err != nil {
			t.Fatalf("CollectAll(resume): %v", err)
		}
		var pks []int64
		for _, r := range rest {
			pks = append(pks, r.PrimaryKey[0].(int64))
		}
		// Resume must yield ONLY keys after the token (99 skipped): 2, 3.
		// The replay-from-0 behavior yielded 1, 2, 3 again.
		if len(pks) != 2 || pks[0] != int64(2) || pks[1] != int64(3) {
			t.Fatalf("resumed rows = %v, want [2 3]", pks)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

package executor

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"fdb.dev/internal/pb/demopb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
)

// TestIntegration_SortLimitPlan tests sort + limit against real records.
func TestIntegration_SortLimitPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(201), Price: proto.Int32(300)},
		&demopb.Order{OrderId: proto.Int64(202), Price: proto.Int32(100)},
		&demopb.Order{OrderId: proto.Int64(203), Price: proto.Int32(200)},
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
		&demopb.Order{OrderId: proto.Int64(1), VectorData: []byte{0x02, 0xff}},
		&demopb.Order{OrderId: proto.Int64(2), VectorData: []byte{0x01}},
		&demopb.Order{OrderId: proto.Int64(3), VectorData: []byte{0x03, 0x00}},
		&demopb.Order{OrderId: proto.Int64(4), VectorData: []byte{0x00, 0xaa}},
		&demopb.Order{OrderId: proto.Int64(5), VectorData: []byte{0x02, 0x01}},
		&demopb.Order{OrderId: proto.Int64(6), VectorData: []byte{0x01, 0xff}},
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
		&demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(300), Tags: []string{"c1", "c2"}, Flower: &demopb.Flower{Type: proto.String("rose"), Color: demopb.Color_RED.Enum()}},
		&demopb.Order{OrderId: proto.Int64(2), Price: proto.Int32(100), Tags: []string{"a1"}, Flower: &demopb.Flower{Type: proto.String("lily"), Color: demopb.Color_BLUE.Enum()}},
		&demopb.Order{OrderId: proto.Int64(3), Price: proto.Int32(500), Tags: []string{"e1", "e2", "e3"}, Flower: &demopb.Flower{Type: proto.String("tulip"), Color: demopb.Color_YELLOW.Enum()}},
		&demopb.Order{OrderId: proto.Int64(4), Price: proto.Int32(200), Tags: []string{"b1", "b2"}, Flower: &demopb.Flower{Type: proto.String("iris"), Color: demopb.Color_PINK.Enum()}},
		&demopb.Order{OrderId: proto.Int64(5), Price: proto.Int32(400), Tags: []string{"d1"}, Flower: &demopb.Flower{Type: proto.String("daisy"), Color: demopb.Color_RED.Enum()}},
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
					// Rows carry *demopb.Flower on BOTH sides of a checkpoint: the
					// restore rebuilds the ENCODED representation (a generated
					// row encodes flag 0 → the metadata's compiled type), so the resumed slot
					// keeps the generated concrete type. That identity is
					// load-bearing — the %T-keyed group/dedup paths
					// (computeGroupKey, packedDedupKey) would otherwise never
					// match a restored row against an equal fresh one.
					fm, isFlower := fv.(*demopb.Flower)
					if !isFlower {
						return nil, fmt.Errorf("flower slot = %#v (%T), want *demopb.Flower — the STRUCT column must survive the sort-continuation resume with its generated type", fv, fv)
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

// TestIntegration_FilterSortLimit_Pipeline tests a realistic pipeline:
// scan → filter → sort → limit.
func TestIntegration_FilterSortLimit_Pipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(1101), Price: proto.Int32(500)},
		&demopb.Order{OrderId: proto.Int64(1102), Price: proto.Int32(300)},
		&demopb.Order{OrderId: proto.Int64(1103), Price: proto.Int32(100)},
		&demopb.Order{OrderId: proto.Int64(1104), Price: proto.Int32(400)},
		&demopb.Order{OrderId: proto.Int64(1105), Price: proto.Int32(200)},
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

// TestIntegration_LimitWithOffset tests LIMIT with OFFSET > 0.
func TestIntegration_LimitWithOffset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(15101), Price: proto.Int32(10)},
		&demopb.Order{OrderId: proto.Int64(15102), Price: proto.Int32(20)},
		&demopb.Order{OrderId: proto.Int64(15103), Price: proto.Int32(30)},
		&demopb.Order{OrderId: proto.Int64(15104), Price: proto.Int32(40)},
		&demopb.Order{OrderId: proto.Int64(15105), Price: proto.Int32(50)},
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

// TestIntegration_SortPlan_MultiKey tests sorting by two fields:
// primary sort by PRICE ascending, secondary sort by QUANTITY descending.
func TestIntegration_SortPlan_MultiKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(17401), Price: proto.Int32(100), Quantity: proto.Int32(3)},
		&demopb.Order{OrderId: proto.Int64(17402), Price: proto.Int32(200), Quantity: proto.Int32(1)},
		&demopb.Order{OrderId: proto.Int64(17403), Price: proto.Int32(100), Quantity: proto.Int32(7)},
		&demopb.Order{OrderId: proto.Int64(17404), Price: proto.Int32(200), Quantity: proto.Int32(9)},
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

func TestIntegration_SortPlan_Reverse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(19201), Price: proto.Int32(10)},
		&demopb.Order{OrderId: proto.Int64(19202), Price: proto.Int32(30)},
		&demopb.Order{OrderId: proto.Int64(19203), Price: proto.Int32(20)},
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

func TestIntegration_LimitOverSort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(19401), Price: proto.Int32(100)},
		&demopb.Order{OrderId: proto.Int64(19402), Price: proto.Int32(200)},
		&demopb.Order{OrderId: proto.Int64(19403), Price: proto.Int32(300)},
		&demopb.Order{OrderId: proto.Int64(19404), Price: proto.Int32(400)},
		&demopb.Order{OrderId: proto.Int64(19405), Price: proto.Int32(500)},
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

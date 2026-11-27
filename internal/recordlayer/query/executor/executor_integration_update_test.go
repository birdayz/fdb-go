package executor

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/internal/pb/demopb"
	"fdb.dev/internal/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/internal/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

// TestIntegration_UpdatePlan tests updating records via the executor.
func TestIntegration_UpdatePlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(601), Price: proto.Int32(100)},
		&demopb.Order{OrderId: proto.Int64(602), Price: proto.Int32(200)},
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
			orderUpdate("price", values.LiteralValue(int64(999))),
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
		updated := rec.Record.(*demopb.Order)
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
		other := untouched.Record.(*demopb.Order)
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
	wantFlower := &demopb.Flower{Type: proto.String("rose"), Color: demopb.Color_BLUE.Enum()}

	insertOrders(t, store,
		&demopb.Order{
			OrderId: proto.Int64(603),
			Price:   proto.Int32(100),
			Flower:  proto.Clone(wantFlower).(*demopb.Flower),
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
				FieldNames:    []string{"price"},
				FieldOrdinals: orderUpdate("price", nil).FieldOrdinals,
				NewValue: &values.ArithmeticValue{
					Op:   values.OpAdd,
					Left: mustTestFieldOrdinal(t, target, 2),
					// An INT, as SQL types the literal 7: INT + LONG is a LONG,
					// which no promotion takes to the INT column.
					Right: &values.ConstantValue{Value: int64(7), Typ: values.NotNullInt},
				},
			},
			orderUpdate("flower", mustTestFieldOrdinal(t, target, 1)),
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
		updated := rec.Record.(*demopb.Order)
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
				// The column's own type, so the plan admits the value and the
				// owner's other exact row is what the executor refuses.
				wrongType := exactTestRowType(values.Field{Name: "price", FieldType: values.NullableInt})
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
			insertOrders(t, store, &demopb.Order{OrderId: proto.Int64(id), Price: proto.Int32(100)})

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
					orderUpdate("price", test.value(t, rowType)),
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
				if got := rec.Record.(*demopb.Order).GetPrice(); got != 100 {
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

// TestIntegration_UpdatePlan_WithParameter tests UPDATE with a parameterized
// SET value against real FDB.
func TestIntegration_UpdatePlan_WithParameter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(8201), Price: proto.Int32(100)},
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
			orderUpdate("price", values.NewParameterValue(1)),
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
		updated := rec.Record.(*demopb.Order)
		if updated.GetPrice() != 777 {
			t.Errorf("price after param update = %d, want 777", updated.GetPrice())
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
		&demopb.Order{OrderId: proto.Int64(17201), Price: proto.Int32(100), Quantity: proto.Int32(5)},
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
			orderUpdate("price", values.LiteralValue(int64(999))),
			orderUpdate("quantity", values.LiteralValue(int64(42))),
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
		updated := rec.Record.(*demopb.Order)
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

func TestIntegration_UpdatePlan_ClearField(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)

	insertOrders(t, store,
		&demopb.Order{OrderId: proto.Int64(19501), Price: proto.Int32(100), Quantity: proto.Int32(5)},
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
			orderUpdate("quantity", values.LiteralValue(nil)),
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
		updated := rec.Record.(*demopb.Order)
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

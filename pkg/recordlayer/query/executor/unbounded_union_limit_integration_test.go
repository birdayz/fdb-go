package executor

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
)

func TestIntegration_LimitResumeOffsetDomain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)
	ks := testSubspace(t)
	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(1)},
		&gen.Order{OrderId: proto.Int64(2)},
		&gen.Order{OrderId: proto.Int64(3)},
	)
	scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
	plan := mustExecutorConstruct(plans.NewRecordQueryLimitPlan(scan, 2, 1))
	for _, offset := range []int64{1, -1, math.MinInt64} {
		t.Run(fmt.Sprint(offset), func(t *testing.T) {
			t.Parallel()
			continuation, err := encodeLimitContinuation(nil, 1, 2)
			if err != nil {
				t.Fatal(err)
			}
			binary.BigEndian.PutUint64(continuation[1:9], uint64(offset))
			_, err = testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				s, err := recordlayer.NewStoreBuilder().SetContext(rtx).
					SetMetaDataProvider(store.GetMetaData()).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				cursor, err := ExecutePlan(ctx, plan, s, EmptyEvaluationContext(), continuation,
					recordlayer.DefaultExecuteProperties())
				if cursor != nil {
					defer cursor.Close()
				}
				if offset < 0 {
					var offsetErr *expressions.InvalidLimitOffsetError
					if !errors.As(err, &offsetErr) || offsetErr.Offset != offset || cursor != nil {
						t.Fatalf("resume offset %d: cursor %T, error %v; want rejection before opening child", offset, cursor, err)
					}
					return nil, nil
				}
				if err != nil {
					return nil, err
				}
				rows, err := CollectAll(ctx, cursor)
				if err != nil {
					return nil, err
				}
				if len(rows) != 2 || rows[0].PrimaryKey[0] != int64(2) || rows[1].PrimaryKey[0] != int64(3) {
					t.Fatalf("valid resume returned %v, want IDs 2,3", rows)
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIntegration_LimitParentSkip(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"direct", "singleton_in", "filtered_child"} {
		for _, tc := range []struct {
			name          string
			limit, offset int64
			skip, cap     int
			want          string
		}{
			{"empty_after_skip", 1, 1, 2, 1, "[]"},
			{"finite_clip", 2, 1, 1, 2, "[3]"},
			{"finite_no_parent_cap", 2, 1, 1, 0, "[3]"},
			{"within_window", 5, 0, 1, 2, "[2 3]"},
			{"unbounded", -1, 1, 2, 1, "[4]"},
			{"zero_skip", 2, 1, 0, 1, "[2]"},
			{"negative_skip", 2, 1, -1, 1, "[2]"},
			{"unbounded_large_offset", -1, math.MaxInt64, 1, 1, "[]"},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				store := setupStore(t)
				for i := int64(1); i <= 6; i++ {
					insertOrders(t, store, &gen.Order{OrderId: proto.Int64(i)})
				}
				scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
				var child plans.RecordQueryPlan = scan
				if kind == "filtered_child" {
					insertOrders(t, store, &gen.Order{OrderId: proto.Int64(0)})
					child = mustExecutorConstruct(plans.NewRecordQueryFilterPlan([]predicates.QueryPredicate{
						predicates.NewComparisonPredicate(integrationField(t, scan, 0), predicates.Comparison{
							Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(0)),
						}),
					}, scan))
				}
				var plan plans.RecordQueryPlan = mustExecutorConstruct(plans.NewRecordQueryLimitPlan(child, tc.limit, tc.offset))
				if kind == "singleton_in" {
					plan = mustExecutorConstruct(plans.NewRecordQueryInUnionPlan(plan, []string{"skip_binding"}, nil, false)).
						WithInSources([][]any{{int64(1)}})
				}
				_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					s, err := recordlayer.NewStoreBuilder().SetContext(rtx).
						SetMetaDataProvider(store.GetMetaData()).SetSubspace(testSubspace(t)).Open()
					if err != nil {
						return nil, err
					}
					cursor, err := ExecutePlan(ctx, plan, s, EmptyEvaluationContext(), nil,
						recordlayer.DefaultExecuteProperties().WithSkip(tc.skip).WithReturnedRowLimit(tc.cap))
					if err != nil {
						return nil, err
					}
					defer cursor.Close()
					rows, err := CollectAll(ctx, cursor)
					if err != nil {
						return nil, err
					}
					var ids []int64
					for _, row := range rows {
						ids = append(ids, row.PrimaryKey[0].(int64))
					}
					if fmt.Sprint(ids) != tc.want {
						t.Fatalf("request skip/cap %d/%d above %s returned %v, want %s", tc.skip, tc.cap, plan.Explain(), ids, tc.want)
					}
					return nil, nil
				})
				if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestIntegration_LimitParentSkipResume(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ wrapped, partialSkip bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("singleton=%v/partial_skip=%v", tc.wrapped, tc.partialSkip), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := setupStore(t)
			orders := make([]*gen.Order, 8)
			for i := range orders {
				orders[i] = &gen.Order{OrderId: proto.Int64(int64(i + 1))}
			}
			insertOrders(t, store, orders...)
			scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
			var plan plans.RecordQueryPlan = mustExecutorConstruct(plans.NewRecordQueryLimitPlan(scan, 5, 1))
			if tc.wrapped {
				plan = mustExecutorConstruct(plans.NewRecordQueryInUnionPlan(plan, []string{"skip_binding"}, nil, false)).
					WithInSources([][]any{{int64(1)}})
			}
			var continuation []byte
			var ids []int64
			ended := false
			for page := 0; page < 5 && !ended; page++ {
				var next []byte
				var emitted []int64
				var exhausted bool
				var reason recordlayer.NoNextReason
				_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					s, err := recordlayer.NewStoreBuilder().SetContext(rtx).
						SetMetaDataProvider(store.GetMetaData()).SetSubspace(testSubspace(t)).Open()
					if err != nil {
						return nil, err
					}
					props := recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(1)
					if page == 0 {
						props.Skip = 2
						if tc.partialSkip {
							props.ScannedRecordsLimit = 2 // semantic offset + one request-skipped row
						}
					} else if page == 1 && tc.partialSkip {
						// Request skip is per execution, as in Java. One of the two
						// requested rows was skipped; this call requests the other one.
						props.Skip = 1
					}
					cursor, err := ExecutePlan(ctx, plan, s, EmptyEvaluationContext(), continuation, props)
					if err != nil {
						return nil, err
					}
					defer cursor.Close()
					emitted = nil
					for {
						r, err := cursor.OnNext(ctx)
						if err != nil {
							return nil, err
						}
						if r.HasNext() {
							emitted = append(emitted, r.GetValue().PrimaryKey[0].(int64))
							continue
						}
						reason = r.GetNoNextReason()
						exhausted = reason.IsSourceExhausted()
						next, err = r.GetContinuation().ToBytes()
						return nil, err
					}
				})
				if err != nil {
					t.Fatal(err)
				}
				wantRows := 1
				if page == 0 && tc.partialSkip {
					wantRows = 0
					if reason != recordlayer.ScanLimitReached {
						t.Fatalf("mid-skip stop = %v, want scan limit", reason)
					}
				} else if exhausted && len(emitted) == 0 {
					// A request cap may stop before asking the envelope whether
					// its last emitted row exhausted the semantic window.
					wantRows = 0
				}
				if len(emitted) != wantRows {
					t.Fatalf("page %d returned %v, want %d rows", page, emitted, wantRows)
				}
				if page == 0 {
					wantRemaining := 2 // two skipped + one emitted consumed three of five
					if tc.partialSkip {
						wantRemaining = 4 // only one request-skipped row consumed
					}
					_, offset, limit, err := decodeLimitContinuation(next, 1, 5)
					if err != nil || offset != 0 || limit != wantRemaining {
						t.Fatalf("first-page semantic window = %d/%d, %v; want 0/%d", offset, limit, err, wantRemaining)
					}
				}
				continuation, ended = next, exhausted
				ids = append(ids, emitted...)
			}
			if !ended || fmt.Sprint(ids) != "[4 5 6]" {
				t.Fatalf("resumed request skip: ended=%v rows=%v, want [4 5 6] within the original semantic cap", ended, ids)
			}
		})
	}
}

// TestIntegration_UnboundedUnionLimit plans a skip-only logical LIMIT through
// the full rule sets, then executes it against FDB. SQL does not expose the
// negative no-cap sentinel, so the logical API is the regression boundary.
func TestIntegration_UnboundedUnionLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)
	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(1)},
		&gen.Order{OrderId: proto.Int64(2)},
		&gen.Order{OrderId: proto.Int64(3)},
	)

	var legs []expressions.Quantifier
	for range 2 {
		scan, err := expressions.NewFullUnorderedScanExpression([]string{"Order"}, integrationOrderType())
		if err != nil {
			t.Fatal(err)
		}
		legs = append(legs, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	}
	union, err := expressions.NewLogicalUnionExpression(legs)
	if err != nil {
		t.Fatal(err)
	}
	limit, err := expressions.NewLogicalLimitExpression(-1, 2,
		expressions.ForEachQuantifier(expressions.InitialOf(union)))
	if err != nil {
		t.Fatal(err)
	}
	planner := cascades.NewPlanner(cascades.DefaultExpressionRules(), nil).
		WithPlanningExpressionRules(cascades.BatchAExpressionRules()).
		WithImplementationRules(cascades.DefaultImplementationRules())
	best, _, err := planner.PlanWithContext(ctx, expressions.InitialOf(limit))
	if err != nil {
		t.Fatal(err)
	}
	physical, ok := best.(plans.RecordQueryPlan)
	if !ok {
		t.Fatalf("planner returned %T, want executable plan", best)
	}
	t.Logf("skip-only union plan: %s", physical.Explain())

	_, err = testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().SetContext(rtx).
			SetMetaDataProvider(store.GetMetaData()).SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}
		cursor, err := ExecutePlan(ctx, physical, s, EmptyEvaluationContext(), nil,
			recordlayer.DefaultExecuteProperties())
		if err != nil {
			return nil, err
		}
		defer cursor.Close()
		rows, err := CollectAll(ctx, cursor)
		if err != nil {
			return nil, err
		}
		if len(rows) != 4 {
			t.Fatalf("UNION ALL has six rows; OFFSET 2 without a cap returned %d, want 4; plan %s", len(rows), physical.Explain())
		}
		counts := make(map[int64]int)
		for _, row := range rows {
			if len(row.PrimaryKey) != 1 {
				t.Fatalf("unexpected primary key %v", row.PrimaryKey)
			}
			id, ok := row.PrimaryKey[0].(int64)
			if !ok {
				t.Fatalf("primary key has type %T", row.PrimaryKey[0])
			}
			counts[id]++
		}
		if len(counts) != 3 || counts[1] != 1 || counts[2] != 1 || counts[3] != 2 {
			t.Fatalf("remaining IDs = %v, want one each of 1,2 and two of 3", counts)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_StrictFirstOrDefaultRequest(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"direct", "map", "projection"} {
		for _, probe := range []struct{ outOfBand, mixedTypes bool }{{}, {true, false}, {false, true}, {true, true}} {
			outOfBand := probe.outOfBand
			t.Run(fmt.Sprintf("%s/oob=%t/mixed=%t", kind, outOfBand, probe.mixedTypes), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				store := setupStore(t)
				insertOrders(t, store, &gen.Order{OrderId: proto.Int64(1)}, &gen.Order{OrderId: proto.Int64(2)})
				if probe.mixedTypes {
					// A raw scan budget of two would count Customer 0, then only
					// Order 1, and hide the second matching scalar row (Order 2).
					_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
						s, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).SetSubspace(testSubspace(t)).Open()
						if err != nil {
							return nil, err
						}
						_, err = s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(0)})
						return nil, err
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
				var plan plans.RecordQueryPlan = mustExecutorConstruct(plans.NewRecordQueryFirstOrDefaultPlanStrict(scan, values.NewNullValue(scan.GetResultType())))
				constant := &values.ConstantValue{Value: int64(42), Typ: values.NotNullLong}
				if kind == "map" {
					plan = mustExecutorConstruct(plans.NewRecordQueryMapPlan(plan, constant))
				} else if kind == "projection" {
					plan = mustExecutorConstruct(plans.NewRecordQueryProjectionPlan([]values.Value{constant}, plan))
				}
				_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					s, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).SetSubspace(testSubspace(t)).Open()
					if err != nil {
						return nil, err
					}
					props := recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(1)
					if outOfBand {
						props.ScannedRecordsLimit = 1
					}
					cursor, err := ExecutePlan(ctx, plan, s, EmptyEvaluationContext(), nil, props)
					if cursor != nil {
						defer cursor.Close()
					}
					if !outOfBand || err != nil {
						return nil, err
					}
					stop, err := cursor.OnNext(ctx)
					if err != nil || stop.HasNext() || stop.GetNoNextReason() != recordlayer.ScanLimitReached {
						t.Fatalf("second-row OOB probe must stop without a scalar result: %v, %v", stop, err)
					}
					token := continuationBytesForTest(t, stop.GetContinuation())
					mode, _, err := decodeFirstOrDefaultContinuation(token)
					if probe.mixedTypes {
						if err != nil || mode != fodResumeInner {
							t.Fatalf("stop before first matching row must checkpoint: mode=%v err=%v", mode, err)
						}
					} else if err != nil || mode != fodResumeStart || len(token) != 1 || token[0] != fodTagRestart {
						t.Fatalf("truncated cardinality proof must restart with held first row: mode=%v err=%v", mode, err)
					}
					resumed, err := ExecutePlan(ctx, plan, s, EmptyEvaluationContext(), token,
						recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(1).WithScannedRecordsLimit(3))
					if resumed != nil {
						defer resumed.Close()
					}
					return nil, err
				})
				var cardinality *api.Error
				if !errors.As(err, &cardinality) || cardinality.Code != api.ErrCodeCardinalityViolation {
					t.Fatalf("bounded request must not suppress real two-row scalar: %v, want 21000", err)
				}
			})
		}
	}
}

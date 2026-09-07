package executor

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

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

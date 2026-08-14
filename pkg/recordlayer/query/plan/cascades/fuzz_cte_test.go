package cascades

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// FuzzPlanner_RecursiveDfsJoin_NoPanic verifies that running the
// planner on a RecursiveUnionExpression tree with randomized
// traversal strategies never panics.
func FuzzPlanner_RecursiveDfsJoin_NoPanic(f *testing.F) {
	f.Add(byte(0))
	f.Add(byte(1))
	f.Add(byte(2))
	f.Add(byte(3))
	f.Add(byte(255))

	f.Fuzz(func(t *testing.T, strategyByte byte) {
		strategy := expressions.TraversalStrategy(strategyByte % 4)

		scanAlias := values.UniqueCorrelationIdentifier()
		insertAlias := values.UniqueCorrelationIdentifier()
		rowType := values.NewRecordType("RecursiveRow", false, []values.Field{
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		})

		initialScan := mustFullUnorderedScan(t, []string{"T"}, rowType)
		initialRef := expressions.InitialOf(initialScan)
		initialQ := expressions.ForEachQuantifier(initialRef)
		initialInsert := mustTempTableInsert(t, initialQ, insertAlias, true)

		recursiveScan := mustTempTableScan(t, scanAlias, rowType)
		recursiveRef := expressions.InitialOf(recursiveScan)
		recursiveQ := expressions.ForEachQuantifier(recursiveRef)
		recursiveInsert := mustTempTableInsert(t, recursiveQ, insertAlias, false)

		initialInsertQ := expressions.ForEachQuantifier(expressions.InitialOf(initialInsert))
		recursiveInsertQ := expressions.ForEachQuantifier(expressions.InitialOf(recursiveInsert))

		recUnion := mustRecursiveUnion(t,
			initialInsertQ, recursiveInsertQ,
			scanAlias, insertAlias,
			strategy,
		)

		rootRef := expressions.InitialOf(recUnion)

		rules := DefaultExpressionRules()
		p := NewPlanner(rules, EmptyPlanContext()).
			WithPlanningExpressionRules(BatchAExpressionRules())
		_, _, _ = p.PlanWithContext(context.Background(), rootRef)
	})
}

// FuzzTempTable_ConcurrentOps exercises TempTable under concurrent
// add/clear/list operations looking for data races.
func FuzzTempTable_ConcurrentOps(f *testing.F) {
	f.Add(uint8(10), uint8(3))
	f.Add(uint8(100), uint8(1))
	f.Add(uint8(50), uint8(5))

	f.Fuzz(func(t *testing.T, nOps, nClears uint8) {
		tt := NewTempTable()
		done := make(chan struct{})

		go func() {
			for i := 0; i < int(nOps); i++ {
				tt.Add(i)
			}
			close(done)
		}()

		for i := 0; i < int(nClears); i++ {
			tt.Clear()
			_ = tt.IsEmpty()
			_ = tt.List()
			_ = tt.Len()
		}

		<-done
	})
}

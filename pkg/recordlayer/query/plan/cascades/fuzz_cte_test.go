package cascades

import (
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

		initialScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		initialRef := expressions.InitialOf(initialScan)
		initialQ := expressions.ForEachQuantifier(initialRef)
		initialInsert := expressions.NewTempTableInsertExpression(initialQ, insertAlias, true)

		recursiveScan := expressions.NewTempTableScanExpression(scanAlias)
		recursiveRef := expressions.InitialOf(recursiveScan)
		recursiveQ := expressions.ForEachQuantifier(recursiveRef)
		recursiveInsert := expressions.NewTempTableInsertExpression(recursiveQ, insertAlias, false)

		initialInsertQ := expressions.ForEachQuantifier(expressions.InitialOf(initialInsert))
		recursiveInsertQ := expressions.ForEachQuantifier(expressions.InitialOf(recursiveInsert))

		recUnion := expressions.NewRecursiveUnionExpression(
			initialInsertQ, recursiveInsertQ,
			scanAlias, insertAlias,
			strategy,
		)

		rootRef := expressions.InitialOf(recUnion)

		rules := DefaultExpressionRules()
		p := NewPlanner(rules, EmptyPlanContext()).
			WithPlanningExpressionRules(BatchAExpressionRules())
		_, _, _ = p.Plan(rootRef)
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

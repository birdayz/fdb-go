package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func FuzzPlanner_E2E_NoPanic(f *testing.F) {
	f.Add(uint8(0), uint8(0), uint8(0))
	f.Add(uint8(1), uint8(1), uint8(0))
	f.Add(uint8(2), uint8(0), uint8(1))
	f.Add(uint8(3), uint8(2), uint8(2))
	f.Add(uint8(4), uint8(0), uint8(0))
	f.Add(uint8(5), uint8(1), uint8(1))

	f.Fuzz(func(t *testing.T, shape uint8, filterCount uint8, sortCount uint8) {
		if filterCount > 3 {
			filterCount = 3
		}
		if sortCount > 3 {
			sortCount = 3
		}

		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		current := expressions.InitialOf(scan)

		for i := uint8(0); i < filterCount; i++ {
			pred := predicates.NewComparisonPredicate(
				&values.FieldValue{Field: string(rune('a' + i)), Typ: values.UnknownType},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(i)),
			)
			filter := expressions.NewLogicalFilterExpression(
				[]predicates.QueryPredicate{pred},
				expressions.ForEachQuantifier(current),
			)
			current = expressions.InitialOf(filter)
		}

		for i := uint8(0); i < sortCount; i++ {
			sort := expressions.NewLogicalSortExpression(
				[]expressions.SortKey{{
					Value:   &values.FieldValue{Field: string(rune('x' + i)), Typ: values.UnknownType},
					Reverse: i%2 == 1,
				}},
				expressions.ForEachQuantifier(current),
			)
			current = expressions.InitialOf(sort)
		}

		switch shape % 6 {
		case 1:
			distinct := expressions.NewLogicalDistinctExpression(
				expressions.ForEachQuantifier(current))
			current = expressions.InitialOf(distinct)
		case 2:
			unique := expressions.NewLogicalUniqueExpression(
				expressions.ForEachQuantifier(current))
			current = expressions.InitialOf(unique)
		case 3:
			limit := expressions.NewLogicalLimitExpression(10, 0,
				expressions.ForEachQuantifier(current))
			current = expressions.InitialOf(limit)
		case 4:
			scan2 := expressions.NewFullUnorderedScanExpression([]string{"U"}, values.UnknownType)
			union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
				expressions.ForEachQuantifier(current),
				expressions.ForEachQuantifier(expressions.InitialOf(scan2)),
			})
			current = expressions.InitialOf(union)
		case 5:
			proj := expressions.NewLogicalProjectionExpression(
				[]values.Value{&values.FieldValue{Field: "proj", Typ: values.UnknownType}},
				expressions.ForEachQuantifier(current),
			)
			current = expressions.InitialOf(proj)
		}

		p := NewPlanner(allRules(), nil).WithPlanningExpressionRules(BatchAExpressionRules()).WithImplementationRules(DefaultImplementationRules())
		_, _, _ = p.Plan(current)
	})
}

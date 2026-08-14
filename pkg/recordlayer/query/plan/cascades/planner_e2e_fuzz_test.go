package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustE2EFuzzConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct planner E2E fuzz fixture: " + err.Error())
	}
	return value
}

func e2eFuzzRowType() values.Type {
	names := []string{"a", "b", "c", "x", "y", "z", "proj"}
	fields := make([]values.Field, len(names))
	for i, name := range names {
		fields[i] = values.Field{Name: name, FieldType: values.NullableLong, Ordinal: i}
	}
	return values.NewRecordType("E2EFuzzRow", false, fields)
}

func e2eFuzzRoot(q expressions.Quantifier) values.Value {
	flowedType := mustE2EFuzzConstruct(q.GetFlowedObjectType())
	return mustE2EFuzzConstruct(values.NewQuantifiedObjectValue(q.GetAlias(), flowedType))
}

func e2eFuzzField(root values.Value, name string) values.Value {
	request := mustE2EFuzzConstruct(values.FieldByName(name))
	return mustE2EFuzzConstruct(values.ResolveFieldAccess(
		root, []values.FieldRequest{request}))
}

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

		scan := mustE2EFuzzConstruct(expressions.NewFullUnorderedScanExpression(
			[]string{"T"}, e2eFuzzRowType()))
		current := expressions.InitialOf(scan)

		for i := uint8(0); i < filterCount; i++ {
			q := expressions.ForEachQuantifier(current)
			root := e2eFuzzRoot(q)
			pred := predicates.NewComparisonPredicate(
				e2eFuzzField(root, string(rune('a'+i))),
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(i)),
			)
			filter := mustE2EFuzzConstruct(expressions.NewLogicalFilterExpression(
				[]predicates.QueryPredicate{pred},
				q,
			))
			current = expressions.InitialOf(filter)
		}

		for i := uint8(0); i < sortCount; i++ {
			q := expressions.ForEachQuantifier(current)
			root := e2eFuzzRoot(q)
			sort := mustE2EFuzzConstruct(expressions.NewLogicalSortExpression(
				[]expressions.SortKey{{
					Value:   e2eFuzzField(root, string(rune('x'+i))),
					Reverse: i%2 == 1,
				}},
				q,
			))
			current = expressions.InitialOf(sort)
		}

		switch shape % 6 {
		case 1:
			distinct := mustE2EFuzzConstruct(expressions.NewLogicalDistinctExpression(
				expressions.ForEachQuantifier(current)))
			current = expressions.InitialOf(distinct)
		case 2:
			unique := mustE2EFuzzConstruct(expressions.NewLogicalUniqueExpression(
				expressions.ForEachQuantifier(current)))
			current = expressions.InitialOf(unique)
		case 3:
			limit := mustE2EFuzzConstruct(expressions.NewLogicalLimitExpression(10, 0,
				expressions.ForEachQuantifier(current)))
			current = expressions.InitialOf(limit)
		case 4:
			scan2 := mustE2EFuzzConstruct(expressions.NewFullUnorderedScanExpression(
				[]string{"U"}, e2eFuzzRowType()))
			union := mustE2EFuzzConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
				expressions.ForEachQuantifier(current),
				expressions.ForEachQuantifier(expressions.InitialOf(scan2)),
			}))
			current = expressions.InitialOf(union)
		case 5:
			q := expressions.ForEachQuantifier(current)
			root := e2eFuzzRoot(q)
			proj := mustE2EFuzzConstruct(expressions.NewLogicalProjectionExpression(
				[]values.Value{e2eFuzzField(root, "proj")}, q))
			current = expressions.InitialOf(proj)
		}

		p := NewPlanner(DefaultExpressionRules(), nil).
			WithPlanningExpressionRules(BatchAExpressionRules()).
			WithImplementationRules(DefaultImplementationRules())
		_, _, _ = p.Plan(current)
	})
}

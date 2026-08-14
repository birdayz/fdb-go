package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// mustConstruct keeps successful fallible-constructor fixtures concise
// while preserving the error as part of every test setup. It deliberately does
// not substitute a zero value: a fixture that cannot state its type must stop
// the test at the construction boundary.
func mustConstruct[T any](t testing.TB, value T, err error) T {
	t.Helper()
	if err != nil {
		t.Fatalf("construct RFC-232 fixture: %v", err)
	}
	return value
}

func mustFullUnorderedScan(
	t testing.TB, recordTypes []string, flowedType values.Type,
) *expressions.FullUnorderedScanExpression {
	t.Helper()
	value, err := expressions.NewFullUnorderedScanExpression(recordTypes, flowedType)
	return mustConstruct(t, value, err)
}

func mustTempTableScan(
	t testing.TB, alias values.CorrelationIdentifier, flowedType values.Type,
) *expressions.TempTableScanExpression {
	t.Helper()
	value, err := expressions.NewTempTableScanExpression(alias, flowedType)
	return mustConstruct(t, value, err)
}

func mustTempTableInsert(
	t testing.TB,
	inner expressions.Quantifier,
	alias values.CorrelationIdentifier,
	owning bool,
) *expressions.TempTableInsertExpression {
	t.Helper()
	value, err := expressions.NewTempTableInsertExpression(inner, alias, owning)
	return mustConstruct(t, value, err)
}

func mustRecursiveUnion(
	t testing.TB,
	initialState, recursiveState expressions.Quantifier,
	tempTableScanAlias, tempTableInsertAlias values.CorrelationIdentifier,
	strategy expressions.TraversalStrategy,
) *expressions.RecursiveUnionExpression {
	t.Helper()
	value, err := expressions.NewRecursiveUnionExpression(
		initialState, recursiveState, tempTableScanAlias, tempTableInsertAlias, strategy)
	return mustConstruct(t, value, err)
}

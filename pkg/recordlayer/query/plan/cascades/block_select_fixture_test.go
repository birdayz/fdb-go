package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// newBlockSelectForTest is a query block publishing projected over inner: the
// Select a SELECT list translates to.
func newBlockSelectForTest(projected []values.Value, inner expressions.Quantifier) (*expressions.SelectExpression, error) {
	return newBlockSelectWithOutputSchemaForTest(projected, nil, nil, nil, inner)
}

func newBlockSelectWithAliasesForTest(projected []values.Value, aliases []string, inner expressions.Quantifier) (*expressions.SelectExpression, error) {
	return newBlockSelectWithOutputSchemaForTest(projected, aliases, nil, nil, inner)
}

func newBlockSelectWithOutputSchemaForTest(
	projected []values.Value,
	aliases []string,
	_ []bool,
	outputNames []string,
	inner expressions.Quantifier,
) (*expressions.SelectExpression, error) {
	result, err := values.ProjectionResultValueForOutputSchema(projected, aliases, outputNames)
	if err != nil {
		return nil, err
	}
	return expressions.NewSelectExpression(result, []expressions.Quantifier{inner}, nil)
}

// newProjectionMapForTest is the physical block result: a Map over inner
// publishing projected (aliases optional).
func newProjectionMapForTest(projected []values.Value, aliases []string, inner plans.RecordQueryPlan) (*plans.RecordQueryMapPlan, error) {
	return newProjectionMapFromQuantifierForTest(projected, aliases, plans.QuantifierOverPlan(inner))
}

func newProjectionMapFromQuantifierForTest(projected []values.Value, aliases []string, innerQ expressions.Quantifier) (*plans.RecordQueryMapPlan, error) {
	return newProjectionMapWithOutputSchemaForTest(projected, aliases, nil, innerQ)
}

func newProjectionMapWithOutputSchemaForTest(
	projected []values.Value,
	aliases, outputNames []string,
	innerQ expressions.Quantifier,
) (*plans.RecordQueryMapPlan, error) {
	result, err := values.ProjectionResultValueForOutputSchema(projected, aliases, outputNames)
	if err != nil {
		return nil, err
	}
	return plans.NewRecordQueryMapPlanFromQuantifier(innerQ, result)
}

func newProjectionMapOverForTest(projected []values.Value, inner plans.RecordQueryPlan) (*plans.RecordQueryMapPlan, error) {
	return newProjectionMapForTest(projected, nil, inner)
}

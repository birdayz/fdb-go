package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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

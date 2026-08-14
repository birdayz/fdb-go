package expressions

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func snapshotExpressionResultType(operator string, typ values.Type) (values.ExactTypeHandle, error) {
	handle, err := values.SnapshotExactType(typ)
	if err != nil {
		return nil, fmt.Errorf("%s result type: %w", operator, err)
	}
	return handle, nil
}

func requireFlowedResult(operator string, quantifier Quantifier) (values.QuantifiedObjectValue, error) {
	result, err := quantifier.RequireFlowedObjectValue()
	if err != nil {
		return nil, fmt.Errorf("%s result: %w", operator, err)
	}
	return result, nil
}

func requireSetOperationResult(operator string, quantifiers []Quantifier) (values.QuantifiedObjectValue, error) {
	for _, quantifier := range quantifiers {
		if quantifier.Kind() == QuantifierExistential {
			continue
		}
		return requireFlowedResult(operator, quantifier)
	}
	return nil, fmt.Errorf("%s result: no non-existential quantifier", operator)
}

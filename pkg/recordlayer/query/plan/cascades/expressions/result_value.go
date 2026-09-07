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

// requireSetOperationResults returns the flowed value of every non-existential
// leg of a set operation, in leg order, and requires every leg after the first
// to state the SAME row as the first, exactly. The first entry is the
// operation's result (Java's mergeValues picks that leg's type). A set
// operation's legs are aligned before the logical expression exists — by the
// SQL translator, or by whatever planner-API producer built the legs — so a
// leg that disagrees here is a construction error, and it fails here rather
// than in an implementation rule's body, where the only visible symptom would
// be a group with no realizable plan (RFC-242). Field names compare exactly:
// two spellings of one name are two names. Union, intersection and the
// recursive union all assert through this one function.
func requireSetOperationResults(operator string, quantifiers []Quantifier) ([]values.QuantifiedObjectValue, error) {
	var results []values.QuantifiedObjectValue
	firstFlowing := -1
	for i, quantifier := range quantifiers {
		if quantifier.Kind() == QuantifierExistential {
			continue
		}
		flowed, err := requireFlowedResult(operator, quantifier)
		if err != nil {
			return nil, fmt.Errorf("input quantifier %d: %w", i, err)
		}
		if len(results) == 0 {
			results = append(results, flowed)
			firstFlowing = i
			continue
		}
		if !values.FlowedTypesEqual(results[0], flowed) {
			return nil, fmt.Errorf(
				"%s result: input quantifier %d type %s disagrees with input quantifier %d type %s",
				operator, firstFlowing, results[0].Type(), i, flowed.Type())
		}
		results = append(results, flowed)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("%s result: no non-existential quantifier", operator)
	}
	return results, nil
}

// requireSetOperationResult is requireSetOperationResults for the operations
// that state only the first leg's value.
func requireSetOperationResult(operator string, quantifiers []Quantifier) (values.QuantifiedObjectValue, error) {
	results, err := requireSetOperationResults(operator, quantifiers)
	if err != nil {
		return nil, err
	}
	return results[0], nil
}

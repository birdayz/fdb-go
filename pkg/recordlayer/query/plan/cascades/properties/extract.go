package properties

import (
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
)

// ExtractBestPlan walks the Reference DAG rooted at `ref` and returns
// a fresh RelationalExpression tree where every reachable Reference
// is a singleton holding the cost-cheapest member chosen by
// CostLess. Children are extracted recursively.
//
// Use case: callers that have just run FixpointApply on a Reference
// and want to materialise a single best plan for execution / display
// / serialisation. Without this, the extracted "plan" is the
// Reference DAG with multiple alternative members still attached;
// downstream consumers have no easy way to lock in one shape.
//
// Behaviour:
//   - Returns nil if `ref` is nil or empty.
//   - The returned expression is structurally fresh — its
//     Quantifiers range over freshly-allocated single-member
//     Reference objects. Pointer identity with the input tree's
//     References is NOT preserved.
//   - Cycle-safe: walks down through Quantifiers only; the seed
//     planner doesn't construct cycles.
//   - Switch-on-concrete-type for constructor dispatch — each
//     concrete RelationalExpression type the seed exposes has an
//     arm. Adding a new type requires extending this switch.
//
// Returns an error if any reachable expression is of a type
// unknown to this extractor — surfacing the missing arm rather
// than silently dropping or panicking.
func ExtractBestPlan(ref *expressions.Reference) (expressions.RelationalExpression, error) {
	if ref == nil || len(ref.Members()) == 0 {
		return nil, nil
	}
	best := ref.GetBest(CostLess)
	if best == nil {
		return nil, nil
	}
	return rebuildExpression(best)
}

// rebuildExpression returns a fresh RelationalExpression of the same
// concrete type as `e`, with each Quantifier's Reference replaced by
// a singleton Reference holding the recursively-extracted best plan
// of the original Reference.
func rebuildExpression(e expressions.RelationalExpression) (expressions.RelationalExpression, error) {
	if e == nil {
		return nil, nil
	}
	// Recurse into each Quantifier first — collect fresh
	// Quantifiers for the new expression's children.
	freshChildren := make([]expressions.Quantifier, 0, len(e.GetQuantifiers()))
	for _, q := range e.GetQuantifiers() {
		inner, err := ExtractBestPlan(q.GetRangesOver())
		if err != nil {
			return nil, err
		}
		var freshRef *expressions.Reference
		if inner == nil {
			// Empty / nil inner Reference — shouldn't happen for a
			// valid plan, but defensive: keep the Quantifier shape
			// with an empty Reference. Caller can detect via
			// ref.Members().
			freshRef = &expressions.Reference{}
		} else {
			freshRef = expressions.InitialOf(inner)
		}
		freshChildren = append(freshChildren, expressions.ForEachQuantifier(freshRef))
	}
	// Switch on concrete type — each arm reconstructs the expression
	// using its public constructor, threading the fresh quantifiers
	// in place of the originals.
	switch ex := e.(type) {

	case *expressions.FullUnorderedScanExpression:
		// Leaf — no children, return a fresh scan with the same
		// record-type set + flowed Type.
		return expressions.NewFullUnorderedScanExpression(
			ex.GetRecordTypes(), ex.GetFlowedType(),
		), nil

	case *expressions.LogicalFilterExpression:
		if len(freshChildren) != 1 {
			return nil, fmt.Errorf("LogicalFilterExpression: expected 1 child, got %d", len(freshChildren))
		}
		return expressions.NewLogicalFilterExpression(
			ex.GetPredicates(), freshChildren[0],
		), nil

	case *expressions.LogicalProjectionExpression:
		if len(freshChildren) != 1 {
			return nil, fmt.Errorf("LogicalProjectionExpression: expected 1 child, got %d", len(freshChildren))
		}
		return expressions.NewLogicalProjectionExpression(
			ex.GetProjectedValues(), freshChildren[0],
		), nil

	case *expressions.LogicalSortExpression:
		if len(freshChildren) != 1 {
			return nil, fmt.Errorf("LogicalSortExpression: expected 1 child, got %d", len(freshChildren))
		}
		return expressions.NewLogicalSortExpression(
			ex.GetSortKeys(), freshChildren[0],
		), nil

	case *expressions.LogicalDistinctExpression:
		if len(freshChildren) != 1 {
			return nil, fmt.Errorf("LogicalDistinctExpression: expected 1 child, got %d", len(freshChildren))
		}
		return expressions.NewLogicalDistinctExpression(freshChildren[0]), nil

	case *expressions.LogicalTypeFilterExpression:
		if len(freshChildren) != 1 {
			return nil, fmt.Errorf("LogicalTypeFilterExpression: expected 1 child, got %d", len(freshChildren))
		}
		return expressions.NewLogicalTypeFilterExpression(
			ex.GetRecordTypes(), freshChildren[0],
		), nil

	case *expressions.LogicalUnionExpression:
		return expressions.NewLogicalUnionExpression(freshChildren), nil

	case *expressions.LogicalIntersectionExpression:
		return expressions.NewLogicalIntersectionExpression(
			freshChildren, ex.GetComparisonKeyValues(),
		), nil

	case *expressions.SelectExpression:
		return expressions.NewSelectExpression(
			ex.GetResultValue(), freshChildren, ex.GetPredicates(),
		), nil

	case *expressions.InsertExpression:
		if len(freshChildren) != 1 {
			return nil, fmt.Errorf("InsertExpression: expected 1 child, got %d", len(freshChildren))
		}
		return expressions.NewInsertExpression(
			freshChildren[0], ex.GetTargetRecordType(), ex.GetTargetType(),
		), nil

	case *expressions.UpdateExpression:
		if len(freshChildren) != 1 {
			return nil, fmt.Errorf("UpdateExpression: expected 1 child, got %d", len(freshChildren))
		}
		return expressions.NewUpdateExpression(
			freshChildren[0], ex.GetTargetRecordType(), ex.GetTransforms(),
		), nil

	case *expressions.DeleteExpression:
		if len(freshChildren) != 1 {
			return nil, fmt.Errorf("DeleteExpression: expected 1 child, got %d", len(freshChildren))
		}
		return expressions.NewDeleteExpression(
			freshChildren[0], ex.GetTargetRecordType(),
		), nil

	default:
		return nil, fmt.Errorf("ExtractBestPlan: unsupported expression type %T (add an arm to rebuildExpression)", e)
	}
}

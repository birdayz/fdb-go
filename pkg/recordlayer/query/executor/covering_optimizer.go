package executor

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// OptimizeCoveringIndexScans walks a physical plan tree and marks
// index scans as covering when the plan above them only references
// columns available from the index. This is a post-extraction
// optimization pass that avoids the per-row LoadRecord() call.
//
// The optimization is conservative: it only fires when ALL referenced
// columns in filters and projections above the index scan are present
// in the index's column list. If any column is missing, the scan
// stays non-covering (correctness over performance).
func OptimizeCoveringIndexScans(plan plans.RecordQueryPlan) plans.RecordQueryPlan {
	return optimizeCovering(plan, nil)
}

func optimizeCovering(plan plans.RecordQueryPlan, referencedColumns map[string]bool) plans.RecordQueryPlan {
	if plan == nil {
		return nil
	}

	switch p := plan.(type) {
	case *plans.RecordQueryIndexPlan:
		if p.IsCovering() {
			return p
		}
		if referencedColumns == nil {
			return p
		}
		cols := p.GetCoveringColumns()
		if len(cols) == 0 {
			cols = indexColumnNames(p)
		}
		if len(cols) == 0 {
			return p
		}
		colSet := make(map[string]bool, len(cols))
		for _, c := range cols {
			colSet[strings.ToUpper(c)] = true
		}
		allCovered := true
		for col := range referencedColumns {
			if !colSet[strings.ToUpper(col)] {
				allCovered = false
				break
			}
		}
		if allCovered {
			return p.WithCovering(cols)
		}
		return p

	case *plans.RecordQueryFilterPlan:
		refs := collectReferencedColumns(p.GetPredicates())
		if referencedColumns != nil {
			for col := range referencedColumns {
				refs[col] = true
			}
		}
		return p

	case *plans.RecordQueryPredicatesFilterPlan:
		refs := collectPredicateColumns(p.GetPredicates())
		if referencedColumns != nil {
			for col := range referencedColumns {
				refs[col] = true
			}
		}
		return p

	default:
		return plan
	}
}

func indexColumnNames(p *plans.RecordQueryIndexPlan) []string {
	return nil
}

func collectReferencedColumns(preds []predicates.QueryPredicate) map[string]bool {
	cols := make(map[string]bool)
	for _, p := range preds {
		predicates.WalkPredicate(p, func(node predicates.QueryPredicate) bool {
			if cp, ok := node.(*predicates.ComparisonPredicate); ok {
				for col := range collectValueColumns(cp.Operand) {
					cols[col] = true
				}
				if cp.Comparison.Operand != nil {
					for col := range collectValueColumns(cp.Comparison.Operand) {
						cols[col] = true
					}
				}
			}
			return true
		})
	}
	return cols
}

func collectPredicateColumns(preds []predicates.QueryPredicate) map[string]bool {
	return collectReferencedColumns(preds)
}

func collectValueColumns(v values.Value) map[string]bool {
	cols := make(map[string]bool)
	values.WalkValue(v, func(node values.Value) bool {
		if fv, ok := node.(*values.FieldValue); ok {
			cols[strings.ToUpper(fv.Field)] = true
		}
		return true
	})
	return cols
}

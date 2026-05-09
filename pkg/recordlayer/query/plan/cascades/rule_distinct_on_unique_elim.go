package cascades

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// DistinctOnUniqueElimRule eliminates a LogicalDistinctExpression when
// the projected column set is guaranteed to be unique — i.e. it
// includes all columns of a primary key or a unique index.
//
//	Distinct(Projection([PK_COL, ...], Scan(T)))  →  Projection([PK_COL, ...], Scan(T))
//	Distinct(Scan(T))                             →  Scan(T)
//
// When SELECT DISTINCT projects a superset of a unique key, every row
// is already unique. The Distinct operator adds CPU overhead
// (hash-based or sort-based deduplication) for zero gain.
//
// The rule consults PlanContext.GetMatchCandidates() for unique-index
// candidates and PlanContext.GetPrimaryKeyColumns() for the PK.
// If any unique key's columns are fully covered by the projected
// column set (or the full row when there's no projection), the
// Distinct is eliminated.
//
// Divergence from Java: this is a logical rewrite rule that Java does
// NOT have. In Java, the equivalent optimization happens during the
// physical planning phase via ImplementDistinctRule + the
// DistinctRecordsProperty property. Java's physical ImplementDistinctRule
// checks DistinctRecordsProperty on the inner plan — if the inner
// already produces distinct records (because it scans a unique index
// covering all projected columns), the Distinct operator is elided at
// physical plan construction time.
//
// Go applies the same optimization earlier, as a logical rewrite, so
// the Distinct node is removed before physical planning. The
// optimization is semantically equivalent: in both cases, a Distinct
// over a provably-unique column set is a no-op and is eliminated. The
// difference is purely in WHEN it fires (logical phase in Go vs.
// physical phase in Java).
type DistinctOnUniqueElimRule struct {
	matcher matching.BindingMatcher
}

func NewDistinctOnUniqueElimRule() *DistinctOnUniqueElimRule {
	return &DistinctOnUniqueElimRule{
		matcher: NewExpressionMatcher[*expressions.LogicalDistinctExpression]("distinct_on_unique"),
	}
}

func (r *DistinctOnUniqueElimRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *DistinctOnUniqueElimRule) OnMatch(call *ExpressionRuleCall) {
	if call.Context == nil {
		return
	}

	d := matching.Get[*expressions.LogicalDistinctExpression](call.Bindings, r.matcher)
	innerRef := d.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	innerExpr := innerRef.Get()

	// Collect projected column names. If there's no projection, the
	// full row is projected and every column is available.
	projectedCols := collectProjectedFieldNames(innerExpr)

	// Find the record type(s) by walking down through transparent
	// operators (projection, filter, sort, etc.) to the scan leaf.
	recordTypes := findRecordTypes(innerExpr)
	if len(recordTypes) == 0 {
		return
	}

	// Check primary key columns for each record type.
	for _, rt := range recordTypes {
		pkCols := call.Context.GetPrimaryKeyColumns(rt)
		if len(pkCols) > 0 && uniqueKeysCovered(pkCols, projectedCols) {
			call.Yield(innerExpr)
			return
		}
	}

	// Check unique index candidates.
	for _, cand := range call.Context.GetMatchCandidates() {
		if !cand.IsUnique() {
			continue
		}
		cols := cand.GetColumnNames()
		if len(cols) > 0 && uniqueKeysCovered(cols, projectedCols) {
			call.Yield(innerExpr)
			return
		}
	}
}

// collectProjectedFieldNames extracts field names from a
// LogicalProjectionExpression's projected values. If the inner
// expression is not a projection, returns nil to indicate "all
// columns available" (full row).
func collectProjectedFieldNames(expr expressions.RelationalExpression) map[string]struct{} {
	proj, ok := expr.(*expressions.LogicalProjectionExpression)
	if !ok {
		// No projection — full row is available. Every column of the
		// underlying scan is projected, so any unique key is covered.
		return nil
	}

	cols := make(map[string]struct{})
	for _, v := range proj.GetProjectedValues() {
		extractFieldNames(v, cols)
	}
	return cols
}

// extractFieldNames recursively collects FieldValue.Field names from
// a Value tree.
func extractFieldNames(v values.Value, out map[string]struct{}) {
	if fv, ok := v.(*values.FieldValue); ok {
		out[fv.Field] = struct{}{}
	}
	for _, child := range v.Children() {
		extractFieldNames(child, out)
	}
}

// findRecordTypes walks down through transparent operators (projection,
// filter, sort, distinct, unique, type-filter) to find a
// FullUnorderedScanExpression and returns its record types.
func findRecordTypes(expr expressions.RelationalExpression) []string {
	switch e := expr.(type) {
	case *expressions.FullUnorderedScanExpression:
		return e.GetRecordTypes()
	case *expressions.LogicalProjectionExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalFilterExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalSortExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalDistinctExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalUniqueExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalTypeFilterExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	}
	return nil
}

func findRecordTypesViaQuantifier(q expressions.Quantifier) []string {
	ref := q.GetRangesOver()
	if ref == nil {
		return nil
	}
	return findRecordTypes(ref.Get())
}

// uniqueKeysCovered reports whether every column in uniqueKeyCols
// appears in projectedCols. If projectedCols is nil, all columns are
// considered available (no projection = full row).
func uniqueKeysCovered(uniqueKeyCols []string, projectedCols map[string]struct{}) bool {
	if projectedCols == nil {
		// No projection — full row; all columns available.
		return true
	}
	for _, col := range uniqueKeyCols {
		// Case-insensitive comparison: PK columns from metadata may
		// use a different case than the SQL-resolved field names.
		found := false
		for pc := range projectedCols {
			if strings.EqualFold(col, pc) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

var _ ExpressionRule = (*DistinctOnUniqueElimRule)(nil)

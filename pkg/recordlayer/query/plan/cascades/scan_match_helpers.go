package cascades

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// Scan/index match helpers shared by the data-access match path and the
// ordered-scan / streaming-aggregate / aggregate-data-access rules. These were
// extracted from the retired Go-only ImplementIndexScanRule (RFC-076): the rule
// is gone — scans are produced solely by the data-access/Compensation match path
// — but these pure helpers remain in use by the surviving scan-producing rules
// and the match infrastructure.

// extractIndexPlan extracts a *RecordQueryIndexPlan from a plan that may be either
// an IndexPlan directly or a FetchFromPartialRecordPlan wrapping one.
func extractIndexPlan(p plans.RecordQueryPlan) *plans.RecordQueryIndexPlan {
	if ip, ok := p.(*plans.RecordQueryIndexPlan); ok {
		return ip
	}
	if fp, ok := p.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
		if inner := fp.GetInner(); inner != nil {
			if ip, ok := inner.(*plans.RecordQueryIndexPlan); ok {
				return ip
			}
		}
	}
	return nil
}

// isScanRangeCompatible reports whether a ComparisonType can be safely pushed into
// an FDB key-range scan. Simple scalar comparisons (=, <, <=, >, >=) and STARTS_WITH
// (prefix range) map cleanly to range bounds. ComparisonIn and others must stay as
// residual predicates.
func isScanRangeCompatible(t predicates.ComparisonType) bool {
	switch t {
	case predicates.ComparisonEquals,
		predicates.ComparisonLessThan,
		predicates.ComparisonLessThanOrEq,
		predicates.ComparisonGreaterThan,
		predicates.ComparisonGreaterThanEq,
		predicates.ComparisonStartsWith:
		return true
	}
	return false
}

// comparisonTypesCompatible checks whether a field's type and the comparison's
// operand type are compatible for index pushdown. Returns false for obvious
// mismatches like BIGINT column vs STRING literal — these must remain as residual
// predicates so the executor surfaces the type error. Unknown types (from
// unresolved columns) pass through.
func comparisonTypesCompatible(fv *values.FieldValue, cmp *predicates.Comparison) bool {
	if cmp.Operand == nil {
		return true
	}
	fieldType := fv.Type()
	if fieldType == nil || fieldType == values.UnknownType {
		return true
	}
	rhsType := cmp.Operand.Type()
	if rhsType == nil || rhsType == values.UnknownType {
		return true
	}
	// Numeric ↔ String is always a type mismatch.
	fieldIsNum := isNumericType(fieldType)
	rhsIsNum := isNumericType(rhsType)
	fieldIsStr := isStringType(fieldType)
	rhsIsStr := isStringType(rhsType)
	if (fieldIsNum && rhsIsStr) || (fieldIsStr && rhsIsNum) {
		return false
	}
	return true
}

func isNumericType(t values.Type) bool {
	if pt, ok := t.(*values.PrimitiveType); ok {
		return pt.TypeCode.IsNumeric()
	}
	return false
}

func isStringType(t values.Type) bool {
	if pt, ok := t.(*values.PrimitiveType); ok {
		return pt.TypeCode == values.TypeCodeString
	}
	return false
}

// findFullScan looks for a FullUnorderedScanExpression in a Reference.
func findFullScan(ref *expressions.Reference) *expressions.FullUnorderedScanExpression {
	for _, m := range ref.Members() {
		if s, ok := m.(*expressions.FullUnorderedScanExpression); ok {
			return s
		}
	}
	return nil
}

// findFullScanThroughFilter is like findFullScan but also looks inside
// LogicalFilterExpression children. Used by AggregateDataAccessRule when
// PushFilterThroughGroupByRule wraps the Scan in a Filter.
func findFullScanThroughFilter(ref *expressions.Reference) *expressions.FullUnorderedScanExpression {
	for _, m := range ref.Members() {
		if s, ok := m.(*expressions.FullUnorderedScanExpression); ok {
			return s
		}
		if f, ok := m.(*expressions.LogicalFilterExpression); ok {
			childRef := f.GetInner().GetRangesOver()
			if childRef != nil {
				if s := findFullScanThroughFilter(childRef); s != nil {
					return s
				}
			}
		}
	}
	return nil
}

// recordTypesOverlap returns true if any element of a appears in b.
func recordTypesOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, ta := range a {
		for _, tb := range b {
			if strings.EqualFold(ta, tb) {
				return true
			}
		}
	}
	return false
}

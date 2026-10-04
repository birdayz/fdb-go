package cascades

import (
	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// columnValueProvider is implemented by match candidates whose key columns are
// not all bare fields — they supply the per-column match Value (e.g. a
// CARDINALITY()-keyed column yields CardinalityValue(FieldValue(col))). The
// base argument is the QuantifiedObjectValue of the index's record source.
// Candidates that don't implement it default to FieldValue(base, col).
type columnValueProvider interface {
	ColumnValue(i int, base values.Value) values.Value
}

// ExpandValueIndex builds a candidate's match Traversal: Java's
// ValueIndexExpansionVisitor over the index's key expression. A value index
// expands its stored root; the other index-like candidates expand their column
// list as the top-level scalar fields it names.
func ExpandValueIndex(candidate MatchCandidate) *Traversal {
	if valueCandidate, ok := candidate.(*ValueIndexScanMatchCandidate); ok {
		if expansion := valueCandidate.indexExpansion(); expansion != nil {
			return expansion.traversal
		}
		return nil
	}
	columns := candidate.GetColumnNames()
	aliases := candidate.GetSargableAliases()
	if len(columns) < len(aliases) {
		return nil
	}
	expansion, err := expandValueIndexRoot(candidate,
		flatColumnsRootKeyExpression(columns[:len(aliases)], nil), nil)
	if err != nil {
		return nil
	}
	return expansion.traversal
}

func candidateBaseType(candidate MatchCandidate) (values.Type, bool) {
	typed, ok := candidate.(interface{ GetBaseType() values.Type })
	if !ok || typed.GetBaseType() == nil {
		return nil, false
	}
	if _, err := values.SnapshotExactType(typed.GetBaseType()); err != nil {
		return nil, false
	}
	return typed.GetBaseType(), true
}

func fanOutFlowedColumns(
	quantifier expressions.Quantifier,
	flowedType values.Type,
) []GraphExpansionColumn {
	typedObject, err := values.NewQuantifiedObjectValue(quantifier.GetAlias(), flowedType)
	if err != nil {
		return nil
	}
	recordType, ok := flowedType.(*values.RecordType)
	if !ok || len(recordType.Fields) == 0 {
		return []GraphExpansionColumn{{Value: typedObject}}
	}
	columns := make([]GraphExpansionColumn, 0, len(recordType.Fields))
	for ordinal, field := range recordType.Fields {
		fieldValue, err := values.ResolveFieldOrdinals(typedObject, []int{ordinal})
		if err != nil {
			return []GraphExpansionColumn{{Value: typedObject}}
		}
		columns = append(columns, GraphExpansionColumn{
			Name:  field.Name,
			Value: fieldValue,
		})
	}
	return columns
}

func arrayElementType(typ values.Type) (values.Type, bool) {
	if arrayType, ok := typ.(*values.ArrayType); ok &&
		arrayType.ElementType != nil {
		if _, err := values.SnapshotExactType(arrayType.ElementType); err == nil {
			return arrayType.ElementType, true
		}
	}
	return nil, false
}

func appendPath(prefix []string, segment string) []string {
	path := make([]string, 0, len(prefix)+1)
	path = append(path, prefix...)
	path = append(path, segment)
	return path
}

func keyExpressionContainsFanOut(expression *gen.KeyExpression) bool {
	if expression == nil {
		return false
	}
	if expression.Field != nil &&
		expression.Field.GetFanType() == gen.Field_FAN_OUT {
		return true
	}
	if expression.Nesting != nil {
		if parent := expression.Nesting.GetParent(); parent != nil &&
			parent.GetFanType() == gen.Field_FAN_OUT {
			return true
		}
		if keyExpressionContainsFanOut(expression.Nesting.GetChild()) {
			return true
		}
	}
	if expression.Then != nil {
		for _, child := range expression.Then.GetChild() {
			if keyExpressionContainsFanOut(child) {
				return true
			}
		}
	}
	if expression.Function != nil &&
		keyExpressionContainsFanOut(expression.Function.GetArguments()) {
		return true
	}
	if expression.Grouping != nil &&
		keyExpressionContainsFanOut(expression.Grouping.GetWholeKey()) {
		return true
	}
	if expression.KeyWithValue != nil &&
		keyExpressionContainsFanOut(expression.KeyWithValue.GetInnerKey()) {
		return true
	}
	if expression.List != nil {
		for _, child := range expression.List.GetChild() {
			if keyExpressionContainsFanOut(child) {
				return true
			}
		}
	}
	if expression.Dimensions != nil &&
		keyExpressionContainsFanOut(expression.Dimensions.GetWholeKey()) {
		return true
	}
	if expression.Split != nil &&
		keyExpressionContainsFanOut(expression.Split.GetJoined()) {
		return true
	}
	return false
}

// keyExpressionTopLevelScalarFieldNames recognizes the only key-expression
// shape that can safely participate in a shortcut which compares bare column
// names instead of candidate Values: one or more top-level SCALAR fields.
// Nesting, functions, concatenation, and fan-out all require semantic or
// structural matching and therefore decline these shortcuts.
func keyExpressionTopLevelScalarFieldNames(
	expression *gen.KeyExpression,
) ([]string, bool) {
	if keyExpressionShapeCount(expression) != 1 {
		return nil, false
	}
	if expression.Field != nil {
		field := expression.Field
		if field.FieldName == nil ||
			field.FanType == nil ||
			field.GetFanType() != gen.Field_SCALAR {
			return nil, false
		}
		return []string{field.GetFieldName()}, true
	}
	if expression.Then != nil {
		var names []string
		for _, child := range expression.Then.GetChild() {
			childNames, ok := keyExpressionTopLevelScalarFieldNames(child)
			if !ok {
				return nil, false
			}
			names = append(names, childNames...)
		}
		if len(names) == 0 {
			return nil, false
		}
		return names, true
	}
	if expression.Version != nil {
		// A VERSION index's version key column behaves exactly like a
		// top-level scalar field of the pseudo-field-extended base type —
		// Java's VersionKeyExpression.toValue is
		// FieldValue.ofFieldName(base, "__ROW_VERSION")
		// (VersionKeyExpression.java:119-121).
		return []string{values.PseudoFieldRowVersion}, true
	}
	if expression.KeyWithValue != nil {
		// A covering root's PHYSICAL entry key is its inner key up to the
		// split point (then the primary key) — plain scalar key columns
		// order the scan exactly as a non-covering index's do; the VALUE
		// part rides in the FDB value and contributes no key ordering. The
		// truncation matches ColumnSize semantics
		// (KeyWithValueExpression.getColumnSize() == split point) at any
		// depth, keeping this list parallel to the column count everywhere
		// the two are compared.
		names, ok := keyExpressionTopLevelScalarFieldNames(
			expression.KeyWithValue.GetInnerKey(),
		)
		if !ok {
			return nil, false
		}
		split := int(expression.KeyWithValue.GetSplitPoint())
		if split < 0 || split > len(names) {
			return nil, false
		}
		return names[:split], true
	}
	return nil, false
}

func keyExpressionShapeCount(expression *gen.KeyExpression) int {
	if expression == nil {
		return 0
	}
	count := 0
	if expression.Then != nil {
		count++
	}
	if expression.Nesting != nil {
		count++
	}
	if expression.Field != nil {
		count++
	}
	if expression.Grouping != nil {
		count++
	}
	if expression.Empty != nil {
		count++
	}
	if expression.Split != nil {
		count++
	}
	if expression.Version != nil {
		count++
	}
	if expression.Value != nil {
		count++
	}
	if expression.Function != nil {
		count++
	}
	if expression.KeyWithValue != nil {
		count++
	}
	if expression.RecordTypeKey != nil {
		count++
	}
	if expression.List != nil {
		count++
	}
	if expression.Dimensions != nil {
		count++
	}
	return count
}

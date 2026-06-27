package recordlayer

import (
	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// ListKeyExpression combines multiple child expressions, producing nested tuples
// for each child's result (unlike Then/Concat which flattens). Each child's
// evaluation result is wrapped as a nested tuple element.
// Matches Java's com.apple.foundationdb.record.metadata.expressions.ListKeyExpression.
type ListKeyExpression struct {
	children []KeyExpression
}

// ListExpr creates a ListKeyExpression from the given children.
func ListExpr(children ...KeyExpression) *ListKeyExpression {
	return &ListKeyExpression{children: children}
}

// Evaluate computes the cross-product of all child expression results, wrapping
// each child's result as a nested tuple.Tuple. Each child's Key.Evaluated values
// become a single nested tuple element in the result.
//
// This matches Java's ListKeyExpression.evaluateMessage() + combine(), where
// childValue.toTupleAppropriateList() is added as a single List<Object> element
// that becomes a nested Tuple when serialized.
//
// IMPORTANT: Child values must be wrapped as tuple.Tuple (named type), NOT bare
// []any. The FDB tuple packer's type switch uses `case Tuple:` which only matches
// the named type. Bare []any causes a runtime panic during Pack().
func (l *ListKeyExpression) Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	if len(l.children) == 0 {
		return [][]any{{}}, nil
	}

	// Collect all children's evaluated results.
	childrenValues := make([][][]any, len(l.children))
	for i, child := range l.children {
		childVals, err := child.Evaluate(record, msg)
		if err != nil {
			return nil, err
		}
		childrenValues[i] = childVals
	}

	// Build cross-product via recursive combination.
	var results [][]any
	l.combine(&results, nil, 0, childrenValues)
	return results, nil
}

// combine builds the cross-product recursively, matching Java's
// ListKeyExpression.combine(combined, listSoFar, valuesIndex, childrenValues).
//
// At each level, we iterate over child values, wrap each as a nested tuple.Tuple
// (matching Java's childValue.toTupleAppropriateList() which becomes a nested Tuple),
// and recurse to the next child.
func (l *ListKeyExpression) combine(combined *[][]any, listSoFar []any, valuesIndex int, childrenValues [][][]any) {
	if valuesIndex == len(childrenValues) {
		// Base case: all children processed. Create a result entry.
		result := make([]any, len(listSoFar))
		copy(result, listSoFar)
		*combined = append(*combined, result)
		return
	}

	for _, childValue := range childrenValues[valuesIndex] {
		// Wrap child's evaluated values as a nested tuple.Tuple.
		// This is critical: the FDB tuple packer only recognizes the named
		// type tuple.Tuple for nested tuples, not bare []any.
		nested := wrapAsTuple(childValue)

		nextList := make([]any, len(listSoFar)+1)
		copy(nextList, listSoFar)
		nextList[len(listSoFar)] = nested

		l.combine(combined, nextList, valuesIndex+1, childrenValues)
	}
}

// wrapAsTuple converts a []any slice to a tuple.Tuple for proper FDB nested tuple
// encoding. This is necessary because the FDB tuple packer checks `case Tuple:`
// (the named type), not `case []any`.
func wrapAsTuple(values []any) tuple.Tuple {
	t := make(tuple.Tuple, len(values))
	for i, v := range values {
		t[i] = v
	}
	return t
}

// FieldNames returns all field names from child expressions.
func (l *ListKeyExpression) FieldNames() []string {
	var names []string
	for _, child := range l.children {
		names = append(names, child.FieldNames()...)
	}
	return names
}

// ColumnSize returns the number of children — each child contributes one column
// (as a nested tuple element). Matches Java's ListKeyExpression.getColumnSize().
func (l *ListKeyExpression) ColumnSize() int {
	return len(l.children)
}

// GetChildren returns the child expressions.
func (l *ListKeyExpression) GetChildren() []KeyExpression {
	return l.children
}

// ToKeyExpression serializes ListKeyExpression to proto.
// Matches Java's ListKeyExpression.toKeyExpression().
func (l *ListKeyExpression) ToKeyExpression() *gen.KeyExpression {
	children := make([]*gen.KeyExpression, len(l.children))
	for i, child := range l.children {
		children[i] = child.ToKeyExpression()
	}
	return &gen.KeyExpression{
		List: &gen.List{
			Child: children,
		},
	}
}

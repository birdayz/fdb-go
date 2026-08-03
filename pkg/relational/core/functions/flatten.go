package functions

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// FlattenRecordWithOneField is Java's
// SqlFunctionCatalog.flattenRecordWithOneField (SqlFunctionCatalog.java:98-108).
//
// WHY IT EXISTS. SQL's grammar gives `(expr)` and a one-element record literal
// the SAME parse: there is no one-tuple literal syntax to tell them apart, so
// the parser cannot decide. Java resolves the ambiguity by POSITION instead of
// by parse, and the split is the whole design:
//
//   - the record constructor ALWAYS builds a record — ExpressionVisitor's
//     visitRecordConstructor (ExpressionVisitor.java:918-925) goes straight to
//     RecordConstructorValue.ofColumns with no one-element unwrap, which is why
//     `SELECT (1 + 2)` is a one-field STRUCT and `SELECT ((1 + 2))` nests twice;
//   - every FUNCTION ARGUMENT is then flattened back down by this function, which
//     is why `(3 + 4) * 5` multiplies by the scalar 7 and not by a record. Java
//     states the precedence outright: when a value can be read as a single-item
//     record or as the value itself, "the precedence is always given to the
//     latter" (SqlFunctionCatalog.java:88-92).
//
// So a parenthesised scalar survives as a record only where nothing consumes it
// as an argument — projection position. That is not a quirk to be normalised
// away; it is the observable difference between the two positions.
//
// WHERE IT APPLIES. Java hangs it off resolveScalarFunction's argument mapping
// (SemanticAnalyzer.java:991-994, :1109, :1121, :1166), and BaseVisitor's
// resolveFunction (BaseVisitor.java:253-261) defaults flattenSingleItemRecords
// to true. Because ExpressionVisitor routes arithmetic (:731), comparison
// (:699), bitwise (:691), logical (:509), NOT (:501), IS NULL (:581), LIKE
// (:615), IN (:627) and BETWEEN (:716-722) all through resolveFunction, every
// one of those operand positions flattens.
//
// THE ONE OPT-OUT is flattenSingleItemRecords=false, passed only to the
// `__internal_array` that builds the row list of the VALUES and inline-table
// paths (QueryVisitor.java:720 and :802). There a one-column row MUST stay a
// row: flattening it would turn `VALUES (1), (2)` into a bare element list and
// destroy the row structure the explode depends on. Callers on that path must
// therefore NOT call this function.
//
// The recursion has two arms, exactly as Java does: a one-field record collapses
// to its single element and is re-examined (so nested parens peel completely),
// and any other node rebuilds itself with flattened children (so a record buried
// under an array or a cast is still reached).
func FlattenRecordWithOneField(v values.Value) values.Value {
	if v == nil {
		return nil
	}
	if rc, ok := v.(*values.RecordConstructorValue); ok && len(rc.Fields) == 1 {
		return FlattenRecordWithOneField(rc.Fields[0].Value)
	}
	children := v.Children()
	if len(children) == 0 {
		return v
	}
	newChildren := make([]values.Value, len(children))
	changed := false
	for i, c := range children {
		newChildren[i] = FlattenRecordWithOneField(c)
		if newChildren[i] != c {
			changed = true
		}
	}
	// Returning v untouched when nothing moved is not just an allocation
	// saving: values.WithChildren returns the node with its ORIGINAL children
	// for any concrete type its switch does not recognise, so rebuilding
	// unconditionally would silently drop nothing here but would make the
	// no-op case depend on that switch's completeness.
	if !changed {
		return v
	}
	return values.WithChildren(v, newChildren)
}

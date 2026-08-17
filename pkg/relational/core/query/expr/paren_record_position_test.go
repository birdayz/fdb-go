package expr_test

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/expr"
)

// A parenthesised scalar means DIFFERENT things in different positions, and
// the whole point of this file is that the difference is real rather than an
// inconsistency to be normalised away.
//
// SQL's grammar gives `(expr)` and a one-element record literal the same
// parse — there is no one-tuple syntax to separate them — so Java resolves the
// ambiguity by POSITION. The record constructor always builds a record
// (ExpressionVisitor.java:918-925 has no one-element unwrap) and every
// FUNCTION ARGUMENT is flattened back to a scalar
// (SqlFunctionCatalog.flattenRecordWithOneField, applied at
// SemanticAnalyzer.java:991-994 with BaseVisitor.java:253-261 defaulting the
// flag to true). Projection position consumes nothing as an argument, so the
// record survives there and only there.
//
// All four shapes below were MEASURED against the live JVM; the Java side is
// pinned in conformance/paren_star_java_probe_test.go, and this file pins the
// Go side of the same four so the two cannot drift apart silently.
//
// The two halves are independently breakable, which is why neither half alone
// is sufficient coverage: restoring the constructor's one-element unwrap
// re-scalarises the first three, and dropping the operand flatten turns the
// last two into record-vs-scalar shapes Java never produces.

// TestParenScalar_ProjectionPosition_IsAOneFieldRecord pins the constructor
// half: with no argument position to flatten it, `(1 + 2)` stays a record.
func TestParenScalar_ProjectionPosition_IsAOneFieldRecord(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE (1 + 2)")

	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	rc, ok := v.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("`(1 + 2)` outside an argument position is a ONE-FIELD RECORD "+
			"(live JVM: STRUCT {_0: 3}); got %T. A one-element unwrap in the "+
			"record constructor is what re-scalarises it.", v)
	}
	if len(rc.Fields) != 1 {
		t.Fatalf("field count: got %d, want 1", len(rc.Fields))
	}
	if want := values.OrdinalFieldName(0); rc.Fields[0].Name != want {
		t.Fatalf("a nameless element takes the ordinal key: got %q, want %q",
			rc.Fields[0].Name, want)
	}
	if _, isRecord := rc.Fields[0].Value.(*values.RecordConstructorValue); isRecord {
		t.Fatal("the arithmetic must sit directly inside the record, not inside a second one")
	}
}

// TestParenScalar_DoubleParens_NestTwice pins that the constructor arm applies
// per parenthesis rather than collapsing a chain: the live JVM answers
// `{_0: {_0: 3}}` for `((1 + 2))`. A unwrap-once implementation passes the
// single-paren test above and still fails here, which is why this shape is
// pinned separately.
func TestParenScalar_DoubleParens_NestTwice(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE ((1 + 2))")

	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	outer, ok := v.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("expected the OUTER record, got %T", v)
	}
	if len(outer.Fields) != 1 {
		t.Fatalf("outer field count: got %d, want 1", len(outer.Fields))
	}
	inner, ok := outer.Fields[0].Value.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("each paren contributes its own record level (live JVM: "+
			"{_0: {_0: 3}}); inner was %T", outer.Fields[0].Value)
	}
	if len(inner.Fields) != 1 {
		t.Fatalf("inner field count: got %d, want 1", len(inner.Fields))
	}
}

// TestParenColumn_IsARecordButKeyedByOrdinal pins the record-vs-scalar result
// for a parenthesised COLUMN — the shape that re-types most broadly — and, on
// the naming axis, pins a KNOWN REMAINING DIVERGENCE rather than leaving it
// unmeasured.
//
// Go keys the field by ordinal (`{_0: 10}`); the live JVM answers `{VAL: 10}`,
// because Java takes an unnamed element's own inherent name
// (Expressions.underlyingAsColumns, Expressions.java:269-288).
//
// Java can afford that only because a record built where a TARGET TYPE is in
// scope never keeps those names — parseRecordFieldsUnderReorderings
// (ExpressionVisitor.java:1040-1083) overwrites them with the target's fields
// BY POSITION. Go has no target type at construction and defers that binding
// to values.BuildStructMessage, which receives an ORDER-LESS map[string]any
// and can recover position only from the ordinal names. So renaming here in
// isolation is not a smaller version of the fix, it is a broken one: it was
// MEASURED to turn `update B set b3 = coalesce(b3, (b1, b2), ...)` into
// `record constructor for "S" carries 2 fields, 0 of which the target struct
// declares`, because `(b1, b2)` then arrives named B1/B2 and matches nothing
// in S.
//
// Closing the divergence means porting Java's construction-time target binding
// (or making the coercion order-preserving). This assertion is what makes the
// gap visible: change the naming without that, and this test names the reason.
func TestParenColumn_IsARecordButKeyedByOrdinal(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE (id)")

	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	rc, ok := v.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("`(id)` in projection position is a one-field record, got %T", v)
	}
	if len(rc.Fields) != 1 {
		t.Fatalf("field count: got %d, want 1", len(rc.Fields))
	}
	if want := values.OrdinalFieldName(0); rc.Fields[0].Name != want {
		t.Fatalf("field key: got %q, want %q. Java answers \"ID\" here; moving Go "+
			"to the inherent name requires the construction-time target binding "+
			"first, or the struct coercion loses the position it binds by.",
			rc.Fields[0].Name, want)
	}
}

// TestParenColumn_ArithmeticOperand_StaysScalar pins the flatten half. This is
// the constraint that makes "just delete the unwrap" wrong: the SAME parse
// that yields a record in projection position must yield a plain scalar here,
// because arithmetic routes through resolveFunction and its arguments flatten
// (ExpressionVisitor.java:731). Live JVM: `SELECT (val) + 1` is BIGINT 11.
func TestParenColumn_ArithmeticOperand_StaysScalar(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE (id) + 1")

	v, err := r.WalkExpression(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if rc, isRecord := v.(*values.RecordConstructorValue); isRecord {
		t.Fatalf("an arithmetic operand FLATTENS its one-field record: the result "+
			"must be the arithmetic itself, not a record of %d field(s)", len(rc.Fields))
	}
	// The flatten has to reach the operand, not merely leave the top level
	// alone: the surviving left operand is the bare column.
	var sawBareColumn bool
	for _, c := range v.Children() {
		if fv, isField := values.AsFieldValue(c); isField && fv.DisplayName() == "ID" {
			sawBareColumn = true
		}
		if _, isRecord := c.(*values.RecordConstructorValue); isRecord {
			t.Fatal("the one-field record survived as an ARGUMENT; the flatten did not reach it")
		}
	}
	if !sawBareColumn {
		t.Fatalf("expected the flattened column as an operand of %T", v)
	}
}

// TestParenColumn_ComparisonOperand_StaysScalar is the predicate half of the
// same constraint — comparison flattens its operands too
// (ExpressionVisitor.java:699), so `WHERE (val) = 10` is an ordinary scalar
// comparison that MATCHES rather than a record-vs-scalar shape. Pinned
// separately from the arithmetic case because the two reach the flatten
// through different walks (walkAtom vs walkOperand), so one can regress while
// the other holds.
func TestParenColumn_ComparisonOperand_StaysScalar(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE (id) = 1")

	pred, err := r.WalkPredicate(ctx)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	cp, ok := pred.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected a plain *ComparisonPredicate, got %T", pred)
	}
	fv, ok := values.AsFieldValue(cp.Operand)
	if !ok {
		t.Fatalf("the compared operand is the FLATTENED column, not a one-field "+
			"record; got %T", cp.Operand)
	}
	if fv.DisplayName() != "ID" {
		t.Fatalf("operand column: got %q, want ID", fv.DisplayName())
	}
}

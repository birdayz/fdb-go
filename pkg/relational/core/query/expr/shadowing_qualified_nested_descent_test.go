package expr_test

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
)

// TestShadowingQualifiedResolvesTheLeafNotTheStructRoot drives the ONE arm of
// ResolveColumnShadowingQualified that a struct descent can reach: the call with
// a NON-ZERO qualifier.
//
// It exists because that arm is unreachable from SQL today and would otherwise
// ship untested. Of the four callers of the helper, three pass a zero qualifier
// (plan_visitor.go:681, :1997, logical_predicate.go:2888) and the bare arm never
// descends — Java's rule, not an omission: lookupNestedField returns empty for a
// one-segment identifier (SemanticAnalyzer.java:557-559). The only qualified
// entry is the GROUP-BY key path (logical_predicate.go:4713), and a nested
// grouping key is refused upstream with 0AF00 before it gets there. That refusal
// is pinned in the driver suite, so the two facts are recorded together: what
// makes this arm unreachable, and what it does when reached.
//
// Driving it here rather than waiting for SQL to reach it is the point. An
// untested branch's first firing reads as a FINDING rather than as a branch
// nobody exercised, and this one is a wrong-column read when wrong: minting the
// root alone returns the whole struct where a member was named, with no error.
func TestShadowingQualifiedResolvesTheLeafNotTheStructRoot(t *testing.T) {
	t.Parallel()

	// The virtual one-column table a lateral unnest binding registers (RFC-142),
	// with a STRUCT element: `FROM t, t.arr AS e` where ARR's element is a
	// struct. The binding column E carries the element's declared fields, which
	// is what makes `e.co` resolve by DESCENT rather than as a source column.
	elem := &semantic.StaticTable{
		TableName: semantic.FromSegments([]string{"E"}, false),
		TableColumns: []semantic.Column{{
			Id: semantic.NewUnquoted("e"), Type: "RECORD", Nullable: true,
			StructFields: []semantic.Column{
				{Id: semantic.NewUnquoted("sk"), Type: "INT"},
				{Id: semantic.NewUnquoted("co"), Type: "INT"},
			},
		}},
	}
	other := &semantic.StaticTable{
		TableName:    semantic.ParseQualifiedName("OTHER", false),
		TableColumns: []semantic.Column{{Id: semantic.NewUnquoted("id"), Type: "INT"}},
	}
	a := semantic.NewAnalyzer(semantic.NewInMemoryCatalog(elem, other), false)
	s := semantic.NewScope(nil)
	if err := s.AddSource(semantic.ScopeSource{
		Table: other, Alias: semantic.NewUnquoted("o"), CorrelationName: "O",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSource(semantic.ScopeSource{
		Table: elem, Alias: semantic.NewUnquoted("e"), CorrelationName: "E", Shadowing: true,
	}); err != nil {
		t.Fatal(err)
	}
	r := expr.New(a, s)

	// PRECONDITION, pinned rather than assumed: the semantic layer really does
	// resolve `E.CO` as a DESCENT. If it ever resolves as a direct source column
	// instead, every assertion below still passes — for the uninteresting reason
	// that there was no chain to fuse — and this test would silently stop
	// probing the fuse it was written for.
	descends, derr := r.DescendsIntoStruct(semantic.NewUnquoted("e"), semantic.NewUnquoted("co"))
	if derr != nil {
		t.Fatalf("DescendsIntoStruct(E.CO): %v", derr)
	}
	if !descends {
		t.Fatal("E.CO no longer resolves as a struct descent, so the fuse under test " +
			"is not exercised by this scope. Rebuild the fixture until it descends; " +
			"do not delete the assertions below.")
	}

	v, ok, err := r.ResolveColumnShadowingQualified(
		semantic.NewUnquoted("e"), semantic.NewUnquoted("co"))
	if err != nil {
		t.Fatalf("ResolveColumnShadowingQualified(E.CO): %v", err)
	}
	if !ok {
		t.Fatal("ResolveColumnShadowingQualified declined a SHADOWING source on the " +
			"QUALIFIED arm. The helper is shadowing-only by definition, so a decline " +
			"here means this test no longer reaches the mint it was written for.")
	}

	fv, isField := v.(*values.FieldValue)
	if !isField {
		t.Fatalf("got %T, want *values.FieldValue", v)
	}
	if fv.Resolved == nil {
		t.Fatal("the reference carries no resolved path")
	}

	// THE ASSERTION THAT MATTERS: two accessors, root then leaf. One accessor
	// means the descent was dropped and the reference denotes the whole struct.
	if got := len(fv.Resolved.Accessors); got != 2 {
		t.Fatalf("resolved path has %d accessor(s) %v, want 2 (root E, then the "+
			"descent to CO).\n"+
			"  ONE accessor is the wrong-column read this test exists for: the "+
			"reference then denotes the whole struct E where the member E.CO was "+
			"named, and nothing reports it — a client scanning into an untyped "+
			"destination receives the struct silently.\n"+
			"  Fix the mint to route through fuseNestedAccessorsIfAny, as Java's "+
			"lookupNestedField fuses before any caller sees the value "+
			"(SemanticAnalyzer.java:599-600). Do not relax this count.",
			got, fv.Resolved.Accessors)
	}
	if fv.Resolved.Accessors[0].Field != "E" {
		t.Fatalf("root accessor is %q, want E — the root reference must stay exactly "+
			"what the struct COLUMN alone would have produced, so the descent is "+
			"purely additive", fv.Resolved.Accessors[0].Field)
	}
	leaf := fv.Resolved.Accessors[1]
	if leaf.Field != "CO" {
		t.Fatalf("leaf accessor names %q, want CO", leaf.Field)
	}
	// Ordinal 1 is CO's position in the element struct's DECLARED field list
	// (sk=0, co=1). Ordinal 0 would be SK — the right struct, the wrong member,
	// which is a wrong-column read the name alone cannot catch.
	if leaf.Ordinal != 1 {
		t.Fatalf("leaf accessor ordinal is %d, want 1 (CO is the SECOND declared "+
			"field of the element struct; 0 is SK)", leaf.Ordinal)
	}

	// The value denotes the LEAF, so it states the leaf's type — not RECORD.
	// Java sets exactly this: `new Expression(requestedIdentifier, type, …)`
	// with `type` walked down to the leaf (SemanticAnalyzer.java:600).
	if fv.Typ == nil || fv.Typ.Code() == values.TypeCodeRecord {
		t.Fatalf("the reference states type %v; a descent denotes its LEAF, so a "+
			"RECORD type here means the value still describes the struct it "+
			"descended through", fv.Typ)
	}

	// The correlation is untouched by the descent: the fuse adds a suffix, it
	// does not re-decide which quantifier the reference reads from.
	qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
	if !isQOV {
		t.Fatalf("reference is not QOV-correlated (child %T) — the fuse must keep the "+
			"root's correlation, since the accessor chain says nothing about which "+
			"quantifier is read", fv.Child)
	}
	if qov.Correlation.Name() != "E" {
		t.Fatalf("reference binds quantifier %q, want E", qov.Correlation.Name())
	}
}

// TestShadowingQualifiedDirectColumnKeepsOneAccessor is the mirror control: the
// SAME qualified arm, over a shadowing source whose binding column is NOT a
// struct, must be unchanged by the fuse.
//
// Without it, the test above is satisfied by a mint that appends an accessor
// unconditionally — which would turn every ordinary shadowing read into a
// descent into a column that has no fields.
func TestShadowingQualifiedDirectColumnKeepsOneAccessor(t *testing.T) {
	t.Parallel()

	elem := &semantic.StaticTable{
		TableName:    semantic.FromSegments([]string{"X"}, false),
		TableColumns: []semantic.Column{{Id: semantic.NewUnquoted("x"), Type: "INT", Nullable: true}},
	}
	other := &semantic.StaticTable{
		TableName:    semantic.ParseQualifiedName("OTHER", false),
		TableColumns: []semantic.Column{{Id: semantic.NewUnquoted("id"), Type: "INT"}},
	}
	a := semantic.NewAnalyzer(semantic.NewInMemoryCatalog(elem, other), false)
	s := semantic.NewScope(nil)
	if err := s.AddSource(semantic.ScopeSource{
		Table: other, Alias: semantic.NewUnquoted("o"), CorrelationName: "O",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSource(semantic.ScopeSource{
		Table: elem, Alias: semantic.NewUnquoted("x"), CorrelationName: "X", Shadowing: true,
	}); err != nil {
		t.Fatal(err)
	}
	r := expr.New(a, s)

	v, ok, err := r.ResolveColumnShadowingQualified(semantic.Identifier{}, semantic.NewUnquoted("x"))
	if err != nil {
		t.Fatalf("ResolveColumnShadowingQualified(X): %v", err)
	}
	if !ok {
		t.Fatal("declined a shadowing source on the bare arm")
	}
	fv, isField := v.(*values.FieldValue)
	if !isField || fv.Resolved == nil {
		t.Fatalf("got %T with no resolved path, want a resolved *values.FieldValue", v)
	}
	if got := len(fv.Resolved.Accessors); got != 1 {
		t.Fatalf("a DIRECT shadowing column resolved to %d accessors %v, want 1: the "+
			"fuse must be identity on an empty chain, or every ordinary reference "+
			"becomes a descent", got, fv.Resolved.Accessors)
	}
}

package expr_test

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
)

// TestFusedNestedReferenceCanBeChildless pins the structural fact that makes
// "a consumer may key on fv.Field" a CLASS of defect rather than a site of one.
//
// `fv.Child == nil` is read across this tree as "this is a flat reference" —
// often written that way in prose, and used as the gate that lets a name-keyed
// lookup proceed. It is not an arity gate and never was. The source-relative
// mint (expr.sourceRelativeColumnRef) builds its root with
// NewFieldValueWithResolvedOrdinalInDomain, which sets no Child at all because a
// reference needing no correlation has no quantifier object to hang one on;
// fuseNestedAccessors then copies that node and appends the descent to Resolved.
// The result is a reference of arbitrary depth with a nil Child.
//
// So childlessness answers a question about CORRELATION, and arity answers a
// question about how many segments the reference spelled. A consumer that wants
// the second must test `len(Resolved.Accessors)`; testing Child instead admits
// exactly the values it meant to exclude, and admits them silently — the leaf
// name it then looks up lives in the enclosing STRUCT's namespace while the map
// it looks into is keyed by the RECORD's, so a spelling shared between the two
// resolves to a different column of a different type with no error anywhere.
//
// The counterpart arm is asserted alongside it: a CORRELATED nested reference
// does carry a QuantifiedObjectValue child, so this test cannot be satisfied by
// a mint that simply stopped attaching children.
func TestFusedNestedReferenceCanBeChildless(t *testing.T) {
	t.Parallel()

	tbl := &semantic.StaticTable{
		TableName: semantic.ParseQualifiedName("T", false),
		TableColumns: []semantic.Column{
			{Id: semantic.NewUnquoted("id"), Type: "INT"},
			{
				Id: semantic.NewUnquoted("n"), Type: "RECORD", Nullable: true,
				StructFields: []semantic.Column{
					{Id: semantic.NewUnquoted("sk"), Type: "INT"},
					{Id: semantic.NewUnquoted("co"), Type: "STRING"},
				},
			},
		},
	}
	a := semantic.NewAnalyzer(semantic.NewInMemoryCatalog(tbl), false)
	s := semantic.NewScope(nil)
	if err := s.AddSource(semantic.ScopeSource{
		Table: tbl, Alias: semantic.NewUnquoted("t"), CorrelationName: "T",
	}); err != nil {
		t.Fatal(err)
	}
	r := expr.New(a, s)

	v, err := r.ResolveIdentifierPath([]semantic.Identifier{
		semantic.NewUnquoted("n"), semantic.NewUnquoted("sk"),
	})
	if err != nil {
		t.Fatalf("resolve n.sk: %v", err)
	}
	fv, ok := v.(*values.FieldValue)
	if !ok {
		t.Fatalf("n.sk resolved to %T, want *values.FieldValue", v)
	}
	if fv.Resolved == nil || len(fv.Resolved.Accessors) != 2 {
		t.Fatalf("n.sk resolved with %v accessors, want the 2-step path [N SK] — "+
			"without a genuinely fused value this test asserts nothing",
			fv.Resolved)
	}
	if fv.Child != nil {
		t.Fatalf("n.sk resolved with a %T child. This test exists to pin that a fused "+
			"reference CAN be childless; if the source-relative mint now always attaches a "+
			"child, the `Child == nil` gates this test warns about have become sound and "+
			"the consumers keyed on them should be revisited deliberately rather than "+
			"left resting on an accident.", fv.Child)
	}

	// The whole point, stated as the assertion a consumer would have to make.
	// A gate written as `fv.Child == nil` admits this value; a gate written on
	// the accessor count does not.
	if fv.Child == nil && len(fv.Resolved.Accessors) <= 1 {
		t.Fatal("unreachable given the checks above; kept so the invariant is spelled out")
	}
	t.Logf("childless fused reference: Field=%q accessors=%d — `Child == nil` is a "+
		"statement about correlation, not about arity",
		fv.Field, len(fv.Resolved.Accessors))

	// The control: a CORRELATED nested reference does carry a QOV child, so the
	// finding above is specific to the source-relative mint rather than a claim
	// that children stopped being attached anywhere.
	inner := &semantic.StaticTable{
		TableName:    semantic.ParseQualifiedName("U", false),
		TableColumns: []semantic.Column{{Id: semantic.NewUnquoted("uid"), Type: "INT"}},
	}
	outerScope := semantic.NewScope(nil)
	if err := outerScope.AddSource(semantic.ScopeSource{
		Table: tbl, Alias: semantic.NewUnquoted("t"), CorrelationName: "T",
	}); err != nil {
		t.Fatal(err)
	}
	innerScope := semantic.NewScope(outerScope)
	if err := innerScope.AddSource(semantic.ScopeSource{
		Table: inner, Alias: semantic.NewUnquoted("u"), CorrelationName: "U",
	}); err != nil {
		t.Fatal(err)
	}
	ra := semantic.NewAnalyzer(semantic.NewInMemoryCatalog(tbl, inner), false)
	cv, err := expr.New(ra, innerScope).ResolveIdentifierPath([]semantic.Identifier{
		semantic.NewUnquoted("t"), semantic.NewUnquoted("n"), semantic.NewUnquoted("sk"),
	})
	if err != nil {
		t.Fatalf("resolve correlated t.n.sk: %v", err)
	}
	cfv, ok := cv.(*values.FieldValue)
	if !ok {
		t.Fatalf("correlated t.n.sk resolved to %T, want *values.FieldValue", cv)
	}
	if cfv.Resolved == nil || len(cfv.Resolved.Accessors) != 2 {
		t.Fatalf("correlated t.n.sk resolved with %v accessors, want 2", cfv.Resolved)
	}
	if _, isQOV := cfv.Child.(*values.QuantifiedObjectValue); !isQOV {
		t.Errorf("correlated t.n.sk has child %T, want *values.QuantifiedObjectValue — "+
			"the control arm: both childless AND childful fused references exist, so a "+
			"consumer can rely on neither shape to tell it the arity.", cfv.Child)
	}
}

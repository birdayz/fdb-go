package expr_test

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
)

// TestCorrelatedColumnRefFusesOnBothFlowedTypeArms drives the ONE correlated
// column-reference mint over BOTH branches of the flowed-type decision and
// asserts the descent survives on each.
//
// WHY THIS IS A DIMENSION AND NOT A DUPLICATE. Before the mints were unified,
// "the projection path fuses" and "the shadowing path fuses" were two
// independent facts about two bodies, and each had its own test. They are now
// ONE fact, which is the point of the unification — but it also means the
// remaining risk changed shape. The single mint still contains a branch:
// flowedTypeFor answers UNKNOWN for a shadowing source and the declared ROW for
// every other correlated source. Nothing structurally prevents a future edit
// from making the fuse conditional on that branch — an `if src.Shadowing`
// wrapped around the wrong statement fuses one arm and drops the other, which
// is the original wrong-column read restored on exactly half the inputs and is
// the hardest version to notice.
//
// So the two arms are driven through the SAME mint here, and the assertion on
// each is the same: the reference denotes the LEAF. The flowed types are
// asserted to actually DIFFER between the arms, because if they ever coincide
// this test silently stops covering two branches and becomes one case run twice.
func TestCorrelatedColumnRefFusesOnBothFlowedTypeArms(t *testing.T) {
	t.Parallel()

	structCol := func(name string) semantic.Column {
		return semantic.Column{
			Id: semantic.NewUnquoted(name), Type: "RECORD", Nullable: true,
			StructFields: []semantic.Column{
				{Id: semantic.NewUnquoted("sk"), Type: "INT"},
				{Id: semantic.NewUnquoted("co"), Type: "INT"},
			},
		}
	}

	// Asserts the resolved reference descended to CO rather than stopping at the
	// struct root. Shared by both arms verbatim: the whole claim is that the two
	// arms are indistinguishable here.
	assertLeaf := func(t *testing.T, arm string, v values.Value) values.Type {
		t.Helper()
		fv, isField := v.(*values.FieldValue)
		if !isField {
			t.Fatalf("%s: got %T, want *values.FieldValue", arm, v)
		}
		if fv.Resolved == nil {
			t.Fatalf("%s: the reference carries no resolved path", arm)
		}
		if got := len(fv.Resolved.Accessors); got != 2 {
			t.Fatalf("%s: resolved path has %d accessor(s) %v, want 2 (the struct "+
				"root, then the descent to CO).\n"+
				"  ONE accessor means this arm dropped the descent and the reference "+
				"now denotes the WHOLE STRUCT where the member CO was named. That is "+
				"silent — no type error, no runtime error, invisible to a client "+
				"scanning into an untyped destination.\n"+
				"  If the OTHER arm of this test still passes, the fuse has been made "+
				"conditional on the flowed-type branch. It must not be: the descent is "+
				"not a property of whether the source shadows. Do not relax this count.",
				arm, got, fv.Resolved.Accessors)
		}
		if leaf := fv.Resolved.Accessors[1]; leaf.Field != "CO" || leaf.Ordinal != 1 {
			t.Fatalf("%s: leaf accessor is %q at ordinal %d, want CO at 1 (CO is the "+
				"SECOND declared field of the struct; ordinal 0 is SK — the right "+
				"struct, the wrong member)", arm, leaf.Field, leaf.Ordinal)
		}
		qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
		if !isQOV {
			t.Fatalf("%s: reference child is %T, want *values.QuantifiedObjectValue — "+
				"a correlated reference must carry the quantifier object whose type "+
				"states the row it flows", arm, fv.Child)
		}
		return qov.Typ
	}

	// ARM 1 — a SHADOWING source (a lateral unnest's binding, RFC-142), reached
	// through ResolveColumnShadowingQualified. flowedTypeFor answers UNKNOWN
	// here: the element is a scalar, never the virtual one-column row that made
	// its name resolve.
	t.Run("shadowing source", func(t *testing.T) {
		t.Parallel()
		elem := &semantic.StaticTable{
			TableName:    semantic.FromSegments([]string{"E"}, false),
			TableColumns: []semantic.Column{structCol("e")},
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

		v, ok, err := r.ResolveColumnShadowingQualified(
			semantic.NewUnquoted("e"), semantic.NewUnquoted("co"))
		if err != nil {
			t.Fatalf("ResolveColumnShadowingQualified(E.CO): %v", err)
		}
		if !ok {
			t.Fatal("the shadowing mint declined a SHADOWING source, so this arm no " +
				"longer reaches the mint under test")
		}
		shadowingFlowed := assertLeaf(t, "shadowing", v)
		if shadowingFlowed != values.UnknownType {
			t.Errorf("shadowing source flows %v, want UnknownType.\n"+
				"  A shadowing binding's element is a scalar; stating the virtual "+
				"one-column row instead makes values.IsMixedSeedElementType read the "+
				"element as a join leg. The decision belongs to expr.flowedTypeFor.",
				shadowingFlowed)
		}
	})

	// ARM 2 — a NON-shadowing correlated source, reached through the projection
	// mint on a DUPLICATE FROM alias (the shape that forces a per-binding bake).
	// flowedTypeFor answers the DECLARED ROW here, which is the branch the
	// shadowing arm above can never take.
	t.Run("non-shadowing correlated source", func(t *testing.T) {
		t.Parallel()
		t1 := &semantic.StaticTable{
			TableName:    semantic.ParseQualifiedName("T1", false),
			TableColumns: []semantic.Column{structCol("n")},
		}
		t2 := &semantic.StaticTable{
			TableName:    semantic.ParseQualifiedName("T2", false),
			TableColumns: []semantic.Column{structCol("m")},
		}
		a := semantic.NewAnalyzer(semantic.NewInMemoryCatalog(t1, t2), false)
		s := semantic.NewScope(nil)
		// Both legs bind the SAME alias `a`; the duplicate-FROM-alias mint gives
		// the later leg a distinct correlation. That duplication is what makes
		// QualifierIsDuplicated true and drives the projection mint past its
		// "ordinary emission owns it" early return.
		//
		// The legs carry DIFFERENTLY NAMED struct columns (N and M) on purpose.
		// The alias must be duplicated but the ATTRIBUTE must not be: two legs
		// both offering `N` under alias `A` is Java's 42702, and the mint is
		// never reached because the resolution fails first with `ambiguous
		// column A.N.CO (matched by: A, A)`. Duplicate ALIAS, unique ATTRIBUTE
		// is the shape that reaches this mint in real SQL.
		if err := s.AddSource(semantic.ScopeSource{
			Table: t1, Alias: semantic.NewUnquoted("a"), CorrelationName: "A",
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.AddSource(semantic.ScopeSource{
			Table: t2, Alias: semantic.NewUnquoted("a"), CorrelationName: "A_1",
		}); err != nil {
			t.Fatal(err)
		}
		r := expr.New(a, s)

		v, err := r.ResolveQualifiedProjectionPath([]semantic.Identifier{
			semantic.NewUnquoted("a"), semantic.NewUnquoted("n"), semantic.NewUnquoted("co"),
		})
		if err != nil {
			t.Fatalf("ResolveQualifiedProjectionPath(A.N.CO): %v", err)
		}
		if v == nil {
			t.Fatal("the projection mint returned nil, so this arm does not reach the " +
				"mint under test. It is reached only for a reference the ordinary " +
				"alias-keyed emission cannot serve — rebuild the fixture until the " +
				"qualifier is duplicated; do not delete the assertions below.")
		}
		flowed := assertLeaf(t, "non-shadowing", v)
		if flowed == values.UnknownType {
			t.Errorf("non-shadowing correlated source flows UnknownType, want the " +
				"source's declared ROW.\n" +
				"  A non-shadowing quantifier CARRIES the row it flows " +
				"(Quantifier.java:801-803). Flowing UNKNOWN here makes every consumer " +
				"deriving a frontier from this child decline the reference.")
		}
		if _, isRow := flowed.(*values.RecordType); !isRow {
			t.Errorf("non-shadowing source flows %T, want *values.RecordType", flowed)
		}
	})
}

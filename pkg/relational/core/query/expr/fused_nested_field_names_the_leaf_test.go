package expr_test

import (
	"testing"

	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
)

// TestFusedNestedReferenceIsNamedAfterItsLeaf pins the ONE property on which the
// SQL resolver's fused-nested mint and the planner's own mints of the same shape
// have to agree: the display Field of a fused FieldValue is the LAST accessor.
//
// Java has no other answer available. Its fused FieldValue carries a FieldPath
// and no root name at all, and the single-name question askable of it is
// getLastFieldName (FieldValue.java:134-135, delegating to
// FieldValue.FieldPath.getLastFieldName at :463-466, which returns
// getOptionalFieldNames().get(size()-1)). getFieldPrefix (:450-454) is the
// complement — everything BUT the last — which is what makes "last" the
// distinguished accessor rather than an arbitrary pick.
//
// The resolver used to copy the root node whole, so a resolver-minted `n.sk`
// carried Field="N" while composeFieldOverField, the rebase/withChildren fuse
// arms, select-merge and the replace walk all minted Field="SK" for the same
// shape. Consumers reading Field therefore got different answers depending on
// which mint produced the value. This asserts the resolver's side; the
// simplifier's side is asserted where it is minted.
//
// It is a unit test on purpose even though an FDB test already covers the
// user-visible consequence: this is the invariant, and it should be able to
// break without needing a metadata derivation to notice on its behalf.
func TestFusedNestedReferenceIsNamedAfterItsLeaf(t *testing.T) {
	t.Parallel()

	// N is the struct root; SK and CO are its members. Both members are asserted
	// so a mint that returned some FIXED name (the root, or accessors[0]) cannot
	// satisfy the test by accident — the two references share a root and differ
	// only in their leaf.
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

	seg := func(names ...string) []semantic.Identifier {
		out := make([]semantic.Identifier, len(names))
		for i, n := range names {
			out[i] = semantic.NewUnquoted(n)
		}
		return out
	}

	for _, tc := range []struct {
		name     string
		path     []semantic.Identifier
		wantLeaf string
		wantAccs []string
	}{
		{"two segments", seg("n", "sk"), "SK", []string{"N", "SK"}},
		{"two segments, sibling leaf", seg("n", "co"), "CO", []string{"N", "CO"}},
		{"three segments, alias-qualified", seg("t", "n", "sk"), "SK", []string{"N", "SK"}},
	} {
		v, err := r.ResolveIdentifierPath(tc.path)
		if err != nil {
			t.Fatalf("%s: resolve: %v", tc.name, err)
		}
		fv := mustExprField(t, v)
		if fv.Path() == nil {
			t.Fatalf("%s: exact FieldValue has no resolved path", tc.name)
		}

		// Anti-vacuity: the assertion below is about a FUSED value, so prove the
		// value is fused before reading its name. A single-accessor value would
		// pass the name check trivially and prove nothing about the fuse.
		got := make([]string, fv.Path().Len())
		for i := range got {
			got[i] = exprAccessorName(t, fv.Path(), i)
		}
		if len(got) != len(tc.wantAccs) {
			t.Fatalf("%s: resolved path %v, want %v — this test asserts the name of a FUSED "+
				"value and there is nothing fused here", tc.name, got, tc.wantAccs)
		}
		for i := range got {
			if got[i] != tc.wantAccs[i] {
				t.Fatalf("%s: resolved path %v, want %v", tc.name, got, tc.wantAccs)
			}
		}

		if fv.DisplayName() != tc.wantLeaf {
			t.Errorf("%s: Field = %q, want %q — the LAST accessor of %v. Java's fused "+
				"FieldValue answers getLastFieldName and has no root name to give; a mint "+
				"that copies the root node whole leaves Field naming the struct the "+
				"reference descended THROUGH, which is a different column of a different "+
				"type, and every consumer reading Field then disagrees with the planner's "+
				"own mints of this same shape",
				tc.name, fv.DisplayName(), tc.wantLeaf, got)
		}

		// Field must not have been made right by making something else wrong:
		// the value still denotes the LEAF, so its type is the leaf's.
		if fv.ResultType() == nil {
			t.Errorf("%s: Typ is nil; a fused reference carries the LEAF's type", tc.name)
		}
	}

	// The leaf types differ between the two members, so a mint that took its
	// type from the root (or from the wrong accessor) is visible here and not
	// only as a name.
	sk, err := r.ResolveIdentifierPath(seg("n", "sk"))
	if err != nil {
		t.Fatal(err)
	}
	co, err := r.ResolveIdentifierPath(seg("n", "co"))
	if err != nil {
		t.Fatal(err)
	}
	if skT, coT := sk.Type(), co.Type(); skT == nil || coT == nil || skT.Code() == coT.Code() {
		t.Errorf("n.sk and n.co carry the same type code (%v vs %v); the mint is typing both "+
			"from something they share — the struct root — rather than from their own leaves",
			skT, coT)
	}
}

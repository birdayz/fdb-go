package expressions

import (
	"testing"
)

func TestReference_InitialOf_SingleMember(t *testing.T) {
	t.Parallel()
	e := &stubExpr{name: "x"}
	r := InitialOf(e)
	if r.Get() != e {
		t.Fatal("Get returned wrong member")
	}
	if got := r.Members(); len(got) != 1 || got[0] != e {
		t.Fatalf("members=%v, want [%v]", got, e)
	}
}

func TestReference_Insert_Dedup(t *testing.T) {
	t.Parallel()
	a := &stubExpr{name: "T"}
	b := &stubExpr{name: "T"} // structurally equal to a
	r := InitialOf(a)
	if inserted := r.Insert(b); inserted {
		t.Fatal("inserted a structurally-equal duplicate")
	}
	if len(r.Members()) != 1 {
		t.Fatalf("members size=%d after dup-insert, want 1", len(r.Members()))
	}
}

func TestReference_Insert_Distinct(t *testing.T) {
	t.Parallel()
	a := &stubExpr{name: "T"}
	c := &stubExpr{name: "U"} // structurally DIFFERENT
	r := InitialOf(a)
	if inserted := r.Insert(c); !inserted {
		t.Fatal("failed to insert structurally-different expression")
	}
	if len(r.Members()) != 2 {
		t.Fatalf("members size=%d after distinct insert, want 2", len(r.Members()))
	}
}

func TestReference_Get_EmptyReturnsNil(t *testing.T) {
	t.Parallel()
	r := &Reference{}
	if r.Get() != nil {
		t.Fatal("empty reference Get should return nil")
	}
}

func TestReference_Insert_SemanticEqualsFallback(t *testing.T) {
	t.Parallel()
	// Build two LogicalDistinct expressions with DIFFERENT inner
	// References pointing at structurally-equivalent Scans:
	//   d1 = Distinct(R1 → Scan(T))
	//   d2 = Distinct(R2 → Scan(T))   // different Reference pointer
	// sameChildReferences(d1, d2) returns false (R1 != R2), but
	// SemanticEquals(d1, d2, EmptyAliasMap) returns true (both Distinct
	// over structurally-equivalent Scans).
	//
	// Reference.Insert should treat them as duplicates via the
	// SemanticEquals fallback. The previous pointer-only contract
	// would have inserted both — this test pins the post-680e664a
	// behavior that the SemanticEquals fallback dedupes them.
	r1 := InitialOf(NewFullUnorderedScanExpression([]string{"T"}, nil))
	r2 := InitialOf(NewFullUnorderedScanExpression([]string{"T"}, nil))
	q1 := ForEachQuantifier(r1)
	q2 := ForEachQuantifier(r2)
	d1 := NewLogicalDistinctExpression(q1)
	d2 := NewLogicalDistinctExpression(q2)
	ref := InitialOf(d1)
	if inserted := ref.Insert(d2); inserted {
		t.Fatalf("Insert(d2) returned true — SemanticEquals fallback should have dedupd against d1")
	}
	if got := len(ref.Members()); got != 1 {
		t.Fatalf("Reference grew to %d members despite SemanticEquals dedup", got)
	}
}

func TestReference_Insert_PanicsOnNil(t *testing.T) {
	t.Parallel()
	r := InitialOf(&stubExpr{name: "X"})
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on Insert(nil)")
		}
	}()
	r.Insert(nil)
}

func TestReference_InsertFinal_AddsToFinalMembers(t *testing.T) {
	t.Parallel()
	r := InitialOf(&stubExpr{name: "logical"})

	final := &stubExpr{name: "physical"}
	ok := r.InsertFinal(final)
	if !ok {
		t.Fatal("InsertFinal returned false for new expression")
	}
	if len(r.FinalMembers()) != 1 || r.FinalMembers()[0] != final {
		t.Fatalf("FinalMembers=%v, want [%v]", r.FinalMembers(), final)
	}
	if len(r.AllMembers()) != 2 {
		t.Fatalf("AllMembers should have 2 (logical + final), got %d", len(r.AllMembers()))
	}
}

func TestReference_InsertFinal_Dedup(t *testing.T) {
	t.Parallel()
	r := &Reference{}
	e := &stubExpr{name: "x"}
	r.InsertFinal(e)
	ok := r.InsertFinal(e)
	if ok {
		t.Fatal("InsertFinal should return false for duplicate")
	}
	if len(r.FinalMembers()) != 1 {
		t.Fatalf("expected 1 final member after dedup, got %d", len(r.FinalMembers()))
	}
}

func TestReference_InsertFinal_NotInExploratoryMembers(t *testing.T) {
	t.Parallel()
	r := &Reference{}
	e := &stubExpr{name: "a"}
	r.InsertFinal(e)
	if len(r.Members()) != 0 {
		t.Fatalf("InsertFinal should NOT add to exploratory Members, got %v", r.Members())
	}
	if len(r.FinalMembers()) != 1 || r.FinalMembers()[0] != e {
		t.Fatalf("InsertFinal should add to FinalMembers, got %v", r.FinalMembers())
	}
	if len(r.AllMembers()) != 1 || r.AllMembers()[0] != e {
		t.Fatalf("AllMembers should include final, got %v", r.AllMembers())
	}
}

func TestReference_FinalMembers_EmptyByDefault(t *testing.T) {
	t.Parallel()
	r := InitialOf(&stubExpr{name: "x"})
	if len(r.FinalMembers()) != 0 {
		t.Fatalf("FinalMembers should be empty by default, got %d", len(r.FinalMembers()))
	}
}

func TestReference_InsertFinal_NilPanics(t *testing.T) {
	t.Parallel()
	r := &Reference{}
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on InsertFinal(nil)")
		}
	}()
	r.InsertFinal(nil)
}

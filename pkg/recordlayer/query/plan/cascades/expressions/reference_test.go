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

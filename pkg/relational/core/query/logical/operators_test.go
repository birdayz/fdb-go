package logical

import (
	"testing"
)

// Static assertion: every concrete operator satisfies the interface.
var (
	_ LogicalOperator = (*LogicalScan)(nil)
	_ LogicalOperator = (*LogicalFilter)(nil)
	_ LogicalOperator = (*LogicalProject)(nil)
	_ LogicalOperator = (*LogicalSort)(nil)
	_ LogicalOperator = (*LogicalLimit)(nil)
	_ LogicalOperator = (*LogicalAggregate)(nil)
	_ LogicalOperator = (*LogicalJoin)(nil)
	_ LogicalOperator = (*LogicalUnion)(nil)
	_ LogicalOperator = (*LogicalInsert)(nil)
	_ LogicalOperator = (*LogicalUpdate)(nil)
	_ LogicalOperator = (*LogicalDelete)(nil)
	_ LogicalOperator = (*LogicalDDL)(nil)
	_ LogicalOperator = (*LogicalCTE)(nil)
	_ LogicalOperator = (*LogicalValues)(nil)
)

func TestValues_Explain(t *testing.T) {
	t.Parallel()
	v := NewValues([]string{"1+2", "'hello'"}, []string{"", "greeting"})
	want := "Values(1+2, 'hello' AS greeting)"
	if got := v.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if len(v.Children()) != 0 {
		t.Fatalf("Values.Children: expected 0, got %d", len(v.Children()))
	}
}

func TestCTE_Explain(t *testing.T) {
	t.Parallel()
	body := NewFilter(NewScan("t", ""), "id > 0")
	main := NewProject(NewScan("x", ""), []string{"id"}, []string{""})
	c := NewCTE("x", body, main, false)
	want := "CTE(x)\n" +
		"  Filter(id > 0)\n" +
		"    Scan(t)\n" +
		"  Project(id)\n" +
		"    Scan(x)"
	if got := c.Explain(""); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if len(c.Children()) != 2 {
		t.Fatalf("CTE.Children: expected 2, got %d", len(c.Children()))
	}
}

func TestCTE_Recursive_Explain(t *testing.T) {
	t.Parallel()
	body := NewScan("seed", "")
	main := NewScan("cte", "")
	c := NewCTE("cte", body, main, true)
	got := c.Explain("")
	if len(got) < len("RecursiveCTE") || got[:len("RecursiveCTE")] != "RecursiveCTE" {
		t.Fatalf("expected RecursiveCTE prefix, got %q", got)
	}
}

func TestScan_ExplainChildren(t *testing.T) {
	t.Parallel()
	s := NewScan("Order", "")
	if got := s.Explain(""); got != "Scan(Order)" {
		t.Fatalf("Scan: got %q", got)
	}
	sa := NewScan("Order", "o")
	if got := sa.Explain("  "); got != "  Scan(Order AS o)" {
		t.Fatalf("Scan-aliased: got %q", got)
	}
	if len(s.Children()) != 0 {
		t.Fatalf("Scan.Children: expected 0, got %d", len(s.Children()))
	}
}

func TestFilter_Explain(t *testing.T) {
	t.Parallel()
	f := NewFilter(NewScan("t", ""), "id > 5")
	want := "Filter(id > 5)\n  Scan(t)"
	if got := f.Explain(""); got != want {
		t.Fatalf("Filter.Explain:\n  got:  %q\n  want: %q", got, want)
	}
	if len(f.Children()) != 1 {
		t.Fatalf("Filter.Children: expected 1, got %d", len(f.Children()))
	}
}

func TestProject_Explain(t *testing.T) {
	t.Parallel()
	p := NewProject(NewScan("t", ""), []string{"id", "v+1"}, []string{"", "vp"})
	want := "Project(id, v+1 AS vp)\n  Scan(t)"
	if got := p.Explain(""); got != want {
		t.Fatalf("Project.Explain:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestSort_Explain(t *testing.T) {
	t.Parallel()
	s := NewSort(NewScan("t", ""), []SortKey{
		{Expr: "id", Dir: SortAsc},
		{Expr: "v", Dir: SortDesc},
	})
	want := "Sort(id ASC, v DESC)\n  Scan(t)"
	if got := s.Explain(""); got != want {
		t.Fatalf("Sort.Explain: got %q", got)
	}
}

func TestLimit_Explain(t *testing.T) {
	t.Parallel()
	l := NewLimit(NewScan("t", ""), 10, 0)
	if got := l.Explain(""); got != "Limit(10)\n  Scan(t)" {
		t.Fatalf("Limit.Explain no offset: got %q", got)
	}
	lo := NewLimit(NewScan("t", ""), 10, 5)
	if got := lo.Explain(""); got != "Limit(10 offset 5)\n  Scan(t)" {
		t.Fatalf("Limit.Explain with offset: got %q", got)
	}
	// Negative limit (pure offset) renders as Offset(N) for
	// legibility — "Limit(-1 offset 5)" is unreadable plan output.
	lp := NewLimit(NewScan("t", ""), -1, 5)
	if got := lp.Explain(""); got != "Offset(5)\n  Scan(t)" {
		t.Fatalf("Limit.Explain pure-offset: got %q", got)
	}
}

func TestAggregate_Explain(t *testing.T) {
	t.Parallel()
	a := NewAggregate(
		NewScan("t", ""),
		[]string{"grp"},
		[]string{"SUM(v)", "COUNT(*)"},
		[]string{"total", ""},
		"",
	)
	want := "Aggregate(group=[grp], agg=[SUM(v) AS total, COUNT(*)])\n  Scan(t)"
	if got := a.Explain(""); got != want {
		t.Fatalf("Aggregate.Explain: got %q", got)
	}
	ah := NewAggregate(
		NewScan("t", ""),
		nil,
		[]string{"COUNT(*)"},
		nil,
		"COUNT(*) > 1",
	)
	if got := ah.Explain(""); got != "Aggregate(group=[], agg=[COUNT(*)], having=COUNT(*) > 1)\n  Scan(t)" {
		t.Fatalf("Aggregate with having: got %q", got)
	}
}

func TestJoin_Explain(t *testing.T) {
	t.Parallel()
	j := NewJoin(NewScan("a", ""), NewScan("b", ""), JoinInner, "a.id = b.a_id")
	want := "InnerJoin(on a.id = b.a_id)\n  Scan(a)\n  Scan(b)"
	if got := j.Explain(""); got != want {
		t.Fatalf("Join.Explain: got %q", got)
	}
	if len(j.Children()) != 2 {
		t.Fatalf("Join.Children: expected 2, got %d", len(j.Children()))
	}

	jl := NewJoin(NewScan("a", ""), NewScan("b", ""), JoinLeft, "")
	if got := jl.Explain(""); got != "LeftJoin\n  Scan(a)\n  Scan(b)" {
		t.Fatalf("LeftJoin no ON: got %q", got)
	}
}

func TestUnion_Explain(t *testing.T) {
	t.Parallel()
	u := NewUnion([]LogicalOperator{NewScan("a", ""), NewScan("b", "")}, false)
	want := "UnionAll\n  Scan(a)\n  Scan(b)"
	if got := u.Explain(""); got != want {
		t.Fatalf("UnionAll.Explain: got %q", got)
	}
	ud := NewUnion([]LogicalOperator{NewScan("a", ""), NewScan("b", "")}, true)
	if got := ud.Explain(""); got != "UnionDistinct\n  Scan(a)\n  Scan(b)" {
		t.Fatalf("UnionDistinct.Explain: got %q", got)
	}
}

func TestInsert_Explain(t *testing.T) {
	t.Parallel()
	i := NewInsert("t", []string{"id", "v"}, nil)
	if got := i.Explain(""); got != "Insert(t(id, v))" {
		t.Fatalf("Insert values-style no source: got %q", got)
	}
	is := NewInsert("t", nil, NewScan("src", ""))
	if got := is.Explain(""); got != "Insert(t)\n  Scan(src)" {
		t.Fatalf("Insert-SELECT: got %q", got)
	}
}

func TestUpdate_Explain(t *testing.T) {
	t.Parallel()
	u := NewUpdate("t", []Assignment{{Column: "v", Expr: "v+1"}}, NewFilter(NewScan("t", ""), "id=5"))
	want := "Update(t SET v=v+1)\n  Filter(id=5)\n    Scan(t)"
	if got := u.Explain(""); got != want {
		t.Fatalf("Update.Explain:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestDelete_Explain(t *testing.T) {
	t.Parallel()
	d := NewDelete("t", NewFilter(NewScan("t", ""), "id=5"))
	want := "Delete(t)\n  Filter(id=5)\n    Scan(t)"
	if got := d.Explain(""); got != want {
		t.Fatalf("Delete.Explain: got %q", got)
	}
}

func TestDDL_Explain(t *testing.T) {
	t.Parallel()
	d := NewDDL("CREATE TABLE", "CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))")
	if len(d.Children()) != 0 {
		t.Fatalf("DDL.Children: expected 0, got %d", len(d.Children()))
	}
	want := "DDL(CREATE TABLE: CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id)))"
	if got := d.Explain(""); got != want {
		t.Fatalf("DDL.Explain: got %q", got)
	}
}

// Children() is part of the LogicalOperator contract — any visitor
// walking the plan tree relies on correct arity and identity. Check
// every concrete operator and its nil-branch behaviour where one
// exists. Caught nothing today, but guards against a silent regression
// where (say) LogicalUpdate started returning Input twice.
func TestChildren_Arity(t *testing.T) {
	t.Parallel()
	leafA := NewScan("a", "")
	leafB := NewScan("b", "")

	cases := []struct {
		name     string
		op       LogicalOperator
		wantLen  int
		wantKids []LogicalOperator // nil → skip identity check
	}{
		{"Scan", leafA, 0, nil},
		{"Values", NewValues([]string{"1"}, nil), 0, nil},
		{"Filter", NewFilter(leafA, "x"), 1, []LogicalOperator{leafA}},
		{"Project", NewProject(leafA, []string{"id"}, []string{""}), 1, []LogicalOperator{leafA}},
		{"Sort", NewSort(leafA, []SortKey{{Expr: "id", Dir: SortAsc}}), 1, []LogicalOperator{leafA}},
		{"Limit", NewLimit(leafA, 10, 0), 1, []LogicalOperator{leafA}},
		{"Aggregate", NewAggregate(leafA, nil, []string{"COUNT(*)"}, nil, ""), 1, []LogicalOperator{leafA}},
		{"Join", NewJoin(leafA, leafB, JoinInner, "x=y"), 2, []LogicalOperator{leafA, leafB}},
		{"Union", NewUnion([]LogicalOperator{leafA, leafB}, false), 2, []LogicalOperator{leafA, leafB}},
		{"Insert-values (no source)", NewInsert("t", []string{"id"}, nil), 0, nil},
		{"Insert-SELECT", NewInsert("t", nil, leafA), 1, []LogicalOperator{leafA}},
		{"Update", NewUpdate("t", []Assignment{{Column: "v", Expr: "v+1"}}, leafA), 1, []LogicalOperator{leafA}},
		{"Update (nil input)", NewUpdate("t", nil, nil), 0, nil},
		{"Delete", NewDelete("t", leafA), 1, []LogicalOperator{leafA}},
		{"Delete (nil input)", NewDelete("t", nil), 0, nil},
		{"DDL", NewDDL("CREATE TABLE", "CREATE TABLE t (id BIGINT)"), 0, nil},
		{"CTE", NewCTE("x", leafA, leafB, false), 2, []LogicalOperator{leafA, leafB}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kids := tc.op.Children()
			if len(kids) != tc.wantLen {
				t.Fatalf("Children len: got %d, want %d", len(kids), tc.wantLen)
			}
			for i, want := range tc.wantKids {
				if kids[i] != want {
					t.Errorf("Children[%d]: got %p, want %p — pointer identity must round-trip", i, kids[i], want)
				}
			}
		})
	}
}

// Compose a realistic-ish tree and verify indentation propagates.
func TestExplain_DeepTree(t *testing.T) {
	t.Parallel()
	tree := NewProject(
		NewSort(
			NewFilter(
				NewJoin(
					NewScan("Customer", "c"),
					NewScan("Orders", "o"),
					JoinInner,
					"c.id = o.customer_id",
				),
				"o.amount > 100",
			),
			[]SortKey{{Expr: "c.name", Dir: SortAsc}},
		),
		[]string{"c.name", "o.amount"},
		[]string{"", ""},
	)
	want := "Project(c.name, o.amount)\n" +
		"  Sort(c.name ASC)\n" +
		"    Filter(o.amount > 100)\n" +
		"      InnerJoin(on c.id = o.customer_id)\n" +
		"        Scan(Customer AS c)\n" +
		"        Scan(Orders AS o)"
	if got := tree.Explain(""); got != want {
		t.Fatalf("Deep tree:\n  got:\n%s\n  want:\n%s", got, want)
	}
}

func TestSortDir_String(t *testing.T) {
	t.Parallel()
	if SortAsc.String() != "ASC" {
		t.Errorf("SortAsc.String() = %q, want ASC", SortAsc.String())
	}
	if SortDesc.String() != "DESC" {
		t.Errorf("SortDesc.String() = %q, want DESC", SortDesc.String())
	}
}

func TestJoinKind_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind JoinKind
		want string
	}{
		{JoinInner, "InnerJoin"},
		{JoinLeft, "LeftJoin"},
		{JoinRight, "RightJoin"},
	}
	for _, tc := range cases {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("JoinKind(%d).String() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestUnion_Children_CopiesSlice(t *testing.T) {
	t.Parallel()
	a := NewScan("a", "")
	b := NewScan("b", "")
	u := NewUnion([]LogicalOperator{a, b}, false)
	kids := u.Children()
	kids[0] = nil
	if u.Inputs[0] == nil {
		t.Error("Children() must return a copy, not the original slice")
	}
}

func TestScan_ExplainWithIndent(t *testing.T) {
	t.Parallel()
	s := NewScan("tbl", "")
	got := s.Explain("    ")
	if got != "    Scan(tbl)" {
		t.Errorf("got %q, want %q", got, "    Scan(tbl)")
	}
}

func TestJoinWithPredicate(t *testing.T) {
	t.Parallel()
	j := NewJoinWithPredicate(NewScan("a", ""), NewScan("b", ""), JoinLeft, "fake-pred")
	if j.OnPredicate != "fake-pred" {
		t.Errorf("OnPredicate = %v, want fake-pred", j.OnPredicate)
	}
	if j.Kind != JoinLeft {
		t.Errorf("Kind = %v, want JoinLeft", j.Kind)
	}
}

func BenchmarkExplain_Scan(b *testing.B) {
	s := NewScan("Order", "o")
	for b.Loop() {
		_ = s.Explain("")
	}
}

func BenchmarkExplain_DeepTree(b *testing.B) {
	tree := NewProject(
		NewSort(
			NewFilter(
				NewJoin(NewScan("a", ""), NewScan("b", ""), JoinInner, "a.id=b.id"),
				"b.x > 0",
			),
			[]SortKey{{Expr: "a.name", Dir: SortAsc}},
		),
		[]string{"a.name", "b.x"},
		[]string{"", ""},
	)
	for b.Loop() {
		_ = tree.Explain("")
	}
}

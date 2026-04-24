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
)

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

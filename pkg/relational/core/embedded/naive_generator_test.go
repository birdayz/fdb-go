package embedded

import (
	"context"
	"strings"
	"testing"
)

// End-to-end tests for naiveGenerator's Plan.Explain() surface. These
// don't touch FDB — Plan() only parses + wraps the parse tree into a
// Plan; Execute would run against a real store but Explain is a pure
// function of the parse tree + the logical builder.

// helperPlan runs naiveGenerator.Plan() against a SQL string and
// returns the resulting Plan. A parse error fails the test.
func helperPlan(t *testing.T, sql string) interface {
	Explain() string
	IsUpdate() bool
} {
	t.Helper()
	g := &naiveGenerator{c: &EmbeddedConnection{}}
	p, err := g.Plan(context.Background(), sql)
	if err != nil {
		t.Fatalf("Plan(%q): %v", sql, err)
	}
	return p
}

func TestNaiveGenerator_Explain_SelectStar(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "SELECT * FROM t")
	if got := p.Explain(); got != "Scan(t)" {
		t.Fatalf("got %q, want Scan(t)", got)
	}
	if p.IsUpdate() {
		t.Fatal("SELECT should not be an update plan")
	}
}

func TestNaiveGenerator_Explain_SelectWhere(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "SELECT id, name FROM users WHERE active = TRUE ORDER BY id LIMIT 10")
	got := p.Explain()
	// Composition: Project → Limit → Sort → Filter → Scan.
	for _, want := range []string{
		"Project(id, name)",
		"Limit(10)",
		"Sort(id ASC)",
		"Filter(active=TRUE)",
		"Scan(users)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("explain %q missing %q", got, want)
		}
	}
}

func TestNaiveGenerator_Explain_JoinWithWhere(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "SELECT a.id FROM a INNER JOIN b ON a.id = b.a_id WHERE a.active = TRUE")
	got := p.Explain()
	for _, want := range []string{"Project(a.id)", "Filter(a.active=TRUE)", "InnerJoin(on a.id=b.a_id)", "Scan(a)", "Scan(b)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("explain %q missing %q", got, want)
		}
	}
}

func TestNaiveGenerator_Explain_Delete(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "DELETE FROM t WHERE id > 5")
	got := p.Explain()
	for _, want := range []string{"Delete(t)", "Filter(id>5)", "Scan(t)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("explain %q missing %q", got, want)
		}
	}
	if !p.IsUpdate() {
		t.Fatal("DELETE should be an update plan")
	}
}

func TestNaiveGenerator_Explain_Update(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "UPDATE users SET active = FALSE WHERE id = 5")
	got := p.Explain()
	for _, want := range []string{"Update(users SET active=FALSE)", "Filter(id=5)", "Scan(users)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("explain %q missing %q", got, want)
		}
	}
}

func TestNaiveGenerator_Explain_Insert(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "INSERT INTO t (id, name) VALUES (1, 'a')")
	if got := p.Explain(); !strings.Contains(got, "Insert(t(id, name))") {
		t.Fatalf("got %q, want Insert(t(id, name))", got)
	}
}

func TestNaiveGenerator_Explain_UnionAll(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "SELECT id FROM a UNION ALL SELECT id FROM b")
	got := p.Explain()
	if !strings.Contains(got, "UnionAll") {
		t.Fatalf("got %q, want UnionAll", got)
	}
	if !strings.Contains(got, "Scan(a)") || !strings.Contains(got, "Scan(b)") {
		t.Fatalf("got %q, want Scan(a) and Scan(b)", got)
	}
}

func TestNaiveGenerator_Explain_Aggregate(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "SELECT COUNT(*) FROM t")
	if got := p.Explain(); !strings.Contains(got, "Aggregate(group=[], agg=[COUNT(*)])") {
		t.Fatalf("got %q, want COUNT(*) aggregate", got)
	}
}

// DDL / TX still use the canonical-SQL fallback (builder returns
// nil for those shapes). Ensure the fallback still renders.
func TestNaiveGenerator_Explain_DDLFallback(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "CREATE SCHEMA /mydb/main WITH TEMPLATE tmpl")
	got := p.Explain()
	if !strings.HasPrefix(got, "DDL:") {
		t.Fatalf("got %q, want DDL:-prefixed explain", got)
	}
}

// Empty SQL: Plan returns a no-op update plan; Explain renders
// "empty" per the seed.
func TestNaiveGenerator_Explain_EmptyStatements(t *testing.T) {
	t.Parallel()
	p := helperPlan(t, "")
	if got := p.Explain(); got != "empty" {
		t.Fatalf("got %q, want empty", got)
	}
}

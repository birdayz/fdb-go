package embedded

import (
	"reflect"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// The template's views, routines and stored queries, as Java's
// RecordMetadataDeserializer builds them from the stored metadata: a view or a
// SQL function is described by its stored definition, a stored query by its
// SELECT text and the temporary functions its DECLARE block became, and none
// of them is temporary.
func TestSchemaTemplateRoutineViewAndStoredQueryGetters(t *testing.T) {
	t.Parallel()
	tmpl, err := BuildSchemaTemplateFromDDLNamed(
		`CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id)) `+
			`CREATE FUNCTION add1(IN x BIGINT) RETURNS BIGINT RETURN x + 1 `+
			`CREATE FUNCTION tf(IN lo BIGINT) AS SELECT id FROM t WHERE id > lo `+
			`CREATE VIEW v AS SELECT id FROM t `+
			`CREATE STORED QUERY q1 AS SELECT id FROM t `+
			`CREATE STORED QUERY q2 DECLARE FUNCTION f(IN x BIGINT) AS (SELECT id FROM t WHERE id = x) AS SELECT * FROM f(1)`,
		"ROUTINE_GETTERS")
	if err != nil {
		t.Fatal(err)
	}

	views, err := tmpl.Views()
	if err != nil || len(views) != 1 {
		t.Fatalf("views = %v, %v", views, err)
	}
	if v := views[0]; v.MetadataName() != "V" || v.Description() != "SELECT id FROM t" || v.IsTemporary() {
		t.Errorf("view = %q %q temporary=%v", v.MetadataName(), v.Description(), v.IsTemporary())
	}
	if v, err := tmpl.FindView("V"); err != nil || v == nil || v.MetadataName() != "V" {
		t.Errorf("FindView(V) = %v, %v", v, err)
	}
	if v, err := tmpl.FindView("W"); err != nil || v != nil {
		t.Errorf("FindView(W) = %v, %v, want none", v, err)
	}

	routines, err := tmpl.InvokedRoutines()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]api.InvokedRoutine{}
	for _, r := range routines {
		got[r.MetadataName()] = r
	}
	if len(got) != 2 || got["ADD1"] == nil || got["TF"] == nil {
		t.Fatalf("routines = %v", got)
	}
	if d := got["TF"].Description(); d != "CREATE FUNCTION tf(IN lo BIGINT) AS SELECT id FROM t WHERE id > lo" {
		t.Errorf("TF description = %q", d)
	}
	for name, r := range got {
		if r.IsTemporary() || r.NormalizedDescription() != "" {
			t.Errorf("%s: temporary=%v normalized=%q", name, r.IsTemporary(), r.NormalizedDescription())
		}
	}
	if r, err := tmpl.FindInvokedRoutine("ADD1"); err != nil || r == nil || r.MetadataName() != "ADD1" {
		t.Errorf("FindInvokedRoutine(ADD1) = %v, %v", r, err)
	}

	queries, err := tmpl.StoredQueries()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]api.StoredQuery{
		"Q1": {Query: "SELECT id FROM t"},
		"Q2": {Query: "SELECT * FROM f(1)", TempFunctions: []string{
			"CREATE TEMPORARY FUNCTION f(IN x BIGINT) ON COMMIT DROP FUNCTION AS SELECT id FROM t WHERE id = x",
		}},
	}
	if !reflect.DeepEqual(queries, want) {
		t.Errorf("stored queries = %#v, want %#v", queries, want)
	}

	var visited []string
	tmpl.Accept(&routineViewVisitor{seen: &visited})
	if len(visited) != 3 {
		t.Errorf("Accept visited %v, want both routines and the view", visited)
	}
}

type routineViewVisitor struct {
	api.Visitor
	seen *[]string
}

func (v *routineViewVisitor) StartVisitSchemaTemplate(api.SchemaTemplate)  {}
func (v *routineViewVisitor) VisitSchemaTemplate(api.SchemaTemplate)       {}
func (v *routineViewVisitor) FinishVisitSchemaTemplate(api.SchemaTemplate) {}
func (v *routineViewVisitor) VisitTable(api.Table)                         {}
func (v *routineViewVisitor) VisitColumn(api.Column)                       {}
func (v *routineViewVisitor) VisitIndex(api.Index)                         {}
func (v *routineViewVisitor) VisitInvokedRoutine(r api.InvokedRoutine) {
	*v.seen = append(*v.seen, r.MetadataName())
}
func (v *routineViewVisitor) VisitView(w api.View) { *v.seen = append(*v.seen, w.MetadataName()) }

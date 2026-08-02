package embedded

import (
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/core/parser"
	"fdb.dev/pkg/relational/core/query/logical"
)

// Negative pins measured while building RFC-202 S2 (this file started as the
// probe that measured them). Each is LOAD-BEARING for a later RFC-202 stage:
// the stage's work includes turning the pin around, and until then the pin
// names exactly what is missing so a silent change re-opens the decision
// rather than sliding by.

func probeTemplate(t *testing.T) *metadata.RecordLayerSchemaTemplate {
	t.Helper()
	b := metadata.NewSchemaTemplateBuilder().SetName("probe_tpl")
	b.AddTable("T1", []metadata.ColumnSpec{
		metadata.NewColumnSpec("P1", api.NewLongType(true), 1),
		metadata.NewColumnSpec("A1", api.NewLongType(true), 2),
		metadata.NewColumnSpec("A2", api.NewLongType(true), 3),
	}, []string{"P1"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	return tmpl
}

func probePlan(t *testing.T, tmpl *metadata.RecordLayerSchemaTemplate, sql string) (logical.LogicalOperator, error) {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	sel := root.Statements().AllStatement()[0].SelectStatement()
	return NewPlanVisitor(tmpl.Underlying()).VisitQuery(sel.Query())
}

// TestIndexDDLProbe_ExtremumAggregateRecognized pins the S3 routing
// precondition in its POSITIVE form: the plan visitor's aggregate
// classification (extractAwfFields) recognizes MIN_EVER / MAX_EVER, the call
// lands in LogicalAggregate.Calls with a source-resolved operand, and the
// aggregate AS-SELECT form therefore routes through the generator's aggregate
// arm (the legacy parse-tree arm is deleted). If this stops holding, extremum
// index DDL silently loses its structured route — the generator would see an
// unclassifiable select element and every extremum index declaration fails.
func TestIndexDDLProbe_ExtremumAggregateRecognized(t *testing.T) {
	t.Parallel()
	tmpl := probeTemplate(t)
	op, err := probePlan(t, tmpl, "SELECT MAX_EVER(a1) FROM t1 GROUP BY a2")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	proj, ok := op.(*logical.LogicalProject)
	if !ok {
		t.Fatalf("top operator is %T, want Project", op)
	}
	agg, ok := proj.Input.(*logical.LogicalAggregate)
	if !ok {
		t.Fatalf("below Project is %T, want Aggregate", proj.Input)
	}
	if len(agg.Calls) != 1 || agg.Calls[0].Func != "MAX_EVER" {
		t.Fatalf("MAX_EVER not classified as an aggregate call (calls=%v) — the "+
			"generator's aggregate arm (RFC-202 S3) cannot see extremum index DDL "+
			"and every MIN_EVER/MAX_EVER CREATE INDEX fails", agg.Calls)
	}
	if len(agg.AggregateOperands) != 1 || agg.AggregateOperands[0] == nil {
		t.Fatalf("MAX_EVER operand unresolved (%v) — the generator refuses to build "+
			"from an unresolved operand", agg.AggregateOperands)
	}
	if len(agg.OutputSlots) != 1 || agg.OutputSlots[0].NativeOrdinal != len(agg.GroupKeys) {
		t.Fatalf("MAX_EVER select slot not addressed to the call (slots=%v)", agg.OutputSlots)
	}
}

// TestIndexDDLProbe_StructQualifierResolves pins that a dotted path into a
// struct-typed column RESOLVES and plans, including in ORDER BY — the
// lookupNestedField descent (SemanticAnalyzer.java:481-488, :548-602).
//
// This test's ancestor asserted the opposite, and the flip is the point: the
// generator's field-path trie is no longer effectively single-accessor from
// SQL, so its multi-accessor arm is now REACHABLE. What this does NOT claim is
// that a nested path can be INDEXED — emitting field(S).nest(...) key
// expressions is RFC-204 §4.6 / Phase 5 joint with RFC-202, and the corpus
// still books six files to `unsupported-DDL:struct-index`. The query here is
// expected to plan through an ordinary scan-and-sort, not through an index on
// the nested path.
func TestIndexDDLProbe_StructQualifierResolves(t *testing.T) {
	t.Parallel()
	st := api.NewStructType("S1", []api.StructField{
		api.NewStructField("SA", api.NewLongType(true), 0),
		api.NewStructField("SB", api.NewLongType(true), 1),
	}, true)
	b := metadata.NewSchemaTemplateBuilder().SetName("probe_struct_tpl")
	b.AddTable("T1", []metadata.ColumnSpec{
		metadata.NewColumnSpec("P1", api.NewLongType(true), 1),
		metadata.NewColumnSpec("S", st, 2),
	}, []string{"P1"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	// Both fields, because a descent hard-wired to ordinal 0 plans SA
	// correctly and silently reads the wrong slot for SB.
	for _, q := range []string{
		"SELECT s.sa FROM t1 ORDER BY s.sa",
		"SELECT s.sb FROM t1 ORDER BY s.sb",
		"SELECT p1 FROM t1 WHERE s.sb = 7",
	} {
		if _, perr := probePlan(t, tmpl, q); perr != nil {
			t.Errorf("%s: struct-qualified access must resolve: %v", q, perr)
		}
	}
	// A field the struct does not declare stays the clean 42703 — the descent
	// widens what resolves, it does not make resolution permissive.
	_, err = probePlan(t, tmpl, "SELECT s.nosuch FROM t1")
	if err == nil {
		t.Fatal("SELECT s.nosuch planned; an undeclared struct field must not resolve")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUndefinedColumn {
		t.Fatalf("want 42703 undefined-column, got %v", err)
	}
}

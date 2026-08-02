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

// TestIndexDDLProbe_StructQualifierUnresolvable pins that a dotted path into
// a struct-typed column does not resolve (42703) — which is what keeps the
// generator's field-path TRIE effectively single-accessor from SQL today.
// Struct columns cannot be DECLARED via SQL DDL either (RFC-201 Phase 3), so
// this pin uses a programmatically-built template — the only way a struct
// column exists at all. When struct DDL lands and this resolution starts
// working, the trie's multi-accessor arm becomes SQL-reachable and needs
// end-to-end goldens (nested nest/concat shapes, IndexTest.java:819-830).
func TestIndexDDLProbe_StructQualifierUnresolvable(t *testing.T) {
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
	_, err = probePlan(t, tmpl, "SELECT s.sa FROM t1 ORDER BY s.sa")
	if err == nil {
		t.Fatal("struct-qualified access now RESOLVES — the RFC-202 trie's " +
			"multi-accessor arm is SQL-reachable and needs end-to-end key-expression " +
			"goldens (field(S).nest(concat(...)) shapes)")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUndefinedColumn {
		t.Fatalf("want 42703 undefined-column, got %v", err)
	}
}

package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// A CTE REFERENCE carrying a column-alias list renames the columns it exposes.
// derivedOutputColumns did that rename IN PLACE, on a slice legColumns is free to
// hand back shared: a pre-translated CTE's schema comes straight out of
// cteColumnsScope with no copy. So renaming at one reference rewrote the
// DEFINITION's schema, and every later reader of that CTE saw the second
// reference's names.
//
// The hazard was already known in this codebase — ordinal_seed.go's legColumns
// caller carries "Copy-on-wrap: legColumns may hand back shared slices" and
// copies — which is what makes the unguarded arm worth a test rather than a
// note: one caller defended and the other did not, so the invariant was being
// maintained by memory.
func TestCTEAliasRenameDoesNotRewriteTheDefinitionSchema(t *testing.T) {
	t.Parallel()

	const cteName = "R"
	stored := []values.Field{
		{Name: "N", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "M", FieldType: values.NotNullLong, Ordinal: 1},
	}
	tr := &cascadesTranslator{
		cteExprScope:    map[string]expressions.RelationalExpression{cteName: nil},
		cteColumnsScope: map[string][]values.Field{cteName: stored},
	}

	ref := &logical.LogicalCTE{
		Name:          "D",
		Body:          &logical.LogicalScan{Table: cteName},
		ColumnAliases: []string{"a", "b"},
	}

	got := tr.derivedOutputColumns(ref)
	if len(got) != 2 {
		t.Fatalf("derivedOutputColumns returned %d columns, want 2 — the test is not "+
			"exercising the rename arm", len(got))
	}
	if got[0].Name != "A" || got[1].Name != "B" {
		t.Fatalf("reference columns are %q/%q, want A/B — the rename did not happen, "+
			"so the aliasing assertion below is vacuous", got[0].Name, got[1].Name)
	}

	// The definition's schema must be untouched. This is the assertion; the two
	// above exist so it cannot pass by the rename never running.
	if stored[0].Name != "N" || stored[1].Name != "M" {
		t.Errorf("the CTE definition's stored schema was rewritten to %q/%q by a "+
			"REFERENCE's alias list; every later reader of %s now sees this "+
			"reference's names", stored[0].Name, stored[1].Name, cteName)
	}
	if same := &got[0] == &stored[0]; same {
		t.Error("derivedOutputColumns returned the stored slice itself, so any caller " +
			"that writes to the result corrupts the definition")
	}
}

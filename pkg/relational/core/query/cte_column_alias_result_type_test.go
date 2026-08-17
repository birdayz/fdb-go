package query

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// A CTE column list renames the BODY's output columns, not the statement's.
// `LogicalCTE.ColumnAliases` says so, `exactCTEDefinitionRecordType` applies it
// that way, and the translator builds a Project over the BODY from it — so a
// derivation that renames the MAIN query's row instead disagrees with every
// other consumer of the same field.
//
// The disagreement is not cosmetic. Both arms below are ordinary SQL:
//
//	WITH c(x) AS (SELECT a, b FROM t) SELECT x AS y FROM c   -- name
//	WITH c(x, y) AS (SELECT a, b FROM t) SELECT x FROM c     -- width
//
// The first returns the column named Y; renaming main's row reports X. The
// second is legal — a main query is free to project fewer columns than the CTE
// declares — but checking the alias arity against main's row makes it an error.
//
// THIS IS THE PIN, and the e2e is not. The SQL surface reaches its result-set
// labels through a different authority today, so
// TestFDB_CTEColumnListDoesNotOverwriteTheMainQueryLabel passes with the
// derivation broken; that was measured by instrumenting the pre-fix branch and
// running the sqldriver and embedded suites, which fired it 0 times. The
// consumers that DO read this derivation are the ones that type an existential
// QOV from a subplan (scalar and correlated-scalar subqueries, the clustered
// outer scalar), where the wrong row becomes the wrong captured schema rather
// than a wrong label.
func cteColumnAliasBodyFixture(t *testing.T) *logical.LogicalInlineValues {
	t.Helper()
	rowType := &values.RecordType{Fields: []values.Field{
		{Name: "A", Ordinal: 0, FieldType: values.NotNullInt},
		{Name: "B", Ordinal: 1, FieldType: values.NotNullLong},
	}}
	row := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "A", Value: &values.ConstantValue{Value: int32(1), Typ: values.NotNullInt}},
		values.RecordConstructorField{Name: "B", Value: &values.ConstantValue{Value: int64(2), Typ: values.NotNullLong}},
	)
	if !row.Type().Equals(rowType) {
		t.Fatalf("fixture row = %v, want %v", row.Type(), rowType)
	}
	source, err := logical.NewInlineValues("V", values.NewArrayConstructorValue(rowType, []values.Value{row}))
	if err != nil {
		t.Fatalf("NewInlineValues: %v", err)
	}
	return source
}

// positionalProject is the shape the machinery builds for a boundary created
// before its input quantifier exists — the CTE column-list rename itself is
// the example InputOrdinals documents. ProjectedValues stays nil per slot and
// the type comes from the input row's ordinal, which is exactly what lets this
// test state a main query without resolving any value.
func positionalProject(input logical.LogicalOperator, projections, aliases []string, ordinals []int) *logical.LogicalProject {
	return &logical.LogicalProject{
		Input:           input,
		Projections:     projections,
		Aliases:         aliases,
		ProjectedValues: make([]values.Value, len(projections)),
		InputOrdinals:   ordinals,
	}
}

func TestCTEColumnAliasesRenameTheBodyNotTheStatement(t *testing.T) {
	t.Parallel()

	t.Run("main_alias_survives", func(t *testing.T) {
		t.Parallel()
		// WITH c(x, w) AS (VALUES (a, b)) SELECT x AS y FROM c
		cte := &logical.LogicalCTE{
			Name:          "C",
			ColumnAliases: []string{"X", "W"},
			Body:          cteColumnAliasBodyFixture(t),
			Main: positionalProject(
				logical.NewScan("C", "C"),
				[]string{"X"}, []string{"Y"}, []int{0},
			),
		}
		typ, err := ExactLogicalResultType(cte, nil)
		if err != nil {
			t.Fatalf("ExactLogicalResultType: %v", err)
		}
		record, ok := typ.(*values.RecordType)
		if !ok {
			t.Fatalf("result type = %T, want *values.RecordType", typ)
		}
		if len(record.Fields) != 1 {
			t.Fatalf("result has %d columns, want 1: %v", len(record.Fields), record)
		}
		if got := record.Fields[0].Name; got != "Y" {
			t.Errorf("result column = %q, want %q — the CTE column list renamed the"+
				" statement's row instead of the body's, so the main query's AS was overwritten", got, "Y")
		}
		// The rename must have reached the BODY: main projects input ordinal 0,
		// whose type is the body's first column.
		if got := record.Fields[0].FieldType; !got.Equals(values.NotNullInt) {
			t.Errorf("result column type = %v, want %v", got, values.NotNullInt)
		}
	})

	t.Run("narrower_main_is_not_an_arity_error", func(t *testing.T) {
		t.Parallel()
		// WITH c(x, w) AS (VALUES (a, b)) SELECT x FROM c
		cte := &logical.LogicalCTE{
			Name:          "C",
			ColumnAliases: []string{"X", "W"},
			Body:          cteColumnAliasBodyFixture(t),
			Main: positionalProject(
				logical.NewScan("C", "C"),
				[]string{"X"}, []string{""}, []int{0},
			),
		}
		typ, err := ExactLogicalResultType(cte, nil)
		if err != nil {
			t.Fatalf("a main query narrower than the CTE column list was rejected: %v", err)
		}
		record, ok := typ.(*values.RecordType)
		if !ok || len(record.Fields) != 1 {
			t.Fatalf("result type = %v, want a one-column record", typ)
		}
		if got := record.Fields[0].Name; got != "X" {
			t.Errorf("result column = %q, want %q", got, "X")
		}
	})

	t.Run("body_bound_under_the_alias_names", func(t *testing.T) {
		t.Parallel()
		// The binding the main query resolves against must carry the ALIAS
		// names. Selecting the body's original name is what a scan of an
		// unrenamed binding would still find, so this arm is the one that
		// fails if the aliases are applied to the statement rather than the
		// binding. A plain scan of the CTE reports the bound row directly.
		cte := &logical.LogicalCTE{
			Name:          "C",
			ColumnAliases: []string{"X", "W"},
			Body:          cteColumnAliasBodyFixture(t),
			Main:          logical.NewScan("C", "C"),
		}
		typ, err := ExactLogicalResultType(cte, nil)
		if err != nil {
			t.Fatalf("ExactLogicalResultType: %v", err)
		}
		record, ok := typ.(*values.RecordType)
		if !ok || len(record.Fields) != 2 {
			t.Fatalf("result type = %v, want a two-column record", typ)
		}
		var names []string
		for _, f := range record.Fields {
			names = append(names, f.Name)
		}
		if got := strings.Join(names, ","); got != "X,W" {
			t.Errorf("CTE binding columns = %q, want %q", got, "X,W")
		}
		for i, f := range record.Fields {
			if f.Ordinal != i {
				t.Errorf("renamed column %d has ordinal %d — the rename dropped the"+
					" positional identity every exact consumer reads", i, f.Ordinal)
			}
		}
	})

	t.Run("alias_arity_disagreeing_with_the_body_is_refused", func(t *testing.T) {
		t.Parallel()
		// Three aliases over a two-column body is the real arity error, and it
		// must be reported against the BODY — the row the aliases actually name.
		cte := &logical.LogicalCTE{
			Name:          "C",
			ColumnAliases: []string{"X", "W", "Z"},
			Body:          cteColumnAliasBodyFixture(t),
			Main:          logical.NewScan("C", "C"),
		}
		if typ, err := ExactLogicalResultType(cte, nil); err == nil {
			t.Fatalf("three aliases over a two-column body typed as %v, want an error", typ)
		}
	})
}

package embedded

import (
	"testing"

	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

func parseInlineValuesFromSource(t *testing.T, sql string) *fromSource {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	statements := root.Statements().AllStatement()
	if len(statements) != 1 || statements[0].SelectStatement() == nil {
		t.Fatalf("parse %q did not produce one SELECT", sql)
	}
	query := statements[0].SelectStatement().Query()
	body, ok := query.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		t.Fatalf("query body = %T, want QueryTermDefault", query.QueryExpressionBody())
	}
	simple, ok := body.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		t.Fatalf("query term = %T, want SimpleTable", body.QueryTerm())
	}
	from, err := parseFromSource(simple)
	if err != nil {
		t.Fatalf("parseFromSource(%q): %v", sql, err)
	}
	return from
}

func TestParseInlineValuesPrimaryCarriesAuthoredDefinition(t *testing.T) {
	t.Parallel()
	from := parseInlineValuesFromSource(t,
		`SELECT "values"."id" FROM VALUES (1, [101]), (2, [201, 202]) AS "values" ("id", "arr")`)

	if from.inlineValues == nil {
		t.Fatal("primary inline VALUES parse node was not carried")
	}
	if got := len(from.inlineValues.AllRecordConstructorForInlineTable()); got != 2 {
		t.Fatalf("literal row count = %d, want 2", got)
	}
	if from.tableName != "values" || from.tableAlias != "values" {
		t.Fatalf("source identity = (%q, %q), want authored quoted alias values", from.tableName, from.tableAlias)
	}
	if len(from.sourceSegments) != 1 || from.sourceSegments[0] != "values" {
		t.Fatalf("source segments = %v, want [values]", from.sourceSegments)
	}
	definition := from.inlineValues.InlineTableDefinition()
	if definition == nil || definition.UidListWithNestingsInParens() == nil {
		t.Fatal("authored inline column definition was not preserved")
	}
	if len(from.joins) != 0 {
		t.Fatalf("joins = %d, want 0", len(from.joins))
	}
}

func TestParseInlineValuesPrimaryWithoutDefinitionMintsStableCarrier(t *testing.T) {
	t.Parallel()
	from := parseInlineValuesFromSource(t, `SELECT 1 FROM VALUES (1), (2)`)
	if from.inlineValues == nil {
		t.Fatal("primary inline VALUES parse node was not carried")
	}
	if from.tableAlias != "Q$INLINE_VALUES0" || from.tableName != "Q$INLINE_VALUES0" {
		t.Fatalf("private source identity = (%q, %q), want Q$INLINE_VALUES0", from.tableName, from.tableAlias)
	}
}

func TestParseInlineValuesCommaSourceCarriesDistinctJoinKind(t *testing.T) {
	t.Parallel()
	from := parseInlineValuesFromSource(t, `SELECT T.ID FROM T, VALUES (1), (2) AS V (ID)`)
	if from.inlineValues != nil {
		t.Fatal("base table was incorrectly marked as inline VALUES")
	}
	if len(from.joins) != 1 {
		t.Fatalf("joins = %d, want 1", len(from.joins))
	}
	join := from.joins[0]
	if join.inlineValues == nil {
		t.Fatal("comma inline VALUES parse node was not carried")
	}
	if !join.fromComma || join.joinType != joinTypeInner {
		t.Fatalf("comma carrier = fromComma %v, joinType %v; want true/inner", join.fromComma, join.joinType)
	}
	if join.tableName != "V" || join.alias != "V" {
		t.Fatalf("comma source identity = (%q, %q), want V", join.tableName, join.alias)
	}
	if got := len(join.inlineValues.AllRecordConstructorForInlineTable()); got != 2 {
		t.Fatalf("literal row count = %d, want 2", got)
	}
}

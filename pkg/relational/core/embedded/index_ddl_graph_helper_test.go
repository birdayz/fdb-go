package embedded

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	querycore "fdb.dev/pkg/relational/core/query"
)

// indexGraphSignature is the translated graph an index definition's generator
// reads, as a class signature: each expression's class, then its quantifiers
// in order, an existential one marked "E:". It runs the path CREATE SCHEMA
// TEMPLATE runs for an AS SELECT index (ddl.go) — visit, the FROM-resolution
// post-passes, the translation Generate performs — against the template's
// tables, so a test can pin that the shape it drives an arm with is the shape
// that arm handles.
func indexGraphSignature(t *testing.T, tablesDDL, indexDDL string) string {
	t.Helper()
	tmpl, err := buildSchemaTemplateFromDDL(tablesDDL)
	if err != nil {
		t.Fatalf("tables: %v", err)
	}
	md := tmpl.Underlying()
	root, err := parser.Parse("CREATE SCHEMA TEMPLATE auto_template " + tablesDDL + "\n" + indexDDL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st := root.Statements().AllStatement()[0].DdlStatement().CreateStatement().(*antlrgen.CreateSchemaTemplateStatementContext)
	var def *antlrgen.IndexAsSelectDefinitionContext
	for _, clause := range st.AllTemplateClause() {
		if d, ok := clause.IndexDefinition().(*antlrgen.IndexAsSelectDefinitionContext); ok {
			def = d
		}
	}
	if def == nil {
		t.Fatalf("no AS SELECT index in %q", indexDDL)
	}
	visitor := NewPlanVisitorWithTemplate(md, tmpl.MetadataName())
	op, err := visitor.VisitQueryTerm(def.QueryTerm())
	if err != nil {
		t.Fatalf("visit: %v", err)
	}
	if err := runFromResolutionPostPasses(op, visitor.templateName, md, md); err != nil {
		t.Fatalf("post-passes: %v", err)
	}
	ref, _, err := querycore.TranslateToCascadesWithError(op, md)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var sb strings.Builder
	writeGraphSignature(&sb, ref)
	return sb.String()
}

func writeGraphSignature(sb *strings.Builder, ref *expressions.Reference) {
	members := ref.Members()
	if len(members) != 1 {
		fmt.Fprintf(sb, "<%d members>", len(members))
		return
	}
	e := members[0]
	name := fmt.Sprintf("%T", e)
	name = strings.TrimPrefix(name[strings.LastIndex(name, ".")+1:], "Logical")
	sb.WriteString(strings.TrimSuffix(name, "Expression"))
	qs := e.GetQuantifiers()
	if len(qs) == 0 {
		return
	}
	sb.WriteString("(")
	for i, q := range qs {
		if i > 0 {
			sb.WriteString(", ")
		}
		if q.Kind() == expressions.QuantifierExistential {
			sb.WriteString("E:")
		}
		writeGraphSignature(sb, q.GetRangesOver())
	}
	sb.WriteString(")")
}

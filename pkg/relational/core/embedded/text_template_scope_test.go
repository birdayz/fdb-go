package embedded

import (
	"strings"
	"testing"
)

func TestFullTextCaseSensitiveTemplateScope(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"TEXT_CONTAINS_ALL", "TEXT_CONTAINS_ANY", "TEXT_CONTAINS_PHRASE", "TEXT_CONTAINS_ALL_WITHIN", "TEXT_CONTAINS_PREFIX", "TEXT_CONTAINS_ALL_PREFIXES", "TEXT_CONTAINS_ANY_PREFIX"} {
		for _, name := range []string{strings.ToLower(family), "Text_" + strings.TrimPrefix(family, "TEXT_"), family} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				const base = "CREATE SCHEMA TEMPLATE T CREATE TABLE DOCS(ID BIGINT, BODY STRING, PRIMARY KEY(ID)) "
				definition := "CREATE FUNCTION \"" + name + "\"(IN s STRING) RETURNS STRING RETURN s "
				for _, call := range []string{name, `"` + name + `"`} {
					// Views and indexes compile after all functions, even when written first.
					ddl := base + "CREATE VIEW V AS SELECT ID," + call + "(BODY) AS VAL FROM DOCS " +
						"CREATE INDEX X AS SELECT " + call + "(BODY) AS VAL FROM DOCS ORDER BY VAL " + definition +
						"CREATE FUNCTION f(IN s STRING) RETURNS STRING RETURN " + call + "(s) " +
						"CREATE FUNCTION tf() AS SELECT ID FROM DOCS WHERE " + call + "(BODY) = 'red'"
					if _, err := BuildSchemaTemplateFromDDL(caseSensitiveIdentifiers(ddl)); err != nil {
						t.Fatalf("%s: %v", ddl, err)
					}
				}
				for _, call := range []string{`"missing_` + name + `"(s)`, name + "(s,s)"} {
					ddl := base + definition + "CREATE FUNCTION f(IN s STRING) RETURNS STRING RETURN " + call
					if _, err := BuildSchemaTemplateFromDDL(caseSensitiveIdentifiers(ddl)); err == nil {
						t.Fatalf("invalid lookup/arity resolved: %s", ddl)
					}
				}
				if name != family {
					ddl := base + definition + "CREATE FUNCTION f(IN s STRING) RETURNS STRING RETURN " + family + "(s)"
					if _, err := BuildSchemaTemplateFromDDL(caseSensitiveIdentifiers(ddl)); err == nil {
						t.Fatalf("case-mismatched lookup resolved: %s", ddl)
					}
				}
				forward := base + "CREATE FUNCTION f(IN s STRING) RETURNS STRING RETURN " + name + "(s) " + definition
				if _, err := BuildSchemaTemplateFromDDL(caseSensitiveIdentifiers(forward)); err == nil {
					t.Fatalf("forward reference resolved: %s", forward)
				}
			})
		}
	}
}

func TestFullTextResidualOrderingPlans(t *testing.T) {
	t.Parallel()
	template, err := BuildSchemaTemplateFromDDL("CREATE TABLE DOCS(ID BIGINT, BODY STRING, PRIMARY KEY(ID)) CREATE FULLTEXT INDEX BODY_TEXT ON DOCS(BODY)")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		predicate string
		residual  bool
	}{
		{"TEXT_CONTAINS_ALL(BODY,'red')", false},
		{"TEXT_CONTAINS_ANY(BODY,'red')", false},
		{"TEXT_CONTAINS_PHRASE(BODY,'red')", true},
		{"TEXT_CONTAINS_ALL_WITHIN(BODY,'red',1)", true},
		{"TEXT_CONTAINS_PREFIX(BODY,'re')", false},
		{"TEXT_CONTAINS_ALL_PREFIXES(BODY,'re')", true},
		{"TEXT_CONTAINS_ANY_PREFIX(BODY,'re')", false},
	} {
		for _, ordered := range []bool{false, true} {
			query := "SELECT ID FROM DOCS WHERE " + tc.predicate
			if ordered {
				query += " ORDER BY ID"
			}
			plan, err := PlanRecordQueryWithMetadata(query, template.Underlying(), nil)
			if err != nil {
				t.Fatal(err)
			}
			shape := plan.Explain()
			wantText := !ordered || !tc.residual
			if strings.Contains(shape, "TextIndexScan(BODY_TEXT") != wantText {
				t.Fatalf("%s: wrong residual/ordering access: %s", query, shape)
			}
			if tc.residual && !strings.Contains(shape, "PredicatesFilter") {
				t.Fatalf("%s: missing exact residual: %s", query, shape)
			}
			if !wantText && !strings.Contains(shape, "Scan(DOCS") {
				t.Fatalf("%s: expected ordered primary fallback: %s", query, shape)
			}
		}
	}
}

func TestFullTextScalarDuplicatePlans(t *testing.T) {
	t.Parallel()
	const schema = "CREATE TABLE docs(id BIGINT, tenant BIGINT, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body) PARTITION BY (tenant)"
	for _, predicate := range []string{"TEXT_CONTAINS_ALL(body,'red blue')", "TEXT_CONTAINS_ANY(body,'red blue')", "TEXT_CONTAINS_PHRASE(body,'red blue')", "TEXT_CONTAINS_ALL_WITHIN(body,'red blue',1)", "TEXT_CONTAINS_PREFIX(body,'re')", "TEXT_CONTAINS_ANY_PREFIX(body,'re bl')", "TEXT_CONTAINS_ALL_PREFIXES(body,'re bl')"} {
		plan, err := PlanPhysicalForTest("SELECT id FROM docs WHERE tenant=1 AND "+predicate, schema, nil)
		if err != nil {
			t.Fatal(err)
		}
		shape := plan.Explain()
		prefix := strings.Contains(predicate, "PREFIX")
		if !strings.Contains(shape, "TextIndexScan(BODY_TEXT") || strings.Contains(shape, "UnorderedPrimaryKeyDistinct") != prefix {
			t.Fatalf("wrong duplicate handling for %s: %s", predicate, shape)
		}
		if (strings.Contains(predicate, "PHRASE") || strings.Contains(predicate, "WITHIN") || strings.Contains(predicate, "ALL_PREFIXES")) && !strings.Contains(shape, "PredicatesFilter") {
			t.Fatalf("missing exact residual for %s: %s", predicate, shape)
		}
	}
}

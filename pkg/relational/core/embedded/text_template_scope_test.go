package embedded

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/expr"
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
				args := "s,'red'"
				bodyArgs := "BODY,'red'"
				if family == "TEXT_CONTAINS_ALL_WITHIN" {
					args += ",1"
					bodyArgs += ",1"
				}
				for _, callName := range []string{`"missing_` + name + `"`, name} {
					ddl := base + definition + "CREATE FUNCTION f(IN s STRING) RETURNS BOOLEAN RETURN " + callName + "(" + args + ")"
					_, err := BuildSchemaTemplateFromDDL(caseSensitiveIdentifiers(ddl))
					if callName == name {
						var lookup *api.Error
						if !errors.As(err, &lookup) || lookup.Code != api.ErrCodeUndefinedFunction {
							t.Fatalf("macro arity must fail lookup, not builtin dispatch: %s: %v", ddl, err)
						}
					} else {
						var lookup *expr.UnsupportedExpressionShapeError
						if !errors.As(err, &lookup) || lookup.Shape != fmt.Sprintf("user-defined scalar function %q", callName) {
							t.Fatalf("undefined quoted lookup: %s: %v", ddl, err)
						}
					}
				}
				if name != family {
					ddl := base + definition + "CREATE FUNCTION f(IN s STRING) RETURNS BOOLEAN RETURN " + family + "(" + args + ")"
					_, err := BuildSchemaTemplateFromDDL(caseSensitiveIdentifiers(ddl))
					var lookup *expr.UnsupportedExpressionShapeError
					if !errors.As(err, &lookup) || lookup.Shape != fmt.Sprintf("user-defined scalar function %q", `"`+family+`"`) {
						t.Fatalf("case-mismatched lookup: %s: %v", ddl, err)
					}
				}
				forward := base + "CREATE FUNCTION before_call() AS SELECT ID FROM DOCS WHERE " + name + "(" + bodyArgs + ") " + definition +
					"CREATE FUNCTION after_call() AS SELECT ID FROM DOCS WHERE " + name + "(BODY) = 'red'"
				rewritten := caseSensitiveIdentifiers(forward)
				if !strings.Contains(rewritten, "WHERE "+name+"("+strings.Replace(bodyArgs, "BODY", `"BODY"`, 1)+")") ||
					!strings.Contains(rewritten, `WHERE "`+name+`"("BODY") = 'red'`) {
					t.Fatalf("declaration-position scope: %s", rewritten)
				}
				if _, err := BuildSchemaTemplateFromDDL(rewritten); err != nil {
					t.Fatalf("builtin before declaration and macro after it: %s: %v", rewritten, err)
				}
			})
		}
	}
}

func TestFullTextTemplateTableFunctionsDoNotShadowBuiltins(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"TEXT_CONTAINS_ALL", "TEXT_CONTAINS_ANY", "TEXT_CONTAINS_PHRASE", "TEXT_CONTAINS_ALL_WITHIN", "TEXT_CONTAINS_PREFIX", "TEXT_CONTAINS_ALL_PREFIXES", "TEXT_CONTAINS_ANY_PREFIX"} {
		for _, name := range []string{strings.ToLower(family), "Text_" + strings.TrimPrefix(family, "TEXT_"), family} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				args := "BODY,'red'"
				if family == "TEXT_CONTAINS_ALL_WITHIN" {
					args += ",1"
				}
				call := name + "(" + args + ")"
				base := "CREATE SCHEMA TEMPLATE T CREATE TABLE DOCS(ID BIGINT, BODY STRING, PRIMARY KEY(ID)) " +
					"CREATE FUNCTION " + name + "() AS SELECT ID FROM DOCS "
				ddl := base + "CREATE VIEW V AS SELECT ID FROM DOCS WHERE " + call + " " +
					"CREATE FUNCTION later_call() AS SELECT ID FROM DOCS WHERE " + call
				rewritten := caseSensitiveIdentifiers(ddl)
				wantCall := name + "(" + strings.Replace(args, "BODY", `"BODY"`, 1) + ")"
				if strings.Count(rewritten, wantCall) != 2 {
					t.Fatalf("table function reserved a scalar name: %s", rewritten)
				}
				template, err := BuildSchemaTemplateFromDDL(rewritten)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := PlanRecordQueryWithMetadata("SELECT ID FROM V", template.Underlying(), nil); err != nil {
					t.Fatalf("view builtin did not plan: %v", err)
				}
				index := caseSensitiveIdentifiers(base + "CREATE INDEX X AS SELECT ID FROM DOCS WHERE " + call + " ORDER BY ID")
				if !strings.Contains(index, wantCall) {
					t.Fatalf("index clause scalar name was reserved: %s", index)
				}
			})
		}
	}
}

func TestFullTextTemplateIgnoresOuterMacroScope(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"TEXT_CONTAINS_ALL", "TEXT_CONTAINS_ANY", "TEXT_CONTAINS_PHRASE", "TEXT_CONTAINS_ALL_WITHIN", "TEXT_CONTAINS_PREFIX", "TEXT_CONTAINS_ALL_PREFIXES", "TEXT_CONTAINS_ANY_PREFIX"} {
		for _, local := range []bool{false, true} {
			args := "BODY,'red'"
			if family == "TEXT_CONTAINS_ALL_WITHIN" {
				args += ",1"
			}
			ddl := "CREATE SCHEMA TEMPLATE T CREATE TABLE DOCS(ID BIGINT, BODY STRING, PRIMARY KEY(ID)) "
			wantName := family
			if local {
				ddl += "CREATE FUNCTION " + family + "(IN body STRING) RETURNS BOOLEAN RETURN FALSE "
				args = "BODY"
				wantName = `"` + family + `"`
			}
			ddl += "CREATE VIEW V AS SELECT ID FROM DOCS WHERE " + family + "(" + args + ")"
			calls := 0
			rewritten := caseSensitiveIdentifiers(ddl, func(string) bool { calls++; return true })
			if calls != 0 || !strings.Contains(rewritten, "WHERE "+wantName+"("+strings.Replace(args, "BODY", `"BODY"`, 1)+")") {
				t.Fatalf("local=%v outer calls=%d: %s", local, calls, rewritten)
			}
			template, err := BuildSchemaTemplateFromDDL(rewritten)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := PlanRecordQueryWithMetadata("SELECT ID FROM V", template.Underlying(), nil); err != nil {
				t.Fatalf("local=%v view did not plan: %v", local, err)
			}
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

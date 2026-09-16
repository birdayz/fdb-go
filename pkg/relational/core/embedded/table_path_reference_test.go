package embedded

import (
	"errors"
	"reflect"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

func TestSelectScanTablePaths(t *testing.T) {
	t.Parallel()
	md := unnestFrontendMetadata(t)
	for _, tc := range []struct {
		name, sql    string
		scans        int
		literalOwner string
	}{
		{"base", `SELECT ID FROM T1`, 1, ""},
		{"qualified", `SELECT ID FROM s.T1`, 1, ""},
		{"joined", `SELECT a.ID FROM T1 a JOIN s.U b ON a.ID = b.ID`, 2, ""},
		{"derived", `SELECT x FROM (SELECT CAST(ARR1 AS BIGINT ARRAY) AS "a.b" FROM T1) AS "q.q", "q.q"."a.b" x`, 2, "q.q"},
		{"cte", `WITH "q.q" AS (SELECT ARR1 FROM T1) SELECT x FROM "q.q", "q.q".ARR1 x`, 2, "q.q"},
		{"derived_join", `SELECT d.ID FROM U a, (SELECT ID FROM T1) d`, 3, "D"},
		{"rebuild", `SELECT "q.q".*, x FROM (SELECT ARR1 FROM T1) AS "q.q", "q.q".ARR1 x`, 2, "q.q"},
		{"sole_derived_star", `SELECT "q.q".* FROM (SELECT ARR1 FROM T1) AS "q.q", "q.q".ARR1 x`, 2, "q.q"},
		{"exists", `SELECT ID FROM U WHERE EXISTS (SELECT 1 FROM T1, T1.ARR1 x WHERE x = U.V)`, 2, ""},
		{"exists_fast", `SELECT ID FROM U WHERE EXISTS (SELECT U.ID FROM T1, T1.ARR1 x)`, 2, ""},
		{"scalar", `SELECT ID FROM U WHERE V = (SELECT MAX(x) FROM T1, T1.ARR1 x WHERE T1.ID = U.ID)`, 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, frontend := range []string{"catalog", "visitor"} {
				t.Run(frontend, func(t *testing.T) {
					t.Parallel()
					q, err := parseQueryFromSelect(t, tc.sql)
					if err != nil {
						t.Fatal(err)
					}
					var op logical.LogicalOperator
					if frontend == "catalog" {
						op, err = buildLogicalPlanForQueryWithCatalog(q, md)
					} else {
						op, err = NewPlanVisitor(md).VisitQuery(q)
					}
					if err != nil {
						t.Fatal(err)
					}
					if err = demoteSchemaQualifiedUnnest(op, defaultEmbeddedSchema, md); err != nil {
						t.Fatal(err)
					}
					var scans []*logical.LogicalScan
					var walk func(logical.LogicalOperator)
					walk = func(node logical.LogicalOperator) {
						if node == nil {
							t.Fatal("nil plan node")
						}
						if scan, ok := node.(*logical.LogicalScan); ok {
							scans = append(scans, scan)
						}
						for _, child := range node.Children() {
							walk(child)
						}
						for _, sub := range subqueryPlans(node) {
							walk(sub)
						}
					}
					walk(op)
					if len(scans) != tc.scans {
						t.Fatalf("got %d scans, want %d: %s", len(scans), tc.scans, op.Explain(""))
					}
					literalFound := false
					for _, scan := range scans {
						if len(scan.TablePath) == 0 {
							t.Errorf("SQL scan has no captured path: %+v", scan)
						}
						if tc.literalOwner != "" && scan.Table == tc.literalOwner {
							literalFound = true
							if !reflect.DeepEqual(scan.TablePath, []string{tc.literalOwner}) {
								t.Errorf("alias carrier path %q", scan.TablePath)
							}
						}
					}
					if tc.literalOwner != "" && !literalFound {
						t.Fatalf("no literal owner %q", tc.literalOwner)
					}
					if err := resolveQualifiedTableNames(op, defaultEmbeddedSchema); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func TestTablePathQualificationIsNotCTEMembership(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		path []string
		code api.ErrorCode
	}{
		{"literal", []string{"q.q"}, ""},
		{"qualified_same_spelling", []string{"q", "q"}, api.ErrCodeUndefinedDatabase},
		{"malformed_not_legacy", []string{}, api.ErrCodeInternalError},
		{"legacy", nil, api.ErrCodeUndefinedDatabase},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scan := logical.NewScan("q.q", "", tc.path...)
			op := logical.NewCTE("q.q", logical.NewScan("T1", "", "T1"), scan, false)
			err := resolveQualifiedTableNames(op, "s")
			if tc.code == "" {
				if err != nil || scan.Table != "q.q" {
					t.Fatalf("literal alias changed: %+v / %v", scan, err)
				}
			} else {
				var coded *api.Error
				if !errors.As(err, &coded) || coded.Code != tc.code {
					t.Fatalf("got %v, want %s", err, tc.code)
				}
			}
		})
	}
	defaultAlias := logical.NewScan("s.T1", "s.T1", "s", "T1")
	explicitAlias := logical.NewScan("s.T1", "x", "s", "T1")
	for _, scan := range []*logical.LogicalScan{defaultAlias, explicitAlias} {
		if err := resolveQualifiedTableNames(scan, "s"); err != nil {
			t.Fatal(err)
		}
		if scan.Table != "T1" {
			t.Errorf("table not resolved: %+v", scan)
		}
	}
	if defaultAlias.Alias != "T1" || explicitAlias.Alias != "x" {
		t.Fatalf("default/explicit alias lockstep: %+v / %+v", defaultAlias, explicitAlias)
	}
}

func TestLateralDuplicateExpressionReference(t *testing.T) {
	t.Parallel()
	md := unnestFrontendMetadata(t)
	for _, frontend := range []string{"catalog", "visitor"} {
		t.Run(frontend, func(t *testing.T) {
			t.Parallel()
			for _, sql := range []string{
				`SELECT x FROM T1, T1.ARR1 x AT x`,
				`SELECT CAST(x AS BIGINT) FROM T1, T1.ARR1 x AT x`,
				`SELECT x + 1 FROM T1, T1.ARR1 x AT x`,
			} {
				q, err := parseQueryFromSelect(t, sql)
				if err != nil {
					t.Fatal(err)
				}
				if frontend == "catalog" {
					_, err = buildLogicalPlanForQueryWithCatalog(q, md)
				} else {
					_, err = NewPlanVisitor(md).VisitQuery(q)
				}
				var coded *api.Error
				if !errors.As(err, &coded) || coded.Code != api.ErrCodeAmbiguousColumn {
					t.Fatalf("%s: got %v, want 42702", sql, err)
				}
			}
		})
	}
}

package embedded

import (
	"errors"
	"reflect"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
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

func TestCTESourceIdentifierSegments(t *testing.T) {
	t.Parallel()
	md := unnestFrontendMetadata(t)
	for _, test := range []struct {
		name, sql string
		path      []string
		cte       bool
	}{
		{"physical", `WITH "S.T1" AS (SELECT ID FROM U) SELECT COUNT(*) FROM s.T1`, []string{"S", "T1"}, false},
		{"literal", `WITH "S.T1" AS (SELECT ID FROM U) SELECT COUNT(*) FROM "S.T1"`, []string{"S.T1"}, true},
		{"literal_projection", `WITH "S.T1" AS (SELECT ID AS OWN_ID FROM U) SELECT OWN_ID FROM "S.T1"`, []string{"S.T1"}, true},
		{"qualified_declaration", `WITH s.T1 AS (SELECT ID FROM U) SELECT COUNT(*) FROM s.T1`, []string{"S", "T1"}, true},
		{"derived", `WITH "S.T1" AS (SELECT ID FROM U) SELECT COUNT(*) FROM (SELECT p.* FROM s.T1 p) d`, []string{"S", "T1"}, false},
		{"joined", `WITH "S.T1" AS (SELECT ID FROM U) SELECT COUNT(*) FROM U u, s.T1 p`, []string{"S", "T1"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for _, frontend := range []string{"catalog", "visitor", "metadata_free"} {
				t.Run(frontend, func(t *testing.T) {
					t.Parallel()
					q, err := parseQueryFromSelect(t, test.sql)
					if err != nil {
						t.Fatal(err)
					}
					var op logical.LogicalOperator
					switch frontend {
					case "catalog":
						op, err = buildLogicalPlanForQueryWithCatalog(q, md)
					case "visitor":
						op, err = NewPlanVisitor(md).VisitQuery(q)
					case "metadata_free":
						op, err = NewPlanVisitor(nil).VisitQuery(q)
					}
					if err != nil {
						t.Fatal(err)
					}
					declaration := op.(*logical.LogicalCTE).CTEProducer
					matched := 0
					var walk func(logical.LogicalOperator)
					walk = func(node logical.LogicalOperator) {
						if scan, ok := node.(*logical.LogicalScan); ok && reflect.DeepEqual(scan.TablePath, test.path) {
							matched++
							var want *logical.CTEProducer
							if test.cte {
								want = declaration
							}
							if !scan.Source.Resolved() || scan.Source.Producer() != want {
								t.Fatalf("source %q retained %p (resolved %t), want %p", scan.TablePath, scan.Source.Producer(), scan.Source.Resolved(), want)
							}
						}
						for _, child := range node.Children() {
							walk(child)
						}
					}
					walk(op)
					if matched != 1 {
						t.Fatalf("found %d scans for path %q, want 1", matched, test.path)
					}
				})
			}
		})
	}
}

func TestSelectSourceCTEScopeUsesRetainedOwnership(t *testing.T) {
	t.Parallel()
	literal := logical.NewCTE("S.T", logical.NewScan("BASE", ""), nil, false).CTEProducer
	qualified := logical.NewCTE("S.T", logical.NewScan("OTHER", ""), nil, false, logical.CTENamePath("S", "T")).CTEProducer
	scope := semantic.ScopeSource{CTE: literal, Alias: semantic.FromNormalized("METADATA")}
	registry := map[string]semantic.ScopeSource{"S.T": scope}
	for _, test := range []struct {
		name   string
		path   []string
		source logical.ScanSource
		want   *logical.CTEProducer
		found  bool
		alias  string
	}{
		{"literal", []string{"S.T"}, logical.ScanSource{}, literal, true, "METADATA"},
		{"qualified_not_literal", []string{"S", "T"}, logical.ScanSource{}, nil, false, ""},
		{"malformed_not_legacy", []string{}, logical.ScanSource{}, nil, false, ""},
		{"legacy", nil, logical.ScanSource{}, literal, true, "METADATA"},
		{"physical", []string{"S.T"}, logical.PhysicalScanSource(), nil, false, ""},
		{"retained", []string{"S.T"}, logical.CTEScanSource(literal), literal, true, "METADATA"},
		{"retained_snapshot", []string{"S.T"}, logical.CTEScanSource(literal.WithBody(logical.NewScan("LOWERED", ""))), literal, true, "METADATA"},
		{"other_declaration_is_not_metadata", []string{"S", "T"}, logical.CTEScanSource(qualified), qualified, true, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, found := selectSourceCTEScope("S.T", test.path, test.source, registry)
			if found != test.found || got.CTE != test.want || got.Alias.Name() != test.alias {
				t.Fatalf("scope=(%p, %q, %t), want (%p, %q, %t)", got.CTE, got.Alias.Name(), found, test.want, test.alias, test.found)
			}
		})
	}
	// ON-only and global registries are alternatives for schema metadata,
	// not a new opportunity to select a different declaration.
	wrong := map[string]semantic.ScopeSource{"S.T": {CTE: qualified}}
	for _, first := range []map[string]semantic.ScopeSource{nil, wrong} {
		got, found := selectSourceCTEScope("T", []string{"T"}, logical.CTEScanSource(literal), first, registry)
		if !found || got.CTE != literal || got.Alias.Name() != "METADATA" {
			t.Fatal("earlier registry or normalized display name hid the retained producer's metadata")
		}
	}
	missing, found := selectSourceCTEScope("S.T", []string{"S.T"}, logical.CTEScanSource(literal), nil)
	if !found || missing.CTE != literal || missing.Table != nil {
		t.Fatal("missing retained schema fell through to catalog lookup instead of a tombstone")
	}
}

func TestNormalizeSelectSourcesUsesIdentifierSegments(t *testing.T) {
	t.Parallel()
	md := unnestFrontendMetadata(t)
	for _, test := range []struct {
		name string
		path []string
		want string
	}{
		{"quoted_dot", []string{"S.T1"}, "S.T1"},
		{"qualified", []string{"S", "T1"}, "T1"},
		{"legacy", nil, "T1"},
		{"malformed_not_legacy", []string{}, "S.T1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			sq := &selectQuery{
				tableName: "S.T1", tableAlias: "S.T1", sourceSegments: test.path,
				joins: []joinClause{{tableName: "S.T1", alias: "P", segments: test.path}},
			}
			normalizeSchemaQualifiedSelectSources(sq, "s", md)
			if sq.tableName != test.want || sq.tableAlias != test.want || sq.joins[0].tableName != test.want || sq.joins[0].alias != "P" {
				t.Fatalf("normalized sources = (%q, %q) / (%q, %q), want %q and explicit P alias", sq.tableName, sq.tableAlias, sq.joins[0].tableName, sq.joins[0].alias, test.want)
			}
			wantPath := test.path
			if len(test.path) == 2 {
				wantPath = test.path[1:]
			}
			if !reflect.DeepEqual(sq.sourceSegments, wantPath) || !reflect.DeepEqual(sq.joins[0].segments, wantPath) {
				t.Fatal("normalization lost captured source segments")
			}
		})
	}
}

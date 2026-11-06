package sqltest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDBFullTextSchemaMacrosKeepPrecedence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const base = "CREATE TABLE docs(id BIGINT, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body)"
	functions := []struct {
		name, args string
		indexed    bool
	}{
		{"TEXT_CONTAINS_ALL", "BODY,'red'", true},
		{"TEXT_CONTAINS_ANY", "BODY,'red'", true},
		{"TEXT_CONTAINS_PHRASE", "BODY,'red'", false},
		{"TEXT_CONTAINS_ALL_WITHIN", "BODY,'red',1", false},
		{"TEXT_CONTAINS_PREFIX", "BODY,'re'", true},
		{"TEXT_CONTAINS_ALL_PREFIXES", "BODY,'re'", false},
		{"TEXT_CONTAINS_ANY_PREFIX", "BODY,'re'", true},
	}
	ddl := base
	for _, f := range functions {
		mixed := "Text_" + strings.TrimPrefix(f.name, "TEXT_")
		ddl += " CREATE FUNCTION " + f.name + "(IN body STRING) RETURNS BOOLEAN RETURN FALSE CREATE FUNCTION \"" + mixed + "\"(IN body STRING) RETURNS BOOLEAN RETURN FALSE"
	}
	successes, lookupErrors := 0, 0
	for _, macros := range []bool{false, true} {
		schema := base
		if macros {
			schema = ddl
		}
		suffix := "fulltext_builtin_resolution"
		if macros {
			suffix = "fulltext_macro_resolution"
		}
		db := testkit.SetupPlanShapeDB(t, suffix, schema)
		if _, err := db.ExecContext(ctx, "INSERT INTO docs VALUES (1,'red blue'),(2,'blue')"); err != nil {
			t.Fatal(err)
		}
		for _, sensitive := range []bool{false, true} {
			conn := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
				if err := ec.SetOption(api.OptCaseSensitiveIdentifiers, sensitive); err != nil {
					t.Fatal(err)
				}
			})
			for _, f := range functions {
				mixed := "Text_" + strings.TrimPrefix(f.name, "TEXT_")
				for _, spelling := range []string{f.name, strings.ToLower(f.name), mixed, `"` + f.name + `"`, `"` + mixed + `"`, `"` + strings.ToLower(f.name) + `"`} {
					quoted := strings.HasPrefix(spelling, `"`)
					wantError := !macros && quoted || macros && (spelling == `"`+strings.ToLower(f.name)+`"` || sensitive && spelling == strings.ToLower(f.name))
					args := f.args
					if macros && !wantError {
						args = "BODY"
					}
					query := "SELECT ID FROM DOCS WHERE " + spelling + "(" + args + ") ORDER BY ID"
					if wantError {
						lookupErrors++
						rows, err := conn.QueryContext(ctx, query)
						if rows != nil {
							rows.Close()
						}
						var lookup *api.Error
						if !errors.As(err, &lookup) || lookup.Code != api.ErrCodeUnsupportedQuery {
							t.Fatalf("macros=%v sensitive=%v unresolved function %s: %T(%v)", macros, sensitive, query, err, err)
						}
						continue
					}
					successes++
					var plan string
					if err := conn.QueryRowContext(ctx, "EXPLAIN "+query).Scan(&plan); err != nil {
						t.Fatalf("macros=%v sensitive=%v %s: %v", macros, sensitive, query, err)
					}
					if strings.Contains(plan, "TextIndexScan(BODY_TEXT") != (!macros && f.indexed) {
						t.Fatalf("macros=%v sensitive=%v %s: %s", macros, sensitive, query, plan)
					}
					if !macros && !f.indexed && (!strings.Contains(plan, "Scan(DOCS") || !strings.Contains(plan, "PredicatesFilter")) {
						t.Fatalf("expected exact residual fallback for %s: %s", query, plan)
					}
					var want []int64
					if !macros {
						want = []int64{1}
					}
					if got := testkit.QueryIDs(t, conn, query); !reflect.DeepEqual(got, want) {
						t.Fatalf("macros=%v sensitive=%v %s: got %v want %v", macros, sensitive, query, got, want)
					}
				}
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if successes != 105 || lookupErrors != 63 {
		t.Fatalf("resolution matrix population: successes=%d lookup errors=%d", successes, lookupErrors)
	}
}

func TestFDBFullTextCaseSensitiveTemplateDeclarations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/__SYS")
	conn := testkit.PinEmbeddedConn(t, setup, func(ec *embedded.EmbeddedConnection) {
		if err := ec.SetOption(api.OptCaseSensitiveIdentifiers, true); err != nil {
			t.Fatal(err)
		}
	})
	for i, family := range []string{"TEXT_CONTAINS_ALL", "TEXT_CONTAINS_ANY", "TEXT_CONTAINS_PHRASE", "TEXT_CONTAINS_ALL_WITHIN", "TEXT_CONTAINS_PREFIX", "TEXT_CONTAINS_ALL_PREFIXES", "TEXT_CONTAINS_ANY_PREFIX"} {
		for j, name := range []string{strings.ToLower(family), "Text_" + strings.TrimPrefix(family, "TEXT_")} {
			template := fmt.Sprintf("TEXT_SCOPE_%s_%d_%d", t.Name(), i, j)
			query := "CREATE SCHEMA TEMPLATE " + template + " CREATE TABLE DOCS(ID BIGINT, BODY STRING, PRIMARY KEY(ID)) " +
				"CREATE VIEW V AS SELECT ID," + name + "(BODY) AS VAL FROM DOCS " +
				"CREATE INDEX X AS SELECT " + name + "(BODY) AS VAL FROM DOCS ORDER BY VAL " +
				"CREATE FUNCTION \"" + name + "\"(IN s STRING) RETURNS STRING RETURN s " +
				"CREATE FUNCTION f(IN s STRING) RETURNS STRING RETURN " + name + "(s) " +
				"CREATE FUNCTION tf() AS SELECT ID FROM DOCS WHERE " + name + "(BODY) = 'red'"
			if _, err := conn.ExecContext(ctx, query); err != nil {
				t.Fatalf("%s: %v", query, err)
			}
			if _, err := conn.ExecContext(ctx, "DROP SCHEMA TEMPLATE "+template); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestFDBFullTextTemplateTableFunctionsAndOuterScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	families := []string{"TEXT_CONTAINS_ALL", "TEXT_CONTAINS_ANY", "TEXT_CONTAINS_PHRASE", "TEXT_CONTAINS_ALL_WITHIN", "TEXT_CONTAINS_PREFIX", "TEXT_CONTAINS_ALL_PREFIXES", "TEXT_CONTAINS_ANY_PREFIX"}
	outerDDL := "CREATE TABLE DOCS(ID BIGINT, BODY STRING, PRIMARY KEY(ID))"
	for _, family := range families {
		outerDDL += " CREATE FUNCTION " + family + "(IN body STRING) RETURNS BOOLEAN RETURN FALSE"
	}
	db := testkit.SetupPlanShapeDB(t, "fulltext_outer_scope", outerDDL)
	var databasePath string
	conn := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		databasePath = ec.GetDBPath()
		if err := ec.SetOption(api.OptCaseSensitiveIdentifiers, true); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := conn.ExecContext(ctx, "INSERT INTO DOCS VALUES (1,'red blue'),(2,'blue')"); err != nil {
		t.Fatal(err)
	}
	for i, family := range families {
		name := strings.ToLower(family)
		args := "BODY,'red'"
		if family == "TEXT_CONTAINS_ALL_WITHIN" {
			args += ",1"
		}
		if got := testkit.QueryIDs(t, conn, "SELECT ID FROM DOCS WHERE "+family+"(BODY)"); len(got) != 0 {
			t.Fatalf("outer macro did not resolve: %s: %v", family, got)
		}
		rows, err := conn.QueryContext(ctx, "SELECT ID FROM DOCS WHERE "+name+"("+args+")")
		if rows != nil {
			rows.Close()
		}
		var lookup *api.Error
		if !errors.As(err, &lookup) || lookup.Code != api.ErrCodeUnsupportedQuery {
			t.Fatalf("outer case-mismatched macro did not reserve name %s: %v", name, err)
		}
		template := fmt.Sprintf("TEXT_OUTER_%s_%d", t.Name(), i)
		schema := fmt.Sprintf("TEXT_INNER_%d", i)
		call := name + "(" + args + ")"
		ddl := "CREATE SCHEMA TEMPLATE " + template + " CREATE TABLE DOCS(ID BIGINT, BODY STRING, PRIMARY KEY(ID)) " +
			"CREATE FUNCTION before_call() AS SELECT ID FROM DOCS WHERE " + call + " " +
			"CREATE FUNCTION " + name + "() AS SELECT ID FROM DOCS " +
			"CREATE VIEW V AS SELECT ID FROM DOCS WHERE " + call + " " +
			"CREATE FUNCTION later_call() AS SELECT ID FROM DOCS WHERE " + call
		if _, err := conn.ExecContext(ctx, ddl); err != nil {
			t.Fatalf("template must ignore outer macros and table-function names: %s: %v", ddl, err)
		}
		if _, err := conn.ExecContext(ctx, `CREATE SCHEMA "`+databasePath+"/"+schema+`" WITH TEMPLATE "`+template+`"`); err != nil {
			t.Fatal(err)
		}
		if err := conn.Raw(func(raw any) error { return raw.(*embedded.EmbeddedConnection).SetSchema(schema) }); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO DOCS VALUES (1,'red blue'),(2,'blue')"); err != nil {
			t.Fatal(err)
		}
		for _, query := range []string{"SELECT ID FROM V ORDER BY ID", "SELECT ID FROM before_call() ORDER BY ID", "SELECT ID FROM later_call() ORDER BY ID"} {
			if got := testkit.QueryIDs(t, conn, query); !reflect.DeepEqual(got, []int64{1}) {
				t.Fatalf("%s: %s: got %v want [1]", family, query, got)
			}
		}
		if err := conn.Raw(func(raw any) error { return raw.(*embedded.EmbeddedConnection).SetSchema("S") }); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, `DROP SCHEMA "`+databasePath+"/"+schema+`"`); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, `DROP SCHEMA TEMPLATE "`+template+`"`); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFDBFullTextBoundTokenizerPlanCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	factory := fulltextSQLBoundFactory
	if err := recordlayer.GlobalTextTokenizerRegistry().Register(factory); err != nil {
		t.Fatal(err)
	}
	db := testkit.SetupPlanShapeDB(t, "fulltext_bound_tokenizer", "CREATE TABLE docs(id BIGINT, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body)")
	if _, err := db.ExecContext(ctx, "INSERT INTO docs VALUES (1,'red blue'),(2,'blue red'),(3,'red')"); err != nil {
		t.Fatal(err)
	}
	logger := &testkit.SyncCaptureLogger{}
	var cache *embedded.RelationalPlanCache
	conn := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		cache = ec.SharedPlanCache()
		ec.SetPlanLogger(logger)
	})
	cache.Invalidate()
	const query = "SELECT id FROM docs WHERE TEXT_CONTAINS_ALL(body,?,?) ORDER BY id"
	for i, name := range []string{"default", factory.Name(), "default", factory.Name(), "default"} {
		want := []int64{1, 2}
		indexed := name == "default"
		if !indexed {
			want = []int64{2}
		}
		before := len(logger.Snapshot())
		if got := testkit.QueryIDs(t, conn, query, "blue", name); !reflect.DeepEqual(got, want) {
			t.Fatalf("name=%s got %v want %v", name, got, want)
		}
		events := logger.Snapshot()[before:]
		if len(events) != 1 || (events[0].Cache == embedded.PlanCacheHit) != (i >= 2) {
			t.Fatalf("iteration=%d name=%s cache events=%+v", i, name, events)
		}
		var plan string
		if err := conn.QueryRowContext(ctx, "EXPLAIN "+query, "blue", name).Scan(&plan); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(plan, "TextIndexScan(BODY_TEXT") != indexed {
			t.Fatalf("name=%s: %s", name, plan)
		}
	}
}

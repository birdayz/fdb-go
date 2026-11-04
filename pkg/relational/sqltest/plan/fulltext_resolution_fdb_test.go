package sqltest

import (
	"context"
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
	functions := []struct{ name, args string }{
		{"TEXT_CONTAINS_ALL", "BODY,'red'"},
		{"TEXT_CONTAINS_ANY", "BODY,'red'"},
		{"TEXT_CONTAINS_PHRASE", "BODY,'red'"},
		{"TEXT_CONTAINS_ALL_WITHIN", "BODY,'red',1"},
		{"TEXT_CONTAINS_PREFIX", "BODY,'re'"},
		{"TEXT_CONTAINS_ALL_PREFIXES", "BODY,'re'"},
		{"TEXT_CONTAINS_ANY_PREFIX", "BODY,'re'"},
	}
	ddl := base
	for _, f := range functions {
		mixed := "Text_" + strings.TrimPrefix(f.name, "TEXT_")
		ddl += " CREATE FUNCTION " + f.name + "(IN body STRING) RETURNS BOOLEAN RETURN FALSE CREATE FUNCTION \"" + mixed + "\"(IN body STRING) RETURNS BOOLEAN RETURN FALSE"
	}
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
					args := f.args
					if macros {
						args = "BODY"
					}
					query := "SELECT ID FROM DOCS WHERE " + spelling + "(" + args + ") ORDER BY ID"
					quoted := strings.HasPrefix(spelling, `"`)
					wantError := !macros && quoted || macros && (spelling == `"`+strings.ToLower(f.name)+`"` || sensitive && spelling == strings.ToLower(f.name))
					if wantError {
						rows, err := conn.QueryContext(ctx, query)
						if rows != nil {
							rows.Close()
						}
						if err == nil {
							t.Fatalf("macros=%v sensitive=%v undefined function resolved: %s", macros, sensitive, query)
						}
						continue
					}
					var plan string
					if err := conn.QueryRowContext(ctx, "EXPLAIN "+query).Scan(&plan); err != nil {
						t.Fatalf("macros=%v sensitive=%v %s: %v", macros, sensitive, query, err)
					}
					if strings.Contains(plan, "TextIndexScan(BODY_TEXT") != !macros {
						t.Fatalf("macros=%v sensitive=%v %s: %s", macros, sensitive, query, plan)
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

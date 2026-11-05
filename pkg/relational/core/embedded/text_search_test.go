package embedded

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
)

func TestFullTextLiteralMetadataPinsCachedPlans(t *testing.T) {
	t.Parallel()
	const schema = "CREATE TABLE docs(id BIGINT, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body)"
	logger := &captureLogger{}
	generator, metadata := newLoggingGenerator(t, schema, logger)
	var previous *cascadesPlan
	for i, tc := range []struct {
		sql string
		hit bool
	}{
		{"SELECT id FROM docs WHERE TEXT_CONTAINS_ALL_WITHIN(body,'red blue',1,'default')", false},
		{"SELECT id FROM docs WHERE TEXT_CONTAINS_ALL_WITHIN(body,'red blue',2,'default')", false},
		{"SELECT id FROM docs WHERE TEXT_CONTAINS_ALL_WITHIN(body,'red blue',2,'default')", true},
		{"SELECT id FROM docs WHERE TEXT_CONTAINS_ALL_WITHIN(body,'other',2,'default')", true},
		{"SELECT id FROM docs WHERE TEXT_CONTAINS_ALL_WITHIN(body,'other',2,'')", false},
	} {
		planned, err := generator.planSelectCascades(context.Background(), parseQuery(t, tc.sql), metadata, true, statementOptions{})
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		current := planned.(*cascadesPlan)
		if hit := logger.events[i].Cache == PlanCacheHit; hit != tc.hit {
			t.Fatalf("case %d hit=%v want=%v", i, hit, tc.hit)
		}
		if previous != nil && (previous.physicalPlan == current.physicalPlan) != tc.hit {
			t.Fatalf("case %d reused wrong metadata-specialized plan", i)
		}
		previous = current
	}
	bound, err := PlanPhysicalForTestWithBoundLiterals("SELECT id FROM docs WHERE TEXT_CONTAINS_ALL_WITHIN(body, ?, ?, ?)", schema, []driver.NamedValue{{Ordinal: 1, Value: "red blue"}, {Ordinal: 2, Value: int64(2)}, {Ordinal: 3, Value: "default"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if shape := bound.Explain(); !strings.Contains(shape, "TextIndexScan") || !strings.Contains(shape, "PredicatesFilter") {
		t.Fatalf("bound positional metadata must retain an exact residual: %s", shape)
	}
}

func TestFullTextDDLAndPlan(t *testing.T) {
	t.Parallel()
	const base = "CREATE TABLE docs(id BIGINT, tenant BIGINT, body STRING, PRIMARY KEY(id)) "
	for _, partitioned := range []bool{false, true} {
		ddl := base + "CREATE FULLTEXT INDEX body_text ON docs(body)"
		if partitioned {
			ddl += " PARTITION BY (tenant)"
		}
		template, err := BuildSchemaTemplateFromDDL(ddl)
		if err != nil {
			t.Fatal(err)
		}
		md := template.Underlying()
		index := md.GetIndex("BODY_TEXT")
		if index == nil || index.Type != recordlayer.IndexTypeText {
			t.Fatalf("index %#v", index)
		}
		query := "SELECT id FROM docs WHERE TEXT_CONTAINS_ALL(body, 'hello world')"
		if partitioned {
			query += " AND tenant=1"
		}
		plan, err := PlanRecordQueryWithMetadata(query, md, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan.Explain(), "TextIndexScan(BODY_TEXT") {
			t.Fatalf("expected text access: %s", plan.Explain())
		}
		sorted, err := PlanRecordQueryWithMetadata(query+" ORDER BY id DESC LIMIT 2", md, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sorted.Explain(), "TextIndexScan(BODY_TEXT") {
			t.Fatalf("sorted query did not retain text access: %s", sorted.Explain())
		}
		if partitioned {
			unbound, err := PlanRecordQueryWithMetadata("SELECT id FROM docs WHERE TEXT_CONTAINS_ALL(body, 'hello')", md, nil)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(unbound.Explain(), "TextIndexScan") {
				t.Fatalf("unbound grouping: %s", unbound.Explain())
			}
		}
	}
}

func TestFullTextFilteredIndexFallsBackToRecordFilter(t *testing.T) {
	t.Parallel()
	template, err := BuildSchemaTemplateFromDDL("CREATE TABLE DOCS(ID BIGINT, BODY STRING, PRIMARY KEY(ID)) CREATE FULLTEXT INDEX BODY_TEXT ON DOCS(BODY)")
	if err != nil {
		t.Fatal(err)
	}
	md := template.Underlying()
	const query = "SELECT ID FROM DOCS WHERE TEXT_CONTAINS_ALL(BODY,'red')"
	dense, err := PlanRecordQueryWithMetadata(query, md, nil)
	if err != nil || !strings.Contains(dense.Explain(), "TextIndexScan(BODY_TEXT") {
		t.Fatalf("dense positive control: plan=%v error=%v", dense, err)
	}
	index := md.GetIndex("BODY_TEXT")
	index.SetPredicate(func(proto.Message) bool { return true })
	if !index.HasFilteringPredicate() {
		t.Fatal("TEXT fixture has no filtering predicate")
	}
	filtered, err := PlanRecordQueryWithMetadata(query, md, nil)
	if err != nil {
		t.Fatal(err)
	}
	shape := filtered.Explain()
	if strings.Contains(shape, "TextIndexScan") || !strings.Contains(shape, "Scan(DOCS") || !strings.Contains(shape, "PredicatesFilter") {
		t.Fatalf("filtered TEXT index did not retain exact record filtering: %s", shape)
	}
	textPredicates := 0
	plans.Walk(filtered, func(plan plans.RecordQueryPlan) bool {
		if filter, ok := plan.(*plans.RecordQueryPredicatesFilterPlan); ok {
			for _, predicate := range filter.GetPredicates() {
				predicates.WalkPredicate(predicate, func(predicate predicates.QueryPredicate) bool {
					if comparison, ok := predicate.(*predicates.ComparisonPredicate); ok && comparison.Comparison.Type == predicates.ComparisonTextContainsAll {
						textPredicates++
					}
					return true
				})
			}
		}
		return true
	})
	if textPredicates != 1 {
		t.Fatalf("record-filter TEXT predicate count=%d want=1: %s", textPredicates, shape)
	}
}

func TestFullTextPartitionInListPlan(t *testing.T) {
	t.Parallel()
	const ddl = "CREATE TABLE docs(id BIGINT, tenant BIGINT, shard BIGINT, other BIGINT, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body) PARTITION BY (tenant, shard)"
	template, err := BuildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatal(err)
	}
	md := template.Underlying()
	for _, tc := range []struct {
		query string
		text  bool
	}{
		{"SELECT id FROM docs WHERE tenant=1 AND shard IN (1,2,3) AND TEXT_CONTAINS_ALL(body,'red blue')", true},
		{"SELECT id FROM docs WHERE tenant IN (1,2) AND shard IN (3,4) AND TEXT_CONTAINS_ANY(body,'red')", true},
		{"SELECT id FROM docs WHERE tenant=1 AND shard=2 AND TEXT_CONTAINS_ALL(body,'red')", true},
		// A grouping value from the scanned row is not a binding.
		{"SELECT id FROM docs WHERE tenant=1 AND shard=other AND TEXT_CONTAINS_ALL(body,'red')", false},
		{"SELECT id FROM docs WHERE tenant=1 AND TEXT_CONTAINS_ALL(body,'red')", false},
	} {
		plan, err := PlanRecordQueryWithMetadata(tc.query, md, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		if got := strings.Contains(plan.Explain(), "TextIndexScan(BODY_TEXT"); got != tc.text {
			t.Fatalf("%s: text access=%v: %s", tc.query, got, plan.Explain())
		}
		// An IN list is an IN join: the scan runs once per list element.
		if strings.Contains(tc.query, " IN ") && !strings.HasPrefix(plan.Explain(), "InJoin(") {
			t.Fatalf("%s: no IN join: %s", tc.query, plan.Explain())
		}
	}
}

func TestFullTextDDLRejectsInvalidShape(t *testing.T) {
	t.Parallel()
	const base = "CREATE TABLE docs(id BIGINT, n BIGINT, tags STRING ARRAY, body STRING, PRIMARY KEY(id)) "
	for _, tc := range []struct{ ddl, message string }{
		{"CREATE FULLTEXT INDEX bad ON docs(n)", `column "N" must be scalar string`},
		{"CREATE FULLTEXT INDEX bad ON docs(body,id)", "exactly one indexed column"},
		{"CREATE FULLTEXT INDEX bad ON docs(body DESC)", "per-column ordering"},
		{"CREATE FULLTEXT INDEX bad ON docs(missing)", `unknown column "MISSING"`},
		{"CREATE FULLTEXT INDEX bad ON missing(body)", `references unknown table "MISSING"`},
		{"CREATE FULLTEXT INDEX bad ON docs(body) PARTITION BY (n DESC)", "PARTITION BY \"BAD\": per-column ordering"},
		{"CREATE FULLTEXT INDEX bad ON docs(body) PARTITION BY (missing)", `unknown column "MISSING"`},
		{"CREATE FULLTEXT INDEX bad ON docs(body) PARTITION BY (tags)", `column "TAGS" must be scalar`},
	} {
		if _, err := BuildSchemaTemplateFromDDL(base + tc.ddl); err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Fatalf("%s: error=%v, want %q", tc.ddl, err, tc.message)
		}
	}
}

// TestFullTextIndexSourceResolvesEscapedTableButNotAQualifier mirrors
// TestDDL_IndexOnSourceResolvesEscapedTableButNotAQualifier for FULLTEXT.
func TestFullTextIndexSourceResolvesEscapedTableButNotAQualifier(t *testing.T) {
	t.Parallel()
	if _, err := BuildSchemaTemplateFromDDL(`CREATE TABLE "MY$TABLE" (id BIGINT, body STRING, PRIMARY KEY (id)) CREATE FULLTEXT INDEX t ON "MY$TABLE" (body)`); err != nil {
		t.Fatalf("escaped table: %v", err)
	}
	for _, ddl := range []string{
		`CREATE TABLE "S.T" (id BIGINT, body STRING, PRIMARY KEY (id)) CREATE FULLTEXT INDEX t ON S.T (body)`,
		`CREATE TABLE t (id BIGINT, body STRING, PRIMARY KEY (id)) CREATE FULLTEXT INDEX t ON S.T (body)`,
	} {
		if _, err := BuildSchemaTemplateFromDDL(ddl); err == nil || !strings.Contains(err.Error(), `references unknown table "S.T"`) {
			t.Fatalf("%s: error=%v", ddl, err)
		}
	}
}

// TestFullTextJoinAndProjectionPlans: a join column binds the partition, and
// a text predicate is a boolean in a projection.
func TestFullTextJoinAndProjectionPlans(t *testing.T) {
	t.Parallel()
	template, err := BuildSchemaTemplateFromDDL("CREATE TABLE tenants(id BIGINT, PRIMARY KEY(id)) CREATE TABLE docs(id BIGINT, tenant BIGINT, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body) PARTITION BY (tenant)")
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"SELECT d.id FROM tenants t, docs d WHERE d.tenant = t.id AND TEXT_CONTAINS_ALL(d.body,'red')",
		"SELECT id, TEXT_CONTAINS_ALL(body,'red') FROM docs WHERE tenant = 1",
	} {
		plan, err := PlanRecordQueryWithMetadata(query, template.Underlying(), nil)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if strings.Contains(query, "tenants t") && !strings.Contains(plan.Explain(), "TextIndexScan(BODY_TEXT") {
			t.Fatalf("join did not bind text partition: %s", plan.Explain())
		}
	}
}

func TestFullTextCaseSensitiveAndMacroResolution(t *testing.T) {
	t.Parallel()
	const base = "CREATE TABLE docs(id BIGINT, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body)"
	for _, name := range []string{"TEXT_CONTAINS_ALL", "TEXT_CONTAINS_ANY", "TEXT_CONTAINS_PHRASE", "TEXT_CONTAINS_ALL_WITHIN", "TEXT_CONTAINS_PREFIX", "TEXT_CONTAINS_ALL_PREFIXES", "TEXT_CONTAINS_ANY_PREFIX"} {
		mixed := "Text_" + strings.TrimPrefix(name, "TEXT_")
		template, err := BuildSchemaTemplateFromDDL(base + " CREATE FUNCTION " + name + "(IN body STRING) RETURNS BOOLEAN RETURN FALSE CREATE FUNCTION \"" + mixed + "\"(IN body STRING) RETURNS BOOLEAN RETURN FALSE")
		if err != nil {
			t.Fatal(err)
		}
		for _, sensitive := range []bool{false, true} {
			for _, spelling := range []string{name, strings.ToLower(name), mixed, `"` + name + `"`, `"` + mixed + `"`, `"` + strings.ToLower(name) + `"`} {
				wantError := spelling == `"`+strings.ToLower(name)+`"` || sensitive && spelling == strings.ToLower(name)
				args := "BODY"
				if wantError {
					args += ",'red'"
					if name == "TEXT_CONTAINS_ALL_WITHIN" {
						args += ",1"
					}
				}
				query := "SELECT ID FROM DOCS WHERE " + spelling + "(" + args + ")"
				if sensitive {
					query = caseSensitiveIdentifiers(query, func(string) bool { return true })
				}
				plan, err := PlanRecordQueryWithMetadata(query, template.Underlying(), nil)
				if wantError {
					var lookup *api.Error
					if !errors.As(err, &lookup) || lookup.Code != api.ErrCodeUnsupportedQuery {
						t.Fatalf("sensitive=%v unresolved function %s: %T(%v)", sensitive, query, err, err)
					}
					continue
				}
				if err != nil || strings.Contains(plan.Explain(), "TextIndexScan") {
					t.Fatalf("sensitive=%v %s: plan=%v err=%v", sensitive, query, plan, err)
				}
			}
		}
	}
	template, err := BuildSchemaTemplateFromDDL(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"SELECT ID FROM DOCS WHERE text_contains_all(BODY,'red')", caseSensitiveIdentifiers("SELECT ID FROM DOCS WHERE text_contains_all(BODY,'red')")} {
		plan, err := PlanRecordQueryWithMetadata(query, template.Underlying(), nil)
		if err != nil || !strings.Contains(plan.Explain(), "TextIndexScan(BODY_TEXT") {
			t.Fatalf("no-macro control %s: plan=%v err=%v", query, plan, err)
		}
	}
	_, err = PlanRecordQueryWithMetadata(`SELECT ID FROM DOCS WHERE "TEXT_CONTAINS_ALL"(BODY,'red')`, template.Underlying(), nil)
	var lookup *api.Error
	if !errors.As(err, &lookup) || lookup.Code != api.ErrCodeUnsupportedQuery {
		t.Fatalf("quoted undefined function error=%T(%v)", err, err)
	}
}

func TestFullTextUnknownTokenizerPreservesCause(t *testing.T) {
	t.Parallel()
	_, err := PlanPhysicalForTestWithBoundLiterals("SELECT id FROM docs WHERE TEXT_CONTAINS_ALL(body,'red','not_registered_text_test')", "CREATE TABLE docs(id BIGINT, body STRING, PRIMARY KEY(id))", nil, nil, nil)
	var metadataError *recordlayer.MetaDataError
	if !errors.As(err, &metadataError) {
		t.Fatalf("unknown tokenizer lost metadata cause: %T %v", err, err)
	}
}

func TestFullTextDDLDeclinesTokenizedPrimaryKey(t *testing.T) {
	t.Parallel()
	_, err := BuildSchemaTemplateFromDDL("CREATE TABLE docs(id STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(id)")
	if err == nil || !strings.Contains(err.Error(), "tokenized column cannot be part of the primary key") {
		t.Fatalf("tokenized primary key error=%v", err)
	}
}

func TestFullTextSQLTypeAndArity(t *testing.T) {
	t.Parallel()
	template, err := BuildSchemaTemplateFromDDL("CREATE TABLE docs(id BIGINT, body STRING, PRIMARY KEY(id))")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ predicate, message string }{
		{"TEXT_CONTAINS_ALL(id, 'hello')", "argument 1 must be STRING"},
		{"TEXT_CONTAINS_ANY(body)", "requires 2 arguments"},
		{"TEXT_CONTAINS_PHRASE(body, 7)", "argument 2 must be STRING"},
		{"TEXT_CONTAINS_ALL_WITHIN(body, 'hello', -1)", "distance must be a positive integer"},
		{"TEXT_CONTAINS_ALL_WITHIN(body, 'hello', 0)", "distance must be a positive integer"},
		{"TEXT_CONTAINS_ALL_WITHIN(body, 'hello', 1.5)", "distance must be an integer constant, got float64"},
		{"TEXT_CONTAINS_ALL_WITHIN(body, 'hello', id)", "distance must be an integer constant"},
		{"TEXT_CONTAINS_ALL(body, 'hello', id)", "tokenizer must be a constant STRING"},
		{"TEXT_CONTAINS_ALL(body, 'hello', 7)", "tokenizer must be a constant STRING"},
		{"TEXT_CONTAINS_ALL(body, 'hello', 'not_registered')", `tokenizer "not_registered" is not registered`},
	} {
		if _, err := PlanRecordQueryWithMetadata("SELECT id FROM docs WHERE "+tc.predicate, template.Underlying(), nil); err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Fatalf("%s: error=%v, want %q", tc.predicate, err, tc.message)
		}
	}
	for _, predicate := range []string{"TEXT_CONTAINS_ALL(body,'hello')", "TEXT_CONTAINS_ALL(body,'hello','default')", "TEXT_CONTAINS_ANY(body,'hello')", "TEXT_CONTAINS_PHRASE(body,'hello')", "TEXT_CONTAINS_ALL_WITHIN(body,'hello',2)", "TEXT_CONTAINS_PREFIX(body,'hel')", "TEXT_CONTAINS_ALL_PREFIXES(body,'hel wor')", "TEXT_CONTAINS_ANY_PREFIX(body,'hel wor')"} {
		if _, err := PlanRecordQueryWithMetadata("SELECT id FROM docs WHERE "+predicate, template.Underlying(), nil); err != nil {
			t.Fatalf("%s: %v", predicate, err)
		}
	}
}

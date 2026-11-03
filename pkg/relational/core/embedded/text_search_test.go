package embedded

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
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
	if !strings.Contains(bound.Explain(), "TextIndexScan") {
		t.Fatalf("bound metadata plan=%s", bound.Explain())
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
	}
}

func TestFullTextDDLRejectsInvalidShape(t *testing.T) {
	t.Parallel()
	const base = "CREATE TABLE docs(id BIGINT, body STRING, PRIMARY KEY(id)) "
	for _, ddl := range []string{"CREATE FULLTEXT INDEX bad ON docs(id)", "CREATE FULLTEXT INDEX bad ON docs(body,id)", "CREATE FULLTEXT INDEX bad ON docs(body DESC)", "CREATE FULLTEXT INDEX bad ON docs(missing)", "CREATE FULLTEXT INDEX bad ON missing(body)"} {
		if _, err := BuildSchemaTemplateFromDDL(base + ddl); err == nil {
			t.Fatalf("accepted %s", ddl)
		}
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
	for _, predicate := range []string{"TEXT_CONTAINS_ALL(id, 'hello')", "TEXT_CONTAINS_ANY(body)", "TEXT_CONTAINS_PHRASE(body, 7)", "TEXT_CONTAINS_ALL_WITHIN(body, 'hello', -1)", "TEXT_CONTAINS_ALL(body, 'hello', id)", "TEXT_CONTAINS_ALL_WITHIN(body, 'hello', 1.5)"} {
		if _, err := PlanRecordQueryWithMetadata("SELECT id FROM docs WHERE "+predicate, template.Underlying(), nil); err == nil {
			t.Fatalf("accepted %s", predicate)
		}
	}
	for _, predicate := range []string{"TEXT_CONTAINS_ALL(body,'hello')", "TEXT_CONTAINS_ALL(body,'hello','default')", "TEXT_CONTAINS_ANY(body,'hello')", "TEXT_CONTAINS_PHRASE(body,'hello')", "TEXT_CONTAINS_ALL_WITHIN(body,'hello',2)", "TEXT_CONTAINS_PREFIX(body,'hel')", "TEXT_CONTAINS_ALL_PREFIXES(body,'hel wor')", "TEXT_CONTAINS_ANY_PREFIX(body,'hel wor')"} {
		if _, err := PlanRecordQueryWithMetadata("SELECT id FROM docs WHERE "+predicate, template.Underlying(), nil); err != nil {
			t.Fatalf("%s: %v", predicate, err)
		}
	}
}

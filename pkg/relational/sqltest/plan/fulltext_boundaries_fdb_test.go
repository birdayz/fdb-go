package sqltest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDBFullTextRowDependentQueryTermsUseRecordFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "fulltext_row_terms",
		"CREATE TABLE docs(id BIGINT, body STRING, search_terms STRING, PRIMARY KEY(id)) "+
			"CREATE FULLTEXT INDEX body_text ON docs(body) "+
			"CREATE TABLE plain_docs(id BIGINT, body STRING, search_terms STRING, PRIMARY KEY(id)) "+
			"CREATE TABLE requests(id BIGINT, doc_id BIGINT, term STRING, PRIMARY KEY(id))")
	for _, table := range []string{"docs", "plain_docs"} {
		if _, err := db.ExecContext(ctx, "INSERT INTO "+table+" VALUES "+
			"(1,'red','red'), (2,'blue','blue'), (3,'red','blue'), "+
			"(4,'red green','green'), (5,'red','blue'), (6,'green','red')"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO requests VALUES "+
		"(11,1,'red'), (12,1,'blue'), (13,2,'blue'), (14,2,'red'), "+
		"(15,3,'red'), (16,4,'green'), (17,5,'red'), (18,6,'blue')"); err != nil {
		t.Fatal(err)
	}

	// Literal terms must still use the available index on the same documents.
	control := "SELECT id FROM docs WHERE TEXT_CONTAINS_ALL(body,'red') ORDER BY id"
	if plan := testkit.ExplainVia(t, ctx, db, control); !strings.Contains(plan, "TextIndexScan(BODY_TEXT") {
		t.Fatalf("literal positive control did not use text access: %s", plan)
	}
	if got := testkit.GetInts(t, ctx, db, control); !reflect.DeepEqual(got, []int64{1, 3, 4, 5}) {
		t.Fatalf("literal positive control: got %v want [1 3 4 5]", got)
	}

	for _, tc := range []struct {
		name  string
		query string
		want  []int64
	}{
		{"same row", "SELECT id FROM %s WHERE TEXT_CONTAINS_ALL(body,search_terms) ORDER BY id", []int64{1, 2, 4}},
		{"joined row", "SELECT r.id FROM requests r, %s d WHERE d.id=r.doc_id AND TEXT_CONTAINS_ALL(d.body,r.term) ORDER BY r.id", []int64{11, 13, 15, 16, 17}},
	} {
		for _, table := range []string{"docs", "plain_docs"} {
			query := fmt.Sprintf(tc.query, table)
			if plan := testkit.ExplainVia(t, ctx, db, query); strings.Contains(plan, "TextIndexScan") {
				t.Fatalf("%s/%s used text access for row-dependent terms: %s", tc.name, table, plan)
			}
			if got := testkit.GetInts(t, ctx, db, query); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s/%s: got %v want %v", tc.name, table, got, tc.want)
			}
		}
	}
}

func TestFDBFullTextUUIDPartitions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "fulltext_uuid_partitions",
		"CREATE TABLE docs(id BIGINT, tenant UUID, body STRING, PRIMARY KEY(id)) "+
			"CREATE FULLTEXT INDEX body_text ON docs(body) PARTITION BY (tenant) "+
			"CREATE TABLE plain_docs(id BIGINT, tenant UUID, body STRING, PRIMARY KEY(id))")
	const (
		tenantA = "550e8400-e29b-41d4-a716-446655440000"
		tenantB = "550e8400-e29b-41d4-a716-446655440001"
		tenantC = "7f000000-0000-0000-0000-0000000000ff"
	)
	for _, table := range []string{"docs", "plain_docs"} {
		query := fmt.Sprintf("INSERT INTO %s VALUES "+
			"(1,'%s','red blue'), (2,'%s','blue'), (3,'%s','red'), "+
			"(4,'%s','green'), (5,'%s','red'), (6,'%s','red blue'), (7,'%s','red')",
			table, tenantA, tenantA, tenantB, tenantB, tenantC, tenantC, tenantA)
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, tc := range []struct {
		where string
		args  []any
		want  []int64
	}{
		{"tenant=? AND TEXT_CONTAINS_ALL(body,?)", []any{tenantA, "red"}, []int64{1, 7}},
		{"tenant=? AND TEXT_CONTAINS_ALL(body,?)", []any{tenantB, "red"}, []int64{3}},
		{"tenant=? AND TEXT_CONTAINS_ALL(body,?)", []any{tenantC, "red"}, []int64{5, 6}},
		{"tenant=? AND TEXT_CONTAINS_ALL(body,?)", []any{tenantA, "red"}, []int64{1, 7}},
		{fmt.Sprintf("tenant='%s' AND TEXT_CONTAINS_ALL(body,'red')", tenantB), nil, []int64{3}},
		{fmt.Sprintf("tenant IN ('%s','%s') AND TEXT_CONTAINS_ALL(body,'red')", tenantA, tenantC), nil, []int64{1, 5, 6, 7}},
		{fmt.Sprintf("tenant IN ('%s','%s','%s') AND TEXT_CONTAINS_ALL(body,'red')", tenantB, tenantA, tenantB), nil, []int64{1, 3, 7}},
		{"tenant IN (?,?) AND TEXT_CONTAINS_ALL(body,?)", []any{tenantA, tenantB, "red"}, []int64{1, 3, 7}},
		{"tenant IN (?,?) AND TEXT_CONTAINS_ALL(body,?)", []any{tenantB, tenantC, "red"}, []int64{3, 5, 6}},
	} {
		for _, table := range []string{"docs", "plain_docs"} {
			query := "SELECT id FROM " + table + " WHERE " + tc.where + " ORDER BY id"
			plan := explainFullText(t, ctx, db, query, tc.args...)
			if usesText := strings.Contains(plan, "TextIndexScan(BODY_TEXT"); usesText != (table == "docs") {
				t.Fatalf("%s args=%v: unexpected text access: %s", query, tc.args, plan)
			}
			if got := testkit.QueryIDs(t, conn, query, tc.args...); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s args=%v: got %v want %v", query, tc.args, got, tc.want)
			}
		}
	}
}

func TestFullTextPartitionDDLRejectsNonScalars(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ddl  string
	}{
		{"struct", "CREATE TYPE AS STRUCT tenant_type (n BIGINT) CREATE TABLE docs(id BIGINT, tenant tenant_type, body STRING, PRIMARY KEY(id)) "},
		{"array", "CREATE TABLE docs(id BIGINT, tenant BIGINT ARRAY, body STRING, PRIMARY KEY(id)) "},
		{"uuid array", "CREATE TABLE docs(id BIGINT, tenant UUID ARRAY, body STRING, PRIMARY KEY(id)) "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := embedded.BuildSchemaTemplateFromDDL(tc.ddl); err != nil {
				t.Fatalf("base schema must be valid before adding text partition: %v", err)
			}
			_, err := embedded.BuildSchemaTemplateFromDDL(tc.ddl + "CREATE FULLTEXT INDEX body_text ON docs(body) PARTITION BY (tenant)")
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidSchemaTemplate || !strings.Contains(apiErr.Message, `column "TENANT" must be scalar`) {
				t.Fatalf("non-scalar partition: error=%v, want INVALID_SCHEMA_TEMPLATE identifying TENANT as non-scalar", err)
			}
		})
	}
}

package sqltest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

func explainFullText(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var plan string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+query, args...).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN %s: %v", query, err)
	}
	return plan
}

func TestFDBFullTextSQL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "fulltext", "CREATE TABLE docs(id BIGINT, tenant BIGINT, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body) PARTITION BY (tenant) CREATE TABLE plain_docs(id BIGINT, tenant BIGINT, body STRING, PRIMARY KEY(id))")
	for _, sql := range []string{
		"INSERT INTO docs VALUES (1,1,'red blue red'), (2,1,'blue green'), (3,1,'red other blue'), (4,1,'prefix prelude'), (5,1,'CAFÉ red'), (6,2,'red blue'), (7,1,'red red'), (8,1,NULL), (9,1,''), (10,1,'prefix alone')",
	} {
		if _, err := db.ExecContext(ctx, sql); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, strings.Replace(sql, "INSERT INTO docs", "INSERT INTO plain_docs", 1)); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		predicate string
		want      []int64
	}{
		{"TEXT_CONTAINS_ALL(body,'red blue')", []int64{1, 3}},
		{"TEXT_CONTAINS_ANY(body,'red green')", []int64{1, 2, 3, 5, 7}},
		{"TEXT_CONTAINS_PHRASE(body,'red blue')", []int64{1}},
		{"TEXT_CONTAINS_PHRASE(body,'red red')", []int64{7}},
		{"TEXT_CONTAINS_ALL_WITHIN(body,'blue red',2)", []int64{1, 3}},
		{"TEXT_CONTAINS_PREFIX(body,'pre')", []int64{4, 10}},
		{"TEXT_CONTAINS_ALL_PREFIXES(body,'pr prel')", []int64{4}},
		{"TEXT_CONTAINS_ALL_PREFIXES(body,'pr missing')", nil},
		{"TEXT_CONTAINS_ALL(body,'red') AND TEXT_CONTAINS_ALL(body,'blue')", []int64{1, 3}},
		{"TEXT_CONTAINS_ANY_PREFIX(body,'re pre')", []int64{1, 3, 4, 5, 7, 10}},
		{"TEXT_CONTAINS_ALL(body,'cafe')", []int64{5}},
		{"TEXT_CONTAINS_ALL(body,'absent')", nil},
		{"TEXT_CONTAINS_ALL(body,NULL)", nil},
		{"TEXT_CONTAINS_ALL(body,'red','default')", []int64{1, 3, 5, 7}},
	}
	for _, tc := range cases {
		query := "SELECT id FROM docs WHERE tenant=1 AND " + tc.predicate + " ORDER BY id"
		explain := testkit.ExplainVia(t, ctx, db, query)
		if !strings.Contains(explain, "TextIndexScan(BODY_TEXT") {
			t.Fatalf("%s: %s", query, explain)
		}
		if strings.Contains(tc.predicate, "ALL_PREFIXES") && !strings.Contains(explain, "PredicatesFilter") {
			t.Fatalf("strict partitioned prefix lost its residual: %s", explain)
		}
		if got := testkit.GetInts(t, ctx, db, query); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: got %v want %v", query, got, tc.want)
		}
		for _, fallback := range []string{
			"SELECT id FROM plain_docs WHERE tenant=1 AND " + tc.predicate + " ORDER BY id",
			"SELECT id FROM docs WHERE tenant>=1 AND tenant<2 AND " + tc.predicate + " ORDER BY id",
		} {
			if plan := testkit.ExplainVia(t, ctx, db, fallback); strings.Contains(plan, "TextIndexScan") {
				t.Fatalf("expected residual text evaluation: %s", plan)
			}
			if actual := testkit.GetInts(t, ctx, db, fallback); !reflect.DeepEqual(actual, tc.want) {
				t.Fatalf("%s got %v want %v", fallback, actual, tc.want)
			}
		}
	}
	for _, tc := range []struct {
		order string
		want  []int64
	}{{"ASC", []int64{2, 3}}, {"DESC", []int64{5, 3}}} {
		query := "SELECT id FROM docs WHERE tenant=1 AND TEXT_CONTAINS_ANY(body,'red green') ORDER BY id " + tc.order + " LIMIT 2 OFFSET 1"
		if plan := testkit.ExplainVia(t, ctx, db, query); !strings.Contains(plan, "TextIndexScan(BODY_TEXT") {
			t.Fatalf("LIMIT/OFFSET lost text access: %s", plan)
		}
		if got := testkit.GetInts(t, ctx, db, query); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: got %v want %v", query, got, tc.want)
		}
	}
	// '' is no token: TextScan reads nothing (TextScan.java:181-183) and
	// TextComparison.eval refuses it (Comparisons.java:2588-2590).
	emptyPrefix := "SELECT id FROM %s WHERE tenant%s AND TEXT_CONTAINS_PREFIX(body,'') ORDER BY id"
	if plan := testkit.ExplainVia(t, ctx, db, fmt.Sprintf(emptyPrefix, "docs", "=1")); !strings.Contains(plan, "TextIndexScan(BODY_TEXT") {
		t.Fatalf("empty prefix: %s", plan)
	}
	var emptyPrefixID int64
	if err := db.QueryRowContext(ctx, fmt.Sprintf(emptyPrefix, "docs", "=1")).Scan(&emptyPrefixID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("indexed empty prefix: id=%d err=%v", emptyPrefixID, err)
	}
	for _, query := range []string{fmt.Sprintf(emptyPrefix, "plain_docs", "=1"), fmt.Sprintf(emptyPrefix, "docs", ">=1")} {
		if err := db.QueryRowContext(ctx, query).Scan(&emptyPrefixID); err == nil || !strings.Contains(err.Error(), "Cannot evaluate prefix comparison with multiple tokens") {
			t.Fatalf("%s: id=%d err=%v", query, emptyPrefixID, err)
		}
	}
	if _, err := db.QueryContext(ctx, "SELECT id FROM docs WHERE TEXT_CONTAINS_ALL(body,'red','not_registered_fulltext_tokenizer')"); err == nil || !strings.Contains(err.Error(), "not_registered_fulltext_tokenizer") {
		t.Fatalf("unknown tokenizer: %v", err)
	}
	if _, err := db.QueryContext(ctx, "SELECT id FROM plain_docs WHERE TEXT_CONTAINS_ALL_WITHIN(body,'red blue',0)"); err == nil || !strings.Contains(err.Error(), "positive integer") {
		t.Fatalf("distance 0: %v", err)
	}
	// A text predicate is a boolean column, evaluated on each row.
	for _, tc := range []struct {
		id   int64
		want sql.NullBool
	}{{1, sql.NullBool{Bool: true, Valid: true}}, {7, sql.NullBool{Valid: true}}, {8, sql.NullBool{}}} {
		var projected sql.NullBool
		if err := db.QueryRowContext(ctx, "SELECT TEXT_CONTAINS_ALL(body,'blue') FROM docs WHERE id=?", tc.id).Scan(&projected); err != nil || projected != tc.want {
			t.Fatalf("id=%d projected text predicate=%v want=%v err=%v", tc.id, projected, tc.want, err)
		}
	}
	query := "SELECT id FROM docs WHERE tenant=? AND TEXT_CONTAINS_ALL(body,?) AND id>1 ORDER BY id DESC LIMIT 1"
	for _, tc := range []struct {
		term string
		want int64
	}{{"red", 7}, {"blue", 3}, {"cafe", 5}} {
		if plan := explainFullText(t, ctx, db, query, int64(1), tc.term); !strings.Contains(plan, "TextIndexScan(BODY_TEXT") {
			t.Fatalf("term=%s: %s", tc.term, plan)
		}
		var id int64
		if err := db.QueryRowContext(ctx, query, int64(1), tc.term).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if id != tc.want {
			t.Fatalf("term=%s id=%d want %d", tc.term, id, tc.want)
		}
	}
	for _, tc := range []struct {
		distance int64
		count    int64
	}{{1, 1}, {2, 2}, {1, 1}, {2, 2}} {
		query := "SELECT COUNT(*) FROM docs WHERE tenant=1 AND TEXT_CONTAINS_ALL_WITHIN(body,'red blue',?)"
		if plan := explainFullText(t, ctx, db, query, tc.distance); !strings.Contains(plan, "TextIndexScan(BODY_TEXT") {
			t.Fatalf("distance=%d: %s", tc.distance, plan)
		}
		var count int64
		if err := db.QueryRowContext(ctx, query, tc.distance).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != tc.count {
			t.Fatalf("distance=%d cached result=%d want=%d", tc.distance, count, tc.count)
		}
	}
	for _, distance := range []int64{0, -1} {
		var count int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM docs WHERE tenant=1 AND TEXT_CONTAINS_ALL_WITHIN(body,'red blue',?)", distance).Scan(&count); err == nil || !strings.Contains(err.Error(), "positive integer") {
			t.Fatalf("cached distance=%d count=%d err=%v", distance, count, err)
		}
	}
	if _, err := db.ExecContext(ctx, "UPDATE docs SET body='yellow' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM docs WHERE id=3"); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM docs WHERE tenant=1 AND TEXT_CONTAINS_ALL(body,'blue')").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("maintenance count=%d", count)
	}
}

// TestFDBFullTextPartitionInList: IN lists and join columns bind partitions;
// one scan runs per element, as under Java's RecordQueryInValuesJoinPlan.
func TestFDBFullTextPartitionInList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "fulltext_in_list", "CREATE TABLE docs(id BIGINT, tenant BIGINT, shard BIGINT, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body) PARTITION BY (tenant, shard) CREATE TABLE plain_docs(id BIGINT, tenant BIGINT, shard BIGINT, body STRING, PRIMARY KEY(id)) CREATE TABLE shards(id BIGINT, tenant BIGINT, shard BIGINT, PRIMARY KEY(id))")
	for _, table := range []string{"docs", "plain_docs"} {
		if _, err := db.ExecContext(ctx, "INSERT INTO "+table+" VALUES (1,1,0,'red blue'), (2,1,1,'red blue'), (3,1,2,'red'), (4,1,3,'blue red'), (5,2,1,'red blue'), (6,1,1,'prefix prelude'), (7,1,2,'prelude red blue'), (8,2,0,'blue')"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO shards VALUES (1,1,1), (2,1,2), (3,2,0)"); err != nil {
		t.Fatal(err)
	}
	collect := func(query string, args ...any) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got = append(got, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return got
	}
	for _, tc := range []struct {
		where string
		args  []any
		want  []int64
	}{
		// An unbound partition column falls back to record filtering.
		{"tenant=1 AND TEXT_CONTAINS_ALL(body,'red blue')", nil, []int64{1, 2, 4, 7}},
		{"tenant=1 AND shard IN (1,2,3) AND TEXT_CONTAINS_ALL(body,'red blue')", nil, []int64{2, 4, 7}},
		{"tenant=1 AND shard IN (2,1,2) AND TEXT_CONTAINS_ALL(body,'red blue')", nil, []int64{2, 7}},
		{"tenant IN (1,2) AND shard IN (0,1) AND TEXT_CONTAINS_ANY(body,'blue')", nil, []int64{1, 2, 5, 8}},
		{"tenant=? AND shard IN (?,?) AND TEXT_CONTAINS_ALL(body,?)", []any{int64(1), int64(0), int64(2), "red"}, []int64{1, 3, 7}},
		{"tenant=1 AND shard IN (1,2) AND TEXT_CONTAINS_ALL_PREFIXES(body,'pre re')", nil, []int64{7}},
		{"tenant=1 AND shard IN (1,2) AND TEXT_CONTAINS_PHRASE(body,'red blue')", nil, []int64{2, 7}},
	} {
		query := "SELECT id FROM docs WHERE " + tc.where + " ORDER BY id"
		plan := explainFullText(t, ctx, db, query, tc.args...)
		bound := strings.Contains(tc.where, " IN ")
		if strings.Contains(plan, "TextIndexScan(BODY_TEXT") != bound || bound && !strings.Contains(plan, "InJoin(") {
			t.Fatalf("%s: %s", query, plan)
		}
		if got := collect(query, tc.args...); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: got %v want %v", query, got, tc.want)
		}
		if got := collect("SELECT id FROM plain_docs WHERE "+tc.where+" ORDER BY id", tc.args...); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("plain %s: got %v want %v", tc.where, got, tc.want)
		}
	}
	// Each outer row's columns bind the partition of the inner text scan.
	for _, table := range []string{"docs", "plain_docs"} {
		join := "SELECT d.id FROM shards s, " + table + " d WHERE d.tenant = s.tenant AND d.shard = s.shard AND TEXT_CONTAINS_ALL(d.body,'red blue') ORDER BY d.id"
		if plan := testkit.ExplainVia(t, ctx, db, join); strings.Contains(plan, "TextIndexScan(BODY_TEXT") != (table == "docs") {
			t.Fatalf("%s: %s", join, plan)
		}
		if got := collect(join); !reflect.DeepEqual(got, []int64{2, 7}) {
			t.Fatalf("%s: got %v", join, got)
		}
	}
}

func TestFDBFullTextQuantifiers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "fulltext_quantifiers", "CREATE TABLE tenants(id BIGINT, PRIMARY KEY(id)) CREATE TABLE docs(id BIGINT, tenant BIGINT, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body) CREATE TABLE plain_docs(id BIGINT, tenant BIGINT, body STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO tenants VALUES (1),(2),(3)"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"docs", "plain_docs"} {
		if _, err := db.ExecContext(ctx, "INSERT INTO "+table+" VALUES (1,1,'red'),(2,1,'blue'),(3,2,'blue')"); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		query string
		want  []int64
		shape string
	}{
		{"SELECT t.id FROM tenants t WHERE EXISTS (SELECT d.id FROM %s d WHERE d.tenant=t.id AND TEXT_CONTAINS_ALL(d.body,'red')) ORDER BY t.id", []int64{1}, "FirstOrDefault"},
		{"SELECT t.id FROM tenants t LEFT JOIN %s d ON d.tenant=t.id AND TEXT_CONTAINS_ALL(d.body,'red') ORDER BY t.id", []int64{1, 2, 3}, "DefaultOnEmpty"},
	} {
		for _, table := range []string{"docs", "plain_docs"} {
			query := fmt.Sprintf(tc.query, table)
			plan := testkit.ExplainVia(t, ctx, db, query)
			if !strings.Contains(plan, tc.shape) {
				t.Fatalf("%s: missing %s: %s", query, tc.shape, plan)
			}
			if got := testkit.GetInts(t, ctx, db, query); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s: got %v want %v", query, got, tc.want)
			}
		}
	}
}

func TestFDBFullTextNaNPartition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "fulltext_nan", "CREATE TABLE docs(id BIGINT, tenant DOUBLE, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body) PARTITION BY (tenant) CREATE TABLE plain_docs(id BIGINT, tenant DOUBLE, body STRING, PRIMARY KEY(id))")
	for _, table := range []string{"docs", "plain_docs"} {
		if _, err := db.ExecContext(ctx, "INSERT INTO "+table+" VALUES (1,?,'red'),(2,1.0,'red'),(3,?,'blue')", math.NaN(), math.NaN()); err != nil {
			t.Fatal(err)
		}
		query := "SELECT id FROM " + table + " WHERE tenant=? AND TEXT_CONTAINS_ALL(body,'red') ORDER BY id"
		if plan := explainFullText(t, ctx, db, query, math.NaN()); strings.Contains(plan, "TextIndexScan(BODY_TEXT") != (table == "docs") {
			t.Fatalf("%s: %s", query, plan)
		}
		rows, err := db.QueryContext(ctx, query, math.NaN())
		if table == "docs" {
			if err == nil {
				rows.Next()
				err = rows.Err()
				rows.Close()
			}
			var unsupported *executor.UnsupportedPhysicalFloatEquivalenceError
			if !errors.As(err, &unsupported) {
				t.Fatalf("NaN text partition: %T %v", err, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			got = append(got, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !reflect.DeepEqual(got, []int64{1}) {
			t.Fatalf("NaN fallback: got %v err=%v", got, err)
		}
	}
}

func TestFDBFullTextSignedZeroPartition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "fulltext_signed_zero", "CREATE TABLE docs(id BIGINT, tenant DOUBLE, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body) PARTITION BY (tenant) CREATE TABLE plain_docs(id BIGINT, tenant DOUBLE, body STRING, PRIMARY KEY(id))")
	// Each tuple-distinct zero partition has both a complete match and a
	// partial match. Neither SQL-equal partition may be lost or conflated.
	for _, table := range []string{"docs", "plain_docs"} {
		if _, err := db.ExecContext(ctx, "INSERT INTO "+table+" VALUES (1,-0.0,'red blue prefix prelude'), (2,0.0,'red blue prefix prelude'), (3,-0.0,'red prefix'), (4,0.0,'blue prefix'), (5,1.0,'red blue prefix prelude'), (6,-1.0,'red blue prefix prelude')"); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		predicate string
		want      []int64
	}{
		{"TEXT_CONTAINS_ALL(body,'red blue')", []int64{1, 2}},
		{"TEXT_CONTAINS_ANY(body,'red blue')", []int64{1, 2, 3, 4}},
		{"TEXT_CONTAINS_ALL_PREFIXES(body,'pr prel')", []int64{1, 2}},
	} {
		for _, table := range []string{"docs", "plain_docs"} {
			query := "SELECT id, tenant FROM " + table + " WHERE tenant=0 AND " + tc.predicate + " ORDER BY id"
			plan := testkit.ExplainVia(t, ctx, db, query)
			if indexed := strings.Contains(plan, "TextIndexScan(BODY_TEXT"); indexed != (table == "docs") {
				t.Fatalf("%s: unexpected plan %s", query, plan)
			}
			rows, err := db.QueryContext(ctx, query)
			if err != nil {
				t.Fatalf("%s: %v", query, err)
			}
			var got []int64
			for rows.Next() {
				var id int64
				var tenant float64
				if err := rows.Scan(&id, &tenant); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				// Verify fetched records retain their original group identity;
				// SQL equality must not rewrite the stored sign of zero.
				if tenant != 0 || math.Signbit(tenant) != (id == 1 || id == 3) {
					rows.Close()
					t.Fatalf("%s: id=%d tenant=%v has wrong partition identity", query, id, tenant)
				}
				got = append(got, id)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s: got %v want %v", query, got, tc.want)
			}
		}
	}
}

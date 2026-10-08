package sqltest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// A GUARDIANN vector index built by DDL (Java guardiann-semantic-search.yamsql):
// k-NN per (zone, bookshelf) partition, maintained through deletes and
// re-inserts.
func TestFDB_GuardiannSemanticSearch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_guardiann")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_guardiann")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE guardiann_tpl "+
		"create table documents(zone string, docId string, bookshelf string, title string, embedding vector(3, half), primary key (zone, docId)) "+
		"create view documentsView as select embedding, zone, bookshelf, docId, title from documents "+
		"create vector index documentsGuardiannIndex using guardiann on documentsView(embedding) partition by(zone, bookshelf) "+
		"options (metric = euclidean_metric, primary_cluster_min = 1, primary_cluster_max = 100, collapse_min_duplicates = 50)")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_guardiann/s WITH TEMPLATE guardiann_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_GUARDIANN?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	insert := func(docID, shelf, title string, v []float64) {
		t.Helper()
		if _, err := db.ExecContext(ctx, "insert into documents values ('zone1', ?, ?, ?, ?)",
			docID, shelf, title, vectorcodec.SerializeHalf(v)); err != nil {
			t.Fatalf("insert %s: %v", docID, err)
		}
	}
	insert("d1", "fiction", "The Great Gatsby", []float64{1, 0, 0})
	insert("d2", "fiction", "1984", []float64{0.9, 0.1, 0})
	insert("d3", "fiction", "To Kill a Mockingbird", []float64{0.8, 0.2, 0})
	insert("d6", "science", "A Brief History of Time", []float64{0, 1, 0})
	insert("d7", "science", "The Selfish Gene", []float64{0.1, 0.9, 0})
	knn := func(shelf string, q string, k int) string {
		t.Helper()
		query := fmt.Sprintf("select docId, euclidean_distance(embedding, %s) as distance from documents "+
			"where zone = 'zone1' and bookshelf = '%s' qualify row_number() over "+
			"(partition by zone, bookshelf order by euclidean_distance(embedding, %s) asc) <= %d", q, shelf, q, k)
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+query).Scan(&plan); err != nil {
			t.Fatalf("explain: %v", err)
		}
		if !strings.Contains(plan, "DOCUMENTSGUARDIANNINDEX") {
			t.Fatalf("not a guardiann index scan: %s", plan)
		}
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			var d float64
			if err := rows.Scan(&id, &d); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%s:%v", id, d))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(out, " ")
	}
	fiction, science := "[1.0, 0.0, 0.0]", "[0.0, 1.0, 0.0]"
	for _, c := range []struct {
		shelf, q string
		k        int
		want     string
	}{
		{"fiction", fiction, 1, "d1:0"},
		{"fiction", fiction, 3, "d1:0 d2:0.14147317261689443 d3:0.28294634523378887"},
		{"science", science, 2, "d6:0 d7:0.14147317261689443"},
	} {
		if got := knn(c.shelf, c.q, c.k); got != c.want {
			t.Errorf("%s top-%d = %s, want %s", c.shelf, c.k, got, c.want)
		}
	}
	testkit.MustExec(t, db, ctx, "delete from documents where zone = 'zone1' and docId = 'd1'")
	if got := knn("fiction", fiction, 3); got != "d2:0.14147317261689443 d3:0.28294634523378887" {
		t.Errorf("after delete: %s", got)
	}
	insert("d1", "fiction", "The Great Gatsby", []float64{1, 0, 0})
	if got := knn("fiction", fiction, 1); got != "d1:0" {
		t.Errorf("after re-insert: %s", got)
	}
	testkit.MustExec(t, db, ctx, "delete from documents where zone = 'zone1' and bookshelf = 'fiction'")
	if got := knn("fiction", fiction, 3); got != "" {
		t.Errorf("emptied partition: %s", got)
	}
	if got := knn("science", science, 2); got != "d6:0 d7:0.14147317261689443" {
		t.Errorf("other partition: %s", got)
	}
}

package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/vectorcodec"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// VECTOR_INDEX_ENGINE_PREFERENCE (Java vector-engine-preference.yamsql): with an
// HNSW and a GuardiANN index over the same field and metric, the preference
// picks the engine; with none, the tie-break does. A table with one engine
// ignores the preference.
func TestFDB_VectorIndexEnginePreference(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_vec_pref")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_vec_pref")
	guardiannOpts := "options (metric = euclidean_metric, primary_cluster_min = 1, primary_cluster_max = 100, collapse_min_duplicates = 50)"
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE vec_pref_tpl "+
		"create table documents(zone string, docId string, bookshelf string, embedding vector(3, half), primary key (zone, docId)) "+
		"create view documentsView as select embedding, zone, bookshelf, docId from documents "+
		"create vector index documentsHnswIndex using hnsw on documentsView(embedding) partition by(zone, bookshelf) options (metric = euclidean_metric) "+
		"create vector index documentsGuardiannIndex using guardiann on documentsView(embedding) partition by(zone, bookshelf) "+guardiannOpts+" "+
		"create table hnswOnly(zone string, docId string, bookshelf string, embedding vector(3, half), primary key (zone, docId)) "+
		"create view hnswOnlyView as select embedding, zone, bookshelf, docId from hnswOnly "+
		"create vector index hnswOnlyIndex using hnsw on hnswOnlyView(embedding) partition by(zone, bookshelf) options (metric = euclidean_metric)")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_vec_pref/s WITH TEMPLATE vec_pref_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///TESTDB_VEC_PREF?cluster_file=%s&schema=S", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	for _, tbl := range []string{"documents", "hnswOnly"} {
		for i, v := range [][]float64{{1, 0, 0}, {0.9, 0.1, 0}, {0.8, 0.2, 0}} {
			if _, err := db.ExecContext(ctx, "insert into "+tbl+" values ('zone1', ?, 'fiction', ?)",
				fmt.Sprintf("d%d", i+1), vectorcodec.SerializeHalf(v)); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}

	knn := func(tbl string) string {
		return "select docId from " + tbl + " where zone = 'zone1' and bookshelf = 'fiction' " +
			"qualify row_number() over (partition by zone, bookshelf " +
			"order by euclidean_distance(embedding, [1.0, 0.0, 0.0]) asc) <= 2"
	}
	for _, c := range []struct {
		pref      api.VectorIndexEnginePreference
		tbl, want string
	}{
		{api.VectorIndexPreferHNSW, "documents", "DOCUMENTSHNSWINDEX"},
		{api.VectorIndexPreferGuardiann, "documents", "DOCUMENTSGUARDIANNINDEX"},
		{api.VectorIndexPreferGuardiann, "hnswOnly", "HNSWONLYINDEX"},
	} {
		conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
			ec.SetOptions(api.NewOptionsBuilder().Set(api.OptVectorIndexEnginePreference, c.pref).Build())
		})
		var plan string
		if err := conn.QueryRowContext(ctx, "EXPLAIN "+knn(c.tbl)).Scan(&plan); err != nil {
			t.Fatalf("explain: %v", err)
		}
		if !strings.Contains(plan, c.want) {
			t.Errorf("%v on %s: want %s, got %s", c.pref, c.tbl, c.want, plan)
		}
		rows, err := conn.QueryContext(ctx, knn(c.tbl))
		if err != nil {
			t.Fatalf("%v: %v", c.pref, err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if got := strings.Join(ids, ","); got != "d1,d2" {
			t.Errorf("%v on %s: rows %s, want d1,d2", c.pref, c.tbl, got)
		}
		conn.Close()
	}
}

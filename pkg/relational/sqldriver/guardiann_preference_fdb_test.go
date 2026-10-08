package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/recordlayer/vectorcodec"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// VECTOR_INDEX_ENGINE_PREFERENCE over Java's vector-engine-preference.yamsql
// fixtures: DOCUMENTS and ARTICLES carry both engines over one field, so the
// preference picks the engine (in each union leg independently); HNSWONLY and
// GUARDIANNONLY have one index and ignore it; MIXEDMETRICS has one index per
// engine over DIFFERENT metrics, so only the matching one can serve an
// ordering whatever is preferred; a query with no vector access is left alone.
// Each case asserts the index names Java's EXPLAIN shows, and the rows.
func TestFDB_VectorIndexEnginePreference(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_vec_pref")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_vec_pref")
	g := "options (metric = %s, primary_cluster_min = 1, primary_cluster_max = 100, collapse_min_duplicates = 50)"
	guardiann := fmt.Sprintf(g, "euclidean_metric")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE vec_pref_tpl "+
		"create table documents(zone string, docId string, bookshelf string, title string, embedding vector(3, half), primary key (zone, docId)) "+
		"create view documentsView as select embedding, zone, bookshelf, docId, title from documents "+
		"create vector index documentsHnswIndex using hnsw on documentsView(embedding) partition by(zone, bookshelf) options (metric = euclidean_metric) "+
		"create vector index documentsGuardiannIndex using guardiann on documentsView(embedding) partition by(zone, bookshelf) "+guardiann+" "+
		"create index documentsByTitle as select title from documents order by title "+
		"create table articles(zone string, articleId string, section string, headline string, embedding vector(3, half), primary key (zone, articleId)) "+
		"create view articlesView as select embedding, zone, section, articleId, headline from articles "+
		"create vector index articlesHnswIndex using hnsw on articlesView(embedding) partition by(zone, section) options (metric = euclidean_metric) "+
		"create vector index articlesGuardiannIndex using guardiann on articlesView(embedding) partition by(zone, section) "+guardiann+" "+
		"create table hnswOnly(zone string, docId string, bookshelf string, embedding vector(3, half), primary key (zone, docId)) "+
		"create view hnswOnlyView as select embedding, zone, bookshelf, docId from hnswOnly "+
		"create vector index hnswOnlyIndex using hnsw on hnswOnlyView(embedding) partition by(zone, bookshelf) options (metric = euclidean_metric) "+
		"create table guardiannOnly(zone string, docId string, bookshelf string, embedding vector(3, half), primary key (zone, docId)) "+
		"create view guardiannOnlyView as select embedding, zone, bookshelf, docId from guardiannOnly "+
		"create vector index guardiannOnlyIndex using guardiann on guardiannOnlyView(embedding) partition by(zone, bookshelf) "+guardiann+" "+
		"create table mixedMetrics(zone string, docId string, bookshelf string, embedding vector(3, half), primary key (zone, docId)) "+
		"create view mixedMetricsView as select embedding, zone, bookshelf, docId from mixedMetrics "+
		"create vector index mixedMetricsHnswEuclideanIndex using hnsw on mixedMetricsView(embedding) partition by(zone, bookshelf) options (metric = euclidean_metric) "+
		"create vector index mixedMetricsGuardiannCosineIndex using guardiann on mixedMetricsView(embedding) partition by(zone, bookshelf) "+fmt.Sprintf(g, "cosine_metric"))
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_vec_pref/s WITH TEMPLATE vec_pref_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_VEC_PREF?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	points := [][]float64{{1, 0, 0}, {0.9, 0.1, 0}, {0.8, 0.2, 0}}
	titles := []string{"The Great Gatsby", "1984", "To Kill a Mockingbird"}
	for i, v := range points {
		vec := vectorcodec.SerializeHalf(v)
		for _, ins := range []struct {
			sql  string
			args []any
		}{
			{"insert into documents values ('zone1', ?, 'fiction', ?, ?)", []any{fmt.Sprintf("d%d", i+1), titles[i], vec}},
			{"insert into articles values ('zone1', ?, 'news', ?, ?)", []any{fmt.Sprintf("a%d", i+1), titles[i], vec}},
			{"insert into hnswOnly values ('zone1', ?, 'fiction', ?)", []any{fmt.Sprintf("h%d", i+1), vec}},
			{"insert into guardiannOnly values ('zone1', ?, 'fiction', ?)", []any{fmt.Sprintf("g%d", i+1), vec}},
			{"insert into mixedMetrics values ('zone1', ?, 'fiction', ?)", []any{fmt.Sprintf("m%d", i+1), vec}},
		} {
			if _, err := db.ExecContext(ctx, ins.sql, ins.args...); err != nil {
				t.Fatalf("%s: %v", ins.sql, err)
			}
		}
	}

	knn := func(tbl, id, part, metric string) string {
		return "select " + id + " as id from " + tbl + " where zone = 'zone1' and " + part +
			" qualify row_number() over (partition by zone, " + strings.Fields(part)[0] +
			" order by " + metric + "(embedding, [1.0, 0.0, 0.0]) asc) <= 2"
	}
	docs := knn("documents", "docId", "bookshelf = 'fiction'", "euclidean_distance")
	union := docs + " union all " + knn("articles", "articleId", "section = 'news'", "euclidean_distance")
	only := func(tbl string) string { return knn(tbl, "docId", "bookshelf = 'fiction'", "euclidean_distance") }
	cosine := knn("mixedMetrics", "docId", "bookshelf = 'fiction'", "cosine_distance")
	covering := "select docId as id from documents where title = 'The Great Gatsby'"

	for _, c := range []struct {
		pref     api.VectorIndexEnginePreference
		query    string
		want     []string // index names the plan must scan
		wantRows string
	}{
		{api.VectorIndexPreferHNSW, docs, []string{"DOCUMENTSHNSWINDEX"}, "d1,d2"},
		{api.VectorIndexPreferHNSW, union, []string{"DOCUMENTSHNSWINDEX", "ARTICLESHNSWINDEX"}, "a1,a2,d1,d2"},
		{api.VectorIndexPreferHNSW, only("guardiannOnly"), []string{"GUARDIANNONLYINDEX"}, "g1,g2"},
		{api.VectorIndexPreferHNSW, cosine, []string{"MIXEDMETRICSGUARDIANNCOSINEINDEX"}, "m1,m2"},
		{api.VectorIndexPreferGuardiann, docs, []string{"DOCUMENTSGUARDIANNINDEX"}, "d1,d2"},
		{api.VectorIndexPreferGuardiann, union, []string{"DOCUMENTSGUARDIANNINDEX", "ARTICLESGUARDIANNINDEX"}, "a1,a2,d1,d2"},
		{api.VectorIndexPreferGuardiann, only("hnswOnly"), []string{"HNSWONLYINDEX"}, "h1,h2"},
		{api.VectorIndexPreferGuardiann, only("mixedMetrics"), []string{"MIXEDMETRICSHNSWEUCLIDEANINDEX"}, "m1,m2"},
		{api.VectorIndexPreferGuardiann, covering, []string{"DOCUMENTSBYTITLE"}, "d1"},
	} {
		conn := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
			ec.SetOptions(api.NewOptionsBuilder().Set(api.OptVectorIndexEnginePreference, c.pref).Build())
		})
		var plan string
		if err := conn.QueryRowContext(ctx, "EXPLAIN "+c.query).Scan(&plan); err != nil {
			t.Fatalf("explain %s: %v", c.query, err)
		}
		for _, idx := range c.want {
			if !strings.Contains(plan, idx) {
				t.Errorf("%v: %s\n  want %s in plan, got %s", c.pref, c.query, idx, plan)
			}
		}
		rows, err := conn.QueryContext(ctx, c.query)
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
		sort.Strings(ids)
		if got := strings.Join(ids, ","); got != c.wantRows {
			t.Errorf("%v: %s\n  rows %s, want %s", c.pref, c.query, got, c.wantRows)
		}
		conn.Close()
	}
}

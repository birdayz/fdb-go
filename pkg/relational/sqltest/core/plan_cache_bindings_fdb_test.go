package sqltest

import (
	"context"
	"database/sql"
	"math"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

func cacheQueryIDs(t *testing.T, conn *sql.Conn, text string, args ...any) []int64 {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(), text, args...)
	if err != nil {
		t.Fatalf("%s %v: %v", text, args, err)
	}
	defer rows.Close()
	var result []int64
	for rows.Next() {
		var value int64
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestFDB_PlanCacheBindingShapes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupErrorDB(t, "/FRL/cache_binding_shapes", "cache_binding_shapes",
		"CREATE TABLE T (id BIGINT, v BIGINT, PRIMARY KEY(id)) CREATE INDEX i_v ON T(v)")
	if _, err := db.ExecContext(ctx, "INSERT INTO T VALUES (1,10),(2,20),(3,30)"); err != nil {
		t.Fatal(err)
	}
	logger := &testkit.SyncCaptureLogger{}
	var cache *embedded.RelationalPlanCache
	conn := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		cache = ec.SharedPlanCache()
		ec.SetPlanLogger(logger)
	})
	for _, tc := range []struct {
		name, query           string
		first, second         []any
		wantFirst, wantSecond []int64
		hit                   bool
	}{
		{"secondary", "SELECT id FROM T WHERE v=?", []any{int64(10)}, []any{int64(30)}, []int64{1}, []int64{3}, true},
		{"false then true", "SELECT id FROM T WHERE ? ORDER BY id", []any{false}, []any{true}, nil, []int64{1, 2, 3}, false},
		{"contradiction then range", "SELECT id FROM T WHERE v>=? AND v<=? ORDER BY id", []any{int64(30), int64(10)}, []any{int64(10), int64(30)}, nil, []int64{1, 2, 3}, true},
		{"case", "SELECT CASE WHEN ? THEN id ELSE id * 10 END FROM T ORDER BY id", []any{true}, []any{false}, []int64{1, 2, 3}, []int64{10, 20, 30}, false},
		{"coalesce error pruning", "SELECT id FROM T WHERE COALESCE(?, 1/0=1) ORDER BY id", []any{true}, []any{false}, []int64{1, 2, 3}, nil, false},
		{"scalar subquery", "SELECT id FROM T WHERE v=(SELECT ? FROM T WHERE id=1)", []any{int64(10)}, []any{int64(30)}, []int64{1}, []int64{3}, true},
		{"join", "SELECT a.id FROM T a JOIN T b ON a.v=b.v WHERE b.id=?", []any{int64(1)}, []any{int64(3)}, []int64{1}, []int64{3}, true},
		{"join output", "SELECT a.id+? FROM T a JOIN T b ON a.v=b.v ORDER BY a.id", []any{int64(7)}, []any{int64(8)}, []int64{8, 9, 10}, []int64{9, 10, 11}, true},
		{"derived join output", "SELECT x.c FROM (SELECT id-? AS c FROM T) x JOIN T b ON x.c=b.id ORDER BY x.c", []any{int64(0)}, []any{int64(1)}, []int64{1, 2, 3}, []int64{1, 2}, true},
		{"in array", "SELECT id FROM T WHERE id IN ? ORDER BY id", []any{[]int64{1, 1}}, []any{[]int64{2, 3}}, []int64{1}, []int64{2, 3}, false},
		{"limit", "SELECT id FROM T ORDER BY id LIMIT ?", []any{int64(1)}, []any{int64(2)}, []int64{1}, []int64{1, 2}, false},
		{"equality partition", "SELECT id FROM T WHERE id=? OR id=? ORDER BY id", []any{int64(1), int64(1)}, []any{int64(2), int64(3)}, []int64{1}, []int64{2, 3}, false},
		{"additional equality", "SELECT ? + ? FROM T WHERE id=3", []any{int64(1), int64(2)}, []any{int64(4), int64(4)}, []int64{3}, []int64{8}, true},
		{"named", "SELECT id FROM T WHERE id=?target", []any{sql.Named("target", int64(1))}, []any{sql.Named("target", int64(3))}, []int64{1}, []int64{3}, true},
		{"null", "SELECT id FROM T WHERE ? IS NULL ORDER BY id", []any{nil}, []any{int64(1)}, []int64{1, 2, 3}, nil, false},
		{"numeric lane", "SELECT id FROM T WHERE v>? ORDER BY id", []any{int64(10)}, []any{float64(20.5)}, []int64{2, 3}, []int64{3}, false},
	} {
		cache.Invalidate()
		before := len(logger.Snapshot())
		if got := cacheQueryIDs(t, conn, tc.query, tc.first...); !reflect.DeepEqual(got, tc.wantFirst) {
			t.Fatalf("%s first: %v, want %v", tc.name, got, tc.wantFirst)
		}
		if got := cacheQueryIDs(t, conn, tc.query, tc.second...); !reflect.DeepEqual(got, tc.wantSecond) {
			t.Fatalf("%s second: %v, want %v", tc.name, got, tc.wantSecond)
		}
		events := logger.Snapshot()[before:]
		if len(events) != 2 || events[0].Cache != embedded.PlanCacheMiss || (events[1].Cache == embedded.PlanCacheHit) != tc.hit {
			t.Fatalf("%s cache events: %+v, want miss then hit=%v", tc.name, events, tc.hit)
		}
		if tc.name == "secondary" && !strings.Contains(events[1].PlanExplain, "IndexScan(I_V") {
			t.Fatalf("reusable equality lost indexed access: %s", events[1].PlanExplain)
		}
	}
	cache.Invalidate()
	for i, query := range []string{"SELECT id FROM T WHERE v=10", "SELECT id FROM T WHERE v=30"} {
		before := cache.Counts()
		if got := cacheQueryIDs(t, conn, query); !reflect.DeepEqual(got, []int64{int64(1 + 2*i)}) {
			t.Fatalf("literal query: %v", got)
		}
		if got := cache.Counts().TertiaryHit - before.TertiaryHit; got != int64(i) {
			t.Fatalf("literal execution %d hit delta=%d", i, got)
		}
	}
}

func TestFDB_PlanCachePreservesRuntimePayloads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupErrorDB(t, "/FRL/cache_binding_payloads", "cache_binding_payloads", "CREATE TABLE T (id BIGINT,PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO T VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	var cache *embedded.RelationalPlanCache
	conn := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) { cache = ec.SharedPlanCache() })
	for _, pair := range [][2]any{
		{true, false},
		{math.Copysign(0, -1), float64(0)},
		{math.Float64frombits(0x7ff8000000000001), math.Float64frombits(0xfff8000000000123)},
		{[]byte{0, 1}, []byte{2, 0}},
		{"a\x00b", ""},
	} {
		cache.Invalidate()
		before := cache.Counts().TertiaryHit
		for _, want := range pair {
			var got any
			if err := conn.QueryRowContext(ctx, "SELECT ? AS payload FROM T WHERE id=1", want).Scan(&got); err != nil {
				t.Fatal(err)
			}
			equal := reflect.DeepEqual(got, want)
			if f, ok := want.(float64); ok {
				g, ok := got.(float64)
				equal = ok && math.Float64bits(g) == math.Float64bits(f)
			}
			if !equal {
				t.Fatalf("cached payload got %#v (%T), want %#v (%T)", got, got, want, want)
			}
		}
		wantHits := int64(1)
		switch pair[0].(type) {
		case bool:
			wantHits = 0 // Java constrains boolean evaluation, not only its type.
		case float64:
			wantHits = 0 // Float key proofs read signed zero and NaN.
		}
		if cache.Counts().TertiaryHit != before+wantHits {
			t.Fatalf("payload %T hit delta=%d, want %d", pair[0], cache.Counts().TertiaryHit-before, wantHits)
		}
	}
}

func TestFDB_PlanCacheDMLRuntimeBindings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupErrorDB(t, "/FRL/cache_binding_dml", "cache_binding_dml", "CREATE TABLE T (id BIGINT,v BIGINT,PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO T VALUES (1,10),(2,20),(3,30)"); err != nil {
		t.Fatal(err)
	}
	logger := &testkit.SyncCaptureLogger{}
	conn := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) { ec.SetPlanLogger(logger) })
	for _, query := range []string{"UPDATE T SET v=? WHERE id=?", "DELETE FROM T WHERE v=? AND id=?"} {
		start := len(logger.Snapshot())
		for i := int64(1); i <= 2; i++ {
			result, err := conn.ExecContext(ctx, query, 100*i, i)
			if err != nil {
				t.Fatal(err)
			}
			if n, err := result.RowsAffected(); err != nil || n != 1 {
				t.Fatalf("%s affected=%d err=%v", query, n, err)
			}
		}
		events := logger.Snapshot()[start:]
		if len(events) != 2 || events[0].Cache != embedded.PlanCacheMiss || events[1].Cache != embedded.PlanCacheHit {
			t.Fatalf("DML cache events: %+v", events)
		}
	}
	if got := cacheQueryIDs(t, conn, "SELECT id FROM T ORDER BY id"); !reflect.DeepEqual(got, []int64{3}) {
		t.Fatalf("DML reused old predicate: %v", got)
	}
}

func TestFDB_PlanCacheNumericPromotions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupErrorDB(t, "/FRL/cache_binding_numeric", "cache_binding_numeric",
		"CREATE TABLE T (id BIGINT,d DOUBLE,f FLOAT,PRIMARY KEY(id)) CREATE INDEX i_d ON T(d) CREATE INDEX i_f ON T(f)")
	if _, err := db.ExecContext(ctx, "INSERT INTO T VALUES (1,1.0,CAST(1.0 AS FLOAT)),(2,2.0,CAST(2.0 AS FLOAT)),(3,3.0,CAST(3.0 AS FLOAT))"); err != nil {
		t.Fatal(err)
	}
	var cache *embedded.RelationalPlanCache
	conn := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) { cache = ec.SharedPlanCache() })
	for _, tc := range []struct {
		query                 string
		first, second         any
		wantFirst, wantSecond []int64
	}{
		{"SELECT id FROM T WHERE d=?", int64(1), int64(3), []int64{1}, []int64{3}},
		{"SELECT id FROM T WHERE f<? ORDER BY id", 1.5, 2.5, []int64{1}, []int64{1, 2}},
		{"SELECT id FROM T WHERE id=?", 1.0, 1.5, []int64{1}, nil},
	} {
		cache.Invalidate()
		if got := cacheQueryIDs(t, conn, tc.query, tc.first); !reflect.DeepEqual(got, tc.wantFirst) {
			t.Fatalf("%s first %v", tc.query, got)
		}
		before := cache.Counts().TertiaryHit
		if got := cacheQueryIDs(t, conn, tc.query, tc.second); !reflect.DeepEqual(got, tc.wantSecond) {
			t.Fatalf("%s second %v", tc.query, got)
		}
		// A float or cross-type comparand is rewritten from its value at plan
		// time, so only the identical value may reuse that plan.
		if cache.Counts().TertiaryHit != before {
			t.Fatalf("%s reused a value-specialized plan for another value", tc.query)
		}
		if got := cacheQueryIDs(t, conn, tc.query, tc.first); !reflect.DeepEqual(got, tc.wantFirst) {
			t.Fatalf("%s repeat %v", tc.query, got)
		}
		if cache.Counts().TertiaryHit != before+1 {
			t.Fatalf("%s did not reuse its plan for the identical value", tc.query)
		}
	}
}

func TestFDB_PlanCacheRuntimePoolsAcrossPagesAndConnections(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupErrorDB(t, "/FRL/cache_binding_pages", "cache_binding_pages",
		"CREATE TABLE T (id BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO T VALUES (1),(2),(3),(4),(5),(6)"); err != nil {
		t.Fatal(err)
	}
	var cache *embedded.RelationalPlanCache
	conn := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		cache = ec.SharedPlanCache()
		ec.SetOptions(api.NewOptionsBuilder().Set(api.OptExecutionScannedRowsLimit, int64(2)).Build())
	})
	other := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	first, err := conn.QueryContext(ctx, "SELECT ? FROM T ORDER BY id", int64(11))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	// Rebind the same cached plan before the first execution fetches lazy pages.
	before := cache.Counts().TertiaryHit
	if got := cacheQueryIDs(t, other, "SELECT ? FROM T ORDER BY id", int64(22)); !reflect.DeepEqual(got, []int64{22, 22, 22, 22, 22, 22}) {
		t.Fatalf("second execution: %v", got)
	}
	if cache.Counts().TertiaryHit != before+1 {
		t.Fatal("second connection did not reuse the plan")
	}
	count := 0
	for first.Next() {
		var value int64
		if err := first.Scan(&value); err != nil || value != 11 {
			t.Fatalf("first execution's pool overwritten: %d, %v", value, err)
		}
		count++
	}
	if err := first.Err(); err != nil || count != 6 {
		t.Fatalf("first execution rows=%d, err=%v", count, err)
	}
}

func TestFDB_PlanCacheParameterSchemaScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupErrorDB(t, "/FRL/cache_binding_schema", "cache_binding_schema",
		"CREATE TABLE T (id BIGINT, v BIGINT, PRIMARY KEY(id))")
	logger := &testkit.SyncCaptureLogger{}
	conn := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) { ec.SetPlanLogger(logger) })
	for _, ddl := range []string{
		"CREATE SCHEMA TEMPLATE cache_binding_other CREATE TABLE T (v BIGINT, id BIGINT, PRIMARY KEY(id))",
		"CREATE SCHEMA /FRL/cache_binding_schema/other WITH TEMPLATE cache_binding_other",
		"INSERT INTO T (id,v) VALUES (1,10),(2,20)",
	} {
		if _, err := conn.ExecContext(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	setSchema := func(name string) {
		t.Helper()
		if err := conn.Raw(func(raw any) error { return raw.(*embedded.EmbeddedConnection).SetSchema(name) }); err != nil {
			t.Fatal(err)
		}
	}
	setSchema("OTHER")
	if _, err := conn.ExecContext(ctx, "INSERT INTO T (id,v) VALUES (1,100),(2,200)"); err != nil {
		t.Fatal(err)
	}
	start := len(logger.Snapshot())
	for _, tc := range []struct {
		schema   string
		id, want int64
	}{
		{"CACHE_BINDING_SCHEMA", 1, 10}, {"OTHER", 1, 100}, {"OTHER", 2, 200}, {"CACHE_BINDING_SCHEMA", 2, 20},
	} {
		setSchema(tc.schema)
		if got := cacheQueryIDs(t, conn, "SELECT v FROM T WHERE id=?", tc.id); !reflect.DeepEqual(got, []int64{tc.want}) {
			t.Fatalf("schema %s: got %v want %d", tc.schema, got, tc.want)
		}
	}
	events := logger.Snapshot()[start:]
	for i, want := range []embedded.PlanCacheEvent{embedded.PlanCacheMiss, embedded.PlanCacheMiss, embedded.PlanCacheHit, embedded.PlanCacheHit} {
		if len(events) != 4 || events[i].Cache != want {
			t.Fatalf("schema switching cache events: %+v", events)
		}
	}
}

func TestFDB_PlanCacheParameterIndexState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := testkit.NewIndexStatePlanningFixture(t)
	logger := &testkit.SyncCaptureLogger{}
	conn := testkit.PinEmbeddedConn(t, fixture.DB, func(ec *embedded.EmbeddedConnection) { ec.SetPlanLogger(logger) })
	query := "SELECT id FROM T WHERE pad=?"
	for i, value := range []int64{10, 20, 30} {
		if i == 2 {
			if err := fixture.SetIndexDisabled(ctx, "IDX_PAD"); err != nil {
				t.Fatal(err)
			}
		}
		if got := cacheQueryIDs(t, conn, query, value); !reflect.DeepEqual(got, []int64{int64(i + 1)}) {
			t.Fatalf("execution %d: %v", i, got)
		}
	}
	events := logger.Snapshot()
	if len(events) != 3 || events[0].Cache != embedded.PlanCacheMiss || events[1].Cache != embedded.PlanCacheHit || events[2].Cache != embedded.PlanCacheMiss {
		t.Fatalf("index-state cache events: %+v", events)
	}
	if !strings.Contains(events[1].PlanExplain, "IndexScan(IDX_PAD") || strings.Contains(events[2].PlanExplain, "IndexScan(IDX_PAD") {
		t.Fatalf("cached index dependency survived DISABLED: %s / %s", events[1].PlanExplain, events[2].PlanExplain)
	}
}

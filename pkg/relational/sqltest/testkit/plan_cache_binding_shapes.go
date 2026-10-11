package testkit

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
)

// PlanCacheBindingShapesSchema is the schema the binding-shape table runs over.
const PlanCacheBindingShapesSchema = "CREATE TABLE T (id BIGINT, v BIGINT, PRIMARY KEY(id)) CREATE INDEX i_v ON T(v)"

// PlanCacheBindingShape runs Query with First then Second over rows (1,10),
// (2,20), (3,30); Hit says whether the second execution reuses the plan.
type PlanCacheBindingShape struct {
	Name, Query           string
	First, Second         []any
	WantFirst, WantSecond []int64
	Hit                   bool
}

// PlanCacheBindingShapes is shared with a fast-lane test that plans every
// query without FDB, so a shape that does not parse cannot hide in test-full.
var PlanCacheBindingShapes = []PlanCacheBindingShape{
	{"secondary", "SELECT id FROM T WHERE v=?", []any{int64(10)}, []any{int64(30)}, []int64{1}, []int64{3}, true},
	{"false then true", "SELECT id FROM T WHERE ? ORDER BY id", []any{false}, []any{true}, nil, []int64{1, 2, 3}, false},
	{"contradiction then range", "SELECT id FROM T WHERE v>=? AND v<=? ORDER BY id", []any{int64(30), int64(10)}, []any{int64(10), int64(30)}, nil, []int64{1, 2, 3}, true},
	{"case", "SELECT CASE WHEN ? THEN id ELSE id * 10 END FROM T ORDER BY id", []any{true}, []any{false}, []int64{1, 2, 3}, []int64{10, 20, 30}, false},
	// COALESCE arguments stay exact. Java evaluates every argument
	// (VariadicFunctionValue.java:118), so an erroring one is pinned in
	// yamsql plan_cache_binding_shapes.yaml, not here.
	{"coalesce exactness", "SELECT id FROM T WHERE COALESCE(?, 0) > 4 ORDER BY id", []any{int64(5)}, []any{int64(3)}, []int64{1, 2, 3}, nil, false},
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
}

// RunPlanCacheBindingShapes executes every shape twice on one pinned
// connection over db (PlanCacheBindingShapesSchema, empty) and checks rows
// and plan-cache reuse.
func RunPlanCacheBindingShapes(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "INSERT INTO T VALUES (1,10),(2,20),(3,30)"); err != nil {
		t.Fatal(err)
	}
	logger := &SyncCaptureLogger{}
	var cache *embedded.RelationalPlanCache
	conn := PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		cache = ec.SharedPlanCache()
		ec.SetPlanLogger(logger)
	})
	for _, tc := range PlanCacheBindingShapes {
		cache.Invalidate()
		before := len(logger.Snapshot())
		if got := QueryIDs(t, conn, tc.Query, tc.First...); !reflect.DeepEqual(got, tc.WantFirst) {
			t.Fatalf("%s first: %v, want %v", tc.Name, got, tc.WantFirst)
		}
		if got := QueryIDs(t, conn, tc.Query, tc.Second...); !reflect.DeepEqual(got, tc.WantSecond) {
			t.Fatalf("%s second: %v, want %v", tc.Name, got, tc.WantSecond)
		}
		events := logger.Snapshot()[before:]
		if len(events) != 2 || events[0].Cache != embedded.PlanCacheMiss || (events[1].Cache == embedded.PlanCacheHit) != tc.Hit {
			t.Fatalf("%s cache events: %+v, want miss then hit=%v", tc.Name, events, tc.Hit)
		}
		if tc.Name == "secondary" && !strings.Contains(events[1].PlanExplain, "IndexScan(I_V") {
			t.Fatalf("reusable equality lost indexed access: %s", events[1].PlanExplain)
		}
	}
	cache.Invalidate()
	for i, query := range []string{"SELECT id FROM T WHERE v=10", "SELECT id FROM T WHERE v=30"} {
		before := cache.Counts()
		if got := QueryIDs(t, conn, query); !reflect.DeepEqual(got, []int64{int64(1 + 2*i)}) {
			t.Fatalf("literal query: %v", got)
		}
		if got := cache.Counts().TertiaryHit - before.TertiaryHit; got != int64(i) {
			t.Fatalf("literal execution %d hit delta=%d", i, got)
		}
	}
}

// QueryIDs runs text on conn and returns its single BIGINT column.
func QueryIDs(t *testing.T, conn *sql.Conn, text string, args ...any) []int64 {
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
		t.Fatalf("%s %v: %v", text, args, err)
	}
	return result
}

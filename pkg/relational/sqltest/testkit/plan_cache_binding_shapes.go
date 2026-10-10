package testkit

import "database/sql"

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
	// `1/0` would lex `/0` as an identifier (ID_LITERAL admits a leading '/', as in Java).
	{"coalesce exactness", "SELECT id FROM T WHERE COALESCE(?, 1 / 0) > 4 ORDER BY id", []any{int64(5)}, []any{int64(3)}, []int64{1, 2, 3}, nil, false},
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

package embedded

import (
	"strings"
	"testing"
)

// The OTel span schema shape: a repeated attribute struct whose fan-out VALUE
// index carries the base record's start time after the element columns.
const fanOutExistsOrderingSchema = `
CREATE TYPE AS STRUCT attr (scope STRING, k STRING, sv STRING, iv BIGINT)
CREATE TABLE spans (trace_id STRING, span_id STRING, start_ns BIGINT, attrs attr ARRAY,
  PRIMARY KEY (trace_id, span_id))
CREATE INDEX spans_attr_str AS SELECT a.scope, a.k, a.sv, s.start_ns, s.trace_id, s.span_id
  FROM spans AS s, (SELECT scope, k, sv FROM s.attrs) AS a
  ORDER BY a.scope, a.k, a.sv, s.start_ns, s.trace_id, s.span_id`

// Java skips equality-pinned fan-out elements, allowing base-column order
// without a sort; unbound and range-bound elements stop that order.
func TestPlanFanOutExistsOrdersPastBoundElements(t *testing.T) {
	t.Parallel()
	const exists = `EXISTS (SELECT 1 FROM s.attrs AS a WHERE a.scope = 'span' AND a.k = 'model' AND a.sv = 'gpt')`
	for _, tc := range []struct {
		name, sql string
		sorted    bool
	}{
		{"time desc", `SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE ` + exists + ` ORDER BY s.start_ns DESC`, false},
		{"time asc", `SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE ` + exists + ` ORDER BY s.start_ns`, false},
		{"time range desc", `SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE ` + exists + ` AND s.start_ns >= 150 AND s.start_ns < 450 ORDER BY s.start_ns DESC`, false},
		{"time then primary key", `SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE ` + exists + ` ORDER BY s.start_ns DESC, s.trace_id DESC, s.span_id DESC`, false},
		// Java uses COMPARE BY unions for expanded OR pages and a filtered
		// range scan for the equivalent AND form.
		{"keyset or", `SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE ` + exists + ` AND (s.start_ns < 200 OR (s.start_ns = 200 AND (s.trace_id < 't1' OR (s.trace_id = 't1' AND s.span_id < 's2')))) ORDER BY s.start_ns DESC, s.trace_id DESC, s.span_id DESC`, false},
		{"keyset and", `SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE ` + exists + ` AND s.start_ns <= 200 AND (s.start_ns < 200 OR s.trace_id < 't1' OR (s.trace_id = 't1' AND s.span_id < 's2')) ORDER BY s.start_ns DESC, s.trace_id DESC, s.span_id DESC`, false},
		// An unbound element column ends the order (Java breaks there).
		{"element unbound", `SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE EXISTS (SELECT 1 FROM s.attrs AS a WHERE a.scope = 'span' AND a.k = 'model') ORDER BY s.start_ns DESC`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := PlanQueryForTest(tc.sql, fanOutExistsOrderingSchema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			t.Logf("%s", plan)
			if !strings.Contains(plan, "IndexScan(SPANS_ATTR_STR") {
				t.Fatalf("plan does not scan the fan-out index: %s", plan)
			}
			if !strings.Contains(plan, "UnorderedPrimaryKeyDistinct") {
				t.Fatalf("plan lost the existential cardinality repair: %s", plan)
			}
			if got := strings.Contains(plan, "InMemorySort"); got != tc.sorted {
				t.Fatalf("InMemorySort present=%v, want %v: %s", got, tc.sorted, plan)
			}
		})
	}
}

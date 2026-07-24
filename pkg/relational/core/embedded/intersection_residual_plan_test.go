package embedded

// Plan-shape pins for the pk-intersection residual-compensation fix (the
// RFC-182 audit's #1 finding) at the planner level, no FDB needed. The
// companion row-level pin is TestFDB_IntersectionResidualCompensation in
// sqldriver. Also pins the adjusted-match MaxMatchMap reader fix
// (ComputeResultCompensation must read the ADJUSTED map): with the reader
// broken, EVERY adjusted match's compensation folded to Impossible and the
// intersector declined all combinations, so no Intersection( could appear.

import (
	"strings"
	"testing"
)

const ixResSchema = `
CREATE TABLE items (id BIGINT NOT NULL, category STRING, name STRING, price BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_category ON items (category)
CREATE INDEX idx_price ON items (price)`

const ixFourWaySchema = `
CREATE TABLE ix4 (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, d BIGINT, payload STRING, PRIMARY KEY (id))
CREATE INDEX idx_a ON ix4 (a)
CREATE INDEX idx_b ON ix4 (b)
CREATE INDEX idx_c ON ix4 (c)
CREATE INDEX idx_d ON ix4 (d)`

func TestIntersection_FourWayProductionShape(t *testing.T) {
	t.Parallel()

	plan, err := PlanQueryForTest(
		"SELECT * FROM ix4 WHERE a = 1 AND b = 2 AND c = 3 AND d = 4",
		ixFourWaySchema,
		nil,
	)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !strings.Contains(plan, "Intersection(") {
		t.Fatalf("four indexed equalities must reach an intersection, got: %s", plan)
	}
	for _, indexName := range []string{"IDX_A", "IDX_B", "IDX_C", "IDX_D"} {
		if !strings.Contains(plan, "IndexScan("+indexName) {
			t.Fatalf("four-way intersection is missing %s, got: %s", indexName, plan)
		}
	}
}

func TestIntersectionResidual_CompensatedShape(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT * FROM items WHERE category = 'electronics' AND price = 120 AND name = 'Keyboard'",
		ixResSchema, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !strings.Contains(plan, "Intersection(") {
		t.Errorf("want the pk-intersection to survive with compensation, got: %s", plan)
	}
	if !strings.Contains(plan, "PredicatesFilter(") {
		t.Errorf("want the name residual reapplied as a filter above the intersection, got: %s", plan)
	}
}

// TestAdjustedSingleAccess_OrderedScanWithResidual pins the SINGLE-access
// consequence of the adjusted-MaxMatchMap reader fix: an adjusted match
// (ORDER BY satisfied by index order) with a
// residual predicate now compensates successfully — before the fix every
// adjusted match's compensation folded to Impossible and this shape only
// planned via the Go-only ImplementIndexScanRule path. The pin asserts the
// index carries the order (no InMemorySort) and the residual survives as a
// filter.
func TestAdjustedSingleAccess_OrderedScanWithResidual(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE T (id BIGINT NOT NULL, a BIGINT, s STRING, PRIMARY KEY (id))
CREATE INDEX idx_a ON T(a)`
	plan, err := PlanQueryForTest("SELECT * FROM t WHERE a > 3 AND s = 'x' ORDER BY a", schema, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !strings.Contains(plan, "IndexScan(IDX_A") {
		t.Errorf("want the ordered index scan to drive the query, got: %s", plan)
	}
	if !strings.Contains(plan, "PredicatesFilter(") {
		t.Errorf("want the s residual retained as a filter, got: %s", plan)
	}
	if strings.Contains(plan, "InMemorySort") {
		t.Errorf("index order satisfies ORDER BY — no sort expected, got: %s", plan)
	}
}

func TestIntersectionResidual_BareShapePreserved(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT * FROM items WHERE category = 'electronics' AND price = 120",
		ixResSchema, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !strings.Contains(plan, "Intersection(") {
		t.Errorf("residual-free conjunction must keep the bare intersection, got: %s", plan)
	}
	if strings.Contains(plan, "PredicatesFilter(") {
		t.Errorf("disjoint per-leg residuals set-intersect to nothing — no filter expected, got: %s", plan)
	}
}

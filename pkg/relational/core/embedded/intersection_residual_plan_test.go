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

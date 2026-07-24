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

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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

const ixDescendingSchema = `
CREATE TABLE ix_desc (
	id BIGINT NOT NULL,
	a BIGINT,
	b BIGINT,
	sort_key BIGINT NOT NULL,
	payload STRING,
	PRIMARY KEY (id)
)
CREATE INDEX idx_desc_a_sort ON ix_desc (a, sort_key)
CREATE INDEX idx_desc_b_sort ON ix_desc (b, sort_key)`

// TestIntersection_DescendingCommonSecondaryOrdering is the typed planner pin
// for RFC-190.5b. With each equality-bound index leg providing the same free
// suffix (SORT_KEY, ID), ORDER BY SORT_KEY DESC, ID DESC can be implemented by
// reversing both scans and the merge. The comparison key must include ID even
// though the SQL order's first component is already shared: it is the
// deterministic tie-breaker and the remaining free primary-key component.
func TestIntersection_DescendingCommonSecondaryOrdering(t *testing.T) {
	t.Parallel()

	tmpl, err := buildSchemaTemplateFromDDL(ixDescendingSchema)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	plan, err := PlanRecordQueryWithMetadata(
		"SELECT * FROM ix_desc WHERE a = 7 AND b = 9 ORDER BY sort_key DESC, id DESC",
		tmpl.Underlying(),
		properties.FixedStatistics{Cardinality: 1_000_000},
	)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	var intersection *plans.RecordQueryIntersectionPlan
	intersectionCount := 0
	plans.Walk(plan, func(node plans.RecordQueryPlan) bool {
		if candidate, ok := node.(*plans.RecordQueryIntersectionPlan); ok {
			intersection = candidate
			intersectionCount++
		}
		return true
	})
	if intersectionCount != 1 {
		t.Fatalf("want exactly one typed intersection, got %d: %s", intersectionCount, plan.Explain())
	}
	if !intersection.IsReverse() {
		t.Fatalf("descending common ordering must reverse the intersection merge: %s", plan.Explain())
	}

	wantKeys := []string{"SORT_KEY", "ID"}
	parts := intersection.GetComparisonKeyOrderingParts()
	if len(parts) != len(wantKeys) {
		t.Fatalf("comparison ordering has %d parts, want %d (%v): %s",
			len(parts), len(wantKeys), wantKeys, plan.Explain())
	}
	physicalKeys := intersection.GetComparisonKeyValues()
	if len(physicalKeys) != len(wantKeys) {
		t.Fatalf("physical comparison key has %d values, want %d (%v): %s",
			len(physicalKeys), len(wantKeys), wantKeys, plan.Explain())
	}
	for i, want := range wantKeys {
		if got := strings.ToUpper(values.ColumnNameValue(parts[i].Value)); got != want {
			t.Fatalf("comparison ordering part %d = %q, want %q: %s", i, got, want, plan.Explain())
		}
		if parts[i].SortOrder != properties.ProvidedSortOrderDescending {
			t.Fatalf("comparison ordering part %s = %v, want DESC NULLS LAST: %s",
				want, parts[i].SortOrder, plan.Explain())
		}
		if got := strings.ToUpper(values.ColumnNameValue(physicalKeys[i])); got != want {
			t.Fatalf("physical comparison key %d = %q, want %q: %s", i, got, want, plan.Explain())
		}
	}

	reverseLegs := map[string]bool{}
	plans.Walk(intersection, func(node plans.RecordQueryPlan) bool {
		indexPlan, ok := node.(*plans.RecordQueryIndexPlan)
		if !ok {
			return true
		}
		reverseLegs[strings.ToUpper(indexPlan.GetIndexName())] = indexPlan.IsReverse()
		return true
	})
	for _, indexName := range []string{"IDX_DESC_A_SORT", "IDX_DESC_B_SORT"} {
		reverse, ok := reverseLegs[indexName]
		if !ok {
			t.Fatalf("descending intersection is missing %s: %s", indexName, plan.Explain())
		}
		if !reverse {
			t.Fatalf("descending intersection leg %s is not reverse: %s", indexName, plan.Explain())
		}
	}
	if planHasType[*plans.RecordQueryInMemorySortPlan](plan) {
		t.Fatalf("common reverse ordering must satisfy ORDER BY without InMemorySort: %s", plan.Explain())
	}
}

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

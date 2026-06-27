package embedded

// RFC-154 §5/§6 — typed-plan assertion that an INNER JOIN with an EXISTS in the
// ON clause lowers to a SEMI-JOIN (FlatMap over a FirstOrDefault one-row inner +
// residual IS-NOT-NULL), the implementJoinWithExistential shape — not a
// materialized cross-product NLJ. Row correctness is pinned by the FDB tests in
// the sqldriver package (exists_in_on_fdb_test.go); this pins the plan SHAPE so a
// future regression to a fallback (or a dropped ON predicate) is caught.

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

const existsInOnSchema = `
CREATE TABLE a (id BIGINT NOT NULL, PRIMARY KEY (id))
CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id))
CREATE TABLE d (id BIGINT NOT NULL, PRIMARY KEY (id))
CREATE INDEX c_a_id ON c(a_id)
`

func planHasType[T plans.RecordQueryPlan](plan plans.RecordQueryPlan) bool {
	found := false
	plans.Walk(plan, func(n plans.RecordQueryPlan) bool {
		if _, ok := n.(T); ok {
			found = true
		}
		return true
	})
	return found
}

func TestExistsInOn_INNER_LowersToSemiJoin(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(existsInOnSchema)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	plan, err := PlanRecordQueryWithMetadata(
		"SELECT a.id, c.id FROM a JOIN c ON c.a_id = a.id AND EXISTS (SELECT 1 FROM d WHERE d.id = a.id)",
		tmpl.Underlying(), properties.FixedStatistics{Cardinality: 1_000_000})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	// The EXISTS semi-join is a FlatMap whose inner is a FirstOrDefault (the
	// one-row existential inner) — implementJoinWithExistential / buildExistsFlatMap.
	if !planHasType[*plans.RecordQueryFlatMapPlan](plan) {
		t.Errorf("INNER EXISTS-in-ON must lower to a FlatMap semi-join, got: %s", plan.Explain())
	}
	if !planHasType[*plans.RecordQueryFirstOrDefaultPlan](plan) {
		t.Errorf("INNER EXISTS-in-ON semi-join must wrap the existential inner in FirstOrDefault, got: %s", plan.Explain())
	}
}

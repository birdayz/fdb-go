package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
)

func TestPlanPhysicalForTestTracedAccountsForTheWholeRun(t *testing.T) {
	t.Parallel()
	const schema = "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) CREATE INDEX idx_a ON t (a) CREATE INDEX idx_b ON t (b)"
	trace := cascades.NewPlannerTrace()
	// A range arm keeps the union unordered by primary key (two equality
	// arms merge by id, Java's COMPARE BY (_.ID)).
	plan, err := PlanPhysicalForTestTraced("SELECT * FROM t WHERE a > 1 OR b = 2", schema, nil, trace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Explain(), "UnorderedUnion") {
		t.Fatalf("plan %s does not exercise union exploration", plan.Explain())
	}
	tasks, groups, unionRule := 0, trace.InitialGroups(), false
	for _, entry := range trace.Entries() {
		tasks += entry.Tasks
		groups += entry.NewGroups
		unionRule = unionRule || entry.Rule == "ImplementUnorderedUnionRule" && entry.NewGroups > 0
	}
	census := trace.Census(3)
	if tasks != trace.Tasks() || groups != census.Groups || groups <= trace.InitialGroups() || !unionRule {
		t.Fatalf("rows hold %d of %d tasks and %d of %d groups (initial %d), union rule attributed=%t",
			tasks, trace.Tasks(), groups, census.Groups, trace.InitialGroups(), unionRule)
	}
}

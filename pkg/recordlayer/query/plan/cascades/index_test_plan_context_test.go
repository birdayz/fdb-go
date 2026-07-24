package cascades

import "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

// indexTestPlanContext is a minimal PlanContext stub carrying a fixed set of match
// candidates, used by data-access-path planner tests (e.g. the InExplode and benchmark
// tests) that drive the full planner against a hand-built candidate set. Extracted from
// the retired ImplementIndexScanRule's test file (RFC-076) so the surviving tests that
// depend on it keep compiling.
type indexTestPlanContext struct {
	candidates []MatchCandidate
}

func (c *indexTestPlanContext) GetPlannerConfiguration() PlannerConfiguration {
	return DefaultPlannerConfiguration()
}

func (c *indexTestPlanContext) GetMatchCandidates() []MatchCandidate {
	return c.candidates
}

func (c *indexTestPlanContext) GetPrimaryKeyColumns(string) []string {
	return nil
}

// newKnownDistinctValueIndexCandidate constructs an ordinary scalar value
// index for shortcut-rule tests. The production metadata adapter supplies this
// affirmative non-fanout signal; the legacy constructor intentionally leaves
// it unknown so missing metadata fails closed.
func newKnownDistinctValueIndexCandidate(
	indexName string,
	recordTypes []string,
	columnNames []string,
	sargableAliases []values.CorrelationIdentifier,
	flowedType values.Type,
	unique bool,
	pkColumnNames []string,
) *ValueIndexScanMatchCandidate {
	createsDuplicates := false
	return NewValueIndexScanMatchCandidateWithFunctions(
		indexName,
		recordTypes,
		columnNames,
		nil,
		sargableAliases,
		flowedType,
		unique,
		pkColumnNames,
		&createsDuplicates,
	)
}

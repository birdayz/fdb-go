package cascades

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

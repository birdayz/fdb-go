package cascades

// PlanContext is the planner's context object — passed to every
// CascadesRule.OnMatch invocation, holds the static metadata about a
// record store the rule needs (planner config, available match
// candidates such as indexes, etc.).
//
// Ports the surface of Java's
// `com.apple.foundationdb.record.query.plan.cascades.PlanContext`. The
// Java interface depends on `RecordQueryPlannerConfiguration` (a rich
// planner-config struct) and `MatchCandidate` (per-match-candidate
// metadata, typically one per index). The seed substitutes
// PlannerConfiguration as a small struct + an empty match-candidate
// set, deferring the rich versions to subsequent shifts as their
// consumer rules port.
type PlanContext interface {
	// GetPlannerConfiguration returns the planner's configuration —
	// flag bag that controls per-feature planning behaviour (use
	// covering indexes, allow filter-pushdown, etc.).
	GetPlannerConfiguration() PlannerConfiguration

	// GetMatchCandidates returns the set of match candidates available
	// for index-pushdown rules. The seed returns an empty set; rules
	// that consult candidates need to wait for the IndexAccessHint /
	// MatchCandidate ports (B5 Batch A).
	GetMatchCandidates() []MatchCandidate
}

// PlannerConfiguration is the seed planner-config struct. Mirrors the
// subset of Java's `RecordQueryPlannerConfiguration` callers actually
// consult today — currently empty since the seed has no rules that
// branch on flags. As config-driven rules port, fields land here in
// step with their consumers.
type PlannerConfiguration struct {
	// AllowDuplicateProjections — when true, a projection can carry
	// the same Value twice (a SQL planner concession; the executor
	// handles the duplicate emission). False by default; matches
	// Java's `RecordQueryPlannerConfiguration.allowDuplicateProjections`.
	AllowDuplicateProjections bool
}

// DefaultPlannerConfiguration mirrors Java's
// `RecordQueryPlannerConfiguration.defaultPlannerConfiguration()`.
func DefaultPlannerConfiguration() PlannerConfiguration {
	return PlannerConfiguration{}
}

// MatchCandidate is the placeholder for a per-index match candidate
// — name plus a hook the rule machinery calls to test if the candidate
// applies. Concrete impls land alongside the index-pushdown rules
// (B5 Batch A).
type MatchCandidate interface {
	// CandidateName returns the candidate's identifier (typically the
	// index name).
	CandidateName() string
}

// emptyPlanContext is the no-info PlanContext singleton — used by
// rules that don't need the metadata (most simplification rules) and
// by tests.
type emptyPlanContext struct{}

func (emptyPlanContext) GetPlannerConfiguration() PlannerConfiguration {
	return DefaultPlannerConfiguration()
}
func (emptyPlanContext) GetMatchCandidates() []MatchCandidate { return nil }

// EmptyPlanContext returns the no-info PlanContext singleton.
// Equivalent to Java's `PlanContext.emptyContext()`.
func EmptyPlanContext() PlanContext {
	return emptyPlanContext{}
}

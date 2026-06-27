package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

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

	// GetPrimaryKeyColumns returns the primary key column names for
	// a given record type. Returns nil if the record type has no
	// explicit PK (defaults to synthetic PK).
	GetPrimaryKeyColumns(recordType string) []string
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

	// AttemptFailedInJoinAsUnionMaxSize controls when InUnionRule falls
	// back from InJoin to InUnion. Java default is 0 (no fallback).
	AttemptFailedInJoinAsUnionMaxSize int

	// ShouldJoinRightDeep — when true, PartitionSelectRule only
	// produces right-deep join trees (the upper partition has exactly
	// 1 quantifier). This reduces join enumeration combinatorics at the
	// cost of excluding bushy plans. Mirrors Java's
	// RecordQueryPlannerConfiguration.shouldJoinRightDeep().
	ShouldJoinRightDeep bool

	// ShouldDeferCrossProducts — when true, PartitionSelectRule only
	// partitions along independent quantifier boundaries (cross-product
	// splits). Partitions that would split connected components are
	// deferred until the components are individually partitioned into
	// smaller pieces. Mirrors Java's
	// RecordQueryPlannerConfiguration.shouldDeferCrossProducts().
	ShouldDeferCrossProducts bool
}

// DefaultPlannerConfiguration mirrors Java's
// `RecordQueryPlannerConfiguration.defaultPlannerConfiguration()`.
func DefaultPlannerConfiguration() PlannerConfiguration {
	return PlannerConfiguration{}
}

// MatchCandidate represents a scan candidate — typically a secondary
// index or the primary-key scan. During the EXPLORE phase, the
// planner matches query predicates against each candidate's sargable
// parameters; during OPTIMIZE it converts successful matches into
// physical index-scan plans.
//
// Ports the core surface of Java's
// `com.apple.foundationdb.record.query.plan.cascades.MatchCandidate`.
type MatchCandidate interface {
	// CandidateName returns the candidate's identifier (typically the
	// index name, or "primary" for the PK scan).
	CandidateName() string

	// GetTraversal returns the Traversal of this candidate's expression
	// tree, used by matching rules (MatchLeafRule, MatchIntermediateRule)
	// to walk the candidate structure. The traversal must be stable once
	// computed. Returns nil if the candidate has no expression tree
	// (seed-minimal candidates that don't yet support traversal-based
	// matching).
	//
	// Ports Java's MatchCandidate.getTraversal().
	GetTraversal() *Traversal

	// GetColumnNames returns the ordered column-name list (one per
	// index key column, parallel to GetSargableAliases). Used by rules
	// to match ComparisonPredicate field references against the index's
	// key columns.
	GetColumnNames() []string

	// GetSargableAliases returns the ordered list of parameter
	// identifiers (one per index key column, left-to-right) that can
	// be bound by predicate matching. The order determines the index
	// key prefix discipline: N equality-bound parameters followed by
	// at most one inequality-bound parameter form a valid scan prefix.
	GetSargableAliases() []values.CorrelationIdentifier

	// GetRecordTypes returns which record types this candidate covers.
	GetRecordTypes() []string

	// IsUnique reports whether the candidate's key uniquely identifies
	// a record (unique index or primary key).
	IsUnique() bool

	// ComputeBoundParameterPrefixMap computes the valid index-scan
	// prefix from a parameter→ComparisonRange binding map. Returns the
	// longest prefix of sargable parameters that satisfies the index
	// scan discipline (N equalities + optional trailing inequality).
	ComputeBoundParameterPrefixMap(
		bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	) map[values.CorrelationIdentifier]*predicates.ComparisonRange

	// ToScanPlan converts a successful match into a physical index
	// scan plan. Called with the prefix map from
	// ComputeBoundParameterPrefixMap + reverse flag.
	ToScanPlan(
		prefixMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
		reverse bool,
	) plans.RecordQueryPlan
}

// emptyPlanContext is the no-info PlanContext singleton — used by
// rules that don't need the metadata (most simplification rules) and
// by tests.
type emptyPlanContext struct{}

func (emptyPlanContext) GetPrimaryKeyColumns(string) []string { return nil }
func (emptyPlanContext) GetPlannerConfiguration() PlannerConfiguration {
	return DefaultPlannerConfiguration()
}
func (emptyPlanContext) GetMatchCandidates() []MatchCandidate { return nil }

// EmptyPlanContext returns the no-info PlanContext singleton.
// Equivalent to Java's `PlanContext.emptyContext()`.
func EmptyPlanContext() PlanContext {
	return emptyPlanContext{}
}

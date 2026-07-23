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
// `com.apple.foundationdb.record.query.plan.cascades.PlanContext`:
// PlannerConfiguration mirrors the consulted subset of Java's
// `RecordQueryPlannerConfiguration`, and MatchCandidate the
// per-candidate (typically per-index) metadata.
type PlanContext interface {
	// GetPlannerConfiguration returns the planner's configuration —
	// flag bag that controls per-feature planning behaviour (use
	// covering indexes, allow filter-pushdown, etc.).
	GetPlannerConfiguration() PlannerConfiguration

	// GetMatchCandidates returns the set of match candidates available
	// for index-pushdown rules — one per index plus the primary scan,
	// built by plan_context_builder.go from the store's metadata.
	// EmptyPlanContext returns none (rule unit tests).
	GetMatchCandidates() []MatchCandidate

	// GetPrimaryKeyColumns returns the primary key column names for
	// a given record type. Returns nil if the record type has no
	// explicit PK (defaults to synthetic PK).
	GetPrimaryKeyColumns(recordType string) []string
}

// PlannerConfiguration mirrors the subset of Java's
// `RecordQueryPlannerConfiguration` that rules actually consult. As
// further config-driven rules port, fields land here in step with
// their consumers.
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

	// IndexScanPreference mirrors Java's
	// RecordQueryPlannerConfiguration.getIndexScanPreference() (proto enum
	// PREFER_SCAN=0 / PREFER_INDEX=1 / PREFER_PRIMARY_KEY_INDEX=2). Consulted by
	// the cost model's primary-vs-index tie-break (comparePrimaryScanToIndexScan).
	// Zero value = PreferScan, the Cascades default; no SQL surface sets a
	// non-default today (same as the sibling knobs), so this changes no reachable
	// plan — it models the mechanism for parity.
	IndexScanPreference IndexScanPreference
}

// IndexScanPreference selects the default direction of the cost model's
// primary-scan-vs-index-scan tie-break, mirroring Java's
// RecordQueryPlannerConfiguration.IndexScanPreference proto enum.
type IndexScanPreference int

const (
	// PreferScan prefers a full/primary scan over an index scan (Cascades
	// default; proto PREFER_SCAN = 0).
	PreferScan IndexScanPreference = iota
	// PreferIndex prefers an index scan (proto PREFER_INDEX = 1).
	PreferIndex
	// PreferPrimaryKeyIndex prefers an index scan on the primary key (proto
	// PREFER_PRIMARY_KEY_INDEX = 2). Java's comparePrimaryScanToIndexScan treats
	// it identically to PREFER_INDEX in the tie-break (prefer the index).
	PreferPrimaryKeyIndex
)

// DefaultPlannerConfiguration mirrors Java's
// `RecordQueryPlannerConfiguration.defaultPlannerConfiguration()`.
func DefaultPlannerConfiguration() PlannerConfiguration {
	return PlannerConfiguration{
		// Java defaults deferCrossProducts ON (RecordQueryPlannerConfiguration:
		// the DONT_DEFER_CROSS_PRODUCTS flag is unset by default): a select
		// whose quantifier graph has >1 independent components partitions only
		// along component boundaries — the unavoidable cross products — and
		// the components then partition normally inside. With this OFF the
		// PartitionSelectRule gate at its call site is dead code and a fully
		// disconnected N-ary select (`FROM a, b, c`) enumerates every
		// bipartition of a pure cross product.
		ShouldDeferCrossProducts: true,
	}
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
	// (a candidate that does not support traversal-based matching).
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

	// ComputeBoundParameterPrefixMap computes the subset of parameter bindings
	// that this candidate's physical scan can consume. For ordinary ordered
	// scans that is the longest valid sargable prefix (N equalities plus an
	// optional trailing inequality); specialized candidates may retain
	// independent index-only bindings as well.
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

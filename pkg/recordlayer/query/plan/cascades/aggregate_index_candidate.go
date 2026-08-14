package cascades

import (
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// AggregateIndexMatchCandidate represents a pre-computed aggregate index
// in FDB (e.g., SUM, COUNT, MAX_EVER_LONG, MIN_EVER_LONG). Such indexes
// maintain running aggregates grouped by a set of key columns. A query
// like "SELECT region, SUM(amount) FROM t GROUP BY region" can be
// answered directly from a SUM index on (region, amount) without scanning
// any data rows.
//
// Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.AggregateIndexMatchCandidate`,
// carrying the surface AggregateDataAccessRule consumes.
type AggregateIndexMatchCandidate struct {
	indexName   string
	recordTypes []string
	groupCols   []string
	aggFunction expressions.AggregateFunction
	aggColumn   string
	aliases     []values.CorrelationIdentifier
	// groupKeyTypes and physicalGroupingPrefixCount answer a SARGABILITY
	// question — which grouping coordinates a scan range can bind, and how far
	// the logical grouping columns stay a contiguous prefix of the physical
	// BY_GROUP key. baseRowType below answers an ORDERING question. Neither
	// subsumes the other; see physical_key_types.go's header.
	groupKeyTypes               []values.Type
	physicalGroupingPrefixCount int
	// baseRowType is the DECLARED layout of the record type this index is built
	// over — the descriptor-shaped positional type, from the one authority
	// (executor.PositionalTypeForDescriptor / values.FieldTypeForProtoField).
	//
	// It exists so the grouping columns, which the candidate otherwise knows
	// only as NAMES, can be asked whether they may extend an ordering claim. An
	// aggregate index is stored grouped, so the plan over it advertises group
	// order — and a FLOAT/DOUBLE group column does not deliver one (see
	// values/ordering_claim.go). Without a layout that question has no true
	// answer, which is how the claim was stated unconditionally.
	//
	// UnknownType when the index serves more than one record type, or none with
	// a descriptor: no single declared layout exists, and the claim then falls
	// back to the fail-open direction the predicate documents.
	baseRowType values.Type

	// countsRows records that this index's stored value is the number of ROWS in
	// the group — i.e. it is a COUNT(*) index (record-layer type `count`), not a
	// COUNT(col) one (`count_not_null`). Both surface as AggCount with the same
	// grouping, so the aggregate function alone cannot tell them apart, and the
	// difference is decisive twice over (RFC-209):
	//
	//   - only a row count makes a stored zero mean "vacated group", because a
	//     live group always has at least one row (§5.3(a)); and
	//   - only a row count can serve as another index's group-existence
	//     companion, because it never looks at the aggregated value and so never
	//     drops a live group whose values are all NULL (§5.1).
	//
	// COUNT(col) is neither: an all-NULL live group legitimately answers 0.
	countsRows bool

	// groupingSignature is the normalized proto encoding of the index's GROUPING
	// key expression — the leading, non-aggregated half of its root. Two
	// aggregate indexes group identically iff their signatures are equal, which
	// is how a group-existence companion is discovered STRUCTURALLY rather than
	// through a stored reference (RFC-209 §5.2). nil when the signature could
	// not be derived, which declines every companion match rather than guessing.
	groupingSignature []byte

	// predicateSignature is the normalized proto encoding of the index's SPARSE
	// (WHERE) predicate, nil for a dense index. A companion must count exactly
	// the rows the owner aggregated: a dense COUNT(*) over a sparse SUM lists
	// groups holding no qualifying row, so driving the merge from it invents
	// groups — the very over-approximation the companion exists to remove.
	predicateSignature []byte

	// needsGroupExistenceCompanion records that this index's own key set can
	// neither prove nor disprove that a group exists, so a grouped query over it
	// MUST be companion-joined or must not use the index at all (RFC-209
	// §5.3(b)/(c)). True for a grouped SUM or COUNT(col); false for a COUNT(*)
	// (its own oracle), for MIN/MAX (a real per-record entry the record's
	// deletion removes), and for every ungrouped spelling (one group, which
	// exists regardless).
	needsGroupExistenceCompanion bool

	traversalOnce sync.Once
	traversal     *Traversal
}

// NewAggregateIndexMatchCandidate creates a candidate for an aggregate
// index. groupCols are the grouping key columns; aggFunction + aggColumn
// describe the pre-computed aggregate; baseRowType is the declared layout the
// grouping-column names resolve against (values.UnknownType when there is no
// single one).
//
// groupKeyTypes and physicalGroupingPrefixCount are PRECONDITIONS, not optional
// enhancements. Callers must supply the authoritative physical grouping-key
// types; a candidate without them cannot exist. The reason is that an Unknown
// physical type must DECLINE range eligibility — the binder cannot prove an
// exact probe against a type it does not know (scan_range_binding.go's
// validateAuthoritativeScanPhysicalType) — so a candidate built without them
// silently declines every binding, and a silently-declining candidate is how
// aggregate intersection dies invisibly. Making them constructor arguments
// makes that unsound state unconstructible rather than merely discouraged.
//
// physicalGroupingPrefixCount is the number of grouping columns that stay a
// contiguous leading prefix of the physical BY_GROUP key: groupingCount for an
// ordinary aggregate, groupingCount-permutedSize for PERMUTED_MIN/MAX. It is
// clamped to [0, len(groupCols)].
func NewAggregateIndexMatchCandidate(
	indexName string,
	recordTypes []string,
	groupCols []string,
	aggFunction expressions.AggregateFunction,
	aggColumn string,
	baseRowType values.Type,
	groupKeyTypes []values.Type,
	physicalGroupingPrefixCount int,
) *AggregateIndexMatchCandidate {
	recordTypes = append([]string(nil), recordTypes...)
	groupCols = append([]string(nil), groupCols...)
	aliases := make([]values.CorrelationIdentifier, len(groupCols))
	for i := range aliases {
		aliases[i] = values.UniqueCorrelationIdentifier()
	}
	if baseRowType == nil {
		baseRowType = values.UnknownType
	} else if exact, err := values.SnapshotExactType(baseRowType); err == nil {
		baseRowType = exact.Type()
	}
	groupKeyTypes = normalizePhysicalKeyTypes(groupKeyTypes, len(groupCols))
	for i, typ := range groupKeyTypes {
		if exact, err := values.SnapshotExactType(typ); err == nil {
			groupKeyTypes[i] = exact.Type()
		}
	}
	if physicalGroupingPrefixCount < 0 {
		physicalGroupingPrefixCount = 0
	}
	if physicalGroupingPrefixCount > len(groupCols) {
		physicalGroupingPrefixCount = len(groupCols)
	}
	return &AggregateIndexMatchCandidate{
		indexName:                   indexName,
		recordTypes:                 recordTypes,
		groupCols:                   groupCols,
		aggFunction:                 aggFunction,
		aggColumn:                   aggColumn,
		aliases:                     aliases,
		baseRowType:                 baseRowType,
		groupKeyTypes:               groupKeyTypes,
		physicalGroupingPrefixCount: physicalGroupingPrefixCount,
	}
}

// GetBaseRowType returns the declared layout the grouping-column names resolve
// against, or values.UnknownType when the index has no single one.
func (c *AggregateIndexMatchCandidate) GetBaseRowType() values.Type {
	if exact, err := values.SnapshotExactType(c.baseRowType); err == nil {
		return exact.Type()
	}
	return c.baseRowType
}

// GetBaseType exposes the exact candidate row through the common
// MatchCandidate contract consumed by index expansion. Aggregate candidates
// historically used only the more specific GetBaseRowType spelling, which
// made the exact-type gate silently exclude every aggregate traversal.
func (c *AggregateIndexMatchCandidate) GetBaseType() values.Type {
	return c.GetBaseRowType()
}

// GetKeyComponentTypes returns logical grouping-key types. The scan plan
// aligns the leading entries to the comparisons it actually carries.
func (c *AggregateIndexMatchCandidate) GetKeyComponentTypes() []values.Type {
	result := make([]values.Type, len(c.groupKeyTypes))
	for i, typ := range c.groupKeyTypes {
		if exact, err := values.SnapshotExactType(typ); err == nil {
			result[i] = exact.Type()
		} else {
			result[i] = typ
		}
	}
	return result
}

// GetPhysicalGroupingPrefixCount returns the number of grouping columns that
// occur before the aggregate value in the physical BY_GROUP key.
func (c *AggregateIndexMatchCandidate) GetPhysicalGroupingPrefixCount() int {
	return c.physicalGroupingPrefixCount
}

// WithGroupExistence records the two structural facts RFC-209 needs about the
// underlying index: whether its stored value counts ROWS (a COUNT(*) index),
// and the normalized encoding of its grouping key expression. Both come from
// the record-layer Index and are supplied by the candidate builder; see the
// field docs for what each decides.
func (c *AggregateIndexMatchCandidate) WithGroupExistence(countsRows bool, groupingSignature []byte) *AggregateIndexMatchCandidate {
	c.countsRows = countsRows
	c.groupingSignature = groupingSignature
	return c
}

// WithGroupExistenceCompanionNeed records the remaining two facts: the index's
// sparse-predicate signature (which a companion must match, or it counts a
// different row population) and whether a grouped query over this index needs a
// companion at all.
func (c *AggregateIndexMatchCandidate) WithGroupExistenceCompanionNeed(
	predicateSignature []byte, needsCompanion bool,
) *AggregateIndexMatchCandidate {
	c.predicateSignature = predicateSignature
	c.needsGroupExistenceCompanion = needsCompanion
	return c
}

// PredicateSignature returns the normalized encoding of the index's sparse
// predicate, nil for a dense index.
func (c *AggregateIndexMatchCandidate) PredicateSignature() []byte { return c.predicateSignature }

// NeedsGroupExistenceCompanion reports whether a GROUPED query over this index
// must be companion-joined to answer correctly.
func (c *AggregateIndexMatchCandidate) NeedsGroupExistenceCompanion() bool {
	return c.needsGroupExistenceCompanion && len(c.groupCols) > 0
}

// CountsRows reports whether the index's stored value is the group's row count.
func (c *AggregateIndexMatchCandidate) CountsRows() bool { return c.countsRows }

// GroupingSignature returns the normalized encoding of the grouping key
// expression, or nil when it could not be derived.
func (c *AggregateIndexMatchCandidate) GroupingSignature() []byte { return c.groupingSignature }

func (c *AggregateIndexMatchCandidate) CandidateName() string { return c.indexName }

// GetTraversal returns the Traversal of this candidate's expression
// tree, built lazily on first access via ExpandValueIndex (using the
// grouping columns as the index columns). The traversal is stable once
// computed (sync.Once).
func (c *AggregateIndexMatchCandidate) GetTraversal() *Traversal {
	c.traversalOnce.Do(func() {
		c.traversal = ExpandValueIndex(c)
	})
	return c.traversal
}
func (c *AggregateIndexMatchCandidate) GetColumnNames() []string { return c.groupCols }
func (c *AggregateIndexMatchCandidate) GetRecordTypes() []string { return c.recordTypes }
func (c *AggregateIndexMatchCandidate) IsUnique() bool           { return false }
func (c *AggregateIndexMatchCandidate) GetAggFunction() expressions.AggregateFunction {
	return c.aggFunction
}
func (c *AggregateIndexMatchCandidate) GetAggColumn() string { return c.aggColumn }

func (c *AggregateIndexMatchCandidate) GetSargableAliases() []values.CorrelationIdentifier {
	return c.aliases
}

func (c *AggregateIndexMatchCandidate) ComputeBoundParameterPrefixMap(
	bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	prefix := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
	for _, alias := range c.aliases[:c.physicalGroupingPrefixCount] {
		cr, ok := bindings[alias]
		if !ok || cr == nil {
			break
		}
		if !cr.IsEquality() {
			prefix[alias] = cr
			break
		}
		prefix[alias] = cr
	}
	return prefix
}

func (c *AggregateIndexMatchCandidate) bindingRangesEligible(
	bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) bool {
	physicalAliases := c.aliases[:c.physicalGroupingPrefixCount]
	// Aggregate-index rows cannot carry a residual record predicate. Decline
	// both incomplete exact-NaN probes and bounds whose tuple-wire domain is
	// unknown; neither can be repaired above the pre-aggregated stream.
	return !bindingsUseUnknownPhysicalKeyType(bindings, physicalAliases, c.groupKeyTypes) &&
		!bindingsContainUnsupportedPhysicalFloatOrdering(bindings, physicalAliases, c.groupKeyTypes) &&
		!bindingsContainUnsupportedPhysicalStartsWith(bindings, physicalAliases, c.groupKeyTypes) &&
		!bindingsContainKnownConstantNaN(bindings, physicalAliases, c.groupKeyTypes)
}

func (c *AggregateIndexMatchCandidate) ToScanPlan(
	prefixMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	reverse bool,
) plans.RecordQueryPlan {
	if _, err := values.SnapshotExactType(c.baseRowType); err != nil {
		// Match candidates are optional access paths. Without the declared row
		// layout there is no exact result identity for the embedded scan, so this
		// candidate is unavailable rather than represented by UnknownType.
		return nil
	}
	comps := make([]*predicates.ComparisonRange, 0, c.physicalGroupingPrefixCount)
	for _, alias := range c.aliases[:c.physicalGroupingPrefixCount] {
		cr, ok := prefixMap[alias]
		if !ok {
			break
		}
		comps = append(comps, cr)
	}
	indexPlan, err := plans.NewRecordQueryIndexPlan(c.indexName, comps, c.recordTypes, c.baseRowType, reverse)
	if err != nil {
		return nil
	}
	return stampIndexMetadata(c, indexPlan).
		WithPhysicalGroupingPrefixCount(c.physicalGroupingPrefixCount)
}

// MatchesGroupBy reports whether this aggregate index can directly satisfy
// the given GroupByExpression. Returns true when:
//   - The grouping keys match the index's groupCols
//   - The GroupBy has exactly one aggregate that matches the index's function + column
func (c *AggregateIndexMatchCandidate) MatchesGroupBy(gb *expressions.GroupByExpression) bool {
	// Grouping subsumption is leaf-level (Java expands the grouping value
	// via Values.primitiveAccessorsForType before matching,
	// GroupByExpression.java:434): a RECORD-typed key contributes its
	// primitive leaves, so it can never falsely name-match a candidate's
	// scalar grouping column. Identity for all-primitive keys.
	keys, err := expandGroupingKeysToPrimitives(gb.GetGroupingKeys())
	if err != nil {
		return false
	}
	if len(keys) != len(c.groupCols) {
		return false
	}
	for i, k := range keys {
		if !aggColumnMatches(k, c.groupCols[i]) {
			return false
		}
	}

	aggs := gb.GetAggregates()
	if len(aggs) != 1 {
		return false
	}
	if aggs[0].Function != c.aggFunction {
		return false
	}
	if c.aggFunction == expressions.AggCount {
		// Single source of truth for count-star (RFC-164 WS-3) — must match the
		// executor's group cursors and the translator's normalization.
		if expressions.IsCountStar(aggs[0]) {
			return c.aggColumn == ""
		}
		return c.aggColumn != "" && aggColumnMatches(aggs[0].Operand, c.aggColumn)
	}
	return aggColumnMatches(aggs[0].Operand, c.aggColumn)
}

// MatchesSingleAggregateOf reports whether this candidate's grouping
// keys match gb's grouping keys AND this candidate covers the aggregate
// at index aggIndex in gb's aggregate list. Used by the multi-aggregate
// intersection path: each candidate covers one aggregate while all
// share the same grouping columns.
func (c *AggregateIndexMatchCandidate) MatchesSingleAggregateOf(gb *expressions.GroupByExpression, aggIndex int) bool {
	// Same leaf-level matching as MatchesGroupBy (GroupByExpression.java:434).
	keys, err := expandGroupingKeysToPrimitives(gb.GetGroupingKeys())
	if err != nil {
		return false
	}
	if len(keys) != len(c.groupCols) {
		return false
	}
	for i, k := range keys {
		if !aggColumnMatches(k, c.groupCols[i]) {
			return false
		}
	}

	aggs := gb.GetAggregates()
	if aggIndex < 0 || aggIndex >= len(aggs) {
		return false
	}
	agg := aggs[aggIndex]
	if agg.Function != c.aggFunction {
		return false
	}
	if c.aggFunction == expressions.AggCount {
		// Single source of truth for count-star (RFC-164 WS-3) — must match the
		// executor's group cursors and the translator's normalization.
		if expressions.IsCountStar(agg) {
			return c.aggColumn == ""
		}
		return c.aggColumn != "" && aggColumnMatches(agg.Operand, c.aggColumn)
	}
	return aggColumnMatches(agg.Operand, c.aggColumn)
}

// aggColumnMatches reports whether a query grouping-key / aggregate-operand
// value denotes the aggregate index's declared column `col` — by full accessor
// PATH, not leaf name, so a nested `addr.city` grouping key never matches a
// same-leaf-named top-level `city` aggregate index (RFC-187 S4/S5/S8). The
// candidate carries single top-level column names today, so a multi-accessor
// (nested) query path does not match and the query falls back to a base-record
// StreamingAgg (correct rows, slower) — the transitional reject-nested until the
// candidate exposes real nested column paths end-to-end (construction, index
// expansion, and execution), tracked as the RFC-187 §3.2 follow-up.
func aggColumnMatches(v values.Value, col string) bool {
	return values.AccessorNamePathMatchesNames(v, []string{col})
}

var _ MatchCandidate = (*AggregateIndexMatchCandidate)(nil)

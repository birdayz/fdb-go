package cascades

import (
	"slices"
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
	bitmapEntrySize int64
	permuted        bool
	indexName       string
	recordTypes     []string
	groupCols       []string
	aggFunction     expressions.AggregateFunction
	aggColumn       string
	// groupPaths and aggPath are the columns' identities: the full field path
	// each reads from the base (Java's expansion Values are FieldValues over
	// these paths). groupCols and aggColumn are their labels, the leaf names.
	groupPaths [][]string
	aggPath    []string
	aliases    []values.CorrelationIdentifier
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
	groupPaths := make([][]string, len(groupCols))
	for i, name := range groupCols {
		groupPaths[i] = []string{name}
	}
	var aggPath []string
	if aggColumn != "" {
		aggPath = []string{aggColumn}
	}
	return &AggregateIndexMatchCandidate{
		indexName:                   indexName,
		recordTypes:                 recordTypes,
		groupCols:                   groupCols,
		aggFunction:                 aggFunction,
		aggColumn:                   aggColumn,
		groupPaths:                  groupPaths,
		aggPath:                     aggPath,
		aliases:                     aliases,
		baseRowType:                 baseRowType,
		groupKeyTypes:               groupKeyTypes,
		physicalGroupingPrefixCount: physicalGroupingPrefixCount,
	}
}

// WithColumnPaths states the full field paths of the grouping columns and the
// aggregated column, for an index whose columns read nested fields; each
// path's leaf is the column's label. A path list that does not name every
// labelled column leaves the top-level reading in place.
func (c *AggregateIndexMatchCandidate) WithColumnPaths(groupPaths [][]string, aggPath []string) *AggregateIndexMatchCandidate {
	if len(groupPaths) != len(c.groupPaths) || (len(aggPath) == 0) != (len(c.aggPath) == 0) {
		return c
	}
	for _, path := range groupPaths {
		if len(path) == 0 {
			return c
		}
	}
	c.groupPaths = make([][]string, len(groupPaths))
	for i, path := range groupPaths {
		c.groupPaths[i] = slices.Clone(path)
	}
	c.aggPath = slices.Clone(aggPath)
	return c
}

// GetGroupColumnPaths returns each grouping column's full field path.
func (c *AggregateIndexMatchCandidate) GetGroupColumnPaths() [][]string { return c.groupPaths }

// GetAggColumnPath returns the aggregated column's full field path, nil for
// COUNT(*).
func (c *AggregateIndexMatchCandidate) GetAggColumnPath() []string { return c.aggPath }

// WithPermutedOrdering records that the aggregate itself occupies a key coordinate.
func (c *AggregateIndexMatchCandidate) WithPermutedOrdering(permuted bool) *AggregateIndexMatchCandidate {
	c.permuted = permuted
	return c
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
// the given GroupByExpression: its grouping keys are the index's grouping
// columns, and its one aggregate is the index's function over its column.
func (c *AggregateIndexMatchCandidate) MatchesGroupBy(gb *expressions.GroupByExpression) bool {
	aggs := gb.GetAggregates()
	return len(aggs) == 1 && c.groupingKeysMatch(gb) && c.aggregateMatches(aggs[0])
}

// MatchesSingleAggregateOf reports whether this candidate's grouping
// keys match gb's grouping keys AND this candidate covers the aggregate
// at index aggIndex in gb's aggregate list. Used by the multi-aggregate
// intersection path: each candidate covers one aggregate while all
// share the same grouping columns.
func (c *AggregateIndexMatchCandidate) MatchesSingleAggregateOf(gb *expressions.GroupByExpression, aggIndex int) bool {
	aggs := gb.GetAggregates()
	return aggIndex >= 0 && aggIndex < len(aggs) && c.groupingKeysMatch(gb) && c.aggregateMatches(aggs[aggIndex])
}

// groupingKeysMatch is GroupByExpression.groupingSubsumedBy followed by the
// pull-up of the query's grouping values from the candidate's result. The
// subsumption is leaf-level: each grouping value is expanded to its primitive
// accessors (Values.primitiveAccessorsForType) and the leaves are the
// candidate's grouping columns, in order. The aggregate row then publishes
// the GroupBy's grouping values slot for slot, so each must itself be one
// candidate column: a RECORD-typed key is built from several, which the
// result value cannot be pulled up through (Java plans no match for it).
func (c *AggregateIndexMatchCandidate) groupingKeysMatch(gb *expressions.GroupByExpression) bool {
	groupingKeys := gb.GetGroupingKeys()
	keys, err := expandGroupingKeysToPrimitives(groupingKeys)
	if err != nil || len(keys) != len(c.groupCols) {
		return false
	}
	for i, k := range keys {
		if !c.groupKeyMatches(k, i) {
			return false
		}
	}
	for _, k := range groupingKeys {
		if _, isRecord := k.Type().(*values.RecordType); isRecord {
			return false
		}
	}
	return len(groupingKeys) == len(keys)
}

func (c *AggregateIndexMatchCandidate) aggregateMatches(agg expressions.AggregateSpec) bool {
	if agg.Function != c.aggFunction {
		return false
	}
	if c.aggFunction == expressions.AggCount {
		// Single source of truth for count-star (RFC-164 WS-3) — must match the
		// executor's group cursors and the translator's normalization.
		if expressions.IsCountStar(agg) {
			return c.aggPath == nil
		}
		return c.aggPath != nil && aggColumnMatches(agg.Operand, c.aggPath)
	}
	return c.aggregateOperandMatches(agg.Operand)
}

// aggColumnMatches reports whether a query grouping-key / aggregate-operand
// value reads the candidate column at path — by the full accessor PATH, so a
// nested `addr.city` and a top-level `city` are different columns (RFC-187
// S4/S5/S8), as Java's FieldValues over the expansion's base are.
func aggColumnMatches(v values.Value, path []string) bool {
	return values.AccessorNamePathMatchesNames(v, path)
}

var _ MatchCandidate = (*AggregateIndexMatchCandidate)(nil)

// WithBitmapEntrySize carries the two arithmetic expressions introduced by
// Java BitmapAggregateIndexExpansionVisitor: bucket offset and bit position.
func (c *AggregateIndexMatchCandidate) WithBitmapEntrySize(size int64) *AggregateIndexMatchCandidate {
	c.bitmapEntrySize = size
	return c
}

func (c *AggregateIndexMatchCandidate) groupKeyMatches(v values.Value, ordinal int) bool {
	if c.bitmapEntrySize > 0 && ordinal == len(c.groupCols)-1 {
		return c.bitmapArithmeticMatches(v, values.OpBitmapBucketOffset)
	}
	return aggColumnMatches(v, c.groupPaths[ordinal])
}

func (c *AggregateIndexMatchCandidate) aggregateOperandMatches(v values.Value) bool {
	if c.bitmapEntrySize > 0 {
		return c.bitmapArithmeticMatches(v, values.OpBitmapBitPosition)
	}
	return aggColumnMatches(v, c.aggPath)
}

func (c *AggregateIndexMatchCandidate) bitmapArithmeticMatches(v values.Value, op values.ArithmeticOp) bool {
	arithmetic, ok := v.(*values.ArithmeticValue)
	if !ok || arithmetic.Op != op || !aggColumnMatches(arithmetic.Left, c.aggPath) {
		return false
	}
	size, ok := arithmetic.Right.(*values.ConstantValue)
	if !ok {
		return false
	}
	n, ok := size.Value.(int64)
	return ok && n == c.bitmapEntrySize
}

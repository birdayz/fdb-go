package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryAggregateIndexPlan wraps an index scan that reads from
// an aggregate index (e.g. SUM, COUNT) and reconstructs records from
// the index entries. This is a leaf plan (no children — the wrapped
// RecordQueryIndexPlan is a structural field, not a child in the plan
// tree sense). Mirrors Java's RecordQueryAggregateIndexPlan.
//
// Fields:
//
//   - indexPlan: the underlying index scan plan.
//
//   - recordTypeName: the base record type name, used FOR A METADATA LOOKUP
//     (cascades_generator.go derives this plan's result-column types by calling
//     md.GetRecordType on it) as well as for the explain string and the
//     scan-range execution identity.
//
//     THAT LOOKUP IS NIL-TOLERANT AND ITS MISS IS SILENT, which is a live
//     hazard rather than a nicety: on a miss the descriptor stays nil, every
//     GROUP BY column falls back to STRING, and an aggregate OVER A COLUMN
//     falls back to BIGINT -- plausible defaults, wrong types, no error.
//     COUNT(*) is BIGINT with or without the miss, so it is the one output the
//     miss does not degrade. A SECOND consumer defaults differently: the
//     multi-intersection derivation reports GROUP BY columns as BIGINT, and it
//     reaches its miss only when EVERY child plan misses -- so the degraded
//     type depends on which derivation ran, which is worse than either default
//     alone.
//
//     RFC-238 §7f carries the two axes that could reach the miss, and BOTH ARE
//     NOW CLOSED, for different reasons. The namespace one is forbidden by
//     §7c's committed design, which translates on the QUERY side precisely so
//     the candidate side does not move; it does not arm, and the reference is
//     not licence to move it. The EMPTY association -- RecordTypesForIndex
//     returning nothing for an index that is neither universal nor associated,
//     leaving this field empty so GetRecordType("") misses -- is refused in
//     Build, which requires every registered index to be universal or claimed
//     by some record type. An earlier version of this paragraph said the state
//     had exactly ONE route, a second SetRecords call; it had several, because
//     the builder hands out live maps, and enumerating them is what went wrong.
//     Pinned by TestBuildRefusesAnIndexNoRecordTypeClaims and
//     TestBuiltMetadataIsDetachedFromTheBuilder in pkg/recordlayer.
//
//     WHAT THAT ROUTE COST IS NOT WHAT THIS COMMENT ANALYSES, which is worth
//     knowing before reviving the analysis above. An orphaned index did not
//     merely lose its descriptor: ToProto emitted it with an EMPTY RecordType
//     list, and a reload reads that as UNIVERSAL, so after a serialization
//     round trip RecordTypesForIndex answered with EVERY type rather than none.
//     The degraded-result-type hazard described here therefore did not survive
//     a reload; a different defect did. It matters HERE because this field
//     carries whichever namespace the plan was built in (RFC-238 §7c), so a
//     plan built with a SQL spelling against metadata keyed by the stored one
//     misses and degrades exactly that way.
//
//   - resultType: the rich Type of the aggregated result row.
//
//   - aggregateFunction: the name of the aggregate function
//     (e.g. "SUM", "COUNT", "MIN", "MAX").
type RecordQueryAggregateIndexPlan struct {
	PlanExprBase
	indexPlan         *RecordQueryIndexPlan
	recordTypeName    string
	resultType        values.Type
	aggregateFunction string
	permuted          bool
	groupCols         []string
	aggColumn         string
	// groupColPaths and aggColumnPath are the columns' full field paths when
	// they read nested fields (groupCols and aggColumn are then leaf labels).
	groupColPaths [][]string
	aggColumnPath []string
	// groupColLayout is the DECLARED layout the groupCols names resolve
	// against, carried so HintOrdering can ask whether a grouping column may
	// extend an ordering claim. Nil/UnknownType leaves the claim unconstrained,
	// which is the direction values.ColumnCanExtendOrderingClaim documents.
	// Excluded from structuralKey: it is derived from the index's record type,
	// so two plans over the same index cannot disagree about it, and folding it
	// into plan identity would key the memo on a type token.
	groupColLayout values.Type

	// physicalGroupingPrefixCount is the number of logical grouping columns
	// that remain a contiguous leading prefix of the physical BY_GROUP key.
	// For ordinary aggregate indexes this is g; for PERMUTED_MIN/MAX it is g-p.
	//
	// Distinct question from groupColLayout above: this is about SARGABILITY
	// (how much of the group key a scan range can bind), while groupColLayout
	// answers whether a grouping column may extend an ORDERING claim. Neither
	// subsumes the other.
	physicalGroupingPrefixCount int
	physicalGroupingPrefixKnown bool

	// candidateGroupingCount is the grouping-column count of the candidate a
	// rule built this plan from, the evidence Java's cardinality reads off the
	// plan's match candidate; known is false for a plan built without one.
	candidateGroupingCount int
	candidateGroupingKnown bool

	// entryReader reads an entry into the result row
	// (AggregateIndexMatchCandidate.createIndexEntryToRecordValue). A pure
	// function of the index and result type, so it stays out of identity,
	// explain and execution salt; without one the cursor decodes the layout.
	entryReader *values.RecordConstructorValue
	// resultValue is the stable per-instance QuantifiedObjectValue standing for
	// the rows this leaf emits — minted once at construction, returned by
	// GetResultValue, EXCLUDED from Equals/Hash (its correlation id is unique per
	// instance). A bare leaf that stands as its own Cascades expression must
	// present a consistent row identity across repeated interrogations, the role
	// physicalAggregateIndexWrapper's fresh-per-call GetResultValue could not
	// (RFC-184 W2). nil for struct-literal test plans that bypass the constructor —
	// GetResultValue falls back to PlanExprBase's fresh QOV there.
	resultValue values.Value
}

// NewRecordQueryAggregateIndexPlan constructs an aggregate index plan.
func NewRecordQueryAggregateIndexPlan(
	indexPlan *RecordQueryIndexPlan,
	recordTypeName string,
	resultType values.Type,
	aggregateFunction string,
) (*RecordQueryAggregateIndexPlan, error) {
	base, err := newPlanExprBaseForType("RecordQueryAggregateIndexPlan", resultType)
	if err != nil {
		return nil, err
	}
	prefixCount, prefixKnown := 0, false
	if indexPlan != nil {
		prefixCount, prefixKnown = indexPlan.physicalGroupingPrefix()
	}
	return &RecordQueryAggregateIndexPlan{
		PlanExprBase:                base,
		indexPlan:                   indexPlan,
		recordTypeName:              recordTypeName,
		resultType:                  base.resultValue.Type(),
		aggregateFunction:           aggregateFunction,
		physicalGroupingPrefixCount: prefixCount,
		physicalGroupingPrefixKnown: prefixKnown,
		resultValue:                 base.resultValue,
	}, nil
}

// GetResultValue returns the aggregate-index plan's STABLE per-instance result
// value — the single correlation identity a bare aggregate-index plan carries as
// its own memo expression (RFC-184 W2). Falls back to PlanExprBase (a fresh QOV
// per call) for struct-literal test plans that bypass the constructor
// (resultValue is nil).
func (p *RecordQueryAggregateIndexPlan) GetResultValue() values.Value {
	return p.resultValue
}

// WithGroupColumns sets the grouping and aggregate column names for
// the executor to map index entries to result rows.
func (p *RecordQueryAggregateIndexPlan) WithGroupColumns(groupCols []string, aggColumn string) *RecordQueryAggregateIndexPlan {
	cp := *p
	cp.groupCols = append([]string(nil), groupCols...)
	cp.aggColumn = aggColumn
	if !cp.physicalGroupingPrefixKnown {
		cp.physicalGroupingPrefixCount = len(groupCols)
		cp.physicalGroupingPrefixKnown = true
	}
	return &cp
}

// WithColumnPaths carries the full field paths of the grouping and aggregated
// columns, so the layout is asked about a nested column by its path, never by
// its leaf label.
func (p *RecordQueryAggregateIndexPlan) WithColumnPaths(groupPaths [][]string, aggPath []string) *RecordQueryAggregateIndexPlan {
	cp := *p
	cp.groupColPaths = make([][]string, len(groupPaths))
	for i, path := range groupPaths {
		cp.groupColPaths[i] = append([]string(nil), path...)
	}
	cp.aggColumnPath = append([]string(nil), aggPath...)
	return &cp
}

// nestedGroupColumnPath is grouping column i's path when it reads a nested
// field, nil for a top-level column.
func (p *RecordQueryAggregateIndexPlan) nestedGroupColumnPath(i int) []string {
	if i < 0 || i >= len(p.groupColPaths) || len(p.groupColPaths[i]) < 2 {
		return nil
	}
	return p.groupColPaths[i]
}

// aggColumnField is the aggregated column's field in the layout, by its path
// when one was carried.
func (p *RecordQueryAggregateIndexPlan) aggColumnField(layout values.Type) (values.Field, bool) {
	path := p.aggColumnPath
	if len(path) == 0 {
		path = []string{p.aggColumn}
	}
	return values.LookupFieldPathUnique(layout, path)
}

// WithPermutedOrdering makes the aggregate participate in the physical key order.
func (p *RecordQueryAggregateIndexPlan) WithPermutedOrdering(permuted bool) *RecordQueryAggregateIndexPlan {
	cp := *p
	cp.permuted = permuted
	return &cp
}

// WithGroupColumnLayout carries the declared layout the grouping-column names
// resolve against — the base record type's descriptor-shaped positional type.
// Only HintOrdering reads it, to decide whether a grouping column may extend
// the group-order claim this plan makes.
func (p *RecordQueryAggregateIndexPlan) WithGroupColumnLayout(layout values.Type) *RecordQueryAggregateIndexPlan {
	cp := *p
	cp.groupColLayout = layout
	return &cp
}

// GetGroupColumnLayout returns the declared layout the grouping-column names
// resolve against, or nil when none was carried.
func (p *RecordQueryAggregateIndexPlan) GetGroupColumnLayout() values.Type { return p.groupColLayout }

// WithCandidateGroupingCount records the grouping-column count of the
// candidate the plan was built from.
func (p *RecordQueryAggregateIndexPlan) WithCandidateGroupingCount(n int) *RecordQueryAggregateIndexPlan {
	cp := *p
	cp.candidateGroupingCount = n
	cp.candidateGroupingKnown = true
	return &cp
}

// WithEntryReader attaches the reader the cursor builds each row with.
func (p *RecordQueryAggregateIndexPlan) WithEntryReader(reader *values.RecordConstructorValue) *RecordQueryAggregateIndexPlan {
	cp := *p
	cp.entryReader = reader
	return &cp
}

// GetEntryReader is the reader of an entry into the result row, or nil.
func (p *RecordQueryAggregateIndexPlan) GetEntryReader() *values.RecordConstructorValue {
	if p == nil {
		return nil
	}
	return p.entryReader
}

// GetGroupCols returns the grouping column names.
func (p *RecordQueryAggregateIndexPlan) GetGroupCols() []string {
	return append([]string(nil), p.groupCols...)
}

// GetAggColumn returns the aggregate column name.
func (p *RecordQueryAggregateIndexPlan) GetAggColumn() string { return p.aggColumn }

// GetKeyComponentTypes returns the authoritative physical grouping-key types
// aligned with the underlying index scan's comparisons.
func (p *RecordQueryAggregateIndexPlan) GetKeyComponentTypes() []values.Type {
	if p.indexPlan == nil {
		return nil
	}
	return p.indexPlan.GetKeyComponentTypes()
}

// GetPhysicalGroupingPrefixCount returns the g-p boundary before the inserted
// aggregate value in a permuted BY_GROUP key.
func (p *RecordQueryAggregateIndexPlan) GetPhysicalGroupingPrefixCount() int {
	if p.physicalGroupingPrefixKnown {
		return p.physicalGroupingPrefixCount
	}
	return len(p.groupCols)
}

// CanonicalAggColumnName returns the index's own name for its aggregate
// column: "FUNC(*)" for an empty aggColumn (e.g. COUNT(*)), else "FUNC(col)".
// It is part of the scan's execution identity; the row the plan publishes is
// named by its result type.
func (p *RecordQueryAggregateIndexPlan) CanonicalAggColumnName() string {
	if p.aggColumn == "" {
		return p.aggregateFunction + "(*)"
	}
	return p.aggregateFunction + "(" + p.aggColumn + ")"
}

// GetIndexPlan returns the underlying index plan.
func (p *RecordQueryAggregateIndexPlan) GetIndexPlan() *RecordQueryIndexPlan { return p.indexPlan }

// GetRecordTypeName returns the base record type name.
func (p *RecordQueryAggregateIndexPlan) GetRecordTypeName() string { return p.recordTypeName }

// GetAggregateFunction returns the aggregate function name.
func (p *RecordQueryAggregateIndexPlan) GetAggregateFunction() string { return p.aggregateFunction }

// GetIndexName returns the index name from the underlying plan.
func (p *RecordQueryAggregateIndexPlan) GetIndexName() string {
	return p.indexPlan.GetIndexName()
}

// IsReverse delegates to the underlying index plan.
func (p *RecordQueryAggregateIndexPlan) IsReverse() bool {
	return p.indexPlan.IsReverse()
}

// GetResultType returns the aggregate result type.
func (p *RecordQueryAggregateIndexPlan) GetResultType() values.Type { return p.resultType }

// GetChildren returns nil — this is a leaf plan. The wrapped index
// plan is a structural field, not a child (mirrors Java where
// RecordQueryAggregateIndexPlan implements
// RecordQueryPlanWithNoChildren).
func (p *RecordQueryAggregateIndexPlan) GetChildren() []RecordQueryPlan { return nil }

// structuralKey folds the aggregate-index identity: the base record-type name,
// the aggregate-function name, and the embedded RecordQueryIndexPlan compared
// STRUCTURALLY via Sub (its own structuralKey — exactly what
// indexPlan.EqualsPlanWithoutChildren does). The stable per-instance resultValue
// is excluded (RFC-184 W2). Drives both Equals and Hash — the hash now folds the
// full nested index key (the hand-rolled hash folded only the index NAME),
// strengthening it while preserving equal⟹same-hash.
func (p *RecordQueryAggregateIndexPlan) structuralKey() *structuralKey {
	// groupCols and aggColumn are folded because they are what the executor's aggregateIndexCursor uses to map index entries
	// onto result rows, so plans differing only in them emit DIFFERENT ROWS from
	// one identity. GetPhysicalGroupingPrefixCount() covers only their ARITY — it
	// returns len(groupCols) when the count is not independently known — which is
	// why differing arity already separated while same-arity different NAMES
	// collapsed.
	//
	// Today's three planner call sites derive both from the match candidate, so
	// two plans over one index agree on them and the collapse is not reachable
	// from SQL. WithGroupColumns is a public builder that enforces nothing, the
	// fold costs two string parts, and the failure mode is serving a different
	// grouping's rows — so this is guarded rather than argued.
	//
	// Three fields stay out, each for its own reason, none of them oversight:
	// resultValue is a QuantifiedObjectValue whose correlation id is unique per
	// instance on newPlanExprBaseForType's ERASED-record branch, where folding it
	// would make every such plan unique and defeat interning (an exact record
	// type takes the layout branch and gets a deterministic carrier, so the
	// hazard is branch-specific rather than universal); groupColLayout is derived
	// from the index's record type (see its field comment — folding it would key
	// the memo on a type token); resultType's slot types are computed by
	// aggregateIndexOutputType from groupCols, the aggregate function and the
	// index's key component types, every one of which is already folded here.
	// Its field NAMES are the GroupBy's the plan publishes, so they are folded:
	// two scans of one index under different names state different rows.
	// The column paths are the columns' identities, folded with the labels.
	key := newStructuralKey().
		Strs(resultFieldNames(p.resultType)).
		Str(p.recordTypeName).
		Str(p.aggregateFunction).
		Str(p.aggColumn).
		Strs(p.groupCols).
		Strs(p.aggColumnPath).
		Int(len(p.groupColPaths))
	for _, path := range p.groupColPaths {
		key.Strs(path)
	}
	return key.
		Int(p.GetPhysicalGroupingPrefixCount()).
		Bool(p.permuted).
		Sub(p.indexPlan.structuralKey())
}

// EqualsWithoutChildren compares index plan, record type name, and
// result type.
func (p *RecordQueryAggregateIndexPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryAggregateIndexPlan)
	return ok && p.keyFor(p).Equal(o.keyFor(o))
}

// HashCodeWithoutChildren mixes index plan hash, record type, and
// aggregate function.
func (p *RecordQueryAggregateIndexPlan) HashCodeWithoutChildren() uint64 {
	if hash, ok := p.cachedStructuralHash(p); ok {
		return hash
	}
	hash := p.keyFor(p).Hash("aggregateindexplan|")
	p.storeStructuralHash(p, hash)
	return hash
}

// Explain renders AggregateIndex(function, indexName, [groupCols], recordType).
func (p *RecordQueryAggregateIndexPlan) Explain() string {
	suffix := ""
	if p.IsReverse() {
		suffix = ", reverse"
	}
	if len(p.groupCols) > 0 {
		return fmt.Sprintf("AggregateIndex(%s, %s, %v, %s%s)",
			p.aggregateFunction, p.indexPlan.GetIndexName(), p.groupCols, p.recordTypeName, suffix)
	}
	return fmt.Sprintf("AggregateIndex(%s, %s, %s%s)",
		p.aggregateFunction, p.indexPlan.GetIndexName(), p.recordTypeName, suffix)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryAggregateIndexPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryAggregateIndexPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryAggregateIndexPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns this plan unchanged — it has no quantifiers to
// replace while children are raw pointers (RFC-183 P5 step 1).
func (p *RecordQueryAggregateIndexPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryAggregateIndexPlan", len(qs), 0); err != nil {
		return nil, err
	}
	return p, nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryAggregateIndexPlan) GetRecordQueryPlan() RecordQueryPlan { return p }

// resultFieldNames lists a record type's field names; nil for a non-record.
func resultFieldNames(t values.Type) []string {
	record, ok := t.(*values.RecordType)
	if !ok || record == nil {
		return nil
	}
	names := make([]string, len(record.Fields))
	for i, f := range record.Fields {
		names[i] = f.Name
	}
	return names
}

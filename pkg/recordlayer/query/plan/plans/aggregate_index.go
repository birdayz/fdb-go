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
//     RFC-238 §7f carries the same two reachability axes. The namespace one is
//     FORBIDDEN by §7c's committed design, which translates on the QUERY side
//     precisely so the candidate side does not move; it does not arm, and the
//     reference is not licence to move it. The EMPTY association needs no
//     namespace change at all: RecordTypesForIndex returns nothing for an index
//     that is neither universal nor associated, the aggregate rule then leaves
//     this field empty, and GetRecordType("") misses. Every obvious route to
//     that state is gated by the metadata builder; what survives is
//     OVERWRITE-AFTER-REGISTRATION -- SetRecords called after ANY
//     type-associating registration (AddIndex; the ONE-name AddMultiTypeIndex,
//     which returns AddIndex; or AddMultiTypeIndex over two or more names),
//     over a descriptor that STILL DECLARES the type: the setter replaces the
//     RecordType and so drops its index slices while the flat registry keeps
//     the entry. A nil-or-empty-name AddMultiTypeIndex delegates instead to
//     AddUniversalIndex and is NOT affected -- that registry hangs off the
//     builder, not off any RecordType. All four readings are pinned by
//     TestOverwriteAfterRegistrationOrphansTheIndexAssociation in
//     pkg/recordlayer, and metadata.go states the same route at the setter that
//     causes it. It matters HERE because this field carries whichever namespace
//     the plan was built in (RFC-238 §7c), so a plan built with a SQL spelling
//     against metadata keyed by the stored one misses and degrades exactly that
//     way.
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
	groupCols         []string
	aggColumn         string
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
	// resultValue is the stable per-instance QuantifiedObjectValue standing for
	// the rows this leaf emits — minted once at construction, returned by
	// GetResultValue, EXCLUDED from Equals/Hash (its correlation id is unique per
	// instance). A bare leaf that stands as its own Cascades expression must
	// present a consistent row identity across repeated interrogations, the role
	// physicalAggregateIndexWrapper's fresh-per-call GetResultValue could not
	// (RFC-184 W2). nil for struct-literal test plans that bypass the constructor —
	// GetResultValue falls back to PlanExprBase's fresh QOV there.
	resultValue values.Value

	// liveGroupsOnly makes the scan drop entries whose stored aggregate is zero
	// (RFC-209 §5.3(a)). It is set ONLY for a GROUPED COUNT(*) index, where the
	// stored value is the group's row count and a zero can therefore only be the
	// residue of a group emptied by DELETE or by an UPDATE that moved its last
	// row away: an atomic ADD decrements the accumulator to zero and never
	// removes the key. A live group's COUNT(*) is never zero, so the drop is
	// exact rather than a heuristic.
	//
	// It must NOT be set for SUM or COUNT(col), where zero is a legitimate
	// answer for a live group (values cancelling, or every value NULL), nor for
	// the ungrouped spelling, whose single group exists whether or not the table
	// has rows and whose empty scan legitimately coalesces to 0.
	//
	// The flag is rendered by Explain because it changes which rows the scan
	// emits: a plan property that alters the answer must be visible in the plan.
	liveGroupsOnly bool
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

// WithLiveGroupsOnly marks this scan as dropping zero-valued entries — see the
// liveGroupsOnly field. Only a grouped COUNT(*) index may carry it.
// It COPIES, like every other WithXxx on a plan. Mutating in place would be
// mutating plan IDENTITY: liveGroupsOnly is folded into structuralKey precisely
// because a scan that drops vacated groups is a different plan from one that does
// not (see structuralKey below). An in-place write therefore changes the identity of
// an object the memo may already hold, and the memo would keep serving it under its
// former key.
//
// That was latent rather than live only because every caller happens to invoke a
// COPYING builder first (WithGroupColumns), so the in-place write landed on a fresh
// copy. Reordering one chain would have armed it.
func (p *RecordQueryAggregateIndexPlan) WithLiveGroupsOnly(v bool) *RecordQueryAggregateIndexPlan {
	cp := *p
	cp.liveGroupsOnly = v
	return &cp
}

// IsLiveGroupsOnly reports whether the scan drops zero-valued entries.
func (p *RecordQueryAggregateIndexPlan) IsLiveGroupsOnly() bool { return p.liveGroupsOnly }

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

// CanonicalAggColumnName returns the canonical column name the executor's
// aggregateIndexCursor writes the aggregate value under: "FUNC(*)" for an
// empty aggColumn (e.g. COUNT(*)), else "FUNC(col)". Single source of that
// name so the cursor and planColumnNamesWithMD stay byte-identical.
func (p *RecordQueryAggregateIndexPlan) CanonicalAggColumnName() string {
	if p.aggColumn == "" {
		return p.aggregateFunction + "(*)"
	}
	return p.aggregateFunction + "(" + p.aggColumn + ")"
}

// OutputColumnNames returns the column names this plan's rows are keyed by —
// the grouping columns (verbatim, as the cursor writes them) followed by the
// canonical aggregate name. A bare aggregate-index plan is always UNALIASED
// (an aliased SELECT tops with a Project that owns the rename), so there is no
// alias to carry; these are exactly the keys aggregateIndexCursor writes. Used
// by planColumnNamesWithMD so a UNION position-remap can normalize a grouped
// aggregate-index branch (RFC-081).
func (p *RecordQueryAggregateIndexPlan) OutputColumnNames() []string {
	names := make([]string, 0, len(p.groupCols)+1)
	names = append(names, p.groupCols...)
	return append(names, p.CanonicalAggColumnName())
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
	// liveGroupsOnly is folded in because it changes which rows the scan emits
	// (RFC-209 §5.3(a)). Two otherwise-identical scans that differ in it are
	// different plans; leaving it out would let the memo intern the filtering
	// scan and the unfiltered one into a single expression and serve whichever
	// arrived first.
	return newStructuralKey().
		Str(p.recordTypeName).
		Str(p.aggregateFunction).
		Int(p.GetPhysicalGroupingPrefixCount()).
		Bool(p.liveGroupsOnly).
		Sub(p.indexPlan.structuralKey())
}

// EqualsWithoutChildren compares index plan, record type name, and
// result type.
func (p *RecordQueryAggregateIndexPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryAggregateIndexPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren mixes index plan hash, record type, and
// aggregate function.
func (p *RecordQueryAggregateIndexPlan) HashCodeWithoutChildren() uint64 {
	if hash, ok := p.cachedStructuralHash(p); ok {
		return hash
	}
	hash := p.structuralKey().Hash("aggregateindexplan|")
	p.storeStructuralHash(p, hash)
	return hash
}

// Explain renders AggregateIndex(function, indexName, [groupCols], recordType),
// with a trailing ", live_groups_only" when the scan drops zero-valued entries
// (RFC-209 §5.3(a)) — a property that changes the answer, so it is not allowed
// to be invisible in the plan.
func (p *RecordQueryAggregateIndexPlan) Explain() string {
	suffix := ""
	if p.liveGroupsOnly {
		suffix = ", live_groups_only"
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

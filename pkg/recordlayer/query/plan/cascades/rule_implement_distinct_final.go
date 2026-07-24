package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// Go extension: Java's fdb-relational 4.11.1.0 rejects SELECT DISTINCT for most
// query shapes; Go supports it broadly via this rule and the hash-distinct executor.
//
// ImplementDistinctFinalRule is the PLANNING-phase rule for
// LogicalDistinctExpression. Ports Java's ImplementDistinctRule
// (ImplementationCascadesRule<LogicalDistinctExpression>).
//
// For each physical FinalMember, the rule checks two sources of
// distinctness information:
//
//  1. Physical-level DistinctRecordsProperty — per-member, matching
//     Java's ImplementDistinctRule 1:1. A scan on a unique index, an
//     identity-mapping MapPlan, etc. propagate distinctness through
//     the property system.
//  2. Logical-level PK/unique-index column coverage — Go extension,
//     fallback. If the projected columns cover a primary key or unique
//     index, ALL physical plans are guaranteed distinct (equivalent to
//     Java's "strictlySorted" path where all partition members get
//     elided).
//
// When either check passes, the Distinct operator is elided and the
// inner plan is yielded directly. Otherwise, the inner is wrapped
// with RecordQueryDistinctPlan.
//
// This rule subsumes the former DistinctOnUniqueElimRule (which ran
// during EXPLORE). Moving the elimination check to PLANNING matches
// Java's architecture: ImplementDistinctRule is an
// ImplementationCascadesRule, not an exploration rule.
type ImplementDistinctFinalRule struct {
	matcher matching.BindingMatcher
}

func NewImplementDistinctFinalRule() *ImplementDistinctFinalRule {
	return &ImplementDistinctFinalRule{
		matcher: NewExpressionMatcher[*expressions.LogicalDistinctExpression]("logical_distinct_final"),
	}
}

func (r *ImplementDistinctFinalRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementDistinctFinalRule) OnMatch(call *ImplementationRuleCall) {
	d := call.Bindings.Get(r.matcher).(*expressions.LogicalDistinctExpression)
	qs := d.GetQuantifiers()
	if len(qs) == 0 {
		return
	}
	innerRef := qs[0].GetRangesOver()
	if innerRef == nil {
		return
	}

	// Check if projected columns cover a unique key (PK or unique index).
	// If so, ALL plans are distinct regardless of their physical
	// properties. This must check the Projection expression specifically,
	// not just innerRef.Get() which might return a Filter or other
	// expression merged into the same Reference during REWRITING.
	pkDistinct := false
	if call.Context != nil {
		for _, m := range innerRef.Members() {
			if proj, ok := m.(*expressions.LogicalProjectionExpression); ok {
				pkDistinct = distinctEliminatedByUniqueKey(proj, call.Context)
				break
			}
		}
	}

	// Java-aligned: use PlanPartitions filtered by StoredRecord. Each
	// partition is evaluated independently — if its plans are already
	// distinct (PK-level), the Distinct is absorbed; otherwise wrapped.
	// Mirrors Java's ImplementDistinctRule which uses
	// filterPlanPartitions(StoredRecordProperty.storedRecord()).
	partitions := ToPlanPartitions(innerRef)

	handled := false
	for _, partition := range partitions {
		if !partition.GetPartitionPropertiesMap().GetBool(properties.PropStoredRecord) {
			continue
		}
		handled = true

		if pkDistinct || partition.IsDistinct() {
			for _, expr := range partition.GetExpressions() {
				call.YieldFinalExpression(expr)
			}
		} else {
			rolled := RollUpPlanPartitions([]*PlanPartition{partition})
			for _, rp := range rolled {
				for _, expr := range rp.GetExpressions() {
					if w := newPhysicalDistinctFor(call, expr); w != nil {
						call.YieldFinalExpression(w)
					}
				}
			}
		}
	}

	// Logical-level fallback: if projected columns cover a unique key,
	// ALL physical plans are guaranteed distinct. This fires when no
	// StoredRecord partitions were found (e.g. unit tests without full
	// PLANNING, or when the fallback partitioner doesn't set properties).
	if !handled {
		allDistinct := false
		if call.Context != nil {
			innerExpr := innerRef.Get()
			allDistinct = distinctEliminatedByUniqueKey(innerExpr, call.Context)
		}

		for _, m := range innerRef.AllMembers() {
			if _, ok := m.(physicalPlanExpression); !ok {
				continue
			}
			if allDistinct {
				call.YieldFinalExpression(m)
			} else if w := newPhysicalDistinctFor(call, m); w != nil {
				call.YieldFinalExpression(w)
			}
		}
	}
}

// distinctEliminatedByUniqueKey checks whether the projected column set
// covers all columns of a primary key or unique index, making the
// DISTINCT operator redundant. Ported from DistinctOnUniqueElimRule.
func distinctEliminatedByUniqueKey(
	innerExpr expressions.RelationalExpression,
	ctx PlanContext,
) bool {
	projectedCols := collectProjectedFieldNames(innerExpr)

	recordTypes := findRecordTypes(innerExpr)
	if len(recordTypes) == 0 {
		return false
	}

	for _, rt := range recordTypes {
		pkCols := ctx.GetPrimaryKeyColumns(rt)
		if len(pkCols) > 0 && uniqueKeysCovered(pkCols, projectedCols) {
			return true
		}
	}

	for _, cand := range ctx.GetMatchCandidates() {
		if !cand.IsUnique() {
			continue
		}
		cols, plainFields := candidatePlainFieldColumnsForShortcut(cand)
		if !plainFields {
			continue
		}
		// UNIQUE on a fan-out index constrains index entries, not one value
		// per base record. Empty repeated fields produce no entry at all, so
		// the index key cannot prove the projected base rows are distinct.
		if !candidatePreservesBaseRecordCardinality(cand) {
			continue
		}
		if len(cols) > 0 && uniqueKeysCovered(cols, projectedCols) {
			return true
		}
	}

	return false
}

// collectProjectedFieldNames returns the set of columns each projected as a
// BARE, top-level field reference. Only a projected value that IS itself a
// FieldValue counts: such a projection is the column value verbatim, so it is
// injective in that column, and the DISTINCT may be elided when the bare
// columns collectively cover a unique key. A projected value that references a
// key column only from INSIDE an expression — id/3, f(id), a+b — is NOT
// injective over that column (f(pk) maps many pks to one output), so it
// contributes NOTHING: crediting its buried FieldValue would wrongly elide
// DISTINCT over a many-to-one projection and emit duplicates.
//
// If the inner expression is not a projection, returns nil to indicate "all
// columns available" (full row).
func collectProjectedFieldNames(expr expressions.RelationalExpression) map[string]struct{} {
	proj, ok := expr.(*expressions.LogicalProjectionExpression)
	if !ok {
		return nil
	}

	cols := make(map[string]struct{})
	for _, v := range proj.GetProjectedValues() {
		// TOP-LEVEL type assertion only — a FieldValue nested inside an
		// ArithmeticValue/function is deliberately not unwrapped here.
		if fv, ok := v.(*values.FieldValue); ok {
			cols[fv.Field] = struct{}{}
		}
	}
	return cols
}

// findRecordTypes walks down through transparent LOGICAL operators
// (projection, filter, sort, distinct, unique, type-filter) to find a
// FullUnorderedScanExpression and returns its record types.
//
// Intentionally handles only logical expressions. During PLANNING,
// innerRef.Get() returns physical wrappers which this function does
// not match, so distinctEliminatedByUniqueKey returns false and the
// per-FinalMember PropDistinctRecords property check (the primary
// path) handles distinctness. This function is a fallback for tests
// and scenarios where the inner Reference contains only logical
// expressions (no PLANNING phase ran).
func findRecordTypes(expr expressions.RelationalExpression) []string {
	switch e := expr.(type) {
	case *expressions.FullUnorderedScanExpression:
		return e.GetRecordTypes()
	case *expressions.LogicalProjectionExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalFilterExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalSortExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalDistinctExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalUniqueExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalTypeFilterExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	}
	return nil
}

func findRecordTypesViaQuantifier(q expressions.Quantifier) []string {
	ref := q.GetRangesOver()
	if ref == nil {
		return nil
	}
	return findRecordTypes(ref.Get())
}

// uniqueKeysCovered reports whether every column in uniqueKeyCols
// appears in projectedCols. If projectedCols is nil, all columns are
// considered available (no projection = full row).
func uniqueKeysCovered(uniqueKeyCols []string, projectedCols map[string]struct{}) bool {
	if projectedCols == nil {
		return true
	}
	for _, col := range uniqueKeyCols {
		found := false
		for pc := range projectedCols {
			if eqFold(col, pc) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// newPhysicalDistinctFor builds the physical distinct for a physical inner
// member, selecting the resume-clean STREAMING executor (distinctStreamCursor)
// when the member's ordering already makes the whole-row dedup key adjacent —
// equal rows contiguous — and the fresh-per-page hash-set otherwise. Streaming
// is sound ONLY when the ordering covers ALL output columns (the full DISTINCT
// dedup key), so a non-adjacent duplicate is never dropped and an adjacent one
// never wrongly kept; anything less conservatively keeps the hash-set (correct
// within a page). This is the ordering-aware fix for the cross-page re-admission
// of TODO.md C5 — the same adjacency predicate streaming aggregation uses for
// its grouping keys. Returns nil for a non-physical member.
//
// The distinct is its own cascades expression carrying ONE child edge (RFC-184
// W2, no physicalDistinctWrapper) — but WHICH edge is CONDITIONAL on the
// streaming mode, a constraint-preserving disentangle:
//
//   - STREAMING → FREEZE the concrete inner plan in a DETACHED single-member
//     final reference (MemoizeFinalExpression). The streaming executor is sound
//     only over the exact ordering this flag was computed for; a live edge that
//     floated to a cost-tied but differently-ordered sibling would run the
//     streaming dedup over unordered input and LEAK a duplicate. The frozen edge
//     makes planFromQuantifier resolve that exact member, never a group winner.
//   - PLAIN (hash) → carry the LIVE edge the wrapper's innerQuant presented
//     (ForEachQuantifier over InitialOf(member)). A hash distinct dedups over
//     ANY inner, so freezing buys nothing and instead strands a pre-push
//     snapshot once a push rule (push_distinct_below_filter / _through_fetch)
//     re-explores the leg — the parent would then cost an unreachable edge.
//     The live exploratory edge resolves the member's plan (== the concrete
//     inner) exactly as the wrapper did, byte-identically.
//
// A follow-up will REQUEST the dedup-key ordering (inserting an InMemorySort
// when no index provides it) so the unordered `SELECT DISTINCT col` — the
// common shape — also streams; that step must not disturb the DISTINCT +
// ORDER-BY-on-a-non-projected-column dedup-by-projected-only semantics, so it
// is deliberately separated from this ordering-detection step.
func newPhysicalDistinctFor(call *ImplementationRuleCall, member expressions.RelationalExpression) expressions.RelationalExpression {
	ph, ok := member.(physicalPlanExpression)
	if !ok {
		return nil
	}
	concreteInner := ph.GetRecordQueryPlan()
	streaming := distinctStreamingEligible(member, concreteInner)
	if streaming {
		// Freeze the ordering-critical inner: a detached single-member final
		// reference over the concrete plan whose ordering this flag was measured
		// against, so it can never float to a differently-ordered sibling.
		innerQ := expressions.ForEachQuantifier(call.MemoizeFinalExpression(concreteInner))
		return plans.NewRecordQueryDistinctPlanFromQuantifier(innerQ, true)
	}
	// Plain hash distinct: carry the live exploratory edge (what the wrapper's
	// innerQuant presented) so a later push-rule canonicalization stays reachable.
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(member))
	return plans.NewRecordQueryDistinctPlanFromQuantifier(innerQ, false)
}

// distinctStreamingEligible reports whether a distinct over innerPlan — whose
// physical realization is `member` — may use the resume-clean streaming
// executor: the member's ordering must make the whole-row dedup key adjacent
// (equal rows contiguous), the same adjacency predicate streaming aggregation
// uses for its grouping keys. This is the SOLE gate for
// RecordQueryDistinctPlan.Streaming and MUST be re-evaluated at every site that
// (re)builds a distinct over a different inner — the push-through-filter/fetch
// rules included — so a rebuild never silently downgrades a streaming distinct
// to the memory-heavy hash-set, nor promotes an unordered inner to
// streaming. (A push preserves the inner ordering but changes WHICH inner the
// distinct sits over, so the decision genuinely has to be recomputed, not
// copied.)
//
// orderingSatisfiesGroupingKeys matches dedup columns to ordering keys by BARE
// field name (case-insensitive) — sound for a single-record-source inner where
// every column name is distinct. Across a JOIN two legs can share a leaf name,
// so a coincidental name+position alignment could false-positive. That is NOT a
// row-dropping hazard: the executor still dedups by the full packed row
// (distinctKey), so a wrongly-streamed non-adjacent input at worst LEAKS a
// duplicate — the same failure class as the hash-set it replaces, never a lost
// unique row. (Streaming a join-distinct also requires the join to emit rows
// ordered by the whole dedup key, itself rare.)
func distinctStreamingEligible(member expressions.RelationalExpression, innerPlan plans.RecordQueryPlan) bool {
	dedupCols := distinctKeyColumns(innerPlan)
	return len(dedupCols) > 0 && orderingSatisfiesGroupingKeys(properties.EstimateOrdering(member), dedupCols)
}

// distinctKeyColumns returns the inner plan's output columns as Values — the
// whole-row DISTINCT dedup key (distinctKey packs exactly these positional
// slots). A projection carries its output columns as projected Values directly
// (its GetResultType is always UnknownType); a BARE-column projection yields
// FieldValues in the same representation the inner ordering's keys use, so
// orderingSatisfiesGroupingKeys can prove adjacency. A COMPUTED projection
// (g/2, f(g)) yields non-FieldValue projected values that won't match — the
// distinct is then conservatively left on the hash-set. A non-projection inner
// (e.g. SELECT DISTINCT *) exposes its columns via a RecordType schema.
func distinctKeyColumns(inner plans.RecordQueryPlan) []values.Value {
	if proj, ok := inner.(*plans.RecordQueryProjectionPlan); ok {
		return proj.GetProjections()
	}
	if rt, ok := inner.GetResultType().(*values.RecordType); ok && len(rt.Fields) > 0 {
		cols := make([]values.Value, len(rt.Fields))
		for i, f := range rt.Fields {
			cols[i] = values.NewFieldValueWithResolvedOrdinal(f.Name, i, f.FieldType)
		}
		return cols
	}
	return nil
}

var _ ImplementationRule = (*ImplementDistinctFinalRule)(nil)

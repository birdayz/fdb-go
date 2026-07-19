package cascades

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// scanComparisonCorrelations returns the union of outer correlations referenced
// by the comparands of a set of scan ComparisonRanges — the correlations a
// physical (index or primary) scan probe carries. A correlated probe
// (`col = QOV(outer).x`) reports the outer alias; a literal/parameter range
// reports nothing. Used by the physical scan wrappers'
// GetCorrelatedToWithoutChildren (RFC-150 Phase-2b D.2): the data-access path
// can SARG a join predicate into a bare correlated PHYSICAL scan (no residual
// filter to carry the correlation), so unless the scan itself reports it, the
// physical probe looks uncorrelated and B1 join-leg detection / winner-stamping
// would mis-treat it. Java's RecordQueryScanPlan derives correlatedTo from its
// ScanComparisons the same way.
func scanComparisonCorrelations(comps []*predicates.ComparisonRange) map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	collect := func(c *predicates.Comparison) {
		if c == nil || c.Operand == nil {
			return
		}
		for a := range values.GetCorrelatedToOfValue(c.Operand) {
			out[a] = struct{}{}
		}
		// A query-parameter (ConstantObjectValue) comparand is an execution constant
		// bound at run time, NOT a row correlation — its constant-pool alias appears
		// in GetCorrelatedToOfValue but must not make a `Scan(T,[k=?param])` look
		// join-correlated to planning (B1 leg detection) or to the
		// probe-fed-residual guard (compensationProbeCorrelations). Subtract any such
		// aliases — the value-level twin of deletePredicateConstantObjectAliases.
		values.WalkValue(c.Operand, func(node values.Value) bool {
			if cov, ok := node.(*values.ConstantObjectValue); ok {
				delete(out, cov.Alias)
			}
			return true
		})
	}
	for _, cr := range comps {
		if cr == nil || cr.IsEmpty() {
			continue
		}
		if cr.IsEquality() {
			collect(cr.GetEqualityComparison())
		} else if cr.IsInequality() {
			for _, c := range cr.GetInequalityComparisons() {
				collect(c)
			}
		}
	}
	return out
}

// valueCorrelationsNoParams returns the outer correlations a value references with
// query-parameter (ConstantObjectValue) aliases subtracted — the value-tree twin of the
// param exclusion scanComparisonCorrelations applies to a SARG comparand.
func valueCorrelationsNoParams(v values.Value) map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	if v == nil {
		return out
	}
	for a := range values.GetCorrelatedToOfValue(v) {
		out[a] = struct{}{}
	}
	values.WalkValue(v, func(node values.Value) bool {
		if cov, ok := node.(*values.ConstantObjectValue); ok {
			delete(out, cov.Alias)
		}
		return true
	})
	return out
}

// dataAccessExprCorrelations collects the COMPLETE set of outer correlations a
// data-access subplan reports — SARG comparands (scan/index), residual filter
// predicates, and map result values — query-parameter aliases subtracted. Used by the
// plan-backed leaf expression scanPlanExpression so a SARGed PK-scan probe
// (`pk = QOV(outer).fk`) and an RFC-153 buried-merge-rebased FlatMap inner report the
// correlations their executable plan actually carries, instead of nil/stale (an
// under-reported correlation lets join-leg / winner / root bookkeeping treat a
// correlated probe as self-contained — a latent planning hazard, the same
// incomplete-coverage family as the fail-open verifier). Complete for the data-access
// node shapes scanPlanExpression wraps (a single PK scan; a fully-rebased recognized
// inner — the verifier declines any inner with an unrecognized node, so a rebased inner
// reaching this point contains only scan/index/filter/map/pass-through nodes).
func dataAccessExprCorrelations(p plans.RecordQueryPlan) map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	if p == nil {
		return out
	}
	plans.Walk(p, func(n plans.RecordQueryPlan) bool {
		switch sp := n.(type) {
		case *plans.RecordQueryScanPlan:
			for a := range scanComparisonCorrelations(sp.GetScanComparisons()) {
				out[a] = struct{}{}
			}
		case *plans.RecordQueryIndexPlan:
			for a := range scanComparisonCorrelations(sp.GetScanComparisons()) {
				out[a] = struct{}{}
			}
		case *plans.RecordQueryPredicatesFilterPlan:
			for _, pr := range sp.GetPredicates() {
				c := map[values.CorrelationIdentifier]struct{}{}
				for a := range predicates.GetCorrelatedToOfPredicate(pr) {
					c[a] = struct{}{}
				}
				deletePredicateConstantObjectAliases(pr, c)
				for a := range c {
					out[a] = struct{}{}
				}
			}
		case *plans.RecordQueryFilterPlan:
			for _, pr := range sp.GetPredicates() {
				c := map[values.CorrelationIdentifier]struct{}{}
				for a := range predicates.GetCorrelatedToOfPredicate(pr) {
					c[a] = struct{}{}
				}
				deletePredicateConstantObjectAliases(pr, c)
				for a := range c {
					out[a] = struct{}{}
				}
			}
		case *plans.RecordQueryMapPlan:
			for a := range valueCorrelationsNoParams(sp.GetResultValue()) {
				out[a] = struct{}{}
			}
		}
		return true
	})
	return out
}

// physicalPlanExpression is implemented by all physical-plan wrapper
// types. Lets implement rules discover physical plans in a Reference
// with a single interface assertion instead of per-type switches.
type physicalPlanExpression interface {
	expressions.RelationalExpression
	GetRecordQueryPlan() plans.RecordQueryPlan
}

// IsPhysicalIndexScan reports whether the given RelationalExpression is
// a physicalIndexScanWrapper. Exported so external test packages can
// identify index scan plans without depending on the unexported type.
func IsPhysicalIndexScan(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalIndexScanWrapper)
	return ok
}

// IsPhysicalIntersection reports whether the given RelationalExpression
// is a physicalIntersectionWrapper.
func IsPhysicalIntersection(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalIntersectionWrapper)
	return ok
}

// IsPhysicalMultiIntersection reports whether the given
// RelationalExpression is a physicalMultiIntersectionWrapper.
func IsPhysicalMultiIntersection(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalMultiIntersectionWrapper)
	return ok
}

// GetPhysicalMultiIntersectionPlan returns the underlying
// RecordQueryMultiIntersectionOnValuesPlan if expr is a
// physicalMultiIntersectionWrapper, nil otherwise.
func GetPhysicalMultiIntersectionPlan(expr expressions.RelationalExpression) *plans.RecordQueryMultiIntersectionOnValuesPlan {
	w, ok := expr.(*physicalMultiIntersectionWrapper)
	if !ok {
		return nil
	}
	return w.plan
}

// IsPhysicalFilter reports whether the given RelationalExpression is
// a physical filter wrapper (either legacy or predicates-based).
func IsPhysicalFilter(expr expressions.RelationalExpression) bool {
	switch expr.(type) {
	case *physicalFilterWrapper, *physicalPredicatesFilterWrapper:
		return true
	}
	return false
}

// IsPhysicalInsert reports whether the given RelationalExpression is
// a physicalInsertWrapper.
func IsPhysicalInsert(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalInsertWrapper)
	return ok
}

// IsPhysicalDelete reports whether the given RelationalExpression is
// a physicalDeleteWrapper.
func IsPhysicalDelete(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalDeleteWrapper)
	return ok
}

// IsPhysicalUpdate reports whether the given RelationalExpression is
// a physicalUpdateWrapper.
func IsPhysicalUpdate(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalUpdateWrapper)
	return ok
}

// IsPhysicalPredicatesFilter reports whether the given expression is
// a physicalPredicatesFilterWrapper.
func IsPhysicalPredicatesFilter(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalPredicatesFilterWrapper)
	return ok
}

// IsPhysicalMap reports whether the given expression is a physicalMapWrapper.
func IsPhysicalMap(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalMapWrapper)
	return ok
}

// IsPhysicalInJoin reports whether the given expression is
// a physicalInJoinWrapper.
func IsPhysicalInJoin(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalInJoinWrapper)
	return ok
}

// ExplainPhysicalPlan returns the Explain() string for a physical-plan
// expression, or empty string if the expression is not a physical plan.
func ExplainPhysicalPlan(expr expressions.RelationalExpression) string {
	ph, ok := expr.(physicalPlanExpression)
	if !ok {
		return ""
	}
	p := ph.GetRecordQueryPlan()
	if p == nil {
		return ""
	}
	return p.Explain()
}

// extractChildPlanFromQuantifier gets the RecordQueryPlan from a
// quantifier's Reference. Used by WithChildren implementations to
// rebuild the plan with the freshly-extracted child plan during plan
// extraction. Returns nil if the quantifier has no physical plan.
func extractChildPlanFromQuantifier(q expressions.Quantifier) plans.RecordQueryPlan {
	ref := q.GetRangesOver()
	if ref == nil {
		return nil
	}
	return findPhysicalPlan(ref)
}

// findPhysicalPlan returns a physical member's underlying RecordQueryPlan, or
// nil if no physical plan has been yielded into ref yet.
//
// FINAL members are searched first. Java enumerates FINAL expressions only when
// it looks for a plan (RecordQueryPlanMatchers.java:115) — a plan is by
// definition a final expression there. Go's AllMembers() concatenates
// exploratory members BEFORE final ones (Reference.AllMembers), so a bare
// first-match scan over it inspects the exploratory set first and can return a
// promoted-but-dominated expression instead of the group's plan.
// FinalizeExpressionsRule promotes the SAME pointer into both sets, so an
// expression really can sit in each.
//
// The exploratory fallback is kept deliberately: rules call this DURING
// planning, before a group has been finalized, and returning nil there would
// silently decline a rule that has a perfectly good child to hand.
func findPhysicalPlan(ref *expressions.Reference) plans.RecordQueryPlan {
	if expr := findPhysicalExpr(ref); expr != nil {
		if ph, ok := expr.(physicalPlanExpression); ok {
			return ph.GetRecordQueryPlan()
		}
	}
	return nil
}

// findBestPhysicalPlan returns the cheapest physical member's plan — the cost
// winner — for a push-through WithChildren whose inner must relink to the
// winner rather than to whichever physical member was yielded first.
// ref.AllMembers() interleaves exploratory and final members in yield order, so
// "first physical" can be a dominated alternative; when ordering constraints add
// ordered variants the first-yielded member flips and the enforcer relinks onto
// the wrong (worse) join order (RFC-076 TestFDB_JoinSelPred_Repro). Falls back
// to findPhysicalPlan when no member ranks.
func findBestPhysicalPlan(ref *expressions.Reference) plans.RecordQueryPlan {
	if ref == nil {
		return nil
	}
	if best := findBestValidPhysicalExpr(ref, nil); best != nil {
		if ph, ok := best.(physicalPlanExpression); ok {
			return ph.GetRecordQueryPlan()
		}
	}
	return findPhysicalPlan(ref)
}

// findPhysicalExpr returns a physical-plan expression from ref, FINAL members
// first. Used by implement rules to obtain the existing wrapper (already
// memoized in the inner Reference by a prior implement-rule fire) without
// re-wrapping from scratch.
//
// See findPhysicalPlan for why the final set is searched first and why the
// exploratory fallback stays.
//
// The exploratory fallback still matters, but for a different reason than it
// used to. MemoizeFinalExpression now genuinely lands plans in the FINAL set
// (FinalOfAtStage), so the finals-first loop below is live at the
// push-through sites rather than inert. What the fallback covers is rules
// calling this MID-PLANNING, before a group has been finalized.
//
// A finals-ONLY tightening here is still not safe, and the reason is
// measured: at these call sites 3821 references have ZERO final members
// (against 2744 with exactly one), so refusing the exploratory set would make
// a large fraction of rules silently decline. That number is also why P5's
// terminal form is not reachable yet — see below.
//
// P5 BLOCKER, quantified. Java dereferences a quantifier straight to its plan:
// Quantifier.Physical.getRangesOverPlan() is
// Iterables.getOnlyElement(getRangesOver().getFinalExpressions()) — it REQUIRES
// exactly one final expression. Go does not have that property: 1186
// references here hold multiple finals (max 52), and 1125 hold multiple
// PHYSICAL finals, so getOnlyElement would throw on every one. Making plans
// hold quantifiers is therefore gated on universal prune-to-one-final-member,
// which unified_tasks.go's stage-boundary arm records was ATTEMPTED AND
// REVERTED: it lost canonical alternatives Go's PLANNING cannot re-derive
// (RFC-153 buried-leg, cross-join-EXISTS). The root gate is Java per-phase
// rule-set parity, tracked in DIVERGENCES.md.
//
// DO NOT make this cost-ranked. Picking the "cheapest" member here looks like
// an obvious improvement — findBestPhysicalPlan does exactly that, and it is
// wired to one site against this function's twenty — but it is wrong, and
// measurably so: ranking with PlanningCostModelLess ignores the REQUESTED
// ORDERING, so a rule asking for a child gets whichever member is cheapest
// rather than one that satisfies the ordering its parent needs. Tried, and it
// turned `SELECT a, b FROM ab WHERE a = 1 ORDER BY a DESC` into an ASCENDING
// result (pinned by yamsql order_by_elimination#36) and moved 79 plan shapes.
//
// Cost is the memo's job, not a rule's. A rule wants A VALID CHILD; which
// alternative wins is decided by OptimizeGroup under the ordering constraints,
// and extraction then reads that winner through the ordering-aware winner
// lookup. A cost comparison at rule time is a second, ordering-blind optimizer
// running outside the cost framework.
func findPhysicalExpr(ref *expressions.Reference) expressions.RelationalExpression {
	if ref == nil {
		return nil
	}
	for _, m := range ref.FinalMembers() {
		if _, ok := m.(physicalPlanExpression); ok {
			return m
		}
	}
	for _, m := range ref.Members() {
		if _, ok := m.(physicalPlanExpression); ok {
			return m
		}
	}
	return nil
}

// bakedInnerPlan returns the concrete RecordQueryPlan that expr carries, for
// use as a parent's child AT RULE TIME.
//
// Java constructs a parent only from an already-memoized child: in
// PushFilterThroughFetchRule.java:197-225 the
// `Quantifier.physical(call.memoizePlan(innerPlan))` is evaluated as a
// constructor ARGUMENT, so no window exists in which a parent lacks its
// child. Go's rules must do the same — pass the child plan, never nil.
//
// Returns nil when expr carries no plan, or carries a structurally
// incomplete one. A rule that gets nil here declines to fire rather than
// yielding a plan with a hole in it; the alternative is the shell that
// extraction then has to repair.
func bakedInnerPlan(expr expressions.RelationalExpression) plans.RecordQueryPlan {
	pe, ok := expr.(physicalPlanExpression)
	if !ok {
		return nil
	}
	p := pe.GetRecordQueryPlan()
	// Structurally incomplete = a non-leaf carrying no children, per the
	// plan-invariant authority (isGenuineLeafPlan).
	if p == nil || (len(p.GetChildren()) == 0 && !isGenuineLeafPlan(p)) {
		return nil
	}
	return p
}

// isLeafReplaceable reports whether a plan is safe to substitute as the
// inner of a projection without altering the output schema or predicate
// semantics. Only leaf-adjacent plans (scans, filters over scans, index
// scans, streaming agg, distinct, etc.) qualify. Compound join plans
// (NLJ, FlatMap, InJoin) encode predicate semantics in their structure
// and must NOT be swapped — extraction already picks the right join plan
// via quantifier traversal.
//
// ORDER-PRESERVING set operations over ONE record type (intersection / union
// of index scans merged on the primary key) are leaf-adjacent in exactly this
// sense: they flow that record type's schema unchanged and carry no join
// structure, so they belong in the list. They were absent only because no
// shape put one under a relinking wrapper until compensated pk-intersections
// started carrying residual filters — a filter over such an intersection then
// kept its eagerly-snapshotted nil-inner plan and extracted
// `PredicatesFilter(<nil>)` (XX000 plan-invariant), and before the
// compensation landed the very same queries silently DROPPED the residual
// (wrong rows).
//
// Three families stay OUT, each for its own reason:
//   - InJoin / InUnion bind a correlation per IN value — join-like structure.
//   - The UNORDERED set ops (UnorderedUnion, UnorderedPrimaryKeyDistinct):
//     findPhysicalPlan picks the first physical member without consulting
//     ordering, so admitting them would let an unordered member satisfy a
//     relink whose consumer needs grouped/ordered input (streaming
//     aggregation). Gate the relink on ordering compatibility before adding
//     them.
//   - MultiIntersectionOnValues intersects on VALUES and computes its own
//     output shape rather than flowing a record type through, so it fails
//     this gate's schema-preservation test; and nothing needs it — no
//     observed shape puts one under a relinking wrapper. (Membership here is
//     independent of definesOutputSchema in cascades_generator.go: that gate
//     governs a column-derivation DESCENT, this one governs plan
//     SUBSTITUTION. StreamingAggregation and AggregateIndex sit in both and
//     belong in both. Do not reason from one list to the other.)
func isLeafReplaceable(p plans.RecordQueryPlan) bool {
	switch p.(type) {
	case *plans.RecordQueryScanPlan,
		*plans.RecordQueryIndexPlan,
		*plans.RecordQueryFilterPlan,
		*plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryFetchFromPartialRecordPlan,
		*plans.RecordQueryInMemorySortPlan,
		*plans.RecordQueryStreamingAggregationPlan,
		*plans.RecordQueryDistinctPlan,
		*plans.RecordQueryLimitPlan,
		*plans.RecordQueryPredicatesFilterPlan,
		*plans.RecordQueryAggregateIndexPlan,
		*plans.RecordQueryIntersectionPlan,
		*plans.RecordQueryUnionPlan,
		*plans.RecordQueryMergeSortUnionPlan:
		return true
	}
	return false
}

// writeHash64 writes a uint64 to the FNV hasher in big-endian
// byte order. Shared by all four wrapper types' HashCodeWithoutChildren
// implementations.
func writeHash64(h hashWriter, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	_, _ = h.Write(b[:])
}

// hashWriter is the minimal io.Writer surface fnv.New64a() returns.
type hashWriter interface {
	Write(p []byte) (n int, err error)
}

// physicalWrapperCostMultiplier is applied to each physical wrapper's
// inherited cost so cost-driven extraction prefers physical plans
// over their logical counterparts. 0.9 = "physical is 10% cheaper
// than logical" — enough to flip ordering on equally-shaped
// alternatives, small enough not to dominate the cost comparison
// with structurally-different alternatives.
const physicalWrapperCostMultiplier = properties.PhysicalWrapperCostMultiplier

// physicalScanWrapper adapts a `*plans.RecordQueryScanPlan` to the
// `expressions.RelationalExpression` interface so implementation rules
// can yield it into the Reference dedup machinery.
//
// The wrapper family exists because Go keeps the plan and expression
// hierarchies separate (RFC-022 design choice), where Java's physical
// plans (RecordQueryPlan) implement RelationalExpression directly —
// the physical_*_wrapper.go files in this package are the bridge, one
// per physical operator family. This one is leaf-like (no Quantifiers,
// no children): the underlying RecordQueryScanPlan is a leaf physical
// plan. Non-leaf wrappers (filter, union, joins, …) expose their inner
// plans as Quantifiers over inner References — see e.g.
// physicalFilterWrapper below and physical_flat_map_wrapper.go.
type physicalScanWrapper struct {
	plan *plans.RecordQueryScanPlan
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalScanWrapper) GetPlan() *plans.RecordQueryScanPlan { return w.plan }

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalScanWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

// GetResultValue returns a fresh QuantifiedObjectValue whose Type is
// the plan's flowed Type. Mirrors FullUnorderedScanExpression's
// shape so callers can interrogate type without unwrapping.
func (w *physicalScanWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

// GetQuantifiers returns the empty list — the wrapped plan is a leaf.
func (w *physicalScanWrapper) GetQuantifiers() []expressions.Quantifier { return nil }

// CanCorrelate is false — leaf can't anchor correlation.
func (w *physicalScanWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is false — leaf has no children.
func (w *physicalScanWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren reports the OUTER correlations the scan's
// comparison comparands reference — for a correlated PK/index probe
// (`pk = QOV(outer).fk`), the outer alias. See scanComparisonCorrelations
// (RFC-150 Phase-2b D.2).
func (w *physicalScanWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	if w.plan == nil {
		return map[values.CorrelationIdentifier]struct{}{}
	}
	return scanComparisonCorrelations(w.plan.GetScanComparisons())
}

// EqualsWithoutChildren compares wrapped plans via plans.Equals on
// the same wrapper concrete type.
func (w *physicalScanWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalScanWrapper)
	if !ok {
		return false
	}
	return plans.Equals(w.plan, o.plan)
}

// HashCodeWithoutChildren mixes the class discriminator with the
// wrapped plan's hash.
func (w *physicalScanWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physcanwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren satisfies WithChildren — scan is a leaf,
// so qs must be empty. Returns the wrapper itself unchanged on
// empty input.
func (w *physicalScanWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 0 {
		return nil, fmt.Errorf("physicalScanWrapper.WithChildren: expected 0 children, got %d", len(qs))
	}
	return w, nil
}

// HintCost matches the LogicalScan equivalent (see properties/cost.go's
// FullUnorderedScanExpression arm) and applies the physical-wrapper
// discount so cost-driven extraction prefers the physical scan over
// the logical one.
// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalScanWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

// HintOrdering: a scan produces rows in PK order when the scan
// carries PK values (from WithPrimaryKey). Otherwise unknown.
// HintOrdering delegates to the plan, which owns its ordering (RFC-183 P5).
func (w *physicalScanWrapper) HintOrdering() properties.Ordering {
	return w.plan.HintOrdering()
}

// HintRichOrdering — see pkScanRichOrdering.
func (w *physicalScanWrapper) HintRichOrdering() *properties.RichOrdering {
	return pkScanRichOrdering(w.plan)
}

// pkScanOrdering derives the directional PK ordering of a primary scan:
// PK columns in order, all ascending (descending under reverse).
// pkScanRichOrdering returns a primary scan's PK ordering with bindings:
// PK positions bound by an equality comparison become FixedBinding
// entries, the rest SortedBinding. A primary scan is a value-index-like
// candidate in Java (PrimaryScanMatchCandidate implements
// ValueIndexLikeMatchCandidate), so its ordering comes from the same
// computeOrderingFromScanComparisons: the equality prefix is
// Binding.fixed, which is compatible with ANY requested direction. That
// is what lets `WHERE a = 1 ORDER BY a DESC` elide the sort over a
// FORWARD eq-bound scan (every row has the same a, so direction on a is
// a no-op) — without the FIXED modeling the prefix reads as directional
// ASC and a DESC request wrongly keeps the sort. Shared by
// physicalScanWrapper and the plan-backed leaf scanPlanExpression (the
// data-access path memoizes a SARGed PK scan as the latter).
func pkScanRichOrdering(plan *plans.RecordQueryScanPlan) *properties.RichOrdering {
	if plan == nil {
		return properties.EmptyOrdering()
	}
	pk := plan.GetPrimaryKeyValues()
	if len(pk) == 0 {
		return properties.EmptyOrdering()
	}
	comps := plan.GetScanComparisons()
	bm := make(map[values.Value][]properties.OrderingBinding, len(pk))
	keys := make([]values.Value, 0, len(pk))
	dir := properties.ProvidedSortOrderAscending
	if plan.IsReverse() {
		dir = properties.ProvidedSortOrderDescending
	}
	for i, key := range pk {
		keys = append(keys, key)
		if i < len(comps) && comps[i].IsEquality() {
			bm[key] = []properties.OrderingBinding{properties.FixedBinding(comps[i])}
		} else {
			bm[key] = []properties.OrderingBinding{properties.SortedBinding(dir)}
		}
	}
	return properties.NewRichOrdering(bm, keys, false)
}

func (w *physicalScanWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalScanWrapper)(nil)

// physicalIndexScanWrapper adapts a `*plans.RecordQueryIndexPlan` to
// the RelationalExpression interface. Same leaf shape as
// physicalScanWrapper — index scans have no children in the Memo.
type physicalIndexScanWrapper struct {
	plan        *plans.RecordQueryIndexPlan
	columnNames []string // index column names for ordering property
	// pkColumnNames is the record type's primary-key column list, used to
	// extend the ordering property past the index key: a value index's
	// entries are (index key, primary key), so the scan's output order
	// covers the trimmed PK suffix too. Mirrors Java's
	// ValueIndexExpansionVisitor.fullKey(index, primaryKey) — the ordering
	// in ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons is
	// derived over the FULL key (index root + trimPrimaryKey'd PK), which is
	// what lets an equality-prefixed scan (status = ?) satisfy ORDER BY pk.
	// Empty for fan-out (createsDuplicates) indexes, where positions past
	// the fan-out are not sort-ordered (Java breaks the sorted-suffix loop
	// at a duplicating key part).
	pkColumnNames []string
	unique        bool
	covering      bool // true when the index provides all needed columns (MergeFetch can eliminate the fetch)
}

func (w *physicalIndexScanWrapper) GetPlan() *plans.RecordQueryIndexPlan      { return w.plan }
func (w *physicalIndexScanWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalIndexScanWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalIndexScanWrapper) GetQuantifiers() []expressions.Quantifier { return nil }
func (w *physicalIndexScanWrapper) CanCorrelate() bool                       { return false }
func (w *physicalIndexScanWrapper) ChildrenAsSet() bool                      { return false }

// GetCorrelatedToWithoutChildren reports the OUTER correlations the index scan's
// comparison comparands reference — the correlated index probe's outer alias.
// See scanComparisonCorrelations (RFC-150 Phase-2b D.2).
func (w *physicalIndexScanWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	if w.plan == nil {
		return map[values.CorrelationIdentifier]struct{}{}
	}
	return scanComparisonCorrelations(w.plan.GetScanComparisons())
}

func (w *physicalIndexScanWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalIndexScanWrapper)
	if !ok {
		return false
	}
	return plans.Equals(w.plan, o.plan)
}

func (w *physicalIndexScanWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physindexscanwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalIndexScanWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 0 {
		return nil, fmt.Errorf("physicalIndexScanWrapper.WithChildren: expected 0 children, got %d", len(qs))
	}
	return w, nil
}

// HintOrdering: an index scan produces rows in index-key order for
// the non-equality-bound suffix columns, extended by the trimmed
// primary-key suffix (index entries are (index key, primary key), so
// the PK columns continue the sort order). E.g. index(a, b, c) with
// a = 1 over PK (id) produces output sorted by (b, c, id). Mirrors the
// full-key ordering of Java's
// ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons.
// HintOrdering delegates to the plan, which owns its ordering (RFC-183 P5).
func (w *physicalIndexScanWrapper) HintOrdering() properties.Ordering {
	return w.plan.HintOrdering()
}

// HintRichOrdering returns the full ordering with bindings: equality-bound
// prefix columns become FixedBinding entries (with comparison reference),
// non-equality suffix columns become SortedBinding entries. The trimmed
// primary-key suffix continues the sorted keys — this is what lets an
// equality-prefixed scan (status = ?) satisfy ORDER BY pk, exactly as
// Java's ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons
// derives the ordering over getFullKeyExpression() (index key + trimmed
// PK) with Binding.fixed for the equality prefix and Binding.sorted for
// the rest. This enables ordering-aware InJoin source matching and
// RemoveSort-style elision in ImplementSortRule.
func (w *physicalIndexScanWrapper) HintRichOrdering() *properties.RichOrdering {
	if w.plan == nil || len(w.columnNames) == 0 {
		return properties.EmptyOrdering()
	}
	comps := w.plan.GetScanComparisons()
	bm := make(map[values.Value][]properties.OrderingBinding)
	keys := make([]values.Value, 0, len(w.columnNames)+len(w.pkColumnNames))

	rev := w.plan.IsReverse()
	dir := properties.ProvidedSortOrderAscending
	if rev {
		dir = properties.ProvidedSortOrderDescending
	}
	for i, col := range w.columnNames {
		key := &values.FieldValue{Field: col, Typ: values.UnknownType}
		keys = append(keys, key)
		if i < len(comps) && comps[i].IsEquality() {
			bm[key] = []properties.OrderingBinding{properties.FixedBinding(comps[i])}
		} else {
			bm[key] = []properties.OrderingBinding{properties.SortedBinding(dir)}
		}
	}
	for _, col := range plans.TrimmedPKSuffix(w.columnNames, w.pkColumnNames) {
		key := &values.FieldValue{Field: col, Typ: values.UnknownType}
		keys = append(keys, key)
		bm[key] = []properties.OrderingBinding{properties.SortedBinding(dir)}
	}
	return properties.NewRichOrdering(bm, keys, w.unique)
}

// trimmedPKSuffix returns the primary-key columns not already present in
// the index key columns, in PK order. Ports Java's Index.trimPrimaryKey
// semantics as used by ValueIndexExpansionVisitor.fullKey: PK components
// that appear in the index key are trimmed, the remainder is appended
// after the index key.
// HintCost: index scans are cheaper than full table scans because
// they read a subset of records. Apply a selectivity multiplier on
// top of the physical-wrapper discount. Unique indexes with all
// columns equality-bound return cardinality=1 (point lookup).
//
// Fetch I/O cost (FetchCPU per row) is NOT included here — it
// belongs on the Fetch enforcer wrapper, which is eliminated for
// covering scans.
// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalIndexScanWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

func indexBaseCardinality(plan *plans.RecordQueryIndexPlan, stats properties.StatisticsProvider) float64 {
	if plan != nil {
		if types := plan.GetRecordTypes(); len(types) > 0 {
			total := 0.0
			for _, t := range types {
				total += stats.RecordTypeCardinality(t)
			}
			return total
		}
	}
	return stats.RecordTypeCardinality("")
}

func (w *physicalIndexScanWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalIndexScanWrapper)(nil)

// physicalFilterWrapper adapts a `*plans.RecordQueryFilterPlan` to
// the RelationalExpression interface. The wrapped plan has a single
// inner — exposed as a single Quantifier ranging over a fresh
// Reference holding a wrapped version of the inner physical plan.
//
// The wrapped-inner indirection is intentional: it keeps the Memo's
// Reference invariant intact (every Quantifier's Reference holds at
// least one RelationalExpression-typed member). Once a proper
// physical-plan-aware Memo lands, this wrapping goes away — plans
// will be Memo members directly, no adapter needed.
type physicalFilterWrapper struct {
	plan       *plans.RecordQueryFilterPlan
	innerQuant expressions.Quantifier
}

// NewPhysicalFilterWrapper constructs the wrapper. innerQuant must
// range over a Reference holding the wrapped inner physical plan.
func NewPhysicalFilterWrapper(plan *plans.RecordQueryFilterPlan, innerQuant expressions.Quantifier) *physicalFilterWrapper {
	return &physicalFilterWrapper{plan: plan, innerQuant: innerQuant}
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalFilterWrapper) GetPlan() *plans.RecordQueryFilterPlan { return w.plan }

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalFilterWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

// GetResultValue returns the inner Quantifier's flowed object value
// — filter doesn't reshape rows.
func (w *physicalFilterWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

// GetQuantifiers returns the inner Quantifier as the only child.
func (w *physicalFilterWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

// CanCorrelate is false — filter doesn't anchor correlation.
func (w *physicalFilterWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is false — filter has one child.
func (w *physicalFilterWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set — the wrapper
// does not surface predicate-side correlation. Callers that need
// correlation visibility through wrapped physical plans recover it
// from the plan itself (cf. compensationProbeCorrelations, which reads
// bound-prefix scan correlations out of the comparison ranges).
func (w *physicalFilterWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares the wrapped plan's predicate list.
// Children equality is the caller's job (typically via SemanticEquals).
func (w *physicalFilterWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalFilterWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsPlanWithoutChildren(o.plan)
}

// HashCodeWithoutChildren mixes class + plan's hash.
func (w *physicalFilterWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physfilterwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren constructs a fresh wrapper using qs[0] as the new
// inner Quantifier. Returns an error if qs doesn't have exactly
// one entry.
func (w *physicalFilterWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalFilterWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		newPlan := plans.NewRecordQueryFilterPlan(w.plan.GetPredicates(), innerPlan)
		return &physicalFilterWrapper{plan: newPlan, innerQuant: qs[0]}, nil
	}
	return &physicalFilterWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost mirrors the LogicalFilter cost formula and applies the
// physical-wrapper discount so cost-driven extraction prefers the
// physical filter over the logical one.
// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalFilterWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

// OrderingSourceRef: this wrapper PRESERVES its input's order (see
// orderingDelegator in winner_lookup.go).
func (w *physicalFilterWrapper) OrderingSourceRef() *expressions.Reference {
	return w.innerQuant.GetRangesOver()
}

func (w *physicalFilterWrapper) HintOrdering() properties.Ordering {
	ref := w.innerQuant.GetRangesOver()
	if ref == nil {
		return properties.Ordering{}
	}
	for _, m := range ref.AllMembers() {
		o := properties.EstimateOrdering(m)
		if o.IsKnown {
			return o
		}
	}
	return properties.Ordering{}
}

func (w *physicalFilterWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalFilterWrapper)(nil)

// physicalDistinctWrapper adapts a `*plans.RecordQueryDistinctPlan` to
// the RelationalExpression interface.
type physicalDistinctWrapper struct {
	plan       *plans.RecordQueryDistinctPlan
	innerQuant expressions.Quantifier
}

// NewPhysicalDistinctWrapper constructs the wrapper.
func NewPhysicalDistinctWrapper(plan *plans.RecordQueryDistinctPlan, innerQuant expressions.Quantifier) *physicalDistinctWrapper {
	return &physicalDistinctWrapper{plan: plan, innerQuant: innerQuant}
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalDistinctWrapper) GetPlan() *plans.RecordQueryDistinctPlan { return w.plan }

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalDistinctWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

// GetResultValue returns the inner Quantifier's flowed object value.
func (w *physicalDistinctWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

// GetQuantifiers returns the inner Quantifier as the only child.
func (w *physicalDistinctWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

// CanCorrelate is false.
func (w *physicalDistinctWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is false.
func (w *physicalDistinctWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set.
func (w *physicalDistinctWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares the wrapped plan.
func (w *physicalDistinctWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalDistinctWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsPlanWithoutChildren(o.plan)
}

// HashCodeWithoutChildren mixes class + plan's hash.
func (w *physicalDistinctWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physdistwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren constructs a fresh wrapper using qs[0] as the new
// inner Quantifier.
func (w *physicalDistinctWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalDistinctWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		return &physicalDistinctWrapper{plan: plans.NewRecordQueryDistinctPlan(innerPlan), innerQuant: qs[0]}, nil
	}
	return &physicalDistinctWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost mirrors LogicalDistinct with the physical-wrapper discount.
// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalDistinctWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

// OrderingSourceRef: this wrapper PRESERVES its input's order (see
// orderingDelegator in winner_lookup.go).
func (w *physicalDistinctWrapper) OrderingSourceRef() *expressions.Reference {
	return w.innerQuant.GetRangesOver()
}

func (w *physicalDistinctWrapper) HintOrdering() properties.Ordering {
	ref := w.innerQuant.GetRangesOver()
	if ref == nil {
		return properties.Ordering{}
	}
	for _, m := range ref.AllMembers() {
		o := properties.EstimateOrdering(m)
		if o.IsKnown {
			return o
		}
	}
	return properties.Ordering{}
}

func (w *physicalDistinctWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalDistinctWrapper)(nil)

// physicalTypeFilterWrapper adapts a `*plans.RecordQueryTypeFilterPlan`
// to the RelationalExpression interface.
type physicalTypeFilterWrapper struct {
	plan       *plans.RecordQueryTypeFilterPlan
	innerQuant expressions.Quantifier
}

// NewPhysicalTypeFilterWrapper constructs the wrapper.
func NewPhysicalTypeFilterWrapper(plan *plans.RecordQueryTypeFilterPlan, innerQuant expressions.Quantifier) *physicalTypeFilterWrapper {
	return &physicalTypeFilterWrapper{plan: plan, innerQuant: innerQuant}
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalTypeFilterWrapper) GetPlan() *plans.RecordQueryTypeFilterPlan { return w.plan }

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalTypeFilterWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

// GetResultValue returns the inner Quantifier's flowed object value.
func (w *physicalTypeFilterWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

// GetQuantifiers returns the inner Quantifier as the only child.
func (w *physicalTypeFilterWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

// CanCorrelate is false.
func (w *physicalTypeFilterWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is false.
func (w *physicalTypeFilterWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set.
func (w *physicalTypeFilterWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares the wrapped plan.
func (w *physicalTypeFilterWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalTypeFilterWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsPlanWithoutChildren(o.plan)
}

// HashCodeWithoutChildren mixes class + plan's hash.
func (w *physicalTypeFilterWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("phystypefiltwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren constructs a fresh wrapper.
func (w *physicalTypeFilterWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalTypeFilterWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		return &physicalTypeFilterWrapper{plan: plans.NewRecordQueryTypeFilterPlan(w.plan.GetRecordTypes(), innerPlan), innerQuant: qs[0]}, nil
	}
	return &physicalTypeFilterWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost mirrors LogicalTypeFilter with the physical-wrapper
// discount.
// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalTypeFilterWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

// OrderingSourceRef: this wrapper PRESERVES its input's order (see
// orderingDelegator in winner_lookup.go).
func (w *physicalTypeFilterWrapper) OrderingSourceRef() *expressions.Reference {
	return w.innerQuant.GetRangesOver()
}

func (w *physicalTypeFilterWrapper) HintOrdering() properties.Ordering {
	ref := w.innerQuant.GetRangesOver()
	if ref == nil {
		return properties.Ordering{}
	}
	for _, m := range ref.AllMembers() {
		o := properties.EstimateOrdering(m)
		if o.IsKnown {
			return o
		}
	}
	return properties.Ordering{}
}

func (w *physicalTypeFilterWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalTypeFilterWrapper)(nil)

// physicalInsertWrapper adapts a `*plans.RecordQueryInsertPlan` to
// the RelationalExpression interface — same shape as the other
// single-inner physical wrappers.
type physicalInsertWrapper struct {
	plan       *plans.RecordQueryInsertPlan
	innerQuant expressions.Quantifier
}

// NewPhysicalInsertWrapper constructs the wrapper.
func NewPhysicalInsertWrapper(plan *plans.RecordQueryInsertPlan, innerQuant expressions.Quantifier) *physicalInsertWrapper {
	return &physicalInsertWrapper{plan: plan, innerQuant: innerQuant}
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalInsertWrapper) GetPlan() *plans.RecordQueryInsertPlan { return w.plan }

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalInsertWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

// GetResultValue returns the inner Quantifier's flowed object value.
func (w *physicalInsertWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

// GetQuantifiers returns the inner Quantifier as the only child.
func (w *physicalInsertWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

// CanCorrelate is false.
func (w *physicalInsertWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is false.
func (w *physicalInsertWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set.
func (w *physicalInsertWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares wrapped plans.
func (w *physicalInsertWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalInsertWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsPlanWithoutChildren(o.plan)
}

// HashCodeWithoutChildren mixes class + plan's hash.
func (w *physicalInsertWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physinsertwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren constructs a fresh wrapper.
func (w *physicalInsertWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalInsertWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		return &physicalInsertWrapper{plan: plans.NewRecordQueryInsertPlan(innerPlan, w.plan.GetTargetRecordType(), w.plan.GetTargetType()), innerQuant: qs[0]}, nil
	}
	return &physicalInsertWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost: INSERT cost is dominated by the per-row write cost
// (Java's CascadesCostModel weights writes heavily). Mirrors the
// LogicalDML write cost — sumCPU + cardinality * WriteCPU.
// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalInsertWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

func (w *physicalInsertWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalInsertWrapper)(nil)

// physicalDeleteWrapper adapts `*plans.RecordQueryDeletePlan` to
// the RelationalExpression interface.
type physicalDeleteWrapper struct {
	plan       *plans.RecordQueryDeletePlan
	innerQuant expressions.Quantifier
}

// NewPhysicalDeleteWrapper constructs the wrapper.
func NewPhysicalDeleteWrapper(plan *plans.RecordQueryDeletePlan, innerQuant expressions.Quantifier) *physicalDeleteWrapper {
	return &physicalDeleteWrapper{plan: plan, innerQuant: innerQuant}
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalDeleteWrapper) GetPlan() *plans.RecordQueryDeletePlan { return w.plan }

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalDeleteWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

// GetResultValue returns the inner Quantifier's flowed object value.
func (w *physicalDeleteWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

// GetQuantifiers returns the inner Quantifier as the only child.
func (w *physicalDeleteWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

// CanCorrelate is false.
func (w *physicalDeleteWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is false.
func (w *physicalDeleteWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set.
func (w *physicalDeleteWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares wrapped plans.
func (w *physicalDeleteWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalDeleteWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsPlanWithoutChildren(o.plan)
}

// HashCodeWithoutChildren mixes class + plan's hash.
func (w *physicalDeleteWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physdeletewrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren constructs a fresh wrapper.
func (w *physicalDeleteWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalDeleteWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		return &physicalDeleteWrapper{plan: plans.NewRecordQueryDeletePlan(innerPlan, w.plan.GetTargetRecordType()), innerQuant: qs[0]}, nil
	}
	return &physicalDeleteWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost: DELETE write-heavy cost like INSERT.
// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalDeleteWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

func (w *physicalDeleteWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalDeleteWrapper)(nil)

// physicalUpdateWrapper adapts `*plans.RecordQueryUpdatePlan` to
// the RelationalExpression interface.
type physicalUpdateWrapper struct {
	plan       *plans.RecordQueryUpdatePlan
	innerQuant expressions.Quantifier
}

// NewPhysicalUpdateWrapper constructs the wrapper.
func NewPhysicalUpdateWrapper(plan *plans.RecordQueryUpdatePlan, innerQuant expressions.Quantifier) *physicalUpdateWrapper {
	return &physicalUpdateWrapper{plan: plan, innerQuant: innerQuant}
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalUpdateWrapper) GetPlan() *plans.RecordQueryUpdatePlan { return w.plan }

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalUpdateWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

// GetResultValue returns the inner Quantifier's flowed object value.
func (w *physicalUpdateWrapper) GetResultValue() values.Value {
	return w.innerQuant.GetFlowedObjectValue()
}

// GetQuantifiers returns the inner Quantifier as the only child.
func (w *physicalUpdateWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

// CanCorrelate is false.
func (w *physicalUpdateWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is false.
func (w *physicalUpdateWrapper) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set.
func (w *physicalUpdateWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares wrapped plans.
func (w *physicalUpdateWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalUpdateWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsPlanWithoutChildren(o.plan)
}

// HashCodeWithoutChildren mixes class + plan's hash.
func (w *physicalUpdateWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physupdatewrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren constructs a fresh wrapper.
func (w *physicalUpdateWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalUpdateWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil && isLeafReplaceable(innerPlan) {
		return &physicalUpdateWrapper{plan: plans.NewRecordQueryUpdatePlan(innerPlan, w.plan.GetTargetRecordType(), w.plan.GetTransforms()), innerQuant: qs[0]}, nil
	}
	return &physicalUpdateWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost: UPDATE write-heavy cost like INSERT/DELETE.
// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalUpdateWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

func (w *physicalUpdateWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalUpdateWrapper)(nil)

// physicalUnionWrapper adapts `*plans.RecordQueryUnionPlan` to the
// RelationalExpression interface. Unlike the single-inner wrappers,
// Union exposes N inner Quantifiers (one per child plan).
type physicalUnionWrapper struct {
	plan        *plans.RecordQueryUnionPlan
	innerQuants []expressions.Quantifier
}

// NewPhysicalUnionWrapper constructs the wrapper.
func NewPhysicalUnionWrapper(plan *plans.RecordQueryUnionPlan, innerQuants []expressions.Quantifier) *physicalUnionWrapper {
	copied := make([]expressions.Quantifier, len(innerQuants))
	copy(copied, innerQuants)
	return &physicalUnionWrapper{plan: plan, innerQuants: copied}
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalUnionWrapper) GetPlan() *plans.RecordQueryUnionPlan { return w.plan }

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalUnionWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

// GetResultValue returns the first inner's flowed object value.
// Java's RecordQueryUnionPlan emits rows compatible with all
// children; Go picks the first child's row shape (union legs are
// column-aligned by construction, so any child's shape stands in).
func (w *physicalUnionWrapper) GetResultValue() values.Value {
	if len(w.innerQuants) == 0 {
		return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
	}
	return w.innerQuants[0].GetFlowedObjectValue()
}

// GetQuantifiers returns the inner Quantifiers (children).
func (w *physicalUnionWrapper) GetQuantifiers() []expressions.Quantifier { return w.innerQuants }

// CanCorrelate is false.
func (w *physicalUnionWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is true — UNION children are bag-equivalent.
func (w *physicalUnionWrapper) ChildrenAsSet() bool { return true }

// GetCorrelatedToWithoutChildren returns the empty set.
func (w *physicalUnionWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares the wrapped plan.
func (w *physicalUnionWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalUnionWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsPlanWithoutChildren(o.plan)
}

// HashCodeWithoutChildren mixes class + plan's hash.
func (w *physicalUnionWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physunionwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren constructs a fresh wrapper with the new quantifiers.
func (w *physicalUnionWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	copied := make([]expressions.Quantifier, len(qs))
	copy(copied, qs)
	return &physicalUnionWrapper{plan: w.plan, innerQuants: copied}, nil
}

// HintCost: UNION cardinality is sum of children, CPU is cumulative
// + per-output-row merge work. Mirrors LogicalUnion.
// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalUnionWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

func (w *physicalUnionWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalUnionWrapper)(nil)

// physicalIntersectionWrapper adapts `*plans.RecordQueryIntersectionPlan`
// to the RelationalExpression interface. Same N-child shape as the
// Union wrapper; cost differs (Intersection bounded by min child
// cardinality, while Union sums).
type physicalIntersectionWrapper struct {
	plan        *plans.RecordQueryIntersectionPlan
	innerQuants []expressions.Quantifier
}

// NewPhysicalIntersectionWrapper constructs the wrapper.
func NewPhysicalIntersectionWrapper(plan *plans.RecordQueryIntersectionPlan, innerQuants []expressions.Quantifier) *physicalIntersectionWrapper {
	copied := make([]expressions.Quantifier, len(innerQuants))
	copy(copied, innerQuants)
	return &physicalIntersectionWrapper{plan: plan, innerQuants: copied}
}

// GetPlan exposes the wrapped physical plan.
func (w *physicalIntersectionWrapper) GetPlan() *plans.RecordQueryIntersectionPlan {
	return w.plan
}

// GetRecordQueryPlan implements physicalPlanExpression.
func (w *physicalIntersectionWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

// GetResultValue returns the first inner's flowed object value —
// intersection emits rows compatible with all children.
func (w *physicalIntersectionWrapper) GetResultValue() values.Value {
	if len(w.innerQuants) == 0 {
		return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
	}
	return w.innerQuants[0].GetFlowedObjectValue()
}

// GetQuantifiers returns the inner Quantifiers (children).
func (w *physicalIntersectionWrapper) GetQuantifiers() []expressions.Quantifier {
	return w.innerQuants
}

// IsIntersection implements properties.IntersectionExpression.
func (w *physicalIntersectionWrapper) IsIntersection() {}

// CanCorrelate is false.
func (w *physicalIntersectionWrapper) CanCorrelate() bool { return false }

// ChildrenAsSet is true — INTERSECTION children are bag-equivalent.
func (w *physicalIntersectionWrapper) ChildrenAsSet() bool { return true }

// GetCorrelatedToWithoutChildren returns the empty set.
func (w *physicalIntersectionWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares the wrapped plan.
func (w *physicalIntersectionWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalIntersectionWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsPlanWithoutChildren(o.plan)
}

// HashCodeWithoutChildren mixes class + plan's hash.
func (w *physicalIntersectionWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physintersectionwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

// WithChildren constructs a fresh wrapper with the new quantifiers.
func (w *physicalIntersectionWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	copied := make([]expressions.Quantifier, len(qs))
	copy(copied, qs)
	return &physicalIntersectionWrapper{plan: w.plan, innerQuants: copied}, nil
}

// HintCost: Intersection cardinality is bounded by the SMALLEST
// child (the intersection can't be larger than its smallest
// participant). CPU sums children + per-output-row merge work
// (more expensive than Union — comparison-key-driven matching).
// Mirrors LogicalIntersection.
// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalIntersectionWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

func (w *physicalIntersectionWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalIntersectionWrapper)(nil)

// --- Projection wrapper -----------------------------------------------

type physicalProjectionWrapper struct {
	plan       *plans.RecordQueryProjectionPlan
	innerQuant expressions.Quantifier
}

func NewPhysicalProjectionWrapper(plan *plans.RecordQueryProjectionPlan, innerQuant expressions.Quantifier) *physicalProjectionWrapper {
	return &physicalProjectionWrapper{plan: plan, innerQuant: innerQuant}
}

func (w *physicalProjectionWrapper) GetPlan() *plans.RecordQueryProjectionPlan { return w.plan }

func (w *physicalProjectionWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalProjectionWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalProjectionWrapper) GetQuantifiers() []expressions.Quantifier {
	return []expressions.Quantifier{w.innerQuant}
}

func (w *physicalProjectionWrapper) CanCorrelate() bool { return false }

func (w *physicalProjectionWrapper) ChildrenAsSet() bool { return false }

func (w *physicalProjectionWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalProjectionWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalProjectionWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsPlanWithoutChildren(o.plan)
}

func (w *physicalProjectionWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physprojwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalProjectionWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("physicalProjectionWrapper.WithChildren: expected 1 child, got %d", len(qs))
	}
	// Always relink to the extracted inner, including compound joins — do NOT
	// gate on isLeafReplaceable here. A projection is a transparent unary cap
	// (like the in-memory sort, RFC-069). Historically
	// MergeProjectionAndFetchRule / ImplementProjectionFinalRule built it over
	// an InJoin whose plan carried a nil inner, and without relinking `SELECT
	// id ... WHERE a IN (...)` extracted `Project([id], InJoin(<nil>))`
	// (RFC-070). That nil-inner state no longer exists (RFC-183 — rules bake
	// the concrete child), so this is now a plain structural rebuild rather
	// than a repair; the relink still matters because qs[0] resolves to the
	// extracted winner, which need not be the plan snapshot taken at build
	// time. Wrappers that
	// embed predicate/filter/DML semantics in their own plan (aggregation,
	// delete/update) keep the leaf gate: their child quantifier need not carry
	// the filtered inner, so relinking to it would drop the filter.
	if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil {
		newPlan := plans.NewRecordQueryProjectionPlanWithAliases(w.plan.GetProjections(), w.plan.GetAliases(), innerPlan)
		return &physicalProjectionWrapper{plan: newPlan, innerQuant: qs[0]}, nil
	}
	return &physicalProjectionWrapper{plan: w.plan, innerQuant: qs[0]}, nil
}

// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalProjectionWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

// OrderingSourceRef: this wrapper PRESERVES its input's order (see
// orderingDelegator in winner_lookup.go).
func (w *physicalProjectionWrapper) OrderingSourceRef() *expressions.Reference {
	return w.innerQuant.GetRangesOver()
}

func (w *physicalProjectionWrapper) HintOrdering() properties.Ordering {
	ref := w.innerQuant.GetRangesOver()
	if ref == nil {
		return properties.Ordering{}
	}
	for _, m := range ref.AllMembers() {
		o := properties.EstimateOrdering(m)
		if o.IsKnown {
			return o
		}
	}
	return properties.Ordering{}
}

func (w *physicalProjectionWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalProjectionWrapper)(nil)

// --- Values wrapper ---------------------------------------------------

type physicalValuesWrapper struct {
	plan *plans.RecordQueryValuesPlan
}

func NewPhysicalValuesWrapper(plan *plans.RecordQueryValuesPlan) *physicalValuesWrapper {
	return &physicalValuesWrapper{plan: plan}
}

func (w *physicalValuesWrapper) GetPlan() *plans.RecordQueryValuesPlan { return w.plan }

func (w *physicalValuesWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalValuesWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalValuesWrapper) GetQuantifiers() []expressions.Quantifier { return nil }

func (w *physicalValuesWrapper) CanCorrelate() bool { return false }

func (w *physicalValuesWrapper) ChildrenAsSet() bool { return false }

func (w *physicalValuesWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalValuesWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalValuesWrapper)
	if !ok {
		return false
	}
	return plans.Equals(w.plan, o.plan)
}

func (w *physicalValuesWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physvalueswrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalValuesWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 0 {
		return nil, fmt.Errorf("physicalValuesWrapper.WithChildren: expected 0 children, got %d", len(qs))
	}
	return w, nil
}

// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalValuesWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

func (w *physicalValuesWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

var _ expressions.RelationalExpression = (*physicalValuesWrapper)(nil)

// physicalAggregateIndexWrapper wraps a RecordQueryAggregateIndexPlan
// as a leaf physical expression. Mirrors the aggregate index scan path
// in Java's Cascades planner.
type physicalAggregateIndexWrapper struct {
	plan *plans.RecordQueryAggregateIndexPlan
}

func (w *physicalAggregateIndexWrapper) GetRecordQueryPlan() plans.RecordQueryPlan { return w.plan }

func (w *physicalAggregateIndexWrapper) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

func (w *physicalAggregateIndexWrapper) GetQuantifiers() []expressions.Quantifier { return nil }
func (w *physicalAggregateIndexWrapper) CanCorrelate() bool                       { return false }
func (w *physicalAggregateIndexWrapper) ChildrenAsSet() bool                      { return false }

func (w *physicalAggregateIndexWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (w *physicalAggregateIndexWrapper) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*physicalAggregateIndexWrapper)
	if !ok {
		return false
	}
	return w.plan.EqualsPlanWithoutChildren(o.plan)
}

func (w *physicalAggregateIndexWrapper) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("physaggidxwrap|"))
	if w.plan != nil {
		writeHash64(h, w.plan.HashCodeWithoutChildren())
	}
	return h.Sum64()
}

func (w *physicalAggregateIndexWrapper) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 0 {
		return nil, fmt.Errorf("physicalAggregateIndexWrapper.WithChildren: expected 0 children, got %d", len(qs))
	}
	return w, nil
}

// HintCost delegates to the plan, which owns its cost (RFC-183 P5).
func (w *physicalAggregateIndexWrapper) HintCost(child []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return w.plan.HintCost(child, stats)
}

func (w *physicalAggregateIndexWrapper) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return w
}

// HintOrdering delegates to the plan, which owns its ordering (RFC-183 P5).
func (w *physicalAggregateIndexWrapper) HintOrdering() properties.Ordering {
	return w.plan.HintOrdering()
}

// IsPhysicalAggregateIndex reports whether the expression is an aggregate
// index scan wrapper.
func IsPhysicalAggregateIndex(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalAggregateIndexWrapper)
	return ok
}

var _ physicalPlanExpression = (*physicalAggregateIndexWrapper)(nil)

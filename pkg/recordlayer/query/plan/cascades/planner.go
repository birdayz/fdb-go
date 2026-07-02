package cascades

import (
	"sort"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// Planner is the task-stack driven cascades planner — Track B6.
//
// Replaces FixpointApply's "fire every rule on every Reference each
// pass" approach with a task-stack driver that:
//
//   - Explores the expression DAG bottom-up: leaves fire rules first,
//     ancestors after.
//   - Tracks per-Reference saturation: a Reference whose member count
//     hasn't changed since the last rule-firing round is NOT re-fired.
//   - Fires rules individually (per-rule task granularity): each rule
//     gets its own TransformReferenceTask, enabling per-rule events,
//     staleness detection, and future rule-priority scheduling.
//   - Exposes event hooks for diagnostic output without changing the
//     core driver.
//
// The Java equivalent is `CascadesPlanner` — task-stack with EXPLORE
// and OPTIMIZE phases. This implements both: EXPLORE drives the
// task-stack to convergence; OPTIMIZE extracts the cheapest member at
// every reachable Reference via properties.ExtractBestPlanFromSelector.
//
// Convergence: same contract as FixpointApply. A saturated Reference
// has stable member count; the stack drains; planner returns. Hard
// cap (MaxTasks) prevents pathological non-termination from
// rule-yielding-fresh-members loops; default 100_000.
//
// The planner is single-threaded (Java's is too).
type Planner struct {
	// stack MUST be LIFO. Two task architectures share it:
	// - Plan() uses unified tasks: InitiatePlannerPhaseTask →
	//   ExploreGroupTask/OptimizeGroupTask → ExploreExprTask/
	//   TransformExprTask/TransformImplTask/OptimizeInputsTask.
	// - Explore() uses legacy tasks: ExploreReferenceTask →
	//   SaturationCheckTask/TransformReferenceTask.
	// Both depend on LIFO pop order for bottom-up exploration
	// (children before parents).
	stack []Task
	rules []ExpressionRule
	ctx   PlanContext
	memo  *Memo

	// rewritingImplRules run during PhaseRewriting. They yield
	// final expressions (FinalizeExpressionsRule promotes exploratory
	// to final for OptimizeGroup selection).
	rewritingImplRules []ImplementationRule

	// implementationRules run during PhasePlanning after the
	// REWRITING phase converges. They yield physical expressions
	// into FinalMembers via InsertFinal.
	implementationRules []ImplementationRule

	// planningExpressionRules are ExpressionRules that fire during
	// PLANNING (not EXPLORE). They yield into InsertFinal so their
	// results go to finalMembers and are authoritative for plan
	// selection. This is the "BatchA→PLANNING migration": scan/filter/
	// agg rules that produce physical wrappers fire here with full
	// constraint and ordering information available.
	planningExpressionRules []ExpressionRule

	// exploreCount[ref] = member count at last saturation check on
	// `ref`. SaturationCheckTask short-circuits when count hasn't grown.
	exploreCount map[*expressions.Reference]int

	// MaxTasks caps the total tasks executed before the planner
	// gives up (returns the partial result). Defaults to 100_000.
	// Hitting the cap is a strong signal of a non-terminating rule —
	// callers should report.
	MaxTasks int

	tasksRun int

	// costModel is the comparator for OptimizeReferenceTask. Defaults
	// to PlanningCostModelLess. Set to RewritingCostModelLess for the
	// REWRITING phase. Matches Java's per-phase cost model architecture.
	costModel func(a, b expressions.RelationalExpression) bool

	// stats is the optional table-level cardinality statistics. When
	// set, the cost model uses real record counts instead of the
	// default 1e6 constant.
	stats properties.StatisticsProvider

	// constraintMap holds ordering constraints propagated during
	// PLANNING's preorder rules. Shared across all tasks.
	constraintMap *ConstraintMap

	// dataAccessConsumed tracks, per canonical reference, the partial-match
	// count last consumed by pushDataAccessTasks's standalone (yieldUnknown)
	// path — the RFC-148 §3c re-entry/termination guard. Re-consumption runs
	// only when the match set grows. Reset per Plan() run.
	dataAccessConsumed map[*expressions.Reference]int

	// events is the (optional) event handler. Nil = no events.
	events PlannerEventHandler
}

// PlannerEventHandler receives callbacks for diagnostic output.
// All methods are optional; nil-implementation passes through.
//
// Implementations must be safe for re-entry (the planner doesn't
// invoke events recursively but doesn't lock either).
type PlannerEventHandler interface {
	// OnExploreReference fires before a Reference is explored.
	OnExploreReference(ref *expressions.Reference)
	// OnExploreExpression fires before an expression's children are
	// pushed onto the task stack.
	OnExploreExpression(e expressions.RelationalExpression)
	// OnTransformRule fires after a single rule has been applied to a
	// Reference. `yielded` is the number of new members the rule
	// produced (0 if no match or all yields were duplicates).
	OnTransformRule(ref *expressions.Reference, rule ExpressionRule, yielded int)
	// OnApplyRules fires at the saturation-check boundary after all
	// per-rule tasks for a Reference have completed. `grew` is the
	// total growth since the round started.
	OnApplyRules(ref *expressions.Reference, grew int)
	// OnOptimizeReference fires when OptimizeReferenceTask picks the
	// best member for a Reference. `best` is the chosen member;
	// nil if the Reference is empty.
	OnOptimizeReference(ref *expressions.Reference, best expressions.RelationalExpression)
}

// NewPlanner builds a planner with the given rule set + context.
// Pass DefaultExpressionRules() for the standard rule set.
//
// Pass nil ctx to use the empty PlanContext.
func NewPlanner(rules []ExpressionRule, ctx PlanContext) *Planner {
	if ctx == nil {
		ctx = EmptyPlanContext()
	}
	return &Planner{
		rules:              rules,
		rewritingImplRules: []ImplementationRule{NewFinalizeExpressionsRule()},
		ctx:                ctx,
		memo:               nil,
		exploreCount:       make(map[*expressions.Reference]int),
		costModel:          PlanningCostModelLess,
		MaxTasks:           100_000,
	}
}

// Memo returns the planner's Memo structure. Available after Explore
// has been called (returns nil before that).
func (p *Planner) Memo() *Memo {
	return p.memo
}

// BestMember returns the OPTIMIZE-chosen best member for `ref`,
// or nil if the Reference wasn't optimized. Delegates to the
// per-properties winner map (NoProperties = cheapest overall).
func (p *Planner) BestMember(ref *expressions.Reference) expressions.RelationalExpression {
	if ref == nil {
		return nil
	}
	return ref.Winner(expressions.NoProperties)
}

// HasBestMember reports whether a winner exists for `ref`.
func (p *Planner) HasBestMember(ref *expressions.Reference) bool {
	if ref == nil {
		return false
	}
	return ref.HasWinner(expressions.NoProperties)
}

// OptimizeGroup finds the cheapest plan in `ref` that satisfies
// `props` and stores it as a winner. Following Graefe 1995 §2:
//
//   - If a winner already exists for (ref, props), return it.
//   - Iterate all members, find cheapest satisfying props.
//   - If no member satisfies props and props requires ordering,
//     use the NoProperties winner + sort enforcer.
//   - Store the winner for (ref, props).
//
// Returns the winner, or nil if the group is empty.
func (p *Planner) OptimizeGroup(ref *expressions.Reference, props expressions.PhysicalProperties) expressions.RelationalExpression {
	if ref == nil {
		return nil
	}

	// Check memoized winner.
	if w := ref.Winner(props); w != nil {
		return w
	}

	// No ordering required → just pick cheapest overall. A nil-inner Fetch
	// shell is a push-through template whose inner is resolved at extraction;
	// costed without it, it ranks artificially cheap and (as a join inner)
	// makes the enclosing nested loop look free, flipping the join order onto
	// the wrong side. Prefer a real physical alternative when one exists; fall
	// back to the shell only when it is the sole option (extraction relinks it).
	if props.IsEmpty() {
		best := ref.GetBest(p.costModel)
		if best != nil && isNilInnerFetch(best) {
			if alt := findBestValidPhysicalExpr(ref, p.costModel); alt != nil {
				best = alt
			}
		}
		if best != nil {
			ref.SetWinner(props, best)
		}
		return best
	}

	// Find cheapest member that satisfies the required ordering.
	var best expressions.RelationalExpression
	for _, m := range ref.AllMembers() {
		h, ok := m.(orderingHinter)
		if !ok {
			continue
		}
		ord := h.HintOrdering()
		if !ord.IsKnown || len(ord.Keys) == 0 {
			continue
		}
		provided := orderingToProps(ord)
		if !provided.Satisfies(props) {
			continue
		}
		if best == nil || p.costModel(m, best) {
			best = m
		}
	}

	if best != nil {
		ref.SetWinner(props, best)
		return best
	}

	// No member satisfies props. The Cascades-paper approach (Graefe 1995)
	// would add an enforcer (sort) here. For now, return nil — the caller handles
	// the enforcer at extraction time via sortWinnerFromChild.
	return nil
}

// orderingToProps converts a properties.Ordering to PhysicalProperties.
func orderingToProps(ord properties.Ordering) expressions.PhysicalProperties {
	names := make([]string, len(ord.Keys))
	for i, k := range ord.Keys {
		if fv, ok := k.(*values.FieldValue); ok {
			names[i] = fv.Field
		} else {
			names[i] = k.Name()
		}
	}
	return expressions.OrderingFromNameDir(names, ord.Descending)
}

// WithImplementationRules adds rules for PhasePlanning. These run
// after the REWRITING phase converges. Returns p for chaining.
func (p *Planner) WithImplementationRules(rules []ImplementationRule) *Planner {
	p.implementationRules = rules
	return p
}

// WithPlanningExpressionRules adds ExpressionRules that fire during
// PLANNING's bottom-up implementation pass. Unlike EXPLORE-phase
// expression rules (which yield to members via Insert), these yield
// to finalMembers via InsertFinal. This is the BatchA→PLANNING
// migration: physical scan/filter/agg wrappers are produced during
// PLANNING where constraint and ordering information is available.
func (p *Planner) WithPlanningExpressionRules(rules []ExpressionRule) *Planner {
	p.planningExpressionRules = append(PlanningExplorationRules(), rules...)
	return p
}

// WithCostModel sets the comparator used by OptimizeReferenceTask.
// Use RewritingCostModelLess for the REWRITING phase, PlanningCostModelLess
// for the PLANNING phase. Matches Java's per-phase cost model. Returns p.
func (p *Planner) WithCostModel(less func(a, b expressions.RelationalExpression) bool) *Planner {
	p.costModel = less
	return p
}

// WithStatistics sets the table-level cardinality statistics for the cost
// model. Stats flow through EstimateCost and HintCost to give scan/index
// wrappers real cardinality instead of the default 1e6 constant.
// Replaces the cost model — call after WithCostModel if both are used.
func (p *Planner) WithStatistics(stats properties.StatisticsProvider) *Planner {
	p.stats = stats
	p.costModel = NewPlanningCostModelLessWithContext(stats, p.ctx)
	return p
}

// Statistics returns the planner's statistics provider, or nil if none set.
func (p *Planner) Statistics() properties.StatisticsProvider { return p.stats }

// WithMaxTasks overrides the task cap. Returns p for chaining.
func (p *Planner) WithMaxTasks(n int) *Planner {
	p.MaxTasks = n
	return p
}

// SetEvents installs an event handler. Pass nil to disable events.
// Returns p for chaining.
func (p *Planner) SetEvents(h PlannerEventHandler) *Planner {
	p.events = h
	return p
}

// Plan runs the unified two-phase REWRITING → PLANNING pipeline and
// returns the cost-cheapest extracted plan tree.
//
// Pushes InitiatePlannerPhaseTask{PhaseRewriting} which chains to
// PhasePlanning via the unified task types (ExploreGroupTask,
// TransformExprTask, TransformImplTask, OptimizeGroupTask,
// OptimizeInputsTask). After the stack drains, extracts the best
// plan via properties.ExtractBestPlanFromSelector.
//
// Returns:
//   - plan: the extracted RelationalExpression; nil if rootRef is empty.
//   - tasks: total tasks executed across both phases.
//   - err: nil on success; ErrPlannerCapHit if EXPLORE hit MaxTasks
//     (no OPTIMIZE attempted); extraction error otherwise.
func (p *Planner) Plan(rootRef *expressions.Reference) (expressions.RelationalExpression, int, error) {
	if rootRef == nil {
		return nil, 0, nil
	}
	if p.memo == nil {
		p.memo = NewMemo(rootRef)
	}
	p.constraintMap = NewConstraintMap()
	p.dataAccessConsumed = make(map[*expressions.Reference]int)

	// One task-stack drives both REWRITING and PLANNING phases.
	// InitiatePlannerPhase(REWRITING) pushes ExploreGroup + OptimizeGroup
	// for REWRITING, then chains to InitiatePlannerPhase(PLANNING).
	p.push(&InitiatePlannerPhaseTask{Phase: PhaseRewriting, RootRef: rootRef})

	for len(p.stack) > 0 {
		if p.tasksRun >= p.MaxTasks {
			return nil, p.tasksRun, ErrPlannerCapHit
		}
		task := p.pop()
		task.Run(p)
		p.tasksRun++
	}

	// After the task-stack drains, each Reference's FinalMembers has
	// been pruned to exactly one physical plan by OptimizeGroup.
	plan, err := properties.ExtractBestPlanFromSelector(rootRef, p, p.stats)
	if err != nil {
		return plan, p.tasksRun, err
	}
	// Catch-all backstop: reject any PHYSICAL plan that carries an index-only
	// predicate as a residual filter (a vector K-NN DistanceRank that no index
	// serves — e.g. a metric-mismatched distance, which can reach a physical filter
	// via ImplementSimpleSelectRule / the NLJ residual builder, NOT just the gated
	// ImplementFilterRule). Surfaces the clean UnplannableIndexOnlyResidualError
	// instead of an execution-time panic in Comparison.EvalAgainst.
	if perr := validateNoIndexOnlyResidual(plan); perr != nil {
		return nil, p.tasksRun, perr
	}
	// When the Java !isIndexOnly() ImplementFilterRule gate instead left the best
	// plan NON-physical (no producer realized the index-only LogicalFilter), the
	// physical walk above sees nothing — surface the same clean error from the
	// logical side rather than letting the caller report the internal type.
	if plan != nil && !isPhysical(plan) {
		if bad := findIndexOnlyLogicalResidual(rootRef); bad != nil {
			return nil, p.tasksRun, &UnplannableIndexOnlyResidualError{Predicate: bad.Explain()}
		}
	}
	return plan, p.tasksRun, nil
}

// ErrPlannerCapHit signals that Explore exited via the MaxTasks
// cap rather than convergence. Callers should treat this as a
// non-termination indicator and report.
var ErrPlannerCapHit = plannerErr("planner: MaxTasks cap hit before convergence")

// plannerErr is a string-error type local to the planner.
type plannerErr string

// Error returns the message.
func (e plannerErr) Error() string { return string(e) }

// Explore drives the task-stack until convergence (no rule yields a
// new member anywhere in the DAG) or the MaxTasks cap is hit.
//
// Returns:
//   - tasksRun: total tasks executed.
//   - converged: true if the stack drained cleanly; false if MaxTasks
//     hit. Hitting the cap is a non-termination signal.
//
// Idempotent at convergence: a second Explore call on the same
// rooted Reference makes no progress (every Reference's saturation
// state is preserved across calls).
func (p *Planner) Explore(rootRef *expressions.Reference) (tasksRun int, converged bool) {
	if rootRef == nil {
		return 0, true
	}
	if p.memo == nil {
		p.memo = NewMemo(rootRef)
	}
	p.push(&ExploreReferenceTask{Ref: rootRef})
	for len(p.stack) > 0 {
		if p.tasksRun >= p.MaxTasks {
			return p.tasksRun, false
		}
		task := p.pop()
		task.Run(p)
		p.tasksRun++
	}
	return p.tasksRun, true
}

// push appends a task to the stack (LIFO).
func (p *Planner) push(t Task) {
	p.stack = append(p.stack, t)
}

// pop removes and returns the top of stack. Caller must check
// len(stack) > 0.
func (p *Planner) pop() Task {
	n := len(p.stack)
	t := p.stack[n-1]
	p.stack = p.stack[:n-1]
	return t
}

// rulesForPhase returns the expression and implementation rules for the
// given planner phase.
func (p *Planner) rulesForPhase(phase PlannerPhase) ([]ExpressionRule, []ImplementationRule) {
	switch phase {
	case PhaseRewriting:
		return p.rules, p.rewritingImplRules
	case PhasePlanning:
		return p.planningExpressionRules, p.implementationRules
	default:
		return nil, nil
	}
}

// costModelForPhase returns the cost model comparator for the given phase.
func (p *Planner) costModelForPhase(phase PlannerPhase) func(a, b expressions.RelationalExpression) bool {
	switch phase {
	case PhaseRewriting:
		return RewritingCostModelLess
	case PhasePlanning:
		return p.costModel
	default:
		return p.costModel
	}
}

// pushDataAccessTasks generates data access expressions (index scans)
// from PartialMatches on the Reference. This is the Go equivalent of
// Java's TransformMatchPartition tasks.
func (p *Planner) pushDataAccessTasks(ref *expressions.Reference, _ expressions.RelationalExpression) {
	// Absorb candidate-side parent expressions (Select → MatchableSort)
	// onto this ref's matches before consuming them. PLANNING-phase
	// matches are seeded during exploration, after the phase-start
	// AdjustMatches walk, so their matched ordering parts (which let an
	// index scan satisfy a requested ordering and eliminate an in-memory
	// sort) are only computed here, at consumption time. Java's
	// AdjustMatchRule is event-driven and fires on each new match; this
	// is the Go equivalent at the data-access boundary.
	AdjustPartialMatchesForRef(ref)

	candidates := GetPartialMatchCandidatesTyped(ref)
	if len(candidates) == 0 {
		return
	}

	// Drop aggregate-index candidates: they are consumed by AggregateDataAccessRule
	// (which matches the GroupByExpression and reads the pre-aggregated value), NOT
	// by the regular value-index data-access path. An aggregate index stores
	// aggregated rows, not base records — matching its underlying scan here yields
	// IndexScan(agg_index, [=]) which a StreamingAgg then re-aggregates, counting
	// the single group row as 1 (or reading the wrong column for SUM). The match
	// infra seeds these matches once the regular path is active; they must not be
	// realized as record scans (TestFDB_AggregateIndexUsage count/sum_with_eq_filter).
	{
		filtered := candidates[:0]
		for _, c := range candidates {
			if _, isAgg := c.(*AggregateIndexMatchCandidate); isAgg {
				continue
			}
			filtered = append(filtered, c)
		}
		candidates = filtered
		if len(candidates) == 0 {
			return
		}
	}

	var requestedOrderings []*RequestedOrdering
	if p.constraintMap != nil {
		if orderings, ok := Get(p.constraintMap, ref, RequestedOrderingConstraintKey); ok {
			requestedOrderings = orderings
		}
	}

	// NOTE (RFC-150 Phase 2b — muzzle retired): a join-leg ref (one whose data-access
	// match binds a parameter correlated to an outer quantifier) used to be muzzled here
	// — its compensations were force-InsertFinal'd so a re-optimized standalone leg
	// filter could not win and sever the join's correlation feed → 0 rows. That muzzle
	// (`!refIsJoinLeg`) was a band-aid for a missing STRUCTURAL property. It is now
	// replaced by the task-graph invariant (B1, unified_tasks.go): OptimizeInputsTask is
	// pushed only for PHYSICAL parent members (Java CascadesPlanner.java:524), so a
	// correlated leg's group is pruned to a winner ONLY as the inner child of the
	// binding physical FlatMap (outer alias live) — never as a free-standing group. A
	// correlated SUBSEL scan therefore cannot be stamped a standalone winner, so the
	// leg compensation goes through the same standalone path as any ref. The
	// compensationSafeForYield outer-correlation guard below stays as defense-in-depth.

	// B4 re-entry / termination guard (RFC-148 §3c). yieldUnknown routes a logical
	// compensation into the EXPLORATORY set, so the enclosing ExploreGroupTask
	// re-explores it and re-enters pushDataAccessTasks on this ref. Re-consuming the
	// same matches re-yields compensations — deduped, but wasted work and a
	// non-convergence risk if a fresh-alias compensation ever escapes structural
	// dedup. Run the standalone consumption ONLY when the partial-match set has GROWN
	// since the last consumption — NOT a "consumed-ever" gate, which would drop
	// matches seeded mid-exploration (AdjustPartialMatchesForRef seeds across rounds).
	//
	// The guard applies ONLY when there is NO requested ordering. A requested ordering
	// can be propagated into this ref AFTER a first consumption at unchanged match
	// count, and DataAccessForMatchPartition (which takes requestedOrderings) would
	// then mint a sort-eliminating ordered scan that a count-only guard would wrongly
	// suppress — and Go has no physical sort, so a missed ordered scan is a wrong /
	// extra-sort plan shape, not merely slower. With an ordering present we re-consume
	// every round (matching OLD, which had no guard at all); convergence there is
	// bounded by Insert dedup + the 10-round cap. (Join legs are no longer exempt — the
	// muzzle is retired, B1 above — so they take the same growth-keyed guard; the
	// cross-candidate intersection below keeps its own hasIntersectionFinal guard.)
	runConsumption := true
	if len(requestedOrderings) == 0 {
		totalMatches := 0
		for _, c := range candidates {
			totalMatches += len(GetPartialMatchesForCandidate(ref, c))
		}
		key := ref.Canonical()
		if last, seen := p.dataAccessConsumed[key]; seen && totalMatches <= last {
			runConsumption = false
		}
		p.dataAccessConsumed[key] = totalMatches
	}

	for _, candidate := range candidates {
		if !runConsumption {
			break
		}
		matches := GetPartialMatchesForCandidate(ref, candidate)
		if len(matches) == 0 {
			continue
		}
		exprs := DataAccessForMatchPartition(requestedOrderings, matches, p.ctx, nil)
		for _, expr := range exprs {
			// Every ref — including a join leg — takes the same path now (the muzzle is
			// retired; B1's task-graph invariant prevents a correlated leg from being
			// stamped a standalone winner). An UNSAFE logical compensation — one whose
			// inner scan is a
			// vector top-K / aggregate — is NOT narrowable by a post-filter (a residual
			// applied after the top-K / grouping changes the result). It keeps the OLD
			// InsertFinal path: it stays logical and the query correctly fails to plan if
			// there is no physical alternative, rather than being re-optimized into a wrong
			// plan. This inner-scan guard is the remaining SAFETY half of the retired
			// isSimpleResidualCompensation allowlist (compensationSafeForYield). The
			// separate index-only-predicate concern is now handled structurally by the Java
			// !isIndexOnly() ImplementFilterRule gate + the validateNoIndexOnlyResidual
			// catch-all backstop (Plan()), not by this compensation guard (RFC-151).
			if !isPhysical(expr) && !compensationSafeForYield(expr) {
				ref.InsertFinal(expr)
				continue
			}
			// Physical plan → final set (competes now). A SAFE logical compensation →
			// exploratory set, re-optimized by the normal ExploreExprTask loop
			// (ImplementFilterRule / InComparisonToExplodeRule / …) until it yields a
			// RecordQueryPlan (Java CascadesRuleCall.yieldUnknownExpression). This replaces
			// the surgical isSimpleResidualCompensation predicate-SHAPE allowlist, whose
			// non-allowlisted shapes (IN, multi-predicate residuals) silently fell to a
			// full scan — the rot. (RFC-148 §3a; the re-entry this creates is bounded by
			// the match-growth guard above.)
			p.yieldUnknown(ref, expr)
		}
		stampOrderingWinners(ref, p.costModel)
	}

	// Cross-candidate intersection: aggregate matches from different
	// indexes and create physical intersection plans during PLANNING.
	// Creates RecordQueryIntersectionPlan directly (not logical) because
	// there's only one intersection strategy (PK-based). If merge or
	// hash intersection is added, this should yield LogicalIntersectionExpression
	// and let ImplementIntersectionRule choose the strategy.
	//
	// Guards: candidate cap (4) and match cap (8) prevent combinatorial
	// explosion in MaximumCoverageMatches for queries with many indexes
	// (e.g., InList with 5+ candidates). hasIntersectionFinal prevents
	// re-creation when pushDataAccessTasks fires multiple times per ref.
	if len(candidates) >= 2 && len(candidates) <= 4 && !hasIntersectionFinal(ref) {
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].CandidateName() < candidates[j].CandidateName()
		})
		var allMatches []PartialMatch
		for _, candidate := range candidates {
			allMatches = append(allMatches, GetPartialMatchesForCandidate(ref, candidate)...)
		}
		// Only include matches with non-empty bound parameter prefix
		// (i.e., matches that actually restrict the scan). Zero-coverage
		// matches produce full index scans that don't help with intersection.
		//
		// Also exclude CORRELATED matches: a leg whose bound prefix references
		// an outer quantifier (a join predicate like customer_id = c.id) is not
		// independently evaluable and must not be folded into a primary-key
		// intersection. Java resolves such a predicate via the FlatMap/NLJ
		// correlation plus a residual filter, never an index intersection;
		// folding it in produces a plan whose correlated binding the
		// intersection cursor cannot evaluate, yielding 0 rows (RFC-069).
		var restrictedMatches []PartialMatch
		for _, m := range allMatches {
			// A vector scan must NEVER be a primary-key intersection arm — in
			// EITHER of its two forms:
			//   - ORDERED-STREAM (un-partitioned, RFC-156 Phase B): the residual
			//     must compose ABOVE the un-limited distance-ordered stream
			//     (Limit → Filter → ordered scan); folding it into the SELF-LIMITING
			//     intersection combinator would self-limit the scan BELOW the
			//     residual, the very thing that phase forbids.
			//   - SELF-LIMITING (partitioned per-partition top-k, RFC-046): the scan
			//     emits (partition, distance) order, which is NOT primary-key-
			//     monotonic, so the pk-keyed sorted-merge (merge_cursor.go max-key/
			//     advance) would drop rows whose distance rank disagrees with their
			//     pk order (wrong rows for k>1). The safe shape is a Filter above the
			//     un-intersected scan (compensationSafeForYield's partition-residual
			//     exception, residualIsPartitionContiguous).
			// Both reduce to the same invariant — a distance-ordered scan cannot be a
			// pk-keyed intersection leg — so exclude ALL vector candidates here, the
			// single home for the rule (RFC-167 Phase 4).
			if _, ok := m.GetMatchCandidate().(*VectorIndexScanMatchCandidate); ok {
				continue
			}
			if hasRestrictedScan(m) && !matchBoundPrefixIsCorrelated(m) {
				restrictedMatches = append(restrictedMatches, m)
			}
		}
		if len(restrictedMatches) >= 2 && len(restrictedMatches) <= 8 {
			bestMatches := MaximumCoverageMatches(restrictedMatches, requestedOrderings, p.ctx)
			if len(bestMatches) >= 2 {
				result := WithPrimaryKeyIntersector(p.ctx)(bestMatches, requestedOrderings)
				if result != nil && result.IsViable() {
					for _, expr := range result.GetExpressions() {
						ref.InsertFinal(expr)
					}
					stampOrderingWinners(ref, p.costModel)
				}
			}
		}
	}
}

// yieldUnknown routes a data-access result by physicality, mirroring Java's
// CascadesRuleCall.yieldUnknownExpression (CascadesRuleCall.java:211-219): a
// physical RecordQueryPlan lands in the FINAL set (competes in winner selection
// immediately); a logical compensation lands in the EXPLORATORY set and is
// re-optimized by the normal ExploreExprTask loop (ImplementFilterRule,
// InComparisonToExplodeRule, …) until it yields a physical plan. This replaces
// the surgical isSimpleResidualCompensation allowlist + implementDataAccessCompensation
// arm for standalone (non-join-leg) refs (RFC-148 §3a). Called only from
// pushDataAccessTasks's standalone branch, under its match-growth re-entry guard.
func (p *Planner) yieldUnknown(ref *expressions.Reference, expr expressions.RelationalExpression) {
	if isPhysical(expr) {
		ref.InsertFinal(expr)
		return
	}
	ref.Insert(expr)
}

// compensationSafeForYield reports whether a standalone logical data-access
// compensation may be routed through yieldUnknown's exploratory re-optimization.
// It is UNSAFE (kept on the OLD InsertFinal path) when its inner scan is a vector
// top-K or aggregate scan: a residual applied AFTER the top-K / grouping changes
// the result (you would post-filter the K rows, not the underlying set), so
// re-optimizing it would mint a wrong plan. This is the remaining SAFETY guard of
// the retired isSimpleResidualCompensation allowlist; the predicate-SHAPE
// restrictions it also carried (IN-only, simple-comparison-only) — the actual rot —
// are gone, so those shapes now yield and realize.
//
// An index-only predicate (a vector DistanceRank that no index serves) is NOT
// guarded here anymore: that property is handled outside this function. Java's
// `ImplementFilterRule` `!isIndexOnly()` matcher gate (rule_implement_filter.go)
// stops THAT producer from building such a physical filter, and the legitimate
// vector scan is consumed by the partial-match re-trigger in TransformExprTask
// (the Java getNewPartialMatches() reaction). But Go has OTHER physical-filter
// builders the gate does not cover (ImplementSimpleSelectRule, the NLJ residual
// builder, ImplementIndexScanRule), so the catch-all validateNoIndexOnlyResidual
// backstop in Plan() is RETAINED — it, not this function, is the authority that
// rejects an index-only physical residual (pinned by
// TestVectorPlan_MetricMismatchInJoinDoesNotLeak). Do NOT remove that net until
// every such builder is gated/retired (TODO follow-up).
// Sentinels: TestVectorPlan_QualifyPlansToVectorScan (must still plan) +
// TestFDB_VectorSearch_MultiPartition_TrailingEqualityResidual (must stay
// unplannable, via the inner-scan guard).
func compensationSafeForYield(expr expressions.RelationalExpression) bool {
	// Only a plain residual FILTER is a yield candidate — byte-identical to the OLD
	// isSimpleResidualCompensation's leading `f, ok := expr.(*LogicalFilterExpression);
	// if !ok { return false }`. A non-filter compensation (a SelectExpression with
	// result compensation / pulled-up quantifiers from ForMatchCompensation.ApplyAllNeeded,
	// a projection over a vector scan, …) stays on the OLD InsertFinal path. Without this
	// top-level reject, such shapes would skip every guard below and fall through to
	// "safe" → yieldUnknown, re-optimizing an unsafe residual the allowlist refused.
	f, ok := expr.(*expressions.LogicalFilterExpression)
	if !ok {
		return false
	}
	// A residual filter with no predicates is not a yield candidate (byte-identical
	// to the OLD isSimpleResidualCompensation's `if len(preds) == 0 { return false }`;
	// ForMatchCompensation.ApplyAllNeeded never produces one, but match the allowlist).
	if len(f.GetPredicates()) == 0 {
		return false
	}
	local := make(map[values.CorrelationIdentifier]struct{}, len(f.GetQuantifiers()))
	for _, q := range f.GetQuantifiers() {
		local[q.GetAlias()] = struct{}{}
	}
	for _, q := range f.GetQuantifiers() {
		cref := q.GetRangesOver()
		if cref == nil {
			continue
		}
		for _, m := range cref.AllMembers() {
			switch v := m.(type) {
			case *physicalVectorIndexScanWrapper:
				// A residual applied AFTER a self-limiting top-k vector scan
				// changes the result (you would post-filter the K rows, not the
				// underlying set) — UNSAFE, keep on the OLD InsertFinal path.
				// But a residual over an ORDERED-STREAM scan (RFC-156 Phase B) is
				// SAFE and CORRECT: the scan emits its full re-ranked horizon in
				// distance order, so a Filter culls non-matching rows BEFORE the
				// Limit(k) above takes k — the VBASE "filter-during-traversal"
				// shape (Limit → Filter → ordered scan). Route it through
				// yieldUnknown so ImplementFilterRule realizes the physical Filter
				// over the ordered scan (the residual is non-index-only; the
				// index-only distance marker lives only inside the scan binding).
				//
				// SELF-LIMITING (partitioned) exception: a residual over the
				// PARTITION-key columns, contiguous immediately after the bound
				// equality prefix, is ALSO safe over a self-limiting per-partition
				// top-k scan. Such a filter selects WHOLE partitions (drops entire
				// regions ≤ 'r1'), never within-partition rows, so the per-partition
				// top-k the maintainer already enforced is preserved: survivors are
				// exactly top-k per surviving partition. This yields the correct
				// Filter(region>r1) → VectorScan(self-limiting) plan instead of the
				// pk-keyed intersection (which drops rows for k>1 because the vector
				// cursor delivers distance order, not pk order). The contiguity check
				// keeps a leading-column-unbound residual (region='r1', zone unbound)
				// unplannable — a non-contiguous residual is out of scope (RFC-046).
				if v.plan == nil {
					return false
				}
				if !v.plan.IsOrderedStream() && !residualIsPartitionContiguous(f, v.plan) {
					return false
				}
			case *physicalAggregateIndexWrapper:
				return false
			}
		}
	}
	// Bound-prefix correlations carried by the compensation's own scan probe(s) —
	// the outer aliases the probe already feeds (see the OUTER-correlation guard
	// below). Computed once: the data-access scan hides these at
	// GetCorrelatedToWithoutChildren, so they are recovered from the scan's
	// ComparisonRanges directly.
	probeCorr := compensationProbeCorrelations(f)
	for _, pred := range f.GetPredicates() {
		// ROT-FIX (RFC-150, post-B1a): the predicate-SHAPE restriction the OLD
		// isSimpleResidualCompensation carried (ComparisonPredicate-only, non-IN) is
		// RETIRED. A compound/OR or IN residual now yields through yieldUnknown and
		// re-optimizes to an index plan instead of silently degrading to a full scan.
		// This was unsafe in Phase 1 only because materializing such a residual on a
		// partition-SUBSEL join leg produced a nil-inner
		// Fetch shell that the NLJ embedded → Fetch(<nil>) / 0 rows. B1a (nil-safe
		// join-child selection, RFC-150 Phase 2a) closed that, so the materialized leg
		// filter is no longer a degenerate winner. The outer-correlation +
		// vector/aggregate-inner SAFETY guards remain.
		//
		// NOTE: an index-only predicate (vector DistanceRank) is NO LONGER guarded
		// here. The Java !isIndexOnly() ImplementFilterRule gate (rule_implement_filter.go)
		// is now the single structural authority: such a residual, even if yielded,
		// cannot be realized to a physical filter, so it stays logical and surfaces the
		// clean UnplannableIndexOnlyResidualError (planner.go Plan()). Guarding it here
		// too would be a redundant second authority for the same property.
		//
		// OUTER-correlation guard (M2 0-row safety, NOT shape rot). A residual
		// correlated to a non-local (OUTER) quantifier belongs at the JOIN, not a
		// standalone leg filter. The bound-prefix correlation signal
		// (matchBoundPrefixIsCorrelated) only inspects the PREFIX, so a leg whose
		// correlation lives in the RESIDUAL — e.g. an unindexed `t.fk = o.id` alongside
		// an indexed `t.k = 5` — is invisible to it; realizing such a compensation as a
		// physical leg filter severs the join's correlation feed → Fetch(<nil>) / 0 rows
		// (the PR-#201 shape). This guard remains as defense-in-depth even with B1's
		// task-graph invariant in place (an architectural-review condition). Query-parameter
		// ConstantObjectValue aliases are execution constants (not row correlations), so
		// subtract them first.
		corr := predicates.GetCorrelatedToOfPredicate(pred)
		deletePredicateConstantObjectAliases(pred, corr)
		for alias := range corr {
			if _, isLocal := local[alias]; isLocal {
				continue
			}
			// A residual correlated to an OUTER alias is SAFE when the
			// compensation's own bound-prefix SCAN is already correlated to that
			// same alias: the probe establishes the join's correlation feed (e.g.
			// the inner U-leg `Scan(U,[id=t.fk])` is a T-driven PK probe), so this
			// residual (`u.c = t.a`) is a SECONDARY filter on the already-bound
			// probe, not the severed primary join key. This is the inverse of the
			// PR-#201 shape, where the join key itself lives in the residual
			// (`t.fk = o.id` over a constant-bound `Scan(T,[k=5])`, whose probe
			// carries NO correlation) — there `o ∉ probeCorr` so the reject stands.
			// Without this, the data-access path can never produce the cheap
			// correlated index-nested-loop inner whenever a second, non-sargable
			// cross-correlation predicate rides along (the probe-fed-residual
			// case: it drives the U full-scan O(N×M) instead of the T-driven
			// U-PK-probe O(N)).
			if _, fedByProbe := probeCorr[alias]; fedByProbe {
				continue
			}
			return false
		}
	}
	return true
}

// residualIsPartitionContiguous reports whether every field the residual filter
// f references is a LOCAL PARTITION-key column of the vector scan, and the
// referenced columns are exactly the contiguous run of partition columns
// immediately after the scan's bound equality prefix. Such a residual selects
// WHOLE partitions, so it composes safely as a Filter above a self-limiting
// per-partition top-k scan (compensationSafeForYield): dropping entire partitions
// never disturbs the per-partition top-k the maintainer already enforced. The
// LOCAL qualifier is enforced here (the field-locality reject below) so the
// guarantee is self-contained — an outer-correlated field sharing a partition
// column's name is not misattributed.
//
// The contiguity anchor (start exactly at len(bound equality prefix)) is what
// distinguishes the plannable partition-INEQUALITY case (WHERE zone='z1' AND
// region>'r1' — bound prefix [zone], residual {region} at index 1 = boundLen 1)
// from the out-of-scope leading-column-GAP case (WHERE region='r1', zone unbound
// — bound prefix [], residual {region} at index 1 ≠ boundLen 0), which stays
// unplannable (TestFDB_VectorSearch_MultiPartition_TrailingEqualityResidual). The
// discriminator is the GAP, not mere unboundedness: a leading INEQUALITY (WHERE
// zone>'z1' — bound prefix [], residual {zone} at index 0 = boundLen 0) has no gap
// and IS admitted (LeadingInequalityResidual). A residual touching any
// non-partition column (or a non-contiguous set) is not certified here and stays
// on the OLD InsertFinal path.
func residualIsPartitionContiguous(
	f *expressions.LogicalFilterExpression,
	plan *plans.RecordQueryVectorIndexPlan,
) bool {
	partCols := plan.GetPartitionColumns()
	if len(partCols) == 0 {
		return false
	}
	colIndex := make(map[string]int, len(partCols))
	for i, c := range partCols {
		colIndex[strings.ToUpper(c)] = i
	}

	// Field-locality: every residual field must belong to THIS filter's own scan.
	// The field-name matching below is by bare name (FieldValue.Field), which is
	// blind to the correlation root (FieldValue.Child) — so an OUTER-correlated
	// field that coincidentally shares a partition column's name (e.g. a join
	// residual `outer.region = ...` over a scan partitioned by `region`) would be
	// misattributed as this scan's partition column, silently breaking the
	// whole-partition guarantee. Reject any residual correlated to a non-local
	// alias here so this function's safety is SELF-CONTAINED, not dependent on the
	// separate OUTER-correlation guard later in compensationSafeForYield (query-
	// parameter ConstantObjectValue aliases are execution constants, not row
	// correlations — subtract them first, as that guard does).
	local := make(map[values.CorrelationIdentifier]struct{}, len(f.GetQuantifiers()))
	for _, q := range f.GetQuantifiers() {
		local[q.GetAlias()] = struct{}{}
	}
	for _, pred := range f.GetPredicates() {
		corr := predicates.GetCorrelatedToOfPredicate(pred)
		deletePredicateConstantObjectAliases(pred, corr)
		for alias := range corr {
			if _, isLocal := local[alias]; !isLocal {
				return false
			}
		}
	}

	residualFields := map[string]struct{}{}
	for _, pred := range f.GetPredicates() {
		collectPredicateFieldValues(pred, residualFields)
	}
	if len(residualFields) == 0 {
		return false
	}

	// The bound equality prefix length: the leading run of non-empty EQUALITY
	// partition ranges the scan actually constrains.
	boundLen := 0
	for _, cr := range plan.GetPrefixComparisons() {
		if cr == nil || cr.IsEmpty() || cr.GetRangeType() != predicates.ComparisonRangeEquality {
			break
		}
		boundLen++
	}

	idxs := make([]int, 0, len(residualFields))
	for fld := range residualFields {
		i, ok := colIndex[strings.ToUpper(fld)]
		if !ok {
			return false // residual touches a non-partition column → not safe here
		}
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	// Contiguous run starting exactly at boundLen. A LEADING inequality on the
	// first partition column (e.g. zone>'z0', boundLen 0, residual {zone} at
	// index 0) is admitted here (0 == 0+0) and is correct: the scan fans out over
	// all partitions and the whole-partition Filter selects those with zone>'z0',
	// preserving each partition's top-k. Only a GAP before the residual (a leading
	// column neither bound nor filtered, e.g. region='r1' with zone unbound →
	// residual {region} at index 1 ≠ boundLen 0) is rejected.
	for pos, i := range idxs {
		if i != boundLen+pos {
			return false
		}
	}
	return true
}

// deletePredicateConstantObjectAliases removes ConstantObjectValue (query-parameter)
// aliases from corr — they appear in a predicate's correlation set but are execution
// constants bound at run time, not join/row correlations. Generalizes the old
// deleteConstantObjectAliases (which handled only ComparisonPredicate) to any
// predicate shape, since OR / compound residuals now reach compensationSafeForYield.
func deletePredicateConstantObjectAliases(pred predicates.QueryPredicate, corr map[values.CorrelationIdentifier]struct{}) {
	predicates.WalkPredicate(pred, func(node predicates.QueryPredicate) bool {
		var vs []values.Value
		switch p := node.(type) {
		case *predicates.ComparisonPredicate:
			vs = []values.Value{p.Operand, p.Comparison.Operand}
		case *predicates.ValuePredicate:
			vs = []values.Value{p.Value}
		}
		for _, v := range vs {
			if v == nil {
				continue
			}
			values.WalkValue(v, func(node values.Value) bool {
				if cov, ok := node.(*values.ConstantObjectValue); ok {
					delete(corr, cov.Alias)
				}
				return true
			})
		}
		return true
	})
}

func hasIntersectionFinal(ref *expressions.Reference) bool {
	for _, m := range ref.FinalMembers() {
		if IsPhysicalIntersection(m) {
			return true
		}
	}
	return false
}

// Task is the task-stack driver's unit of work. Tasks are Run
// against the planner; they may push more tasks.
type Task interface {
	Run(p *Planner)
}

// ExploreReferenceTask explores a Reference: schedules per-rule
// TransformReferenceTasks and per-member ExploreExpressionTasks.
//
// Push order (LIFO execution = reverse of push order):
//  1. SaturationCheckTask (pushed first → fires LAST)
//  2. TransformReferenceTask per rule (in reverse → fire in order)
//  3. ExploreExpressionTask per member (pushed last → fire FIRST)
//
// This ensures bottom-up exploration: children are explored before
// rules fire at this level; saturation is checked after all rules.
type ExploreReferenceTask struct {
	Ref *expressions.Reference
}

// Run pushes per-rule TransformReferenceTasks + per-member
// ExploreExpressionTasks + a SaturationCheckTask sentinel.
func (t *ExploreReferenceTask) Run(p *Planner) {
	if t.Ref == nil {
		return
	}
	if p.events != nil {
		p.events.OnExploreReference(t.Ref)
	}
	beforeCount := len(t.Ref.Members())

	// Early saturation check: if the count hasn't moved since the
	// last fully-saturated pass, skip the whole round.
	if last, seen := p.exploreCount[t.Ref.Canonical()]; seen && last == beforeCount {
		if p.events != nil {
			p.events.OnApplyRules(t.Ref, 0)
		}
		return
	}

	// Push SaturationCheckTask first → fires LAST (deepest on stack).
	p.push(&SaturationCheckTask{Ref: t.Ref, BeforeCount: beforeCount})

	// Push per-rule TransformReferenceTasks in REVERSE order so they
	// fire in FORWARD rule order (LIFO pop reverses push order).
	for i := len(p.rules) - 1; i >= 0; i-- {
		p.push(&TransformReferenceTask{Ref: t.Ref, Rule: p.rules[i]})
	}

	// Push per-member ExploreExpressionTask last → fires FIRST.
	for _, m := range t.Ref.Members() {
		p.push(&ExploreExpressionTask{Expr: m})
	}
}

// ExploreExpressionTask descends into an expression's children:
// pushes ExploreReferenceTask for each Quantifier's Reference.
type ExploreExpressionTask struct {
	Expr expressions.RelationalExpression
}

// Run pushes ExploreReferenceTask for each child Reference.
func (t *ExploreExpressionTask) Run(p *Planner) {
	if t.Expr == nil {
		return
	}
	if p.events != nil {
		p.events.OnExploreExpression(t.Expr)
	}
	for _, q := range t.Expr.GetQuantifiers() {
		if r := q.GetRangesOver(); r != nil {
			p.push(&ExploreReferenceTask{Ref: r})
		}
	}
}

// TransformReferenceTask fires a single rule against all members of
// a Reference. Per-rule granularity enables per-rule events and is
// the foundation for rule-priority scheduling.
//
// Mirrors Java's `TransformExpression` task: one rule, one (group,
// expression) pair. Our variant fires one rule against all members
// of the Reference (same observable behavior as the old monolithic
// ApplyRulesTask, just split into N tasks for N rules).
type TransformReferenceTask struct {
	Ref  *expressions.Reference
	Rule ExpressionRule
}

// Run fires the rule against all current members of the Reference.
func (t *TransformReferenceTask) Run(p *Planner) {
	if t.Ref == nil || t.Rule == nil {
		return
	}
	beforeCount := len(t.Ref.Members())
	FireExpressionRuleWithMemo(t.Rule, t.Ref, p.ctx, p.memo)
	yielded := len(t.Ref.Members()) - beforeCount
	if p.events != nil {
		p.events.OnTransformRule(t.Ref, t.Rule, yielded)
	}
}

// SaturationCheckTask is the post-rule-firing sentinel that detects
// whether a Reference grew during the current round. Fires after all
// per-rule TransformReferenceTasks for this Reference have completed.
//
// Growth → re-push ExploreReferenceTask (re-explore new shapes).
// No growth → mark saturated, push OptimizeReferenceTask.
//
// Critical correctness contract: on growth, do NOT mark saturated.
// Rules need to fire again on the new members. The fuzzer
// (FuzzPlanner_Confluence) validates this property.
type SaturationCheckTask struct {
	Ref         *expressions.Reference
	BeforeCount int
}

// Run checks for growth and either re-explores or saturates.
func (t *SaturationCheckTask) Run(p *Planner) {
	if t.Ref == nil {
		return
	}
	afterCount := len(t.Ref.Members())
	grew := afterCount - t.BeforeCount

	// Update Memo index for newly-added members. After a cross-group
	// merge (RFC-037) t.Ref may have forwarded and BeforeCount was the
	// loser's pre-merge count, so members[BeforeCount:] is only an
	// approximation of "genuinely new" members. That is harmless:
	// AddExpression routes through addParentEdge which dedups, so
	// re-indexing an already-indexed member is a no-op, and any member
	// it skips was already indexed by an earlier AddExpression or by the
	// merge's repointIndices. The slice access is safe — it is guarded by
	// grew > 0, i.e. afterCount > BeforeCount.
	if grew > 0 && p.memo != nil {
		members := t.Ref.Members()
		for _, m := range members[t.BeforeCount:] {
			p.memo.AddExpression(t.Ref, m)
		}
	}

	if p.events != nil {
		p.events.OnApplyRules(t.Ref, grew)
	}

	if grew > 0 {
		p.push(&ExploreReferenceTask{Ref: t.Ref})
		return
	}
	// No growth — Reference is saturated under the current rule set.
	p.exploreCount[t.Ref.Canonical()] = afterCount
	p.push(&OptimizeReferenceTask{Ref: t.Ref})
}

// OptimizeReferenceTask picks the cheapest member of the Reference
// and stamps it as winner. Implements Graefe 1995 §2: winners are
// stored per (Reference, PhysicalProperties) pair. The NoProperties
// winner is the cheapest plan overall; ordering-specific winners
// are the cheapest plans that provide specific orderings.
//
// Fires after SaturationCheckTask reports a Reference saturated.
type OptimizeReferenceTask struct {
	Ref *expressions.Reference
}

// Run delegates to OptimizeGroup for NoProperties (cheapest overall),
// then stamps ordering-specific winners.
func (t *OptimizeReferenceTask) Run(p *Planner) {
	if t.Ref == nil {
		return
	}
	best := p.OptimizeGroup(t.Ref, expressions.NoProperties)

	stampOrderingWinners(t.Ref, p.costModel)

	if p.events != nil {
		p.events.OnOptimizeReference(t.Ref, best)
	}
}

// stampOrderingWinners iterates physical members of a Reference and
// stamps ordering-specific winners. A member that implements
// orderingHinter and returns a known ordering gets stamped as winner
// for that ordering's PhysicalProperties key.
func stampOrderingWinners(ref *expressions.Reference, costModel func(a, b expressions.RelationalExpression) bool) {
	for _, m := range ref.AllMembers() {
		// Never stamp a nil-inner Fetch shell as a per-ordering winner: it is a
		// push-through template whose inner is resolved only at extraction, and
		// (costed without its inner) it ranks artificially cheap. The stamped-
		// winner lookup paths return it without re-checking, so a spurious
		// Fetch(<nil>) wins the ordered slot and a downstream join drives off the
		// wrong (unordered) side. Mirrors the guard on the scan-fallback paths.
		if isNilInnerFetch(m) {
			continue
		}
		h, ok := m.(orderingHinter)
		if !ok {
			continue
		}
		ord := h.HintOrdering()
		if !ord.IsKnown || len(ord.Keys) == 0 {
			continue
		}
		names := make([]string, len(ord.Keys))
		for i, k := range ord.Keys {
			if fv, ok := k.(*values.FieldValue); ok {
				names[i] = fv.Field
			} else {
				names[i] = k.Name()
			}
		}
		props := expressions.OrderingFromNameDir(names, ord.Descending)
		if props.IsEmpty() {
			continue
		}
		existing := ref.Winner(props)
		if existing == nil || costModel(m, existing) {
			ref.SetWinner(props, m)
		}
	}
}

// orderingHinter is implemented by physical wrappers that can
// declare what ordering they produce.
type orderingHinter interface {
	HintOrdering() properties.Ordering
}

// compensationProbeCorrelations returns the outer aliases that the bound prefix of
// any scan beneath the compensation filter f is correlated to (the comparands of
// its ScanComparisons). The data-access scan (scanPlanExpression / the physical
// scan wrappers) deliberately HIDES these at GetCorrelatedToWithoutChildren — the
// path can SARG a correlated join key into a bare probe — so they are recovered
// from the scan's comparison ranges, the value-level twin of D.2's
// scanComparisonCorrelations. Used by compensationSafeForYield to tell a probe-fed
// secondary residual (safe) from a severed primary-join-key residual (PR-#201).
func compensationProbeCorrelations(f *expressions.LogicalFilterExpression) map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	visited := map[expressions.RelationalExpression]struct{}{}
	var walk func(m expressions.RelationalExpression)
	walk = func(m expressions.RelationalExpression) {
		if m == nil {
			return
		}
		if _, ok := visited[m]; ok {
			return
		}
		visited[m] = struct{}{}
		if pe, ok := m.(physicalPlanExpression); ok {
			collectScanPlanCorrelations(pe.GetRecordQueryPlan(), out)
		}
		for _, q := range m.GetQuantifiers() {
			if cref := q.GetRangesOver(); cref != nil {
				for _, cm := range cref.AllMembers() {
					walk(cm)
				}
			}
		}
	}
	for _, q := range f.GetQuantifiers() {
		if cref := q.GetRangesOver(); cref != nil {
			for _, m := range cref.AllMembers() {
				walk(m)
			}
		}
	}
	return out
}

// collectScanPlanCorrelations adds the bound-prefix (ScanComparisons comparand)
// correlations of every primary or index scan in p's plan subtree into out. The
// matched data-access plan is wrapped (TypeFilter / Fetch / Covering …) above the
// bound Scan, so recurse through GetChildren to reach it.
func collectScanPlanCorrelations(p plans.RecordQueryPlan, out map[values.CorrelationIdentifier]struct{}) {
	if p == nil {
		return
	}
	switch sp := p.(type) {
	case *plans.RecordQueryScanPlan:
		for a := range scanComparisonCorrelations(sp.GetScanComparisons()) {
			out[a] = struct{}{}
		}
	case *plans.RecordQueryIndexPlan:
		for a := range scanComparisonCorrelations(sp.GetScanComparisons()) {
			out[a] = struct{}{}
		}
	}
	for _, c := range p.GetChildren() {
		collectScanPlanCorrelations(c, out)
	}
}

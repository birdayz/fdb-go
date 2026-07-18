package cascades

import (
	"fmt"
	"sort"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// Planner is the task-stack driven cascades planner.
//
// The Java equivalent is `CascadesPlanner` — a task-stack driver over
// two phases (REWRITING → PLANNING) that explores the expression DAG
// bottom-up (leaves fire rules first, ancestors after), tracks
// per-Reference exploration rounds for convergence, and fires rules at
// per-(group, expression, rule) task granularity. Plan() drives both
// phases and extracts the cost-cheapest plan via
// properties.ExtractBestPlanFromSelector.
//
// Convergence: a Reference whose member set stops growing is committed
// (Reference.CommitExploration); the stack drains; the planner returns.
// A hard cap (MaxTasks) prevents pathological non-termination from
// rule-yielding-fresh-members loops; default 100_000.
//
// The planner is single-threaded (Java's is too).
type Planner struct {
	// stack MUST be LIFO: Plan() pushes InitiatePlannerPhaseTask →
	// ExploreGroupTask/OptimizeGroupTask → ExploreExprTask/
	// TransformExprTask/TransformImplTask/OptimizeInputsTask, and
	// depends on LIFO pop order for bottom-up exploration (children
	// before parents).
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
	// selection: the scan/filter/agg rules that produce physical
	// wrappers (BatchAExpressionRules in production) fire here, with
	// full constraint and ordering information available.
	planningExpressionRules []ExpressionRule

	// MaxTasks caps the total tasks executed before the planner
	// gives up: Plan returns nil and ErrPlannerCapHit (no partial
	// result — matching Java's throw). Defaults to 100_000. Hitting
	// the cap is a strong signal of a non-terminating rule — callers
	// should report.
	MaxTasks int

	// MaxTaskQueueSize caps the task stack's depth; 0 disables (Java
	// RecordQueryPlannerConfiguration.getMaxTaskQueueSize default). Hitting
	// it returns ErrPlannerQueueCapHit — Java throws
	// RecordQueryPlanComplexityException("Maximum task queue size...").
	MaxTaskQueueSize int

	// MaxNumMatchesPerRuleCall caps the bindings one rule invocation may
	// produce; 0 disables (Java default). Hitting it returns
	// ErrPlannerRuleMatchCapHit via the task-level capErr channel.
	MaxNumMatchesPerRuleCall int

	// DisabledRules holds rule type names (fmt %T spelling) excluded from
	// selection — Java's PlannerRuleSet filtering via
	// configuration.isRuleEnabled(rule). nil/empty = all rules enabled.
	DisabledRules map[string]struct{}

	// capErr carries a complexity-guard trip raised inside a task (tasks
	// have no error channel — Java throws instead); the Plan loop checks
	// it after every task.
	capErr error

	tasksRun int

	// costModel is the comparator for OptimizeGroupTask. Defaults
	// to PlanningCostModelLess. Set to RewritingCostModelLess for the
	// REWRITING phase. Matches Java's per-phase cost model architecture.
	costModel func(a, b expressions.RelationalExpression) bool

	// stats is the optional table-level cardinality statistics. When
	// set, the cost model uses real record counts instead of the
	// default 1e6 constant.
	stats properties.StatisticsProvider

	// maxObservedExplRounds records the maximum exploration rounds any
	// Reference started this Plan() — the WS-P round-cap retirement
	// evidence (exported via MaxObservedExplorationRounds; the epoch
	// model makes maxRoundsPerRef obsolete at stage (d)).
	maxObservedExplRounds int

	// activePhase is the phase whose tasks are currently draining —
	// maintained by InitiatePlannerPhaseTask (phases run sequentially on
	// the one stack). Out-of-band re-exploration tasks scheduled through
	// the memo hook carry it.
	activePhase PlannerPhase

	// constraintMap holds ordering constraints propagated during
	// PLANNING's preorder rules. Shared across all tasks.
	constraintMap *ConstraintMap

	// dataAccessConsumed tracks, per canonical reference, the partial-match
	// count last consumed by pushDataAccessTasks's standalone (yieldUnknown)
	// path — the RFC-148 §3c re-entry/termination guard. Re-consumption runs
	// only when the match set grows. Reset per Plan() run.
	dataAccessConsumed map[*expressions.Reference]int

	// exprRuleIdx / implRuleIdx bucket each phase's rules by their
	// matcher's typed root operator (ruleIndex), so ExploreExprTask only
	// pushes transform tasks for rules that can possibly match an
	// expression. Built lazily per phase; reset per Plan() run so a
	// reconfigured planner (DisabledRules, With*Rules) re-indexes.
	exprRuleIdx map[PlannerPhase]*ruleIndex[ExpressionRule]
	implRuleIdx map[PlannerPhase]*ruleIndex[ImplementationRule]
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
		costModel:          PlanningCostModelLess,
		MaxTasks:           100_000,
	}
}

// Memo returns the planner's Memo structure. Available after Plan
// has been called (returns nil before that).
func (p *Planner) Memo() *Memo {
	return p.memo
}

// BestMember returns the OPTIMIZE-chosen best member for `ref`,
// or nil if the Reference wasn't optimized.
func (p *Planner) BestMember(ref *expressions.Reference) expressions.RelationalExpression {
	if ref == nil {
		return nil
	}
	return ref.Winner()
}

// HasBestMember reports whether a winner exists for `ref`.
func (p *Planner) HasBestMember(ref *expressions.Reference) bool {
	if ref == nil {
		return false
	}
	return ref.HasWinner()
}

// OrderedChildWinner returns the cheapest physical member of childRef whose
// derived rich ordering satisfies sortExpr's keys, or nil when none does
// (the sort must then be materialised). Implements the sort-elision hook of
// properties.ExtractBestPlanFromSelector: satisfaction runs on the full
// Value + four-state sort-order representation (RichOrdering.Satisfies), so
// an ASC_NULLS_LAST sort is never elided against a natural-order ASC scan.
func (p *Planner) OrderedChildWinner(sortExpr *expressions.LogicalSortExpression, childRef *expressions.Reference) expressions.RelationalExpression {
	ro := sortExpressionToRequestedOrdering(sortExpr)
	if ro.IsPreserve() {
		return nil
	}
	return bestSatisfyingMember(childRef, ro, p.costModel)
}

// OrderingSourceRef reports whether expr is an order-PRESERVING wrapper
// (orderingDelegator) and returns the child group its ordering flows
// from. Implements the second half of the sort-elision seam: extraction
// must pin that group to a satisfying member when it elides a sort —
// rebuilding through the group's overall winner could relink the spine
// to a cheaper UNORDERED member after the sort is already gone.
func (p *Planner) OrderingSourceRef(expr expressions.RelationalExpression) (*expressions.Reference, bool) {
	d, ok := expr.(orderingDelegator)
	if !ok {
		return nil, false
	}
	return d.orderingSourceRef(), true
}

// TieBrokenCostLess supplies extraction's fallback GetBest with a
// TOTAL-ORDER comparator (properties.TieBrokenCostSelector): the scalar
// cost comparator ties routinely and GetBest would otherwise resolve by
// member insertion order, flipping picks across plannings.
func (p *Planner) TieBrokenCostLess(stats properties.StatisticsProvider) func(a, b expressions.RelationalExpression) bool {
	return lessWithHashTieBreak(properties.CostLessWith(stats))
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
// to finalMembers via InsertFinal: physical scan/filter/agg wrappers
// are produced during PLANNING, where constraint and ordering
// information is available. Production passes BatchAExpressionRules.
func (p *Planner) WithPlanningExpressionRules(rules []ExpressionRule) *Planner {
	p.planningExpressionRules = append(PlanningExplorationRules(), rules...)
	return p
}

// WithCostModel sets the comparator used by OptimizeGroupTask.
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

// MaxObservedExplorationRounds reports the maximum exploration rounds
// any Reference started during Plan() — round-cap retirement evidence
// (RFC-181 WS-P).
func (p *Planner) MaxObservedExplorationRounds() int { return p.maxObservedExplRounds }

// WithMaxTasks overrides the task cap. Returns p for chaining.
func (p *Planner) WithMaxTasks(n int) *Planner {
	p.MaxTasks = n
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
	// Out-of-band member growth (Absorb, raw inserts into existing
	// groups) must schedule the re-round its epoch re-arm requires —
	// dirtiness without a task is silently never explored.
	p.memo.SetReExploreScheduler(func(ref *expressions.Reference) {
		p.push(&ExploreGroupTask{Phase: p.activePhase, Ref: ref})
	})
	p.constraintMap = NewConstraintMap()
	p.dataAccessConsumed = make(map[*expressions.Reference]int)
	p.exprRuleIdx = nil
	p.implRuleIdx = nil
	p.capErr = nil

	// One task-stack drives both REWRITING and PLANNING phases.
	// InitiatePlannerPhase(REWRITING) pushes ExploreGroup + OptimizeGroup
	// for REWRITING, then chains to InitiatePlannerPhase(PLANNING).
	p.push(&InitiatePlannerPhaseTask{Phase: PhaseRewriting, RootRef: rootRef})

	for len(p.stack) > 0 {
		if p.tasksRun >= p.MaxTasks {
			return nil, p.tasksRun, ErrPlannerCapHit
		}
		if p.MaxTaskQueueSize > 0 && len(p.stack) > p.MaxTaskQueueSize {
			return nil, p.tasksRun, ErrPlannerQueueCapHit
		}
		task := p.pop()
		task.Run(p)
		p.tasksRun++
		if p.capErr != nil {
			return nil, p.tasksRun, p.capErr
		}
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

// ErrPlannerCapHit signals that Plan exited via the MaxTasks
// cap rather than convergence. Callers should treat this as a
// non-termination indicator and report.
var ErrPlannerCapHit = plannerErr("planner: MaxTasks cap hit before convergence")

// ErrPlannerQueueCapHit mirrors Java's "Maximum task queue size was exceeded"
// RecordQueryPlanComplexityException (CascadesPlanner.isTaskQueueSizeExceeded).
var ErrPlannerQueueCapHit = plannerErr("planner: MaxTaskQueueSize cap hit — task queue exceeded the configured bound")

// ErrPlannerRoundCapHit signals a Reference still inserting new exploratory
// members after the round cap — a rule-cycle divergence the memo dedup should
// have collapsed (RFC-180 I2). A planner bug indicator, never load.
var ErrPlannerRoundCapHit = plannerErr("planner: exploration round cap hit — a Reference kept producing new members after 10 rounds (rule-cycle divergence)")

// ErrPlannerRuleMatchCapHit mirrors Java's "Maximum number of matches per rule
// call has been exceeded" (CascadesPlanner.isMaxNumMatchesPerRuleCallExceeded).
var ErrPlannerRuleMatchCapHit = plannerErr("planner: MaxNumMatchesPerRuleCall cap hit — one rule invocation produced more matches than the configured bound")

// plannerErr is a string-error type local to the planner.
type plannerErr string

// Error returns the message.
func (e plannerErr) Error() string { return string(e) }

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
	var er []ExpressionRule
	var ir []ImplementationRule
	switch phase {
	case PhaseRewriting:
		er, ir = p.rules, p.rewritingImplRules
	case PhasePlanning:
		er, ir = p.planningExpressionRules, p.implementationRules
	default:
		return nil, nil
	}
	// Java's configuration.isRuleEnabled filtering (PlannerRuleSet.getRules
	// passes the predicate): a rule named in DisabledRules is never
	// selected. Keyed by the concrete type's %T spelling — the Go analog of
	// Java's rule class.
	if len(p.DisabledRules) > 0 {
		fe := er[:0:0]
		for _, r := range er {
			if _, off := p.DisabledRules[fmt.Sprintf("%T", r)]; !off {
				fe = append(fe, r)
			}
		}
		fi := ir[:0:0]
		for _, r := range ir {
			if _, off := p.DisabledRules[fmt.Sprintf("%T", r)]; !off {
				fi = append(fi, r)
			}
		}
		return fe, fi
	}
	return er, ir
}

// ruleIndexesForPhase returns the phase's rule indexes, building them from
// rulesForPhase on first use (per Plan() run — Plan resets the maps).
func (p *Planner) ruleIndexesForPhase(phase PlannerPhase) (*ruleIndex[ExpressionRule], *ruleIndex[ImplementationRule]) {
	if p.exprRuleIdx == nil {
		p.exprRuleIdx = make(map[PlannerPhase]*ruleIndex[ExpressionRule])
	}
	if p.implRuleIdx == nil {
		p.implRuleIdx = make(map[PlannerPhase]*ruleIndex[ImplementationRule])
	}
	ei, ok := p.exprRuleIdx[phase]
	ii, ok2 := p.implRuleIdx[phase]
	if !ok || !ok2 {
		er, ir := p.rulesForPhase(phase)
		ei = newRuleIndex(er)
		ii = newRuleIndex(ir)
		p.exprRuleIdx[phase] = ei
		p.implRuleIdx[phase] = ii
	}
	return ei, ii
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
// Java's TransformMatchPartition tasks. Four phases, one function
// each: collect the consumable candidates, look up the propagated
// ordering constraints, run the (growth-guarded) standalone
// consumption, then attempt the cross-candidate PK intersection.
//
// Join-leg refs take the same standalone consumption path as any other
// ref. What makes that safe is structural, not a guard here:
// OptimizeInputsTask is pushed only for PHYSICAL parent members
// (unified_tasks.go, Java CascadesPlanner.java:524), so a correlated
// leg's group is pruned to a winner only as the inner child of the
// binding physical join — never free-standing — and a correlated
// SUBSEL scan cannot be stamped a standalone winner.
// compensationSafeForYield's outer-correlation guard is
// defense-in-depth for the same property (RFC-150 §8).
func (p *Planner) pushDataAccessTasks(ref *expressions.Reference, _ expressions.RelationalExpression) {
	candidates := dataAccessCandidates(ref)
	if len(candidates) == 0 {
		return
	}

	var requestedOrderings []*RequestedOrdering
	if p.constraintMap != nil {
		if orderings, ok := Get(p.constraintMap, ref, RequestedOrderingConstraintKey); ok {
			requestedOrderings = orderings
		}
	}

	if p.shouldConsumeMatches(ref, candidates, requestedOrderings) {
		p.consumeMatchPartitions(ref, candidates, requestedOrderings)
	}
	p.pushCrossCandidateIntersection(ref, candidates, requestedOrderings)
}

// dataAccessCandidates adjusts this ref's partial matches and returns
// the match candidates the standalone data-access path may consume.
//
// The AdjustPartialMatchesForRef call absorbs candidate-side parent
// expressions (Select → MatchableSort) onto this ref's matches before
// they are consumed. PLANNING-phase matches are seeded during
// exploration, after the phase-start AdjustMatches walk, so their
// matched ordering parts (which let an index scan satisfy a requested
// ordering and eliminate an in-memory sort) are only computed here, at
// consumption time. Java's AdjustMatchRule is event-driven and fires
// on each new match; this is the Go equivalent at the data-access
// boundary.
//
// Aggregate-index candidates are dropped: they are consumed by
// AggregateDataAccessRule (which matches the GroupByExpression and
// reads the pre-aggregated value), NOT by the regular value-index
// data-access path. An aggregate index stores aggregated rows, not
// base records — matching its underlying scan here yields
// IndexScan(agg_index, [=]) which a StreamingAgg then re-aggregates,
// counting the single group row as 1 (or reading the wrong column for
// SUM). The match infra seeds these matches once the regular path is
// active; they must not be realized as record scans
// (TestFDB_AggregateIndexUsage count/sum_with_eq_filter).
func dataAccessCandidates(ref *expressions.Reference) []MatchCandidate {
	AdjustPartialMatchesForRef(ref)

	candidates := GetPartialMatchCandidatesTyped(ref)
	if len(candidates) == 0 {
		return nil
	}
	filtered := candidates[:0]
	for _, c := range candidates {
		if _, isAgg := c.(*AggregateIndexMatchCandidate); isAgg {
			continue
		}
		filtered = append(filtered, c)
	}
	return filtered
}

// shouldConsumeMatches is the re-entry / termination guard
// (RFC-148 §3c): yieldUnknown routes a logical compensation into the
// EXPLORATORY set, so the enclosing ExploreGroupTask re-explores it
// and re-enters pushDataAccessTasks on this ref. Run the standalone
// consumption ONLY when the partial-match set has GROWN since the last
// consumption — a growth key, NOT a "consumed-ever" gate, which would
// drop matches seeded mid-exploration (AdjustPartialMatchesForRef
// seeds across rounds).
//
// The guard applies ONLY when there is NO requested ordering: an
// ordering can be propagated into this ref AFTER a first consumption
// at unchanged match count, and the sort-eliminating ordered scan it
// unlocks must not be suppressed (a missed ordered scan degrades the
// plan to the in-memory-sort fallback — an extra-Sort plan-shape
// regression, with unbounded materialization on large inputs). With an
// ordering present we re-consume every round; convergence is bounded
// by Insert dedup + the 10-round cap (RFC-148 §3c addendum). The
// cross-candidate intersection keeps its own hasIntersectionFinal
// guard.
func (p *Planner) shouldConsumeMatches(ref *expressions.Reference, candidates []MatchCandidate, requestedOrderings []*RequestedOrdering) bool {
	if len(requestedOrderings) > 0 {
		return true
	}
	totalMatches := 0
	for _, c := range candidates {
		totalMatches += len(GetPartialMatchesForCandidate(ref, c))
	}
	key := ref.Canonical()
	last, seen := p.dataAccessConsumed[key]
	p.dataAccessConsumed[key] = totalMatches
	return !(seen && totalMatches <= last)
}

// consumeMatchPartitions realizes each candidate's match partition
// into data-access expressions and routes every result by safety:
// physical plans and SAFE logical compensations go through
// yieldUnknown; UNSAFE logical compensations go to InsertFinal.
func (p *Planner) consumeMatchPartitions(ref *expressions.Reference, candidates []MatchCandidate, requestedOrderings []*RequestedOrdering) {
	for _, candidate := range candidates {
		matches := GetPartialMatchesForCandidate(ref, candidate)
		if len(matches) == 0 {
			continue
		}
		exprs := DataAccessForMatchPartition(requestedOrderings, matches, p.ctx, nil)
		for _, expr := range exprs {
			// An UNSAFE logical compensation — one whose inner scan is a vector
			// top-K / aggregate — is NOT narrowable by a post-filter (a residual
			// applied after the top-K / grouping changes the result). It goes to
			// InsertFinal: it stays logical, and the query correctly fails to plan
			// when no physical alternative exists rather than being re-optimized
			// into a wrong plan. Index-only residuals are handled elsewhere — the
			// ImplementFilterRule !isIndexOnly() gate plus the
			// validateNoIndexOnlyResidual backstop in Plan() (RFC-151 §5).
			if !isPhysical(expr) && !compensationSafeForYield(expr) {
				// NO exploration task: an unsafe final exists only so the
				// query FAILS to plan when no alternative lands — running
				// rules on it would physicalize the top-K-before-filter
				// shape this arm exists to prevent (silent wrong rows;
				// the finals loop's PLANNING gate skips it for the same
				// reason).
				ref.InsertFinal(expr)
				continue
			}
			// Physical plan → final set (competes now). A SAFE logical compensation →
			// exploratory set, re-optimized by the normal ExploreExprTask loop
			// (ImplementFilterRule / InComparisonToExplodeRule / …) until it yields a
			// RecordQueryPlan (Java CascadesRuleCall.yieldUnknownExpression;
			// RFC-148 §3a). The re-entry this creates is bounded by
			// shouldConsumeMatches' match-growth guard.
			p.yieldUnknown(ref, expr)
		}
	}
}

// pushCrossCandidateIntersection aggregates matches from different
// indexes and creates physical intersection plans during PLANNING.
// Creates RecordQueryIntersectionPlan directly (not logical) because
// there's only one intersection strategy (PK-based). If merge or
// hash intersection is added, this should yield LogicalIntersectionExpression
// and let ImplementIntersectionRule choose the strategy.
//
// Guards: candidate cap (4) and match cap (8) prevent combinatorial
// explosion in MaximumCoverageMatches for queries with many indexes
// (e.g., InList with 5+ candidates). hasIntersectionFinal prevents
// re-creation when pushDataAccessTasks fires multiple times per ref.
func (p *Planner) pushCrossCandidateIntersection(ref *expressions.Reference, candidates []MatchCandidate, requestedOrderings []*RequestedOrdering) {
	if len(candidates) < 2 || len(candidates) > 4 || hasIntersectionFinal(ref) {
		return
	}
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
		//     exception, residualSelectsWholePartitions).
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
	if len(restrictedMatches) < 2 || len(restrictedMatches) > 8 {
		return
	}
	// INTERSECTION-LOCAL duplicate resolution: a raw seeded match and
	// its ADJUSTED twin (AdjustPartialMatchesForRef re-anchors it at the
	// candidate's MatchableSort parent, ATTACHING the
	// matchedOrderingParts) coexist by design under different candidate
	// refs — the main data-access path builds scans from the raw one,
	// but here the pair must collapse: keeping both feeds the intersector
	// a self-intersection of the same index, and taking the raw one
	// starves the PK-merge compatibility gate of its ordering parts.
	// Keep, per (candidate, bound comparisons), only the match with the
	// MOST matched ordering parts. This collapse is LOCAL to the
	// intersection path — MaximumCoverageMatches stays untouched for the
	// scan-building consumers.
	restrictedMatches = collapseAdjustedTwins(restrictedMatches)
	if len(restrictedMatches) < 2 {
		return
	}
	bestMatches := MaximumCoverageMatches(restrictedMatches, requestedOrderings, p.ctx)
	if len(bestMatches) < 2 {
		return
	}
	result := WithPrimaryKeyIntersector(p.ctx)(bestMatches, requestedOrderings)
	if result == nil || !result.IsViable() {
		return
	}
	for _, expr := range result.GetExpressions() {
		if ref.InsertFinal(expr) {
			if isPhysical(expr) {
				p.push(&OptimizeInputsTask{Phase: PhasePlanning, Ref: ref, Expr: expr})
			}
			p.push(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: expr})
		}
	}
}

// collapseAdjustedTwins keeps, per (candidate, bound-parameter
// comparisons), only the partial match with the most matched ordering
// parts (see the call site for why). Order-preserving for the kept
// representatives.
func collapseAdjustedTwins(matches []PartialMatch) []PartialMatch {
	var out []PartialMatch
	for _, m := range matches {
		pmi, ok := m.(*PartialMatchImpl)
		if !ok {
			out = append(out, m)
			continue
		}
		prefix := pmi.GetBoundParameterPrefixMap()
		dup := -1
		for i, e := range out {
			epmi, eok := e.(*PartialMatchImpl)
			if !eok || e.GetMatchCandidate() != m.GetMatchCandidate() {
				continue
			}
			if equalParameterPrefixMaps(epmi.GetBoundParameterPrefixMap(), prefix) {
				dup = i
				break
			}
		}
		if dup < 0 {
			out = append(out, m)
			continue
		}
		if len(m.GetMatchInfo().GetMatchedOrderingParts()) > len(out[dup].GetMatchInfo().GetMatchedOrderingParts()) {
			out[dup] = m
		}
	}
	return out
}

// yieldUnknown routes a data-access result by physicality, mirroring Java's
// CascadesRuleCall.yieldUnknownExpression (CascadesRuleCall.java:211-219): a
// physical RecordQueryPlan lands in the FINAL set (competes in winner selection
// immediately); a logical compensation lands in the EXPLORATORY set and is
// re-optimized by the normal ExploreExprTask loop (ImplementFilterRule,
// InComparisonToExplodeRule, …) until it yields a physical plan (RFC-148 §3a).
// Called only from pushDataAccessTasks, under its match-growth re-entry guard.
func (p *Planner) yieldUnknown(ref *expressions.Reference, expr expressions.RelationalExpression) {
	if isPhysical(expr) {
		if ref.InsertFinal(expr) {
			// Insert-driven exploration (Java executeRuleCall
			// :1064-1070): under epoch convergence the group does NOT
			// re-round on member growth, so every insert site owns its
			// new expression's tasks.
			p.push(&OptimizeInputsTask{Phase: PhasePlanning, Ref: ref, Expr: expr})
			p.push(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: expr})
		}
		return
	}
	if ref.Insert(expr) {
		p.push(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: expr})
	}
}

// compensationSafeForYield reports whether a standalone logical data-access
// compensation may be routed through yieldUnknown's exploratory re-optimization.
// It is UNSAFE (routed to InsertFinal instead) when its inner scan is a vector
// top-K or aggregate scan: a residual applied AFTER the top-K / grouping changes
// the result (you would post-filter the K rows, not the underlying set), so
// re-optimizing it would mint a wrong plan.
//
// An index-only predicate (a vector DistanceRank that no index serves) is
// deliberately NOT guarded here. Java's `ImplementFilterRule` `!isIndexOnly()`
// matcher gate (rule_implement_filter.go) stops THAT producer from building such
// a physical filter, and the legitimate vector scan is consumed by the
// partial-match re-trigger in TransformExprTask (the Java getNewPartialMatches()
// reaction). But Go has OTHER physical-filter builders the gate does not cover
// (ImplementSimpleSelectRule, the NLJ residual builder, ImplementIndexScanRule),
// so the catch-all validateNoIndexOnlyResidual backstop in Plan() is RETAINED —
// it, not this function, is the authority that rejects an index-only physical
// residual (pinned by TestVectorPlan_MetricMismatchInJoinDoesNotLeak). Do NOT
// remove that net until every such builder is gated/retired (RFC-151 §5;
// registered as RFC-175 §2 C4).
// Sentinels: TestVectorPlan_QualifyPlansToVectorScan (must still plan) +
// TestFDB_VectorSearch_MultiPartition_TrailingEqualityResidual (must stay
// unplannable, via the inner-scan guard).
func compensationSafeForYield(expr expressions.RelationalExpression) bool {
	// Only a plain residual FILTER is a yield candidate. A non-filter compensation
	// (a SelectExpression with result compensation / pulled-up quantifiers from
	// ForMatchCompensation.ApplyAllNeeded, a projection over a vector scan, …)
	// goes to InsertFinal. Without this top-level reject, such shapes would skip
	// every guard below and fall through to "safe" → yieldUnknown, re-optimizing
	// an unsafe residual.
	f, ok := expr.(*expressions.LogicalFilterExpression)
	if !ok {
		return false
	}
	// A residual filter with no predicates is not a yield candidate
	// (ForMatchCompensation.ApplyAllNeeded never produces one; reject defensively).
	if len(f.GetPredicates()) == 0 {
		return false
	}
	return compensationInnerScanSafe(f) && compensationResidualCorrelationSafe(f)
}

// compensationInnerScanSafe is the inner-scan half of
// compensationSafeForYield: it rejects a residual filter whose inner
// scan is a vector top-k or aggregate scan.
//
// A residual applied AFTER a self-limiting top-k vector scan changes
// the result (you would post-filter the K rows, not the underlying
// set) — UNSAFE, route to InsertFinal. But a residual over an
// ORDERED-STREAM scan (RFC-156 Phase B) is SAFE and CORRECT: the scan
// emits its full re-ranked horizon in distance order, so a Filter
// culls non-matching rows BEFORE the Limit(k) above takes k — the
// VBASE "filter-during-traversal" shape (Limit → Filter → ordered
// scan). Route it through yieldUnknown so ImplementFilterRule realizes
// the physical Filter over the ordered scan (the residual is
// non-index-only; the index-only distance marker lives only inside the
// scan binding).
//
// SELF-LIMITING (partitioned) exception: a residual over ONLY the
// PARTITION-key columns is ALSO safe over a self-limiting
// per-partition top-k scan. Such a filter selects WHOLE partitions
// (drops entire regions), never within-partition rows, so the
// per-partition top-k the maintainer already enforced is preserved:
// survivors are exactly top-k per surviving partition. This yields the
// correct Filter → VectorScan(self-limiting) plan instead of the
// pk-keyed intersection (which drops rows for k>1 because the vector
// cursor delivers distance order, not pk order). Positions in the
// partition prefix do not matter for the safety property — see
// residualSelectsWholePartitions.
func compensationInnerScanSafe(f *expressions.LogicalFilterExpression) bool {
	for _, q := range f.GetQuantifiers() {
		cref := q.GetRangesOver()
		if cref == nil {
			continue
		}
		for _, m := range cref.AllMembers() {
			switch v := m.(type) {
			case *physicalVectorIndexScanWrapper:
				if v.plan == nil {
					return false
				}
				if !v.plan.IsOrderedStream() && !residualSelectsWholePartitions(f, v.plan) {
					return false
				}
			case *physicalAggregateIndexWrapper:
				return false
			}
		}
	}
	return true
}

// compensationResidualCorrelationSafe is the OUTER-correlation half of
// compensationSafeForYield (0-row safety). A residual correlated to a
// non-local (OUTER) quantifier belongs at the JOIN, not a standalone
// leg filter. The bound-prefix correlation signal
// (matchBoundPrefixIsCorrelated) only inspects the PREFIX, so a leg
// whose correlation lives in the RESIDUAL — e.g. an unindexed
// `t.fk = o.id` alongside an indexed `t.k = 5` — is invisible to it;
// realizing such a compensation as a physical leg filter severs the
// join's correlation feed → Fetch(<nil>) / 0 rows. Kept as
// defense-in-depth even with the task-graph invariant in place
// (RFC-150 §8). Query-parameter ConstantObjectValue aliases are
// execution constants (not row correlations), so they are subtracted
// first.
//
// No predicate-SHAPE restriction here: compound/OR and IN residuals
// yield through yieldUnknown and re-optimize to an index plan
// (RFC-150 §8) — the guard is correlation SAFETY, not shape.
// Index-only predicates (vector DistanceRank) are likewise not guarded
// here — the !isIndexOnly() ImplementFilterRule gate is the single
// structural authority for that property; a second guard here would be
// a redundant second authority (RFC-151 §5).
func compensationResidualCorrelationSafe(f *expressions.LogicalFilterExpression) bool {
	local := make(map[values.CorrelationIdentifier]struct{}, len(f.GetQuantifiers()))
	for _, q := range f.GetQuantifiers() {
		local[q.GetAlias()] = struct{}{}
	}
	// Bound-prefix correlations carried by the compensation's own scan probe(s) —
	// the outer aliases the probe already feeds (see the EXCEPTION below).
	// Computed once: the data-access scan hides these at
	// GetCorrelatedToWithoutChildren, so they are recovered from the scan's
	// ComparisonRanges directly.
	probeCorr := compensationProbeCorrelations(f)
	for _, pred := range f.GetPredicates() {
		corr := predicates.GetCorrelatedToOfPredicate(pred)
		deletePredicateConstantObjectAliases(pred, corr)
		for alias := range corr {
			if _, isLocal := local[alias]; isLocal {
				continue
			}
			// EXCEPTION: a residual correlated to an OUTER alias is SAFE when the
			// compensation's own bound-prefix SCAN is already correlated to that
			// same alias — the probe establishes the join's correlation feed
			// (e.g. the inner U-leg `Scan(U,[id=t.fk])` is a T-driven PK probe),
			// so this residual (`u.c = t.a`) is a SECONDARY filter on the
			// already-bound probe, not the severed primary join key. When the
			// join key itself lives in the residual (`t.fk = o.id` over a
			// constant-bound `Scan(T,[k=5])`, whose probe carries NO correlation)
			// `o ∉ probeCorr` and the reject stands. Without the exception, the
			// data-access path could never produce the cheap correlated
			// index-nested-loop inner whenever a second, non-sargable
			// cross-correlation predicate rides along (RFC-150 §8).
			if _, fedByProbe := probeCorr[alias]; fedByProbe {
				continue
			}
			return false
		}
	}
	return true
}

// residualSelectsWholePartitions reports whether every field the residual
// filter f references is a LOCAL PARTITION-key column of the vector scan.
// Such a residual selects WHOLE partitions — it can only admit or drop an
// entire (zone, region, …) partition, never rows WITHIN one — so it
// composes safely as a Filter above a self-limiting per-partition top-k
// scan (compensationSafeForYield): dropping entire partitions never
// disturbs the per-partition top-k the maintainer already enforced. The
// LOCAL qualifier is enforced here (the field-locality reject below) so
// the guarantee is self-contained — an outer-correlated field sharing a
// partition column's name is not misattributed.
//
// Positions do NOT matter for safety: a bound-prefix GAP (WHERE
// region='r1' with zone unbound — the scan fans out over every partition
// and the Filter keeps whole region='r1' partitions,
// TestFDB_VectorSearch_MultiPartition_TrailingEqualityResidual), a
// leading inequality (zone>'z1'), and the contiguous post-prefix run
// (zone='z1' AND region>'r1') all commute identically. The former
// contiguity anchor was an RFC-046 SCOPE limit, not a correctness
// discriminator; the epoch convergence materializes the gap case's
// fan-out plan and its exact-rows pin proves it. What is NOT certified
// here — and stays a fail-to-plan sentinel — is any residual touching a
// NON-partition column (it filters within partitions, so top-K-then-
// filter would silently drop rows: the unsafe-residual pin's shape).
func residualSelectsWholePartitions(
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

	for fld := range residualFields {
		if _, ok := colIndex[strings.ToUpper(fld)]; !ok {
			return false // residual touches a non-partition column → not safe here
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
		if hasIntersectionWithin(m, 3) {
			return true
		}
	}
	return false
}

// hasIntersectionWithin reports whether expr IS a physical intersection or
// wraps one within the given depth. A compensated intersection (the
// createIntersectionAndCompensation port) is a LogicalFilterExpression or
// SelectExpression OVER the intersection wrapper — up to two wrapper levels
// (filter compensation + result compensation), hence the callers' depth of 3
// counting the intersection itself — so the naked-physical check alone would
// miss it and pushCrossCandidateIntersection would rebuild the same
// intersection on every pushDataAccessTasks pass.
func hasIntersectionWithin(expr expressions.RelationalExpression, depth int) bool {
	if IsPhysicalIntersection(expr) {
		return true
	}
	if depth == 0 {
		return false
	}
	for _, q := range expr.GetQuantifiers() {
		child := q.GetRangesOver()
		if child == nil {
			continue
		}
		for _, m := range child.AllMembers() {
			if hasIntersectionWithin(m, depth-1) {
				return true
			}
		}
	}
	return false
}

// Task is the task-stack driver's unit of work. Tasks are Run
// against the planner; they may push more tasks.
type Task interface {
	Run(p *Planner)
}

// compensationProbeCorrelations returns the outer aliases that the bound prefix of
// any scan beneath the compensation filter f is correlated to (the comparands of
// its ScanComparisons). The data-access scan (scanPlanExpression / the physical
// scan wrappers) deliberately HIDES these at GetCorrelatedToWithoutChildren — the
// path can SARG a correlated join key into a bare probe — so they are recovered
// from the scan's comparison ranges (scanComparisonCorrelations). Used by
// compensationSafeForYield to tell a probe-fed secondary residual (safe) from a
// severed primary-join-key residual (RFC-150 §8).
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

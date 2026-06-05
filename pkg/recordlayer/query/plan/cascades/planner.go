package cascades

import (
	"sort"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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

	// No member satisfies props. The Graefe approach would add an
	// enforcer (sort) here. For now, return nil — the caller handles
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
	// Reject a final plan that would evaluate an index-only predicate (e.g. a
	// vector K-NN DistanceRank) as a residual filter — not executable, see
	// validateNoIndexOnlyResidual. Surfaces a clean planning error instead of an
	// execution-time panic when no index serves the index-only predicate.
	if err := validateNoIndexOnlyResidual(plan); err != nil {
		return nil, p.tasksRun, err
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

	// A ref with any correlated match is a JOIN LEG (the surrounding join binds a
	// parameter into its scan, e.g. `customer_id = ac.id`). Its data-access
	// compensations are meant to be consumed by the join, not stand alone — realizing
	// one as a final leg winner severs the join's correlation feed (the filter wins at
	// the leg but the join no longer drives it) → 0 rows. So never materialize a
	// compensation on a join-leg ref; only on a standalone single-source ref.
	refIsJoinLeg := refHasCorrelatedMatch(ref)

	for _, candidate := range candidates {
		matches := GetPartialMatchesForCandidate(ref, candidate)
		if len(matches) == 0 {
			continue
		}
		exprs := DataAccessForMatchPartition(requestedOrderings, matches, p.ctx, nil)
		for _, expr := range exprs {
			inserted := ref.InsertFinal(expr)
			// A compensated data-access result may be a LOGICAL expression — a residual
			// LogicalFilter over the physical scan (Java Compensation.apply), produced when
			// an index match leaves a non-sargable residual (e.g. an equality on an indexed
			// column plus a `>` on a non-indexed one). Inserted as-is it stays non-physical
			// and loses criterion #1 (physical beats non-physical) to a full scan, so the
			// index scan is silently dropped. Realize ONLY the unambiguously-safe simple
			// single-source residual (isSimpleResidualCompensation) on a non-join-leg ref as
			// a physical filter here; every other compensation (IN, correlated, index-only,
			// join-leg) is left logical and handled by the existing flow. Guarded on
			// `inserted` so the convergent fixpoint terminates (pushDataAccessTasks re-yields
			// equal compensations, which dedup).
			if inserted && !refIsJoinLeg && !isPhysical(expr) && isSimpleResidualCompensation(expr) {
				p.implementDataAccessCompensation(ref, expr)
			}
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

// refHasCorrelatedMatch reports whether any partial match on the reference binds a scan
// parameter that is correlated to an outer quantifier — the signature of a JOIN LEG (the
// surrounding join drives a value into this scan). Such a ref's data-access compensations
// must be consumed by the join, never materialized as a standalone leg winner.
func refHasCorrelatedMatch(ref *expressions.Reference) bool {
	for _, cand := range GetPartialMatchCandidatesTyped(ref) {
		for _, m := range GetPartialMatchesForCandidate(ref, cand) {
			if matchBoundPrefixIsCorrelated(m) {
				return true
			}
		}
	}
	return false
}

// isSimpleResidualCompensation reports whether a logical compensation is a PLAIN residual
// filter safe to realize as a standalone physical filter over its (physical) inner scan:
// a LogicalFilterExpression every predicate of which is a simple non-IN, non-index-only,
// non-correlated ComparisonPredicate, sitting over an uncorrelated, narrowable
// (value-index / primary / fetch — NOT vector top-K or aggregate) inner scan. Everything
// else stays logical and is handled by the existing flow.
func isSimpleResidualCompensation(expr expressions.RelationalExpression) bool {
	f, ok := expr.(*expressions.LogicalFilterExpression)
	if !ok {
		return false
	}
	preds := f.GetPredicates()
	if len(preds) == 0 {
		return false
	}
	local := make(map[values.CorrelationIdentifier]struct{}, len(f.GetQuantifiers()))
	for _, q := range f.GetQuantifiers() {
		local[q.GetAlias()] = struct{}{}
	}
	// The inner scan must NOT be correlated to an outer quantifier, and must be
	// NARROWABLE by a post-filter. A correlated scan (e.g. a join inner
	// `IndexScan(orders, customer_id = ac.id)`) is bound by the surrounding join;
	// realizing the residual filter standalone over it severs that binding (the filter
	// wins at the leg ref but the join no longer feeds it the correlated row) → 0 rows.
	// A vector top-K scan (`rank<=K`) or an aggregate scan is NOT narrowable: a residual
	// applied AFTER the top-K / grouping changes the result, so such a residual is
	// semantically invalid and the query must stay unplannable (the trailing-partition-
	// equality vector case) rather than be realized as a wrong plan. A value-index /
	// primary / fetch scan returns a superset the residual can correctly filter.
	for _, q := range f.GetQuantifiers() {
		cref := q.GetRangesOver()
		if cref == nil {
			continue
		}
		for alias := range cref.GetCorrelatedTo() {
			if _, isLocal := local[alias]; !isLocal {
				return false
			}
		}
		for _, m := range cref.AllMembers() {
			switch m.(type) {
			case *physicalVectorIndexScanWrapper, *physicalAggregateIndexWrapper:
				return false
			}
		}
	}
	for _, pred := range preds {
		cp, ok := pred.(*predicates.ComparisonPredicate)
		if !ok {
			return false
		}
		if cp.Comparison.Type == predicates.ComparisonIn {
			return false
		}
		if predicateContainsUncompensatableValues(pred) {
			return false
		}
		for alias := range predicates.GetCorrelatedToOfPredicate(pred) {
			if _, isLocal := local[alias]; !isLocal {
				return false
			}
		}
	}
	return true
}

func (p *Planner) implementDataAccessCompensation(ref *expressions.Reference, expr expressions.RelationalExpression) {
	exprRules, _ := p.rulesForPhase(PhasePlanning)
	for i := len(exprRules) - 1; i >= 0; i-- {
		p.push(&TransformExprTask{Phase: PhasePlanning, Ref: ref, Expr: expr, Rule: exprRules[i]})
	}
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

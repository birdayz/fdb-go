package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
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
	// stack MUST be LIFO. The bottom-up exploration invariant —
	// children-before-parent — depends on stack-pop returning the
	// most-recently-pushed Task. ExploreReferenceTask pushes the
	// SaturationCheckTask FIRST (deepest), then per-rule
	// TransformReferenceTasks, then per-member ExploreExpressionTasks
	// (topmost). LIFO pop order: ExploreExpression runs first
	// (descends to leaves), then rules fire, then saturation check.
	stack []Task
	rules []ExpressionRule
	ctx   PlanContext
	memo  *Memo

	// implementationRules run during PhasePlanning after the
	// REWRITING phase converges. They yield physical expressions
	// into Reference.Members via Insert.
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
		rules:        rules,
		ctx:          ctx,
		memo:         nil, // initialized lazily on first Explore call
		exploreCount: make(map[*expressions.Reference]int),
		costModel:    PlanningCostModelLess,
		MaxTasks:     100_000,
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

	// No ordering required → just pick cheapest overall.
	if props.IsEmpty() {
		best := ref.GetBest(p.costModel)
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
	p.planningExpressionRules = rules
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
	p.costModel = NewPlanningCostModelLess(stats)
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

// Plan runs the full EXPLORE → OPTIMIZE pipeline on `rootRef` and
// returns the cost-cheapest extracted plan tree.
//
// EXPLORE: drives the task-stack to convergence (Explore method).
// OPTIMIZE: extracts the cheapest member at every reachable Reference
//
//	via the cost-aware comparator + WithChildren-or-switch
//	rebuild path (delegates to properties.ExtractBestPlan).
//
// Equivalent to:
//
//	tasks, conv := p.Explore(rootRef)
//	if !conv { return nil, tasks, ErrPlannerCapHit }
//	plan, err := properties.ExtractBestPlan(rootRef)
//	return plan, tasks, err
//
// Returns:
//   - plan: the extracted RelationalExpression (singleton-Reference
//     tree); nil if rootRef is empty.
//   - tasks: total tasks executed during EXPLORE.
//   - err: nil on success; ErrPlannerCapHit if EXPLORE hit MaxTasks
//     (no OPTIMIZE attempted); extraction error otherwise.
func (p *Planner) Plan(rootRef *expressions.Reference) (expressions.RelationalExpression, int, error) {
	tasks, conv := p.Explore(rootRef)
	if !conv {
		return nil, tasks, ErrPlannerCapHit
	}

	// MATCHING phase: after EXPLORE converges, adjust partial matches
	// by absorbing candidate-side-only expressions (AdjustMatchRule).
	// No-op when no PartialMatches were created during EXPLORE.
	AdjustMatches(rootRef)

	// PLANNING phase: constraint propagation → data access → implementation.
	p.runPlanningPhase(rootRef)

	// Re-optimize all References to pick up PLANNING-phase results.
	// FinalMembers now contains physical plans from BatchA +
	// implementation rules. The winner is set to the best physical plan.
	p.reoptimizeAll(rootRef)

	// Post-PLANNING promotion: these passes promote InJoin/InUnion
	// and FlatMap plans when they have lower data access cost. With
	// advancePlannerStage clearing EXPLORE artifacts, these are safety
	// nets for edge cases where the cost model without statistics
	// doesn't distinguish InJoin vs Filter+Scan.
	p.promoteInJoinWinners(rootRef)
	promoteByDataAccessCost(rootRef, p.stats)

	plan, err := properties.ExtractBestPlanFromSelector(rootRef, p, p.stats)
	return plan, tasks, err
}

// ErrPlannerCapHit signals that Explore exited via the MaxTasks
// cap rather than convergence. Callers should treat this as a
// non-termination indicator and report.
var ErrPlannerCapHit = plannerErr("planner: MaxTasks cap hit before convergence")

// plannerErr is a string-error type local to the planner.
type plannerErr string

// Error returns the message.
func (e plannerErr) Error() string { return string(e) }

// runPlanningPhase fires implementation rules bottom-up on every
// Reference in the Memo. Leaf References first, then parents.
// Each rule inserts expressions into Members via ref.Insert.
//
// Phase sequence (matches Java's CascadesPlanner):
//  1. Top-down constraint propagation (ordering constraints)
//  2. Data access generation (uses propagated ordering constraints)
//  3. Bottom-up implementation (children before parents)
func (p *Planner) runPlanningPhase(rootRef *expressions.Reference) {
	cm := NewConstraintMap()

	// Pass 1: top-down constraint propagation. Rules that push
	// ordering constraints (e.g., DistinctUnionRule) fire here so
	// child References receive constraints before implementation.
	if len(p.implementationRules) > 0 {
		p.propagateConstraints(rootRef, make(map[*expressions.Reference]bool), cm)
	}

	// Pass 2: data access generation. Runs after constraint
	// propagation so requestedOrderings are available. Ports Java's
	// architecture where data access rules fire during the PLANNING
	// phase with ordering constraints.
	p.generateDataAccessWithConstraints(rootRef, cm)

	// Pass 3: bottom-up implementation. Children are implemented
	// first (with constraints from Pass 1 available), then parents.
	// Visit ALL Memo references to ensure data access expressions
	// generated in Pass 2 (which may be in non-root-reachable
	// references created during exploration) get properly implemented.
	if len(p.implementationRules) > 0 {
		visited := make(map[*expressions.Reference]bool)
		p.implementBottomUp(rootRef, visited, cm)
		if p.memo != nil {
			for ref := range p.memo.References() {
				p.implementBottomUp(ref, visited, cm)
			}
		}
	}
}

// advancePlannerStage transitions all References from EXPLORE to
// PLANNING. Each Reference keeps only the best logical expression
// (the EXPLORE-phase winner) as the canonical seed for PLANNING.
// All other exploratory members are cleared. Winners are cleared.
// PartialMatches are preserved (data access rules consume them).
//
// Mirrors Java's Reference.advancePlannerStage which clears
// exploratoryMembers, promotes finalMembers as the new seed, and
// clears finalMembers. In Go, EXPLORE doesn't produce finals, so
// the winner (best logical) serves as the canonical form.
func (p *Planner) advancePlannerStage(rootRef *expressions.Reference) {
	visited := make(map[*expressions.Reference]bool)
	p.advanceRecursive(rootRef, visited)
	if p.memo != nil {
		for ref := range p.memo.References() {
			p.advanceRecursive(ref, visited)
		}
	}
}

func (p *Planner) advanceRecursive(ref *expressions.Reference, visited map[*expressions.Reference]bool) {
	if ref == nil || visited[ref] {
		return
	}
	visited[ref] = true

	for _, m := range ref.AllMembers() {
		for _, q := range m.GetQuantifiers() {
			if childRef := q.GetRangesOver(); childRef != nil {
				p.advanceRecursive(childRef, visited)
			}
		}
	}

	ref.ClearWinners()
}

// reoptimizeAll re-runs OptimizeGroup on every reachable Reference
// bottom-up after the PLANNING phase. Implementation rules insert
// physical plans into Members; the cost model (criterion #1: physical
// beats non-physical) ensures they replace logical EXPLORE-phase winners.
func (p *Planner) reoptimizeAll(rootRef *expressions.Reference) {
	visited := make(map[*expressions.Reference]bool)
	p.reoptimizeRecursive(rootRef, visited)
	if p.memo != nil {
		for ref := range p.memo.References() {
			p.reoptimizeRecursive(ref, visited)
		}
	}
}

func (p *Planner) reoptimizeRecursive(ref *expressions.Reference, visited map[*expressions.Reference]bool) {
	if ref == nil || visited[ref] {
		return
	}
	visited[ref] = true
	for _, m := range ref.AllMembers() {
		for _, q := range m.GetQuantifiers() {
			if childRef := q.GetRangesOver(); childRef != nil {
				p.reoptimizeRecursive(childRef, visited)
			}
		}
	}

	candidates := ref.FinalMembers()
	if len(candidates) == 0 {
		candidates = ref.AllMembers()
	}

	existing := ref.Winner(expressions.NoProperties)
	if existing == nil {
		var best expressions.RelationalExpression
		for _, m := range candidates {
			if isNilInnerFetch(m) {
				continue
			}
			if best == nil || p.costModel(m, best) {
				best = m
			}
		}
		if best != nil {
			ref.SetWinner(expressions.NoProperties, best)
		}
	} else if _, isPhys := existing.(physicalPlanExpression); !isPhys {
		var bestPhys expressions.RelationalExpression
		for _, m := range candidates {
			if _, ok := m.(physicalPlanExpression); !ok {
				continue
			}
			if isNilInnerFetch(m) {
				continue
			}
			if bestPhys == nil || p.costModel(m, bestPhys) {
				bestPhys = m
			}
		}
		if bestPhys != nil {
			ref.SetWinner(expressions.NoProperties, bestPhys)
		}
	}

	stampOrderingWinners(ref, p.costModel)
}

// promoteInJoinWinners walks all References and checks if an InJoin
// or InUnion physical member is cheaper than the current NoProperties
// winner under the cost model. These plans are produced during PLANNING
// by ImplementInJoinRule/ImplementInUnionRule and may be cheaper than
// the EXPLORE-phase winner (e.g., InJoin(IndexScan) vs Filter(Scan)).
func (p *Planner) promoteInJoinWinners(rootRef *expressions.Reference) {
	visited := make(map[*expressions.Reference]bool)
	p.promoteInJoinRecursive(rootRef, visited)
	if p.memo != nil {
		for ref := range p.memo.References() {
			p.promoteInJoinRecursive(ref, visited)
		}
	}
}

func (p *Planner) promoteInJoinRecursive(ref *expressions.Reference, visited map[*expressions.Reference]bool) {
	if ref == nil || visited[ref] {
		return
	}
	visited[ref] = true
	for _, m := range ref.AllMembers() {
		for _, q := range m.GetQuantifiers() {
			if childRef := q.GetRangesOver(); childRef != nil {
				p.promoteInJoinRecursive(childRef, visited)
			}
		}
	}
	existing := ref.Winner(expressions.NoProperties)
	if existing == nil {
		return
	}
	if _, isPhys := existing.(physicalPlanExpression); !isPhys {
		return
	}
	existingProvidesOrdering := existingIsOrderingWinner(ref, existing)
	for _, m := range ref.AllMembers() {
		if !IsPhysicalInJoin(m) && !isPhysicalInUnion(m) {
			continue
		}
		if isNilInnerFetch(m) {
			continue
		}
		if existingProvidesOrdering {
			if oh, ok := m.(orderingHinter); ok {
				ord := oh.HintOrdering()
				if !ord.IsKnown || len(ord.Keys) == 0 {
					continue
				}
			} else {
				continue
			}
		}
		if p.costModel(m, existing) {
			existing = m
		}
	}
	ref.SetWinner(expressions.NoProperties, existing)
}

func isPhysicalInUnion(expr expressions.RelationalExpression) bool {
	_, ok := expr.(*physicalInUnionWrapper)
	return ok
}

func promoteByDataAccessCost(rootRef *expressions.Reference, stats properties.StatisticsProvider) {
	if rootRef == nil {
		return
	}
	existing := rootRef.Winner(expressions.NoProperties)
	if existing == nil {
		return
	}
	if _, ok := existing.(physicalPlanExpression); !ok {
		return
	}
	existingCounts := findExpressionsByType(existing, stats)
	if existingCounts.maxDataAccessCardinality < 0 {
		return
	}
	_, existingIsProj := existing.(*physicalProjectionWrapper)
	for _, m := range rootRef.AllMembers() {
		if _, ok := m.(physicalPlanExpression); !ok {
			continue
		}
		counts := findExpressionsByType(m, stats)
		if counts.maxDataAccessCardinality < 0 {
			continue
		}
		if counts.maxDataAccessCardinality > existingCounts.maxDataAccessCardinality {
			continue
		}
		if counts.maxDataAccessCardinality == existingCounts.maxDataAccessCardinality {
			if counts.flatMapCount == 0 || existingCounts.flatMapCount > 0 {
				continue
			}
		}
		// Don't replace a winner that includes InMemorySort with a
		// candidate that drops the sort — the query's ORDER BY requires it.
		if counts.inMemorySortCount < existingCounts.inMemorySortCount {
			continue
		}
		if existingIsProj {
			if _, ok := m.(*physicalProjectionWrapper); !ok {
				continue
			}
		}
		existing = m
		existingCounts = counts
	}
	rootRef.SetWinner(expressions.NoProperties, existing)
}

// existingIsOrderingWinner returns true if the given expression is the
// ordering-specific winner for any non-empty ordering on this Reference.
// When true, replacing the NoProperties winner would invalidate sort
// elimination at a parent Sort level.
func existingIsOrderingWinner(ref *expressions.Reference, expr expressions.RelationalExpression) bool {
	oh, ok := expr.(orderingHinter)
	if !ok {
		return false
	}
	ord := oh.HintOrdering()
	if !ord.IsKnown || len(ord.Keys) == 0 {
		return false
	}
	props := orderingToProps(ord)
	if props.IsEmpty() {
		return false
	}
	winner := ref.Winner(props)
	return winner == expr
}

// isNilInnerFetch returns true if expr is a physicalFetchFromPartialRecordWrapper
// whose embedded plan has a nil inner. These are push-through-fetch shells
// created by rules like PushInJoinThroughFetchRule — they're assembled
// into valid plans during extraction via WithChildren, but should never
// be selected as standalone winners.
func isNilInnerFetch(expr expressions.RelationalExpression) bool {
	fw, ok := expr.(*physicalFetchFromPartialRecordWrapper)
	if !ok {
		return false
	}
	return fw.plan != nil && fw.plan.GetInner() == nil
}

// reExplorePlanning re-runs the task-stack with PlanningExplorationRules
// to re-derive logical alternatives from the canonical seed after
// advancePlannerStage. Uses the existing Explore infrastructure with a
// temporary rule set swap.
func (p *Planner) reExplorePlanning(rootRef *expressions.Reference, rules []ExpressionRule) {
	savedRules := p.rules
	savedCount := p.exploreCount
	p.rules = rules
	p.exploreCount = make(map[*expressions.Reference]int)
	p.Explore(rootRef)
	p.rules = savedRules
	p.exploreCount = savedCount
}

func (p *Planner) propagateConstraints(ref *expressions.Reference, visited map[*expressions.Reference]bool, cm *ConstraintMap) {
	if ref == nil || visited[ref] {
		return
	}
	visited[ref] = true

	for _, rule := range p.implementationRules {
		for _, member := range ref.AllMembers() {
			bindings := rule.Matcher().BindMatches(matching.NewBindings(), member)
			for _, b := range bindings {
				call := &ImplementationRuleCall{
					Bindings:       b,
					Reference:      ref,
					Context:        p.ctx,
					Constraints:    cm,
					constraintOnly: true,
				}
				rule.OnMatch(call)
			}
		}
	}

	for _, m := range ref.Members() {
		for _, q := range m.GetQuantifiers() {
			if childRef := q.GetRangesOver(); childRef != nil {
				p.propagateConstraints(childRef, visited, cm)
			}
		}
	}
}

func (p *Planner) implementBottomUp(ref *expressions.Reference, visited map[*expressions.Reference]bool, cm *ConstraintMap) {
	if ref == nil || visited[ref] {
		return
	}
	visited[ref] = true

	for _, m := range ref.Members() {
		for _, q := range m.GetQuantifiers() {
			if childRef := q.GetRangesOver(); childRef != nil {
				p.implementBottomUp(childRef, visited, cm)
			}
		}
	}

	// Fixpoint: fire planning expression rules (BatchA) and
	// implementation rules until no new members are produced.
	// Planning expression rules produce physical scan/filter wrappers;
	// implementation rules consume them to produce higher-level plans
	// (InJoin, Sort elimination, etc.). Both yield to InsertFinal.
	const maxFixpointRounds = 8
	for round := 0; round < maxFixpointRounds; round++ {
		before := len(ref.AllMembers())
		p.firePlanningExprRules(ref)
		for _, rule := range p.implementationRules {
			FireImplementationRuleWithContext(rule, ref, p.ctx, p.memo, cm)
		}
		if len(ref.AllMembers()) == before {
			break
		}
	}

	computeRefPlanProperties(ref)
}

// firePlanningExprRules fires planningExpressionRules (BatchA) against
// a Reference during PLANNING. Yields go to InsertFinal so they land
// in finalMembers — authoritative for plan selection.
//
// For SelectExpressions with ChildrenAsSet (commutative joins), also
// fires each rule on the swapped-quantifier variant. Mirrors
// FireImplementationRuleWithContext's join commutativity exploration.
func (p *Planner) firePlanningExprRules(ref *expressions.Reference) {
	if len(p.planningExpressionRules) == 0 {
		return
	}
	yieldFn := func(expr expressions.RelationalExpression) bool {
		return ref.InsertFinal(expr)
	}
	for _, rule := range p.planningExpressionRules {
		for _, member := range ref.AllMembers() {
			p.firePlanningExprRuleOnMember(rule, ref, member, yieldFn)

			if sel, ok := member.(*expressions.SelectExpression); ok && sel.ChildrenAsSet() {
				qs := sel.GetQuantifiers()
				if len(qs) >= 2 && sel.GetJoinType() != expressions.JoinLeftOuter &&
					qs[0].Kind() == expressions.QuantifierForEach &&
					qs[1].Kind() == expressions.QuantifierForEach {
					swapped := sel.WithSwappedQuantifiers()
					p.firePlanningExprRuleOnMember(rule, ref, swapped, yieldFn)
				}
			}
		}
	}
}

func (p *Planner) firePlanningExprRuleOnMember(rule ExpressionRule, ref *expressions.Reference, member expressions.RelationalExpression, yieldFn func(expressions.RelationalExpression) bool) {
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), member)
	for _, b := range bindings {
		call := &ExpressionRuleCall{
			Bindings:  b,
			Reference: ref,
			Context:   p.ctx,
			memo:      p.memo,
			yieldFn:   yieldFn,
		}
		rule.OnMatch(call)
	}
}

// generateDataAccessWithConstraints walks the expression DAG bottom-up
// and generates data access expressions (index scans) from
// PartialMatches. Uses ordering constraints from the ConstraintMap
// (propagated during Pass 1) to inform scan direction selection.
//
// This is the Go equivalent of Java's WithPrimaryKeyDataAccessRule
// which extends CascadesRule<MatchPartition>. Rather than modelling
// MatchPartition as a rule trigger, we run this as an explicit pass
// within the PLANNING phase — architecturally equivalent since Java's
// data access rules fire during the same phase as constraint
// propagation.
func (p *Planner) generateDataAccessWithConstraints(rootRef *expressions.Reference, cm *ConstraintMap) {
	visited := map[*expressions.Reference]bool{}
	p.generateDataAccessRecursive(rootRef, visited, cm)
	if p.memo != nil {
		for ref := range p.memo.References() {
			p.generateDataAccessRecursive(ref, visited, cm)
		}
	}
}

// generateDataAccessRecursive recurses children first (bottom-up),
// then generates data access expressions for the current Reference.
func (p *Planner) generateDataAccessRecursive(ref *expressions.Reference, visited map[*expressions.Reference]bool, cm *ConstraintMap) {
	if ref == nil || visited[ref] {
		return
	}
	visited[ref] = true

	// Recurse children first so leaf scans are processed before
	// parent operators that depend on them.
	for _, m := range ref.AllMembers() {
		for _, q := range m.GetQuantifiers() {
			if childRef := q.GetRangesOver(); childRef != nil {
				p.generateDataAccessRecursive(childRef, visited, cm)
			}
		}
	}

	// Collect all MatchCandidates that have PartialMatches on this ref.
	candidates := GetPartialMatchCandidatesTyped(ref)
	if len(candidates) == 0 {
		return
	}

	// Read ordering constraints propagated during Pass 1.
	var requestedOrderings []*RequestedOrdering
	if cm != nil {
		if orderings, ok := Get(cm, ref, RequestedOrderingConstraintKey); ok {
			requestedOrderings = orderings
		}
	}

	for _, candidate := range candidates {
		matches := GetPartialMatchesForCandidate(ref, candidate)
		if len(matches) == 0 {
			continue
		}

		exprs := DataAccessForMatchPartition(
			requestedOrderings,
			matches,
			p.ctx,
			nil, // intersector (single-scan only)
		)

		// Insert generated expressions as final members so Pass 3
		// (bottom-up implementation) and ToPlanPartitions see them.
		// Also stamp ordering winners for physical scans that provide
		// ordering (enables sort elimination via per-properties winners
		// in extraction).
		for _, expr := range exprs {
			if fw, ok := expr.(*physicalFetchFromPartialRecordWrapper); ok {
				if fw.plan == nil || fw.plan.GetInner() == nil {
					continue
				}
			}
			ref.InsertFinal(expr)
		}
		stampOrderingWinners(ref, p.costModel)
	}
}

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
	if last, seen := p.exploreCount[t.Ref]; seen && last == beforeCount {
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

	// Update Memo index for newly-added members.
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
	p.exploreCount[t.Ref] = afterCount
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

	// Stamp ordering-specific winners: for each physical member that
	// provides a known ordering, store it as winner for those ordering
	// properties. If multiple members provide the same ordering, the
	// cost model picks the better one.
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

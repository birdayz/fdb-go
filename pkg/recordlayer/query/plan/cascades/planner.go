package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
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
	// REWRITING phase converges. They yield final expressions into
	// Reference.finalMembers via InsertFinal.
	implementationRules []ImplementationRule

	// exploreCount[ref] = member count at last saturation check on
	// `ref`. SaturationCheckTask short-circuits when count hasn't grown.
	exploreCount map[*expressions.Reference]int

	// bestMember[ref] = the cheapest member chosen by the OPTIMIZE
	// phase. Populated by OptimizeReferenceTask after saturation.
	// Available via BestMember(ref) after Explore returns.
	bestMember map[*expressions.Reference]expressions.RelationalExpression

	// MaxTasks caps the total tasks executed before the planner
	// gives up (returns the partial result). Defaults to 100_000.
	// Hitting the cap is a strong signal of a non-terminating rule —
	// callers should report.
	MaxTasks int

	tasksRun int

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
		bestMember:   make(map[*expressions.Reference]expressions.RelationalExpression),
		MaxTasks:     100_000,
	}
}

// Memo returns the planner's Memo structure. Available after Explore
// has been called (returns nil before that).
func (p *Planner) Memo() *Memo {
	return p.memo
}

// BestMember returns the OPTIMIZE-chosen best member for `ref`,
// or nil if the Reference wasn't optimized (e.g. unreachable from
// the Explore root, or Explore hit MaxTasks).
//
// Available after Explore (or Plan) returns. The map is populated
// by OptimizeReferenceTask which fires after a Reference's
// ApplyRulesTask reports no growth (saturation reached).
func (p *Planner) BestMember(ref *expressions.Reference) expressions.RelationalExpression {
	if ref == nil {
		return nil
	}
	return p.bestMember[ref]
}

// HasBestMember reports whether OptimizeReferenceTask has stamped
// a best for `ref`. Used by tests + the integration path that
// distinguishes "Reference not yet optimized" from "Reference
// optimized to nil (empty)".
func (p *Planner) HasBestMember(ref *expressions.Reference) bool {
	if ref == nil {
		return false
	}
	_, ok := p.bestMember[ref]
	return ok
}

// WithImplementationRules adds rules for PhasePlanning. These run
// after the REWRITING phase converges. Returns p for chaining.
func (p *Planner) WithImplementationRules(rules []ImplementationRule) *Planner {
	p.implementationRules = rules
	return p
}

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

	// DATA ACCESS phase: convert PartialMatches into index scan
	// expressions. Runs after matching so all PartialMatches are
	// available, before PLANNING so implementation rules see them.
	// Ports Java's WithPrimaryKeyDataAccessRule / dataAccessForMatchPartition
	// pipeline — walks the expression DAG bottom-up and, for every
	// Reference that carries PartialMatches, generates scan plan
	// expressions via DataAccessForMatchPartition.
	p.generateDataAccess(rootRef)

	// PLANNING phase: fire implementation rules bottom-up to finalize
	// exploratory expressions into final members.
	if len(p.implementationRules) > 0 {
		p.runPlanningPhase(rootRef)
	}

	// Use the selector path so extraction reuses the OPTIMIZE-stamped
	// best member per Reference (avoids re-computing CostLess that
	// the OPTIMIZE phase already did).
	plan, err := properties.ExtractBestPlanFromSelector(rootRef, p, properties.DefaultStatistics{})
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
// Each rule produces final members via InsertFinal.
func (p *Planner) runPlanningPhase(rootRef *expressions.Reference) {
	cm := NewConstraintMap()

	// Pass 1: top-down constraint propagation. Rules that push
	// ordering constraints (e.g., DistinctUnionRule) fire here so
	// child References receive constraints before implementation.
	p.propagateConstraints(rootRef, make(map[*expressions.Reference]bool), cm)

	// Pass 2: bottom-up implementation. Children are implemented
	// first (with constraints from Pass 1 available), then parents.
	p.implementBottomUp(rootRef, make(map[*expressions.Reference]bool), cm)
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

	for _, rule := range p.implementationRules {
		FireImplementationRuleWithContext(rule, ref, p.ctx, cm)
	}

	computeRefPlanProperties(ref)
}

// generateDataAccess walks the expression DAG bottom-up and generates
// data access expressions (index scans) from PartialMatches. For each
// Reference that carries PartialMatches, it calls
// DataAccessForMatchPartition to convert the matches into scan plan
// expressions and inserts them into the Reference.
//
// This is the Go equivalent of Java's WithPrimaryKeyDataAccessRule
// which extends CascadesRule<MatchPartition>. Rather than modelling
// MatchPartition as a rule trigger, we run this as an explicit phase
// between AdjustMatches and PLANNING — architecturally equivalent but
// simpler since Go doesn't need the Java rule-dispatch infrastructure.
func (p *Planner) generateDataAccess(rootRef *expressions.Reference) {
	visited := map[*expressions.Reference]bool{}
	p.generateDataAccessRecursive(rootRef, visited)
}

// generateDataAccessRecursive recurses children first (bottom-up),
// then generates data access expressions for the current Reference.
func (p *Planner) generateDataAccessRecursive(ref *expressions.Reference, visited map[*expressions.Reference]bool) {
	if ref == nil || visited[ref] {
		return
	}
	visited[ref] = true

	// Recurse children first so leaf scans are processed before
	// parent operators that depend on them.
	for _, m := range ref.AllMembers() {
		for _, q := range m.GetQuantifiers() {
			if childRef := q.GetRangesOver(); childRef != nil {
				p.generateDataAccessRecursive(childRef, visited)
			}
		}
	}

	// Collect all MatchCandidates that have PartialMatches on this ref.
	candidates := GetPartialMatchCandidatesTyped(ref)
	if len(candidates) == 0 {
		return
	}

	for _, candidate := range candidates {
		matches := GetPartialMatchesForCandidate(ref, candidate)
		if len(matches) == 0 {
			continue
		}

		// Generate data access expressions. No requested orderings in
		// the seed — the full implementation propagates ordering
		// constraints from parent operators. No intersector — the seed
		// does single-scan only.
		exprs := DataAccessForMatchPartition(
			nil, // requestedOrderings
			matches,
			p.ctx,
			nil, // intersector (single-scan only)
		)

		// Insert generated expressions into the Reference so the
		// PLANNING phase's implementation rules can see them.
		for _, expr := range exprs {
			ref.Insert(expr)
		}
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

// OptimizeReferenceTask is the OPTIMIZE phase: picks the cheapest
// member of the Reference under the planner's cost comparator and
// stamps it in the bestMember map. Mirrors Java's
// OPTIMIZE phase in CascadesPlanner.
//
// Fires after ApplyRulesTask reports a Reference saturated. The
// pre-condition is that all child Reference's ApplyRulesTask have
// also fired (bottom-up traversal guarantees this) — but we don't
// require children to be optimized first; the cost model walks
// child References itself via firstMemberCost (the cost model's
// recursion contract).
//
// The seed uses properties.CostLess as the comparator. A future
// shift can plumb a configurable comparator via PlanContext.
//
// PERF NOTE: properties.CostLess is the un-memoised cost-comparator
// — every GetBest comparison re-walks the full sub-tree. For a
// Reference with K members over an N-deep tree this is O(K·N)
// per OptimizeReferenceTask. Acceptable for the seed (small trees,
// low K). When N or K grow, switch to a memoised comparator
// (properties.BestRefCostWith populates a per-call cache; expose
// it via the PlanContext as a follow-up).
type OptimizeReferenceTask struct {
	Ref *expressions.Reference
}

// Run picks the cheapest member, stamps it, fires the event.
func (t *OptimizeReferenceTask) Run(p *Planner) {
	if t.Ref == nil {
		return
	}
	best := t.Ref.GetBest(properties.CostLess)
	p.bestMember[t.Ref] = best
	if p.events != nil {
		p.events.OnOptimizeReference(t.Ref, best)
	}
}

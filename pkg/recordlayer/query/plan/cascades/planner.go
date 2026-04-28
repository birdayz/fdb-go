package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
)

// Planner is the task-stack driven cascades planner — Track B6 seed.
//
// Replaces FixpointApply's "fire every rule on every Reference each
// pass" approach with a task-stack driver that:
//
//   - Explores the expression DAG bottom-up: leaves fire rules first,
//     ancestors after.
//   - Tracks per-Reference saturation: a Reference whose member count
//     hasn't changed since last ApplyRules pass is NOT re-fired.
//     This avoids the O(N*passes) wasted work in FixpointApply.
//   - Exposes event hooks for diagnostic output without changing the
//     core driver.
//
// The Java equivalent is `CascadesPlanner` — task-stack with EXPLORE
// and OPTIMIZE phases. The seed implements EXPLORE only; OPTIMIZE is
// `properties.ExtractBestPlan` (already shipped) which the planner
// invokes after exploration converges.
//
// Convergence: same contract as FixpointApply. A saturated Reference
// has stable member count; the stack drains; planner returns. Hard
// cap (MaxTasks) prevents pathological non-termination from
// rule-yielding-fresh-members loops; default 100_000.
//
// The seed planner is single-threaded. Multi-threaded exploration
// (Java's planner is also single-threaded) is a future concern.
type Planner struct {
	stack []Task
	rules []ExpressionRule
	ctx   PlanContext

	// exploreCount[ref] = member count at last ApplyRules pass on
	// `ref`. ApplyRulesTask short-circuits when count hasn't grown.
	exploreCount map[*expressions.Reference]int

	// bestMember[ref] = the cheapest member chosen by the OPTIMIZE
	// phase. Populated by OptimizeReferenceTask after ApplyRules
	// reports no growth (Reference is saturated). Available via
	// BestMember(ref) after Explore returns.
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
	// OnApplyRules fires when ApplyRulesTask runs; `grew` is the
	// number of members added during this pass.
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
		exploreCount: make(map[*expressions.Reference]int),
		bestMember:   make(map[*expressions.Reference]expressions.RelationalExpression),
		MaxTasks:     100_000,
	}
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
	plan, err := properties.ExtractBestPlan(rootRef)
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

// ExploreReferenceTask explores a Reference: schedules ApplyRules on
// the Reference (to fire after children) and ExploreExpression on
// each member (which descend into children first).
//
// Bottom-up ordering invariant: ApplyRulesTask is pushed BEFORE
// per-member ExploreExpressionTask. LIFO stack means ExploreExpression
// runs first, descending to the leaves; ApplyRules fires last, on
// already-explored children.
type ExploreReferenceTask struct {
	Ref *expressions.Reference
}

// Run pushes ApplyRulesTask + per-member ExploreExpressionTask.
func (t *ExploreReferenceTask) Run(p *Planner) {
	if t.Ref == nil {
		return
	}
	if p.events != nil {
		p.events.OnExploreReference(t.Ref)
	}
	// Push ApplyRules first → fires AFTER children (LIFO).
	p.push(&ApplyRulesTask{Ref: t.Ref})
	// Push per-member ExploreExpressionTask → fire BEFORE ApplyRules.
	// Snapshot members at task-spawn time so subsequently-yielded
	// members get re-explored via the post-ApplyRules ExploreReference
	// re-push (not via this snapshot).
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

// ApplyRulesTask fires every rule in the planner's rule set against
// the given Reference. New yields go into the Reference via
// Reference.Insert (dedup'd). If any rule grew the member set,
// re-pushes ExploreReferenceTask for re-exploration of the new
// shapes.
//
// Saturation: if the Reference's member count is the same as the
// last ApplyRules pass on it, this task is a no-op. Avoids re-firing
// rules on saturated References.
type ApplyRulesTask struct {
	Ref *expressions.Reference
}

// Run fires every rule on every member; on growth, re-pushes
// ExploreReferenceTask.
//
// Saturation semantics: a Reference is saturated when ApplyRules
// fired on its current member set and no new yields were produced.
// We mark saturation by recording the post-fire count in
// exploreCount[ref]. The next ApplyRules pass on `ref` short-circuits
// only if the recorded count matches the current count (i.e. nothing
// has been added since saturation).
//
// Critical correctness contract: when a fire GROWS the member set,
// we MUST NOT mark the Reference saturated — rules need to fire
// again on the new members. Setting exploreCount to the post-grow
// count would cause the next pass to short-circuit, missing rules
// that match the freshly-added shapes. The fuzzer caught this:
// FuzzPlanner_Confluence reproduced an input where FixpointApply's
// 4 members became Planner's 3 because a post-grow saturation
// stamp prevented the fourth member from being yielded.
//
// Fix: only set exploreCount when grew == 0 (saturation reached).
// On growth, leave exploreCount unchanged and re-push
// ExploreReferenceTask so rules fire again on the larger set.
func (t *ApplyRulesTask) Run(p *Planner) {
	if t.Ref == nil {
		return
	}
	beforeCount := len(t.Ref.Members())

	// Saturation: if the member count hasn't moved since the last
	// fully-saturated ApplyRules pass, skip — no new shapes to match on.
	if last, seen := p.exploreCount[t.Ref]; seen && last == beforeCount {
		if p.events != nil {
			p.events.OnApplyRules(t.Ref, 0)
		}
		return
	}

	for _, rule := range p.rules {
		FireExpressionRule(rule, t.Ref)
	}
	afterCount := len(t.Ref.Members())
	grew := afterCount - beforeCount

	if p.events != nil {
		p.events.OnApplyRules(t.Ref, grew)
	}

	if grew > 0 {
		// New members may have new sub-References; re-explore so
		// rules at THIS Reference get to see the freshly-yielded
		// shapes' children too. Do NOT mark saturated — the next
		// ApplyRules pass must run rules on the now-larger set to
		// catch any rules that match the freshly-added shapes.
		p.push(&ExploreReferenceTask{Ref: t.Ref})
		return
	}
	// No growth — Reference is saturated under the current rule set.
	// Stamp the count so future ApplyRules passes short-circuit.
	p.exploreCount[t.Ref] = afterCount
	// Schedule OPTIMIZE for this Reference. The OptimizeReferenceTask
	// picks the best member by cost.
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

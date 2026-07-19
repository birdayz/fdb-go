package cascades

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// Plan reachability: every child a physical plan executes must be an
// expression the corresponding quantifier's group can actually produce.
//
// This is the invariant RFC-183 exists to establish. When it breaks, the memo
// costs one expression while a different one executes — the planner's cost
// model is reasoning about a plan that will never run. Nothing downstream
// notices, because extraction reads the PLAN and never consults the
// quantifier's group, so both the test suite and the corpus explain-differ
// stay green while the optimizer quietly makes decisions on fiction.
//
// That silence is why this check is permanent code rather than a throwaway
// script. It was reconstructed three separate times during RFC-183 — each
// time to answer a question the existing green signals structurally could not
// answer.
//
// Comparison is plans.Equals (node-local fields plus recursive children), NOT
// rendered explain text. Explain is lossy and has hidden the deciding field
// repeatedly: `Scan(T, [=])` renders identically for different comparison
// operands, and a conjoined predicate renders as `[1 preds]` exactly like a
// dropped one. A reachability check built on rendered text would report clean
// on real divergence.

// ReachabilityViolation is one plan child that its quantifier's group cannot
// produce.
type ReachabilityViolation struct {
	ParentType  string
	ChildIndex  int
	NumChildren int

	// Reason distinguishes a genuine unreachable edge from the two shapes
	// that merely LOOK like one. Counting without this conflates them, which
	// is how RFC-183 §12 over-counted 343 edges down to a true 158.
	Reason string

	ParentExplain string
	ChildExplain  string
	GroupExplain  string
}

func (v ReachabilityViolation) String() string {
	return fmt.Sprintf("%s child[%d/%d] %s\n   parent-> %s\n   plan-child-> %s\n   group-> %s",
		v.ParentType, v.ChildIndex, v.NumChildren, v.Reason,
		v.ParentExplain, v.ChildExplain, v.GroupExplain)
}

// Reasons. A group holding MULTIPLE members including the plan's child is
// Cascades working correctly (alternatives), not a defect — conflating that
// with a true miss is precisely the error RFC-183 §13 corrects.
const (
	// ReasonAbsent is the real defect: no member of the group equals the
	// child the plan executes.
	ReasonAbsent = "child absent from group"
	// ReasonEmptyGroup is a mis-seeded reference — the quantifier ranges
	// over a group with no final members at all.
	ReasonEmptyGroup = "group has no final members"
	// ReasonNoQuantifier is a plan child with no corresponding quantifier:
	// structural plumbing the memo never modelled.
	ReasonNoQuantifier = "plan child has no quantifier"
)

// CheckPlanReachability reports every plan child that its quantifier's group
// cannot produce, for ONE expression: its own plan children against its own
// quantifiers, depth 1.
//
// It deliberately does NOT recurse into group members. Every expression is
// checked when IT is yielded, so recursing re-counts the same edge once per
// ancestor — an earlier revision did exactly that and reported 3541 where the
// true figure is two orders of magnitude smaller.
//
// A nil or non-physical expression yields no violations: this asks whether a
// plan is consistent with its memo, and there is nothing to ask of a non-plan.
func CheckPlanReachability(expr expressions.RelationalExpression) []ReachabilityViolation {
	var out []ReachabilityViolation
	if expr == nil {
		return out
	}
	ph, ok := expr.(physicalPlanExpression)
	if !ok {
		return out
	}
	plan := ph.GetRecordQueryPlan()
	if plan == nil {
		return out
	}
	collectReachability(expr, plan, &out)
	return out
}

func collectReachability(expr expressions.RelationalExpression, plan plans.RecordQueryPlan, out *[]ReachabilityViolation) {
	children := plan.GetChildren()
	quants := expr.GetQuantifiers()

	for i, child := range children {
		if child == nil {
			continue
		}
		if i >= len(quants) {
			// Not every physical wrapper models each plan child as a
			// quantifier. Report it rather than skip: an unmodelled child is
			// invisible to the memo by construction.
			*out = append(*out, ReachabilityViolation{
				ParentType: planTypeName(plan), ChildIndex: i, NumChildren: len(children),
				Reason:        ReasonNoQuantifier,
				ParentExplain: safeExplain(plan), ChildExplain: safeExplain(child),
			})
			continue
		}

		ref := quants[i].GetRangesOver()
		if ref == nil {
			*out = append(*out, ReachabilityViolation{
				ParentType: planTypeName(plan), ChildIndex: i, NumChildren: len(children),
				Reason:        ReasonEmptyGroup,
				ParentExplain: safeExplain(plan), ChildExplain: safeExplain(child),
			})
			continue
		}

		// AllMembers, not FinalMembers. At yield time a child is typically
		// still an exploratory member — FinalMembers reported 18174 empty
		// groups across the corpus, which is a measurement artifact, not a
		// defect. verifyChildrenMemoized next door uses AllMembers for the
		// same reason.
		members := ref.AllMembers()
		if len(members) == 0 {
			*out = append(*out, ReachabilityViolation{
				ParentType: planTypeName(plan), ChildIndex: i, NumChildren: len(children),
				Reason:        ReasonEmptyGroup,
				ParentExplain: safeExplain(plan), ChildExplain: safeExplain(child),
			})
			continue
		}

		// Reachable if ANY member produces this child. A group legitimately
		// holds alternatives; the plan being built on one of them is the
		// optimizer choosing, not diverging.
		found := false
		for _, m := range members {
			mp, ok := m.(physicalPlanExpression)
			if !ok {
				continue
			}
			if plans.Equals(mp.GetRecordQueryPlan(), child) {
				found = true
				break
			}
		}
		if !found {
			*out = append(*out, ReachabilityViolation{
				ParentType: planTypeName(plan), ChildIndex: i, NumChildren: len(children),
				Reason:        ReasonAbsent,
				ParentExplain: safeExplain(plan), ChildExplain: safeExplain(child),
				GroupExplain: explainMembers(members),
			})
		}
	}
}

func planTypeName(p plans.RecordQueryPlan) string {
	return strings.TrimPrefix(fmt.Sprintf("%T", p), "*plans.RecordQuery")
}

func explainMembers(members []expressions.RelationalExpression) string {
	parts := make([]string, 0, len(members))
	for _, m := range members {
		if mp, ok := m.(physicalPlanExpression); ok {
			parts = append(parts, safeExplain(mp.GetRecordQueryPlan()))
		} else {
			parts = append(parts, fmt.Sprintf("<non-physical %T>", m))
		}
	}
	return fmt.Sprintf("(%d) %s", len(members), strings.Join(parts, " | "))
}

// safeExplain renders for HUMAN diagnosis only — never for the equality
// decision, which uses plans.Equals. Two plans that render identically here
// can still be genuinely different, and that is exactly why this string is
// not the predicate.
//
// Deliberately does NOT recover around Explain, beyond the nil guard. A
// speculative recover here would be worse than useless: this runs INSIDE
// planning, so a swallowed panic re-emerges through explaindiff's planGuarded
// as a bogus per-query plan error, silently corrupting the very baseline this
// work relies on. A panicking Explain is a real defect and belongs in a stack
// trace, not in a diagnostic string.
func safeExplain(p plans.RecordQueryPlan) string {
	if p == nil {
		return "<nil>"
	}
	return p.Explain()
}

// Corpus-wide accounting.
//
// Off unless RFC183_REACHABILITY is set, so production planning pays only a
// single already-loaded bool. It is opt-in rather than always-on because the
// invariant is not yet clean: RFC-183 inherited a population of violations and
// is driving it to zero, so failing hard here today would simply disable the
// planner. Once the count reaches zero this graduates into verifyNoShell's
// company as an unconditional error — that is the whole point of measuring.
//
// Collection happens at YIELD time, which is the only place the memo and the
// plan are both in hand. Groups legitimately hold alternatives at that moment,
// which is exactly why the check accepts ANY matching member: an earlier
// RFC-183 measurement counted a plan built on one of several alternatives as a
// divergence and had to be retracted (§13). Do not "tighten" this to compare
// against a single member — that reintroduces the retracted error, and
// narrowing a group to its used member is a real regression (it destroyed the
// InUnion alternative when tried on the IN-join rule).
var (
	reachOnce       sync.Once
	reachEnabled    bool
	reachMu         sync.Mutex
	reachEdges      int
	reachViolations []ReachabilityViolation
)

func reachabilityEnabled() bool {
	reachOnce.Do(func() { reachEnabled = os.Getenv("RFC183_REACHABILITY") != "" })
	return reachEnabled
}

// recordReachability accounts one yielded expression.
func recordReachability(expr expressions.RelationalExpression) {
	if !reachabilityEnabled() {
		return
	}
	ph, ok := expr.(physicalPlanExpression)
	if !ok {
		return
	}
	plan := ph.GetRecordQueryPlan()
	if plan == nil {
		return
	}
	v := CheckPlanReachability(expr)

	reachMu.Lock()
	defer reachMu.Unlock()
	reachEdges += len(plan.GetChildren())
	reachViolations = append(reachViolations, v...)
}

// ReachabilityReport renders the accumulated tally: per-plan-type counts by
// reason, plus samples. Safe to call with collection disabled (reports zero).
func ReachabilityReport(maxSamples int) string {
	reachMu.Lock()
	defer reachMu.Unlock()

	byType := map[string]int{}
	byReason := map[string]int{}
	for _, v := range reachViolations {
		byType[v.ParentType]++
		byReason[v.Reason]++
	}

	var b strings.Builder
	fmt.Fprintf(&b, "edges=%d UNREACHABLE=%d\n", reachEdges, len(reachViolations))

	fmt.Fprintf(&b, "\nby reason:\n")
	for _, r := range sortedKeys(byReason) {
		fmt.Fprintf(&b, "  %-32s %d\n", r, byReason[r])
	}
	fmt.Fprintf(&b, "\nby plan type:\n")
	for _, t := range sortedKeys(byType) {
		fmt.Fprintf(&b, "  %-32s %d\n", t, byType[t])
	}

	fmt.Fprintf(&b, "\nsamples:\n")
	for i, v := range reachViolations {
		if i >= maxSamples {
			// Never truncate silently: a capped list that reads as
			// complete is how a partial measurement gets quoted as a total.
			fmt.Fprintf(&b, "  ... %d more suppressed\n", len(reachViolations)-maxSamples)
			break
		}
		fmt.Fprintf(&b, "%s\n", v)
	}
	return b.String()
}

// ResetReachability clears the tally so a test can measure one planning run in
// isolation.
func ResetReachability() {
	reachMu.Lock()
	defer reachMu.Unlock()
	reachEdges, reachViolations = 0, nil
}

// ReachabilityCount returns the count of GENUINE unreachable edges
// (ReasonAbsent): a child the plan executes that its quantifier's group cannot
// produce.
//
// ReasonNoQuantifier is deliberately excluded from the headline. It is
// dominated by leaf adapters — scanPlanExpression and friends report no
// quantifiers BY DESIGN while wrapping a plan that has children — so folding
// it in would restate RFC-183 §12's over-count in a new form: a big number
// whose bulk is architecture working as intended. Those cases are still
// reported by reason in ReachabilityReport, and deserve their own triage
// rather than a silent merge into this one.
func ReachabilityCount() int {
	reachMu.Lock()
	defer reachMu.Unlock()
	n := 0
	for _, v := range reachViolations {
		if v.Reason == ReasonAbsent {
			n++
		}
	}
	return n
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

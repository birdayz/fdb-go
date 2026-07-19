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

	// Divergence names the first node where the executed child and the
	// nearest group member stop being equal. Explain alone cannot answer
	// this: some violations render identically on both sides.
	Divergence string
}

func (v ReachabilityViolation) String() string {
	return fmt.Sprintf("%s child[%d/%d] %s\n   parent-> %s\n   plan-child-> %s\n   group-> %s",
		v.ParentType, v.ChildIndex, v.NumChildren, v.Reason,
		v.ParentExplain, v.ChildExplain, v.GroupExplain) + divergenceLine(v.Divergence)
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

// collectReachability appends violations and returns the number of edges it
// ACTUALLY compared against a group.
//
// The compared count is produced HERE, incremented immediately before the
// member scan, and not by a sibling helper. An earlier revision computed it in
// a separate countComparedEdges walk, which made the anti-blindness guard
// unable to detect the very failure it names: stubbing this function to return
// immediately left the checker totally blind, yet the separate counter still
// reported 17884 compared edges and the ratchet went GREEN. A guard that
// cannot observe the thing it guards is worse than none, because it reads as
// coverage.
func collectReachability(expr expressions.RelationalExpression, plan plans.RecordQueryPlan, out *[]ReachabilityViolation) int {
	children := plan.GetChildren()
	quants := expr.GetQuantifiers()
	compared := 0

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
		compared++
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
				Divergence:   firstDivergence(child, members),
			})
		}
	}
	return compared
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
	reachCompared   int
	reachViolations []ReachabilityViolation
)

func reachabilityEnabled() bool {
	reachOnce.Do(func() {
		reachMu.Lock()
		defer reachMu.Unlock()
		reachEnabled = reachEnabled || os.Getenv("RFC183_REACHABILITY") != ""
	})
	reachMu.Lock()
	defer reachMu.Unlock()
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
	var v []ReachabilityViolation
	comparedEdges := collectReachability(expr, plan, &v)

	reachMu.Lock()
	defer reachMu.Unlock()
	reachEdges += len(plan.GetChildren())
	reachCompared += comparedEdges
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
	fmt.Fprintf(&b, "edges=%d compared=%d UNREACHABLE=%d\n", reachEdges, reachCompared, len(reachViolations))

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
	reachEdges, reachCompared, reachViolations = 0, 0, nil
}

// ReachabilityCount returns the count of GENUINE defects: a plan child its
// quantifier's group cannot produce (ReasonAbsent), plus a quantifier ranging
// over an empty reference (ReasonEmptyGroup, a mis-seeded reference).
//
// BOTH are defects, so both are counted. An earlier revision counted only
// ReasonAbsent, which made the headline "0" a zero of ONE reason code: an
// EmptyGroup regression would have slipped through the ratchet silently while
// the number still read zero. EmptyGroup happens to be 0 across the corpus
// today, so that filtering did not inflate any published figure — but a
// ratchet that cannot see a whole class of the defect it guards is exactly
// the "green while broken" failure this file exists to prevent.
//
// ReasonNoQuantifier is the ONLY exclusion, and it is a real one rather than
// convenience: those are leaf adapters that model no quantifier for a plan
// child BY DESIGN. Folding them in would restate RFC-183 §12's over-count in
// a new form — a big number whose bulk is architecture working as intended.
// They are reported separately by ReachabilityReport and tracked as a
// modelling gap, not silently dropped.
func ReachabilityCount() int {
	reachMu.Lock()
	defer reachMu.Unlock()
	n := 0
	for _, v := range reachViolations {
		if v.Reason == ReasonAbsent || v.Reason == ReasonEmptyGroup {
			n++
		}
	}
	return n
}

// ReachabilityComparedEdges returns the number of edges actually compared
// against a group — the anti-blindness signal. See countComparedEdges.
func ReachabilityComparedEdges() int {
	reachMu.Lock()
	defer reachMu.Unlock()
	return reachCompared
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// firstDivergence localises WHY a child is unreachable: it walks the child and
// the nearest group member in parallel and names the first node where they
// stop being equal.
//
// This exists because rendered explain repeatedly cannot answer the question.
// Several PredicatesFilter violations render byte-identical on both sides
// while plans.Equals rejects them, so the deciding field is one Explain does
// not print. Without this, the only available move is to guess — and guessing
// from a lossy rendering is how a working fix nearly got reverted (a
// `[2 preds]` -> `[1 preds]` change that was a CONJUNCTION, not a dropped
// predicate) and how InJoin was briefly miscounted as 20 real defects.
//
// Reports the node TYPE and the shallow field dump of both sides. A node-local
// mismatch means the defect is at that node; a child-count mismatch means the
// shapes diverge structurally above it.
func firstDivergence(child plans.RecordQueryPlan, members []expressions.RelationalExpression) string {
	var best plans.RecordQueryPlan
	for _, m := range members {
		if mp, ok := m.(physicalPlanExpression); ok {
			if p := mp.GetRecordQueryPlan(); p != nil {
				best = p
				break
			}
		}
	}
	if best == nil {
		return "no physical member to compare against"
	}
	return describeDivergence(child, best, "")
}

func describeDivergence(a, b plans.RecordQueryPlan, path string) string {
	if a == nil || b == nil {
		return fmt.Sprintf("%s: nil vs non-nil (%T / %T)", pathOr(path), a, b)
	}
	if fmt.Sprintf("%T", a) != fmt.Sprintf("%T", b) {
		return fmt.Sprintf("%s: node TYPE differs: %T vs %T", pathOr(path), a, b)
	}
	if !a.EqualsPlanWithoutChildren(b) {
		// Node-local: same type, differing fields. This is the case explain
		// cannot show — print the shallow struct dumps so the field is named.
		return fmt.Sprintf("%s: node-local fields differ on %T\n      plan-side:  %s\n      group-side: %s",
			pathOr(path), a, shallowFields(a), shallowFields(b))
	}
	ac, bc := a.GetChildren(), b.GetChildren()
	if len(ac) != len(bc) {
		return fmt.Sprintf("%s: child COUNT differs on %T: %d vs %d", pathOr(path), a, len(ac), len(bc))
	}
	for i := range ac {
		if !plans.Equals(ac[i], bc[i]) {
			return describeDivergence(ac[i], bc[i], fmt.Sprintf("%s/%T[%d]", path, a, i))
		}
	}
	return "equal by plans.Equals (unexpected — the caller only calls this on a mismatch)"
}

func pathOr(p string) string {
	if p == "" {
		return "<root>"
	}
	return p
}

// shallowFields dumps a plan node WITHOUT its children, so the output names
// the differing field rather than re-printing the whole subtree.
func shallowFields(p plans.RecordQueryPlan) string {
	s := fmt.Sprintf("%+v", p)
	const max = 300
	if len(s) > max {
		return s[:max] + "…(truncated)"
	}
	return s
}

func divergenceLine(d string) string {
	if d == "" {
		return ""
	}
	return "\n   why-> " + d
}

// EnableReachabilityCollection turns collection on programmatically, for the
// corpus ratchet test. Returns a restore func.
//
// Exists so the test does not have to juggle RFC183_REACHABILITY around a
// sync.Once that latches on first use — a shape where the assertion silently
// measures nothing if any earlier call in the process got there first. A test
// that reports zero because it collected nothing is worse than no test.
func EnableReachabilityCollection() func() {
	reachMu.Lock()
	prev := reachEnabled
	reachEnabled = true
	reachMu.Unlock()
	// Deliberately does NOT consume reachOnce: the once-body ORs the env var
	// in rather than assigning, so whichever path runs first, neither can
	// switch the other off.
	ResetReachability()
	return func() {
		reachMu.Lock()
		reachEnabled = prev
		reachMu.Unlock()
	}
}

// NoQuantifierCount returns edges whose plan child has NO quantifier at all.
//
// Excluded from ReachabilityCount because it is dominated by leaf adapters
// that model no quantifier by design — but MEASURED AND RATCHETED separately,
// not waved through. The two-leg intersection that executed with zero memo
// edges (AggregateDataAccessRule passing nil quantifiers) was exactly a
// ReasonNoQuantifier bug, so a ratchet blind to this class cannot catch that
// defect recurring. "Explained by a category" is not "checked".
func NoQuantifierCount() int {
	reachMu.Lock()
	defer reachMu.Unlock()
	n := 0
	for _, v := range reachViolations {
		if v.Reason == ReasonNoQuantifier {
			n++
		}
	}
	return n
}

// ReachabilityEdges returns every plan child walked, compared or not — the
// denominator for the proportional anti-blindness check.
func ReachabilityEdges() int {
	reachMu.Lock()
	defer reachMu.Unlock()
	return reachEdges
}

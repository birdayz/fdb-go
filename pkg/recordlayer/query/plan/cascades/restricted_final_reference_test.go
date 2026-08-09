package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// The restriction has TWO entry points — ExpressionRuleCall.MemoizeMemberPlansFromOther
// and ImplementationRuleCall.MemoizeFinalExpressionsFromOther — and only the
// first was pinned. The second was the one carrying the live defect, across
// nine non-test call sites, and reverting its fix left the whole suite green.
// restricted_inner_reference_test.go drives the other entry point and does not
// reach this one.
//
// These pins close that, plus the two unconditional panics the restriction
// gained. A panic whose steady state is zero firings is exactly the arm nothing
// can tell you is even reachable, so each is driven directly and its message
// asserted.

// restrictedSourceRef builds a source reference holding two DISTINCT physical
// members with a populated plan-properties map.
//
// TWO members is the whole point: with one, a restricted reference and an
// unrestricted copy of the source map are indistinguishable, which is how the
// unrestricted copy survived review. The members are deliberately given
// DIFFERENT distinctness so they land in different partitions — that way the
// partition COUNT alone separates the two behaviours, without depending on how
// any single property is derived.
func restrictedSourceRef(t *testing.T) (
	ref *expressions.Reference,
	distinct *plans.RecordQueryPredicatesFilterPlan,
	nonDistinct *plans.RecordQueryProjectionPlan,
) {
	t.Helper()

	childRef := expressions.InitialOf(
		plans.NewRecordQueryScanPlan([]string{"Order"}, values.UnknownType, false))
	computeRefPlanProperties(childRef)

	// A filter over a primary scan delegates record-level distinctness to its
	// child, so it reports DistinctRecords.
	distinct = plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(
		expressions.ForEachQuantifier(childRef),
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)})
	// A projection can map two records onto one tuple, so it does not.
	nonDistinct = plans.NewRecordQueryProjectionPlanFromQuantifier(
		nil, nil, expressions.ForEachQuantifier(childRef))

	ref = expressions.InitialOf(distinct)
	ref.Insert(nonDistinct)
	computeRefPlanProperties(ref)

	// Both halves of the premise, so a later change to a property derivation
	// turns this into a loud failure rather than a quietly weaker test.
	if !computeWrapperProperties(distinct).GetBool(properties.PropDistinctRecords) {
		t.Fatal("premise broken: the filter member must report DistinctRecords, else " +
			"both members share a partition and the partition COUNT stops separating " +
			"a restricted reference from an unrestricted copy")
	}
	if computeWrapperProperties(nonDistinct).GetBool(properties.PropDistinctRecords) {
		t.Fatal("premise broken: the projection member must NOT report DistinctRecords, " +
			"for the same reason")
	}
	if got := len(ToPlanPartitions(ref)); got != 2 {
		t.Fatalf("premise broken: the SOURCE reference reports %d partitions, want 2. "+
			"The pin below distinguishes 1 from 2, so a source that already reports 1 "+
			"would make it pass under both behaviours", got)
	}
	return ref, distinct, nonDistinct
}

// TestMemoizeFinalExpressionsFromOtherRestrictsThePropertyMap is the pin for
// the entry point that carried the live bug.
//
// It counts PARTITIONS on the returned reference, because that is the exact
// mechanism by which the defect was observable: ToPlanPartitions walks the
// property MAP, not the member list. A reference restricted to one member but
// carrying the source's whole map still offers the entire source group to every
// partition-reading consumer — so the restriction is undone silently while the
// member list looks correct.
//
// MUTATION: revert implementation_rule.go to
// `ref.SetPlanProperties(source.GetPlanProperties())` and this goes RED with
// "reports 2 partitions, want 1".
func TestMemoizeFinalExpressionsFromOtherRestrictsThePropertyMap(t *testing.T) {
	t.Parallel()

	source, distinct, _ := restrictedSourceRef(t)
	call := &ImplementationRuleCall{}

	restricted := call.MemoizeFinalExpressionsFromOther(
		source, []expressions.RelationalExpression{distinct})

	parts := ToPlanPartitions(restricted)
	if len(parts) != 1 {
		t.Fatalf("the restricted reference reports %d partitions, want 1. "+
			"ToPlanPartitions walks the plan-property MAP rather than the member list, "+
			"so copying the source's whole map onto a reference restricted to one "+
			"member leaves it offering the ENTIRE source group to every "+
			"partition-reading consumer — the restriction undone silently while the "+
			"member list still looks right", len(parts))
	}

	// The partition must be the one the retained member belongs to, not merely
	// some single partition.
	got := parts[0].GetExpressions()
	if len(got) != 1 || got[0] != distinct {
		t.Errorf("the sole partition holds %d expression(s) and does not contain exactly "+
			"the retained member; a restricted reference must describe the same set of "+
			"plans its member list does", len(got))
	}

	// The member list itself was never wrong, so asserting it alone would have
	// passed under the defect. Pinned anyway so the two cannot drift apart.
	if members := restricted.FinalMembers(); len(members) != 1 || members[0] != distinct {
		t.Errorf("restricted reference holds %d final members, want exactly the one "+
			"retained member", len(members))
	}
}

// TestRestrictedFinalReferencePanicsOnAMissingPropertyEntry drives the
// nil-property arm. Measured zero firings across 15739 real calls is why the
// panic is SAFE; it is also why nothing else can tell anyone the arm is
// reachable or that its message is intact.
//
// The fixture is the exact state the arm exists to reject: a source that HAS a
// property map (so the restriction runs) which is SHORT of a member that is
// genuinely in the group (so the copy misses). Skipping it silently would mint
// a member-short map, and the plan would then belong to no partition at all.
func TestRestrictedFinalReferencePanicsOnAMissingPropertyEntry(t *testing.T) {
	t.Parallel()

	source, distinct, nonDistinct := restrictedSourceRef(t)

	// Overwrite the source's map with one that covers only ONE of the two
	// members, leaving the other a genuine member with no entry.
	short := NewPlanPropertiesMap()
	short.Set(distinct, computeWrapperProperties(distinct))
	source.SetPlanProperties(short)
	if GetRefPlanPropertiesMap(source).GetProperties(nonDistinct) != nil {
		t.Fatal("premise broken: the source map must be SHORT of nonDistinct, else the " +
			"arm under test is unreachable and this test proves nothing")
	}

	call := &ImplementationRuleCall{}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("no panic: a member with no property entry was silently SKIPPED. " +
				"That mints a non-nil but member-short map, and ToPlanPartitions walks " +
				"the map — so the plan belongs to no partition and the alternative " +
				"disappears without a word")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value is %T, want a string message", r)
		}
		// The message is the whole value of a panic on a path that never fires
		// in practice: it is what tells the next person what to do about it.
		for _, want := range []string{
			"MemoizeFinalExpressionsFromOther", // names WHICH entry point
			"no entry for retained member",     // names the condition
			"computeRefPlanProperties",         // names the remedy
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("panic message does not mention %q; it reads:\n%s", want, msg)
			}
		}
	}()

	call.MemoizeFinalExpressionsFromOther(
		source, []expressions.RelationalExpression{nonDistinct})
}

// TestRestrictedFinalReferencePanicsOnANonMember drives assertMembersOf — the
// half of Java's Reference.java:588 precondition that DOES port to Go.
//
// A parent built over a non-member ranges over a plan its own group never
// offered, which is unsound in a way no downstream assertion would attribute
// back here.
func TestRestrictedFinalReferencePanicsOnANonMember(t *testing.T) {
	t.Parallel()

	source, _, _ := restrictedSourceRef(t)
	// A plan that is a perfectly good expression but belongs to a DIFFERENT
	// group — the shape a refactor would introduce by passing the wrong
	// reference alongside the right members.
	stranger := plans.NewRecordQueryScanPlan([]string{"Customer"}, values.UnknownType, false)

	call := &ImplementationRuleCall{}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("no panic: a restricted reference was built over an expression that " +
				"is NOT a member of the source group. Both callers pass real members " +
				"today, which is exactly why the invariant is one refactor away from " +
				"being untrue and unchecked")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value is %T, want a string message", r)
		}
		for _, want := range []string{
			"MemoizeFinalExpressionsFromOther",
			"is not a member of the source reference",
			"Reference.java:588", // names the Java precondition being ported
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("panic message does not mention %q; it reads:\n%s", want, msg)
			}
		}
	}()

	call.MemoizeFinalExpressionsFromOther(
		source, []expressions.RelationalExpression{stranger})
}

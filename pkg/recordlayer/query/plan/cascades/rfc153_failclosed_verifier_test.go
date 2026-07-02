package cascades

// RFC-153 — planReferencesAnyBuriedAlias must FAIL-CLOSED on plan node types the
// buried-merge rebaser (rebasePlanBuriedRefs) does NOT rewrite. The original verifier
// only inspected Scan/Index SARG comparands, PredicatesFilter/Filter preds, and Map
// result values — so a node that carries a buried-preserved correlation in its OWN field
// (a nested NLJ/FlatMap's preds, an InJoin/InUnion comparand, an Aggregate/StreamingAgg
// grouping) fell through the switch, `found` stayed false, the node was reported CLEAN,
// and the probe shipped with an unbound buried correlation → WRONG ROWS (the §2 trap).
// The fix: any node that is neither an explicitly-inspected type nor a known
// correlation-free pass-through is treated as MIGHT-reference-buried → decline the
// probe → materialized NLJ fallback.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func rfc153BuriedFieldOfA() *values.FieldValue {
	aliasA := values.NamedCorrelationIdentifier("A")
	return values.NewFieldValue(values.NewQuantifiedObjectValue(aliasA), "id", values.UnknownType)
}

// TestRFC153_Verifier_RecognizedCleanInner_NoDecline: a recognized data-access inner
// with NO buried reference (a bare Scan, an index scan SARGed only on a literal) must
// NOT be flagged — a legitimate buried-free probe must still fire (no over-decline of
// the inners the rebaser fully understands).
func TestRFC153_Verifier_RecognizedCleanInner_NoDecline(t *testing.T) {
	t.Parallel()
	buried := []string{"A"}

	bareScan := plans.NewRecordQueryScanPlan([]string{"C"}, values.UnknownType, false)
	if planReferencesAnyBuriedAlias(bareScan, buried) {
		t.Error("bare Scan (no SARG) must NOT be flagged — legit probe must fire")
	}

	litEq := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(7))
	litRange := predicates.EmptyComparisonRange().Merge(&litEq).Range
	litProbe := plans.NewRecordQueryIndexPlan("c_idx", []*predicates.ComparisonRange{litRange}, []string{"C"}, values.UnknownType, false)
	if planReferencesAnyBuriedAlias(litProbe, buried) {
		t.Error("index probe SARGed on a LITERAL must NOT be flagged — legit probe must fire")
	}
}

// TestRFC153_Verifier_RecognizedSargWithBuriedRef_Declines: a recognized node (an index
// scan) whose SARG comparand STILL references the buried alias (the rebase was
// incomplete — an alias mismatch) must be flagged and decline.
func TestRFC153_Verifier_RecognizedSargWithBuriedRef_Declines(t *testing.T) {
	t.Parallel()
	buriedEq := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: rfc153BuriedFieldOfA()}
	buriedRange := predicates.EmptyComparisonRange().Merge(&buriedEq).Range
	buriedProbe := plans.NewRecordQueryIndexPlan("c_idx", []*predicates.ComparisonRange{buriedRange}, []string{"C"}, values.UnknownType, false)
	if !planReferencesAnyBuriedAlias(buriedProbe, []string{"A"}) {
		t.Error("an index SARG still referencing the buried alias A must be flagged (incomplete rebase) → decline")
	}
	// Case-insensitive: the same buried ref under a lower-case leg-alias spelling.
	if !planReferencesAnyBuriedAlias(buriedProbe, []string{"a"}) {
		t.Error("buried-alias match must be case-insensitive")
	}
}

// TestRFC153_Verifier_UnrecognizedNode_FailsClosed: THE bug every reviewer
// flagged. A node type the rebaser's walker does NOT rewrite (here a nested
// NestedLoopJoin — same class as StreamingAgg/InJoin/FlatMap whose own preds/comparand/
// grouping carry correlation) must FAIL-CLOSED → flagged → decline, EVEN when the
// buried reference lives in the node's OWN field (which the verifier does not inspect).
// The old verifier let this through (found stayed false) → wrong-rows probe.
func TestRFC153_Verifier_UnrecognizedNode_FailsClosed(t *testing.T) {
	t.Parallel()
	resVal := values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
	cleanScan := plans.NewRecordQueryScanPlan([]string{"C"}, values.UnknownType, false)

	// A NestedLoopJoin whose join predicate references the buried alias A — the buried
	// ref is in the NLJ's OWN GetPredicates(), a field the verifier never inspects.
	buriedPred := predicates.NewComparisonPredicate(rfc153BuriedFieldOfA(), predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))
	nljWithBuried := plans.NewRecordQueryNestedLoopJoinPlan(
		cleanScan, cleanScan, []predicates.QueryPredicate{buriedPred},
		plans.JoinInner, "", "", resVal,
	)
	if !planReferencesAnyBuriedAlias(nljWithBuried, []string{"A"}) {
		t.Fatal("FAIL-CLOSED BROKEN: an unrecognized node (NLJ) carrying the buried ref in its OWN predicate was reported CLEAN — the §2 wrong-rows trap")
	}

	// Even an unrecognized node with NO buried reference fails closed (conservative
	// over-decline into the correct-but-slow materialized NLJ — never under-catches).
	cleanNLJ := plans.NewRecordQueryNestedLoopJoinPlan(
		cleanScan, cleanScan, nil, plans.JoinInner, "", "", resVal,
	)
	if !planReferencesAnyBuriedAlias(cleanNLJ, []string{"A"}) {
		t.Error("an unrecognized node type must fail-CLOSED regardless of whether it carries a buried ref (conservative over-decline)")
	}

	// An unrecognized node nested BELOW a recognized pass-through (DefaultOnEmpty) is
	// still reached by the recursion and fails closed.
	wrapped := plans.NewRecordQueryDefaultOnEmptyPlan(nljWithBuried, values.NewNullValue(values.UnknownType))
	if !planReferencesAnyBuriedAlias(wrapped, []string{"A"}) {
		t.Error("an unrecognized node nested under a pass-through must still fail-CLOSED")
	}
}

// TestRFC153_Verifier_PassThroughsAreTransparent: the known correlation-free
// pass-throughs (Fetch/TypeFilter/DefaultOnEmpty/FirstOrDefault) do NOT themselves
// trigger a decline — only their children matter. A pass-through over a clean recognized
// scan stays a fireable probe.
func TestRFC153_Verifier_PassThroughsAreTransparent(t *testing.T) {
	t.Parallel()
	cleanScan := plans.NewRecordQueryScanPlan([]string{"C"}, values.UnknownType, false)
	doe := plans.NewRecordQueryDefaultOnEmptyPlan(cleanScan, values.NewNullValue(values.UnknownType))
	tf := plans.NewRecordQueryTypeFilterPlan([]string{"C"}, doe)
	if planReferencesAnyBuriedAlias(tf, []string{"A"}) {
		t.Error("pass-throughs (TypeFilter/DefaultOnEmpty) over a clean scan must NOT decline — they carry no buried correlation in their own fields")
	}
}

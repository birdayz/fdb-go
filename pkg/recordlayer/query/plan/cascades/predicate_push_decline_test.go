package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// scanOfRow builds a scan whose flowed row is the given type, so two scans can
// be made to DISAGREE on row shape — the condition pushOverChild declines on.
func scanOfRow(t testing.TB, name string, row values.Type) expressions.RelationalExpression {
	t.Helper()
	scan, err := expressions.NewFullUnorderedScanExpression([]string{name}, row)
	if err != nil {
		t.Fatalf("scan %s: %v", name, err)
	}
	return scan
}

// TestPushOverChildDeclineIsNotARunFailure pins that "do not push this
// predicate" is answered as a DECLINE and not as a broken quantifier.
//
// pushOverChild used to reply (expressions.Quantifier{}, nil) when its
// same-row precondition failed. That is a lawful, expected answer — the two
// aliases do not denote the same row, so the rebase would silently retarget
// ordinals — but every caller tested only the error, so the ZERO quantifier
// flowed onward into an expression constructor. RequireFlowedObjectValue then
// rejected a quantifier with no Reference, the rule call failed, and the whole
// planning run died over a predicate it merely should not have pushed.
//
// No test reached that branch, which is why the zero value could sit there: the
// decline is rare, and when it did fire it presented as an internal error
// rather than as a rule choosing not to fire.
func TestPushOverChildDeclineIsNotARunFailure(t *testing.T) {
	t.Parallel()

	wide := values.NewRecordType("wide", false, []values.Field{
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "B", FieldType: values.NotNullLong, Ordinal: 1},
	})
	narrow := values.NewRecordType("narrow", false, []values.Field{
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 0},
	})

	pushQuantifier := expressions.ForEachQuantifier(
		expressions.InitialOf(scanOfRow(t, "WIDE", wide)))
	child := expressions.ForEachQuantifier(
		expressions.InitialOf(scanOfRow(t, "NARROW", narrow)))

	// The precondition this whole test is about. If these ever agree, the arms
	// below stop exercising the decline and would pass for the wrong reason.
	if rebasedAliasesDenoteOneRow(pushQuantifier, child) {
		t.Fatal("test precondition failed: a 2-column and a 1-column row were reported as " +
			"denoting the same row, so pushOverChild would not decline and this test is vacuous")
	}

	call := NewExpressionRuleCall(expressions.InitialOf(fixtureScan("PUSH-DECLINE")), nil, nil)
	preds := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}

	quantifier, pushed, err := pushOverChild(call, preds, pushQuantifier, child)
	if err != nil {
		t.Fatalf("a declined push reported an ERROR (%v). Declining is not failing: the rule "+
			"simply must not fire here", err)
	}
	if pushed {
		t.Fatal("pushOverChild claimed it pushed over a child whose row shape disagrees; " +
			"the rebase would retarget ordinals onto a different row")
	}
	if quantifier.GetRangesOver() != nil {
		t.Fatal("a declined push still returned a quantifier; the caller would hand it to an " +
			"expression constructor")
	}

	// The callers are the half that mattered: each must SKIP the rewrite rather
	// than build over the declined result.
	sort, err := expressions.NewLogicalSortExpression(nil, child)
	if err != nil {
		t.Fatalf("sort fixture: %v", err)
	}
	rewritten, err := pushThroughSort(call, preds, pushQuantifier, sort)
	if err != nil {
		t.Fatalf("a declined push through a Sort FAILED THE RULE (%v) instead of not firing — "+
			"this is the run-killing path", err)
	}
	if rewritten != nil {
		t.Fatalf("a declined push through a Sort still produced a rewrite (%T)", rewritten)
	}
}

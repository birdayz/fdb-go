package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// disagreeingStubExpr is a relational expression whose result value flows a
// chosen ROW type, so a Reference can be given two members that flow DIFFERENT
// rows — the memo defect expressions.MemberResultTypeDisagreementError reports.
type disagreeingStubExpr struct {
	name string
	typ  *values.RecordType
}

func (s *disagreeingStubExpr) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(s.name), s.typ)
}
func (s *disagreeingStubExpr) GetQuantifiers() []expressions.Quantifier { return nil }
func (s *disagreeingStubExpr) CanCorrelate() bool                       { return false }
func (s *disagreeingStubExpr) ChildrenAsSet() bool                      { return false }
func (s *disagreeingStubExpr) HashCodeWithoutChildren() uint64          { return 0 }

func (s *disagreeingStubExpr) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (s *disagreeingStubExpr) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*disagreeingStubExpr)
	return ok && o.name == s.name
}

func (s *disagreeingStubExpr) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return s
}

// disagreementProbeAlias is the quantifier alias this test's disagreeing
// reference is bound under. Its spelling is deliberately used by nothing else in
// the package, so the assertions below can name their OWN witness in a
// package-scoped counter under t.Parallel — the same discipline the leg-identity
// census's per-test witness uses. Resetting the counter instead would discard
// every concurrently-running sibling's contribution.
const disagreementProbeAlias = "Q$MERGEDISAGREE"

// TestPositionalMergeObservesMemberDisagreement pins the OBSERVATION on
// positionalMergeCase's member-disagreement decline.
//
// The decline itself is right: two members of one equivalence class flowing
// different row shapes means no member is authoritative, and picking one would
// choose a merge slot type by memo insertion order. What was missing is that the
// decline was SILENT — indistinguishable from the arm simply not applying — while
// MemberResultTypeDisagreementError's own doc says a disagreement is a memo defect
// that must surface. Java Verify-fails here; Go counts and witnesses instead,
// because Go's untyped-member reporting gap makes the error reachable without a
// real defect.
//
// Without this test the observation is a write-only counter: nothing would notice
// if the recording call were dropped, or if a future refactor moved the decline
// above it.
func TestPositionalMergeObservesMemberDisagreement(t *testing.T) {
	t.Parallel()

	// A reference whose two members flow DIFFERENT rows. This is the memo defect.
	ab := values.NewRecordType("AB", false, []values.Field{
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "B", FieldType: values.NotNullLong, Ordinal: 1},
	})
	abc := values.NewRecordType("ABC", false, []values.Field{
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "B", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "C", FieldType: values.NotNullLong, Ordinal: 2},
	})
	badRef := expressions.InitialOf(&disagreeingStubExpr{name: "m1", typ: ab})
	if !badRef.Insert(&disagreeingStubExpr{name: "m2", typ: abc}) {
		t.Fatal("fixture: the second member was not inserted, so the reference does " +
			"not disagree and this test measures nothing")
	}
	bad := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(disagreementProbeAlias), badRef)
	good := scanQuantifier("GOOD")

	sel := expressions.NewSelectExpressionWithAliases(
		good.GetFlowedObjectValue(),
		[]expressions.Quantifier{bad, good},
		nil,
		[]string{disagreementProbeAlias, "GOOD"},
	)

	aliasToQ := map[values.CorrelationIdentifier]expressions.Quantifier{
		bad.GetAlias():  bad,
		good.GetAlias(): good,
	}
	live := []values.CorrelationIdentifier{bad.GetAlias(), good.GetAlias()}
	lowerBuilder := NewGraphExpansionBuilder()
	lowerBuilder.AddQuantifier(bad)
	lowerBuilder.AddQuantifier(good)

	before, _ := MergeSlotTypeDisagreements()

	r := &PartitionSelectRule{}
	got := r.positionalMergeCase(&ExpressionRuleCall{}, sel, sel.GetResultValue(),
		aliasToQ, live, map[values.CorrelationIdentifier]struct{}{}, live, lowerBuilder, nil)

	// The decline is unchanged — this test must not turn into a licence to yield.
	if got != nil {
		t.Fatalf("positionalMergeCase yielded %v on a disagreeing reference — it must "+
			"DECLINE, not pick a merge slot type by insertion order", got)
	}
	// And the decline is now observable. Read as a LOWER bound: the counter is
	// package-scoped and siblings run in parallel, so extra traffic can only make
	// this fail, never falsely pass.
	after, witnesses := MergeSlotTypeDisagreements()
	if after <= before {
		t.Fatalf("MergeSlotTypeDisagreements() stayed at %d across a declining merge — "+
			"the decline recorded nothing, so a memo defect is again indistinguishable "+
			"from 'the arm does not apply'", after)
	}
	// The witness has to IDENTIFY the equivalence class, or it cannot be acted on.
	var mine string
	for _, w := range witnesses {
		if strings.Contains(w, disagreementProbeAlias) {
			mine = w
			break
		}
	}
	if mine == "" {
		t.Fatalf("no witness names %s among %v (count=%d).\n"+
			"  A count with no witness cannot be traced to the member that disagreed. If\n"+
			"  the witness set is SATURATED at its cap, that is itself the finding: this\n"+
			"  package produced other real disagreements.",
			disagreementProbeAlias, witnesses, after)
	}
	// Both row shapes, so a witness that merely names the alias does not satisfy this.
	if !strings.Contains(mine, "disagree") {
		t.Errorf("witness %q does not say what went wrong", mine)
	}

	// The healthy path must NOT record. Same select shape with both quantifiers
	// over single-member references: the arm proceeds, and no witness names its
	// aliases — so the observation counts defects rather than merges. (Asserting on
	// the witnesses rather than on the count is what keeps this valid beside a
	// parallel sibling.)
	okA := scanQuantifier("Q$MERGEAGREEA")
	okB := scanQuantifier("Q$MERGEAGREEB")
	okSel := expressions.NewSelectExpressionWithAliases(
		okA.GetFlowedObjectValue(),
		[]expressions.Quantifier{okA, okB},
		nil,
		[]string{"Q$MERGEAGREEA", "Q$MERGEAGREEB"},
	)
	okLower := NewGraphExpansionBuilder()
	okLower.AddQuantifier(okA)
	okLower.AddQuantifier(okB)
	okLive := []values.CorrelationIdentifier{okA.GetAlias(), okB.GetAlias()}
	if merged := r.positionalMergeCase(&ExpressionRuleCall{}, okSel, okSel.GetResultValue(),
		map[values.CorrelationIdentifier]expressions.Quantifier{
			okA.GetAlias(): okA, okB.GetAlias(): okB,
		}, okLive, map[values.CorrelationIdentifier]struct{}{}, okLive, okLower, nil); merged == nil {
		t.Fatal("control: the AGREEING merge declined, so it exercises the same arm as " +
			"the defect and cannot show that the observation discriminates")
	}
	_, witnesses = MergeSlotTypeDisagreements()
	for _, w := range witnesses {
		if strings.Contains(w, "Q$MERGEAGREE") {
			t.Errorf("an AGREEING merge produced the witness %q — the observation must "+
				"count memo defects, not merges", w)
		}
	}
}

package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// stackedProjections builds Project([outerVals]) over Project([innerVals]) over Scan("T").
func stackedProjections(outerVals, innerVals []values.Value) *expressions.LogicalProjectionExpression {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerProj := expressions.NewLogicalProjectionExpression(innerVals, innerQ)
	outerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerProj))
	return expressions.NewLogicalProjectionExpression(outerVals, outerQ)
}

// outerRead is an outer projection slot reading the inner projection's output
// slot `ord`. It is BAKED, because that is the only shape the resolver hands
// this rule: an outer read of a projection output arrives carrying its output
// ordinal. Tests used to build these LAZY (name only), which exercised a
// composition arm no query could reach — see
// TestProjectionMergeRule_LazyOuterReadDeclines.
func outerRead(name string, ord int) *values.FieldValue {
	return values.NewFieldValueWithResolvedOrdinal(name, ord, values.UnknownType)
}

func TestProjectionMergeRule_Fires(t *testing.T) {
	t.Parallel()
	outerVals := []values.Value{outerRead("id", 0)}
	innerVals := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
		&values.FieldValue{Field: "name", Typ: values.UnknownType},
		&values.FieldValue{Field: "age", Typ: values.UnknownType},
	}
	stacked := stackedProjections(outerVals, innerVals)
	ref := expressions.InitialOf(stacked)
	rule := NewProjectionMergeRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("rule yielded %d expressions, want 1", len(yielded))
	}
	flat, ok := yielded[0].(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalProjectionExpression", yielded[0])
	}
	// Outer projection list preserved — exactly one entry, the inner's slot 0.
	pv := flat.GetProjectedValues()
	if len(pv) != 1 {
		t.Fatalf("flat projected values len=%d, want 1", len(pv))
	}
	fv, ok := pv[0].(*values.FieldValue)
	if !ok || fv.Field != "id" {
		t.Fatalf("flat projected[0] = %v, want FieldValue(id)", pv[0])
	}
	// Inner of the flat projection is the Scan, not the inner projection.
	if _, ok := flat.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("flat inner = %T, want *FullUnorderedScanExpression", flat.GetInner().GetRangesOver().Get())
	}
}

func TestProjectionMergeRule_DeclinesOnNonProjectionInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}},
		q,
	)
	ref := expressions.InitialOf(proj)
	rule := NewProjectionMergeRule()
	yielded := FireExpressionRule(rule, ref)
	if got := len(yielded); got != 0 {
		t.Fatalf("rule yielded %d on non-projection inner, want 0", got)
	}
}

// TestProjectionMergeRule_LazyOuterReadDeclines is the pin that earns the
// removal of this rule's name-matching composition arm (RFC-197, name-keyed).
//
// A LAZY outer read carries a display name and nothing else. The removed arm
// selected an inner slot by unique output-name match, which is the RFC's
// forbidden move in its purest form: a name choosing a slot. The arm was
// MEASURED to take ZERO real traffic — over the whole ./pkg/relational/...
// suite (FDB sqldriver, all four conformance corpora, yamsql, rowdiff) the
// rule fires 897 times and the lazy arm is entered on none of them; every
// outer read arrives baked, because the resolver bakes a projection-output
// reference to its output ordinal. So the arm is now a fail-closed DECLINE.
//
// If this goes red, the arm has come back and a display name is selecting a
// slot again. If instead a LOST merge shows up in a plan golden, the upstream
// resolver-side baking regressed and the decline started costing something —
// that is the recoverable direction, and it is the one this rule chooses.
func TestProjectionMergeRule_LazyOuterReadDeclines(t *testing.T) {
	t.Parallel()
	outerVals := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType}, // LAZY: Resolved == nil
	}
	innerVals := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
		&values.FieldValue{Field: "name", Typ: values.UnknownType},
	}
	ref := expressions.InitialOf(stackedProjections(outerVals, innerVals))
	yielded := FireExpressionRule(NewProjectionMergeRule(), ref)
	if got := len(yielded); got != 0 {
		t.Fatalf("rule yielded %d on a LAZY outer read, want 0 (decline). "+
			"A lazy read carries only a display name; composing it means a name "+
			"selecting an inner slot, which RFC-197 forbids and which this rule "+
			"no longer does. Re-armed by restoring the unique-output-name match arm",
			got)
	}
}

// TestProjectionMergeCensus_CountsTheLazyArmItGuards keeps the corpus-level zero
// in explaindiff from being a broken detector reading as coverage.
//
// That test asserts LazyOuterReads == 0 over the whole corpus. A counter that is
// mis-wired, or wired behind the guard it is meant to observe, produces the same
// zero and reads as good news. So this drives a lazy outer read deliberately and
// requires the count to MOVE.
//
// It asserts a DELTA rather than an absolute, and does not Reset: the counters
// are package-scoped and sibling tests in this binary fire the same rule in
// parallel. Concurrent firings can only ADD, so a strictly-increased assertion is
// SAFE under them where an absolute one would not be. Safe is not exact — if this
// test's own read recorded nothing and a sibling recorded one, `after > before`
// still passes. What it does catch deterministically is the mutation that
// matters: a counter that cannot rise at all, or one wired behind the guard it is
// meant to observe, never moves for anybody.
func TestProjectionMergeCensus_CountsTheLazyArmItGuards(t *testing.T) {
	t.Parallel()

	before := ProjectionMergeCensusSnapshot()

	outerVals := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType}, // LAZY
	}
	innerVals := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
	}
	FireExpressionRule(NewProjectionMergeRule(), expressions.InitialOf(stackedProjections(outerVals, innerVals)))

	after := ProjectionMergeCensusSnapshot()
	if after.LazyOuterReads <= before.LazyOuterReads {
		t.Fatalf("LazyOuterReads did not move (%d -> %d) after a LAZY outer read "+
			"reached the rule. The corpus census in explaindiff asserts this count "+
			"is ZERO; if the counter cannot rise, that zero proves nothing",
			before.LazyOuterReads, after.LazyOuterReads)
	}
	if after.RuleFirings <= before.RuleFirings {
		t.Fatalf("RuleFirings did not move (%d -> %d). It is the denominator that "+
			"keeps the lazy zero from being vacuous; a denominator that cannot rise "+
			"cannot do that job", before.RuleFirings, after.RuleFirings)
	}
}

// TestProjectionMergeCensus_CountsTheOrdinalArmToo pins the other half. A census
// that only ever moves on the arm expected to be empty cannot distinguish "the
// rule composed nothing" from "the rule composed everything by ordinal", which is
// the exact confusion the explaindiff vacuity guards exist to prevent.
func TestProjectionMergeCensus_CountsTheOrdinalArmToo(t *testing.T) {
	t.Parallel()

	before := ProjectionMergeCensusSnapshot()

	outerVals := []values.Value{outerRead("id", 0)}
	innerVals := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
		&values.FieldValue{Field: "name", Typ: values.UnknownType},
	}
	FireExpressionRule(NewProjectionMergeRule(), expressions.InitialOf(stackedProjections(outerVals, innerVals)))

	after := ProjectionMergeCensusSnapshot()
	if after.BakedSingleAccessor <= before.BakedSingleAccessor {
		t.Fatalf("BakedSingleAccessor did not move (%d -> %d) after an ORDINAL-baked "+
			"outer read composed. explaindiff fails when this arm reads zero over the "+
			"corpus, on the grounds that a corpus which never merges cannot testify "+
			"about the removed name arm; that guard needs a counter that rises",
			before.BakedSingleAccessor, after.BakedSingleAccessor)
	}
}

// TestProjectionMergeRule_DuplicateInnerSlotNames_OrdinalPicksTheRightSlot is
// the DIMENSION the removed name arm could never handle and never had a test
// for: two inner slots sharing ONE output name.
//
// Under the name arm this shape was ambiguous and the whole merge declined —
// so a perfectly composable projection lost its merge because two unrelated
// slots happened to spell the same. Under ordinal composition the outer read's
// own ordinal answers, and the merge fires on the RIGHT slot. Both halves are
// asserted: that it fires at all, and that it picked slot 1's value rather
// than slot 0's.
func TestProjectionMergeRule_DuplicateInnerSlotNames_OrdinalPicksTheRightSlot(t *testing.T) {
	t.Parallel()
	// Two inner slots, both named K, distinguishable ONLY by ordinal.
	innerVals := []values.Value{
		&values.FieldValue{Field: "LEFT_SOURCE", Typ: values.UnknownType},
		&values.FieldValue{Field: "RIGHT_SOURCE", Typ: values.UnknownType},
	}
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerProj := expressions.NewLogicalProjectionExpressionWithAliases(innerVals, []string{"K", "K"}, innerQ)

	outerVals := []values.Value{outerRead("K", 1)} // the SECOND K
	outerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerProj))
	outer := expressions.NewLogicalProjectionExpression(outerVals, outerQ)

	yielded := FireExpressionRule(NewProjectionMergeRule(), expressions.InitialOf(outer))
	if len(yielded) != 1 {
		t.Fatalf("rule yielded %d on duplicate inner slot names, want 1 — an "+
			"ordinal-composed merge does not care that two slots spell the same",
			len(yielded))
	}
	flat := yielded[0].(*expressions.LogicalProjectionExpression)
	pv := flat.GetProjectedValues()
	if len(pv) != 1 {
		t.Fatalf("flat projected values len=%d, want 1", len(pv))
	}
	got, ok := pv[0].(*values.FieldValue)
	if !ok || got.Field != "RIGHT_SOURCE" {
		t.Fatalf("merged slot 0 composed to %v, want FieldValue(RIGHT_SOURCE) — "+
			"the outer read is baked at ordinal 1, so it must substitute the "+
			"SECOND inner slot; picking LEFT_SOURCE is the same-leaf-name "+
			"conflation RFC-197 exists to stop", pv[0])
	}
}

func TestProjectionMergeRule_TriplyNested_FlattensInTwoFires(t *testing.T) {
	t.Parallel()
	// Project([id]) over Project([id, name]) over Project([id, name, age]) over Scan
	deepest := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
		&values.FieldValue{Field: "name", Typ: values.UnknownType},
		&values.FieldValue{Field: "age", Typ: values.UnknownType},
	}
	middle := []values.Value{outerRead("id", 0), outerRead("name", 1)}
	top := []values.Value{outerRead("id", 0)}

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	deepProj := expressions.NewLogicalProjectionExpression(deepest, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	midProj := expressions.NewLogicalProjectionExpression(middle, expressions.ForEachQuantifier(expressions.InitialOf(deepProj)))
	topProj := expressions.NewLogicalProjectionExpression(top, expressions.ForEachQuantifier(expressions.InitialOf(midProj)))

	// Drive through the unified exploration driver so the rule
	// re-fires on its own yields until stable.
	ref := expressions.InitialOf(topProj)
	if _, conv := exploreRewriting(NewPlanner([]ExpressionRule{NewProjectionMergeRule()}, nil), ref); !conv {
		t.Fatal("exploration did not converge")
	}
	// After the rule applies twice, we should have a 1-deep projection
	// over the scan. The Reference's last yielded member is the flattest.
	members := ref.Members()
	flatFound := false
	for _, m := range members {
		p, ok := m.(*expressions.LogicalProjectionExpression)
		if !ok {
			continue
		}
		// Look at the immediate inner: is it the scan?
		if _, ok := p.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression); ok && len(p.GetProjectedValues()) == 1 {
			flatFound = true
			break
		}
	}
	if !flatFound {
		t.Fatalf("exploration did not produce a 1-deep projection over Scan; members=%d", len(members))
	}
}

// TestProjectionMergeRule_PinsOuterEffectiveNames pins the merged
// projection's OUTPUT SCHEMA: an outer projection whose output names come
// from its VALUES' own field names (alias list nil) must keep those names
// after composition substitutes the inner's values — the field name rides the
// replaced value, so the merge must pin each slot's effective name
// (OutputColumnName) as the merged alias. Pre-fix the merged output regressed
// to the INNER values' names — for a CTE
// `WITH c AS (SELECT la.k AS ak, lb.k AS bk ...) SELECT ak, bk FROM c`
// the output schema became the dup-bare [K, K] and the consumer's column
// VANISHED (the RFC-186 winner-flip triage's wrong-rows case).
//
// The CTE-consumer reads are BAKED here, which is what that consumer actually
// produces today; the earlier lazy spelling exercised a composition arm no
// query reaches (see TestProjectionMergeRule_LazyOuterReadDeclines). The
// schema hazard is unchanged by that: the inner's dup-bare [K, K] must still
// not leak.
func TestProjectionMergeRule_PinsOuterEffectiveNames(t *testing.T) {
	t.Parallel()

	// Inner projection: two values whose bare names collide (K, K),
	// aliased apart as AK / BK — the CTE-body shape.
	innerVals := []values.Value{
		&values.FieldValue{Field: "K", Typ: values.UnknownType},
		&values.FieldValue{Field: "K", Typ: values.UnknownType},
	}
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerProj := expressions.NewLogicalProjectionExpressionWithAliases(innerVals, []string{"AK", "BK"}, innerQ)

	// Outer projection: the CTE-consumer shape (`SELECT "AK", "BK" FROM "C"`),
	// reading the body's output slots 0 and 1, with NO alias list.
	outerVals := []values.Value{outerRead("AK", 0), outerRead("BK", 1)}
	outerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerProj))
	outer := expressions.NewLogicalProjectionExpression(outerVals, outerQ)

	ref := expressions.InitialOf(outer)
	yielded := FireExpressionRule(NewProjectionMergeRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("rule yielded %d expressions, want 1", len(yielded))
	}
	flat, ok := yielded[0].(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalProjectionExpression", yielded[0])
	}
	pv := flat.GetProjectedValues()
	al := flat.GetAliases()
	if len(pv) != 2 {
		t.Fatalf("flat projected values len=%d, want 2", len(pv))
	}
	for i, want := range []string{"AK", "BK"} {
		alias := ""
		if al != nil {
			alias = al[i]
		}
		if got := values.OutputColumnName(pv[i], alias); got != want {
			t.Fatalf("merged output name[%d] = %q, want %q (outer effective name must survive the merge; inner names [K K] must not leak)", i, got, want)
		}
	}
}

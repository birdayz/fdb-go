package embedded

// The post-aggregate PULL-UP guard: a reference that answers to MORE THAN ONE
// grouping key does not determine a slot, so the engine stops instead of binding
// the first match.
//
// This is Java's structure, not an invention. Expressions.pullUp asserts
// `pulledUpExpressionMap.get(subExpression).size() == 1` with AMBIGUOUS_COLUMN
// before taking the element (Expressions.java:112 at 4.12.11.0); its HAVING twin
// Expression.pullUp ends in a bare Iterables.getOnlyElement (Expression.java:246)
// which throws on more than one without that SQLSTATE. Both guard LOCALLY at the
// pull-up and delegate to no upstream duplicate-key gate. Go raises 42702 on both
// halves — the deliberate refinement is spending the precise code where Java
// throws unclassified.
//
// WHY THESE ARE UNIT PINS, and it is now true of ALL THREE sites rather than
// one. No SQL shape in the corpus drives any of them to a multi-match: the
// output-construction pull-up refuses duplicates first, so every duplicate
// query dies before a post-aggregate walk can see two candidates. Measured over
// the whole //pkg/relational/sqldriver target at 6158 subtests, the three sites
// are consulted 797 times (binder 414, computed 360, FieldValue walk 23) and not
// one consultation sees more than one match.
//
// An earlier revision of this header named e2e arms as driving two of them —
// TestFDB_ComputedGroupKeyRereadBindsItsOwnSlot's under_a_join_… arm and
// TestFDB_OrderedGroupedScalarSubquery_QualifiedJoinKeyIdentity's
// repeated_equivalent_single_source_keys_… arm. Both of those now die at
// construction, so neither reaches the walk it was credited with, and the same
// file said so 200 lines below. The claim is deleted rather than softened.
//
// The COMPUTED walk was the first to become unreachable and its reason is its
// own: the duplicate gate refuses the byte-identical computed twin before the
// walk runs, and the parenthesised twin — the one spelling that slips the gate —
// yields keys [RecordConstructorValue, ArithmeticValue], which a reference
// matches exactly ONCE. So it is a ported assert with no live caller, and
// without the arms below every one of the three would be an untested branch
// that a later normalization change makes live.
//
// The population is stated deliberately: the 6158-subtest reading is the
// //pkg/relational/sqldriver target only. The yamsql corpus and
// //pkg/relational/core/embedded were NOT instrumented for reachability.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// gpugAgg builds an aggregate whose group keys are exactly the given values.
func gpugAgg(keys ...values.Value) *logical.LogicalAggregate {
	gks := make([]logical.GroupKey, 0, len(keys))
	for _, k := range keys {
		gks = append(gks, logical.GroupKey{Value: k, Display: k.Name()})
	}
	return &logical.LogicalAggregate{Input: logical.NewScan("t", ""), GroupKeys: gks}
}

// gpugComputed is a NON-FieldValue grouping key — the node kind the computed
// walk owns. Two separately-built copies are semantically equal, which is what
// makes a single reference answer to both.
func gpugComputed(col string) values.Value {
	return &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  &values.FieldValue{Field: col, Typ: values.UnknownType},
		Right: values.LiteralValue(int64(1)),
	}
}

// TestGroupKeyPullUpGuard_ComputedWalkRefusesAMultiMatch drives the arm the SQL
// corpus cannot reach. Two semantically-equal computed keys, one reference: the
// walk must decline to bind and record the ambiguity rather than take slot 0.
func TestGroupKeyPullUpGuard_ComputedWalkRefusesAMultiMatch(t *testing.T) {
	t.Parallel()

	agg := gpugAgg(gpugComputed("C1"), gpugComputed("C1"))
	var amb groupKeyPullUpAmbiguity
	out := rebasePostAggregateComputedGroupKey(gpugComputed("C1"), agg, &amb)

	if !amb.hit {
		t.Fatalf("two semantically equal computed group keys did not record an "+
			"ambiguity: the computed pull-up walk bound a reference that answers "+
			"to BOTH keys, which is the first-match Java refuses "+
			"(Expressions.java:112). got out=%T", out)
	}
	err := amb.err()
	if err == nil {
		t.Fatal("ambiguity recorded but err() returned nil — the verdict cannot reach a caller")
	}
	if !strings.Contains(err.Error(), "42702") {
		t.Errorf("got %v, want SQLSTATE 42702 (AMBIGUOUS_COLUMN)", err)
	}
	// The VALUE, not only the relationship: a guard that fires with an empty or
	// wrong column name is indistinguishable from one that fires correctly if
	// only the SQLSTATE is asserted.
	if !strings.Contains(err.Error(), "C1") {
		t.Errorf("got %v, want the message to name the duplicated key C1", err)
	}
	// The node must be left ALONE on a decline. Returning a bound ordinal here
	// would mean the guard recorded the ambiguity and then answered anyway.
	if fv, bound := out.(*values.FieldValue); bound {
		t.Errorf("declined reference was still rewritten to %v — on an ambiguity "+
			"the walk must return the node untouched", fv)
	}
}

// TestGroupKeyPullUpGuard_ComputedWalkStillBindsAUniqueMatch is the control that
// keeps the guard from passing by refusing everything. One matching key, and the
// reference must bind to ITS slot — asserted as a VALUE (slot 1, not 0), because
// a walk that always answered slot 0 would satisfy a mere "it bound something".
func TestGroupKeyPullUpGuard_ComputedWalkStillBindsAUniqueMatch(t *testing.T) {
	t.Parallel()

	agg := gpugAgg(gpugComputed("OTHER"), gpugComputed("C1"))
	var amb groupKeyPullUpAmbiguity
	out := rebasePostAggregateComputedGroupKey(gpugComputed("C1"), agg, &amb)

	if amb.hit {
		t.Fatalf("a UNIQUE match was reported ambiguous (%v) — the guard is "+
			"counting keys it did not match", amb.err())
	}
	fv, bound := out.(*values.FieldValue)
	if !bound {
		t.Fatalf("a unique computed-key match did not bind: got %T", out)
	}
	if fv.Resolved == nil || fv.Resolved.Last().Ordinal != 1 {
		t.Errorf("bound to %v, want the aggregate output slot 1 — the key's index "+
			"in GroupKeys IS its slot (the row is [keys..., aggregates...]), which "+
			"is the loop index Java pulls up by "+
			"(CompensateRecordConstructorRule.java:88-92)", fv.Resolved)
	}
}

// TestGroupKeyPullUpGuard_RecordsOnlyTheFirstVerdict pins the collector's own
// contract: the verdict is one fact per statement however many references share
// it, matching Java's message naming a single sub-expression.
func TestGroupKeyPullUpGuard_RecordsOnlyTheFirstVerdict(t *testing.T) {
	t.Parallel()

	var amb groupKeyPullUpAmbiguity
	if err := amb.err(); err != nil {
		t.Fatalf("a collector with no ambiguity must yield no error, got %v", err)
	}
	amb.record("FIRST")
	amb.record("SECOND")
	err := amb.err()
	if err == nil {
		t.Fatal("record() did not arm the collector")
	}
	if !strings.Contains(err.Error(), "FIRST") || strings.Contains(err.Error(), "SECOND") {
		t.Errorf("got %v, want the FIRST recorded name only — a collector that "+
			"overwrites reports whichever reference the walk happened to visit last",
			err)
	}
}

// gpugQualified is a grouping key read through a named quantifier — the shape
// whose correlation is what keeps `o.k` and `i.k` two keys rather than one.
func gpugQualified(corr, field string) *values.FieldValue {
	return &values.FieldValue{
		Field: field,
		Typ:   values.UnknownType,
		Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(corr)),
	}
}

// TestGroupKeyPullUpGuard_ConstructionRefusesTwoEqualKeys drives the guard that
// now decides every duplicate shape — Java's LogicalOperator.java:454, which
// pulls the grouping expressions up against the group-by result value through
// the asserting Expressions.pullUp.
//
// It is corpus-reachable (the join arm in //pkg/relational/sqldriver drives it
// end-to-end on six spellings), so this arm exists for the NEGATIVE direction
// below and to keep the positive one honest if that arm is ever narrowed.
func TestGroupKeyPullUpGuard_ConstructionRefusesTwoEqualKeys(t *testing.T) {
	t.Parallel()

	err := groupByOutputConstructionPullUp(gpugAgg(
		gpugQualified("A", "K"),
		gpugQualified("A", "K"),
	))
	if err == nil {
		t.Fatal("two identical grouping keys were accepted at output construction; " +
			"Java raises AMBIGUOUS_COLUMN here (Expressions.java:112 via " +
			"LogicalOperator.java:454) before any SELECT-list or HAVING pull-up runs")
	}
	if !strings.Contains(err.Error(), "42702") || !strings.Contains(err.Error(), "K") {
		t.Errorf("got %v, want 42702 naming K", err)
	}
}

// TestGroupKeyPullUpGuard_ConstructionKeepsTwoQuantifiersApart is the arm that
// stops the guard from being a blanket refusal. `GROUP BY o.k, i.k` reads the
// same column name through two DIFFERENT quantifiers; those are two distinct
// grouping keys and must plan. Without this, a guard that refused everything
// would satisfy the positive arm above.
func TestGroupKeyPullUpGuard_ConstructionKeepsTwoQuantifiersApart(t *testing.T) {
	t.Parallel()

	if err := groupByOutputConstructionPullUp(gpugAgg(
		gpugQualified("O", "K"),
		gpugQualified("I", "K"),
	)); err != nil {
		t.Fatalf("two quantifiers over one column name were refused (%v) — the "+
			"correlation is what separates them, and collapsing it would refuse "+
			"every self-join GROUP BY", err)
	}

	// A single key can never be ambiguous with itself: the guard compares
	// DISTINCT indices, and an off-by-one that compared i to i would refuse
	// every grouped query in the corpus.
	if err := groupByOutputConstructionPullUp(gpugAgg(gpugQualified("A", "K"))); err != nil {
		t.Fatalf("a lone grouping key was refused (%v) — the guard is comparing a "+
			"key against itself", err)
	}
}

// TestGroupKeyPullUpGuard_FieldValueWalkRefusesAMultiMatch and its binder twin
// drive the two post-aggregate guards that the construction guard has made
// UNREACHABLE from SQL.
//
// MEASURED, and the population is stated because it is what makes these arms
// load-bearing rather than decorative: over the whole
// //pkg/relational/sqldriver target at 6158 subtests, the three post-aggregate
// sites are consulted 797 times (binder 414, computed 360, FieldValue walk 23)
// and NOT ONE consultation sees more than one match. The construction pull-up
// refuses duplicates first, exactly as Java's ordering predicts.
//
// They are kept rather than deleted because Java keeps its asserts at every
// pull-up site — Expressions.pullUp guards both construction and the SELECT
// list, Expression.pullUp guards HAVING — and because a normalization change is
// all that stands between "no duplicate reaches here" and "one does". Deleting
// them would re-arm silent first-match at three sites at once. They are unit-
// driven here so that "unreachable" never means "untested".
func TestGroupKeyPullUpGuard_FieldValueWalkRefusesAMultiMatch(t *testing.T) {
	t.Parallel()

	agg := gpugAgg(gpugQualified("A", "K"), gpugQualified("A", "K"))
	var amb groupKeyPullUpAmbiguity
	out := rebasePostAggregateGroupKeyValue(gpugQualified("A", "K"), agg, &amb)

	if !amb.hit {
		t.Fatalf("a reference answering to BOTH equal grouping keys was bound "+
			"instead of refused (got %T) — this walk is first-matching again", out)
	}
	if err := amb.err(); err == nil || !strings.Contains(err.Error(), "42702") {
		t.Errorf("got %v, want 42702", err)
	}
}

func TestGroupKeyPullUpGuard_ExactBoundaryBinderRefusesAMultiMatch(t *testing.T) {
	t.Parallel()

	agg := gpugAgg(gpugQualified("A", "K"), gpugQualified("A", "K"))
	_, err := bindPostAggregateValueToNativeOrdinals(gpugQualified("A", "K"), agg)
	if err == nil {
		t.Fatal("the exact-boundary binder bound a reference that answers to TWO " +
			"grouping keys; this is the site Java's SELECT-list assert guards " +
			"(Expressions.java:112)")
	}
	if !strings.Contains(err.Error(), "42702") {
		t.Errorf("got %v, want 42702", err)
	}
}

// TestGroupKeyPullUpGuard_BinderStillBindsAUniqueMatch is the binder's control,
// and it asserts the SLOT rather than merely that something bound: the key's
// index in GroupKeys IS its output ordinal, and a binder pinned to 0 would pass
// any weaker check.
func TestGroupKeyPullUpGuard_BinderStillBindsAUniqueMatch(t *testing.T) {
	t.Parallel()

	agg := gpugAgg(gpugQualified("A", "OTHER"), gpugQualified("A", "K"))
	out, err := bindPostAggregateValueToNativeOrdinals(gpugQualified("A", "K"), agg)
	if err != nil {
		t.Fatalf("a unique match was refused: %v", err)
	}
	fv, bound := out.(*values.FieldValue)
	if !bound || fv.Resolved == nil {
		t.Fatalf("a unique match did not bind to an ordinal: got %T", out)
	}
	if fv.Resolved.Last().Ordinal != 1 {
		t.Errorf("bound to ordinal %d, want 1 — the key's index in GroupKeys is "+
			"its slot in the [keys..., aggregates...] output row",
			fv.Resolved.Last().Ordinal)
	}
}

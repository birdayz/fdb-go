package embedded

// Exact post-aggregate binding must reject a reference that matches more than
// one grouping-key slot. Production refuses duplicate keys while constructing
// the output contract and repeats the ambiguity check at the draft-validation
// boundary; these tests pin both layers and their unique-match controls.

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

func gpugSourceType() *values.RecordType {
	return &values.RecordType{Fields: []values.Field{
		{Name: "C1", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "OTHER", Ordinal: 1, FieldType: values.NotNullLong},
		{Name: "K", Ordinal: 2, FieldType: values.NotNullLong},
	}}
}

func gpugQualified(t testing.TB, corr, field string) values.Value {
	t.Helper()
	typ := gpugSourceType()
	ordinal, ok := typ.FieldIndexUnique(field)
	if !ok {
		t.Fatalf("unknown fixture field %q", field)
	}
	qov, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(corr), typ)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue: %v", err)
	}
	value, err := values.ResolveFieldOrdinals(qov, []int{ordinal})
	if err != nil {
		t.Fatalf("resolve %s.%s: %v", corr, field, err)
	}
	return value
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
		gpugQualified(t, "A", "K"),
		gpugQualified(t, "A", "K"),
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
		gpugQualified(t, "O", "K"),
		gpugQualified(t, "I", "K"),
	)); err != nil {
		t.Fatalf("two quantifiers over one column name were refused (%v) — the "+
			"correlation is what separates them, and collapsing it would refuse "+
			"every self-join GROUP BY", err)
	}

	// A single key can never be ambiguous with itself: the guard compares
	// DISTINCT indices, and an off-by-one that compared i to i would refuse
	// every grouped query in the corpus.
	if err := groupByOutputConstructionPullUp(gpugAgg(gpugQualified(t, "A", "K"))); err != nil {
		t.Fatalf("a lone grouping key was refused (%v) — the guard is comparing a "+
			"key against itself", err)
	}
}

func TestGroupKeyPullUpGuard_ExactBoundaryBinderRefusesAMultiMatch(t *testing.T) {
	t.Parallel()

	agg := gpugAgg(gpugQualified(t, "A", "K"), gpugQualified(t, "A", "K"))
	err := validatePostAggregateValueDraft(gpugQualified(t, "A", "K"), agg)
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

	reference := gpugQualified(t, "A", "K")
	agg := gpugAgg(gpugQualified(t, "A", "OTHER"), gpugQualified(t, "A", "K"))
	err := validatePostAggregateValueDraft(reference, agg)
	if err != nil {
		t.Fatalf("a unique exact group-key draft was refused: %v", err)
	}
}

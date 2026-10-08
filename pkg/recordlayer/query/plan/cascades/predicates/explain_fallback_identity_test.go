package predicates

import (
	"regexp"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// heapAddress matches fmt's rendering of a pointer. A predicate's Explain()
// must never contain one: two identity mechanisms fold Explain() in their
// default arm, so an address there makes identity allocation-dependent.
var heapAddress = regexp.MustCompile(`0x[0-9a-f]{6,}`)

func fieldRefOperand(t *testing.T) values.Value {
	t.Helper()
	root, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("q"),
		values.NewRecordType("R", false, []values.Field{
			{Name: "A", FieldType: values.NullableLong, Ordinal: 0},
		}))
	if err != nil {
		t.Fatalf("building the QOV root: %v", err)
	}
	fv, err := values.ResolveFieldOrdinals(root, []int{0})
	if err != nil {
		t.Fatalf("resolving field ordinal: %v", err)
	}
	return fv
}

func sargableOverFieldRef(t *testing.T) *PredicateWithValueAndRanges {
	t.Helper()
	rc := NewRangeConstraints([]Comparison{
		{Type: ComparisonGreaterThan, Operand: fieldRefOperand(t)},
	}, nil)
	return NewPredicateWithValueAndRanges(values.LiteralValue(int64(1)), []*RangeConstraints{rc})
}

// Structural equality still uses Explain for range predicates; semantic
// equality and hashing use explicit range/value arms instead.
func TestExplainFallback_IsAllocationIndependent(t *testing.T) {
	t.Parallel()

	a, b := sargableOverFieldRef(t), sargableOverFieldRef(t)

	// The premise: these really are two separate objects over separate operands.
	// If the fixture ever shares one operand, every check below passes for a
	// reason that has nothing to do with the defect.
	if a == b {
		t.Fatal("the fixture returned the same pointer twice — allocation independence " +
			"cannot be tested against a single object")
	}
	if a.GetComparisons()[0].Operand == b.GetComparisons()[0].Operand {
		t.Fatal("both predicates share one operand object, so their renderings would match " +
			"even with an address in them; the fixture no longer builds the failing shape")
	}

	if got := a.Explain(); heapAddress.MatchString(got) {
		t.Errorf("Explain() contains a heap address: %q; structural identity must be allocation-independent", got)
	}
	if a.Explain() != b.Explain() {
		t.Errorf("two structurally identical predicates render differently:\n  %q\n  %q",
			a.Explain(), b.Explain())
	}
	if !StructurallyEqual(a, b) {
		t.Error("two structurally identical sargables are not StructurallyEqual — they will " +
			"occupy separate memo buckets and never dedup")
	}
	if StructuralHash(a) != StructuralHash(b) {
		t.Error("...and they hash apart under StructuralHash")
	}
	if SemanticHashCode(a) != SemanticHashCode(b) {
		t.Error("equal range predicates hash apart under SemanticHashCode")
	}

	// Control: the fallback must still DISCRIMINATE. A rendering that collapsed
	// everything would satisfy every check above.
	other := NewPredicateWithValueAndRanges(values.LiteralValue(int64(2)),
		[]*RangeConstraints{NewRangeConstraints([]Comparison{
			{Type: ComparisonGreaterThan, Operand: fieldRefOperand(t)},
		}, nil)})
	if StructurallyEqual(a, other) {
		t.Error("sargables over DIFFERENT values compared equal — the Explain fallback has " +
			"stopped discriminating and the assertions above are vacuous")
	}
	// Structural identity must distinguish different ranges.
	differentRange := NewPredicateWithValueAndRanges(values.LiteralValue(int64(1)),
		[]*RangeConstraints{NewRangeConstraints([]Comparison{
			{Type: ComparisonLessThan, Operand: fieldRefOperand(t)},
		}, nil)})
	if StructurallyEqual(a, differentRange) {
		t.Error("sargables differing only in their RANGE compared equal — they bound " +
			"different key ranges")
	}
	if StructuralHash(a) == StructuralHash(differentRange) {
		t.Error("...and they hash identically")
	}
}

// TestConstantPredicate_IdentityRestsOnTheTriBoolSingletons pins a negative
// result: writeStructuralHash folds ConstantPredicate.Value with %v, and TriBool
// is *bool, so that IS a heap address in the hash — and it is nonetheless
// correct, for a reason nothing else states.
//
// StructurallyEqual compares the same field by POINTER (`ap.Value == bp.Value`),
// so the two sides agree and the equal-implies-same-hash invariant holds however
// the pointer was obtained. Dedup then works only because every TriBool in
// production is one of the three package singletons — including the one
// non-literal construction site, rule_simplify.go's plan-time constant folding,
// whose value comes from Comparison.EvalAgainst, every return path of which
// yields TriTrue/TriFalse/TriUnknown.
//
// Hand ConstantPredicate a freshly allocated *bool and identity silently
// degrades: the predicate stops deduping against its twin, with no failure
// anywhere. This is what would catch that.
func TestConstantPredicate_IdentityRestsOnTheTriBoolSingletons(t *testing.T) {
	t.Parallel()

	for _, tri := range []struct {
		name string
		v    TriBool
	}{{"TRUE", TriTrue}, {"FALSE", TriFalse}, {"UNKNOWN", TriUnknown}} {
		a := NewConstantPredicate(tri.v)
		b := NewConstantPredicate(tri.v)
		if !StructurallyEqual(a, b) {
			t.Errorf("two independently built ConstantPredicate(%s) are not StructurallyEqual "+
				"— identity here is POINTER-based, so this means a fresh *bool reached the "+
				"constructor and constant predicates have stopped deduping", tri.name)
		}
		if StructuralHash(a) != StructuralHash(b) {
			t.Errorf("two independently built ConstantPredicate(%s) hash apart", tri.name)
		}
	}

	// The fact the above rests on, asserted directly so its failure names the
	// cause rather than the symptom.
	if TriTrue == nil || TriFalse == nil {
		t.Fatal("TriTrue/TriFalse are no longer non-nil singletons")
	}
	if TriTrue == TriFalse {
		t.Fatal("TriTrue and TriFalse are the same pointer — TRUE and FALSE constants would " +
			"compare equal")
	}
	fresh := true
	if StructurallyEqual(NewConstantPredicate(TriTrue), NewConstantPredicate(TriBool(&fresh))) {
		t.Error("a freshly allocated *bool holding true compared EQUAL to TriTrue. That would " +
			"make identity value-based, which is fine — but writeStructuralHash folds the " +
			"POINTER, so equality and hash have then diverged and the memo invariant is broken.")
	}
}

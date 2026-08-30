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

// TestExplainFallback_IsAllocationIndependent covers the predicate types that
// reach StructurallyEqual's and writeSemanticHash's DEFAULT arms, which decide
// identity by folding Explain() and describe it as "a stable structural
// discriminator".
//
// PredicateWithValueAndRanges rendered its comparison operands with
// fmt.Sprintf("%v", …). A Value is an interface over pointer structs and fmt
// prints a NESTED pointer field as a hex address, so two independently built but
// structurally identical sargables rendered
//
//	"1 IN {> &{A LONG NULL 0x3df9bb6a0450 0x3df9bb6a04b0 ...}}"
//
// with different addresses — and compared UNEQUAL and hashed APART under both
// mechanisms. Two identical sargables would not share a memo bucket, and the
// hash would differ across processes.
//
// The type is never originated in the Go port, so this was latent rather than
// shipped; it is pinned because the default arm is shared, and the next type to
// land in it inherits the same contract.
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
		t.Errorf("Explain() contains a heap address: %q. Both StructurallyEqual and "+
			"writeSemanticHash fold this string in their default arm, so identity is now "+
			"allocation-dependent — use values.ExplainValue for every Value rendered here, "+
			"never %%v.", got)
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
		t.Error("...and under SemanticHashCode, which folds Explain() in its own default arm")
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
	// And on the range side, which is the half the deleted HashCodeWithoutChildren
	// ignored entirely.
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

// TestQueryPredicates_DoNotDefineHashCodeWithoutChildren keeps a deleted
// mechanism deleted.
//
// Two predicate types carried a HashCodeWithoutChildren method. Nothing called
// them and no interface required them — but the NAME is the memo hash at the two
// layers above (expressions.RelationalExpression and plans.RecordQueryPlan both
// declare it), so a reader would reasonably wire one up. Both were wrong:
// PredicateWithValueAndRanges folded only its value and ignored its RANGES, and
// ExistentialValuePredicate folded a constant, so every existential predicate
// hashed identically.
//
// The predicates package has exactly two identity mechanisms — StructurallyEqual
// / StructuralHash, and SemanticEqualsUnderAliasMap / SemanticHashCode — and a
// third spelling that is dead and wrong is the configuration that produced the
// original Comparison bug: a fold fixed in one place and missed in another.
func TestQueryPredicates_DoNotDefineHashCodeWithoutChildren(t *testing.T) {
	t.Parallel()

	type memoHasher interface{ HashCodeWithoutChildren() uint64 }

	qov, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("q"),
		values.NewRecordType("R", false, []values.Field{
			{Name: "A", FieldType: values.NullableLong, Ordinal: 0},
		}))
	if err != nil {
		t.Fatalf("building the QOV: %v", err)
	}

	candidates := map[string]QueryPredicate{
		"PredicateWithValueAndRanges": sargableOverFieldRef(t),
		"ExistentialValuePredicate": MustNewExistentialValuePredicate(
			qov, Comparison{Type: ComparisonIsNotNull}),
		"ComparisonPredicate": NewComparisonPredicate(
			values.LiteralValue("c"), Comparison{Type: ComparisonEquals}),
		"ConstantPredicate": NewConstantPredicate(TriTrue),
	}
	if len(candidates) == 0 {
		t.Fatal("no candidates — this test would pass over nothing")
	}
	for name, p := range candidates {
		if _, ok := p.(memoHasher); ok {
			t.Errorf("%s defines HashCodeWithoutChildren. That name is the MEMO HASH at the "+
				"expression and plan layers, and this package's identity mechanisms are "+
				"StructuralHash and SemanticHashCode — a third spelling here will be wired "+
				"up by mistake. Fold the field set into one of the existing two instead.",
				name)
		}
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

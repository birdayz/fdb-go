package predicates

import (
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// comparisonFieldMutators changes exactly one identity field of a Comparison,
// keyed by the field's name so the table can be checked against the struct
// itself. A field with no mutator here is a field this test cannot discriminate
// on, which is the state the bug lived in.
var comparisonFieldMutators = map[string]func(*Comparison){
	"Type":               func(c *Comparison) { c.Type = ComparisonTextContainsAny },
	"Operand":            func(c *Comparison) { c.Operand = values.LiteralValue("goodbye") },
	"Escape":             func(c *Comparison) { c.Escape = '#' },
	"ParameterName":      func(c *Comparison) { c.ParameterName = "other" },
	"TextTokenizerName":  func(c *Comparison) { c.TextTokenizerName = "ngram" },
	"TextAnalyzerName":   func(c *Comparison) { c.TextAnalyzerName = "other" },
	"TextMaxDistance":    func(c *Comparison) { c.TextMaxDistance = 7 },
	"TextStrictPrefix":   func(c *Comparison) { c.TextStrictPrefix = true },
	"QueryVector":        func(c *Comparison) { c.QueryVector = values.LiteralValue("other") },
	"EfSearch":           func(c *Comparison) { v := 99; c.EfSearch = &v },
	"IsReturningVectors": func(c *Comparison) { v := true; c.IsReturningVectors = &v },
}

// baselineComparison is deliberately NON-ZERO in every field, so a mutator that
// flips a field to its zero value still changes something. A baseline of all
// zeros would make "set it to false" a no-op and the discrimination test would
// pass without testing anything.
func baselineComparison() Comparison {
	ef := 10
	ret := false
	return Comparison{
		Type:               ComparisonTextContainsAll,
		Operand:            values.LiteralValue("hello"),
		Escape:             '\\',
		ParameterName:      "p",
		TextTokenizerName:  "default",
		TextAnalyzerName:   "std",
		TextMaxDistance:    3,
		TextStrictPrefix:   false,
		QueryVector:        values.LiteralValue("vec"),
		EfSearch:           &ef,
		IsReturningVectors: &ret,
	}
}

// TestComparisonIdentityFoldsEveryField is the gate that would have caught the
// bug this file exists for, and the one that keeps it caught.
//
// StructurallyEqual folded Type, Escape and Operand and ignored the other
// eight. StructuralHash mirrored exactly those three. So the equal-implies-
// same-hash invariant HELD — the two sides moved together — while both were
// wrong, and no pairwise consistency test could have seen it. The only check
// that can is one against the STRUCT, which is what this is.
//
// The identical defect was found and fixed one layer up in
// plans.comparisonEqual; the fix never reached this layer. That is the argument
// for reflection rather than a hand-kept list: a list gets fixed in the place
// somebody is looking at.
func TestComparisonIdentityFoldsEveryField(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeOf(Comparison{})
	if typ.NumField() == 0 {
		t.Fatal("Comparison has no fields — reflection is not seeing the struct, and every " +
			"check below passes vacuously")
	}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if _, folded := comparisonIdentityFields[name]; !folded {
			t.Errorf("Comparison.%s is not in comparisonIdentityFields. Either fold it into "+
				"comparisonIdentityEqual AND writeComparisonIdentity and add it there with a "+
				"reason, or if it genuinely carries no identity, say so in that map — a field "+
				"nobody classified is how two TEXT_CONTAINS predicates differing only in "+
				"tokenizer came to compare equal.", name)
		}
		if _, driven := comparisonFieldMutators[name]; !driven {
			t.Errorf("Comparison.%s has no mutator in comparisonFieldMutators, so "+
				"TestComparisonIdentity_DiscriminatesEveryField never varies it and cannot "+
				"prove it is folded.", name)
		}
	}

	// The reverse direction. A stale entry naming a removed field would make the
	// counts agree while a real field went unclassified.
	fields := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		fields[typ.Field(i).Name] = true
	}
	for name := range comparisonIdentityFields {
		if !fields[name] {
			t.Errorf("comparisonIdentityFields names %q, which Comparison no longer has", name)
		}
	}
	for name := range comparisonFieldMutators {
		if !fields[name] {
			t.Errorf("comparisonFieldMutators names %q, which Comparison no longer has", name)
		}
	}
}

// TestComparisonIdentity_DiscriminatesEveryField proves the folding actually
// works, field by field, on BOTH sides. Checking equality alone would not be
// enough: the bug was that equality and hash agreed with each other while both
// ignored the field.
func TestComparisonIdentity_DiscriminatesEveryField(t *testing.T) {
	t.Parallel()

	mk := func(mut func(*Comparison)) *ComparisonPredicate {
		c := baselineComparison()
		if mut != nil {
			mut(&c)
		}
		return NewComparisonPredicate(values.LiteralValue("col"), c)
	}
	base := mk(nil)

	// Identical predicates must still collapse — the fix must not break dedup,
	// which is the whole reason structural identity exists.
	if !StructurallyEqual(base, mk(nil)) {
		t.Fatal("two identically-built comparisons must be StructurallyEqual; the folding " +
			"has become over-strict and memo dedup is now broken")
	}
	if StructuralHash(base) != StructuralHash(mk(nil)) {
		t.Fatal("two identically-built comparisons must hash identically")
	}

	checked := 0
	for name, mut := range comparisonFieldMutators {
		other := mk(mut)
		if StructurallyEqual(base, other) {
			t.Errorf("two comparisons differing only in %s are StructurallyEqual — they will "+
				"collapse into one memo Reference despite reading different data", name)
		}
		if StructuralHash(base) == StructuralHash(other) {
			t.Errorf("two comparisons differing only in %s hash identically — the hash is not "+
				"folding it even if equality is", name)
		}
		checked++
	}
	if checked != len(comparisonIdentityFields) {
		t.Errorf("discriminated %d fields but %d are declared identity-bearing — the two "+
			"tables have drifted apart", checked, len(comparisonIdentityFields))
	}
}

// TestComparisonIdentity_PointerFieldsCompareByValue pins the three-state
// handling of the two pointer fields. They are pointers to carry presence, so
// comparing the POINTERS would call two separately-built comparisons with the
// same setting unequal and defeat dedup entirely — and folding nil identically
// to a pointer-at-false would put the original bug back for those fields.
func TestComparisonIdentity_PointerFieldsCompareByValue(t *testing.T) {
	t.Parallel()

	withEf := func(v *int) Comparison { c := baselineComparison(); c.EfSearch = v; return c }
	withRet := func(v *bool) Comparison { c := baselineComparison(); c.IsReturningVectors = v; return c }

	a, b := 10, 10
	if !comparisonIdentityEqual(withEf(&a), withEf(&b)) {
		t.Error("two distinct *int both holding 10 must be equal — EfSearch is compared by " +
			"pointer, so no two separately-built comparisons will ever dedup")
	}
	if comparisonIdentityEqual(withEf(&a), withEf(nil)) {
		t.Error("EfSearch present-at-10 must differ from absent")
	}

	f1, f2 := false, false
	tru := true
	if !comparisonIdentityEqual(withRet(&f1), withRet(&f2)) {
		t.Error("two distinct *bool both holding false must be equal")
	}
	if comparisonIdentityEqual(withRet(&f1), withRet(nil)) {
		t.Error("IsReturningVectors absent must differ from present-and-false; folding them " +
			"together is the same class of collapse this file fixes")
	}
	if StructuralHash(NewComparisonPredicate(values.LiteralValue("c"), withRet(&f1))) ==
		StructuralHash(NewComparisonPredicate(values.LiteralValue("c"), withRet(nil))) {
		t.Error("absent and present-and-false must hash apart, not just compare apart")
	}
	if comparisonIdentityEqual(withRet(&f1), withRet(&tru)) {
		t.Error("IsReturningVectors false must differ from true")
	}
}

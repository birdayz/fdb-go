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
		_, folded := comparisonIdentityFields[name]
		_, excluded := comparisonIdentityExcludedFields[name]
		if folded && excluded {
			t.Errorf("Comparison.%s is listed as BOTH folded and excluded", name)
		}
		if !folded && !excluded {
			t.Errorf("Comparison.%s is classified nowhere. Either fold it into "+
				"comparisonIdentityEqual AND writeComparisonIdentity and add it to "+
				"comparisonIdentityFields with a reason, or — if it genuinely carries no "+
				"identity — record it in comparisonIdentityExcludedFields with the reason. "+
				"A field nobody classified is how two TEXT_CONTAINS predicates differing "+
				"only in tokenizer came to compare equal.", name)
		}
		// EVERY field needs a mutator, excluded ones included. Exempting them
		// looks harmless and is the hole: proving a field is NOT folded needs a
		// mutation just as much as proving one IS, and without it an entry in
		// comparisonIdentityExcludedFields is an unfalsifiable claim. The
		// plan-layer gate in plans/plan_comparison_identity_drift_test.go drives
		// its excluded fields for exactly this reason.
		if _, driven := comparisonFieldMutators[name]; !driven {
			t.Errorf("Comparison.%s has no mutator in comparisonFieldMutators. A FOLDED "+
				"field without one is never varied by "+
				"TestComparisonIdentity_DiscriminatesEveryField, so nothing proves it is "+
				"folded; an EXCLUDED one without a mutator makes its exclusion "+
				"unfalsifiable.", name)
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
	for name := range comparisonIdentityExcludedFields {
		if !fields[name] {
			t.Errorf("comparisonIdentityExcludedFields names %q, which Comparison no longer has",
				name)
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

// TestComparisonIdentity_ExcludedFieldsAreInvisibleHere is the counterpart to
// TestComparisonIdentity_DiscriminatesEveryField, and it is the half that rots
// silently.
//
// comparisonIdentityExcludedFields lets a field be classified as carrying no
// identity, with a reason. Nothing checked that claim: the classification gate
// accepted the entry, the discrimination test skipped the field, and the reason
// written beside it could become false with everything green. The plan-layer
// gate does this properly and this one did not, which is the same
// fixed-in-one-layer-only shape as the original defect.
//
// The map is EMPTY today, so the loop below runs zero times — and a test whose
// body never executes is the dominant false positive in this codebase. Hence the
// explicit guard at the end: it fails the moment the map becomes non-empty
// WITHOUT this test having been re-read, so the dormant gate cannot go live
// unexamined.
func TestComparisonIdentity_ExcludedFieldsAreInvisibleHere(t *testing.T) {
	t.Parallel()

	base := baselineComparison()
	checked := 0
	for name := range comparisonIdentityExcludedFields {
		mut, ok := comparisonFieldMutators[name]
		if !ok {
			t.Errorf("no mutator for excluded field %s, so its exclusion is unproven", name)
			continue
		}
		other := baselineComparison()
		mut(&other)
		a := NewComparisonPredicate(values.LiteralValue("col"), base)
		b := NewComparisonPredicate(values.LiteralValue("col"), other)
		if !StructurallyEqual(a, b) {
			t.Errorf("Comparison.%s is listed in comparisonIdentityExcludedFields but "+
				"comparisonIdentityEqual now discriminates on it. Move the entry to "+
				"comparisonIdentityFields — the reason recorded beside it no longer "+
				"describes this code.", name)
		}
		if StructuralHash(a) != StructuralHash(b) {
			t.Errorf("Comparison.%s is listed as excluded but writeComparisonIdentity folds "+
				"it — the two sides of this fold have drifted apart", name)
		}
		checked++
	}
	if checked != len(comparisonIdentityExcludedFields) {
		t.Fatalf("checked %d excluded fields, want %d", checked,
			len(comparisonIdentityExcludedFields))
	}
	if checked != 0 {
		t.Fatalf("comparisonIdentityExcludedFields now has %d entr(y/ies). That is allowed, "+
			"but this test was written against an EMPTY map and its loop had never executed "+
			"— read it before trusting the green it just produced, then update this guard to "+
			"the new count.", checked)
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

// TestStructurallyEqual_ExistentialArmFoldsTheWholeComparison covers an arm that
// was changed and then had no test: StructurallyEqual's ExistentialValuePredicate
// case folds the full comparison identity rather than only Comparison.Type.
//
// Reverting it to type-only leaves every package green, which is the whole
// reason this exists. The arm is DEFENSIVE, not load-bearing:
// ExistentialValuePredicate.Comparison is documented as always IS NOT NULL, and
// tracing the five production construction sites confirms it — one mints
// Comparison{Type: ComparisonIsNotNull} and the other four propagate an existing
// comparison through transforms that rewrite Operand and QueryVector while
// leaving Type alone. So no production input can distinguish the two spellings.
//
// A test can, because MustNewExistentialValuePredicate accepts any Comparison
// and asserts only the QuantifiedObjectValue precondition. That makes the
// invariant a convention rather than a guarantee, and this pins what the arm
// does if the convention ever lapses.
func TestStructurallyEqual_ExistentialArmFoldsTheWholeComparison(t *testing.T) {
	t.Parallel()

	qov := mustQOV(t, values.NamedCorrelationIdentifier("q"))
	mk := func(c Comparison) *ExistentialValuePredicate {
		return MustNewExistentialValuePredicate(qov, c)
	}

	canonical := mk(Comparison{Type: ComparisonIsNotNull})
	if !StructurallyEqual(canonical, mk(Comparison{Type: ComparisonIsNotNull})) {
		t.Fatal("two canonical existential predicates must be equal, or every check below " +
			"passes for the wrong reason")
	}

	// Same Type, differing in a field the type-only spelling could not see.
	tagged := mk(Comparison{Type: ComparisonIsNotNull, ParameterName: "p"})
	if StructurallyEqual(canonical, tagged) {
		t.Error("existential predicates differing in Comparison.ParameterName compared " +
			"EQUAL — the arm is folding only Comparison.Type again, and the two would " +
			"share one memo identity")
	}
	if StructuralHash(canonical) == StructuralHash(tagged) {
		t.Error("...and they hash identically, so the hash arm regressed with it")
	}

	// Control: a differing Type must still separate, so the assertions above are
	// about the widened fold rather than about equality having broken entirely.
	if StructurallyEqual(canonical, mk(Comparison{Type: ComparisonIsNull})) {
		t.Error("existential predicates differing in Comparison.Type must not be equal")
	}
}

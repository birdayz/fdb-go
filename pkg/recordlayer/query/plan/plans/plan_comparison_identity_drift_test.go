package plans

import (
	"bytes"
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// planComparisonFieldMutators changes exactly one field of a Comparison, keyed
// by the field's name so the table can be checked against the struct itself.
// Every field is driven — the excluded ones too, because proving a field is NOT
// folded needs a mutation just as much as proving one IS.
var planComparisonFieldMutators = map[string]func(*predicates.Comparison){
	"Type":               func(c *predicates.Comparison) { c.Type = predicates.ComparisonTextContainsAny },
	"Operand":            func(c *predicates.Comparison) { c.Operand = values.LiteralValue("goodbye") },
	"Escape":             func(c *predicates.Comparison) { c.Escape = '#' },
	"ParameterName":      func(c *predicates.Comparison) { c.ParameterName = "other" },
	"TextTokenizerName":  func(c *predicates.Comparison) { c.TextTokenizerName = "ngram" },
	"TextAnalyzerName":   func(c *predicates.Comparison) { c.TextAnalyzerName = "other" },
	"TextMaxDistance":    func(c *predicates.Comparison) { c.TextMaxDistance = 7 },
	"TextStrictPrefix":   func(c *predicates.Comparison) { c.TextStrictPrefix = true },
	"QueryVector":        func(c *predicates.Comparison) { c.QueryVector = values.LiteralValue("other") },
	"EfSearch":           func(c *predicates.Comparison) { v := 99; c.EfSearch = &v },
	"IsReturningVectors": func(c *predicates.Comparison) { v := true; c.IsReturningVectors = &v },
}

// planBaselineComparison is deliberately NON-ZERO in every field, so a mutator
// that flips a field to its zero value still changes something. A baseline of
// all zeros would make "set it to false" a no-op and every discrimination check
// below would pass without discriminating anything.
func planBaselineComparison() predicates.Comparison {
	ef := 10
	ret := false
	return predicates.Comparison{
		Type:               predicates.ComparisonTextContainsAll,
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

func planComparisonHash(c predicates.Comparison) string {
	var buf bytes.Buffer
	writeComparisonHash(&buf, &c)
	return buf.String()
}

// TestPlanComparisonIdentity_ClassifiesEveryField is the drift gate for the
// plan layer's fold of predicates.Comparison.
//
// There are TWO folds of Comparison in the tree and they cover different field
// sets on purpose: predicates.comparisonIdentityEqual folds all eleven, this one
// folds eight and excludes the three DistanceRank comparand fields because a
// DistanceRank comparison cannot reach a ComparisonRange. Only the predicates
// side had a reflection gate, so a twelfth field added tomorrow would redden
// there, get folded there, and leave this fold silently blind to it — which is
// precisely how the original eleven-field bug came to exist in one layer and not
// the other.
func TestPlanComparisonIdentity_ClassifiesEveryField(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeOf(predicates.Comparison{})
	if typ.NumField() == 0 {
		t.Fatal("predicates.Comparison has no fields — reflection is not seeing the struct, " +
			"and every check below passes vacuously")
	}

	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		_, folded := planComparisonIdentityFields[name]
		_, excluded := planComparisonIdentityExcludedFields[name]
		if folded && excluded {
			t.Errorf("Comparison.%s is listed as BOTH folded and excluded by the plan layer", name)
		}
		if !folded && !excluded {
			t.Errorf("Comparison.%s is classified nowhere by the PLAN layer. Either fold it "+
				"into comparisonEqual AND writeComparisonHash and add it to "+
				"planComparisonIdentityFields with a reason, or — if a plan can never see it "+
				"— record it in planComparisonIdentityExcludedFields with the argument for "+
				"why it is unreachable and where its identity is carried instead. Note the "+
				"predicates-layer gate does NOT cover this: the two folds are deliberately "+
				"different sets.", name)
		}
		if _, driven := planComparisonFieldMutators[name]; !driven {
			t.Errorf("Comparison.%s has no mutator in planComparisonFieldMutators, so neither "+
				"direction below can be proven for it", name)
		}
	}

	// The reverse direction. A stale entry naming a removed field would make the
	// counts agree while a real field went unclassified.
	fields := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		fields[typ.Field(i).Name] = true
	}
	for _, table := range []struct {
		name  string
		names []string
	}{
		{"planComparisonIdentityFields", mapKeys(planComparisonIdentityFields)},
		{"planComparisonIdentityExcludedFields", mapKeys(planComparisonIdentityExcludedFields)},
	} {
		for _, name := range table.names {
			if !fields[name] {
				t.Errorf("%s names %q, which predicates.Comparison no longer has",
					table.name, name)
			}
		}
	}
	for name := range planComparisonFieldMutators {
		if !fields[name] {
			t.Errorf("planComparisonFieldMutators names %q, which predicates.Comparison no "+
				"longer has", name)
		}
	}
}

func mapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestPlanComparisonIdentity_FoldedFieldsDiscriminate proves the eight folded
// fields are actually folded, on BOTH sides — equality and hash. Checking
// equality alone would not do: the bug this whole area exists for was equality
// and hash agreeing with each other while both ignored the field, so the
// equal-implies-same-hash invariant held and no pairwise test could see it.
func TestPlanComparisonIdentity_FoldedFieldsDiscriminate(t *testing.T) {
	t.Parallel()

	base := planBaselineComparison()

	// Two identically-built comparisons must still collapse, or the checks below
	// are about equality being broken outright rather than about the fold.
	twin := planBaselineComparison()
	if !comparisonEqual(&base, &twin) {
		t.Fatal("two identically-built comparisons must be comparisonEqual; the fold has " +
			"become over-strict and plan dedup is now broken")
	}
	if planComparisonHash(base) != planComparisonHash(twin) {
		t.Fatal("two identically-built comparisons must hash identically")
	}

	checked := 0
	for name := range planComparisonIdentityFields {
		mut, ok := planComparisonFieldMutators[name]
		if !ok {
			t.Errorf("no mutator for folded field %s", name)
			continue
		}
		other := planBaselineComparison()
		mut(&other)
		if comparisonEqual(&base, &other) {
			t.Errorf("two comparisons differing only in %s are comparisonEqual — the scans "+
				"carrying them read different data and will collapse into one plan identity",
				name)
		}
		if planComparisonHash(base) == planComparisonHash(other) {
			t.Errorf("two comparisons differing only in %s hash identically — the hash is not "+
				"folding it even if equality is", name)
		}
		checked++
	}
	if checked != len(planComparisonIdentityFields) {
		t.Fatalf("discriminated %d fields but %d are declared folded", checked,
			len(planComparisonIdentityFields))
	}
	if checked == 0 {
		t.Fatal("no folded fields were discriminated — planComparisonIdentityFields is empty " +
			"and this test proved nothing")
	}
}

// TestPlanComparisonIdentity_ExcludedFieldsAreInvisibleHere pins the OTHER half
// of the classification, which is the half that rots silently.
//
// A field in planComparisonIdentityExcludedFields is not folded because a plan
// can never see it. If someone later folds one anyway, the exclusion entry — and
// the argument written next to it about where that field's identity is actually
// carried — becomes false while everything stays green. This is the check that
// reddens instead, and its message says to MOVE the entry rather than to revert
// the fold: folding them is not wrong, it is just no longer described by the
// table.
//
// It is deliberately not a claim that ignoring them is SAFE. That claim rests on
// isScanRangeEqualityType classifying DistanceRank as an inequality so the scan
// binder rejects it, pinned by
// TestBindScanComparisonsToRangeSet_RejectsMalformedTailBeforeProjection, and on
// TestRecordQueryVectorIndexPlan_QueryVectorIdentity for where the identity does
// live.
func TestPlanComparisonIdentity_ExcludedFieldsAreInvisibleHere(t *testing.T) {
	t.Parallel()

	base := planBaselineComparison()
	checked := 0
	for name := range planComparisonIdentityExcludedFields {
		mut, ok := planComparisonFieldMutators[name]
		if !ok {
			t.Errorf("no mutator for excluded field %s, so its exclusion is unproven", name)
			continue
		}
		other := planBaselineComparison()
		mut(&other)
		if !comparisonEqual(&base, &other) {
			t.Errorf("Comparison.%s is listed in planComparisonIdentityExcludedFields but "+
				"comparisonEqual now discriminates on it. Move the entry to "+
				"planComparisonIdentityFields — the exclusion note claiming its identity is "+
				"carried elsewhere no longer describes this code.", name)
		}
		if planComparisonHash(base) != planComparisonHash(other) {
			t.Errorf("Comparison.%s is listed as excluded but writeComparisonHash folds it — "+
				"the two sides of the plan fold have drifted apart", name)
		}
		checked++
	}
	if checked != len(planComparisonIdentityExcludedFields) {
		t.Fatalf("checked %d excluded fields, want %d", checked,
			len(planComparisonIdentityExcludedFields))
	}
	// The vacuity guard, and it points at GROWTH rather than collapse: the
	// excluded set is expected to be exactly the three DistanceRank comparand
	// fields. An empty set would make this test pass over nothing; a fourth entry
	// means a field was declared unreachable without this test's argument being
	// re-checked for it.
	if checked != 3 {
		t.Fatalf("the plan layer excludes %d Comparison fields, expected the 3 DistanceRank "+
			"comparand fields. A new exclusion needs its own unreachability argument — the "+
			"one written here covers DistanceRank only.", checked)
	}
}

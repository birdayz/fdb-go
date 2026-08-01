package factorycorpus_test

import (
	"testing"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
)

// TestFamilyOfMapsTheMeasuredShapes pins the family mapping on the exact
// feature-vector shapes the generator emits, one per structural case: a bare
// leaf predicate, a connective with duplicate child classes (deduplicated and
// sorted), a negated connective, nested arguments split at the TOP level only,
// and the subquery-class axis.
func TestFamilyOfMapsTheMeasuredShapes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ fv, family, file string }{
		{
			"shape=single;idx=A;proj=n4;where=cmp.eq;order=none",
			"single|cmp|none",
			"single__cmp__none.yamsql",
		},
		{
			"shape=join2.left-outer;idx=A+B,B,D;proj=n16;where=and(in.in,cmp.eq);order=desc+asc",
			"join2.left-outer|and(cmp+in)|none",
			"join2_left-outer__and_cmp-in__none.yamsql",
		},
		{
			// Duplicate child classes collapse: and(cmp,cmp,colcol) is the same
			// family as and(colcol,cmp).
			"shape=join3.comma;where=and(cmp.lt,cmp.ge,colcol.eq);order=asc",
			"join3.comma|and(cmp+colcol)|none",
			"join3_comma__and_cmp-colcol__none.yamsql",
		},
		{
			// The negation is part of the top class: !and reaches a different
			// planner path than and, and its file name must not collide with
			// the un-negated family's (a folded `_` would be trimmed away).
			"shape=single;where=!and(cmp.eq,in.in)",
			"single|!and(cmp+in)|none",
			"single__not_and_cmp-in__none.yamsql",
		},
		{
			// Nested connectives split at the top level only: the or's children
			// are `and` and `cmp`, not the and's own leaves.
			"shape=single;where=or(and(cmp.eq,in.in),cmp.gt)",
			"single|or(and+cmp)|none",
			"single__or_and-cmp__none.yamsql",
		},
		{
			"shape=single;where=cmp.eq;exists=not.corr",
			"single|cmp|exists",
			"single__cmp__exists.yamsql",
		},
		{
			"shape=single;where=and(cmp.eq,cmp.lt);exists=corr;scalarsub=min",
			"single|and(cmp)|exists+scalarsub",
			"single__and_cmp__exists-scalarsub.yamsql",
		},
	} {
		if got := factorycorpus.FamilyOf(tc.fv); got != tc.family {
			t.Errorf("FamilyOf(%q) = %q, want %q", tc.fv, got, tc.family)
		}
		if got := factorycorpus.FamilyFileName(tc.family); got != tc.file {
			t.Errorf("FamilyFileName(%q) = %q, want %q", tc.family, got, tc.file)
		}
	}
}

// TestFamilyFileNamesAreInjectiveOverTheCorpus pins that no two distinct
// family keys in the committed corpus collide on one file name — the property
// that keeps two families from silently merging into a single file.
func TestFamilyFileNamesAreInjectiveOverTheCorpus(t *testing.T) {
	t.Parallel()
	scenarios, err := factorycorpus.LoadDir(factorycorpus.TestdataDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	byFile := map[string]string{}
	for _, s := range scenarios {
		family := factorycorpus.FamilyOf(s.Header.FeatureVector)
		file := factorycorpus.FamilyFileName(family)
		if prev, ok := byFile[file]; ok && prev != family {
			t.Errorf("families %q and %q both map to %s", prev, family, file)
		}
		byFile[file] = family
	}
}

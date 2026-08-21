package query

import (
	"testing"

	"fdb.dev/pkg/relational/core/query/logical"
)

// A DERIVED TABLE'S OUTPUT NAME COMES FROM THE PARSE-TREE SEGMENTS WHEN IT HAS
// THEM, and from the split only when it does not. `ProjectionRefs`' own doc
// states the rule — "qualification is FullId SEGMENT COUNT, never a scan of the
// rendered name" — and this site was recovering the qualifier by slicing at the
// last dot, so a column DECLARED `"a.b"` was published as `b`:
//
//	CREATE TABLE dottarr (id BIGINT, "a.b" BIGINT ARRAY, PRIMARY KEY (id));
//	SELECT x FROM (SELECT "a.b" FROM dottarr) d, d."a.b" AS x
//	  -> 42703: column "a.b" does not exist on source "D"
//
// WHY THIS IS A UNIT PIN AND NOT THAT QUERY. The shape does NOT plan even with
// this fixed — it declines one site later, in classifyDerivedUnnestArray, which
// still splits DELIBERATELY (see the comment there: migrating it too makes a
// VALID aliased form fail with a false 42703 instead of an honest decline, and
// the honest decline is the better of two non-working answers). RFC-238 step 6
// migrates that site together with the semantic registration that makes the
// query actually work, and lands the e2e arm this stands in for.
//
// What this fix DOES change, observably: the query above used to fail
// `42703: column "a.b" does not exist on source "D"`, which is false — the
// derived table does output `a.b`. It now declines `0AF00 … not yet supported`,
// which is true.
//
// So the pin is here, where the corrected authority is directly observable. It
// reddens if the site goes back to splitting, whatever the sites downstream of
// it do.
func TestProjectionOutputNamesTakesSegmentsOverTheSplit(t *testing.T) {
	t.Parallel()

	// project builds a body whose single projected item renders as `rendered`
	// and carries the segment triple `ref` — the pairing ColumnRefFor demands,
	// since a triple describing a different string than the rendering is worse
	// than no triple at all.
	project := func(rendered string, ref logical.ColumnRef) logical.LogicalOperator {
		p := logical.NewProject(logical.NewScan("DOTTARR", "DOTTARR"), []string{rendered}, nil)
		p.ProjectionRefs = []logical.ColumnRef{ref}
		return p
	}

	for _, tc := range []struct {
		name     string
		rendered string
		ref      logical.ColumnRef
		want     string
		why      string
	}{
		{
			name:     "one delimited segment containing a dot keeps it whole",
			rendered: `a.b`,
			ref:      logical.ColumnRef{Present: true, Bare: `a.b`, Qualified: false},
			want:     `a.b`,
			why: "the defect. One segment, so there is no qualifier to remove — but the " +
				"rendering is indistinguishable from a qualified reference",
		},
		{
			name:     "a genuine qualifier is still removed",
			rendered: `DOTTARR.ARR`,
			ref:      logical.ColumnRef{Present: true, Bare: `ARR`, Qualifier: `DOTTARR`, Qualified: true},
			want:     `ARR`,
			why:      "two segments, so the leading one is a qualifier and comes off",
		},
		{
			name:     "no triple falls back to the split",
			rendered: `DOTTARR.ARR`,
			ref:      logical.ColumnRef{},
			want:     `ARR`,
			why: "an absent triple reads as UNKNOWN, and the only safe reading of unknown " +
				"is whatever the rendered name supported before — never 'not qualified'",
		},
		{
			name:     "no triple and no dot is returned untouched",
			rendered: `ARR`,
			ref:      logical.ColumnRef{},
			want:     `ARR`,
			why:      "the fallback has nothing to split",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := projectionOutputNames(project(tc.rendered, tc.ref))
			if len(got) != 1 {
				t.Fatalf("projectionOutputNames returned %d names, want 1: %v", len(got), got)
			}
			if got[0] != tc.want {
				t.Errorf("projectionOutputNames(%q) = %q, want %q\n  %s",
					tc.rendered, got[0], tc.want, tc.why)
			}
		})
	}
}

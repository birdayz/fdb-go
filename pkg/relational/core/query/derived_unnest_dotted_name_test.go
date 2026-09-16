package query

import (
	"testing"

	"fdb.dev/pkg/relational/core/query/logical"
)

// TestExactOutputLabelsTakeSegmentsOverTheSplit pins quoted dotted identifiers
// at the live logical output-label authority. Lateral binding consumes the
// resulting semantic column, never a descriptor-backtracked body projection.
func TestExactOutputLabelsTakeSegmentsOverTheSplit(t *testing.T) {
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
			got, err := ExactLogicalOutputLabels(project(tc.rendered, tc.ref), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("ExactLogicalOutputLabels returned %d names, want 1: %v", len(got), got)
			}
			if got[0] != tc.want {
				t.Errorf("ExactLogicalOutputLabels(%q) = %q, want %q\n  %s",
					tc.rendered, got[0], tc.want, tc.why)
			}
		})
	}
}

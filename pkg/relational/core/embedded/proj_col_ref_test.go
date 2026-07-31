package embedded

// projColRef states a projected column's parse-tree SEGMENTS against the name
// the builder actually EMITTED. The two are not always the same string: the
// derived-table shell strips a `X.` qualifier prefix off the rendering, and the
// segment triple behind it still says "qualified by X".
//
// A triple that describes a different string than the one downstream carries is
// worse than no triple, because it is trusted. The consumer uses it to decide
// whether to resolve through a LEG WINDOW; told a bare `ID` is qualified by X,
// it looks for leg X's ID instead of the flat column the shell just produced.

import (
	"testing"

	"fdb.dev/pkg/relational/core/query/logical"
)

func TestProjColRef_ReconcilesAgainstTheEmittedName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		col      projCol
		rendered string
		want     logical.ColumnRef
	}{
		{
			// The derived-table shell's prefix strip. The segments still name a
			// qualifier; the emitted name no longer carries one.
			name:     "a stripped qualifier prefix leaves ONE segment",
			col:      projCol{name: "X.ID", bare: "ID", qualifier: "X", qualified: true},
			rendered: "ID",
			want:     logical.ColumnRef{Present: true, Bare: "ID"},
		},
		{
			name:     "an unstripped qualified reference keeps its qualifier",
			col:      projCol{name: "X.ID", bare: "ID", qualifier: "X", qualified: true},
			rendered: "X.ID",
			want:     logical.ColumnRef{Present: true, Bare: "ID", Qualifier: "X", Qualified: true},
		},
		{
			// The quoted whole name: one segment whose spelling contains a dot.
			name:     "a quoted one-segment name is captured as one segment",
			col:      projCol{name: "A.B", bare: "A.B"},
			rendered: "A.B",
			want:     logical.ColumnRef{Present: true, Bare: "A.B"},
		},
		{
			// A projCol built without segments (the sentinel and reclassified
			// entries) must claim nothing — "absent" and "unqualified" are the
			// same zero value downstream and mean opposite things.
			name:     "no captured bare segment claims nothing",
			col:      projCol{name: "X.ID"},
			rendered: "X.ID",
			want:     logical.ColumnRef{},
		},
		{
			// The segments spell neither the emitted name nor its bare tail —
			// a rebase rewrote the rendering. Claiming the stale triple would
			// authorize a qualified reading of a name it does not describe.
			name:     "segments that spell a DIFFERENT name claim nothing",
			col:      projCol{name: "X.ID", bare: "ID", qualifier: "X", qualified: true},
			rendered: "SUM(X.ID)",
			want:     logical.ColumnRef{},
		},
	} {
		if got := projColRef(tc.col, tc.rendered); got != tc.want {
			t.Errorf("%s: projColRef(%+v, %q) = %+v, want %+v",
				tc.name, tc.col, tc.rendered, got, tc.want)
		}
	}
}

package query

import (
	"testing"

	"fdb.dev/pkg/relational/core/query/logical"
)

func TestProjectionRefAtKeepsCapturedSegmentCount(t *testing.T) {
	t.Parallel()
	p := &logical.LogicalProject{ProjectionRefs: []logical.ColumnRef{
		{Present: true, Bare: "L.NAME"},
		{Present: true, Bare: "NAME", Qualifier: "L", Qualified: true},
	}}
	if got := projectionRefAt(p, 0); !got.Present || got.Qualified || got.Bare != "L.NAME" {
		t.Fatalf("quoted one-segment ref = %+v", got)
	}
	if got := projectionRefAt(p, 1); !got.Present || !got.Qualified || got.Qualifier != "L" || got.Bare != "NAME" {
		t.Fatalf("qualified two-segment ref = %+v", got)
	}
	if got := projectionRefAt(p, 2); got.Present {
		t.Fatalf("out-of-range ref = %+v, want uncaptured", got)
	}
}

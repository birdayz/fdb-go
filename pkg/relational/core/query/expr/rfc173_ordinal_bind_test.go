package expr_test

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
)

// TestResolveIdentifier_BakesLogicalOrdinal pins RFC-173 item C's
// construction-time ordinal bind: a single-source column reference leaves the
// resolver ALREADY bound to its LOGICAL ordinal — the column's position in
// the source's declared column order (Java's FieldValue.ofFieldName resolving
// the FieldPath against the child's result type at construction,
// FieldValue.java:273-299). Runtime then reads the slot positionally; it
// never re-derives the position from the name.
func TestResolveIdentifier_BakesLogicalOrdinal(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t) // USERS: [ID, NAME, ACTIVE, ADMIN]
	r := expr.New(a, s)

	for _, tc := range []struct {
		col  string
		want int
	}{
		{"id", 0},
		{"name", 1},
		{"active", 2},
		{"admin", 3},
	} {
		v, err := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted(tc.col))
		if err != nil {
			t.Fatalf("resolve %s: %v", tc.col, err)
		}
		fv, ok := v.(*values.FieldValue)
		if !ok {
			t.Fatalf("resolve %s: got %T, want *FieldValue", tc.col, v)
		}
		if fv.Resolved == nil {
			t.Fatalf("resolve %s: lazy FieldValue — item C requires the plan-time ordinal bind", tc.col)
		}
		if got := fv.Resolved.Root().Ordinal; got != tc.want {
			t.Fatalf("resolve %s: ordinal %d, want %d (declared order)", tc.col, got, tc.want)
		}
		if fv.Resolved.FrontierPinned {
			t.Fatalf("resolve %s: must be UNPINNED (no frontier contract on the common path)", tc.col)
		}
	}

	// The baked reference reads its slot positionally even when the row's
	// name map would disagree — position, not spelling, is authoritative.
	v, err := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("name"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.(*values.FieldValue).Evaluate(values.OrdinalRow(ordinalPredRow{
		cols: []string{"ID", "NAME", "ACTIVE", "ADMIN"},
		m:    map[string]any{"ID": int64(1), "NAME": "alice"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got != "alice" {
		t.Fatalf("baked NAME read = %v, want alice", got)
	}
}

package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestMergeShapeIgnoresRenderedCompositeNames pins the distinction between a
// name that IS a dotted qualifier (`alias.column` — the merge-row signal) and a
// name that merely CONTAINS a dot because it is a rendered composite TYPE.
//
// A one-field record over a qualified column renders its column name as
// `{_0: C.ID#0}`. Classifying that as merge-shaped sent an ordinary one-column
// leg into adaptLegPositional's zero-match tripwire — "a gated join must not
// consume a merge-shaped leg" — instead of the slot-for-slot arm that is
// correct for a row whose width already equals the leg type's. The dot is
// incidental to the rendering; the delimiter is what tells the two apart.
func TestMergeShapeIgnoresRenderedCompositeNames(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		columnName string
		want       bool
	}{
		// The real merge signal: a qualifier the merge row produced.
		{"bare dotted qualifier", "C.ID", true},
		{"qualifier with underscores", "ORDERS.CUSTOMER_ID", true},

		// Rendered composites. Every one of these contains a dot ONLY because
		// the type it renders mentions a qualified column.
		{"rendered one-field record over a qualified column", "{_0: C.ID#0}", false},
		{"rendered multi-field record", "{_0: C.ID#0, _1: I.QTY#2}", false},
		{"rendered array of records", "[{_0: C.ID#0}]", false},

		// Ordinary undotted names stay unremarkable.
		{"plain column", "QTY", false},
		{"ordinal name", "_0", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isDottedQualifiedName(tc.columnName); got != tc.want {
				t.Fatalf("isDottedQualifiedName(%q) = %v, want %v", tc.columnName, got, tc.want)
			}
		})
	}
}

// TestRowIsMergeShapedOverCompositeColumn is the same fact one level up, at the
// caller that actually gates the leg adapter: a single-column row whose column
// is a rendered record must NOT read as a merge row.
func TestRowIsMergeShapedOverCompositeColumn(t *testing.T) {
	t.Parallel()

	composite := NewPositionalRow(positionalTypeFromNames([]string{"{_0: C.ID#0}"}))
	composite.Set(0, int64(1))
	if rowIsMergeShaped(composite) {
		t.Fatal("a one-column row over a rendered record type read as merge-shaped; " +
			"the leg adapter will refuse it instead of taking the slot-for-slot arm")
	}

	// The guard must still fire on a genuine dotted-qualified merge row —
	// narrowing it must not disarm it.
	qualified := NewPositionalRow(positionalTypeFromNames([]string{"C.ID", "I.QTY"}))
	if !rowIsMergeShaped(qualified) {
		t.Fatal("a dotted-qualified row stopped reading as merge-shaped; the tripwire is disarmed")
	}

	// And on a row carrying explicit leg boundaries, which is the other arm.
	// The leg table is stated in the type this test BUILDS, rather than written
	// into a graph a constructor handed back. positionalTypeFromNames does
	// allocate, but that is a fact about another function; a Type graph is shared
	// under RFC-234 and the writer has to own what it writes.
	leggedType := &values.RecordType{
		Fields: []values.Field{
			{Name: "A", FieldType: values.UnknownType, Ordinal: 0},
			{Name: "B", FieldType: values.UnknownType, Ordinal: 1},
		},
		Legs: []values.RecordTypeLeg{{Start: 0, Width: 2}},
	}
	legged := NewPositionalRow(leggedType)
	if !rowIsMergeShaped(legged) {
		t.Fatal("a row with leg boundaries stopped reading as merge-shaped")
	}
}

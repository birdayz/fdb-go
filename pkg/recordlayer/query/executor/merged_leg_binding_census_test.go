package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// legLayout builds a merged RecordType with the given legs, each contributing its
// own columns in order — the shape bindMergedOuterLegs decomposes.
func legLayout(t *testing.T, legs ...struct {
	alias string
	cols  []string
},
) *values.RecordType {
	t.Helper()
	var fields []values.Field
	var table []values.RecordTypeLeg
	for _, leg := range legs {
		start := len(fields)
		for _, c := range leg.cols {
			fields = append(fields, values.Field{
				Name:      c,
				FieldType: &values.PrimitiveType{TypeCode: values.TypeCodeLong, Nullable: true},
				Ordinal:   len(fields),
			})
		}
		table = append(table, values.RecordTypeLeg{
			Alias: values.NamedCorrelationIdentifier(leg.alias),
			Name:  leg.alias,
			Start: start,
			Width: len(leg.cols),
		})
	}
	// Built as a literal, not through NewRecordType: a merged concat routinely
	// carries the same column name once per leg (every table here has an `ID`),
	// and NewRecordType rejects duplicate names. The shape renderer reads Fields
	// and Legs positionally, which is what a merged row hands it.
	return &values.RecordType{Nullable: true, Fields: fields, Legs: table}
}

type legSpec = struct {
	alias string
	cols  []string
}

// TestMergedRowShape_SeparatesRowsTheAliasCannot is the unit-level counterpart of
// the exclusion the sqldriver gate grants: a read is attributed to (alias, merged
// row shape), and this pins that the shape half actually separates rows a bare
// alias folds together.
//
// The alias `ST` is a plain table name. If the shape does not distinguish the
// merged row a read came out of, then a proof about `ST` read out of one join
// silently excuses `ST` read out of any other — which is the alias-collision
// argument the census already accepted for classifying reads, applied to excusing
// them.
func TestMergedRowShape_SeparatesRowsTheAliasCannot(t *testing.T) {
	t.Parallel()

	// The PROVEN shape: the two-leg `FROM ST, OT` merge the redundancy pin runs.
	proven := legLayout(t,
		legSpec{"ST", []string{"ID", "C", "ARR"}},
		legSpec{"OT", []string{"ID", "K"}})

	// Every way a DIFFERENT merged row can still offer an `ST` leg.
	others := map[string]*values.RecordType{
		"a third leg joins the merge": legLayout(t,
			legSpec{"ST", []string{"ID", "C", "ARR"}},
			legSpec{"OT", []string{"ID", "K"}},
			legSpec{"MA", []string{"ID", "C", "ARR"}}),
		"ST sits at a different offset": legLayout(t,
			legSpec{"OT", []string{"ID", "K"}},
			legSpec{"ST", []string{"ID", "C", "ARR"}}),
		"the sibling leg is a different table of the same width": legLayout(t,
			legSpec{"ST", []string{"ID", "C", "ARR"}},
			legSpec{"OT", []string{"ID", "J"}}),
		"ST itself carries different columns": legLayout(t,
			legSpec{"ST", []string{"ID", "C", "BRR"}},
			legSpec{"OT", []string{"ID", "K"}}),
	}

	want := mergedRowShape(proven)
	for why, other := range others {
		if got := mergedRowShape(other); got == want {
			t.Fatalf("%s: this merged row renders as the SAME shape as the proven one (%q).\n"+
				"  A read of ST out of it would carry the proven read's identity and be\n"+
				"  excused by the proven read's proof, which never ran against this row.\n"+
				"  That is the bare-alias key the exclusion moved off, reintroduced\n"+
				"  underneath it.", why, got)
		}
	}

	// The other direction: the shape must be STABLE for the row it names, or the
	// pin registers an identity the corpus's own reads never produce and the gate
	// goes red on a shape that was proven.
	if again := mergedRowShape(legLayout(t,
		legSpec{"ST", []string{"ID", "C", "ARR"}},
		legSpec{"OT", []string{"ID", "K"}})); again != want {
		t.Fatalf("the same merged layout rendered two different shapes: %q then %q.\n"+
			"  The redundancy pin registers the shape it measured and the gate looks\n"+
			"  the corpus's reads up under theirs; an unstable rendering makes the\n"+
			"  registration miss and turns a proven shape into a red gate.", want, again)
	}
}

// TestMergedRowShape_OutOfBoundsLegIsItsOwnShape pins that a leg whose span
// leaves the merged row does not render as its in-bounds prefix.
//
// Silently truncating it would equate a defective layout with a valid narrower
// one, and the shape is what an exclusion is keyed on — so the equation would
// hand a proof about the valid row the power to excuse reads out of the broken
// one.
func TestMergedRowShape_OutOfBoundsLegIsItsOwnShape(t *testing.T) {
	t.Parallel()

	valid := legLayout(t, legSpec{"ST", []string{"ID", "C"}}, legSpec{"OT", []string{"K"}})
	broken := legLayout(t, legSpec{"ST", []string{"ID", "C"}}, legSpec{"OT", []string{"K"}})
	broken.Legs[1].Width = 4 // runs off the end of a 3-field row

	if mergedRowShape(broken) == mergedRowShape(valid) {
		t.Fatalf("an out-of-bounds leg rendered as the in-bounds row's shape (%q)",
			mergedRowShape(valid))
	}
	if mergedRowShape(nil) == mergedRowShape(valid) {
		t.Fatalf("a nil merged type rendered as a real row's shape (%q)", mergedRowShape(valid))
	}
}

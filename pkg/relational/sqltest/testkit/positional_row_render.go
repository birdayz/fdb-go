package testkit

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/executor"
)

// Row renderers for the ordinal-row tests, shared so the ORDER dimension is
// covered once rather than per file.
//
// WHY THIS FILE EXISTS. A row's slots are addressed POSITIONALLY — that is the
// whole point of the ordinal model — but the tests that assert on multi-column
// rows were rendering them through the row's own name→value map. A map-keyed
// rendering is blind in exactly the dimension these tests exist to police:
// permute (Fields, Slots) TOGETHER and every name still maps to its own value, so
// the rendering is byte-identical while every slot moved. The failures leg windows
// exist to prevent are precisely mis-bound windows, i.e. a permutation.
//
// The blindness is systemic rather than local — the same map-keyed loop was
// copied across the suite — so the fix is one shared renderer and
// order-sensitive expectations everywhere, not a second assertion bolted onto
// one site. MEASURED population as of the last sweep: 21 files render rows
// through positionalNamedPipeSprint (24 call sites), 6 through
// positionalPipeSprint, 1 through positionalSprint; no test file renders a
// multi-slot row through executor.RowValue any more.
//
// NO renderer here calls executor.RowValue. The degenerate rows return a
// sentinel directly instead, which is both simpler and strictly louder:
//
//   - Positional == nil: there is no row. "<nil>".
//   - Positional != nil, Type == nil: there are SLOTS and no layout to name them
//     with. Routing that through RowValue rendered "map[]" — MEASURED, not
//     assumed: positionalToMap returns a nil map for a Type-less row, and
//     fmt prints a nil map as "map[]". A two-slot row rendering as "map[]" is
//     the defect this whole file exists to remove, wearing a plausible disguise
//     ("the row was empty"). It now renders a marker naming the slot count.
//   - len(Fields) != len(Slots): the loop used to `break` at the shorter of the
//     two and render a TRUNCATED row, silently. A width change is exactly the
//     kind of drift these assertions exist to catch, so it is a marker too.
//
// A marker can never be mistaken for data: no expectation in this suite matches
// one, so every degenerate row fails its assertion and says why.

// positionalSprint renders a row as its whole slot list ("[a b c]"), so the
// assertion pins slot ORDER as well as the values.
func PositionalSprint(r executor.QueryResult) string {
	if r.Positional == nil {
		return "<nil>"
	}
	return fmt.Sprint(r.Positional.Slots)
}

// positionalPipeSprint renders a row as its slot values joined by "|", in SLOT
// order. It is the order-sensitive replacement for the sorted-map-key rendering:
// same separator, same per-value formatting, so a converted site's expectations
// change only where the positional order differs from the alphabetical one.
func PositionalPipeSprint(r executor.QueryResult) string {
	if r.Positional == nil {
		return "<nil>"
	}
	parts := make([]string, len(r.Positional.Slots))
	for i, s := range r.Positional.Slots {
		parts[i] = UnnestSprint(s)
	}
	return strings.Join(parts, "|")
}

// positionalNamedPipeSprint renders a row as "NAME=value" pairs joined by "|", in
// SLOT order.
//
// It is the order-sensitive replacement for the OTHER blind form in this suite:
// the loop that collected a row map's keys, sorted them, and joined "k=v" pairs
// alphabetically. That form is blind twice over — a permutation of (Fields, Slots)
// re-sorts to the identical string, and positionalToMap has already collapsed any
// duplicate output name LAST-WINS before the sort ever runs, so a value is missing
// rather than merely misordered.
//
// Keeping the names (rather than converting those sites to positionalPipeSprint's
// bare values) is deliberate: at those sites the name is what identifies which
// output column a value belongs to, and dropping it would force every expectation
// to be re-derived from the SELECT list instead of merely re-ordered. Slot order
// plus the names is strictly more information than either renderer alone.
func PositionalNamedPipeSprint(r executor.QueryResult) string {
	if r.Positional == nil {
		return "<nil>"
	}
	slots := r.Positional.Slots
	// Slots and no layout. Loud, and it names the slot count — the shape that
	// used to render "map[]" and read as an empty row.
	if r.Positional.Type == nil {
		return fmt.Sprintf("<UNTYPED ROW: %d slot(s), no RecordType>", len(slots))
	}
	fields := r.Positional.Type.Fields
	// A width mismatch is a defect in the row, not a rendering detail. Truncating
	// to the shorter side drops real slots and still produces a string that could
	// match an expectation written before the width moved.
	if len(fields) != len(slots) {
		return fmt.Sprintf("<WIDTH MISMATCH: %d field(s) %v, %d slot(s) %v>",
			len(fields), r.Positional.TypeNames(), len(slots), slots)
	}
	parts := make([]string, 0, len(fields))
	for i, f := range fields {
		parts = append(parts, f.Name+"="+UnnestSprint(slots[i]))
	}
	return strings.Join(parts, "|")
}

// positionalSlots returns a row's slot VALUES in slot order, untyped and
// unrendered.
//
// The renderers above cover assertions that compare a row as a STRING. The other
// consumer shape needs the value itself — a test that type-asserts a slot
// (`.(int64)`, `math.Signbit(...)`, a struct-field read) cannot go through a
// renderer. Those sites reached for the name-keyed map instead, which reintroduces
// the whole defect for a typed read: `row["G"]` resolves by name, so a permuted
// (Fields, Slots) pair still hands back G's own value, and a duplicate output
// name hands back whichever one survived the last-wins collapse.
//
// It returns nil for a row with no positional row, so a caller that indexes it
// gets a bounds panic at the test rather than a silently wrong value.
func PositionalSlots(r executor.QueryResult) []any {
	if r.Positional == nil {
		return nil
	}
	return r.Positional.Slots
}

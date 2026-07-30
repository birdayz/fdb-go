package sqldriver_test

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
// copied into eight files — so the fix is one shared renderer and
// order-sensitive expectations everywhere, not a second assertion bolted onto
// one site.
//
// Both renderers route a row that carries NO positional row through
// unnestSprint(executor.RowValue(r)). Calling that a "name-keyed fallback" would
// overstate it: executor.RowValue returns nil the moment Positional is nil, so
// there is no name-keyed row on that branch and never can be — the branch renders
// "<nil>". It is a guard against a nil dereference, not an alternative rendering,
// and the test at the bottom of this file says so in those terms. What matters
// either way is that a row WITH slots is never rendered by name.

// positionalSprint renders a row as its whole slot list ("[a b c]"), so the
// assertion pins slot ORDER as well as the values.
func positionalSprint(r executor.QueryResult) string {
	if r.Positional == nil {
		return unnestSprint(executor.RowValue(r))
	}
	return fmt.Sprint(r.Positional.Slots)
}

// positionalPipeSprint renders a row as its slot values joined by "|", in SLOT
// order. It is the order-sensitive replacement for the sorted-map-key rendering:
// same separator, same per-value formatting, so a converted site's expectations
// change only where the positional order differs from the alphabetical one.
func positionalPipeSprint(r executor.QueryResult) string {
	if r.Positional == nil {
		return unnestSprint(executor.RowValue(r))
	}
	parts := make([]string, len(r.Positional.Slots))
	for i, s := range r.Positional.Slots {
		parts[i] = unnestSprint(s)
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
func positionalNamedPipeSprint(r executor.QueryResult) string {
	if r.Positional == nil || r.Positional.Type == nil {
		return unnestSprint(executor.RowValue(r))
	}
	slots := r.Positional.Slots
	parts := make([]string, 0, len(r.Positional.Type.Fields))
	for i, f := range r.Positional.Type.Fields {
		if i >= len(slots) {
			break
		}
		parts = append(parts, f.Name+"="+unnestSprint(slots[i]))
	}
	return strings.Join(parts, "|")
}

// TestPositionalRenderersSeeAPermutation is the proof that converting these
// assertions bought something.
//
// It builds ONE row and its PERMUTATION — the same (name, value) pairs, both
// moved to different slots — which is what a mis-bound leg window produces. The
// name-keyed rendering the converted sites used maps each name to its own value
// either way, so it is byte-identical across the permutation: an assertion built
// on it passes with every slot moved. The positional renderers must differ.
//
// Without this, "the map renderer is blind in the ORDER dimension" would be a
// claim in a comment, and the eight converted sites would be churn with no stated
// benefit.
func TestPositionalRenderersSeeAPermutation(t *testing.T) {
	t.Parallel()
	typ := func(names ...string) *values.RecordType {
		fields := make([]values.Field, len(names))
		for i, n := range names {
			fields[i] = values.Field{Name: n, FieldType: values.NotNullLong, Ordinal: i}
		}
		return &values.RecordType{Fields: fields}
	}
	row := func(rt *values.RecordType, vals ...any) executor.QueryResult {
		p := executor.NewPositionalRow(rt)
		for i, v := range vals {
			p.Set(i, v)
		}
		return executor.QueryResult{Positional: p}
	}

	// A|B|C holding 1|2|3, and the permutation C|A|B holding 3|1|2 — every name
	// still maps to its own value, every slot moved.
	straight := row(typ("A", "B", "C"), int64(1), int64(2), int64(3))
	permuted := row(typ("C", "A", "B"), int64(3), int64(1), int64(2))

	// The name-keyed rendering CANNOT tell them apart. Asserted, not assumed:
	// this is the blindness the conversion removes, and if RowValue ever starts
	// preserving order the conversion's rationale changes and this test says so.
	if a, b := unnestSprint(executor.RowValue(straight)), unnestSprint(executor.RowValue(permuted)); a != b {
		t.Errorf("the name-keyed rendering distinguished the permutation (%q vs %q).\n"+
			"  That is BETTER than assumed — but the converted assertions' rationale\n"+
			"  rests on it being blind here, so re-check that rationale.", a, b)
	}

	// The positional renderings must NOT be equal — that is the whole point.
	if a, b := positionalPipeSprint(straight), positionalPipeSprint(permuted); a == b {
		t.Errorf("positionalPipeSprint rendered a permuted row identically (%q).\n"+
			"  A mis-bound leg window IS a permutation, so a renderer that cannot see one\n"+
			"  cannot police the failure these tests exist for.", a)
	}
	if a, b := positionalSprint(straight), positionalSprint(permuted); a == b {
		t.Errorf("positionalSprint rendered a permuted row identically (%q)", a)
	}
	if a, b := positionalNamedPipeSprint(straight), positionalNamedPipeSprint(permuted); a == b {
		t.Errorf("positionalNamedPipeSprint rendered a permuted row identically (%q).\n"+
			"  It replaces the SORTED-map-key \"k=v|k=v\" form, whose whole defect was that\n"+
			"  re-sorting a permuted row reproduces the same string.", a)
	}
	// And the values themselves, in slot order, so a renderer that merely differs
	// (by hashing, say) does not satisfy this test.
	if got, want := positionalPipeSprint(straight), "1|2|3"; got != want {
		t.Errorf("positionalPipeSprint = %q, want %q — slot order, values joined by |", got, want)
	}
	if got, want := positionalPipeSprint(permuted), "3|1|2"; got != want {
		t.Errorf("permuted positionalPipeSprint = %q, want %q", got, want)
	}
	if got, want := positionalNamedPipeSprint(straight), "A=1|B=2|C=3"; got != want {
		t.Errorf("positionalNamedPipeSprint = %q, want %q", got, want)
	}
	if got, want := positionalNamedPipeSprint(permuted), "C=3|A=1|B=2"; got != want {
		t.Errorf("permuted positionalNamedPipeSprint = %q, want %q — slot order, and it is "+
			"the ORDER that differs from the alphabetical rendering the sorted-map loop "+
			"produced for both rows", got, want)
	}

	// And the DUPLICATE-name loss the sorted-map form suffered on top of the order
	// loss: two output columns legitimately share a name on a merged/unnest row, and
	// positionalToMap collapses them last-wins BEFORE any sort. So the map form
	// renders a row of width 3 as two entries and silently drops a value; the
	// positional form renders all three.
	dup := row(typ("V", "V", "W"), int64(1), int64(2), int64(3))
	if got, want := positionalNamedPipeSprint(dup), "V=1|V=2|W=3"; got != want {
		t.Errorf("positionalNamedPipeSprint on a duplicate-named row = %q, want %q — a "+
			"renderer that collapses duplicates loses a value outright, which is worse "+
			"than losing its order", got, want)
	}
	if m, ok := executor.RowValue(dup).(map[string]any); !ok || len(m) != 2 {
		t.Errorf("the name-keyed projection of a 3-slot duplicate-named row gave %v — the "+
			"last-wins collapse to 2 entries is the hazard the note on "+
			"executor.positionalToMap records; if it stopped collapsing, that note and "+
			"this rationale need re-checking", executor.RowValue(dup))
	}

	// A row with NO positional row must not panic either renderer. That is ALL this
	// checks — a nil guard, not a name-keyed-rendering test, and it used to be
	// labelled as the latter while asserting only `got != ""`. There is no
	// name-keyed row to assert on here and there cannot be: executor.RowValue
	// returns nil the moment Positional is nil, so this branch renders the nil
	// sentinel and could not render a map even in principle. The exact expected
	// string is asserted so the guard has teeth — `!= ""` passed for any output at
	// all, including a panic-adjacent garbage rendering.
	if got, want := positionalPipeSprint(executor.QueryResult{}), "<nil>"; got != want {
		t.Errorf("positionalPipeSprint on an empty QueryResult = %q, want %q — the "+
			"no-positional-row branch renders the nil sentinel, because RowValue yields "+
			"nil there rather than a name-keyed row", got, want)
	}
	if got, want := positionalSprint(executor.QueryResult{}), "<nil>"; got != want {
		t.Errorf("positionalSprint on an empty QueryResult = %q, want %q", got, want)
	}
}

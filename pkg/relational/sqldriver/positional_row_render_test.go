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
// Both renderers fall back to the name-keyed form when a row carries NO
// positional row. That fallback is not a compromise: a row with no positional
// slots has no order to assert, and rendering it by name is the only thing
// available. What matters is that a row WITH slots is never rendered by name.

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
	// And the values themselves, in slot order, so a renderer that merely differs
	// (by hashing, say) does not satisfy this test.
	if got, want := positionalPipeSprint(straight), "1|2|3"; got != want {
		t.Errorf("positionalPipeSprint = %q, want %q — slot order, values joined by |", got, want)
	}
	if got, want := positionalPipeSprint(permuted), "3|1|2"; got != want {
		t.Errorf("permuted positionalPipeSprint = %q, want %q", got, want)
	}

	// The no-positional-row fallback stays name-keyed: a row with no slots has no
	// order to assert, and the renderers must not panic on it.
	if got := positionalPipeSprint(executor.QueryResult{}); got == "" {
		t.Error("a row with no positional slots must still render (name-keyed fallback)")
	}
}

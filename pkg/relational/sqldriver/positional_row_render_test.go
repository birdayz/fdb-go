package sqldriver_test

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

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
// claim in a comment, and the converted sites would be churn with no stated
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
	if a, b := testkit.UnnestSprint(executor.RowValue(straight)), testkit.UnnestSprint(executor.RowValue(permuted)); a != b {
		t.Errorf("the name-keyed rendering distinguished the permutation (%q vs %q).\n"+
			"  That is BETTER than assumed — but the converted assertions' rationale\n"+
			"  rests on it being blind here, so re-check that rationale.", a, b)
	}

	// The positional renderings must NOT be equal — that is the whole point.
	if a, b := testkit.PositionalPipeSprint(straight), testkit.PositionalPipeSprint(permuted); a == b {
		t.Errorf("positionalPipeSprint rendered a permuted row identically (%q).\n"+
			"  A mis-bound leg window IS a permutation, so a renderer that cannot see one\n"+
			"  cannot police the failure these tests exist for.", a)
	}
	if a, b := testkit.PositionalSprint(straight), testkit.PositionalSprint(permuted); a == b {
		t.Errorf("positionalSprint rendered a permuted row identically (%q)", a)
	}
	if a, b := testkit.PositionalNamedPipeSprint(straight), testkit.PositionalNamedPipeSprint(permuted); a == b {
		t.Errorf("positionalNamedPipeSprint rendered a permuted row identically (%q).\n"+
			"  It replaces the SORTED-map-key \"k=v|k=v\" form, whose whole defect was that\n"+
			"  re-sorting a permuted row reproduces the same string.", a)
	}
	// And the values themselves, in slot order, so a renderer that merely differs
	// (by hashing, say) does not satisfy this test.
	if got, want := testkit.PositionalPipeSprint(straight), "1|2|3"; got != want {
		t.Errorf("positionalPipeSprint = %q, want %q — slot order, values joined by |", got, want)
	}
	if got, want := testkit.PositionalPipeSprint(permuted), "3|1|2"; got != want {
		t.Errorf("permuted positionalPipeSprint = %q, want %q", got, want)
	}
	if got, want := testkit.PositionalNamedPipeSprint(straight), "A=1|B=2|C=3"; got != want {
		t.Errorf("positionalNamedPipeSprint = %q, want %q", got, want)
	}
	if got, want := testkit.PositionalNamedPipeSprint(permuted), "C=3|A=1|B=2"; got != want {
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
	if got, want := testkit.PositionalNamedPipeSprint(dup), "V=1|V=2|W=3"; got != want {
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

	// A row with NO positional row must not panic either renderer. The exact
	// expected string is asserted so the guard has teeth — an earlier version
	// asserted only `got != ""`, which passed for any output at all.
	if got, want := testkit.PositionalPipeSprint(executor.QueryResult{}), "<nil>"; got != want {
		t.Errorf("positionalPipeSprint on an empty QueryResult = %q, want %q", got, want)
	}
	if got, want := testkit.PositionalSprint(executor.QueryResult{}), "<nil>"; got != want {
		t.Errorf("positionalSprint on an empty QueryResult = %q, want %q", got, want)
	}
	if got, want := testkit.PositionalNamedPipeSprint(executor.QueryResult{}), "<nil>"; got != want {
		t.Errorf("positionalNamedPipeSprint on an empty QueryResult = %q, want %q", got, want)
	}

	// THE TWO DEGENERATE ROWS, both previously unprobed and both previously QUIET.
	//
	// (1) Slots with no Type. The old code sent this through
	// unnestSprint(executor.RowValue(r)) and — MEASURED, not reasoned —
	// positionalToMap returns a nil map for a Type-less row, which fmt prints as
	// "map[]". So a row carrying two real values rendered as an empty map: not a
	// visible failure, a plausible-looking empty row. That is the exact class this
	// file exists to remove.
	untyped := executor.QueryResult{Positional: &executor.PositionalRow{Slots: []any{int64(7), int64(8)}}}
	if got, want := testkit.PositionalNamedPipeSprint(untyped), "<UNTYPED ROW: 2 slot(s), no RecordType>"; got != want {
		t.Errorf("positionalNamedPipeSprint on a Type-less row with slots = %q, want %q.\n"+
			"  If this ever renders \"map[]\" again, a row's values are being discarded into\n"+
			"  something that reads as an empty result.", got, want)
	}
	// And the measurement that motivates the marker, pinned so the claim above is
	// a fact rather than a recollection: the name-keyed projection of that same
	// row IS the empty-looking rendering.
	if got := fmt.Sprint(executor.RowValue(untyped)); got != "map[]" {
		t.Errorf("executor.RowValue on a Type-less 2-slot row renders %q, want \"map[]\" — the\n"+
			"  marker above exists BECAUSE this is what routing the row through the name-keyed\n"+
			"  projection produced. If it changed, re-derive the marker's rationale.", got)
	}

	// (2) A width mismatch between Fields and Slots. The old loop broke at the
	// shorter side and rendered a TRUNCATED row silently — so a row that grew a
	// slot rendered exactly as it did before, and a row that lost one rendered as
	// the narrower row an older expectation was written against. Both are the
	// assertion agreeing with a changed row.
	narrowSlots := executor.QueryResult{
		Positional: &executor.PositionalRow{Type: typ("A", "B", "C"), Slots: []any{int64(1), int64(2)}},
	}
	if got := testkit.PositionalNamedPipeSprint(narrowSlots); !strings.HasPrefix(got, "<WIDTH MISMATCH:") {
		t.Errorf("positionalNamedPipeSprint on 3 fields / 2 slots = %q, want a <WIDTH MISMATCH: ...>\n"+
			"  marker. Truncating to \"A=1|B=2\" is the silent form: it is byte-identical to the\n"+
			"  correct rendering of a genuine two-column row, so the assertion passes while a\n"+
			"  slot has gone missing.", got)
	}
	if got := testkit.PositionalNamedPipeSprint(narrowSlots); got == "A=1|B=2" {
		t.Errorf("positionalNamedPipeSprint TRUNCATED a 3-field/2-slot row to %q", got)
	}
	wideSlots := executor.QueryResult{
		Positional: &executor.PositionalRow{Type: typ("A", "B"), Slots: []any{int64(1), int64(2), int64(3)}},
	}
	if got := testkit.PositionalNamedPipeSprint(wideSlots); !strings.HasPrefix(got, "<WIDTH MISMATCH:") {
		t.Errorf("positionalNamedPipeSprint on 2 fields / 3 slots = %q, want a <WIDTH MISMATCH: ...>\n"+
			"  marker — the extra slot is DROPPED by a fields-driven loop, which is the same\n"+
			"  invisibility in the other direction", got)
	}
	if got := testkit.PositionalNamedPipeSprint(wideSlots); got == "A=1|B=2" {
		t.Errorf("positionalNamedPipeSprint DROPPED the third slot of a 2-field/3-slot row: %q", got)
	}
}

// unnestUnstableSprintf stands in for a protobuf message: a value whose fmt
// rendering carries the extra separator width that prototext's detrand adds.
type unnestUnstableSprintf struct{}

func (unnestUnstableSprintf) String() string { return "SUB:5  K:0  SUBSTRUCT:{DEEP:11  LEAF:0}" }

// TestUnnestSprintIsStableAcrossBuilds pins that a slot holding a protobuf
// message renders to bytes an expectation can be written against.
//
// prototext varies its whitespace ON PURPOSE (internal/detrand), seeded from the
// binary — so the rendering of a message-valued slot is constant within one test
// binary and flips on the next build that perturbs it. That is worse than plain
// nondeterminism: an expectation written against it passes locally, passes in
// CI, and then fails on an unrelated edit. It is the reason the duplicate-name
// SELECT * row in buried_chained_rotation_fdb_test.go was pinned only by COUNT
// for as long as it was, and that row's values are now asserted BECAUSE this
// normalization exists.
//
// A slot holding a real string must be untouched: unnestSprint returns those
// verbatim, so text with its own double spaces never reaches the collapse.
func TestUnnestSprintIsStableAcrossBuilds(t *testing.T) {
	t.Parallel()
	if got, want := testkit.UnnestSprint(unnestUnstableSprintf{}), "SUB:5 K:0 SUBSTRUCT:{DEEP:11 LEAF:0}"; got != want {
		t.Errorf("unnestSprint of a message-like value = %q, want %q — the separator width\n"+
			"  prototext randomizes per build must be collapsed, or no expectation over a\n"+
			"  message-valued slot survives a rebuild", got, want)
	}
	if got, want := testkit.UnnestSprint("a  b"), "a  b"; got != want {
		t.Errorf("unnestSprint of a STRING slot = %q, want %q — string slots are returned\n"+
			"  verbatim; collapsing them would corrupt real data", got, want)
	}
	if got, want := testkit.UnnestSprint([]any{"x", 1}), "[x 1]"; got != want {
		t.Errorf("unnestSprint of a slice = %q, want %q", got, want)
	}

	// A string INSIDE a composite. The comment on unnestCollapseSpaces used to
	// claim real text was never touched because "the string case returns it raw"
	// — false: fmt.Sprint([]any{"a  b"}) is "[a  b]", one blob, and the collapse
	// then rewrote the DATA to "[a b]". It is reachable, not theoretical: SARR
	// and STRARR are STRING arrays and `SELECT *` puts the whole array in a slot.
	// unnestSprint now renders a slice element-wise, so each element is a string
	// slot again.
	if got, want := testkit.UnnestSprint([]any{"a  b"}), "[a  b]"; got != want {
		t.Errorf("unnestSprint of a slice holding a double-spaced STRING = %q, want %q —\n"+
			"  a composite is rendered element-wise precisely so its string elements keep\n"+
			"  their own spacing; collapsing them corrupts row data an expectation is\n"+
			"  written against", got, want)
	}
	if got, want := testkit.UnnestSprint([]any{"a  b", "c"}), "[a  b c]"; got != want {
		t.Errorf("unnestSprint of a mixed slice = %q, want %q — the element-wise form must\n"+
			"  still reproduce fmt's slice rendering exactly (single-space separators, square\n"+
			"  brackets), or every existing composite expectation moves", got, want)
	}
	if got, want := testkit.UnnestSprint([]string{"a  b"}), "[a  b]"; got != want {
		t.Errorf("unnestSprint of a typed string slice = %q, want %q — the element-wise\n"+
			"  path is by KIND, not by []any, because a slot can carry a concrete slice", got, want)
	}
	// A message nested in a composite must STILL be collapsed — the element-wise
	// path routes each element back through unnestSprint rather than short-circuiting.
	if got, want := testkit.UnnestSprint([]any{unnestUnstableSprintf{}}), "[SUB:5 K:0 SUBSTRUCT:{DEEP:11 LEAF:0}]"; got != want {
		t.Errorf("unnestSprint of a slice holding a message-like value = %q, want %q — the\n"+
			"  build-unstable separator must be collapsed inside a composite too, or a\n"+
			"  repeated message slot is unpinnable again", got, want)
	}
	// And the boundary that remains, pinned so it is a STATED limitation rather
	// than an unnoticed one: a string nested inside a value fmt renders as one
	// blob (a proto message's own string field, a struct field) is NOT
	// distinguishable from a separator, and its spacing is collapsed.
	if got, want := testkit.UnnestSprint(unnestStringInsideABlob{}), "K:{X:a b}"; got != want {
		t.Errorf("unnestSprint of a Stringer embedding a double-spaced string = %q, want %q —\n"+
			"  this is the collapse's REMAINING reach, deliberately pinned: fmt renders such a\n"+
			"  value as one blob, so content and separator are the same bytes. If this ever\n"+
			"  needs to be exact, the fix is a structural rendering of the message, not a\n"+
			"  wider collapse.", got, want)
	}
}

func TestUnnestSprintRendersExactOrdinalRecordStructurally(t *testing.T) {
	t.Parallel()

	nestedType := values.NewRecordType("NESTED", false, []values.Field{{
		Name: "DEEP", FieldType: values.NotNullLong, Ordinal: 0,
	}})
	elementType := values.NewRecordType("ELEM", false, []values.Field{
		{Name: "SUB", FieldType: values.NewArrayType(false, values.NotNullLong), Ordinal: 0},
		{Name: "K", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "NESTED", FieldType: values.NewArrayType(false, nestedType), Ordinal: 2},
	})
	row := &executor.PositionalRow{Type: elementType, Slots: []any{
		[]any{int64(1), int64(7)}, int64(0), []any{
			&executor.PositionalRow{Type: nestedType, Slots: []any{int64(11)}},
			&executor.PositionalRow{Type: nestedType, Slots: []any{int64(13)}},
		},
	}}

	if got, want := testkit.UnnestSprint(row), "SUB:1 SUB:7 K:0 NESTED:{DEEP:11} NESTED:{DEEP:13}"; got != want {
		t.Fatalf("ordinal record rendering = %q, want %q", got, want)
	}
	if row.Slots[1] != int64(0) {
		t.Fatalf("renderer mutated source row: %#v", row.Slots)
	}
}

// unnestStringInsideABlob stands in for a proto message carrying a string field
// whose own value contains a double space — the case the whitespace collapse
// cannot distinguish from prototext's separator.
type unnestStringInsideABlob struct{}

func (unnestStringInsideABlob) String() string { return "K:{X:a  b}" }

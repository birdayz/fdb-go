package docscheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The gate in field_name_decision_test.go is only worth its line count if it
// actually FIRES on the shapes it claims to cover. Its first version did not:
// it caught `fv.Field == x` and missed `return fv.Field` entirely — so
// `correlatedInnerField`, one of the seven bugs the gate was written for and
// the function it is named after in the header comment, was invisible to it.
// A regression of that exact bug would have gone in green.
//
// A build gate is a claim about what CANNOT reach the tree. Nothing tested that
// claim, and "the tree is clean" is worthless if the detector matches nothing.
// So both directions are pinned here against synthetic source:
//
//   - RECALL: each covered shape must be reported. This is what was broken.
//   - PRECISION: `.Field` on an unrelated struct (an error's display field, a
//     SortKey) and constructing a FieldValue FROM a name must stay silent. A
//     noisy gate gets read as noise, and then its real findings get scrolled
//     past — the same end state as no gate at all.
func scanSnippet(t *testing.T, body string) []string {
	t.Helper()
	forms, _ := scanSnippetInPackage(t, "p", body)
	return forms
}

// scanSnippetInPackage additionally controls the PACKAGE CLAUSE and returns the
// reported LINE of each decision.
//
// The package matters because the gate reads it: inside `values` the type is
// written `*FieldValue`, unqualified, and the discriminator has to accept that
// or the package declaring FieldValue is the one directory the gate cannot see
// into. The lines matter because the ratchet counts decisions PER LINE, and a
// callback that reports one decision per line is indistinguishable from one that
// counts correctly until a fixture puts two on the same line.
func scanSnippetInPackage(t *testing.T, pkg, body string) (forms []string, lines []int) {
	t.Helper()
	src := "package " + pkg + "\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n\t\"slices\"\n)\n\nvar _, _, _ = fmt.Sprintf, strings.Contains, slices.Contains\n\n" + body
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse snippet: %v\n---\n%s", err, src)
	}
	scanFieldDecisions(f, func(pos token.Pos, form string) {
		forms = append(forms, form)
		lines = append(lines, fset.Position(pos).Line)
	})
	return forms, lines
}

func TestFieldDecisionDetectorFiresOnEveryCoveredShape(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		want string // substring of the reported form
		body string
	}{
		{
			name: "equality",
			want: "== comparison",
			body: `func f(fv *values.FieldValue, s string) bool { return fv.Field == s }`,
		},
		{
			name: "inequality",
			want: "!= comparison",
			body: `func f(fv *values.FieldValue, s string) bool { return fv.Field != s }`,
		},
		{
			// Sorting BY leaf name is leaf-name-as-identity just as much as
			// comparing by it, and was unchecked in the first version.
			name: "ordering comparison",
			want: "< comparison",
			body: `func f(a, b *values.FieldValue) bool { return a.Field < b.Field }`,
		},
		{
			name: "switch tag",
			want: "switch tag",
			body: `func f(fv *values.FieldValue) int {
	switch fv.Field {
	case "A":
		return 1
	}
	return 0
}`,
		},
		{
			// An EMPTY-tag switch has no Tag node; it must still be caught,
			// via the ordinary BinaryExpr in each case clause.
			name: "empty-tag switch case",
			want: "== comparison",
			body: `func f(fv *values.FieldValue, s string) int {
	switch {
	case fv.Field == s:
		return 1
	}
	return 0
}`,
		},
		{
			name: "map index",
			want: "map key",
			body: `func f(m map[string]bool, fv *values.FieldValue) bool { return m[fv.Field] }`,
		},
		{
			// Never produces an IndexExpr, so the index check alone misses it.
			name: "composite-literal key",
			want: "composite-literal key",
			body: `func f(fv *values.FieldValue) map[string]bool {
	return map[string]bool{fv.Field: true}
}`,
		},
		{
			name: "method-selector string helper",
			want: "EqualFold call",
			body: `func f(fv *values.FieldValue, s string) bool { return strings.EqualFold(fv.Field, s) }`,
		},
		{
			// A package-level generic helper. Still a selector, but the ARGUMENT
			// carries the name rather than the receiver — unmatched while the
			// check only looked at the receiver of a method call.
			name: "generic package-level helper",
			want: "Contains call",
			body: `func f(names []string, fv *values.FieldValue) bool { return slices.Contains(names, fv.Field) }`,
		},
		{
			// A same-package helper is a bare Ident, not a selector. Without the
			// Ident arm in callFuncName this is invisible — and a mutation that
			// deleted that arm survived until this case existed, which is the
			// whole reason the arm needed its own fixture rather than sharing
			// the selector one above.
			name: "bare same-package helper call",
			body: `func Contains(names []string, s string) bool { return false }

func f(names []string, fv *values.FieldValue) bool { return Contains(names, fv.Field) }`,
			want: "Contains call",
		},
		{
			// Explicit type arguments wrap the callee in an IndexExpr, so the
			// selector underneath is no longer at the top of the call.
			name: "explicitly instantiated generic call",
			want: "Contains call",
			body: `func f(names []string, fv *values.FieldValue) bool { return slices.Contains[[]string, string](names, fv.Field) }`,
		},
		{
			// THE shape that defeated the first version, in the function the
			// gate is named after. The caller sees a string and has nothing
			// left to consult.
			name: "escape via bare return",
			want: "escaping as a bare string",
			body: `func correlatedInnerField(v values.Value) (string, bool) {
	fv, ok := v.(*values.FieldValue)
	if !ok {
		return "", false
	}
	return fv.Field, true
}`,
		},
		{
			name: "escape laundered through ToUpper",
			want: "escaping as a bare string",
			body: `func f(v values.Value) string {
	if fv, ok := v.(*values.FieldValue); ok {
		return strings.ToUpper(fv.Field)
	}
	return ""
}`,
		},
		{
			// Requiring `.Field` to be the sink's IMMEDIATE child let one call
			// wrapper hide the decision. This exact line lives in
			// match_candidate_index.go and passed the first version of the gate.
			name: "map key laundered through ToUpper",
			want: "map key",
			body: `func f(covered map[string]struct{}, fv *values.FieldValue) bool {
	_, ok := covered[strings.ToUpper(fv.Field)]
	return ok
}`,
		},
		{
			name: "switch tag laundered through ToUpper",
			want: "switch tag",
			body: `func f(fv *values.FieldValue) int {
	switch strings.ToUpper(fv.Field) {
	case "A":
		return 1
	}
	return 0
}`,
		},
		{
			name: "comparison laundered through ToUpper",
			want: "== comparison",
			body: `func f(fv *values.FieldValue, s string) bool { return strings.ToUpper(fv.Field) == s }`,
		},
		{
			// The parent FuncDecl names the type; a closure inside it inherits
			// that, or every escape behind a callback goes unseen.
			name: "escape from a closure inside a FieldValue-handling func",
			want: "escaping as a bare string",
			body: `func f(v values.Value) func() string {
	fv, _ := v.(*values.FieldValue)
	return func() string { return fv.Field }
}`,
		},
		{
			// A whitelist of WRAPPERS can never reach a concatenation: the map
			// key is a BinaryExpr, not a call, so no set of laundering function
			// names would have matched it. This exact line is
			// pkg/relational/core/embedded/cascades_generator.go:3186, and its
			// sibling one arm below (:3192 — identical lookup, no leg prefix) was
			// recorded as debt while this one passed silently.
			name: "map key laundered through concatenation",
			want: "map key",
			body: `func f(inner map[string]int, fv *values.FieldValue, legPrefix string) int {
	return inner[legPrefix+strings.ToUpper(fv.Field)]
}`,
		},
		{
			// Slicing the flat "ALIAS.col" name to get the qualifier is the same
			// blind spot through a SliceExpr —
			// pkg/relational/core/query/cascades_translator.go:5730 and
			// pkg/relational/core/query/exists_gathered_cluster_wrap.go:131.
			name: "map key laundered through a slice of the name",
			want: "map key",
			body: `func f(layouts map[string]int, fv *values.FieldValue, dot int) int {
	return layouts[strings.ToUpper(fv.Field[:dot])]
}`,
		},
		{
			// The comparison form of the same slice —
			// pkg/relational/core/query/cascades_translator.go:5674.
			name: "comparison against a slice of the name",
			want: "!= comparison",
			body: `func f(fv *values.FieldValue, key string, dot int) bool {
	return strings.ToUpper(fv.Field[:dot]) != key
}`,
		},
		{
			// A return may not use deep containment (a constructor call would
			// match), so concatenation needs its own arm in the escape shapes.
			name: "escape via concatenation",
			want: "escaping as a bare string",
			body: `func f(v values.Value, prefix string) string {
	if fv, ok := v.(*values.FieldValue); ok {
		return prefix + fv.Field
	}
	return ""
}`,
		},
		{
			name: "escape via a slice of the name",
			want: "escaping as a bare string",
			body: `func f(v values.Value, dot int) string {
	if fv, ok := v.(*values.FieldValue); ok {
		return fv.Field[:dot]
	}
	return ""
}`,
		},
		{
			// Two launderings stacked. One level of peeling sees a CallExpr whose
			// callee is not a launderer and stops — which is how
			// aggregateOperandColumn escaped the name in plain sight. That function
			// no longer exists (migrated under RFC-197 item 2 — the aggregate
			// operand's width now indexes the input's typed column list by ordinal),
			// so there is no line to cite: the fixture IS the record of the shape.
			// Below a launderer the argument is already a string, so there is no
			// constructor to confuse and the search goes to any depth.
			name: "escape laundered through a nested string helper",
			want: "escaping as a bare string",
			body: `func stripColumnQualifier(s string) string { return s }

func f(v values.Value) (string, bool) {
	fv, ok := v.(*values.FieldValue)
	if !ok {
		return "", false
	}
	return strings.ToUpper(stripColumnQualifier(fv.Field)), true
}`,
		},
		{
			// ONE local assignment between the name and the sink hid the
			// decision from both tiers, because both inspect only the sink
			// expression. This is buriedLegOrdinalLayout — one of the seven —
			// verbatim, as it stood: it built a `corr + "." + name` key and probed
			// and wrote the layout under it. The gate was named for that function
			// and could not see it. The name-built key is GONE (the function is
			// now keyed by values.ColumnIdentity —
			// pkg/recordlayer/query/plan/cascades/rule_implement_nested_loop_join.go:2226),
			// so the fixture is the only surviving statement of the shape.
			name: "map key via a local derived from the name",
			want: "a map key via local key derived from the name",
			body: `func f(layout map[string]int, fv *values.FieldValue, corr string) int {
	key := strings.ToUpper(corr) + "." + strings.ToUpper(fv.Field)
	return layout[key]
}`,
		},
		{
			// The second of the two invisible bugs: fieldValueAliasAndCol, which
			// assigned `upper := strings.ToUpper(fv.Field)` and returned two slices
			// of it. A returned SLICE of a local holding the name is the name
			// leaving as a bare string exactly as much as a slice of `fv.Field`
			// is. The function was deleted under RFC-197 item 2, so there is no
			// line to cite and this fixture is what keeps the shape detectable.
			name: "return escape via a local derived from the name",
			want: "escaping as a bare string (return) via local upper derived from the name",
			body: `func f(fv *values.FieldValue, dot int) (string, string) {
	upper := strings.ToUpper(fv.Field)
	return upper[:dot], upper[dot+1:]
}`,
		},
		{
			// The engine's name matching lives in Replace/pull-up closures, so
			// a taint pass that stopped at a FuncLit would be blind to most of
			// it. The scoping question is settled by keying the taint on the
			// DECLARATION, not by refusing to look inside closures.
			//
			// How much that is worth is MEASURED rather than asserted, and the
			// measurement is committed: TestFieldDecisionsInsideClosuresAreReported
			// enumerates the engine decisions that are reported from inside a
			// FuncLit and fails if the walk stops descending. An earlier version of
			// this comment listed the sites inline, and three of the six it named
			// were gone by the time anyone read it — an inline site list is a
			// measurement that rots, which is the failure the whole gate exists to
			// prevent one level down.
			name: "helper call via a local derived from the name inside a closure",
			want: "a Contains call via local leaf derived from the name",
			body: `func f(v values.Value, cols []string) bool {
	match := func(node values.Value) bool {
		fv, ok := node.(*values.FieldValue)
		if !ok {
			return false
		}
		leaf := fv.Field
		return slices.Contains(cols, leaf)
	}
	return match(v)
}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := scanSnippet(t, tc.body)
			for _, g := range got {
				if strings.Contains(g, tc.want) {
					return
				}
			}
			t.Fatalf("the gate did not fire on a shape it claims to cover.\n"+
				"want a report containing %q, got %v\n\nsource:\n%s\n\n"+
				"A gate that misses a shape reads as coverage while providing none — "+
				"which is exactly how the escape-via-return case shipped.", tc.want, got, tc.body)
		})
	}
}

func TestFieldDecisionDetectorStaysSilentOnUnrelatedField(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
	}{
		{
			// `Field` is a common struct-field name. An error type's display
			// field is not a column identity, and flagging every Error() method
			// buries the real signal.
			name: "error struct display field",
			body: `type UnresolvableOrdinalError struct {
	Field  string
	Source string
}

func (e *UnresolvableOrdinalError) Error() string {
	return fmt.Sprintf("column %q against %q", e.Field, e.Source)
}`,
		},
		{
			name: "unrelated struct field returned",
			body: `type SortKey struct{ Field string }

func (k SortKey) Name() string { return k.Field }`,
		},
		{
			// Building a FieldValue FROM a name is what the values package is
			// for. Flagging construction would flag the correct code, including
			// the resolved-ordinal path that FIXED one of the seven bugs.
			name: "name passed to a FieldValue constructor",
			body: `func f(fv *values.FieldValue, alias values.CorrelationIdentifier) values.Value {
	return values.NewFieldValueWithResolvedOrdinal(fv.Field, 0, fv.Typ)
}`,
		},
		{
			// Reading the name to RENDER it decides nothing.
			name: "name only formatted into a message",
			body: `func f(fv *values.FieldValue) error {
	return fmt.Errorf("cannot resolve %s", fv.Field)
}`,
		},
		{
			// Against "" it partitions "has a name" from "has none" — Java's
			// null accessor name, i.e. pure ordinal access. It cannot confuse
			// column A with column B, which is the only failure this gate is
			// about, so flagging it told four sites to consult Resolved for a
			// question Resolved does not answer.
			name: "emptiness test on the name",
			body: `func f(acc *values.FieldValue) bool { return acc.Field == "" }`,
		},
		{
			// FieldValue.Field is a string and cannot be compared to nil, so a
			// nil comparison proves the receiver is something else. This is a
			// protobuf KeyExpression variant selector; it spent a release in the
			// debt list described as a decision it does not make, pointing its
			// reader at a Resolved accessor the type does not have.
			name: "protobuf variant selector compared to nil",
			body: `func f(expression *gen.KeyExpression) bool {
	switch {
	case expression.Field != nil:
		return expression.Field.FanType == nil
	}
	return false
}`,
		},
		{
			// The same protobuf variant reached through a GETTER rather than a
			// nil test. What the comparison sees is a FanType enum, not a name.
			//
			// This is the measured cost of deep containment, and it is why the
			// deep tier is gated on the function naming the type while the
			// shape-matched tier is not: ungated, this shape reports four sites
			// in index_expansion.go and match_candidate_index.go, and gating BOTH
			// tiers instead silences two sites the gate holds today. Neither
			// half of that trade is visible from the tree walk, which is green
			// either way.
			name: "protobuf variant selector read through a getter",
			body: `func f(expression *gen.KeyExpression) bool {
	return expression.Field.GetFanType() == gen.Field_FAN_OUT
}`,
		},
		{
			// A local keyed into a map is only a decision if the local holds the
			// NAME. This one holds an unrelated string in a function that does
			// handle a FieldValue, so the taint pass has every chance to be
			// sloppy and must not take it.
			name: "local assigned from something else entirely, used as a map key",
			body: `func f(m map[string]bool, fv *values.FieldValue, names []string, i int) bool {
	if fv.Child != nil {
		return false
	}
	key := strings.ToUpper(names[i])
	return m[key]
}`,
		},
		{
			// The measured reason the taint predicate is escapesFieldName (which
			// proves the result is still a STRING holding the name) rather than
			// deep containment (which accepts anything an expression MENTIONING
			// the name produces). Under deep containment `dot` is a display name,
			// `dot >= 0` is an identity decision, and the debt list grows by 53
			// entries against the 22 real ones — int offsets, loop indices,
			// slices built beside the name — for which there is no Resolved
			// accessor to consult and so no possible fix. A list padded with
			// unfixable entries is one nobody reads, which costs the real
			// entries their audience.
			name: "int offset computed from a local holding the name",
			body: `func f(fv *values.FieldValue) bool {
	upper := strings.ToUpper(fv.Field)
	dot := strings.IndexByte(upper, '.')
	return dot >= 0
}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := scanSnippet(t, tc.body); len(got) > 0 {
				t.Fatalf("the gate fired on a shape that decides nothing: %v\n\nsource:\n%s\n\n"+
					"False positives are not harmless here: the debt list is read by hand, and a "+
					"gate padded with non-findings trains its readers to skip it.", got, tc.body)
			}
		})
	}
}

// Inside cascades/values/ the type is written `*FieldValue`. Matching only the
// QUALIFIED `values.FieldValue` meant the return-escape check never armed for a
// single function in the package that DECLARES FieldValue — including
// ProjectionColumnName, which hands the display name out as the projection
// output-naming authority the rest of the engine reads through.
//
// The widening is scoped to that package, so both directions are pinned: the
// same source is a decision in `values` and not one anywhere else, where a bare
// `FieldValue` is some other package's type entirely.
func TestFieldDecisionDetectorSeesUnqualifiedFieldValueInValuesPackage(t *testing.T) {
	t.Parallel()

	const body = `func ProjectionColumnName(v Value) string {
	if fv, ok := v.(*FieldValue); ok {
		return fv.Field
	}
	return ""
}`

	inValues, _ := scanSnippetInPackage(t, "values", body)
	var escaped bool
	for _, form := range inValues {
		if strings.Contains(form, "escaping as a bare string") {
			escaped = true
		}
	}
	if !escaped {
		t.Fatalf("the gate did not see an escape written with an UNQUALIFIED *FieldValue, got %v.\n\n"+
			"Nothing in cascades/values/ writes `values.FieldValue`, so a discriminator that only "+
			"matches the qualified selector is blind to the entire package that declares the type — "+
			"and it stayed blind to the projection output-naming authority sitting in it.", inValues)
	}

	if outside, _ := scanSnippetInPackage(t, "p", body); len(outside) > 0 {
		t.Fatalf("the gate fired on a bare `FieldValue` OUTSIDE the values package: %v.\n\n"+
			"The widening is package-scoped on purpose. Elsewhere a bare FieldValue names some "+
			"unrelated type, and accepting it there is how the discriminator stops discriminating.", outside)
	}
}

// The taint follows the DECLARATION, never the spelling.
//
// Keying it by name reports rule_implement_nested_loop_join.go:2269-2270, where
// a second `key := leg.Name + "." + strings.ToUpper(fields[…].Name)` in a
// sibling block of the same function is keyed into a map. That key is built from
// a record constructor's column names and never touches a FieldValue, so the
// entry would be a lie on a list whose entire value is that every line on it is
// true. go/parser resolves block scopes, so the two `key`s are two objects.
//
// This fixture is also the tripwire on that resolution. *ast.Object is
// deprecated; if it ever stops being populated the taint set empties and the
// gate silently loses every local-hop site it holds — a build gate degrading to
// green is the worst failure mode it has. Here the degradation is loud in both
// directions: no resolution means either nothing reports, or the fallback
// reports both.
func TestFieldDecisionTaintFollowsTheDeclarationNotTheSpelling(t *testing.T) {
	t.Parallel()

	forms, lines := scanSnippetInPackage(t, "p", `func f(fv *values.FieldValue, names []string) map[string]int {
	layout := map[string]int{}
	{
		key := strings.ToUpper(fv.Field)
		layout[key] = 1
	}
	{
		key := strings.ToUpper(names[0])
		layout[key] = 2
	}
	return layout
}`)

	if len(forms) != 1 {
		t.Fatalf("two same-named locals in sibling scopes produced %d report(s) on line(s) %v: %v\n\n"+
			"Exactly one of them holds the name. Reporting the other puts a site on the debt "+
			"list that never touches a FieldValue, and a debt list is only worth reading while "+
			"every line on it is true; reporting NEITHER means identifier resolution went away "+
			"underneath the taint set and every local-hop site the gate holds is now invisible.",
			len(forms), lines, forms)
	}
	if !strings.Contains(forms[0], "via local key derived from the name") {
		t.Fatalf("report = %q, want the map-key decision naming the local it arrived through", forms[0])
	}
	// scanSnippetInPackage prepends a fixed 10-line prologue, so the first
	// block's `layout[key] = 1` is line 15 and the second block's is line 19.
	// Naming the line is what makes this test discriminate: both blocks report
	// the identical form string, so the LINE is the only evidence of which `key`
	// the taint matched.
	if lines[0] != 15 {
		t.Fatalf("the decision was reported on line %d, want 15 — the FIRST block, whose key "+
			"holds fv.Field. Line 19 is the second block, whose key is built from an unrelated "+
			"string; reporting it means the taint matched a spelling.", lines[0])
	}
}

// The shallow tier types a direct `.Field` selector by SPELLING ALONE, and the
// two tests below pin both halves of that trade so it cannot be quietly
// renegotiated. This half: a name-typed CARRIER must stay visible.
//
// A carrier is a plain struct field holding a string that came off a FieldValue
// upstream — plans.SortKey.Field (in_memory_sort.go:142) and the conformance
// oracle's sort key (rowdiff/ordering.go:241). Comparing two of them conflates
// two same-named columns exactly as comparing two `fv.Field`s does; the name
// simply travelled one type further from where it was read. Both are recorded
// debt.
//
// Neither function names FieldValue anywhere, so gating the shallow tier on the
// type discriminator — the obvious "fix" for the precision cost pinned below —
// silences both. That was measured, and it loses on both axes: two real sites
// traded for four protobuf false positives.
func TestFieldDecisionDetectorReportsNameTypedCarriers(t *testing.T) {
	t.Parallel()

	got := scanSnippet(t, `type SortKey struct {
	Field     string
	Ascending bool
}

func sortKeysEqual(a, b SortKey) bool { return a.Field == b.Field }`)

	for _, form := range got {
		if strings.Contains(form, "== comparison") {
			return
		}
	}
	t.Fatalf("a name-typed CARRIER comparison was not reported: %v\n\n"+
		"plans.SortKey.Field holds a leaf name read off a FieldValue upstream, and comparing "+
		"two of them conflates two same-named columns just as comparing two `fv.Field`s does. "+
		"in_memory_sort.go:142 and rowdiff/ordering.go:241 are exactly this shape and are "+
		"recorded debt. If this went silent because the shallow tier was gated on the type "+
		"discriminator, that gating also has to explain the four protobuf false positives it "+
		"lets back in.", got)
}

// The other half of the same trade, asserted as CURRENT behavior rather than as
// something desirable: a `.Field` selector on a type with no relation to
// FieldValue, in a function that never mentions the type, IS reported.
//
// This is a false positive and it is the accepted price. The gate cannot
// distinguish it from the carrier above without type information, and buying the
// precision back by gating the shallow tier on the discriminator costs the
// carriers — which are real debt on real columns. Precision on unrelated structs
// is the cheaper thing to spend.
//
// The test asserts the cost so that removing it is a DECISION. Someone adding
// type-based gating as a "precision fix" turns this red and the carrier test
// above red at the same time, and has to re-derive which side of the trade they
// are on rather than discovering it in production. If the trade is genuinely
// re-made — full type checking, say, which answers both — delete both tests
// together and say so.
func TestFieldDecisionShallowTierCostsPrecisionOnUnrelatedStructs(t *testing.T) {
	t.Parallel()

	got := scanSnippet(t, `type HTTPValidationError struct {
	Field  string
	Reason string
}

func rejects(e HTTPValidationError, s string) bool { return e.Field == s }`)

	for _, form := range got {
		if strings.Contains(form, "== comparison") {
			return
		}
	}
	t.Fatalf("an unrelated `.Field` comparison stopped being reported: %v\n\n"+
		"This test pins a COST, not a virtue. The shallow tier types a direct selector by "+
		"spelling, so it cannot tell this from plans.SortKey.Field — a carrier holding a name "+
		"read off a FieldValue, and real debt. If this went silent because type-based gating "+
		"was added, TestFieldDecisionDetectorReportsNameTypedCarriers is red too, and the "+
		"question to answer is which of the two the gate is for.", got)
}

// The ratchet counts decisions PER LINE, and in the real tree every entry
// carries n == 1 — so `seen[key]++` and `seen[key] = 1` produce identical
// results over all 828 files. The counting is the whole difference between a
// ratchet and a presence check, and nothing exercised it.
func TestFieldDecisionDetectorCountsEveryDecisionOnAPackedLine(t *testing.T) {
	t.Parallel()

	// Two map lookups packed into one line, the shape logical_predicate.go:4151
	// carried three of.
	forms, lines := scanSnippetInPackage(t, "p", `func f(m map[string]bool, a, b *values.FieldValue) bool {
	return m[a.Field] && m[b.Field]
}`)

	if len(forms) != 2 {
		t.Fatalf("a line hosting two decisions produced %d report(s): %v\n\n"+
			"The debt list records HOW MANY decisions a line hosts. A callback that fires once "+
			"per line makes every count 1, and then deleting one of three violations on a packed "+
			"line leaves the ratchet green.", len(forms), forms)
	}
	if lines[0] != lines[1] {
		t.Fatalf("the two decisions were reported on lines %d and %d, want the same line — "+
			"the fixture is not exercising per-line counting at all", lines[0], lines[1])
	}
}

// The accumulation, end to end: scan a file, fold each decision into the
// tallies, and check that a debt line hosting TWO decisions is counted as two.
//
// This is the composition the tree walk cannot test. Every recorded site carries
// n == 1, so `seen[key]++` and `seen[key] = 1` agree on all 828 files; only a
// packed line separates them, and the tree has none among the listed sites.
func TestTallyCountsRepeatedDecisionsOnOneDebtLine(t *testing.T) {
	t.Parallel()

	// Line 4 of the source below hosts two map-key decisions; line 5 hosts one
	// that nothing covers, so the offense path is exercised in the same pass.
	const body = `package p

func f(m map[string]bool, a, b, c *values.FieldValue) bool {
	return m[a.Field] && m[b.Field] ||
		m[c.Field]
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", body, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	debt := map[string]fieldDebt{"x.go:4": {2, "name-keyed: two lookups packed onto one line"}}
	seenAllowed, seenDebt := map[string]int{}, map[string]int{}
	offenses := tallyFieldDecisions("x.go", fset, f, nil, debt, seenAllowed, seenDebt)

	if seenDebt["x.go:4"] != 2 {
		t.Fatalf("a debt line hosting two decisions tallied %d.\n\n"+
			"The tally must COUNT, not merely mark the line seen: with a bare assignment every "+
			"site reads as 1, and then a second violation landing on a listed line — or one of a "+
			"pair being deleted — passes the ratchet silently.", seenDebt["x.go:4"])
	}
	if len(offenses) != 1 || !strings.Contains(offenses[0], "x.go:5") {
		t.Fatalf("offenses = %v, want exactly the unlisted x.go:5 decision", offenses)
	}
	if len(seenAllowed) != 0 {
		t.Fatalf("seenAllowed = %v, want empty against an empty allowlist", seenAllowed)
	}

	// And the tally must now AGREE with the entry that declared two.
	if got := debtMismatches(debt, seenDebt); len(got) > 0 {
		t.Fatalf("an entry recording 2 decisions on a line that hosts 2 was reported stale: %v", got)
	}
}

// The detector must not depend on a snippet parsing into anything exotic — if
// scanSnippet ever silently produced an empty file, every RECALL case above
// would pass by matching nothing and every PRECISION case by finding nothing.
func TestFieldDecisionDetectorSnippetHarnessIsLive(t *testing.T) {
	t.Parallel()
	src := "package p\n\nfunc f(fv *values.FieldValue, s string) bool { return fv.Field == s }\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var decls int
	ast.Inspect(f, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncDecl); ok {
			decls++
		}
		return true
	})
	if decls != 1 {
		t.Fatalf("harness parsed %d func decls, want 1 — the snippet fixtures are not "+
			"reaching the detector, so both directions above prove nothing", decls)
	}
}

// The two STRUCTURAL gates over the lists themselves — TestFieldDebtBucketsArePartition
// and TestFieldDecisionAllowlistIsPerSite — are claims about what those lists
// cannot contain, and a gate never shown to REJECT anything is indistinguishable
// from one that accepts everything. Both had been checked only by hand-mutating
// the validation and watching them go red, which establishes the fact once and
// then deletes the proof along with the mutation. The fixtures below are that
// check in committed form: they drive the extracted validators over the exact
// inputs the real lists must never hold.
func TestBucketTagVocabularyIsClosed(t *testing.T) {
	t.Parallel()

	// All seven, spelled out here rather than read off the regexp: a fixture that
	// derives its expectation from the thing under test agrees with any mutation
	// of it.
	for _, bucket := range []string{
		"boundary", "escape", "contract", "dotted", "name-keyed", "translator", "harness",
	} {
		t.Run("accepts "+bucket, func(t *testing.T) {
			t.Parallel()
			why := bucket + ": a reason"
			got, ok := bucketTagOf(why)
			if !ok || got != bucket {
				t.Fatalf("bucketTagOf(%q) = (%q, %v), want (%q, true).\n\n"+
					"A legal bucket dropping out of the vocabulary does not fail loudly on its "+
					"own — every site it owns simply stops being counted, and the per-bucket "+
					"totals still sum to something plausible.", why, got, ok, bucket)
			}
		})
	}

	for _, tc := range []struct{ name, why string }{
		{"untagged reason", "covered-column set holds index-definition names"},
		{"out-of-vocabulary tag", "comparison: x"},
		{"legal word, no colon at all", "boundary the name crosses a layer"},
		{"colon without the space", "boundary:the name crosses a layer"},
		{"tag not at the head of the reason", "see boundary: the name crosses a layer"},
		{"empty reason", ""},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			t.Parallel()
			if got, ok := bucketTagOf(tc.why); ok {
				t.Fatalf("bucketTagOf(%q) accepted it as bucket %q.\n\n"+
					"The tag names the site's SINGLE owning bucket. Anything outside the closed "+
					"vocabulary is a category nobody agreed to, and the migration arithmetic "+
					"built on the resulting totals is fiction.", tc.why, got)
			}
		})
	}
}

// An untagged entry must be REPORTED and must not be counted anywhere — being
// silently dropped into a bucket would be the same defect the partition exists
// to prevent.
func TestBucketCountsSumsTaggedAndReportsUntagged(t *testing.T) {
	t.Parallel()

	counts, untagged := bucketCounts(map[string]fieldDebt{
		"a.go:1": {2, "escape: a line hosting two decisions"},
		"b.go:2": {1, "escape: one more in the same bucket"},
		"c.go:3": {3, "dotted: three on one line"},
		"d.go:4": {1, "comparison: not a bucket at all"},
	})

	if len(untagged) != 1 || !strings.Contains(untagged[0], "d.go:4") {
		t.Fatalf("bucketCounts reported untagged = %v, want exactly the d.go:4 entry", untagged)
	}
	if counts["escape"] != 3 {
		t.Errorf("escape = %d, want 3 — the bucket total is a sum of per-site COUNTS, not a "+
			"count of sites", counts["escape"])
	}
	if counts["dotted"] != 3 {
		t.Errorf("dotted = %d, want 3", counts["dotted"])
	}
	if got, ok := counts["comparison"]; ok {
		t.Errorf("an untagged entry was counted under %q (%d) — it must be reported, not filed",
			"comparison", got)
	}
	if len(counts) != 2 {
		t.Errorf("counts has %d buckets, want 2: %v", len(counts), counts)
	}
}

// The count comparison is the mechanism that makes the debt list a RATCHET
// rather than a set of "this line is known" suppressions, and over the real tree
// it is unexercised: every entry carries n == 1, so a mutation replacing the
// per-site increment with a bare assignment leaves every count identical and
// the suite green. Both directions of disagreement are driven here directly.
func TestDebtMismatchesChecksCountsInBothDirections(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		want map[string]fieldDebt
		seen map[string]int
		msg  string // substring the mismatch must name, "" for none
	}{
		{
			name: "exact match is clean",
			want: map[string]fieldDebt{"a.go:1": {1, "escape: x"}, "b.go:2": {3, "dotted: y"}},
			seen: map[string]int{"a.go:1": 1, "b.go:2": 3},
		},
		{
			// The direction a bare assignment hides: two violations recorded,
			// one still present. Someone deleted a violation and did not
			// decrement, or swapped one of them for a decision of another shape.
			name: "recorded 2, found 1",
			want: map[string]fieldDebt{"a.go:1": {2, "escape: x"}},
			seen: map[string]int{"a.go:1": 1},
			msg:  "hosts 1 decisions, entry says 2",
		},
		{
			// The other direction: a NEW violation landed on a line that already
			// had one. Presence-only would call this known and wave it through —
			// which is exactly a new offense entering the tree green.
			name: "recorded 1, found 2",
			want: map[string]fieldDebt{"a.go:1": {1, "escape: x"}},
			seen: map[string]int{"a.go:1": 2},
			msg:  "hosts 2 decisions, entry says 1",
		},
		{
			name: "recorded 1, found none — stale",
			want: map[string]fieldDebt{"a.go:1": {1, "escape: x"}},
			seen: map[string]int{},
			msg:  "a.go:1 (no decision found)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := debtMismatches(tc.want, tc.seen)
			if tc.msg == "" {
				if len(got) > 0 {
					t.Fatalf("a debt list matching the walk exactly was reported stale: %v", got)
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0], tc.msg) {
				t.Fatalf("debtMismatches = %v, want exactly one message containing %q.\n\n"+
					"A ratchet that only checks PRESENCE accepts any subset of a packed line's "+
					"violations, and accepts new ones landing on a line already listed.", got, tc.msg)
			}
		})
	}
}

// allowedFieldDecisions is empty, so its stale check runs zero times in the tree
// walk — enforcement that has never once executed. Same discipline, same
// fixtures, and here it is more load-bearing than for debt: a stale debt entry
// points at work still owed, a stale exemption claims work was never needed.
func TestAllowlistMismatchesChecksCountsInBothDirections(t *testing.T) {
	t.Parallel()

	const why = "the name is the identity at this layer"
	site := func(n int) []fieldDecisionSite {
		return []fieldDecisionSite{{site: "a.go:1", n: n, why: why}}
	}

	for _, tc := range []struct {
		name  string
		sites []fieldDecisionSite
		seen  map[string]int
		msg   string
	}{
		{"exact match is clean", site(2), map[string]int{"a.go:1": 2}, ""},
		{"exempted 2, found 1", site(2), map[string]int{"a.go:1": 1}, "allowlisted for 2 decisions, hosts 1"},
		{"exempted 1, found 2", site(1), map[string]int{"a.go:1": 2}, "allowlisted for 1 decisions, hosts 2"},
		{"exempted, found none", site(1), map[string]int{}, "allowlisted, but no decision found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := allowlistMismatches(tc.sites, tc.seen)
			if tc.msg == "" {
				if len(got) > 0 {
					t.Fatalf("an allowlist matching the walk exactly was reported stale: %v", got)
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0], tc.msg) {
				t.Fatalf("allowlistMismatches = %v, want exactly one message containing %q.\n\n"+
					"An exemption whose count drifted is an exemption nobody re-justified.", got, tc.msg)
			}
		})
	}
}

func TestAllowlistShapeValidatorRejectsNonSites(t *testing.T) {
	t.Parallel()

	const why = "the name is the identity at this layer"
	for _, tc := range []struct {
		name  string
		entry fieldDecisionSite
		valid bool
	}{
		{
			// The shape the per-site rule replaced. It would never MATCH a
			// reported site, so it grants nothing while reading like an
			// exemption — and the obvious "fix" is prefix matching, which
			// re-opens the file-wide hole.
			name:  "file-wide entry",
			entry: fieldDecisionSite{site: "pkg/relational/core/query/cascades_translator.go", n: 1, why: why},
		},
		{
			name:  "per-site entry",
			entry: fieldDecisionSite{site: "pkg/relational/core/query/cascades_translator.go:12", n: 1, why: why},
			valid: true,
		},
		{
			name:  "multi-decision per-site entry",
			entry: fieldDecisionSite{site: "pkg/x/file.go:4151", n: 3, why: why},
			valid: true,
		},
		{
			name:  "zero count",
			entry: fieldDecisionSite{site: "pkg/x/file.go:12", n: 0, why: why},
		},
		{
			name:  "negative count",
			entry: fieldDecisionSite{site: "pkg/x/file.go:12", n: -1, why: why},
		},
		{
			name:  "blank reason",
			entry: fieldDecisionSite{site: "pkg/x/file.go:12", n: 1, why: "   "},
		},
		{
			name:  "non-numeric line",
			entry: fieldDecisionSite{site: "pkg/x/file.go:top", n: 1, why: why},
		},
		{
			name:  "colon with no line after it",
			entry: fieldDecisionSite{site: "pkg/x/file.go:", n: 1, why: why},
		},
		{
			name:  "not a Go file",
			entry: fieldDecisionSite{site: "pkg/x/file.proto:12", n: 1, why: why},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bad := invalidAllowlistEntries([]fieldDecisionSite{tc.entry})
			if tc.valid && len(bad) > 0 {
				t.Fatalf("a well-formed per-site exemption was rejected: %v", bad)
			}
			if !tc.valid && len(bad) == 0 {
				t.Fatalf("%+v was accepted.\n\nAn allowlist entry that cannot match a reported "+
					"site is not an exemption, it is a comment; and one without a count or a "+
					"reason is an exemption nobody has to justify.", tc.entry)
			}
		})
	}
}

// debtFixture wraps a debt-literal body and optional trailing declarations into
// a parseable file, so the header reader can be exercised on the structure it
// actually reads rather than on a text fragment.
func debtFixture(literalBody, trailing string) []byte {
	return []byte("package p\n\n" +
		"// dotted (900) — the var's own DOC comment, which discusses buckets\n" +
		"var knownFieldDecisionDebt = map[string]fieldDebt{\n" +
		literalBody +
		"}\n" + trailing)
}

// The group headers are the only summary anyone reads off the debt list, and
// they are parsed out of a source file that also DISCUSSES buckets in prose —
// in the var's own doc comment, in other declarations, and inside function
// bodies.
//
// The first version discriminated by INDENT (`^\t`), which is not a scope: one
// tab is also the indent of a comment inside any function body, and the reader
// took the LAST match, so a later look-alike silently overrode the header the
// list actually advertises. Over the real file that was unfalsifiable — it
// happens to contain no line shaped like a stray header — so the gate agreed
// with a correct anchor and with no anchor at all.
//
// Scope is now the composite literal's span from the AST, and the FIRST header
// per bucket wins. These fixtures are what makes that checkable.
func TestBucketHeaderCountsReadHeadersNotProse(t *testing.T) {
	t.Parallel()

	src := debtFixture(
		"\t// boundary (0) — MIGRATED\n"+
			"\t// escape (0) -- MIGRATED, trailing prose\n"+
			"\t// contract (11)\n"+
			"\t// dotted (6)\n"+
			"\t// name-keyed (3)\n"+
			"\t// translator (17)\n"+
			"\t// harness (1)\n",
		"// dotted (999) — a top-level sentence recalling an old count\n"+
			"func f() {\n"+
			"\t// translator (998) — a one-tab comment inside a function body\n"+
			"}\n"+
			"//   dotted (997) — an indented doc-comment bullet\n")

	got, problems := bucketHeaderCounts(src)
	if len(problems) > 0 {
		t.Fatalf("clean fixture reported problems: %v", problems)
	}

	want := map[string]int{
		"boundary": 0, "escape": 0, "contract": 11, "dotted": 6,
		"name-keyed": 3, "translator": 17, "harness": 1,
	}
	for b, w := range want {
		if got[b] != w {
			t.Errorf("bucket %q read as %d, want %d — prose outside the literal won over the header",
				b, got[b], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("read %d buckets, want %d: %v", len(got), len(want), got)
	}
}

// TestBucketHeaderCountsRejectsTheStaleHeaderDecoy is the exact shape the
// previous reader passed: a STALE header inside the literal, plus a one-tab
// comment in a FUNCTION BODY carrying the number the tally actually has.
//
// Under `^\t` + last-wins the function-body line was read as the header and the
// gate reported 6 — agreeing with the live tally and reporting nothing, while
// the list on screen advertised 7. That is the exact failure mode the header
// check exists to prevent, arriving through the check itself.
func TestBucketHeaderCountsRejectsTheStaleHeaderDecoy(t *testing.T) {
	t.Parallel()

	src := debtFixture(
		"\t// dotted (7)\n"+
			"\t\"a/b.go:1\": {1, \"dotted: x\"},\n",
		"func helper() {\n"+
			"\t// dotted (6)\n"+
			"\t_ = 0\n"+
			"}\n")

	got, problems := bucketHeaderCounts(src)
	if len(problems) > 0 {
		t.Fatalf("the decoy is not a duplicate-header case; problems = %v", problems)
	}
	if got["dotted"] != 7 {
		t.Fatalf("dotted read as %d, want 7 — the STALE header inside the literal is what "+
			"the list advertises; a one-tab comment in a function body is not a header, "+
			"and letting it win is how a stale count checks out green", got["dotted"])
	}
	// And the whole point: read as 7, it disagrees with a live tally of 6.
	if bad := bucketHeaderMismatches(got, map[string]int{"dotted": 6}); len(bad) == 0 {
		t.Fatal("the stale header agreed with a tally of 6 — the decoy is not detected")
	}
}

// A bucket headed twice is not a tiebreak to resolve quietly: the list is then
// advertising two totals, and whichever the reader picks, half the readers of
// the file see the other one.
func TestBucketHeaderCountsReportsDuplicateHeaders(t *testing.T) {
	t.Parallel()

	src := debtFixture(
		"\t// dotted (7)\n"+
			"\t\"a/b.go:1\": {1, \"dotted: x\"},\n"+
			"\t// dotted (6)\n"+
			"\t\"a/b.go:2\": {1, \"dotted: y\"},\n", "")

	got, problems := bucketHeaderCounts(src)
	if len(problems) != 1 || !strings.Contains(problems[0], "headed twice") {
		t.Fatalf("duplicate header reported as %v, want one \"headed twice\" problem", problems)
	}
	if got["dotted"] != 7 {
		t.Fatalf("dotted read as %d, want the FIRST header's 7 — last-wins is what let a "+
			"stale header be overridden by a later look-alike", got["dotted"])
	}
}

// A file that does not parse, or one with no debt literal, must report a
// problem rather than an empty-and-therefore-agreeable reading.
func TestBucketHeaderCountsRequiresTheLiteral(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		src  string
		want string
	}{
		{"no literal", "package p\n\n// dotted (6)\nfunc f() {}\n", "not found"},
		{"unparseable", "package p\n\nvar x = (\n", "does not parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, problems := bucketHeaderCounts([]byte(tc.src))
			if len(got) != 0 {
				t.Fatalf("counts = %v, want none", got)
			}
			if len(problems) != 1 || !strings.Contains(problems[0], tc.want) {
				t.Fatalf("problems = %v, want one mentioning %q", problems, tc.want)
			}
		})
	}
}

// Both directions of header/tally disagreement, and the missing header. A
// bucket at zero still has to advertise itself: dropping its header when it
// empties is how a bucket stops being visible at exactly the moment its count
// becomes a claim worth making.
func TestBucketHeaderMismatchesReportsBothDirectionsAndAbsence(t *testing.T) {
	t.Parallel()

	full := map[string]int{
		"boundary": 0, "escape": 0, "contract": 11, "dotted": 6,
		"name-keyed": 3, "translator": 17, "harness": 1,
	}
	clone := func(edit func(map[string]int)) map[string]int {
		m := map[string]int{}
		for k, v := range full {
			m[k] = v
		}
		edit(m)
		return m
	}

	if bad := bucketHeaderMismatches(full, full); len(bad) > 0 {
		t.Fatalf("agreeing header and tally reported %v", bad)
	}

	for _, tc := range []struct {
		name           string
		header, live   map[string]int
		wantSubstrings []string
	}{
		{
			name:           "header overstates — hides a migrated site",
			header:         full,
			live:           clone(func(m map[string]int) { m["dotted"] = 5 }),
			wantSubstrings: []string{`"dotted"`, "header says 6", "tally 5"},
		},
		{
			name:           "header understates — hides a site that arrived",
			header:         full,
			live:           clone(func(m map[string]int) { m["translator"] = 18 }),
			wantSubstrings: []string{`"translator"`, "header says 17", "tally 18"},
		},
		{
			name:           "an emptied bucket drops its header",
			header:         clone(func(m map[string]int) { delete(m, "boundary") }),
			live:           full,
			wantSubstrings: []string{`"boundary"`, "no `// boundary (N)` group header"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bad := bucketHeaderMismatches(tc.header, tc.live)
			if len(bad) != 1 {
				t.Fatalf("got %d messages, want exactly 1: %v", len(bad), bad)
			}
			for _, want := range tc.wantSubstrings {
				if !strings.Contains(bad[0], want) {
					t.Errorf("message %q does not name %q", bad[0], want)
				}
			}
		})
	}
}

// TestFieldDecisionsInsideClosuresAreReported is the committed form of the
// closure measurement the fixture above cites.
//
// The engine's name matching lives in `values.Replace` callbacks and pull-up
// closures, so a walk that stopped at a FuncLit — or a taint pass scoped to the
// closure rather than the declaration — would go blind to most of the surface
// the gate exists to watch, while still reporting enough sites elsewhere to
// look healthy. Nothing detected that: the ratchet is a set of file:line
// entries, and a walk that stops finding some of them fails with "a debt entry
// no longer matches", which reads as normal line drift.
//
// So the property is asserted directly: engine decisions ARE reported from
// inside closures, and every one of them is a known debt site. The floor is a
// floor rather than an exact count because sites move; what it cannot survive
// is the walk ceasing to descend.
func TestFieldDecisionsInsideClosuresAreReported(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)
	var engineSites []string
	for _, rel := range trackedGoFiles(t, root) {
		if strings.HasSuffix(rel, "_test.go") {
			continue // the gate's subject is the engine, not its tests
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			continue // the tree walk skips unparseable files too
		}
		type span struct{ lo, hi token.Pos }
		var lits []span
		ast.Inspect(f, func(n ast.Node) bool {
			if fl, isLit := n.(*ast.FuncLit); isLit {
				lits = append(lits, span{fl.Pos(), fl.End()})
			}
			return true
		})
		scanFieldDecisions(f, func(pos token.Pos, _ string) {
			for _, s := range lits {
				if pos >= s.lo && pos <= s.hi {
					engineSites = append(engineSites,
						fmt.Sprintf("%s:%d", rel, fset.Position(pos).Line))
					return
				}
			}
		})
	}
	sort.Strings(engineSites)
	t.Logf("engine decisions reported from inside a closure: %d\n  %s",
		len(engineSites), strings.Join(engineSites, "\n  "))

	// The floor: this many engine sites are only visible because the walk
	// descends into FuncLits and keys the taint on the DECLARATION.
	const closureSiteFloor = 10
	if len(engineSites) < closureSiteFloor {
		t.Fatalf("only %d engine decisions reported from inside a closure, want at least %d "+
			"— the walk has stopped descending into FuncLits (or the taint has been "+
			"re-scoped to the closure), and the Replace/pull-up callbacks where the "+
			"engine's name matching actually lives are now invisible to this gate:\n  %s",
			len(engineSites), closureSiteFloor, strings.Join(engineSites, "\n  "))
	}
	// And each is accounted for. A closure site missing from the ratchet would
	// mean the main walk reports it as a fresh offense, but this states the
	// invariant where the closure question is being asked.
	for _, site := range engineSites {
		if _, known := knownFieldDecisionDebt[site]; known {
			continue
		}
		if _, allowed := fieldDecisionAllowed(allowedFieldDecisions, site); allowed {
			continue
		}
		t.Errorf("closure site %s is in neither the debt list nor the allowlist", site)
	}
}

// A struct field is not a map key, and go/parser cannot tell them apart.
//
// The composite-literal check used to fire on any KeyValueExpr whose key
// "decides", and it reached that verdict through the taint set — which is keyed
// on the parser's *ast.Object precisely so a spelling collision cannot make one
// variable answer for another. In a STRUCT literal the collision happens one
// level up, in what the key MEANS: go/parser has no types, so it resolves the
// bare field identifier in `extraSortCol{name: name}` to whatever declaration is
// in scope with that spelling — here the local holding the display name. The
// object matches, the taint fires, and a struct field that never touched a
// column got reported as a name-keyed decision.
//
// Both directions are pinned because narrowing a gate is how recall dies
// quietly: the map literal MUST still be reported, and it is the shape the check
// was written for.
//
// Found while measuring a widening (treating the RFC-197 naming authorities as
// name PRODUCERS, so a decision on `AggregateKeyColumnName(k)` is visible): that
// widening reports `extraSortCol{name: name}` at cascades_translator.go, and the
// report is false. Nothing in the live tree is affected today — no listed debt
// entry is a struct literal — so this fixture is the only thing that can hold
// the distinction.
func TestFieldDecisionCompositeLiteralKeysAreMapKeysNotStructFields(t *testing.T) {
	t.Parallel()

	reportsKey := func(t *testing.T, body string) bool {
		t.Helper()
		for _, form := range scanSnippet(t, body) {
			if strings.Contains(form, "composite-literal key") {
				return true
			}
		}
		return false
	}

	// RECALL: a MAP literal keyed by the display name is the conflation.
	if !reportsKey(t, `func f(fv *values.FieldValue) map[string]int {
	return map[string]int{fv.Field: 1}
}`) {
		t.Error("a map literal keyed by fv.Field is no longer reported — this is the shape " +
			"the composite-literal check exists for, and without it `map[string]T{fv.Field: v}` " +
			"builds the same same-leaf-name conflation an IndexExpr would, with nothing to catch it")
	}

	// RECALL through the taint set, which is how the real sites arrive.
	if !reportsKey(t, `func f(fv *values.FieldValue) map[string]int {
	name := fv.Field
	return map[string]int{name: 1}
}`) {
		t.Error("a map literal keyed by a name-derived LOCAL is no longer reported")
	}

	// PRECISION: a STRUCT literal whose field is spelled like the local.
	if reportsKey(t, `type extraSortCol struct {
	name string
	val  int
}

func f(fv *values.FieldValue) extraSortCol {
	name := fv.Field
	return extraSortCol{name: name, val: 1}
}`) {
		t.Error("a STRUCT literal's field name is reported as a name-keyed decision.\n\n" +
			"`extraSortCol{name: name}` keys nothing — the left `name` is a field, and it " +
			"resolves to the local only because go/parser has no types and the two share a " +
			"spelling. A debt list whose entries are not all true is one nobody reads, and " +
			"this entry would be unfixable by construction: there is no Resolved accessor to " +
			"consult for a struct field name.")
	}

	// PRECISION: an ARRAY literal's integer indices are not keys either.
	if reportsKey(t, `func f(fv *values.FieldValue) [4]string {
	name := fv.Field
	return [4]string{2: name}
}`) {
		t.Error("an array literal's integer index is reported as a name-keyed decision")
	}

	// The deliberate over-approximation, pinned so it is a choice and not a
	// surprise: an element literal with no Type of its own could be a map
	// element, so the check is kept there.
	if !reportsKey(t, `type row struct{ name string }

func f(fv *values.FieldValue) map[string]row {
	name := fv.Field
	return map[string]row{"a": {name: name}}
}`) {
		t.Error("an UNTYPED nested composite literal stopped being checked — the elided " +
			"element type is exactly where a map element still appears, so erring toward " +
			"reporting there is the deliberate side to err on")
	}
}

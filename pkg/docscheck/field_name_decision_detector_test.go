package docscheck

import (
	"go/ast"
	"go/parser"
	"go/token"
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
	src := "package p\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n\t\"slices\"\n)\n\nvar _, _, _ = fmt.Sprintf, strings.Contains, slices.Contains\n\n" + body
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse snippet: %v\n---\n%s", err, src)
	}
	var got []string
	scanFieldDecisions(f, func(_ token.Pos, form string) {
		got = append(got, form)
	})
	return got
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

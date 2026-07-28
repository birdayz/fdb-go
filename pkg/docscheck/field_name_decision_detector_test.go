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
			// cascades_generator.go:3169, and its sibling one arm below —
			// identical lookup, no leg prefix — was recorded as debt while this
			// one passed silently.
			name: "map key laundered through concatenation",
			want: "map key",
			body: `func f(inner map[string]int, fv *values.FieldValue, legPrefix string) int {
	return inner[legPrefix+strings.ToUpper(fv.Field)]
}`,
		},
		{
			// Slicing the flat "ALIAS.col" name to get the qualifier is the same
			// blind spot through a SliceExpr — cascades_translator.go:5765 and
			// exists_gathered_cluster_wrap.go:131.
			name: "map key laundered through a slice of the name",
			want: "map key",
			body: `func f(layouts map[string]int, fv *values.FieldValue, dot int) int {
	return layouts[strings.ToUpper(fv.Field[:dot])]
}`,
		},
		{
			// The comparison form of the same slice — cascades_translator.go:5726.
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
			// aggregateOperandColumn (cascades_translator.go:7392) escaped the
			// name in plain sight. Below a launderer the argument is already a
			// string, so there is no constructor to confuse and the search goes
			// to any depth.
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
// per-site increment with a bare assignment leaves all 46 counts identical and
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

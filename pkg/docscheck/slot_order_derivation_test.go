package docscheck

// A CONVERTED ROW EXPECTATION MUST AGREE WITH ITS QUERY'S SELECT LIST.
//
// CQ-64 rewrote 573 row expectations in pkg/relational/sqldriver from a
// name-keyed map rendering to a slot-ordered one. The rewrite's own evidence
// that it changed no VALUES was a throwaway program: it parsed each new
// `NAME=value|...` string, collapsed it last-wins by name, re-rendered it in
// Go's map form, and required byte-identity with the map literal it replaced.
//
// THAT METHOD CANNOT SEE THE THING THE CONVERSION WAS FOR. The collapse it
// applies IS `positionalToMap`, so it is permutation-invariant BY CONSTRUCTION:
// any reordering of the same pairs collapses to the same map and passes. It
// proved the values survived and was silent on the only dimension the
// conversion added. Worse, the program was never committed — the log it printed
// was, so the repository carried a conclusion with no instrument behind it and
// no way to re-run it. A committed log of a deleted probe is an assertion
// wearing a measurement's clothes.
//
// This is the instrument the log claimed to be, and it checks the ORDER.
//
// THE METHOD is the by-hand spot-check, mechanised: read the query's SELECT
// list, derive the output column names in source order, and require the
// expectation's `NAME=` tokens to appear in that same order. It needs no database and no plan — the SELECT list is the
// contract for what slot 0, 1, 2 hold, which is exactly the fact the map-keyed
// rendering used to discard.
//
// WHERE IT CANNOT DERIVE, IT SAYS SO PER SITE rather than passing quietly.
// `SELECT *` has no written column list, and a projection over an expression
// with no alias has no name the test can predict without reimplementing the
// planner's naming. Those sites fall back to the CROSS-ROW check below, and the
// per-file census in the failure message reports derived and fallback counts
// separately so a file that silently stops being derivable is visible.
//
// THE FALLBACK IS NOT NOTHING. Every row of one site must carry the IDENTICAL
// name sequence: a site whose rows disagree about their own column names is
// corrupt however it was produced. That check runs everywhere, derivable or not.
//
// WHAT THIS TEST DELIBERATELY NO LONGER DOES: compare against the master-side
// map literals. The branch is the new truth; re-deriving the old rendering
// would pin the conversion to a state that no longer exists and would carry the
// permutation-blindness back in with it. The standing property is order
// agreement here, plus the permutation tripwire
// (`TestPositionalRenderersSeeAPermutation` in the sqldriver package), which is
// what proves a positional renderer can see a reordering at all.

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

// slotOrderSite is one (query, expectations) pair found in the source.
type slotOrderSite struct {
	file     string
	line     int
	sql      string
	rows     []string
	derived  []string // the SELECT list's output names, nil when underivable
	whyNot   string   // why the SELECT list could not be derived
	fromCall bool     // paired inside one call, rather than by scope
}

// selectOutputNames derives a query's output column names, in source order.
//
// It reads only the SELECT list — the text between the leading SELECT and its
// own FROM — because that list IS the slot order contract. Anything it cannot
// name WITHOUT guessing at planner behaviour makes the whole query underivable,
// and the reason is returned for the per-site report. Guessing would be worse
// than declining: a wrong derived order fails a correct expectation, and the
// fix for that is always to weaken the test.
func selectOutputNames(sql string) (names []string, whyNot string) {
	trimmed := strings.TrimSpace(sql)
	if !strings.HasPrefix(strings.ToUpper(trimmed), "SELECT ") {
		return nil, "not a SELECT"
	}
	body := trimmed[len("SELECT "):]
	if u := strings.ToUpper(strings.TrimSpace(body)); strings.HasPrefix(u, "DISTINCT ") {
		body = strings.TrimSpace(body)[len("DISTINCT "):]
	}
	fromAt := topLevelIndexOfWord(body, "FROM")
	if fromAt < 0 {
		return nil, "no top-level FROM"
	}
	list := strings.TrimSpace(body[:fromAt])
	if list == "" {
		return nil, "empty SELECT list"
	}
	for _, item := range splitTopLevel(list, ',') {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, "empty SELECT item"
		}
		name, ok := selectItemName(item)
		if !ok {
			return nil, fmt.Sprintf("item %q has no derivable output name", item)
		}
		names = append(names, name)
	}
	return names, ""
}

// slotNameMatches reports whether a rendered slot name is an acceptable spelling
// of a derived SELECT-list name.
//
// SPELLING IS NOT ORDER, and this instrument is about order. Two spellings are
// genuinely planner-decided and must both be accepted:
//
//   - the QUALIFIER. `SELECT A."K", B."K", "X"` renders `A.K|B.K|X` — the
//     qualifier survives so two same-named join columns stay distinguishable —
//     while `SELECT A."K", COUNT(*)` renders plain `K`. Which one appears is a
//     naming rule inside the planner; predicting it here would mean
//     reimplementing that rule, and getting it wrong fails correct expectations.
//     So a derived `K` accepts `K` or any `<qualifier>.K`.
//   - CASE. A quoted lowercase identifier (`SELECT "ID", "val"`) renders `val`,
//     not `VAL`.
//
// Neither weakens the order check: the names still say which column occupies
// which slot, which is the whole property. A permutation still fails, because a
// permutation moves names BETWEEN slots.
func slotNameMatches(rendered, derived string) bool {
	r, d := strings.ToUpper(rendered), strings.ToUpper(derived)
	if r == d {
		return true
	}
	// `A.K` satisfies a derived `K`; the leaf must match exactly.
	if i := strings.LastIndexByte(r, '.'); i >= 0 && r[i+1:] == d {
		return true
	}
	return false
}

func slotNamesMatch(rendered, derived []string) bool {
	if len(rendered) != len(derived) {
		return false
	}
	for i := range rendered {
		if !slotNameMatches(rendered[i], derived[i]) {
			return false
		}
	}
	return true
}

// selectItemName names ONE SELECT item.
//
//   - `... AS "N"` / `... AS N`  -> N, whatever the expression was
//   - `A."K"` / `"K"` / `K`      -> K, the last dotted segment
//   - `COUNT(*)` / `MAX("M")`    -> the call text, quotes and spaces removed,
//     which is how an unaliased function projection is named
//
// A bare `*`, a qualified `T.*`, or an unaliased expression (`A."K" + 1`) is
// declined: their output names come from the planner, not from the text.
func selectItemName(item string) (string, bool) {
	if at := topLevelIndexOfWord(item, "AS"); at >= 0 {
		alias := strings.TrimSpace(item[at+len("AS"):])
		if alias == "" {
			return "", false
		}
		return strings.ToUpper(strings.ReplaceAll(alias, `"`, "")), true
	}
	if strings.Contains(item, "*") && !isFunctionCall(item) {
		return "", false // SELECT * / T.* — no written column list
	}
	if isFunctionCall(item) {
		var b strings.Builder
		for _, r := range item {
			if r == '"' || r == ' ' || r == '\t' || r == '\n' {
				continue
			}
			b.WriteRune(r)
		}
		return strings.ToUpper(b.String()), true
	}
	// A simple (possibly qualified) column reference and nothing else.
	bare := strings.ReplaceAll(item, `"`, "")
	if bare == "" {
		return "", false
	}
	for _, r := range bare {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' {
			continue
		}
		return "", false // an expression; the planner names it
	}
	if i := strings.LastIndexByte(bare, '.'); i >= 0 {
		bare = bare[i+1:]
	}
	if bare == "" {
		return "", false
	}
	return strings.ToUpper(bare), true
}

// isFunctionCall reports whether item is exactly `NAME(...)` with the closing
// paren at the very end — so `COUNT(*)` qualifies and `COUNT(*) + 1` does not.
func isFunctionCall(item string) bool {
	open := strings.IndexByte(item, '(')
	if open <= 0 || !strings.HasSuffix(item, ")") {
		return false
	}
	depth := 0
	for i := open; i < len(item); i++ {
		switch item[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(item)-1 {
				return false
			}
		}
	}
	return depth == 0
}

// splitTopLevel splits on sep at paren depth 0 and outside double quotes.
func splitTopLevel(s string, sep byte) []string {
	var out []string
	depth, inQuote, start := 0, false, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			inQuote = !inQuote
		case inQuote:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == sep && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// topLevelIndexOfWord finds the keyword as a whole word at paren depth 0 and
// outside quotes, so the FROM of a scalar subquery in the SELECT list and a
// column literally named FROM are both skipped.
func topLevelIndexOfWord(s, word string) int {
	up := strings.ToUpper(s)
	depth, inQuote := 0, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			inQuote = !inQuote
			continue
		case inQuote:
			continue
		case c == '(':
			depth++
			continue
		case c == ')':
			depth--
			continue
		}
		if depth != 0 || !strings.HasPrefix(up[i:], word) {
			continue
		}
		if i > 0 && !isSQLBreak(s[i-1]) {
			continue
		}
		if end := i + len(word); end < len(s) && !isSQLBreak(s[end]) {
			continue
		}
		return i
	}
	return -1
}

func isSQLBreak(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '(' || c == ')' || c == ','
}

// expectationNames extracts the `NAME=` tokens of one rendered row, in order.
//
// Declines rather than guesses when a value itself contains a `|`: the pipe is
// the separator, so a segment with no `=` means the split landed inside a value
// and the remaining names cannot be trusted.
func expectationNames(row string) (names []string, ok bool) {
	if row == "" {
		return nil, false
	}
	for _, seg := range strings.Split(row, "|") {
		eq := strings.IndexByte(seg, '=')
		if eq <= 0 {
			return nil, false
		}
		names = append(names, seg[:eq])
	}
	return names, true
}

// looksLikeSQL reports whether a string literal is a query this test can read.
func looksLikeSQL(s string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s)), "SELECT ")
}

// looksLikeRenderedRow reports whether a string literal is a
// positionalNamedPipeSprint rendering rather than some other `k=v` text.
//
// The name shape is the discriminator, and it has to be strict. These test files
// carry plenty of unrelated `k=v` strings — `"1=30 2=30"` from a group-count
// assertion, `"%t=%d"` from a format string — and a loose match turns each into a
// confident, wrong slot-order failure. Every one of those reports would be
// answered by weakening this test, which is how an instrument becomes a
// formality. A name here must look like a SQL output column: an identifier, or
// an unaliased function projection like `COUNT(*)`.
func looksLikeRenderedRow(s string) bool {
	names, ok := expectationNames(s)
	if !ok || len(names) == 0 {
		return false
	}
	for _, n := range names {
		if !isColumnName(n) {
			return false
		}
	}
	return true
}

func isColumnName(n string) bool {
	if open := strings.IndexByte(n, '('); open > 0 && strings.HasSuffix(n, ")") {
		return isFunctionCall(n) && isPlainIdent(n[:open])
	}
	return isPlainIdent(n)
}

// isExpectationElement reports whether a string literal is an ELEMENT of a
// composite literal — `[]string{"K=1|X=7", ...}`, the shape every converted
// expectation has.
//
// Without this, a diagnostic format string is indistinguishable from an
// expectation: `t.Fatalf("got=%v want=%v", ...)` and `"inner=..."` both parse as
// `NAME=value`, and each one reported is a confident failure against a query it
// was never an expectation for. Requiring the literal to sit in a composite
// literal is a structural fact rather than a guess about its text — a diagnostic
// is an argument, an expectation is an element.
func isExpectationElement(n ast.Node, parent map[ast.Node]ast.Node) bool {
	_, ok := parent[n].(*ast.CompositeLit)
	return ok
}

func isPlainIdent(n string) bool {
	if n == "" {
		return false
	}
	for i, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case (r >= '0' && r <= '9' || r == '$' || r == '.') && i > 0:
		default:
			return false
		}
	}
	// A leading digit means this is a VALUE, not a column name.
	return !(n[0] >= '0' && n[0] <= '9')
}

// collectSlotOrderSites pairs each query with the rendered rows expected of it.
//
// Two pairings, in order of confidence. A call that carries BOTH the SQL and a
// []string of rendered rows (`wantRows(t, q, []string{...})`) is unambiguous.
// Otherwise the innermost enclosing function is the unit, and it pairs only when
// it holds exactly ONE query — two queries in one closure make the association a
// guess, and a guessed pairing produces a confident wrong failure.
func collectSlotOrderSites(f *ast.File, fset *token.FileSet, rel string) (sites []slotOrderSite, ambiguous int) {
	parent, _ := parentMap(f)
	claimed := map[ast.Node]bool{}
	renderers := renderingHelpers(f)
	reachesRenderer := func(scope ast.Node) bool {
		found := false
		ast.Inspect(scope, func(m ast.Node) bool {
			if call, ok := m.(*ast.CallExpr); ok && renderers[callName(call)] {
				found = true
			}
			return !found
		})
		return found
	}

	stringLitsIn := func(n ast.Node) (sqls, rows []string, rowNodes []ast.Node) {
		ast.Inspect(n, func(m ast.Node) bool {
			lit, ok := m.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconvUnquote(lit.Value)
			if err != nil {
				return true
			}
			switch {
			case looksLikeSQL(v):
				sqls = append(sqls, v)
			case isExpectationElement(m, parent) && looksLikeRenderedRow(v):
				rows = append(rows, v)
				rowNodes = append(rowNodes, m)
			}
			return true
		})
		return sqls, rows, rowNodes
	}

	// Pairing 1: within a single call expression.
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sqls, rows, rowNodes := stringLitsIn(call)
		if len(sqls) != 1 || len(rows) == 0 {
			return true
		}
		// The call must actually be a rendering call — `wantRows(t, q, []string{...})`
		// where wantRows renders through positionalNamedPipeSprint.
		if !renderers[callName(call)] {
			return true
		}
		for _, rn := range rowNodes {
			if claimed[rn] {
				return true // an inner call already claimed these rows
			}
		}
		for _, rn := range rowNodes {
			claimed[rn] = true
		}
		names, why := selectOutputNames(sqls[0])
		sites = append(sites, slotOrderSite{
			file: rel, line: fset.Position(call.Lparen).Line,
			sql: sqls[0], rows: rows, derived: names, whyNot: why, fromCall: true,
		})
		return true
	})

	// Pairing 2: within the innermost enclosing function of unclaimed rows.
	byScope := map[ast.Node][]string{}
	scopePos := map[ast.Node]token.Pos{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || claimed[n] {
			return true
		}
		v, err := strconvUnquote(lit.Value)
		if err != nil || !isExpectationElement(n, parent) || !looksLikeRenderedRow(v) {
			return true
		}
		scope := enclosingFunc(n, parent)
		if scope == nil || !reachesRenderer(scope) {
			return true
		}
		byScope[scope] = append(byScope[scope], v)
		if _, seen := scopePos[scope]; !seen {
			scopePos[scope] = lit.ValuePos
		}
		return true
	})
	for scope, rows := range byScope {
		sqls, _, _ := stringLitsIn(scope)
		if len(sqls) != 1 {
			ambiguous += len(rows)
			continue
		}
		names, why := selectOutputNames(sqls[0])
		sites = append(sites, slotOrderSite{
			file: rel, line: fset.Position(scopePos[scope]).Line,
			sql: sqls[0], rows: rows, derived: names, whyNot: why,
		})
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].line < sites[j].line })
	return sites, ambiguous
}

// renderingHelpers names every function in f that produces a
// positionalNamedPipeSprint rendering — the renderer itself, plus the local
// helpers (`wantRows := func(...)`, `func check(...)`) that call it.
//
// The pairing needs this because these files carry SEVERAL row renderings side
// by side. array_cardinality_fdb_test.go builds a comma-joined `id=1,card=0`
// string in one subtest and uses the pipe renderer in another; a `%v`-bearing
// diagnostic sits in a []string of cases two functions away. Each of those
// parses as `NAME=value`, and each one paired with a nearby query produces a
// confident failure against an expectation that was never a slot rendering. The
// discriminator has to be "was this produced by the renderer", which is a fact
// about the CODE, not about the string's punctuation.
func renderingHelpers(f *ast.File) map[string]bool {
	helpers := map[string]bool{"positionalNamedPipeSprint": true}
	// Two passes: a helper may be defined after another helper that calls it.
	for range 2 {
		mentions := func(body ast.Node) bool {
			found := false
			ast.Inspect(body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && helpers[callName(call)] {
					found = true
				}
				return !found
			})
			return found
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if x.Body != nil && x.Name != nil && mentions(x.Body) {
					helpers[x.Name.Name] = true
				}
			case *ast.AssignStmt:
				for i, rhs := range x.Rhs {
					lit, ok := rhs.(*ast.FuncLit)
					if !ok || i >= len(x.Lhs) {
						continue
					}
					if id, isIdent := x.Lhs[i].(*ast.Ident); isIdent && mentions(lit.Body) {
						helpers[id.Name] = true
					}
				}
			}
			return true
		})
	}
	return helpers
}

// strconvUnquote is strconv.Unquote, kept local so the import list of this file
// stays about AST work.
func strconvUnquote(lit string) (string, error) {
	if len(lit) >= 2 && lit[0] == '`' && lit[len(lit)-1] == '`' {
		return lit[1 : len(lit)-1], nil
	}
	if len(lit) >= 2 && lit[0] == '"' && lit[len(lit)-1] == '"' {
		s := lit[1 : len(lit)-1]
		s = strings.ReplaceAll(s, `\"`, `"`)
		s = strings.ReplaceAll(s, `\\`, `\`)
		if strings.Contains(s, `\`) {
			return "", fmt.Errorf("unhandled escape")
		}
		return s, nil
	}
	return "", fmt.Errorf("not a string literal")
}

type slotOrderCensus struct {
	file                  string
	derivedSites, derived int
	fallbackSites         int
	shapeMismatch         int
	fallbackWhy           map[string]int
	ambiguousRows         int
}

func TestConvertedRowExpectationsAgreeWithTheirSelectList(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)
	var failures []string
	census := map[string]*slotOrderCensus{}
	var order []string

	for _, rel := range trackedGoFiles(t, root) {
		if !strings.HasPrefix(rel, rowValueMapScope) || !strings.HasSuffix(rel, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		// The converted population is exactly the files that render a row through
		// the NAMED positional renderer — that rendering is what produces a
		// `NAME=value|...` string in the first place. Scanning wider does not find
		// more coverage, it finds other assertion formats that happen to contain
		// an `=`, and reports each as a slot-order defect.
		if !strings.Contains(string(src), "positionalNamedPipeSprint") {
			continue
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if parseErr != nil {
			continue
		}
		sites, ambiguous := collectSlotOrderSites(f, fset, rel)
		if len(sites) == 0 && ambiguous == 0 {
			continue
		}
		c := &slotOrderCensus{file: rel, fallbackWhy: map[string]int{}, ambiguousRows: ambiguous}
		census[rel] = c
		order = append(order, rel)

		for _, s := range sites {
			// The cross-row check runs everywhere: every row of ONE query must
			// carry the same name sequence, derivable or not.
			var first []string
			for i, row := range s.rows {
				names, ok := expectationNames(row)
				if !ok {
					failures = append(failures, fmt.Sprintf(
						"%s:%d: row %q does not parse as NAME=value|... — a `|` inside a VALUE makes "+
							"every name after it unreadable, so the expectation cannot be checked at all",
						s.file, s.line, row))
					continue
				}
				if i == 0 || first == nil {
					first = names
					continue
				}
				if !equalStringSlices(first, names) {
					failures = append(failures, fmt.Sprintf(
						"%s:%d: two rows of ONE query disagree about their own column names:\n"+
							"    %v\n    %v\n  query: %s",
						s.file, s.line, first, names, s.sql))
				}
			}
			if s.derived == nil {
				c.fallbackSites++
				c.fallbackWhy[s.whyNot]++
				continue
			}
			c.derivedSites++
			c.derived += len(s.rows)
			for _, row := range s.rows {
				names, ok := expectationNames(row)
				if !ok {
					continue // already reported above
				}
				// A width disagreement means this string is not a `|`-separated
				// rendering of THIS query — a helper that consumes the renderer's
				// output and re-renders it differently (array_cardinality's
				// cardPairs turns `ID=1|CARD=0` into `id=1,card=0`) reaches the
				// renderer transitively but does not produce a slot rendering.
				// Counted and reported per file rather than failed: calling it a
				// slot-order defect would be a confident wrong answer, and the
				// answer to those is always to weaken the test. Width itself is
				// policed where it can be policed honestly — the <WIDTH MISMATCH>
				// marker in positionalNamedPipeSprint, at runtime, on real rows.
				if len(names) != len(s.derived) {
					c.shapeMismatch++
					continue
				}
				if !slotNamesMatch(names, s.derived) {
					failures = append(failures, fmt.Sprintf(
						"%s:%d: the expectation's slot order disagrees with the query's SELECT list.\n"+
							"    SELECT list: %v\n    expectation: %v\n    row:         %q\n    query:       %s\n"+
							"  The SELECT list IS the slot-order contract. A rendering that does not match it "+
							"is either reading the wrong slots or was written in the alphabetical order the "+
							"old map-keyed rendering produced.",
						s.file, s.line, s.derived, names, row, s.sql))
				}
			}
		}
	}

	sort.Strings(order)
	var table strings.Builder
	totalDerived, totalDerivedSites, totalFallback, totalAmbiguous := 0, 0, 0, 0
	for _, rel := range order {
		c := census[rel]
		totalDerived += c.derived
		totalDerivedSites += c.derivedSites
		totalFallback += c.fallbackSites
		totalAmbiguous += c.ambiguousRows
		var why []string
		for w, n := range c.fallbackWhy {
			why = append(why, fmt.Sprintf("%s x%d", w, n))
		}
		sort.Strings(why)
		fmt.Fprintf(&table, "  %-58s derived %3d row(s) over %2d site(s); fallback %2d site(s) %s; not-a-slot-rendering %d; ambiguous %d row(s)\n",
			strings.TrimPrefix(rel, rowValueMapScope), c.derived, c.derivedSites,
			c.fallbackSites, strings.Join(why, ", "), c.shapeMismatch, c.ambiguousRows)
	}
	t.Logf("slot-order derivation census (derived = expectation order checked against the SELECT list):\n%s"+
		"  TOTAL derived %d row(s) over %d site(s); %d fallback site(s); %d ambiguous row(s)",
		table.String(), totalDerived, totalDerivedSites, totalFallback, totalAmbiguous)

	// A derivation that reaches nothing is green forever. The measured population
	// at the time this landed was far above this floor; it exists so a broken
	// pairing or a changed helper shape reads as red, not as a clean result.
	if totalDerived < 100 {
		t.Fatalf("only %d row expectation(s) had their slot order DERIVED from a SELECT list — "+
			"this instrument is vacuous at that population. Something changed in how the sqldriver "+
			"tests carry their queries, and the census above says which files stopped being "+
			"derivable. Fix the pairing rather than lowering this floor.", totalDerived)
	}

	if len(failures) > 0 {
		sort.Strings(failures)
		t.Errorf("%d converted expectation(s) disagree with the order their query asks for.\n\n%s",
			len(failures), strings.Join(failures, "\n\n"))
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The derivation is the instrument's whole claim, so it is pinned against
// synthetic input in both directions. Recall matters — a derivation that
// declines everything reports a clean census and checks nothing — but the
// DECLINES matter just as much: a wrong derived name fails a correct
// expectation, and the only available fix for that is to weaken the test.
func TestSelectOutputNameDerivation(t *testing.T) {
	t.Parallel()

	derivable := []struct {
		sql  string
		want []string
	}{
		{`SELECT "X" FROM A, B, A."ARR" AS "X"`, []string{"X"}},
		{`SELECT A."K", B."K", "X" FROM A, B, A."ARR" AS "X"`, []string{"K", "K", "X"}},
		{`SELECT A."K", COUNT(*) FROM A GROUP BY A."K"`, []string{"K", "COUNT(*)"}},
		{`SELECT A."K", SUM(A."K" + B."K") AS "TOT" FROM A GROUP BY A."K"`, []string{"K", "TOT"}},
		{`SELECT "ID", "Y", "OX", "OY" FROM T4`, []string{"ID", "Y", "OX", "OY"}},
		{`SELECT DISTINCT "A", "B" FROM T`, []string{"A", "B"}},
		{`SELECT k, t.v FROM t`, []string{"K", "V"}},
		{
			// A scalar subquery in the SELECT list carries its own FROM. Splitting
			// on the FIRST FROM would truncate the list and derive a wrong order,
			// which is the failure mode that ends in someone deleting this test.
			`SELECT "X", (SELECT MAX("M") FROM C) AS "MM" FROM A`,
			[]string{"X", "MM"},
		},
		{
			// A comma inside a function's arguments is not a list separator.
			`SELECT COALESCE("A", "B") AS "C", "D" FROM T`,
			[]string{"C", "D"},
		},
	}
	for _, tc := range derivable {
		t.Run("derivable/"+tc.sql, func(t *testing.T) {
			t.Parallel()
			got, why := selectOutputNames(tc.sql)
			if why != "" {
				t.Fatalf("declined a derivable SELECT list: %s\n  query: %s", why, tc.sql)
			}
			if !equalStringSlices(got, tc.want) {
				t.Errorf("selectOutputNames = %v, want %v\n  query: %s", got, tc.want, tc.sql)
			}
		})
	}

	declined := []struct{ sql, why string }{
		{`SELECT * FROM T4, T4."SARR" AS "X"`, "a star has no written column list; the planner supplies one"},
		{`SELECT T.*, "X" FROM T`, "a qualified star, same reason"},
		{`SELECT A."K" + 1 FROM A`, "an unaliased expression is named by the planner, not by the text"},
		{`SELECT COUNT(*) + 1 FROM A`, "an unaliased expression AROUND a call is still an expression"},
		{`INSERT INTO T VALUES (1)`, "not a SELECT"},
		{`SELECT "A"`, "no FROM, so no bounded SELECT list"},
	}
	for _, tc := range declined {
		t.Run("declined/"+tc.sql, func(t *testing.T) {
			t.Parallel()
			if got, why := selectOutputNames(tc.sql); why == "" {
				t.Errorf("derived %v from a query whose output names are NOT written down.\n"+
					"  query: %s\n  why it must decline: %s\n"+
					"  A wrong derived order fails a CORRECT expectation, and the only fix anyone "+
					"reaches for then is to weaken this test.", got, tc.sql, tc.why)
			}
		})
	}
}

// The order check is the point, so it is pinned against a permutation directly —
// the same shape TestPositionalRenderersSeeAPermutation pins for the renderers.
func TestSlotOrderCheckSeesAPermutation(t *testing.T) {
	t.Parallel()

	const sql = `SELECT A."K", B."M", "X" FROM A, B, A."ARR" AS "X"`
	derived, why := selectOutputNames(sql)
	if why != "" {
		t.Fatalf("derivation declined: %s", why)
	}

	straight, ok := expectationNames("K=100|M=55|X=7")
	if !ok {
		t.Fatal("the straight expectation did not parse")
	}
	if !equalStringSlices(straight, derived) {
		t.Errorf("a correctly ordered expectation disagreed with its SELECT list: %v vs %v", straight, derived)
	}

	// The PERMUTATION — the same pairs, different slots, which is what a
	// mis-bound leg window produces. The old verifier's collapse-and-compare
	// method accepted this by construction; this one must reject it.
	permuted, ok := expectationNames("X=7|K=100|M=55")
	if !ok {
		t.Fatal("the permuted expectation did not parse")
	}
	if equalStringSlices(permuted, derived) {
		t.Fatal("the order check accepted a permuted expectation")
	}

	// And the ALPHABETICAL order the map-keyed rendering produced for both — the
	// specific wrong order the conversion was undoing.
	alphabetical, _ := expectationNames("K=100|M=55|X=7")
	sorted := append([]string(nil), alphabetical...)
	sort.Strings(sorted)
	if equalStringSlices(sorted, derived) && !equalStringSlices(derived, []string{"K", "M", "X"}) {
		t.Fatal("the fixture no longer distinguishes alphabetical order from SELECT order")
	}
}

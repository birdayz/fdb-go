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
// This instrument checks label order, not value ownership. Moving NAME=value
// pairs is detectable when their labels differ; swapping values while retaining
// the labels is not, including for anonymous _N labels. Real-FDB assertions remain
// the independent value oracle.
//
// THE METHOD is the by-hand spot-check, mechanised: read the query's SELECT
// list, derive the output column names in source order, and require the
// expectation's `NAME=` tokens to appear in that same order. It needs no database and no plan — the SELECT list is the
// contract for what slot 0, 1, 2 hold, which is exactly the fact the map-keyed
// rendering used to discard.
//
// WHERE IT CANNOT DERIVE, IT SAYS SO PER SITE rather than passing quietly.
// Stars, WITH/set/parenthesized bodies, multiple/non-SELECT statements, no-FROM
// queries and unnamed computations other than bare aggregate calls are declined.
// Typed SELECT elements supply explicit-AS names, inherited column identifiers
// and anonymous aggregate labels from absolute SELECT ordinals. Declined sites
// fall back to the CROSS-ROW check below, and the
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

	sqlparser "fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// slotOrderSite is one (query, expectations) pair found in the source.
type slotOrderSite struct {
	file      string
	line      int
	sql       string
	rows      []string
	derived   []string // the SELECT list's output names, nil when underivable
	nameKinds []selectNameKind
	whyNot    string // why the SELECT list could not be derived
	fromCall  bool   // paired inside one call, rather than by scope
}

type selectNameKind uint8

const (
	selectNameInherited selectNameKind = iota
	selectNameExplicit
	selectNameAnonymousAggregate
)

// selectOutputNames is the label-only view used by the independent gate tests.
func selectOutputNames(sql string) ([]string, string) {
	names, _, why := selectOutputNamesWithProvenance(sql)
	return names, why
}

// selectOutputNamesWithProvenance does not resolve catalog metadata or values.
// It declines stars, WITH/set/parenthesized bodies, multiple/non-SELECT statements,
// no-FROM queries and unnamed computations other than a bare aggregate call.
// Supported labels come only from typed SELECT elements: an explicit AS alias,
// an inherited column identifier, or the absolute ordinal of an anonymous
// aggregate. A parsed trailing uid without AS does not grant a SQL name.
func selectOutputNamesWithProvenance(sql string) (names []string, kinds []selectNameKind, why string) {
	root, err := sqlparser.Parse(sql)
	if err != nil {
		return nil, nil, "SQL parse error"
	}
	if root.Statements() == nil || len(root.Statements().AllStatement()) != 1 {
		return nil, nil, "not exactly one statement"
	}
	statement := root.Statements().Statement(0).SelectStatement()
	if statement == nil {
		return nil, nil, "not a SELECT"
	}
	query := statement.Query()
	if query.Ctes() != nil {
		return nil, nil, "WITH body requires semantic publication"
	}
	body, ok := query.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return nil, nil, "set body requires semantic publication"
	}
	table, ok := body.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok || table.FromClause() == nil {
		return nil, nil, "not a simple SELECT with FROM"
	}
	for i, item := range table.SelectElements().AllSelectElement() {
		selected, ok := item.(*antlrgen.SelectExpressionElementContext)
		if !ok {
			return nil, nil, "star requires semantic publication"
		}
		if selected.AS() != nil && selected.Uid() != nil {
			names = append(names, selectIdentifierName(selected.Uid()))
			kinds = append(kinds, selectNameExplicit)
			continue
		}
		expression, ok := selected.Expression().(*antlrgen.PredicatedExpressionContext)
		if !ok || expression.Predicate() != nil {
			return nil, nil, "unnamed expression requires semantic publication"
		}
		switch atom := expression.ExpressionAtom().(type) {
		case *antlrgen.FullColumnNameExpressionAtomContext:
			parts := atom.FullColumnName().FullId().AllUid()
			names = append(names, selectIdentifierName(parts[len(parts)-1]))
			kinds = append(kinds, selectNameInherited)
		case *antlrgen.FunctionCallExpressionAtomContext:
			if _, aggregate := atom.FunctionCall().(*antlrgen.AggregateFunctionCallContext); !aggregate {
				return nil, nil, "unnamed non-aggregate call requires semantic publication"
			}
			// Java Expressions.getStructType uses the SELECT ordinal, not the
			// aggregate's position among aggregate calls. No production namer is used.
			names = append(names, fmt.Sprintf("_%d", i))
			kinds = append(kinds, selectNameAnonymousAggregate)
		default:
			return nil, nil, "unnamed expression requires semantic publication"
		}
	}
	return names, kinds, ""
}

// selectIdentifierName reads a typed identifier token, never expression text.
// The grammar's DOUBLE_QUOTE_ID token excludes embedded double quotes.
func selectIdentifierName(id antlrgen.IUidContext) string {
	if quoted := id.DOUBLE_QUOTE_ID(); quoted != nil {
		text := quoted.GetText()
		return text[1 : len(text)-1]
	}
	return strings.ToUpper(id.GetText())
}

// slotNameMatches reports whether a rendered slot name is an acceptable spelling
// of a derived SELECT-list name.
//
// SPELLING IS NOT ORDER, and this instrument is about order. Two spellings are
// genuinely planner-decided and must both be accepted:
//
//   - the QUALIFIER. Legacy renderings of `SELECT A."K", B."K", "X"` can carry
//     `A.K|B.K|X`, while other renderings publish unqualified labels. Which
//     spelling appears is a
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

// isFunctionCall recognizes legacy NAME(...) tokens in rendered expectations,
// not SQL syntax. Keeping them recognizable lets the gate report stale labels.
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
		names, kinds, why := selectOutputNamesWithProvenance(sqls[0])
		sites = append(sites, slotOrderSite{
			file: rel, line: fset.Position(call.Lparen).Line,
			sql: sqls[0], rows: rows, derived: names, nameKinds: kinds, whyNot: why, fromCall: true,
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
		names, kinds, why := selectOutputNamesWithProvenance(sqls[0])
		sites = append(sites, slotOrderSite{
			file: rel, line: fset.Position(scopePos[scope]).Line,
			sql: sqls[0], rows: rows, derived: names, nameKinds: kinds, whyNot: why,
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
	namedOrder            int
	fallbackSites         int
	shapeMismatch         int
	fallbackWhy           map[string]int
	ambiguousRows         int
}

// hasNamedSlotOrder requires a distinguishable pair with at least one inherited
// or explicitly authored name. Anonymous _N alone cannot sustain this population;
// an authored alias spelled _0 can. This observes labels, not value ownership.
func hasNamedSlotOrder(names []string, kinds []selectNameKind) bool {
	if len(names) != len(kinds) {
		return false
	}
	for i, kind := range kinds {
		if kind != selectNameInherited && kind != selectNameExplicit {
			continue
		}
		for j := range names {
			if !slotNameMatches(names[i], names[j]) || !slotNameMatches(names[j], names[i]) {
				return true
			}
		}
	}
	return false
}

func slotOrderPopulationProblem(derived, namedOrder int) string {
	if derived < 0 || namedOrder < 0 || namedOrder > derived {
		return fmt.Sprintf("invalid slot-order population: derived=%d named=%d", derived, namedOrder)
	}
	if derived < 100 {
		return fmt.Sprintf("only %d row expectation(s) had their slot order DERIVED from a SELECT list — "+
			"this instrument is vacuous at that population. Something changed in how the sqldriver "+
			"tests carry their queries, and the census above says which files stopped being "+
			"derivable. Fix the pairing rather than lowering this floor.", derived)
	}
	if namedOrder < 1 {
		return "named/distinguishable slot-order population COLLAPSED: no width-matched row exercises " +
			"a distinguishable pair with an inherited or explicit-AS name. Anonymous ordinal labels " +
			"cannot substitute for named-order coverage; fix the pairing, not this floor."
	}
	return ""
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
				if hasNamedSlotOrder(s.derived, s.nameKinds) {
					c.namedOrder++
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
	totalDerived, totalDerivedSites, totalFallback, totalAmbiguous, totalNamedOrder := 0, 0, 0, 0, 0
	for _, rel := range order {
		c := census[rel]
		totalDerived += c.derived
		totalNamedOrder += c.namedOrder
		totalDerivedSites += c.derivedSites
		totalFallback += c.fallbackSites
		totalAmbiguous += c.ambiguousRows
		var why []string
		for w, n := range c.fallbackWhy {
			why = append(why, fmt.Sprintf("%s x%d", w, n))
		}
		sort.Strings(why)
		fmt.Fprintf(&table, "  %-58s derived %3d row(s) over %2d site(s); fallback %2d site(s) %s; not-a-slot-rendering %d; ambiguous %d row(s); named/distinguishable %d row(s)\n",
			strings.TrimPrefix(rel, rowValueMapScope), c.derived, c.derivedSites,
			c.fallbackSites, strings.Join(why, ", "), c.shapeMismatch, c.ambiguousRows, c.namedOrder)
	}
	t.Logf("slot-order derivation census (derived = expectation order checked against the SELECT list):\n%s"+
		"  TOTAL derived %d row(s) over %d site(s); %d fallback site(s); %d ambiguous row(s); %d named/distinguishable row(s)",
		table.String(), totalDerived, totalDerivedSites, totalFallback, totalAmbiguous, totalNamedOrder)

	// A derivation that reaches nothing is green forever. The measured population
	// at the time this landed was far above this floor; it exists so a broken
	// pairing or a changed helper shape reads as red, not as a clean result.
	if problem := slotOrderPopulationProblem(totalDerived, totalNamedOrder); problem != "" {
		t.Fatal(problem)
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
		{`SELECT A."K", COUNT(*) FROM A GROUP BY A."K"`, []string{"K", "_1"}},
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

func TestSelectOutputNameTypedBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{`SELECT COUNT(*), k, SUM(v) FROM t GROUP BY k`, []string{"_0", "K", "_2"}},
		{`SELECT id x, COUNT(*) n FROM t GROUP BY id`, []string{"ID", "_1"}},
		{`SELECT id AS x, COUNT(*) AS n FROM t GROUP BY id`, []string{"X", "N"}},
		{`SELECT t."a.b", COUNT(*) AS "_0" FROM t GROUP BY t."a.b"`, []string{"a.b", "_0"}},
		{`SELECT 'FROM, AS )' AS "text", CAST(1 AS BIGINT) AS n FROM t`, []string{"text", "N"}},
		{`SELECT "a""b" FROM t`, []string{"a"}},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()
			got, why := selectOutputNames(tc.sql)
			if why != "" || !equalStringSlices(got, tc.want) {
				t.Fatalf("names=%q reason=%q, want %q", got, why, tc.want)
			}
		})
	}
	for _, sql := range []string{
		`SELECT SUM(v) + 1 FROM t`,
		`SELECT CAST(SUM(v) AS BIGINT) FROM t`,
		`SELECT COALESCE(SUM(v), 0) FROM t`,
		`SELECT CASE WHEN k > 0 THEN SUM(v) ELSE 0 END FROM t GROUP BY k`,
		`WITH c AS (SELECT k FROM t) SELECT k FROM c`,
		`SELECT k FROM t UNION ALL SELECT k FROM t`,
		`(SELECT k FROM t)`, `SELECT k FROM t; SELECT k FROM t`,
		`SELECT k FROM`, `SELECT k`, `INSERT INTO t VALUES (1)`, ``, `SELECT k AS "a""b" FROM t`,
	} {
		t.Run("decline/"+sql, func(t *testing.T) {
			t.Parallel()
			if names, why := selectOutputNames(sql); why == "" {
				t.Fatalf("unexpectedly derived %q from unsupported shape", names)
			}
		})
	}
}

func TestSelectOutputNameProvenance(t *testing.T) {
	t.Parallel()
	names, kinds, why := selectOutputNamesWithProvenance(`SELECT id ignored, COUNT(*) ignored, COUNT(*) AS "_0" FROM t GROUP BY id`)
	wantKinds := []selectNameKind{selectNameInherited, selectNameAnonymousAggregate, selectNameExplicit}
	if why != "" || !equalStringSlices(names, []string{"ID", "_1", "_0"}) || len(kinds) != len(wantKinds) {
		t.Fatalf("names=%q kinds=%v reason=%q", names, kinds, why)
	}
	for i, want := range wantKinds {
		if kinds[i] != want {
			t.Errorf("slot %d provenance=%v, want %v", i, kinds[i], want)
		}
	}
}

func TestNamedSlotOrderPopulation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		names []string
		kinds []selectNameKind
		want  bool
	}{
		{"empty", nil, nil, false},
		{"missing provenance", []string{"K", "V"}, nil, false},
		{"single named", []string{"K"}, []selectNameKind{selectNameInherited}, false},
		{"duplicate names", []string{"K", "k"}, []selectNameKind{selectNameInherited, selectNameExplicit}, false},
		{"anonymous only", []string{"_0", "_1"}, []selectNameKind{selectNameAnonymousAggregate, selectNameAnonymousAggregate}, false},
		{"authored ordinal spelling", []string{"_0", "_1"}, []selectNameKind{selectNameExplicit, selectNameAnonymousAggregate}, true},
		{"inherited pair", []string{"K", "V"}, []selectNameKind{selectNameInherited, selectNameInherited}, true},
		{"inherited and anonymous", []string{"_0", "K"}, []selectNameKind{selectNameAnonymousAggregate, selectNameInherited}, true},
		{"authored anonymous collision", []string{"_1", "_1"}, []selectNameKind{selectNameExplicit, selectNameAnonymousAggregate}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := hasNamedSlotOrder(tc.names, tc.kinds); got != tc.want {
				t.Fatalf("hasNamedSlotOrder(%q, %v)=%v, want %v", tc.names, tc.kinds, got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		derived, named int
		want           string
	}{
		{0, 0, "vacuous"},
		{99, 1, "vacuous"},
		{100, 0, "COLLAPSED"},
		{100, 1, ""},
		{101, 101, ""},
		{-1, 0, "invalid"},
		{100, -1, "invalid"},
		{100, 101, "invalid"},
	} {
		t.Run(fmt.Sprintf("floors/%d/%d", tc.derived, tc.named), func(t *testing.T) {
			t.Parallel()
			got := slotOrderPopulationProblem(tc.derived, tc.named)
			if (tc.want == "" && got != "") || (tc.want != "" && !strings.Contains(got, tc.want)) {
				t.Fatalf("population problem=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestAnonymousSlotOrderScope(t *testing.T) {
	t.Parallel()
	derived, why := selectOutputNames(`SELECT COUNT(*), SUM(v) FROM t`)
	if why != "" || !equalStringSlices(derived, []string{"_0", "_1"}) {
		t.Fatalf("derived=%q reason=%q", derived, why)
	}
	for _, tc := range []struct {
		row  string
		want bool
	}{
		{"_0=2|_1=30", true},
		{"_1=30|_0=2", false}, // Moving NAME=value pairs must be detected.
		{"_0=30|_1=2", true},  // Fixed-label value swaps are outside this gate.
	} {
		t.Run(tc.row, func(t *testing.T) {
			t.Parallel()
			names, ok := expectationNames(tc.row)
			if !ok || slotNamesMatch(names, derived) != tc.want {
				t.Fatalf("names=%q parsed=%v match=%v, want %v", names, ok, slotNamesMatch(names, derived), tc.want)
			}
		})
	}
}

func FuzzTypedSelectLabels(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3})
	f.Add([]byte{3, 2, 0, 1})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, choices []byte) {
		if len(choices) == 0 {
			choices = []byte{2}
		}
		if len(choices) > 16 {
			choices = choices[:16]
		}
		var items, want []string
		var wantKinds []selectNameKind
		for i, choice := range choices {
			switch choice % 4 {
			case 0:
				items = append(items, `q."k.part" ignored`)
				want = append(want, "k.part")
				wantKinds = append(wantKinds, selectNameInherited)
			case 1:
				items = append(items, `COALESCE(q.k, 0) AS "_0"`)
				want = append(want, "_0")
				wantKinds = append(wantKinds, selectNameExplicit)
			case 2:
				items = append(items, "COUNT(*)")
				want = append(want, fmt.Sprint("_", i))
				wantKinds = append(wantKinds, selectNameAnonymousAggregate)
			case 3:
				items = append(items, "SUM(q.k) ignored")
				want = append(want, fmt.Sprint("_", i))
				wantKinds = append(wantKinds, selectNameAnonymousAggregate)
			}
		}
		sql := "SELECT " + strings.Join(items, ", /* FROM AS , ) */ ") + " FROM t AS q GROUP BY q.k, q.\"k.part\""
		got, kinds, why := selectOutputNamesWithProvenance(sql)
		if why != "" || !equalStringSlices(got, want) || len(kinds) != len(wantKinds) {
			t.Fatalf("query=%s names=%q kinds=%v reason=%q, want %q/%v", sql, got, kinds, why, want, wantKinds)
		}
		for i, kind := range kinds {
			if kind != wantKinds[i] {
				t.Fatalf("slot %d provenance=%v, want %v", i, kind, wantKinds[i])
			}
		}
	})
}

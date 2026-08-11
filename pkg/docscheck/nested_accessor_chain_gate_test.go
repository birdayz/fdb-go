// A RESOLVED DESCENT MAY NOT BE SEPARATED FROM THE VALUE IT BELONGS TO.
//
// A qualified column reference (`n.co`, where `N` is a struct column and `CO`
// one of its members) resolves to two things: the ROOT read that names the
// struct, and the ACCESSOR CHAIN that descends to the member. The reference
// denotes the LEAF. A mint that builds the root and drops the chain therefore
// returns the WHOLE STRUCT where a member was named — and nothing reports it.
// It is not a type error, not a runtime error, and not visible to a client
// scanning into `any`; a client demanding the member's own type sees
// `converting driver.Value type *rowstruct.MessageStruct to a int64`, which
// reads like a client bug. That defect shipped, twice, and was fixed twice by
// adding the missing fuse call to one more mint.
//
// WHY A GATE AND NOT JUST THE FIX. Both previous fixes closed the SITES that
// existed while leaving the SHAPE intact: each mint built a root and then, of
// its own accord, remembered to fuse. The fix therefore had to be re-applied by
// every future mint, and the failure mode of forgetting is silence. The commit
// that closed the second site said so in as many words — that a third mint
// which forgets is the same silent wrong-column read, and that it "must be
// written deliberately" rather than being impossible. This gate is what makes
// the stronger claim true.
//
// Java does not need the gate because its structure forbids the mistake:
// SemanticAnalyzer.lookupNestedField builds its accessor list as a LOCAL
// (SemanticAnalyzer.java:578-597) and returns only the already-fused Expression
// (SemanticAnalyzer.java:598-601). No Java caller can hold a chain apart from
// the value it belongs to, because no Java function hands one out. Go's
// analyzer RETURNS the chain — `(Column, ScopeSource, []NestedAccessor, error)`
// — which is the one structural difference that makes the defect expressible.
// So the gate polices exactly that difference, in two halves:
//
//  1. A chain a resolver hands out may not be DISCARDED at the call site.
//  2. A column-reference ROOT may only be built inside a mint that requires
//     the chain, so there is no way to obtain an unfused root and then forget.
//
// Neither half alone is sufficient, and that is the point of having both. Half
// 1 alone leaves a mint free to build a root and ignore a chain it did bind.
// Half 2 alone leaves a caller free to discard the chain before any mint sees
// it — which is precisely how the original defect was written.
//
// There is no allowlist of offending SITES. Half 2 exempts two functions BY
// NAME, and the exemption is structural rather than discretionary: both take
// the chain as a required parameter and both return through the fuse, so being
// inside one of them is itself the proof the chain was not dropped. The
// precision/recall fixtures below pin that a THIRD function copying either body
// is still caught — otherwise the exemption would be a way to reopen the class
// by writing one wrapper.
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

// exprPackageDir is the package whose column-reference mints half 2 polices,
// as a slash-separated repo-relative prefix.
//
// The restriction is scoped to this package because the ordinal constructors it
// names are general-purpose and legitimately used across the planner (index
// match candidates, the cascades translator) to build values that are not SQL
// column references and have no descent to carry. Widening the gate to those
// would report a permanent finding nobody can act on, which is how a gate gets
// deleted. What is special here is not the constructor, it is that in THIS
// package every use of it is a column reference minted from a resolution that
// may have descended.
const exprPackageDir = "pkg/relational/core/query/expr/"

// columnRefMints are the only functions in exprPackageDir that may build a
// column-reference root.
//
// Both take `accessors []semantic.NestedAccessor` as a required parameter and
// both return through fuseNestedAccessorsIfAny. A caller cannot obtain an
// unfused root from either, so "inside one of these" is a structural proof, not
// a promise. They are two rather than one because a reference is CORRELATED or
// SOURCE-RELATIVE and that choice is real; the descent is not a difference
// between them, which is exactly why it is not a parameter either one decides.
var columnRefMints = map[string]bool{
	"correlatedColumnRef":     true,
	"sourceRelativeColumnRef": true,
}

// columnRefRootConstructors are the value constructors that mint a column
// reference's root. A descent is fused ONTO one of these, so a call to one is
// the moment a chain could be forgotten.
var columnRefRootConstructors = map[string]bool{
	"NewCorrelatedFieldValueWithResolvedOrdinalInDomain": true,
	"NewFieldValueWithResolvedOrdinalInDomain":           true,
}

// isNestedAccessorSlice reports whether e is `[]NestedAccessor` or
// `[]semantic.NestedAccessor`.
//
// Matching on the syntactic type rather than a resolved one is deliberate and
// follows the ordinal-domain gate next door: this must run without type-checking
// the repo, and the name is unique enough in this tree that a syntactic match is
// exact. The precision/recall test is what holds that claim.
func isNestedAccessorSlice(e ast.Expr) bool {
	arr, ok := e.(*ast.ArrayType)
	if !ok || arr.Len != nil {
		return false
	}
	switch t := arr.Elt.(type) {
	case *ast.Ident:
		return t.Name == "NestedAccessor"
	case *ast.SelectorExpr:
		return t.Sel != nil && t.Sel.Name == "NestedAccessor"
	}
	return false
}

// chainReturners maps each function in f that HANDS OUT an accessor chain to
// the position of that chain in its result list.
//
// Discovered structurally rather than from a hard-coded list of resolver names,
// so the gate fails CLOSED on a resolver nobody has written yet: the day someone
// adds a sixth chain-returning lookup, its callers are policed the moment it
// exists, with no edit here. A hard-coded list would have needed maintaining by
// exactly the person most likely to be reintroducing the bug.
func chainReturners(f *ast.File) map[string]int {
	out := map[string]int{}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name == nil || fd.Type == nil || fd.Type.Results == nil {
			continue
		}
		idx := 0
		for _, field := range fd.Type.Results.List {
			// One field can declare several results (`a, b []NestedAccessor`),
			// so the position advances per VALUE, not per field.
			n := len(field.Names)
			if n == 0 {
				n = 1
			}
			if isNestedAccessorSlice(field.Type) {
				out[fd.Name.Name] = idx
				break
			}
			idx += n
		}
	}
	return out
}

// scanChainDiscards reports every place a chain-returning resolver's accessor
// result is assigned to the blank identifier.
//
// The blank identifier is the only silent discard available: Go already refuses
// to compile an unused `:=` binding, so a chain that is bound under a name is a
// chain some statement must mention. `_` is the shape the original defect had
// (`col, src, _, err := ...`), and it is the one shape the compiler will not
// object to.
func scanChainDiscards(f *ast.File, returners map[string]int, report func(token.Pos, string)) {
	ast.Inspect(f, func(n ast.Node) bool {
		var lhs []ast.Expr
		var rhs []ast.Expr
		switch x := n.(type) {
		case *ast.AssignStmt:
			lhs, rhs = x.Lhs, x.Rhs
		case *ast.ValueSpec:
			for _, id := range x.Names {
				lhs = append(lhs, id)
			}
			rhs = x.Values
		default:
			return true
		}
		if len(rhs) != 1 || len(lhs) < 2 {
			return true
		}
		call, ok := rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		name := callName(call)
		idx, isResolver := returners[name]
		if !isResolver || idx >= len(lhs) {
			return true
		}
		if id, isIdent := lhs[idx].(*ast.Ident); isIdent && id.Name == "_" {
			report(call.Lparen, name+" result "+fmt.Sprint(idx)+
				" (the resolved descent) discarded into `_`")
		}
		return true
	})
}

// scanColumnRefMints reports every column-reference root built outside the
// sanctioned mints. Only meaningful for files in exprPackageDir.
func scanColumnRefMints(f *ast.File, report func(token.Pos, string)) {
	for _, decl := range f.Decls {
		enclosing := ""
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name != nil {
			enclosing = fd.Name.Name
		}
		if columnRefMints[enclosing] {
			continue
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name := callName(call); columnRefRootConstructors[name] {
				where := "a package-level initializer"
				if enclosing != "" {
					where = enclosing
				}
				report(call.Lparen, name+" called in "+where+
					", which is not one of the column-reference mints")
			}
			return true
		})
	}
}

func TestResolvedDescentCannotBeSeparatedFromItsValue(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)
	files := trackedGoFiles(t, root)

	type parsed struct {
		rel  string
		fset *token.FileSet
		file *ast.File
	}
	var production []parsed
	returners := map[string]int{}

	for _, rel := range files {
		if strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "gen/") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		// Cheap pre-filter; the parse below is the authority. It must admit
		// every file either half could report on: the chain's type name for
		// half 1, and the constructor names for half 2.
		text := string(src)
		if !strings.Contains(text, "NestedAccessor") &&
			!strings.Contains(text, "WithResolvedOrdinalInDomain") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			continue // not this gate's job to police syntax
		}
		production = append(production, parsed{rel: rel, fset: fset, file: f})
		for name, idx := range chainReturners(f) {
			returners[name] = idx
		}
	}

	// A gate whose input collapsed reports green forever. Both populations are
	// guarded: the resolvers that hand out a chain, and the mints that consume
	// one. If either goes to zero the gate has stopped watching anything, and
	// that must fail rather than pass — a zero here would read exactly like
	// "no offences found".
	if len(returners) == 0 {
		t.Fatal("no chain-returning resolver found in the tree. Either the " +
			"accessor chain stopped being handed out (in which case this gate's " +
			"half 1 is obsolete and the DISCARD it watches for is unreachable — " +
			"say so and retire it deliberately), or `NestedAccessor` was renamed " +
			"and the detector is now blind. Do not treat this as a pass.")
	}
	var mintFiles int
	for _, p := range production {
		if strings.HasPrefix(p.rel, exprPackageDir) {
			mintFiles++
		}
	}
	if mintFiles == 0 {
		t.Fatalf("no production file under %s was scanned; half 2 is watching "+
			"nothing. The package moved or the pre-filter stopped admitting it.",
			exprPackageDir)
	}

	var findings []string
	for _, p := range production {
		add := func(pos token.Pos, form string) {
			findings = append(findings, fmt.Sprintf("%s:%d: %s",
				p.rel, p.fset.Position(pos).Line, form))
		}
		scanChainDiscards(p.file, returners, add)
		if strings.HasPrefix(p.rel, exprPackageDir) {
			scanColumnRefMints(p.file, add)
		}
	}
	sort.Strings(findings)
	if len(findings) > 0 {
		t.Errorf("%d site(s) can separate a resolved descent from the value it belongs to.\n\n"+
			"A qualified reference into a struct denotes the LEAF. A root built without its\n"+
			"accessor chain returns the WHOLE STRUCT instead, and that is silent: no type\n"+
			"error, no runtime error, and invisible to a client scanning into `any`. It has\n"+
			"shipped twice.\n\n"+
			"If a resolver's chain is discarded: it is not yours to drop. Pass it to the mint\n"+
			"(correlatedColumnRef / sourceRelativeColumnRef), which takes it as a required\n"+
			"argument and returns the fused value. A consumer that genuinely wants the\n"+
			"descent's LEAF COLUMN rather than a value takes the chain's last accessor — that\n"+
			"is a use, not a discard.\n\n"+
			"If a root constructor is called outside a mint: build the reference through a\n"+
			"mint instead. There is no allowlist of sites, and the two exemptions are by\n"+
			"function name because each of those functions REQUIRES the chain and returns\n"+
			"through the fuse — a new helper that copies one of their bodies is caught, by\n"+
			"design.\n\n%s",
			len(findings), strings.Join(findings, "\n"))
	}
}

// The gate is a claim about what cannot reach the tree, so it is pinned against
// synthetic source in BOTH directions.
//
// Recall alone is worth little here: a detector that also flagged the CORRECT
// mints would leave no legal way to build a column reference, and a gate with no
// legal alternative is a gate someone deletes the first time it is inconvenient.
// The negative cases are therefore the more important half.
func TestNestedAccessorGatePrecisionAndRecall(t *testing.T) {
	t.Parallel()

	parse := func(t *testing.T, body string) (*token.FileSet, *ast.File) {
		t.Helper()
		src := "package p\n\n" + body
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "snippet.go", src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse snippet: %v\n---\n%s", err, src)
		}
		return fset, f
	}

	// A realistic resolver declaration, so the discard fixtures exercise the
	// SAME discovery path the tree scan uses rather than a hand-fed map.
	const resolverDecl = "func Resolve(s *Scope, id Identifier) (Column, ScopeSource, []semantic.NestedAccessor, error) { return c, s, nil, nil }\n"

	scanDiscards := func(t *testing.T, body string) []string {
		t.Helper()
		_, f := parse(t, resolverDecl+body)
		returners := chainReturners(f)
		if _, ok := returners["Resolve"]; !ok {
			t.Fatalf("fixture resolver was not discovered as a chain returner; "+
				"the discard fixtures would be vacuous. got: %v", returners)
		}
		var got []string
		scanChainDiscards(f, returners, func(_ token.Pos, form string) {
			got = append(got, form)
		})
		return got
	}

	scanMints := func(t *testing.T, body string) []string {
		t.Helper()
		_, f := parse(t, body)
		var got []string
		scanColumnRefMints(f, func(_ token.Pos, form string) {
			got = append(got, form)
		})
		return got
	}

	t.Run("caught", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			body string
			scan func(*testing.T, string) []string
		}{
			{
				name: "the original defect's exact shape: chain discarded at the resolver",
				body: `func f() { col, src, _, err := Resolve(sc, id); _ = col; _ = src; _ = err }`,
				scan: scanDiscards,
			},
			{
				name: "discarded through a package selector call",
				body: `func f() { c, s, _, e := analyzer.Resolve(sc, id); _, _, _ = c, s, e }`,
				scan: scanDiscards,
			},
			{
				name: "discarded in a var declaration rather than a short assign",
				body: `var c, s, _, e = Resolve(sc, id)`,
				scan: scanDiscards,
			},
			{
				name: "a would-be THIRD mint hand-building a correlated root",
				body: `func myNewMint() { root := values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(qov, f, o, ty, d); _ = root }`,
				scan: scanMints,
			},
			{
				name: "a would-be third mint hand-building a source-relative root",
				body: `func myNewMint() { _ = values.NewFieldValueWithResolvedOrdinalInDomain(f, o, ty, d) }`,
				scan: scanMints,
			},
			{
				// The other half of the by-name exemption. A wrapper that copies
				// an exempt body is a FRESH mint and must be caught, or the
				// exemption becomes a way to reopen the class by writing one
				// function.
				name: "a new helper byte-identical to an exempt mint's body",
				body: `func mintColumnRef(col Column, src ScopeSource, accessors []semantic.NestedAccessor) (values.Value, error) {
					root := values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(qov, f, o, ty, d)
					return fuseNestedAccessorsIfAny(root, accessors)
				}`,
				scan: scanMints,
			},
			{
				// No enclosing function to name, so the report has to say
				// something other than an empty string — and the site is still
				// a root built outside a mint.
				name: "root built in a PACKAGE-LEVEL initializer, with no enclosing function",
				body: `var stray = values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(qov, f, o, ty, d)`,
				scan: scanMints,
			},
			{
				name: "root built unqualified, as it would be from inside package values",
				body: `func other() { _ = NewCorrelatedFieldValueWithResolvedOrdinalInDomain(qov, f, o, ty, d) }`,
				scan: scanMints,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if got := tc.scan(t, tc.body); len(got) == 0 {
					t.Errorf("detector MISSED a site that can separate a descent from its value.\n"+
						"This is the defect class the gate exists for; a miss here means the\n"+
						"gate reports green while the bug is writable.\n---\n%s", tc.body)
				}
			})
		}
	})

	t.Run("allowed", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			body string
			scan func(*testing.T, string) []string
		}{
			{
				name: "the chain is BOUND and passed to the mint — the correct shape",
				body: `func f() { col, src, accessors, err := Resolve(sc, id); _ = err; _ = correlatedColumnRef(col, src, accessors) }`,
				scan: scanDiscards,
			},
			{
				name: "a consumer taking the chain's LEAF rather than fusing is a USE",
				body: `func f() { col, _, accessors, err := Resolve(sc, id); if err == nil && len(accessors) > 0 { col = accessors[len(accessors)-1].Col }; _ = col }`,
				scan: scanDiscards,
			},
			{
				// Discarding the COLUMN or the SOURCE is ordinary and legal; only
				// the chain position is policed. A gate that flagged any `_` in
				// any multi-assign would be unusable and would be deleted.
				name: "discarding a NON-chain result position",
				body: `func f() { _, _, accessors, err := Resolve(sc, id); _ = accessors; _ = err }`,
				scan: scanDiscards,
			},
			{
				name: "an unrelated function returning a slice that is not a chain",
				body: `func g() (Column, ScopeSource, []Identifier, error) { return c, s, nil, nil }
				       func f() { a, b, _, e := g(); _, _, _ = a, b, e }`,
				scan: scanDiscards,
			},
			{
				name: "the correlated mint building its own root",
				body: `func correlatedColumnRef(col Column, src ScopeSource, accessors []semantic.NestedAccessor) (values.Value, error) {
					root := values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(qov, f, o, ty, d)
					return fuseNestedAccessorsIfAny(root, accessors)
				}`,
				scan: scanMints,
			},
			{
				name: "the source-relative mint building its own root",
				body: `func sourceRelativeColumnRef(col Column, src ScopeSource, accessors []semantic.NestedAccessor) (values.Value, error) {
					root := values.NewFieldValueWithResolvedOrdinalInDomain(f, o, ty, d)
					return fuseNestedAccessorsIfAny(root, accessors)
				}`,
				scan: scanMints,
			},
			{
				name: "a mint CALL from an ordinary resolver is not a root build",
				body: `func ResolveQualifiedProjectionPath() { _ = correlatedColumnRef(col, src, accessors) }`,
				scan: scanMints,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if got := tc.scan(t, tc.body); len(got) > 0 {
					t.Errorf("detector flagged a LEGAL shape: %v\n"+
						"The legal alternative must stay legal, or the gate leaves no way to\n"+
						"write a column reference and gets deleted.\n---\n%s", got, tc.body)
				}
			})
		}
	})
}

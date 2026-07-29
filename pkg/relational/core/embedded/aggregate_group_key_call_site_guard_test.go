package embedded

// The matcher's answer is non-decisive for symmetric inputs because EVERY call
// site ORs it with values.SemanticEqualsUnderAliasMap. That is a property of
// the CALL SITES, not of the matcher, so no test that only calls the matcher
// can establish it — hence the AST scan below.
//
// This is the sufficient half of the pair. The necessary half —
// "SemanticEqualsUnderAliasMap actually decides the symmetric shapes" — is
// TestAggregateGroupKeyMatcherIsDeadUnderSemanticEquality in
// aggregate_group_key_accessor_name_test.go. Together they say: for any pair
// the OR's first arm accepts, the matcher cannot change the outcome. Neither
// alone says it.

import (
	"bytes"
	"embed"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// embeddedSources embeds the package's own sources so the scan below runs
// regardless of working directory. Reading from the filesystem at runtime is
// unreliable under Bazel's sandbox, which does not stage sources next to the
// test binary; an embedded FS is resolved by the compiler and travels with the
// binary (rules_go surfaces it via go_test embedsrcs, populated by gazelle).
//
//go:embed *.go
var embeddedSources embed.FS

const aggkMatcherName = "fieldValueMatchesAggregateGroupKey"

const aggkSemanticEqName = "SemanticEqualsUnderAliasMap"

// TestAggregateGroupKeyMatcherIsAlwaysORedWithSemanticEquality pins the
// OR-guard property at every call site.
//
// The matcher is UNEXPORTED, so its call sites cannot exist outside this
// package and scanning the package's own sources is a COMPLETE enumeration —
// that is what makes this an argument rather than a sample.
//
// Two spellings are accepted, because both express the same guard:
//
//	semEq(a, b, am) || matcher(a, b, agg)        // accept if either says yes
//	!semEq(a, b, am) && !matcher(a, b, agg)      // reject only if both say no
//
// The second is the De Morgan dual of the first. The polarity check below is
// the load-bearing part: an OR of NEGATIONS, or an AND of positives, would
// silently make the matcher decisive again while still "mentioning" semantic
// equality next to it.
func TestAggregateGroupKeyMatcherIsAlwaysORedWithSemanticEquality(t *testing.T) {
	t.Parallel()

	if strings.ToUpper(aggkMatcherName[:1]) == aggkMatcherName[:1] {
		t.Fatalf("%s is EXPORTED: call sites can now exist outside this package, so "+
			"scanning this package's sources is no longer a complete enumeration",
			aggkMatcherName)
	}

	fset := token.NewFileSet()
	entries, err := fs.ReadDir(embeddedSources, ".")
	if err != nil {
		t.Fatalf("read embedded sources: %v", err)
	}

	// callSite is one syntactic invocation of the matcher.
	type callSite struct {
		pos     token.Position
		guarded bool
		why     string
	}
	var sites []callSite

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rerr := embeddedSources.ReadFile(name)
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		file, perr := parser.ParseFile(fset, name, src, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}

		// Parent links, so a matcher call can look outward at the boolean
		// expression it sits in.
		parent := map[ast.Node]ast.Node{}
		ast.Inspect(file, func(c ast.Node) bool {
			if c == nil {
				return false
			}
			for _, ch := range childrenOf(c) {
				parent[ch] = c
			}
			return true
		})

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != aggkMatcherName {
				return true
			}
			site := callSite{pos: fset.Position(call.Pos())}
			site.guarded, site.why = checkORGuard(fset, call, parent)
			sites = append(sites, site)
			return true
		})
	}

	if len(sites) == 0 {
		t.Fatalf("found ZERO call sites of %s: the scan is broken (or the matcher is "+
			"now dead code and this file, plus the reachability argument in "+
			"aggregate_group_key_accessor_name_test.go, must be re-derived)",
			aggkMatcherName)
	}

	// The count is asserted so that ADDING a call site is a deliberate act: a
	// new site must be read against the reachability argument, not just
	// compile.
	const wantSites = 3
	if len(sites) != wantSites {
		var where []string
		for _, s := range sites {
			where = append(where, s.pos.String())
		}
		t.Errorf("%s has %d call sites, want %d.\n  sites: %s\n"+
			"A NEW call site invalidates the reachability argument in "+
			"aggregate_group_key_accessor_name_test.go, which reasons over the "+
			"call sites as an enumerated set. Re-derive it, then update this count.",
			aggkMatcherName, len(sites), wantSites, strings.Join(where, ", "))
	}

	for _, s := range sites {
		if !s.guarded {
			t.Errorf("%s: call to %s is NOT OR-guarded by values.%s — %s\n"+
				"An unguarded call makes the matcher DECISIVE: its answer, rather than "+
				"semantic equality's, now selects the group-key slot. The whole "+
				"reachability argument (matcher is consulted only for the asymmetric "+
				"childless-vs-QOV shape) rests on this guard.",
				s.pos, aggkMatcherName, aggkSemanticEqName, s.why)
		}
	}
}

// childrenOf returns the direct child nodes of n. Only the node kinds that can
// appear on the path from a boolean operand up to its enclosing expression
// need real answers; ast.Inspect supplies the rest of the traversal.
func childrenOf(n ast.Node) []ast.Node {
	var out []ast.Node
	switch v := n.(type) {
	case *ast.BinaryExpr:
		out = append(out, v.X, v.Y)
	case *ast.UnaryExpr:
		out = append(out, v.X)
	case *ast.ParenExpr:
		out = append(out, v.X)
	case *ast.CallExpr:
		out = append(out, v.Fun)
		for _, a := range v.Args {
			out = append(out, a)
		}
	case *ast.IfStmt:
		out = append(out, v.Cond)
	}
	return out
}

// checkORGuard walks outward from a matcher call to the boolean expression it
// is an operand of, and verifies the sibling operand tests semantic equality
// on the SAME two values with the polarity that makes the matcher's answer
// non-decisive.
func checkORGuard(fset *token.FileSet, call *ast.CallExpr, parent map[ast.Node]ast.Node) (bool, string) {
	if len(call.Args) < 2 {
		return false, "matcher call has fewer than 2 arguments"
	}
	wantA := render(fset, call.Args[0])
	wantB := render(fset, call.Args[1])

	// Climb past parens, and past a single leading `!`, recording whether the
	// matcher call is negated.
	var node ast.Node = call
	negated := false
	for {
		p, ok := parent[node]
		if !ok {
			return false, "matcher call is not inside a boolean expression"
		}
		switch pv := p.(type) {
		case *ast.ParenExpr:
			node = p
			continue
		case *ast.UnaryExpr:
			if pv.Op == token.NOT {
				negated = !negated
			}
			node = p
			continue
		case *ast.BinaryExpr:
			if pv.Op != token.LOR && pv.Op != token.LAND {
				return false, "matcher call's enclosing operator is " + pv.Op.String() +
					", not || or &&"
			}
			sibling := pv.X
			if pv.X == node {
				sibling = pv.Y
			}
			semNeg, found := findSemanticEquals(fset, sibling, wantA, wantB)
			if !found {
				// The guard may sit one level further out, e.g.
				// `key.Value != nil && (semEq(...) || matcher(...))`.
				node = p
				continue
			}
			switch pv.Op {
			case token.LOR:
				if negated || semNeg {
					return false, "OR of NEGATED operands: `!semEq || !matcher` accepts " +
						"whenever EITHER says no, which is not the guard"
				}
				return true, ""
			case token.LAND:
				if !negated || !semNeg {
					return false, "AND of POSITIVE operands: `semEq && matcher` requires " +
						"BOTH to say yes, making the matcher decisive"
				}
				return true, ""
			}
			return false, "unreachable"
		default:
			return false, "matcher call is not an operand of a boolean expression"
		}
	}
}

// findSemanticEquals reports whether expr contains a call to
// values.SemanticEqualsUnderAliasMap over the same first two arguments, and
// whether that call is negated within expr.
func findSemanticEquals(fset *token.FileSet, expr ast.Expr, wantA, wantB string) (negated, found bool) {
	var walk func(e ast.Expr, neg bool) bool
	walk = func(e ast.Expr, neg bool) bool {
		switch v := e.(type) {
		case *ast.ParenExpr:
			return walk(v.X, neg)
		case *ast.UnaryExpr:
			if v.Op == token.NOT {
				return walk(v.X, !neg)
			}
			return false
		case *ast.BinaryExpr:
			return walk(v.X, neg) || walk(v.Y, neg)
		case *ast.CallExpr:
			sel, ok := v.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != aggkSemanticEqName || len(v.Args) < 2 {
				return false
			}
			if render(fset, v.Args[0]) != wantA || render(fset, v.Args[1]) != wantB {
				return false
			}
			negated, found = neg, true
			return true
		}
		return false
	}
	walk(expr, false)
	return negated, found
}

func render(fset *token.FileSet, e ast.Expr) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, e); err != nil {
		return "<unrenderable>"
	}
	return buf.String()
}

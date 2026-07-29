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

// A copy/rebind method that rebuilds its receiver from a FIELD-BY-FIELD
// composite literal silently zeroes every field added to the struct later.
//
// The method compiles. The tests pass. The new field is simply gone from every
// copy the memo makes, and because these methods are the memo's rewrite path,
// "every copy" is "every plan that survives more than one rule application".
//
// Two shipped instances prove the class:
//
//   - LogicalProjectionExpression.WithQuantifiers rebuilt three of four fields;
//     the fourth, the minted-alias provenance marker, was added later and would
//     have been dropped on every memo rewrite. It is excluded from memo
//     identity, so interning made the loss invisible end-to-end.
//   - SelectExpression.WithQuantifiers dropped quantifiersSwapped. Both readers
//     happened to be safety declines, so the loss failed OPEN — a missed
//     optimization, not a wrong row, which is exactly the kind of defect that
//     never produces a bug report.
//
// The fix in both cases is a struct copy with explicit overrides:
//
//	cp := *e
//	cp.inner = quantifiers[0]
//	return &cp
//
// which is correct for every field the struct will ever grow, including the
// ones nobody has written yet. That is the whole point: the field-literal form
// is not wrong TODAY, it is wrong on the day someone else adds a field, and
// that person has no reason to look at this method.
//
// So the gate is structural rather than a list of known-bad sites. It fails
// when a copy method contains a composite literal OF THE RECEIVER'S OWN TYPE.
//
// # What is deliberately allowed
//
//   - Struct copy plus field assignment (`cp := *e; cp.x = y`). No literal, so
//     nothing to enumerate and nothing to forget.
//   - Composite literals of OTHER types. Building a Quantifier, a slice, or a
//     child plan inside a copy method is ordinary work, not a rebuild.
//   - Returning the receiver unchanged, or a value the method received.
//
// # What a DELIBERATE reset looks like
//
// Some rebuilds genuinely want a field cleared — a cached derivation that the
// new children invalidate, for instance. Under the field-literal form a
// deliberate reset and an accidental omission are the SAME TEXT, so the intent
// is unrecoverable by reading. Under the struct-copy form they are distinct:
// the reset is an explicit `cp.cached = nil` with a comment saying why. The
// gate therefore does not need to distinguish them — the form does.
// # Why this is a predicate and not a list
//
// The first version of this gate held a LIST of seven method names, and it
// missed `SelectExpression.WithSwappedQuantifiers` — an 18th instance of the
// class, in the very file whose WithQuantifiers motivated the sweep. A
// hardcoded enumeration of what to check is the same defect the gate exists to
// prevent, one level up: it is correct until someone adds a name, and that
// person has no reason to look here.
//
// So membership is structural. Go's convention for "return a modified copy" is
// a `With…` prefix, which covers every instance found (WithQuantifiers,
// WithChildren, WithNewChildren, WithSwappedQuantifiers) plus every one anyone
// invents later, and the four bare verbs cover the non-`With` idioms.
func isCopyMethodName(name string) bool {
	switch name {
	case "Copy", "Clone", "Duplicate", "Rebind":
		return true
	}
	// Bare `With` is included: `Options.With` is a copy-with-one-option-applied
	// and rebuilt field-by-field, so excluding it as "builder-style" — which the
	// first draft did — was a hole justified by a guess about naming rather than
	// by what the tree contains.
	//
	// `Rebase…` is the same contract under a different verb, and not by
	// analogy: FieldPath.Domain's own documentation states that the
	// preservation contract "binds COPY, REBUILD and REBASE sites". A rebase
	// hands back the same node moved to a new alias, so every field it does not
	// name is a field it drops. `QuantifiedObjectValue.RebaseLeaf` was found
	// exactly this way, after the `With`-only predicate reported clean.
	return strings.HasPrefix(name, "With") || strings.HasPrefix(name, "Rebase")
}

// scanCopyMethodRebuilds reports every composite literal of the receiver's own
// type inside a copy/rebind method.
//
// The receiver type is matched UNQUALIFIED, which is what a same-package
// literal looks like (`&SelectExpression{...}` inside package expressions). A
// qualified literal (`other.Foo{...}`) is by construction not the receiver's
// type, so ignoring selector-typed literals loses nothing.
//
// Generic receivers (`func (p *pair[T]) Copy()`) present their type as an
// IndexExpr; the base identifier is what the literal will also name, so the
// index is stripped from both sides.
func scanCopyMethodRebuilds(f *ast.File, report func(pos token.Pos, method, recv, form string)) {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Body == nil {
			continue
		}
		if !isCopyMethodName(fn.Name.Name) {
			continue
		}
		recv := receiverTypeName(fn.Recv.List[0].Type)
		if recv == "" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || lit.Type == nil {
				return true
			}
			if baseTypeName(lit.Type) != recv {
				return true
			}
			report(lit.Pos(), fn.Name.Name, recv, fmt.Sprintf(
				"composite literal %s{...} rebuilds the receiver field-by-field", recv))
			return true
		})
	}
}

// receiverTypeName extracts `T` from `T`, `*T`, `T[P]`, or `*T[P]`.
func receiverTypeName(e ast.Expr) string {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	return baseTypeName(e)
}

// baseTypeName extracts the unqualified base identifier of a type expression,
// stripping pointers and type arguments. Qualified names (pkg.T) return "" —
// they cannot be the receiver's own type in the receiver's own package.
func baseTypeName(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}

// TestCopyMethodsDoNotRebuildFieldByField is the ratchet. It must stay at zero:
// there is no allowlist, because every instance found so far was convertible to
// a struct copy with identical semantics, and a list of exceptions is how the
// class regrows one "just this one" at a time.
func TestCopyMethodsDoNotRebuildFieldByField(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)
	var findings []string
	for _, rel := range trackedGoFiles(t, root) {
		if strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "gen/") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			continue // not this gate's job to police syntax
		}
		scanCopyMethodRebuilds(f, func(pos token.Pos, method, recv, form string) {
			findings = append(findings, fmt.Sprintf("%s:%d: %s.%s: %s",
				rel, fset.Position(pos).Line, recv, method, form))
		})
	}
	sort.Strings(findings)
	if len(findings) > 0 {
		t.Errorf("%d copy/rebind method(s) rebuild the receiver from a field-by-field literal.\n"+
			"Any field added to the struct later is silently dropped by these methods.\n"+
			"Convert to a struct copy with explicit overrides:\n"+
			"\tcp := *e\n\tcp.<field> = <new>\n\treturn &cp\n"+
			"A field the copy must deliberately RESET becomes an explicit override with a\n"+
			"comment saying so — under a literal, deliberate and accidental are the same text.\n\n%s",
			len(findings), strings.Join(findings, "\n"))
	}
}

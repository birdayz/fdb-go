package docscheck

// A PINNED ordinal must state the layout it indexes.
//
// RFC-197 step 0 names three elements of a column's identity: the ordinal, the
// pin (is this slot a recorded fact, or a bake to be re-derived from a display
// name), and the DOMAIN — which row layout the integer addresses. The first two
// without the third are not two thirds of an identity; they are an assertion
// with no referent. `FieldPath.Domain` fails closed on an unknown token at every
// `OrdinalIn`, so a domain-less pinned ordinal does not merely under-specify: it
// silently declines every downstream comparison that would have used it.
//
// The failure this exists to prevent is not hypothetical. A group key's
// SOURCE-relative ordinal and an aggregate's OUTPUT-row ordinal met in one
// comparison and matched because the integers coincided, rewriting the `SUM(v)`
// of `HAVING v > SUM(v)` into a reference to the group key, after which the
// predicate looked key-only and was pushed onto the raw scan. Wrong rows, from
// two correct integers compared against unstated layouts.
//
// WHY A GATE AND NOT JUST THE DELETION. The domain-less constructor
// (`NewFieldValueWithPinnedOrdinal`) had every call site migrated, then sat at
// zero callers for several revisions before being removed. Nothing marked it
// dead and nothing would have objected to a new caller — the migration's own
// pin, `TestPinnedAggregateReferenceStatesTheLayoutItsOrdinalIndexes`, covers
// the AGGREGATE path and is silent about the rest of the tree. So the closed
// state was closed only by the absence of anyone reopening it, which is the
// shape of every latent defect in this workstream: green because unexercised.
//
// The gate is the same shape as the `.Field`-decides ratchet next door: it is an
// AST fact about what may reach the tree, checked at build time, with NO
// allowlist. If a composition genuinely cannot state the layout it numbered a
// slot against, it has not decided the slot, and the honest mint is an UNPINNED
// ordinal — which is a legal, supported, domain-optional shape.

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

// pinnedDomainOffense is one production mint of a pinned ordinal that does not
// state a domain.
type pinnedDomainOffense struct {
	pos  token.Pos
	form string
}

// callName returns the bare function name of a call expression, whether it is
// called unqualified (inside package values) or through a package selector
// (`values.NewFieldValueWithPinnedOrdinalInDomain`). Returns "" for anything
// that is not a plain function or method call by name.
//
// Matching on the SELECTOR rather than the resolved type is deliberate: this is
// a source-level gate that must run without type-checking the whole repo, and
// these constructor names are unique enough in this tree that a syntactic match
// is exact. The precision/recall test below is what holds that claim.
func callName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// isLiteralTrue reports whether e is the untyped boolean literal `true`.
// A VARIABLE carrying true is deliberately not matched: the gate's claim is
// about mints that are pinned UNCONDITIONALLY and stated no domain, which is
// the shape a human writes by hand. A computed pin (pullup.go threads one) is
// paired with a computed domain and is not this class.
func isLiteralTrue(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "true"
}

// isEmptyOrdinalDomainLiteral reports whether e is `OrdinalDomain{}` or
// `values.OrdinalDomain{}` with no elements — the unknown token written inline,
// which is exactly the "I am pinned against an unstated row" claim.
func isEmptyOrdinalDomainLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.CompositeLit)
	if !ok || len(lit.Elts) != 0 {
		return false
	}
	switch t := lit.Type.(type) {
	case *ast.Ident:
		return t.Name == "OrdinalDomain"
	case *ast.SelectorExpr:
		return t.Sel.Name == "OrdinalDomain"
	}
	return false
}

// scanPinnedOrdinalMints reports every production mint of a PINNED FieldPath
// that states no domain. Split out from the test so both its precision and its
// recall can be pinned against synthetic source: a detector that matches nothing
// is green forever while the class regrows underneath it.
func scanPinnedOrdinalMints(f *ast.File, report func(token.Pos, string)) {
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch callName(call) {
		// The removed convenience constructor. Its whole body was
		// `...InDomain(field, ordinal, typ, OrdinalDomain{})`, so any
		// reintroduction is a domain-less pinned mint by construction — there is
		// no correct version of this name.
		case "NewFieldValueWithPinnedOrdinal":
			report(call.Lparen, "NewFieldValueWithPinnedOrdinal (removed: mints Domain unknown)")

		// The raw FieldPath constructor with the pin argument hard-coded true.
		// Signature: (field string, ordinal int, frontierPinned bool).
		case "NewFieldPathOfSingle":
			if len(call.Args) == 3 && isLiteralTrue(call.Args[2]) {
				report(call.Lparen, "NewFieldPathOfSingle(..., true) — pinned, and this "+
					"constructor cannot state a domain")
			}

		// Pinned through the domain-taking constructor, with the domain written
		// as the unknown token. This is the hole the deletion above does NOT
		// close: the correct constructor, called with a token that says nothing.
		case "NewFieldPathOfSingleInDomain":
			if len(call.Args) == 4 && isLiteralTrue(call.Args[2]) &&
				isEmptyOrdinalDomainLiteral(call.Args[3]) {
				report(call.Lparen, "NewFieldPathOfSingleInDomain(..., true, OrdinalDomain{})")
			}
		case "NewFieldValueWithPinnedOrdinalInDomain":
			if len(call.Args) == 4 && isEmptyOrdinalDomainLiteral(call.Args[3]) {
				report(call.Lparen, "NewFieldValueWithPinnedOrdinalInDomain(..., OrdinalDomain{})")
			}
		}
		return true
	})
}

func TestPinnedOrdinalAlwaysStatesItsDomain(t *testing.T) {
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
		// Cheap pre-filter; the parse below is the authority.
		if !strings.Contains(string(src), "PinnedOrdinal") &&
			!strings.Contains(string(src), "NewFieldPathOfSingle") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			continue // not this gate's job to police syntax
		}
		scanPinnedOrdinalMints(f, func(pos token.Pos, form string) {
			findings = append(findings, fmt.Sprintf("%s:%d: %s",
				rel, fset.Position(pos).Line, form))
		})
	}
	sort.Strings(findings)
	if len(findings) > 0 {
		t.Errorf("%d production mint(s) pin an ordinal without stating the layout it indexes.\n"+
			"A pinned ordinal claims the slot is a RECORDED FACT; an unknown domain says the\n"+
			"fact is about an UNSTATED ROW. Together they are precisely the comparison\n"+
			"FieldPath.Domain exists to refuse — a group key's source-relative ordinal and an\n"+
			"aggregate's output-row ordinal matching because the integers coincided, which\n"+
			"pushed a non-key HAVING predicate onto a raw scan in production.\n\n"+
			"There is no allowlist, by design. The composition that pinned the ordinal is\n"+
			"holding the layout by definition — deciding a slot MEANS holding the layout. A\n"+
			"caller that cannot name one has not decided a slot, and its honest mint is an\n"+
			"UNPINNED ordinal (NewFieldValueWithResolvedOrdinal / ...InDomain), which is a\n"+
			"supported shape and carries no false claim.\n\n"+
			"Derive the token with OrdinalDomainOfColumnNames (an ordered column list) or\n"+
			"OrdinalDomainOfType (a RecordType) from the SAME enumeration that chose the\n"+
			"slot, so a slot's position and the layout containing it cannot drift.\n\n%s",
			len(findings), strings.Join(findings, "\n"))
	}
}

// The gate is a claim about what cannot reach the tree, so it is pinned against
// synthetic source in BOTH directions.
//
// Recall alone is not enough: a detector that also matched the CORRECT mints
// would leave no legal way to pin an ordinal, and a gate with no legal
// alternative gets deleted the first time it is inconvenient. So the negative
// cases below are the shapes that MUST stay legal, and they are the more
// important half of this test.
func TestPinnedOrdinalDetectorPrecisionAndRecall(t *testing.T) {
	t.Parallel()

	scan := func(body string) []string {
		src := "package p\n\n" + body
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "snippet.go", src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse snippet: %v\n---\n%s", err, src)
		}
		var got []string
		scanPinnedOrdinalMints(f, func(_ token.Pos, form string) {
			got = append(got, form)
		})
		return got
	}

	caught := []struct {
		name string
		body string
	}{
		{
			name: "the removed convenience constructor, unqualified",
			body: `func f() { _ = NewFieldValueWithPinnedOrdinal("K", 1, typ) }`,
		},
		{
			name: "the removed convenience constructor, package-qualified",
			body: `func f() { _ = values.NewFieldValueWithPinnedOrdinal("K", 1, typ) }`,
		},
		{
			name: "raw FieldPath pinned with a hard-coded true",
			body: `func f() { _ = NewFieldPathOfSingle("K", 1, true) }`,
		},
		{
			name: "raw FieldPath pinned with true, package-qualified",
			body: `func f() { _ = values.NewFieldPathOfSingle("K", 1, true) }`,
		},
		{
			name: "the domain-taking path constructor called with the unknown token",
			body: `func f() { _ = NewFieldPathOfSingleInDomain("K", 1, true, OrdinalDomain{}) }`,
		},
		{
			name: "the domain-taking path constructor, qualified token",
			body: `func f() { _ = NewFieldPathOfSingleInDomain("K", 1, true, values.OrdinalDomain{}) }`,
		},
		{
			name: "the domain-taking value constructor called with the unknown token",
			body: `func f() { _ = NewFieldValueWithPinnedOrdinalInDomain("K", 1, typ, OrdinalDomain{}) }`,
		},
	}
	for _, tc := range caught {
		t.Run("caught/"+tc.name, func(t *testing.T) {
			t.Parallel()
			if got := scan(tc.body); len(got) == 0 {
				t.Errorf("detector missed a domain-less pinned mint.\n"+
					"source: %s\n"+
					"A gate that does not match this shape is green forever while the class "+
					"regrows underneath it.", tc.body)
			}
		})
	}

	legal := []struct {
		name string
		body string
		why  string
	}{
		{
			name: "pinned WITH a derived domain — the shape the whole gate exists to steer toward",
			body: `func f() { _ = NewFieldValueWithPinnedOrdinalInDomain("K", 1, typ, OrdinalDomainOfColumnNames(names)) }`,
			why:  "this is the correct mint; flagging it would leave no legal way to pin",
		},
		{
			name: "pinned with a domain derived from a type",
			body: `func f() { _ = NewFieldPathOfSingleInDomain("K", 1, true, OrdinalDomainOfType(rt)) }`,
			why:  "OrdinalDomainOfType is the other sanctioned derivation",
		},
		{
			name: "UNPINNED with no domain",
			body: `func f() { _ = NewFieldPathOfSingle("K", 1, false) }`,
			why: "an unpinned ordinal makes no claim about a composed row, so it needs no " +
				"layout; this is the honest mint for a caller that did not decide the slot",
		},
		{
			name: "UNPINNED through the domain-taking constructor with the unknown token",
			body: `func f() { _ = NewFieldPathOfSingleInDomain("K", 1, false, OrdinalDomain{}) }`,
			why:  "same: the pin is what makes a missing domain a false claim",
		},
		{
			name: "a computed pin paired with a computed domain",
			body: `func f() { _ = NewFieldPathOfSingleInDomain(n, i, inPinned, OrdinalDomainOfType(rc.Type())) }`,
			why: "pullup.go's real shape — the pin is threaded, and the domain is derived " +
				"from the same type the reference flows",
		},
		{
			name: "an unrelated constructor that merely mentions an ordinal",
			body: `func f() { _ = NewFieldValueWithResolvedOrdinal("K", 1, typ) }`,
			why:  "source-relative ordinals are a different class and are not this gate's business",
		},
	}
	for _, tc := range legal {
		t.Run("legal/"+tc.name, func(t *testing.T) {
			t.Parallel()
			if got := scan(tc.body); len(got) != 0 {
				t.Errorf("detector flagged a LEGAL mint: %v\n"+
					"source: %s\n"+
					"why this must stay legal: %s\n"+
					"A gate that forbids the correct shape as well as the wrong one leaves no "+
					"legal alternative, and a gate with no legal alternative gets deleted.",
					got, tc.body, tc.why)
			}
		})
	}
}

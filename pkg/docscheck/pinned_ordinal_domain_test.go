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
//
// There is exactly one structural exemption, and it is not an allowlist of
// offending SITES: `theDomainLessConstructor`'s own body may forward the unknown
// token, because forwarding it is the function's definition. Its callers are
// policed instead. The precision/recall fixtures pin that the exemption is by
// NAME and does not extend to a wrapper that copies the body.

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

// isLiteralTrue / isLiteralFalse report whether e is the untyped boolean literal.
//
// Only `false` proves a mint UNPINNED. `true` and every computed expression are
// treated alike once the domain is provably the zero token — see the DOMAIN-FIRST
// note on scanPinnedOrdinalMints for why the pin's shape stopped being the thing
// that decides.
func isLiteralTrue(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "true"
}

func isLiteralFalse(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "false"
}

// isOrdinalDomainType reports whether e names the `OrdinalDomain` type, bare or
// package-qualified.
func isOrdinalDomainType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == "OrdinalDomain"
	case *ast.SelectorExpr:
		return t.Sel.Name == "OrdinalDomain"
	}
	return false
}

// isEmptyOrdinalDomainLiteral reports whether e is `OrdinalDomain{}` or
// `values.OrdinalDomain{}` with no elements — the unknown token written inline,
// which is exactly the "I am pinned against an unstated row" claim.
func isEmptyOrdinalDomainLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.CompositeLit)
	if !ok || len(lit.Elts) != 0 {
		return false
	}
	return isOrdinalDomainType(lit.Type)
}

// zeroDomainIdents returns the identifiers in f that are PROVABLY the zero
// OrdinalDomain — every binding the file gives the name is the unknown token.
//
// Writing `OrdinalDomain{}` at the call site is the obvious shape and the one the
// gate first matched. It is not the only one, and the alternatives are not exotic:
// `d := OrdinalDomain{}` one line above the call, a package-level `var
// emptyDomain = OrdinalDomain{}` shared by several mints, or `var d
// OrdinalDomain` — a zero VALUE declared with no literal at all, which carries
// the unknown token while containing none of the syntax the gate looked for.
//
// Provably means ALL bindings, not any: a name bound once to the token and later
// to `OrdinalDomainOfType(...)` is not a zero-domain variable, and flagging it
// would make the gate fire on a correct mint. That is the direction the
// precision half of the test below exists to hold.
func zeroDomainIdents(f *ast.File) map[string]bool {
	bindings, zeroBindings := map[string]int{}, map[string]int{}
	bind := func(lhs, rhs []ast.Expr) {
		if len(lhs) != len(rhs) {
			return // a multi-value call; nothing here is a domain literal
		}
		for i, l := range lhs {
			id, ok := l.(*ast.Ident)
			if !ok || id.Name == "_" {
				continue
			}
			bindings[id.Name]++
			if isEmptyOrdinalDomainLiteral(rhs[i]) {
				zeroBindings[id.Name]++
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ValueSpec:
			if len(x.Values) == 0 {
				// `var d OrdinalDomain` — the zero value, no literal written.
				if isOrdinalDomainType(x.Type) {
					for _, id := range x.Names {
						if id.Name != "_" {
							bindings[id.Name]++
							zeroBindings[id.Name]++
						}
					}
				}
				return true
			}
			lhs := make([]ast.Expr, len(x.Names))
			for i, id := range x.Names {
				lhs[i] = id
			}
			bind(lhs, x.Values)
		case *ast.AssignStmt:
			bind(x.Lhs, x.Rhs)
		}
		return true
	})
	zero := map[string]bool{}
	for name, n := range bindings {
		if zeroBindings[name] == n {
			zero[name] = true
		}
	}
	return zero
}

// isProvablyZeroDomain reports whether e is the unknown token, written inline or
// carried by a variable this file only ever binds to it.
func isProvablyZeroDomain(e ast.Expr, zero map[string]bool) bool {
	if isEmptyOrdinalDomainLiteral(e) {
		return true
	}
	id, ok := e.(*ast.Ident)
	return ok && zero[id.Name]
}

// scanPinnedOrdinalMints reports every production mint of a PINNED FieldPath
// that states no domain. Split out from the test so both its precision and its
// recall can be pinned against synthetic source: a detector that matches nothing
// is green forever while the class regrows underneath it.
//
// IT ASKS ABOUT THE DOMAIN FIRST, and only then about the pin. The first version
// did the reverse — it required a literal `true` in the pin position — and two
// shapes walked straight through it:
//
//	NewFieldPathOfSingleInDomain(f, i, computedPin, OrdinalDomain{})
//	NewFieldValueWithPinnedOrdinalInDomain(f, i, typ, someEmptyDomainVar)
//
// Neither is a stretch. The first is what any mint threading a pin through a
// parameter looks like, and the rationale for exempting it — "a computed pin is
// paired with a computed domain" — was an observation about the call sites that
// happened to exist, asserted as if it were a property. It is not one, and the
// call site right next to it is the counterexample. The second needs only a
// `d := OrdinalDomain{}` on the line above.
//
// So the ordering inverts. A provably-zero domain is the offence; the pin is
// consulted only to see whether the mint is EXEMPT, and only a literal `false`
// exempts it. `true`, a variable, a function call, a field read — all flagged,
// because the gate cannot see what they carry, and a mint that MIGHT be pinned
// against an unstated row is exactly what it refuses. That also makes the gate
// fail CLOSED on a constructor nobody has written yet: any future `...InDomain`
// name whose last argument is the unknown token is reported unless it is
// classified here.
func scanPinnedOrdinalMints(f *ast.File, report func(token.Pos, string)) {
	zero := zeroDomainIdents(f)
	for _, decl := range f.Decls {
		enclosing := ""
		if fd, isFunc := decl.(*ast.FuncDecl); isFunc && fd.Name != nil {
			enclosing = fd.Name.Name
		}
		scanDeclForPinnedOrdinalMints(decl, enclosing, zero, report)
	}
}

// theDomainLessConstructor is the ONE function whose body may forward the
// unknown token, because forwarding it is what the function IS.
//
// `newFieldPathOfSingle(f, i, pinned)` is `...InDomain(f, i, pinned, OrdinalDomain{})`
// and nothing else. It is a legal, supported shape — an UNPINNED ordinal needs no
// layout — so it is policed at its CALL SITES (`NewFieldPathOfSingle(..., true)`),
// which is where the pin becomes a literal and the mint becomes a false claim.
// Reporting the delegation itself would report one unchanging fact forever, and a
// gate whose only finding is its own definition is a gate someone deletes.
//
// `NewFieldValueWithPinnedOrdinal` is deliberately NOT exempt. It is deleted, and
// there is no correct version of that name — a reintroduction would be pinned
// unconditionally, so its body must be flagged even before it has a single
// caller. That zero-caller window is precisely the state CQ-56 found it in.
const theDomainLessConstructor = "newFieldPathOfSingle"

func isDomainLessConstructor(name string) bool {
	// Keep the exported spelling for the detector's synthetic historical
	// fixture. Production renamed the private constructor under RFC-232, but the
	// gate must still prove it recognizes the old API if somebody reintroduces
	// it while exempting only the one legitimate forwarding body.
	return name == theDomainLessConstructor || name == "NewFieldPathOfSingle"
}

func scanDeclForPinnedOrdinalMints(decl ast.Decl, enclosing string, zero map[string]bool, report func(token.Pos, string)) {
	ast.Inspect(decl, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := callName(call)
		switch name {
		// The removed convenience constructor. Its whole body was
		// `...InDomain(field, ordinal, typ, OrdinalDomain{})`, so any
		// reintroduction is a domain-less pinned mint by construction — there is
		// no correct version of this name.
		case "NewFieldValueWithPinnedOrdinal":
			report(call.Lparen, "NewFieldValueWithPinnedOrdinal (removed: mints Domain unknown)")
			return true

		// The raw FieldPath constructor with the pin argument hard-coded true.
		// Signature: (field string, ordinal int, frontierPinned bool). It cannot
		// state a domain at all, so there is nothing to inspect but the pin.
		case "NewFieldPathOfSingle", "newFieldPathOfSingle":
			if len(call.Args) == 3 && isLiteralTrue(call.Args[2]) {
				report(call.Lparen, "NewFieldPathOfSingle(..., true) — pinned, and this "+
					"constructor cannot state a domain")
			}
			return true
		}

		// Every domain-taking constructor takes the token LAST.
		if !strings.HasSuffix(name, "InDomain") || len(call.Args) == 0 {
			return true
		}
		if isDomainLessConstructor(enclosing) {
			return true
		}
		domainArg := call.Args[len(call.Args)-1]
		if !isProvablyZeroDomain(domainArg, zero) {
			return true
		}
		how := "OrdinalDomain{}"
		if id, isIdent := domainArg.(*ast.Ident); isIdent {
			how = id.Name + " (a variable this file only ever binds to OrdinalDomain{})"
		}

		switch {
		// A RESOLVED ordinal is source-relative and unpinned: it makes no claim
		// about a composed row, so it needs no layout and an unknown token is
		// honest. This is the legal alternative the gate steers toward, and
		// flagging it would leave no way to mint an undecided slot.
		case strings.Contains(name, "Resolved"):
			return true

		// The pin is an ARGUMENT here, so it can exempt the call — but only when
		// it is literally `false`.
		case name == "NewFieldPathOfSingleInDomain" || name == "newFieldPathOfSingleInDomain":
			if len(call.Args) == 4 && isLiteralFalse(call.Args[2]) {
				return true
			}
			report(call.Lparen, name+"(..., <pin not provably false>, "+how+")")

		default:
			report(call.Lparen, name+"(..., "+how+")")
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
		// Cheap pre-filter; the parse below is the authority. It must admit every
		// file the scan could report on — the scan now reaches ANY `...InDomain`
		// constructor, so filtering on the two original names would have made the
		// generalization unreachable in exactly the files it was added for.
		if !strings.Contains(string(src), "PinnedOrdinal") &&
			!strings.Contains(string(src), "NewFieldPathOfSingle") &&
			!strings.Contains(string(src), "InDomain") {
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

		// The two shapes that walked through the pin-first version of this gate,
		// plus the variants of each that share their mechanism.
		{
			name: "COMPUTED pin with the unknown token — pin-first missed this entirely",
			body: `func f() { _ = NewFieldPathOfSingleInDomain("K", 1, inPinned, OrdinalDomain{}) }`,
		},
		{
			name: "computed pin from a call, with the unknown token",
			body: `func f() { _ = NewFieldPathOfSingleInDomain("K", 1, rc.Pinned(), values.OrdinalDomain{}) }`,
		},
		{
			name: "empty domain via a PACKAGE-SCOPE variable",
			body: "var emptyDomain = OrdinalDomain{}\n" +
				`func f() { _ = NewFieldValueWithPinnedOrdinalInDomain("K", 1, typ, emptyDomain) }`,
		},
		{
			name: "empty domain via a local short declaration",
			body: `func f() { d := OrdinalDomain{}; _ = NewFieldPathOfSingleInDomain("K", 1, true, d) }`,
		},
		{
			name: "empty domain via a ZERO-VALUE var declaration, no literal written at all",
			body: "var d OrdinalDomain\n" +
				`func f() { _ = NewFieldValueWithPinnedOrdinalInDomain("K", 1, typ, d) }`,
		},
		{
			name: "a pinned mint whose pin is a variable AND whose domain is a variable",
			body: "var d values.OrdinalDomain\n" +
				`func f() { _ = NewFieldPathOfSingleInDomain("K", 1, p, d) }`,
		},
		{
			// The other half of the theDomainLessConstructor exemption. The
			// exemption is by the ENCLOSING FUNCTION'S NAME, so a new helper that
			// forwards the unknown token is a fresh domain-less mint and is
			// caught — otherwise the exemption would be a way to reopen the class
			// by writing one wrapper.
			name: "a NEW helper forwarding the unknown token, byte-identical to the exempt body",
			body: `func mintPinned(f string, i int) *FieldPath { return NewFieldPathOfSingleInDomain(f, i, true, OrdinalDomain{}) }`,
		},
		{
			name: "the REMOVED constructor reintroduced with zero callers, caught by its BODY",
			body: `func NewFieldValueWithPinnedOrdinal(f string, i int, typ Type) *FieldValue { return NewFieldValueWithPinnedOrdinalInDomain(f, i, typ, OrdinalDomain{}) }`,
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
				"from the same type the reference flows. This is the case whose EXISTENCE was " +
				"once used to exempt every computed pin; the domain is what makes it legal",
		},
		{
			name: "an unrelated constructor that merely mentions an ordinal",
			body: `func f() { _ = NewFieldValueWithResolvedOrdinal("K", 1, typ) }`,
			why:  "source-relative ordinals are a different class and are not this gate's business",
		},
		{
			name: "the RESOLVED domain-taking constructor with the unknown token",
			body: `func f() { _ = NewFieldValueWithResolvedOrdinalInDomain("K", 1, typ, OrdinalDomain{}) }`,
			why: "an unpinned ordinal states no claim about a composed row, so an unknown " +
				"token is honest — the domain-first ordering must not swallow the whole " +
				"resolved family along with the pinned one",
		},
		{
			name: "the CORRELATED resolved domain-taking constructor with the unknown token",
			body: `func f() { _ = NewCorrelatedFieldValueWithResolvedOrdinalInDomain(child, "K", 1, typ, OrdinalDomain{}) }`,
			why:  "same family, and its domain sits at a different argument index — matched as LAST, not as [3]",
		},
		{
			name: "the domain-less constructor's OWN body forwarding the unknown token",
			body: `func NewFieldPathOfSingle(field string, ordinal int, frontierPinned bool) *FieldPath {
	return NewFieldPathOfSingleInDomain(field, ordinal, frontierPinned, OrdinalDomain{})
}`,
			why: "forwarding the token is what this function IS, and it is a legal shape " +
				"because an UNPINNED ordinal needs no layout. It is policed at its call " +
				"sites, where the pin becomes a literal. Flagging the definition would make " +
				"the gate's only standing finding be its own premise — see the caught case " +
				"that proves the exemption is by NAME and does not generalize to a wrapper",
		},
		{
			name: "a domain variable REASSIGNED to a derived token",
			body: `func f() { d := OrdinalDomain{}; d = OrdinalDomainOfType(rt); _ = NewFieldPathOfSingleInDomain("K", 1, true, d) }`,
			why: "provably-zero means EVERY binding is the token. One that is later derived " +
				"is not proven, and flagging it would fire the gate on a correct mint — which " +
				"is how a gate earns its deletion",
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

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

// The rule that a FLOAT/DOUBLE coordinate TERMINATES an ordering claim lives in
// exactly one file, and this gate is why that sentence is enforceable rather
// than aspirational.
//
// The rule is not obvious and it is not stable. It currently reads "any float
// coordinate", and it is knowingly conservative: a scanned range with a finite
// LOWER bound cannot reach the negative-NaN block — the block that is physically
// first, logically last, and the one that splits the NaN tie class across two
// disjoint ranges — so such a scan could keep its claim. That refinement is
// unbuilt. When someone builds it, it has to land in the authority file so every
// consumer inherits it at once.
//
// The failure mode this forbids is the tempting one: a consumer that wants the
// optimization adds its OWN float-plus-range check at its own call site. Nothing
// goes red, the two derivations drift, and they classify the same coordinate
// differently — which is the exact defect the shared predicates were introduced
// to remove, and which cost a query its correct row ORDER once already (an
// IN-list binding over a float column kept an ordered merge whose per-binding leg
// was not in primary-key order).
//
// So the gate is structural: a production file that CONSUMES the ordering-claim
// authority may not also hand-roll a float type test. Consuming and re-deciding
// is the drift; consuming alone is the design.
//
// It deliberately does NOT forbid float type tests in general. Coercion,
// arithmetic, encoding and physical-shape code test for floats constantly and
// have nothing to do with ordering claims. The population is narrowed to files
// that already ask the authority, because those are the ones positioned to
// re-answer it.
//
// WHAT GREEN HERE DOES AND DOES NOT PROVE. This gate catches a duplicated claim
// rule EXPRESSED AS A TYPE-CODE TEST — the shape it was derived from, and the
// shape the existing copies take. It does not catch one written any other way: a
// type helper (`t.IsFloat()`), a name match on the column, a switch over some
// other discriminator, or a rule reached through a wrapper. No AST gate is closed
// under paraphrase, and pretending otherwise is worse than the gap, because it
// invites reading a green run as proof that no second copy exists.
//
// So read a pass as "no second copy is written as a type-code test", never as
// "no second copy exists". The claim that the rowdiff ordering axis is covered
// carries the same qualifier: it is covered IF its rule is written that way.
// Where certainty is needed, the check is to read the consumer, not to trust
// this.

// orderingClaimAuthorityFile is where the rule lives. It is the only file
// allowed to both decide and be consulted.
const orderingClaimAuthorityFile = "pkg/recordlayer/query/plan/cascades/values/ordering_claim.go"

// orderingClaimPredicates are the authority's exported entry points. A file
// naming any of them is a CONSUMER for this gate's purposes.
var orderingClaimPredicates = []string{
	"TypeTerminatesOrderingClaim",
	"ColumnCanExtendOrderingClaim",
	"ClaimableOrderingPrefix",
	"ClaimableTypedKeyPrefix",
}

// orderingClaimFloatTestAllowlist are consumer sites that test for a float and
// are NOT a second copy of the claim rule, each with the DIFFERENT question it
// answers. An entry earns its place by answering something the authority does
// not: the authority answers "does this coordinate terminate an ordering
// claim?", and nothing else may.
//
// Keyed by file and by the enclosing function, not by line: a line-keyed
// allowlist silently re-blesses whatever moves onto that line.
var orderingClaimFloatTestAllowlist = map[string]string{
	// The OPERAND question, not the COLUMN question. It asks whether a
	// comparison's operand could be a float so a zero binding might widen the
	// probe — about a value, not about a coordinate's order.
	"pkg/recordlayer/query/plan/plans/ordering.go#couldBeFloatOperand": "operand type, not claim termination",

	// A leaf REPRESENTABILITY question. It asks whether this particular
	// candidate can express an ordered float bound at all (an aggregate index
	// reads a pre-aggregated stream; a partitioned vector index is self-limiting),
	// so the candidate is declined rather than compensated. Nothing to do with
	// whether a claim survives the coordinate.
	"pkg/recordlayer/query/plan/cascades/physical_key_types.go#candidateRangeHasUnsupportedPhysicalFloatOrdering": "leaf representability, not claim termination",

	// A WIRE-ENCODING question, in the executor. It asks whether a scan bound is
	// written through the float tuple carrier, which is what decides that the
	// range set must be built over signed-zero and NaN blocks in the first place.
	// It runs BEFORE and BENEATH any ordering claim: the claim rule reasons about
	// the scan the executor produces, and this decides how that scan is encoded.
	// Structurally it groups both widths exactly as the claim rule does, which is
	// why the shape test cannot separate them and a named exemption is the honest
	// answer rather than a cleverer heuristic.
	"pkg/recordlayer/query/executor/scan_range_binding.go#scanBoundUsesFloatWire": "wire-carrier selection, not claim termination",
}

// orderingClaimConsumerFloor is a vacuity guard. This gate is only meaningful
// if it actually found the consumer population; a refactor that renamed the
// predicates would otherwise leave it scanning nothing and reporting green.
// The floor is deliberately well below the current count so ordinary churn does
// not trip it, while zero-or-near-zero does.
const orderingClaimConsumerFloor = 5

// floatTypeCodeTests reports functions in f that classify a type as FLOAT-KIND
// by testing BOTH TypeCodeFloat and TypeCodeDouble, with the enclosing
// function's name. Both the qualified form (values.TypeCodeFloat) and the bare
// form (inside package values) count; an import alias is still the same
// constant, so the qualifier is not pinned to "values".
//
// Requiring BOTH codes is the discriminator, and it is not a convenience — it
// is what separates the two kinds of float test that exist here:
//
//   - The CLAIM rule always treats the two widths TOGETHER. Its whole content is
//     "this coordinate's tuple key order is not its logical order", which is
//     true of FLOAT and DOUBLE alike; a copy of it necessarily names both.
//   - A WIDTH discriminator necessarily names ONE. The executor's scan-range
//     binding singles out TypeCodeFloat to pick 32-bit wire encoding
//     (floatWireInfinity, scanBoundUsesFloatWire and neighbours). Those are
//     about how a float is written, never about whether it is ordered.
//
// Measured: without this the gate reported six findings, all of them width
// discriminators in one executor file and none of them a claim rule. An
// allowlist of six would have been six standing exemptions papering over a
// detector that was asking the wrong question.
func floatTypeCodeTests(f *ast.File, report func(fn string, pos token.Pos, form string)) {
	isFloatCode := func(e ast.Expr) (string, bool) {
		switch n := e.(type) {
		case *ast.SelectorExpr:
			if n.Sel != nil && (n.Sel.Name == "TypeCodeFloat" || n.Sel.Name == "TypeCodeDouble") {
				return n.Sel.Name, true
			}
		case *ast.Ident:
			if n.Name == "TypeCodeFloat" || n.Name == "TypeCodeDouble" {
				return n.Name, true
			}
		}
		return "", false
	}
	for _, decl := range f.Decls {
		fn, isFn := decl.(*ast.FuncDecl)
		if !isFn || fn.Body == nil {
			continue
		}
		name := fn.Name.Name
		// subtreeCodes reports which float codes appear anywhere under n.
		subtreeCodes := func(n ast.Node) map[string]bool {
			found := map[string]bool{}
			ast.Inspect(n, func(m ast.Node) bool {
				if e, isExpr := m.(ast.Expr); isExpr {
					if which, ok := isFloatCode(e); ok {
						found[which] = true
					}
				}
				return true
			})
			return found
		}
		bothIn := func(n ast.Node) bool {
			c := subtreeCodes(n)
			return c["TypeCodeFloat"] && c["TypeCodeDouble"]
		}
		reported := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if reported {
				return false
			}
			switch e := n.(type) {
			case *ast.BinaryExpr:
				// One boolean expression covering both widths:
				// `c == Float || c == Double`, `c != Float && c != Double`.
				if (e.Op == token.LOR || e.Op == token.LAND) && bothIn(e) {
					report(name, e.OpPos, "one boolean expression grouping both float widths")
					reported = true
					return false
				}
			case *ast.CaseClause:
				// One case arm covering both: `case Float, Double:`.
				var listed int
				for _, v := range e.List {
					if _, ok := isFloatCode(v); ok {
						listed++
					}
				}
				if listed >= 2 {
					report(name, e.Case, "one switch arm grouping both float widths")
					reported = true
					return false
				}
			}
			return true
		})
	}
}

func TestOrderingClaimRuleHasOneAuthority(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)
	var findings []string
	matchedAllowlist := map[string]bool{}
	consumers := 0

	for _, rel := range trackedGoFiles(t, root) {
		slash := filepath.ToSlash(rel)
		if strings.HasSuffix(slash, "_test.go") || strings.HasPrefix(slash, "gen/") {
			continue
		}
		if slash == orderingClaimAuthorityFile {
			continue // the one place allowed to decide
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		text := string(src)
		isConsumer := false
		for _, p := range orderingClaimPredicates {
			if strings.Contains(text, p) {
				isConsumer = true
				break
			}
		}
		if !isConsumer {
			continue // not positioned to re-answer the question
		}
		consumers++
		if !strings.Contains(text, "TypeCodeFloat") && !strings.Contains(text, "TypeCodeDouble") {
			continue // cheap pre-filter; the parse below is the authority
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			continue // not this gate's job to police syntax
		}
		seen := map[string]bool{}
		floatTypeCodeTests(parsed, func(fn string, pos token.Pos, form string) {
			key := slash + "#" + fn
			if _, allowed := orderingClaimFloatTestAllowlist[key]; allowed {
				matchedAllowlist[key] = true
				return
			}
			if seen[key] {
				return // one finding per function, not per operand
			}
			seen[key] = true
			findings = append(findings, fmt.Sprintf("%s:%d: %s in %s",
				slash, fset.Position(pos).Line, form, fn))
		})
	}

	if consumers < orderingClaimConsumerFloor {
		t.Fatalf("found only %d consumer(s) of the ordering-claim authority, want >= %d.\n\n"+
			"This gate scans the files that NAME %v. Finding almost none means the\n"+
			"predicates were renamed or the authority was dissolved, and the gate is now\n"+
			"scanning an empty population and reporting green — which is the failure it\n"+
			"exists to prevent. Re-point orderingClaimPredicates at the current names.",
			consumers, orderingClaimConsumerFloor, orderingClaimPredicates)
	}

	sort.Strings(findings)
	if len(findings) > 0 {
		t.Errorf("%d hand-rolled float test(s) in files that consume the ordering-claim authority:\n%s\n\n"+
			"The rule that a FLOAT/DOUBLE coordinate terminates an ordering claim lives in\n"+
			"%s, and consumers ask it rather than re-deciding it. A second copy is how two\n"+
			"derivations come to classify the same coordinate differently — the defect the\n"+
			"shared predicates removed, which cost an IN-list query its correct row ORDER.\n\n"+
			"This fires most likely because someone built the range-aware refinement (a\n"+
			"finite LOWER bound cannot reach the negative-NaN block, so such a scan could\n"+
			"keep its claim) at a CALL SITE. That refinement is welcome — in the authority\n"+
			"file, where both the planner and the rowdiff ordering axis inherit it at once.\n\n"+
			"If the site genuinely answers a DIFFERENT question, add it to\n"+
			"orderingClaimFloatTestAllowlist keyed by file#function with that question\n"+
			"named — not the fact that it is float-related.",
			len(findings), strings.Join(findings, "\n"), orderingClaimAuthorityFile)
	}

	for key := range orderingClaimFloatTestAllowlist {
		if !matchedAllowlist[key] {
			t.Errorf("stale allowlist entry %q matched nothing.\n\n"+
				"The site was renamed, moved or deleted. A stale entry is a standing\n"+
				"exemption for a function that no longer exists, and it will silently bless\n"+
				"the next function that takes the name. Remove it or re-point it.", key)
		}
	}
}

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

// FieldValue.Field is a DISPLAY name. It must never decide anything.
//
// Seven separate hand-rolled proofs of a semantic property by leaf-name
// comparison went wrong in this codebase, each found by a different route and
// none by the test suite: PushValueThroughFetch, correlatedInnerField,
// correlatedFieldOf, fieldValueAliasAndCol, buriedLegOrdinalLayout,
// rebaseOuterLegValue, and the unique-key proof. They share one shape — two
// columns with the same leaf name are treated as the same column, or the same
// column reached by two paths is treated as two.
//
// The correct inputs already exist. `FieldValue.Resolved` is the
// construction-time resolved accessor (Java's ResolvedAccessor), and
// `SemanticEqualsUnderAliasMap` compares values under a correlation mapping.
// CockroachDB settles this at name-resolution time by assigning a column id the
// optimizer then uses exclusively; `ColumnMeta.Alias` is documented as
// display-only.
//
// Fixing the seven and stopping there guarantees an eighth, so this is the
// build-time gate instead. It fails when `.Field` reaches a DECISION —
// equality, a switch tag, a map key, or a string-comparison helper — anywhere
// outside the allowlist below.
//
// Adding an entry is deliberately annoying: it needs a one-line justification,
// and the reviewer question is always "why can Resolved not answer this?"
type fieldDecisionSite struct {
	file string
	why  string
}

// allowedFieldDecisions are the sites where comparing the display name is
// genuinely correct. Each needs a reason that survives the question above.
var allowedFieldDecisions = []fieldDecisionSite{
	{
		file: "pkg/recordlayer/query/plan/cascades/values/",
		why: "the values package OWNS FieldValue: constructing, resolving and rendering it " +
			"necessarily touches the name. Decisions made ON a resolved value belong here; " +
			"decisions made ELSEWHERE about which column something is do not.",
	},
	{
		file: "pkg/recordlayer/key_expression_proto.go",
		why: "record-layer key expressions are defined BY field name in the metadata proto — " +
			"the name IS the identity at that layer, before any query-side resolution exists.",
	},
	{
		file: "pkg/recordlayer/query/plan/cascades/index_expansion.go",
		why: "expands an index definition, whose columns are likewise named in metadata; the " +
			"name is the input, not a stand-in for a resolved column.",
	},
}

func fieldDecisionAllowed(rel string) bool {
	for _, a := range allowedFieldDecisions {
		if strings.HasPrefix(rel, a.file) {
			return true
		}
	}
	return false
}

// knownFieldDecisionDebt is the surface that EXISTED when this gate was added:
// sites that should consult Resolved and do not yet. It is a RATCHET, not an
// exemption — the test fails if a new site appears, and it also fails if an
// entry here stops matching, so fixing one forces deleting its line rather than
// letting the list rot into a permanent allowlist.
//
// Deliberately NOT merged with allowedFieldDecisions above. That list says "the
// name is the identity at this layer" and is expected to stay. This one says
// "this is wrong and not yet migrated" and is expected to reach zero. Collapsing
// them would erase exactly the distinction that matters.
//
// Recorded as file:line at the moment of writing. Line drift makes an entry
// stale, which fails loudly — annoying by design, since a stale entry means
// nobody checked whether the site still needs it.
//
// Three flavors are represented, and they are not equally bad:
//
//   - COMPARISON: two names checked against each other. The original seven bugs.
//   - ESCAPE: the name returned as a bare string, so the caller decides with no
//     type left to consult. The `correlatedInnerField` shape.
//   - DOTTED-NAME PROBE: `strings.Contains(fv.Field, ".")` asking whether a
//     reference is qualified. Structure encoded in a string; the flat "ALIAS.col"
//     representation is the actual debt, and these sites are its readers.
var knownFieldDecisionDebt = map[string]string{
	"pkg/recordlayer/query/plan/cascades/match_candidate_index.go:239":            "index column name vs query field",
	"pkg/recordlayer/query/plan/cascades/expressions/group_by.go:118":             "escape: THE group-key output-naming authority — the name IS the contract with the executor, so this moves only when the contract becomes an ordinal",
	"pkg/recordlayer/query/plan/cascades/fk_chain_cardinality.go:394":             "escape: leafFieldName, guarded to a flat non-nested PK column but still name-keyed downstream",
	"pkg/recordlayer/query/plan/cascades/fk_chain_cardinality.go:421":             "escape: same, after a Resolved.Single() guard",
	"pkg/recordlayer/query/plan/cascades/left_outer_existential.go:112":           "dotted-name probe: leg-relative vs qualified ref",
	"pkg/recordlayer/query/plan/cascades/rule_implement_nested_loop_join.go:2337": "dotted-name probe: declines re-qualifying an already-dotted ref",
	"pkg/recordlayer/query/plan/cascades/rule_implement_nested_loop_join.go:3691": "escape: (alias, column) pair after a Resolved.Single() guard",
	"pkg/recordlayer/query/plan/cascades/rule_implement_nested_loop_join.go:3735": "escape: bareColumnName, the flat-string alias-stripping path",
	"pkg/recordlayer/query/plan/cascades/rule_implement_nested_loop_join.go:3741": "escape: same function's fallback arm",
	"pkg/recordlayer/query/plan/plans/cost.go:643":                                "escape: correlatedInnerField — the shape this gate is named after. Guarded by Resolved.Single() and a flat-QOV child, so the name is unambiguous TODAY; the caller still keys want[]/bound[] by it",
	"pkg/relational/core/embedded/logical_predicate.go:6093":                      "escape: aggregate group-key output name, SQL translator layer",
	"pkg/recordlayer/query/plan/cascades/referenced_fields.go:125":                "referenced-field set keyed by name",
	"pkg/recordlayer/query/plan/cascades/rule_implement_distinct_final.go:197":    "distinct-key set keyed by name",
	"pkg/recordlayer/query/plan/cascades/rule_projection_merge.go:113":            "projection merge matches by name",
	"pkg/recordlayer/query/plan/plans/in_memory_sort.go:142":                      "sort key matched by name",
	"pkg/relational/conformance/rowdiff/ordering.go:241":                          "oracle-side ordering, harness not engine",
	"pkg/relational/core/embedded/cascades_generator.go:2264":                     "SQL translator layer",
	"pkg/relational/core/embedded/cascades_generator.go:3155":                     "SQL translator layer",
	"pkg/relational/core/embedded/cascades_generator.go:3281":                     "SQL translator layer",
	"pkg/relational/core/embedded/cascades_generator.go:3287":                     "SQL translator layer",
	"pkg/relational/core/embedded/logical_predicate.go:4151":                      "SQL translator layer",
	"pkg/relational/core/embedded/logical_predicate.go:6188":                      "SQL translator layer",
	"pkg/relational/core/query/box_conjunct.go:149":                               "dotted-name probe: frontier read attribution, SQL translator layer",
	"pkg/relational/core/query/ordinal_seed.go:761":                               "dotted-name probe: leg-ref detection, SQL translator layer",
	"pkg/relational/core/query/cascades_translator.go:2093":                       "SQL translator layer",
	"pkg/relational/core/query/cascades_translator.go:4748":                       "escape: sort-key field ref, SQL translator layer",
	"pkg/relational/core/query/cascades_translator.go:5048":                       "SQL translator layer",
	"pkg/relational/core/query/cascades_translator.go:5879":                       "SQL translator layer",
	"pkg/relational/core/query/cascades_translator.go:6096":                       "SQL translator layer",
	"pkg/relational/core/query/cascades_translator.go:6104":                       "SQL translator layer",
}

// isFieldSelector reports whether e reads `.Field` off something.
func isFieldSelector(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel != nil && sel.Sel.Name == "Field"
}

// stringCompareHelpers are calls whose result is a name equality/ordering
// decision. Matched on the function's own identifier, so BOTH `strings.EqualFold(…)`
// (a SelectorExpr) and a bare or generic call like `slices.Contains(names, …)`
// are covered — an earlier version only matched method-selector calls and
// therefore missed every package-level generic helper.
var stringCompareHelpers = map[string]bool{
	"EqualFold": true, "Compare": true, "HasPrefix": true, "HasSuffix": true,
	"Contains": true, "Index": true, "SearchStrings": true, "ContainsFunc": true,
	"IndexFunc": true, "Equal": true,
}

// callFuncName returns the identifier a call expression invokes, for either
// `pkg.Fn(…)` / `x.Method(…)` (SelectorExpr) or a bare `Fn(…)` (Ident).
func callFuncName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		if f.Sel != nil {
			return f.Sel.Name
		}
	case *ast.Ident:
		return f.Name
	case *ast.IndexExpr: // explicit instantiation: slices.Contains[string](…)
		return callFuncName(f.X)
	case *ast.IndexListExpr: // …with more than one type argument
		return callFuncName(f.X)
	}
	return ""
}

// nameLaunderers are string->string calls that pass the leaf name through
// unchanged in every way that matters for identity. `strings.ToUpper(fv.Field)`
// escapes exactly as much as `fv.Field` does. A CONSTRUCTOR taking the name is
// deliberately not here: building a FieldValue from a name is what the values
// package is for, and flagging it would flag the correct code.
var nameLaunderers = map[string]bool{
	"ToUpper": true, "ToLower": true, "TrimSpace": true, "Clone": true,
	"TrimPrefix": true, "TrimSuffix": true, "Title": true,
}

// funcTouchesFieldValue reports whether fn names the type *values.FieldValue
// anywhere — a type assertion, a type-switch case, a parameter, a var decl.
//
// This is the discriminator the gate needs and cannot get from syntax alone:
// `.Field` is a common struct-field name. `UnresolvableOrdinalError.Field`,
// `CorrelatedShadowError.Field` and `plans.SortKey.Field` are all display
// strings on unrelated types, and flagging their Error() methods would bury the
// real signal under noise the reader learns to scroll past. Full type
// information would answer this exactly, but it would mean loading and
// type-checking the whole tree from a test; naming the type in the same
// function is the cheap approximation, and it errs toward silence rather than
// toward a gate nobody trusts.
func funcTouchesFieldValue(fn ast.Node) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		if found {
			return false
		}
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel != nil && sel.Sel.Name == "FieldValue" {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "values" {
				found = true
			}
		}
		return !found
	})
	return found
}

// isOrderingOp reports whether op orders two values. Sorting BY leaf name is
// leaf-name-as-identity exactly as much as comparing by it — a
// `sort.Slice(cols, func(i,j int) bool { return cols[i].Field < cols[j].Field })`
// reintroduces the same conflation and was previously unchecked.
func isOrderingOp(op string) bool {
	switch op {
	case "<", ">", "<=", ">=":
		return true
	}
	return false
}

// scanFieldDecisions walks one parsed file and calls report for every site
// where FieldValue.Field reaches a decision. Split out of the tree walk so the
// detector itself is testable against synthetic source — a gate whose RECALL is
// never exercised is indistinguishable from a gate that matches nothing, and
// the first version of this one silently missed the function it was named for.
func scanFieldDecisions(f *ast.File, report func(pos token.Pos, form string)) {
	// Whether the enclosing top-level func names *values.FieldValue, tracked
	// so a closure inherits its parent's answer.
	handlesFieldValue := false

	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			handlesFieldValue = funcTouchesFieldValue(x)
		case *ast.BinaryExpr:
			op := x.Op.String()
			if op == "==" || op == "!=" || isOrderingOp(op) {
				if isFieldSelector(x.X) || isFieldSelector(x.Y) {
					report(x.Pos(), "a "+op+" comparison")
				}
			}
		case *ast.SwitchStmt:
			// Switching on a display name is equality N times over. (An
			// EMPTY-tag switch needs no arm here: ast.Inspect still visits
			// each case's boolean expression as an ordinary BinaryExpr.)
			if x.Tag != nil && isFieldSelector(x.Tag) {
				report(x.Pos(), "a switch tag")
			}
		case *ast.IndexExpr:
			// Keying a map by display name conflates same-named columns.
			if isFieldSelector(x.Index) {
				report(x.Pos(), "a map key")
			}
		case *ast.KeyValueExpr:
			// map[string]T{fv.Field: …} builds the same conflation through
			// a composite literal, which never produces an IndexExpr.
			if isFieldSelector(x.Key) {
				report(x.Pos(), "a composite-literal key")
			}
		case *ast.ReturnStmt:
			// The name ESCAPING as a bare string is the shape that defeated
			// the first version of this gate, and it defeated it in the very
			// function the gate was named after: `correlatedInnerField`
			// returns fv.Field, and its caller then writes `want[field]`. By
			// the caller the AST node is an Ident, not a selector, so every
			// check downstream is blind. Catching the RETURN catches it while
			// the type is still visible.
			if !handlesFieldValue {
				break
			}
			for _, r := range x.Results {
				if isFieldSelector(r) {
					report(x.Pos(), "the name escaping as a bare string (return)")
					break
				}
				// strings.ToUpper(fv.Field) launders it without changing it.
				if call, ok := r.(*ast.CallExpr); ok && nameLaunderers[callFuncName(call.Fun)] {
					for _, arg := range call.Args {
						if isFieldSelector(arg) {
							report(x.Pos(), "the name escaping as a bare string (return)")
							break
						}
					}
				}
			}
		case *ast.CallExpr:
			if name := callFuncName(x.Fun); stringCompareHelpers[name] {
				for _, arg := range x.Args {
					if isFieldSelector(arg) {
						report(x.Pos(), "a "+name+" call")
						break
					}
				}
			}
		}
		return true
	})
}

func TestFieldNameNeverDecides(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	var offenses []string
	var scanned int
	seenDebt := map[string]bool{}

	for _, rel := range trackedGoFiles(t, root) {
		if strings.HasSuffix(rel, "_test.go") || fieldDecisionAllowed(rel) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("read %s: %v", rel, err)
			continue
		}
		if isGeneratedFile(src, nil) {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			t.Errorf("parse %s: %v", rel, err)
			continue
		}
		if isGeneratedFile(src, f) {
			continue
		}
		scanned++

		scanFieldDecisions(f, func(pos token.Pos, form string) {
			p := fset.Position(pos)
			key := fmt.Sprintf("%s:%d", rel, p.Line)
			if _, known := knownFieldDecisionDebt[key]; known {
				seenDebt[key] = true
				return
			}
			offenses = append(offenses,
				fmt.Sprintf("%s: %s uses FieldValue.Field", key, form))
		})
	}

	if scanned == 0 {
		t.Fatal("scanned no files — the walk is broken, so a green result proves nothing")
	}

	// Self-cleaning: a debt entry that no longer matches means the site moved or
	// was fixed. Either way the line must go, or the list silently becomes a
	// permanent allowlist pointing at code that has changed underneath it.
	var stale []string
	for key := range knownFieldDecisionDebt {
		if !seenDebt[key] {
			stale = append(stale, key)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("knownFieldDecisionDebt has %d entry/entries that no longer match a "+
			"FieldValue.Field decision:\n  %s\n\nIf you FIXED the site, delete its line — the "+
			"debt list only earns its keep by shrinking. If the line merely MOVED, update it and "+
			"check whether Resolved can answer it now while you are there.",
			len(stale), strings.Join(stale, "\n  "))
	}

	if len(offenses) > 0 {
		sort.Strings(offenses)
		t.Fatalf("FieldValue.Field is a DISPLAY name and must not decide anything.\n\n%s\n\n"+
			"Seven wrong proofs in this codebase came from comparing leaf names: two columns "+
			"with the same name treated as one, or one column reached two ways treated as two. "+
			"None were caught by the suite.\n\n"+
			"Use FieldValue.Resolved (the construction-time resolved accessor) or "+
			"SemanticEqualsUnderAliasMap (comparison under a correlation mapping) instead. "+
			"CockroachDB assigns a column id at name resolution and its optimizer never sees a "+
			"name again.\n\n"+
			"If comparing the NAME is genuinely right here — because the name is the identity at "+
			"that layer, as in metadata key expressions — add the file to allowedFieldDecisions "+
			"with a reason that answers: why can Resolved not answer this?\n\n"+
			"scanned %d files", strings.Join(offenses, "\n"), scanned)
	}
	t.Logf("no FieldValue.Field decisions outside the allowlist (%d files scanned)", scanned)
}

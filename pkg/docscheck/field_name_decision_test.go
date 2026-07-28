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
var knownFieldDecisionDebt = map[string]string{
	"pkg/recordlayer/query/plan/cascades/match_candidate_index.go:239":         "index column name vs query field",
	"pkg/recordlayer/query/plan/cascades/referenced_fields.go:125":             "referenced-field set keyed by name",
	"pkg/recordlayer/query/plan/cascades/rule_implement_distinct_final.go:197": "distinct-key set keyed by name",
	"pkg/recordlayer/query/plan/cascades/rule_projection_merge.go:113":         "projection merge matches by name",
	"pkg/recordlayer/query/plan/plans/in_memory_sort.go:142":                   "sort key matched by name",
	"pkg/relational/conformance/rowdiff/ordering.go:241":                       "oracle-side ordering, harness not engine",
	"pkg/relational/core/embedded/cascades_generator.go:2264":                  "SQL translator layer",
	"pkg/relational/core/embedded/cascades_generator.go:3155":                  "SQL translator layer",
	"pkg/relational/core/embedded/cascades_generator.go:3281":                  "SQL translator layer",
	"pkg/relational/core/embedded/cascades_generator.go:3287":                  "SQL translator layer",
	"pkg/relational/core/embedded/logical_predicate.go:4151":                   "SQL translator layer",
	"pkg/relational/core/embedded/logical_predicate.go:6188":                   "SQL translator layer",
	"pkg/relational/core/query/cascades_translator.go:2093":                    "SQL translator layer",
	"pkg/relational/core/query/cascades_translator.go:5048":                    "SQL translator layer",
	"pkg/relational/core/query/cascades_translator.go:5879":                    "SQL translator layer",
	"pkg/relational/core/query/cascades_translator.go:6096":                    "SQL translator layer",
	"pkg/relational/core/query/cascades_translator.go:6104":                    "SQL translator layer",
}

// isFieldSelector reports whether e reads `.Field` off something.
func isFieldSelector(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel != nil && sel.Sel.Name == "Field"
}

// stringCompareHelpers are calls whose result is a name equality decision.
var stringCompareHelpers = map[string]bool{
	"EqualFold": true, "Compare": true, "HasPrefix": true, "HasSuffix": true,
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

		report := func(pos token.Pos, form string) {
			p := fset.Position(pos)
			key := fmt.Sprintf("%s:%d", rel, p.Line)
			if _, known := knownFieldDecisionDebt[key]; known {
				seenDebt[key] = true
				return
			}
			offenses = append(offenses,
				fmt.Sprintf("%s: %s uses FieldValue.Field", key, form))
		}

		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BinaryExpr:
				// Equality on a display name.
				if x.Op.String() == "==" || x.Op.String() == "!=" {
					if isFieldSelector(x.X) || isFieldSelector(x.Y) {
						report(x.Pos(), "an equality comparison")
					}
				}
			case *ast.SwitchStmt:
				// Switching on a display name is equality N times over.
				if x.Tag != nil && isFieldSelector(x.Tag) {
					report(x.Pos(), "a switch tag")
				}
			case *ast.IndexExpr:
				// Keying a map by display name conflates same-named columns.
				if isFieldSelector(x.Index) {
					report(x.Pos(), "a map key")
				}
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok &&
					sel.Sel != nil && stringCompareHelpers[sel.Sel.Name] {
					for _, arg := range x.Args {
						if isFieldSelector(arg) {
							report(x.Pos(), "a "+sel.Sel.Name+" call")
							break
						}
					}
				}
			}
			return true
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

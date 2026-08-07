package docscheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// THE `GetResultType() == UnknownType` STUB INVENTORY (RFC-213).
//
// Java has no stub. `RelationalExpression.getResultType()` is a DEFAULT method
// deriving `new Type.Relation(getResultValue().getResultType())`
// (RelationalExpression.java:195-196), and it is the ONLY definition of that
// method in the entire Java tree — no plan overrides it, RecordQueryFlatMapPlan
// included. It cannot return unknown because `Value getResultValue()` is
// ABSTRACT on the same interface (`:200`): every relational expression is
// required to have a result value, so the type is always derivable.
//
// GO INVERTED THAT DEPENDENCY, and every symptom follows from the inversion.
// `plans.RecordQueryPlan` requires `GetResultType()` (plan.go:70) and does NOT
// require `GetResultValue()`; only some plans implement one voluntarily. With no
// mandatory source to derive from, plans that cannot answer return the
// UnknownType singleton — and the count is not the one CQ-97 booked. It booked
// `RecordQueryFlatMapPlan`, mentioning `RecordQueryNestedLoopJoinPlan` in
// passing. Measured by this gate, it is TWELVE.
//
// WHY THIS IS AN INVENTORY AND NOT A ZERO. The stub is not a bug that can be
// deleted today: four of the twelve have neither a result value nor an inner to
// derive from, so a zero here would be an assertion nobody can satisfy, and this
// file's siblings record at length what happens when a census asserts a wish.
// It is a DEBT LIST, and it earns its keep by shrinking — RFC-213's
// implementation removes entries, and each removal comes here as a deliberate
// edit rather than as a silent improvement nobody can point at.
//
// BOTH DIRECTIONS FAIL, and the growth direction is the one that matters:
//
//   - A THIRTEENTH stub means a new plan joined the divergence silently. Every
//     consumer that type-asserts the result — measured at 28 call sites, all
//     failing CLOSED — starts declining on that plan's rows too, and declining
//     is invisible: it costs an optimization, never a wrong answer, so nothing
//     else in the suite goes red.
//   - A stub DISAPPEARING is good news that still fails, because a shrinking
//     list cannot be told from a renamed plan. Delete the entry and say which.
func stubInventory() map[string]string {
	return map[string]string{
		// Tier 1 — HAVE a result value. Java's derivation applies verbatim, and
		// these are RFC-213's first phase.
		"RecordQueryFlatMapPlan":              "has GetResultValue; Java derives from it (RelationalExpression.java:195)",
		"RecordQueryNestedLoopJoinPlan":       "has GetResultValue",
		"RecordQueryStreamingAggregationPlan": "has GetResultValue",
		"RecordQueryTempTableScanPlan":        "has GetResultValue",
		"RecordQueryValuesPlan":               "has GetResultValue",

		// Tier 2 — no result value, but a pass-through with an inner. These change
		// no row shape at all, so forwarding is the whole fix. RecordQueryLimitPlan
		// is the sharpest case in the file: a LIMIT cannot alter a type, its inner
		// is one call away, and it still answers unknown.
		"RecordQueryLimitPlan":           "pure pass-through; has GetInner",
		"RecordQueryProjectionPlan":      "has GetProjections + GetInner; distinctKeyColumns already special-cases this plan to route around the stub",
		"RecordQueryTempTableInsertPlan": "has GetInner",

		// Tier 3 — neither a result value nor an inner. These are why the gate is
		// an inventory rather than a zero: closing them needs a type from
		// somewhere that does not exist yet, and RFC-213 argues each one.
		"RecordQueryLoadByKeysPlan":          "no result value, no inner",
		"RecordQueryRecursiveDfsJoinPlan":    "no result value, no inner",
		"RecordQueryRecursiveLevelUnionPlan": "no result value, no inner",
		"RecordQueryTextIndexPlan":           "no result value, no inner",
	}
}

// planPkgDir is where the plan implementations live.
const planPkgDir = "pkg/recordlayer/query/plan/plans"

// findStubs returns every plan whose GetResultType body is exactly
// `return values.UnknownType`.
//
// The match is STRUCTURAL — a single-statement body returning a selector named
// UnknownType — rather than a grep for the identifier. That distinction is
// load-bearing: 22 further plans mention UnknownType inside GetResultType as a
// NIL-GUARD fallback before forwarding to their inner (`if inner == nil { return
// values.UnknownType }; return inner.GetResultType()`), and those are correct
// forwarders, not stubs. A textual gate would report 34 and describe neither
// population.
func findStubs(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	dir := filepath.Join(root, planPkgDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != "GetResultType" || fd.Body == nil || fd.Recv == nil || len(fd.Recv.List) == 0 {
				continue
			}
			if len(fd.Body.List) != 1 {
				continue
			}
			ret, ok := fd.Body.List[0].(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				continue
			}
			sel, ok := ret.Results[0].(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "UnknownType" {
				continue
			}
			star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			id, ok := star.X.(*ast.Ident)
			if !ok {
				continue
			}
			out[id.Name] = e.Name()
		}
	}
	return out
}

func TestResultTypeStubInventoryIsCurrent(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	found := findStubs(t, root)
	want := stubInventory()

	// A structural gate that matched nothing would pass vacuously while the whole
	// divergence stood, and this one navigates an AST shape to find its
	// population — exactly where a silent miss hides.
	if len(found) == 0 {
		t.Fatal("found ZERO plans returning `values.UnknownType` unconditionally. Either the " +
			"divergence closed entirely — in which case delete this gate and say so in " +
			"RFC-213 — or findStubs stopped matching the AST shape it looks for and is now " +
			"asserting nothing.")
	}

	var added, removed []string
	for name, file := range found {
		if _, known := want[name]; !known {
			added = append(added, name+" ("+file+")")
		}
	}
	for name := range want {
		if _, still := found[name]; !still {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)

	if len(added) != 0 {
		t.Fatalf("%d plan(s) newly return `values.UnknownType` unconditionally and are not in "+
			"the RFC-213 inventory:\n    %s\n"+
			"  A NEW STUB IS INVISIBLE WITHOUT THIS GATE, which is why it is checked rather "+
			"than counted. All 28 consumers that read a result type fail CLOSED on unknown — "+
			"they type-assert `*values.RecordType` and decline — so a plan joining this list "+
			"costs optimizations on its rows and turns NOTHING ELSE red.\n"+
			"  Java cannot express this state at all: `getResultValue()` is abstract on "+
			"RelationalExpression (:200) and `getResultType()` is a default derived from it "+
			"(:195-196), so every expression states its row.\n"+
			"  If the new plan genuinely has nothing to derive from, add it to tier 3 with "+
			"that reason. Do not add it silently.",
			len(added), strings.Join(added, "\n    "))
	}

	if len(removed) != 0 {
		t.Fatalf("%d inventory entry/entries no longer stub `GetResultType`:\n    %s\n"+
			"  This is the GOOD direction and it still fails, because a shrinking list cannot "+
			"be told from a renamed plan. If RFC-213 closed these, delete the entries and say "+
			"which phase did it — the list earns its keep by shrinking deliberately.",
			len(removed), strings.Join(removed, "\n    "))
	}
}

// The interface inversion RFC-213 exists to correct.
//
// This is the ROOT of the divergence and it is one line in each language. Java's
// RelationalExpression requires the VALUE and derives the TYPE. Go's
// RecordQueryPlan requires the TYPE and requires no value, so the type has no
// mandatory source and twelve plans answer unknown.
//
// Pinned because RFC-213's whole design rests on it, and because it is the kind
// of precondition that gets quietly satisfied by someone else's change: the day
// `GetResultValue()` joins the interface, this gate goes red and whoever did it
// finds out they have landed RFC-213's phase 0 — which is a good outcome, and a
// far better one than the RFC silently describing a tree that no longer exists.
func TestRecordQueryPlanStillDoesNotRequireGetResultValue(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	const rel = planPkgDir + "/plan.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}

	var hasResultType, hasResultValue, foundIface bool
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name == nil || ts.Name.Name != "RecordQueryPlan" {
			return true
		}
		iface, ok := ts.Type.(*ast.InterfaceType)
		if !ok {
			return true
		}
		foundIface = true
		for _, m := range iface.Methods.List {
			for _, nm := range m.Names {
				switch nm.Name {
				case "GetResultType":
					hasResultType = true
				case "GetResultValue":
					hasResultValue = true
				}
			}
		}
		return false
	})

	if !foundIface {
		t.Fatalf("%s: interface RecordQueryPlan not found — this gate names it by hand, so a "+
			"rename must come here too rather than silently retiring the check", rel)
	}
	if !hasResultType {
		t.Fatalf("%s: RecordQueryPlan no longer requires GetResultType. That is a bigger change "+
			"than RFC-213 describes; re-read the RFC against the tree before trusting it", rel)
	}
	if hasResultValue {
		t.Fatalf("%s: RecordQueryPlan now REQUIRES GetResultValue.\n"+
			"  That is RFC-213's phase 0 and it inverts the dependency the whole RFC is about: "+
			"Java requires the VALUE (RelationalExpression.java:200, abstract) and DERIVES the "+
			"type from it (:195-196); Go required the TYPE and left the value optional, which "+
			"is why twelve plans answer UnknownType.\n"+
			"  If this was deliberate, RFC-213 needs updating and the stub inventory above "+
			"should be shrinking in the same change. If it was incidental, it is a much larger "+
			"change than its author probably intended.", rel)
	}
}

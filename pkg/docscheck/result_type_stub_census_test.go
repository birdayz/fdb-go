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

// THE `GetResultType() == UnknownType` STUB INVENTORY (RFC-213).
//
// JAVA HAS A STUB TOO — IT JUST PUTS IT SOMEWHERE ELSE, and getting that
// backwards is what RFC-213 rev 1 was NAK'd for. This header carried the wrong
// version until rev 2; it is corrected here rather than quietly deleted, because
// an instrument that still argues a withdrawn premise is the thing the RFC's own
// §8 asks about.
//
// Both ends of Java's chain are DEFAULTS, and the terminal one is a SENTINEL:
//
//	RelationalExpression.java:194-196   default Type.Relation getResultType()
//	                                     -> new Type.Relation(getResultValue().getResultType())
//	values/Value.java:107-111           default Type getResultType()
//	                                     -> Type.primitiveType(Type.TypeCode.UNKNOWN)
//
// TypeCode.UNKNOWN is a first-class constant (typing/Type.java:774) and
// Type.isUnresolved() (`:298-300`) is exactly that check — Java has an erased
// type AND a predicate for asking about it. It is REACHED in production:
// values/EmptyValue.java declares no override, so it answers UNKNOWN, and it is
// planted by KeyExpressionExpansionVisitor.java:124,
// ScalarTranslationVisitor.java:116 and AggregateIndexExpansionVisitor.java:238.
//
// SCOPED, because the unscoped version was the other withdrawn claim: within the
// `cascades/expressions` RelationalExpression hierarchy, `:195` is the sole
// definition and RecordQueryFlatMapPlan overrides nothing. Tree-wide there are
// 60 definitions of getResultType (50 on Value, one on typing/Typed.java:38
// which `:194` overrides with a covariant return, plus Reference, InSource and
// four on relational QueryPlan/CopyPlan).
//
// SO THE DIFFERENCE IS WHERE THE SENTINEL LIVES, NOT WHETHER ONE EXISTS. Java
// puts it at the VALUE level, names it (EmptyValue) and gives it a predicate; a
// plan that flows no row SAYS SO. Go puts it at the PLAN level, unnamed, as a
// method that opts out — indistinguishable from an unfinished one, which is how
// twelve of them accumulated without anyone deciding to.
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

// THE CONSUMER CLASSIFIER (RFC-213 §3).
//
// The stub inventory above watches the PRODUCERS. This watches the CONSUMERS,
// and it is the more dangerous axis: RFC-213's claim that the divergence is not
// a wrong-results bug rests entirely on every consumer of a result type failing
// CLOSED on an unresolved one. That was established by reading 28 sites. Reading
// does not survive the 29th.
//
// A consumer that fails closed declines an optimization — invisible, because a
// worse plan is not red. A consumer that reads an unresolved type RAW gets a
// `*PrimitiveType` where it expected a row and is a wrong-results bug. Nothing
// else in the suite distinguishes those two, which is exactly why the classifier
// is committed rather than the count being quoted.
//
// FORWARD vs DECIDE is STRUCTURAL, not correlated: a forward is the call being
// the sole operand of a `return` inside a `GetResultType` method — that is the
// definition of pass-through, not a proxy for it. Everything else consumes the
// value somehow and is a DECIDE.
type resultTypeSite struct {
	file, fn, kind string
	line           int
}

// classifyResultTypeSites walks the non-test tree and classifies every call to
// `.GetResultType()`.
//
// GUARDED  the result is immediately type-asserted or type-switched, so an
//
//	unresolved type takes the else branch — fail-closed by construction.
//
// PROPAGATED the result is handed to a constructor, returned, or stored without
//
//	being branched on here. It cannot decide wrongly at THIS site; it
//	carries the sentinel onward, which is a separate concern (§3).
//
// RAW      anything else: the value is used directly. This class must be EMPTY.
func classifyResultTypeSites(t *testing.T, root string) []resultTypeSite {
	t.Helper()
	var out []resultTypeSite
	fset := token.NewFileSet()
	err := filepath.Walk(filepath.Join(root, "pkg"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if perr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				return false
			}
			stack = append(stack, n)
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "GetResultType" {
				return true
			}
			out = append(out, resultTypeSite{
				file: rel, line: fset.Position(call.Pos()).Line,
				fn:   enclosingFuncName(stack),
				kind: classifyOneSite(stack, call),
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

func enclosingFuncName(stack []ast.Node) string {
	for i := len(stack) - 1; i >= 0; i-- {
		if fd, ok := stack[i].(*ast.FuncDecl); ok {
			return fd.Name.Name
		}
	}
	return "(closure)"
}

func classifyOneSite(stack []ast.Node, call *ast.CallExpr) string {
	// Immediate parent decides GUARDED; a TypeAssertExpr or a TypeSwitch on the
	// call is a fail-closed read.
	for i := len(stack) - 2; i >= 0; i-- {
		switch p := stack[i].(type) {
		case *ast.TypeAssertExpr:
			if p.X == call {
				return "GUARDED"
			}
		case *ast.TypeSwitchStmt:
			return "GUARDED"
		case *ast.ReturnStmt:
			if len(p.Results) == 1 && p.Results[0] == call {
				for j := i; j >= 0; j-- {
					if fd, ok := stack[j].(*ast.FuncDecl); ok {
						if fd.Name.Name == "GetResultType" {
							return "FORWARD"
						}
						break
					}
				}
			}
			return "PROPAGATED"
		case *ast.CallExpr:
			// A call whose ONLY consumer is the RFC-213 payoff census is not a
			// consumer of the result type — it is the instrument measuring the
			// consumers. Counting it would let the census inflate its own
			// denominator, which is the "summed denominator, true by construction"
			// failure this file's siblings record at length.
			if s, ok := p.Fun.(*ast.Ident); ok && s.Name == "recordResultTypeRead" {
				return "CENSUS"
			}
			if p != call {
				return "PROPAGATED" // an argument to something else
			}
		case *ast.KeyValueExpr, *ast.AssignStmt:
			// Assigned or used as a struct field value: propagated unless a later
			// assertion catches it, which the GUARDED arms above already saw.
			return "PROPAGATED"
		}
	}
	return "RAW"
}

// TestResultTypeConsumersFailClosed is RFC-213 §3's committed basis.
func TestResultTypeConsumersFailClosed(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)
	sites := classifyResultTypeSites(t, root)

	counts := map[string]int{}
	var raw []string
	for _, s := range sites {
		if s.kind == "CENSUS" {
			continue
		}
		counts[s.kind]++
		if s.kind == "RAW" {
			raw = append(raw, fmt.Sprintf("%s:%d in %s", s.file, s.line, s.fn))
		}
	}
	sort.Strings(raw)

	if len(sites) == 0 {
		t.Fatal("classified ZERO GetResultType call sites. The walk found no population, so " +
			"every assertion below is vacuous — check classifyResultTypeSites against the tree.")
	}

	// THE LOAD-BEARING ASSERTION. RFC-213 is an RFC and not a bug report because
	// no consumer reads an unresolved result type raw.
	if len(raw) != 0 {
		t.Fatalf("%d GetResultType consumer(s) read the result RAW — neither type-asserted nor "+
			"propagated:\n    %s\n"+
			"  RFC-213 §3 rests on every consumer failing CLOSED on an unresolved type. Twelve "+
			"plans return values.UnknownType (a *PrimitiveType), and a raw read gets that where "+
			"it expected a row.\n"+
			"  THIS IS THE LINE BETWEEN AN RFC AND A BUG. A fail-closed consumer declines an "+
			"optimization, which is invisible because a worse plan is not red. A raw consumer "+
			"returns wrong data. If this fired, RFC-213's framing is wrong and the site above "+
			"is a live defect to fix NOW, ahead of the RFC.",
			len(raw), strings.Join(raw, "\n    "))
	}

	// The split is an inventory: it moves only by a deliberate edit, so a new
	// consumer arrives as a decision rather than as drift.
	// MEASURED, not guessed: the first draft of this line said 8/20 from reading and
	// the classifier corrected it. GUARDED means asserted AT THE CALL SITE; a site
	// that assigns and guards on the NEXT line — planning_cost_model.go:2552 does
	// exactly that, via OrdinalDomainOfType(layout).IsKnown() — reads as PROPAGATED.
	// This classifier is per-site syntax and deliberately does no dataflow, so
	// PROPAGATED means "not decided HERE", never "unguarded".
	// FORWARD moved 20 -> 21 for RFC-220's `plans.RecordQueryCoveringIndexPlan`
	// (`covering_index_scan.go`), which forwards its inner index plan's result
	// type unchanged. It is a FORWARD rather than a GUARDED site by design: the
	// covering plan is a wrapper whose result type IS the inner's, so asserting
	// on it here would duplicate the inner scan's own guard rather than add one.
	//
	// PROPAGATED moved 21 -> 22 for RFC-220's `makeStrictlySorted`
	// (`rule_implement_sort.go`), which rebuilds a Fetch over a strictly-sorted
	// COVERING scan and passes the fetch's own result type straight through
	// (`fw.GetResultType()`). It is PROPAGATED and not GUARDED deliberately: the
	// rebuild preserves the fetch's type verbatim rather than deciding anything
	// about it, so a guard here would assert on a type this site never inspects.
	const wantForward, wantGuarded, wantPropagated = 21, 7, 22
	if counts["FORWARD"] != wantForward || counts["GUARDED"] != wantGuarded || counts["PROPAGATED"] != wantPropagated {
		t.Fatalf("consumer split moved: FORWARD=%d (want %d) GUARDED=%d (want %d) "+
			"PROPAGATED=%d (want %d), total %d.\n"+
			"  These are RFC-213 §3's numbers. A change here is a real change to the consumer "+
			"population and the RFC's blast-radius argument is stated against them — update "+
			"both together, and say which site moved and why.",
			counts["FORWARD"], wantForward, counts["GUARDED"], wantGuarded,
			counts["PROPAGATED"], wantPropagated, len(sites))
	}
}

// A STUB CAN BE CREATED AT THE CALL SITE, WHERE NO METHOD-BODY GATE CAN SEE IT.
//
// RecordQueryAggregateIndexPlan's GetResultType returns a stored field, so the
// inventory above correctly does not list it — and yet every aggregate index
// plan the planner builds carries values.UnknownType, because all three
// production callers pass the singleton EXPLICITLY and the constructor also
// defaults nil to it. It is a thirteenth stub, made one layer up.
//
// This gate exists because the first version of RFC-213 asserted these plans
// "return a stored real type" and was wrong: it had inspected the method and not
// the callers. A producer census that reads only method bodies is blind to
// exactly this construction, and the blindness is not theoretical — it already
// produced a false claim in a reviewed document.
func TestResultTypeStubsCreatedAtCallSites(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	const rel = "pkg/recordlayer/query/plan/cascades/rule_aggregate_data_access.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	var unknownArgs int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != "NewRecordQueryAggregateIndexPlan" {
			return true
		}
		for _, a := range call.Args {
			if s, ok := a.(*ast.SelectorExpr); ok && s.Sel != nil && s.Sel.Name == "UnknownType" {
				unknownArgs++
			}
		}
		return true
	})

	const want = 3
	if unknownArgs != want {
		t.Fatalf("%s: %d NewRecordQueryAggregateIndexPlan call(s) pass values.UnknownType as the "+
			"result type, want %d.\n"+
			"  MORE means another aggregate construction joined the call-site stub population. "+
			"FEWER means one started passing a real type — good news, and RFC-213's tier list "+
			"plus the continuation-fingerprint note in §3 both need updating, because that note "+
			"says the aggregate `result-type` field contributes ZERO entropy precisely BECAUSE "+
			"it is always the same singleton.\n"+
			"  Either way this is a deliberate edit, not drift.", rel, unknownArgs, want)
	}
}

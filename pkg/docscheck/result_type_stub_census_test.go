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
// SO THE HISTORICAL DIFFERENCE WAS WHERE THE SENTINEL LIVED, NOT WHETHER ONE
// EXISTED. Java put it at the VALUE level, named it (EmptyValue) and gave it a
// predicate; pre-RFC-232 Go put it at the PLAN level, unnamed, as a method that
// opted out — indistinguishable from an unfinished one. That is how twelve
// unconditional plan stubs accumulated before RFC-213/RFC-232 closed them.
//
// RFC-232 completed RFC-213's dependency inversion: RelationalExpression now
// requires GetResultValue, RecordQueryPlan embeds that interface, and every
// physical plan states an exact result value/type. The former twelve-entry debt
// inventory therefore reached zero. The detector remains as a zero ratchet:
// reintroducing an unconditional plan-level UnknownType is a regression, not a
// new inventory entry.
//
// Any new stub fails. The empty inventory is intentional: there is no accepted
// plan-level UnknownType debt left to shrink or grow.
func stubInventory() map[string]string {
	return map[string]string{}
}

// planPkgDir is where the plan implementations live.
const planPkgDir = "pkg/recordlayer/query/plan/plans"

// findStubs returns every plan whose GetResultType body is exactly
// `return values.UnknownType`.
//
// The match is STRUCTURAL — a single-statement body returning a selector named
// UnknownType — rather than a grep for the identifier. That distinction is
// load-bearing: guarded uses elsewhere are consumers of possibly absent
// metadata, not unconditional plan producers.
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
			"than counted. The consumer classifier below keeps raw reads at zero; guarded "+
			"sites decline and propagated sites carry the type onward, so a plan joining this "+
			"list can cost optimizations without turning another test red.\n"+
			"  Java cannot express this state at all: `getResultValue()` is abstract on "+
			"RelationalExpression (:200) and `getResultType()` is a default derived from it "+
			"(:195-196), so every expression states its row.\n"+
			"  RFC-232 closed this population at zero by requiring an exact result value. "+
			"Do not add a new debt entry: derive the type from that value.",
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

// The interface inversion RFC-213 proposed and RFC-232 completed. This pins the
// post-migration root contract: every RecordQueryPlan is a RelationalExpression,
// which requires GetResultValue, and also states GetResultType explicitly.
func TestRecordQueryPlanRequiresGetResultValue(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	const rel = planPkgDir + "/plan.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}

	var hasResultType, embedsRelationalExpression, foundIface bool
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
			if len(m.Names) == 0 {
				if sel, ok := m.Type.(*ast.SelectorExpr); ok && sel.Sel != nil && sel.Sel.Name == "RelationalExpression" {
					embedsRelationalExpression = true
				}
			}
			for _, nm := range m.Names {
				switch nm.Name {
				case "GetResultType":
					hasResultType = true
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
	if !embedsRelationalExpression {
		t.Fatalf("%s: RecordQueryPlan no longer embeds expressions.RelationalExpression, "+
			"which is the RFC-232/RFC-213 contract requiring every plan to state GetResultValue", rel)
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
	byKind := map[string][]string{}
	var raw []string
	for _, s := range sites {
		if s.kind == "CENSUS" {
			continue
		}
		counts[s.kind]++
		byKind[s.kind] = append(byKind[s.kind], fmt.Sprintf("%s:%d in %s", s.file, s.line, s.fn))
		if s.kind == "RAW" {
			raw = append(raw, fmt.Sprintf("%s:%d in %s", s.file, s.line, s.fn))
		}
	}
	for kind := range byKind {
		sort.Strings(byKind[kind])
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
			"  RFC-213 §3 rests on every consumer failing CLOSED on an unresolved type. The "+
			"stub population is now zero; if one is reintroduced, a raw read gets a "+
			"*PrimitiveType where it expected a row.\n"+
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
	// RFC-232 moved result-type production from ad-hoc forwarding methods to
	// exact result values. The one surviving FORWARD is a true physical wrapper;
	// the guarded/propagated split below is the measured post-migration surface.
	// Subsequent executor admission work added exact record-shape guards for
	// projection, UPDATE, aggregate index, multi-intersection, and
	// DefaultOnEmpty (a net three guarded sites after replaced guards), while
	// VALUES now propagates the declared plan type into its runtime validator.
	// FirstOrDefault empty-arm materialization added one more intentional
	// PROPAGATED read. `firstOrDefaultResultFromValue` stores the plan's declared
	// result type, then branches on its exact record shape: record defaults are
	// checked or built against that type and provided layout, while scalar
	// defaults carry that same declared type into their positional row. The AST
	// classifier deliberately does no dataflow, so the next-statement type
	// assertion does not make this syntactically GUARDED.
	// Correlated FlatMap construction subsequently retired two PROPAGATED
	// `GetResultType()` fallbacks: the selected inner plan fallback and the
	// planner-local exact predicate-edge helper. Selected outer/inner edge types
	// now come solely from `ProvidedOutputLayout().Carrier().FlowedType()`; an
	// absent physical layout fails closed instead of consulting a declared result
	// type.
	// The two descendant producer walkers subsequently retired all four of their
	// outer/inner `GetResultType()` reads. Carrier pointer identity plus
	// `OrdinalLayout.RawEqual` already proves the exact physical row and layout;
	// consulting the separately declared type was redundant and weaker. Two of
	// those reads were in the last pinned population and two arrived transiently
	// with `descendantRetainedResultProducer`, so the stable census moves 32 -> 30.
	// `predicatesFilterIsFullPKPointProbe` then retired one further PROPAGATED
	// `GetResultType()` read. Its proof now takes both the row type and the
	// pointer-exact current owner from `scan.ProvidedOutputLayout().Carrier()`.
	// The declared result type could not identify the selected evaluation phase
	// after exact filter normalization and made every PK point probe over-decline;
	// the physical carrier is the stronger and necessary authority. The stable
	// census therefore moves 30 -> 29.
	// RFC-235 then retired the physical leg-concat walk that the three-quantifier
	// NLJ arm drove, taking THREE classified reads with it — the only movement
	// here that is a deletion rather than a migration. Two were GUARDED and paired
	// a `recordResultTypeRead` with an immediate exact-shape test on the same
	// value (`planBuriedLegConcat` type-asserting a leg's row to *RecordType,
	// `planRowRecordType` switching on it); one was PROPAGATED, passing a plan's
	// declared type straight into a rebuilt fetch plan. Nothing replaced them: the
	// walk had no production caller once the arm went, so these are reads that
	// stopped happening rather than reads that moved to a stronger authority. The
	// stable census moves GUARDED 14 -> 12 and PROPAGATED 29 -> 28.
	// RFC-242 then deleted the three union re-alignment mechanisms, and three
	// reads went with them — again deletions, not migrations. Two were GUARDED
	// tail reads that type-asserted a union leg's `GetResultType()` to
	// *RecordType to fold its field names (`physicalPlanColumnNames` in the
	// implementation rule, `planColumnNamesWithMD` in the executor); one was
	// PROPAGATED, passing a leg's declared type into `columnRenameValue` to build
	// a rename Map. The translator states every leg's row and both set-operation
	// constructors assert it, so nothing reads a leg's declared type to compare
	// legs any more. The stable census moves GUARDED 12 -> 10 and
	// PROPAGATED 28 -> 27.
	const wantForward, wantGuarded, wantPropagated = 1, 10, 27
	if counts["FORWARD"] != wantForward || counts["GUARDED"] != wantGuarded || counts["PROPAGATED"] != wantPropagated {
		t.Fatalf("consumer split moved: FORWARD=%d (want %d) GUARDED=%d (want %d) "+
			"PROPAGATED=%d (want %d), total %d.\n"+
			"  FORWARD:\n    %s\n  GUARDED:\n    %s\n  PROPAGATED:\n    %s\n"+
			"  These are RFC-213 §3's numbers. A change here is a real change to the consumer "+
			"population and the RFC's blast-radius argument is stated against them — update "+
			"both together, and say which site moved and why.",
			counts["FORWARD"], wantForward, counts["GUARDED"], wantGuarded,
			counts["PROPAGATED"], wantPropagated, len(sites),
			strings.Join(byKind["FORWARD"], "\n    "),
			strings.Join(byKind["GUARDED"], "\n    "),
			strings.Join(byKind["PROPAGATED"], "\n    "))
	}
}

// A STUB CAN BE CREATED AT THE CALL SITE, WHERE NO METHOD-BODY GATE CAN SEE IT.
//
// RecordQueryAggregateIndexPlan's GetResultType returns a stored field, so the
// inventory above correctly does not list it — and yet every aggregate index
// plan the planner built ONCE carried values.UnknownType, because the
// production callers passed the singleton EXPLICITLY and the constructor
// defaulted a nil type to it. That was a thirteenth stub, made one layer up.
//
// RFC-232 CLOSED IT, which is why this paragraph is historical and not a live
// claim: the callers now derive an exact result type before constructing the
// plan, and the constructor returns an error rather than defaulting. `want`
// below holds that population at zero and is what would catch a relapse.
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

	const want = 0
	if unknownArgs != want {
		t.Fatalf("%s: %d NewRecordQueryAggregateIndexPlan call(s) pass values.UnknownType as the "+
			"result type, want %d.\n"+
			"  RFC-232 closed this call-site stub population at zero: every aggregate candidate "+
			"derives its exact output type before constructing the physical plan. Any hit is a "+
			"regression to an unstated result row.", rel, unknownArgs, want)
	}
}

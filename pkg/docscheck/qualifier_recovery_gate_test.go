package docscheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// THE QUALIFIER RECOVERY CENSUS MUST CONSULT ITS GATE BEFORE IT CLASSIFIES.
//
// values.RecordQualifierRecovery checks LegIdentityCensusEnabled itself, so a
// recorder that classifies first and files second is still CORRECT with the
// census off. It is not FREE. The classification is the expensive half: a
// parseColRef, a strings.ToUpper of a qualifier, a counterparty lookup — built
// to make an argument the disabled sink drops on the floor. The worst of them,
// recordDisplayLabelStrip, sits beside a production line that already calls
// parseColRef and isPlainQualifiedColumnReference (which calls parseColRef a
// SECOND time and adds a ContainsAny); a recorder classifying ahead of the gate
// doubles all of it on every projected column of every query — 750 calls over
// the real-FDB corpus.
//
// WHY THIS IS A STRUCTURAL GATE AND NOT A MEASURED ONE, which is the same
// reasoning TestPlanCacheScope_SizeEstimateExact records for the plan-cache
// scope builder. The property is "no work happens", and no work leaves no trace
// the two candidate instruments can read:
//
//   - testing.AllocsPerRun PANICS under t.Parallel, and every test in the two
//     packages that hold these recorders is parallel. Measured, not assumed: the
//     first draft of this pin was an AllocsPerRun probe and it died with
//     "testing: AllocsPerRun called during parallel test".
//   - testing.Benchmark reads process-wide MemStats, so concurrent tests inflate
//     it — a flaky number for a claim that must be exact.
//
// And a census-delta probe cannot see it at all: BOTH orderings report a delta
// of zero with the census off, which is precisely why the wrong order can ship
// green. The ordering itself is the property, it is deterministic, and it is
// what this gate pins.
//
// THE HOISTED CALLERS ARE PART OF THE PROPERTY. recordExistsSortSplit's own body
// is allocation-free — ClassifyQualifierRecovery slices substrings and compares
// them — so a gate inside it buys nothing measurable. That site's census-off
// cost is sortKeyQualifierIdentity, which the EXISTS fold's two sort-key readers
// call for no reason but to hand this recorder a counterparty, and which
// upper-cases a qualifier per sort key. So the gate is hoisted ABOVE that call
// at both callers, and a gate on the recorder alone would leave the real cost
// exactly where it was.

// censusGateFunc is one recorder whose FIRST statement must be the gate check.
type censusGateFunc struct {
	file string
	fn   string
	why  string
}

var censusGateRecorders = []censusGateFunc{
	{
		file: "pkg/relational/core/embedded/colref.go",
		fn:   "recordDisplayLabelStrip",
		why: "the costliest of the four: its production line already parses the label " +
			"twice, and a recorder classifying first doubles that on every projected " +
			"column of every query (750 calls over the real-FDB corpus)",
	},
	{
		file: "pkg/relational/core/embedded/colref.go",
		fn:   "recordProjQualVsScan",
		why:  "upper-cases the slot's triple qualifier on every projected column",
	},
	{
		file: "pkg/relational/core/query/derived_unnest.go",
		fn:   "recordDerivedUnnestSplit",
		why:  "upper-cases both the source and the slot's triple qualifier per unnest classification",
	},
	{
		file: "pkg/relational/core/query/cascades_translator.go",
		fn:   "recordExistsSortSplit",
		why: "its own body is allocation-free, so this gate is the cheap half of the " +
			"pair; the half that carries the cost is the hoist at its two callers, " +
			"pinned by TestQualifierRecoveryCensusGateIsHoistedAboveIdentity",
	},
}

// isCensusGateCall reports whether e is a call to LegIdentityCensusEnabled,
// qualified or bare. The package qualifier is deliberately NOT pinned to
// "values": an import alias names the same function, and a gate a rename could
// bypass is not a gate.
func isCensusGateCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name == "LegIdentityCensusEnabled"
	case *ast.SelectorExpr:
		return fn.Sel != nil && fn.Sel.Name == "LegIdentityCensusEnabled"
	}
	return false
}

// findFunc returns the top-level function declaration named fn.
func findFunc(f *ast.File, name string) *ast.FuncDecl {
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name != nil && fd.Name.Name == name {
			return fd
		}
	}
	return nil
}

func parseSource(t *testing.T, root, rel string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	return fset, f
}

// TestQualifierRecoveryCensusGateIsFirstStatement pins that each recorder reads
// the gate before it does anything else, and returns on it.
func TestQualifierRecoveryCensusGateIsFirstStatement(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	for _, rec := range censusGateRecorders {
		t.Run(rec.fn, func(t *testing.T) {
			t.Parallel()
			_, f := parseSource(t, root, rec.file)
			fd := findFunc(f, rec.fn)
			if fd == nil || fd.Body == nil {
				t.Fatalf("%s: function %s not found — this gate names the recorders by "+
					"hand, so a rename must come here too rather than silently retiring the check",
					rec.file, rec.fn)
			}
			if len(fd.Body.List) == 0 {
				t.Fatalf("%s: %s has an empty body", rec.file, rec.fn)
			}
			fail := func() {
				t.Fatalf("%s: %s does not open with `if !<values>.LegIdentityCensusEnabled() "+
					"{ return }`.\n"+
					"  The census being OFF must cost this site one atomic load and nothing "+
					"else. RecordQualifierRecovery's own gate keeps a late check CORRECT, which "+
					"is exactly why a late check ships green: both orderings report a census "+
					"delta of zero with the census off, and production quietly pays the "+
					"classification anyway.\n  %s", rec.file, rec.fn, rec.why)
			}
			ifStmt, ok := fd.Body.List[0].(*ast.IfStmt)
			if !ok || ifStmt.Init != nil || ifStmt.Else != nil {
				fail()
			}
			neg, ok := ifStmt.Cond.(*ast.UnaryExpr)
			if !ok || neg.Op != token.NOT || !isCensusGateCall(neg.X) {
				fail()
			}
			if len(ifStmt.Body.List) != 1 {
				fail()
			}
			ret, ok := ifStmt.Body.List[0].(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 0 {
				fail()
			}
		})
	}
}

// censusOnlyHelper is a function computed FOR a census and for nothing else,
// whose every call must therefore sit inside a positive census guard.
//
// The gate above pins the RECORDER's first statement. This one pins the callers,
// and it is the half that carries the cost: a gate inside the recorder cannot
// recover work that was already done to build the recorder's ARGUMENT, or work
// done inline by a production function that merely happens to print a census
// witness. Both entries below were found by reading, and both were live.
type censusOnlyHelper struct {
	file string
	fn   string
	// minCalls asserts the population is not empty. A hand-named helper that has
	// been renamed away would otherwise clear an empty set and say nothing.
	minCalls int
	why      string
}

var censusOnlyHelpers = []censusOnlyHelper{
	{
		file:     "pkg/relational/core/query/cascades_translator.go",
		fn:       "sortKeyQualifierIdentity",
		minCalls: 2,
		why: "it is computed for the census and for nothing else — neither resolveKeyName's " +
			"answer nor sortKeySourceValue's value depends on it — and it upper-cases a " +
			"qualifier per sort key. Ungated, every sort key of every EXISTS fold over a join " +
			"pays that allocation with the census off, and a gate inside recordExistsSortSplit " +
			"cannot recover it: the cost is incurred building the ARGUMENT",
	},
	{
		file:     "pkg/recordlayer/query/plan/cascades/fold_step1_seed_census.go",
		fn:       "describeFlatMapResultOrigin",
		minCalls: 1,
		why: "it takes flatMapProducerMu, a PROCESS-GLOBAL mutex, and its only reader is " +
			"classifyDeclinedLeg — which is not a census function at all. classifyDeclinedLeg " +
			"runs inside reconstructFoldStep1Seed on the production seed path, for every " +
			"declined leg of every EXISTS-over-join firing, census on or off. Read ungated it " +
			"put a global lock acquisition on that path to build a witness string that the " +
			"disabled census then dropped. This is the SAME class as the recorders above and " +
			"a DIFFERENT shape: not a recorder classifying ahead of its gate, but a " +
			"production function reading a census instrument inline",
	},
}

// TestCensusOnlyHelpersAreCalledUnderTheGate pins the half of each census gate
// that actually costs something.
//
// A census-only helper called outside a positive LegIdentityCensusEnabled guard
// is paid for on every production firing, and NOTHING can see it: the census is
// off, so its counters read zero either way, and a census-delta probe reports no
// difference between the guarded and the unguarded spelling. The ordering is the
// property, it is deterministic, and it is what this gate reads off the AST —
// the same argument TestQualifierRecoveryCensusGateIsFirstStatement records for
// why this is structural and not measured.
func TestCensusOnlyHelpersAreCalledUnderTheGate(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	for _, h := range censusOnlyHelpers {
		t.Run(h.fn, func(t *testing.T) {
			t.Parallel()
			fset, f := parseSource(t, root, h.file)

			// Every range covered by the body of an `if <gate>()` — the regions in
			// which the helper may legally be called. The helper's own DECLARATION is
			// excluded by construction: it is a FuncDecl, and only CallExprs are
			// collected below.
			type span struct{ lo, hi token.Pos }
			var guarded []span
			ast.Inspect(f, func(n ast.Node) bool {
				if ifStmt, ok := n.(*ast.IfStmt); ok && isCensusGateCall(ifStmt.Cond) {
					guarded = append(guarded, span{ifStmt.Body.Pos(), ifStmt.Body.End()})
				}
				return true
			})

			var unguarded []string
			var found int
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok || id.Name != h.fn {
					return true
				}
				found++
				for _, g := range guarded {
					if call.Pos() >= g.lo && call.End() <= g.hi {
						return true
					}
				}
				unguarded = append(unguarded, fset.Position(call.Pos()).String())
				return true
			})

			if len(unguarded) != 0 {
				t.Fatalf("%s: %s is called outside a LegIdentityCensusEnabled guard at %v.\n"+
					"  %s.\n"+
					"  If a production reader ever genuinely needs this value, that is a real "+
					"change and this gate is where it gets argued — not routed around.",
					h.file, h.fn, unguarded, h.why)
			}
			// A gate that found no calls at all would pass vacuously, and this one is
			// pinned by hand to a function name, so the population is asserted.
			if found < h.minCalls {
				t.Fatalf("%s: found %d call(s) to %s, want >= %d. Below that this gate is "+
					"clearing an empty population and says nothing about any caller",
					h.file, found, h.fn, h.minCalls)
			}
		})
	}
}

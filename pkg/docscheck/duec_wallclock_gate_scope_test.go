package docscheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// THE OPT-IN GATE MUST NOT SPREAD BEYOND THE TIMING ARMS.
//
// DUEC_ASSERT_WALLCLOCK makes the three wall-clock criteria advisory by default.
// That trade is only sound while everything NON-temporal keeps asserting on
// every lane — otherwise a mechanism sold as "abstain from the timing" has
// quietly become "abstain from everything", and a green run means nothing.
//
// This is the same argument the budgetArmsRan/rowArmsRan anti-silence tally
// makes at runtime, and it is deliberately made a SECOND way, structurally,
// because the two catch different failures. The tally proves the load-independent
// arms RAN on a given execution. It cannot prove that a future edit did not put
// one of them behind the gate — a tucked-away arm simply stops incrementing, and
// the floor is a floor, so removing one arm while adding another elsewhere keeps
// it satisfied. This gate reads the source and refuses the edit itself.
//
// The probe's own comment already names the analogous hazard for the regime
// verdict: "a future edit that tucks one of them behind `if timingResolvable`
// reds here rather than silently halving the probe". The env gate is strictly
// more dangerous than `timingResolvable`, because it is off by DEFAULT on every
// lane rather than only on a loaded box.
func TestDuecWallClockGateDoesNotSpread(t *testing.T) {
	t.Parallel()

	// Lives in docscheck, and NOT beside the probe, for a reason its first draft
	// found the hard way: a test that parses its own package by relative path
	// passes under `go test` and dies in the Bazel sandbox, where the runfiles root
	// is not the package directory. sourceTreeRoot is how every other AST gate here
	// addresses the tree.
	root := sourceTreeRoot(t)
	const rel = "pkg/relational/sqldriver/distinct_unique_elision_cost_probe_test.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}

	var defs, uses int
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range v.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == "wallClockAsserted" {
					defs++
				}
			}
		case *ast.BinaryExpr:
			if id, ok := v.X.(*ast.Ident); ok && id.Name == "wallClockAsserted" {
				uses++
			}
		}
		return true
	})

	// One definition, and exactly three guarded conditions: B/D', s1 and s50.
	const wantDefs, wantUses = 1, 3
	if defs != wantDefs || uses != wantUses {
		t.Fatalf("wallClockAsserted: %d definition(s) and %d guarded condition(s), want %d and %d.\n"+
			"  MORE uses means the opt-in gate has spread past the three wall-clock criteria it\n"+
			"  is scoped to. Everything else in this probe — plan shapes, elision decisions, row\n"+
			"  counts, NULL counts, the nine statement-memory-budget rows, the allocation\n"+
			"  criteria — is load-INDEPENDENT and must assert on every lane, gate or no gate.\n"+
			"  A gate that reaches one of them converts 'the timing is advisory' into 'the probe\n"+
			"  is advisory', and a green CI run would then mean nothing at all.\n"+
			"  FEWER means a timing arm stopped being gated and is asserting a quiet-machine\n"+
			"  criterion on every lane again, which is the defect this change exists to fix.\n"+
			"  Either way: deliberate edit, and update this count with the reason.",
			defs, uses, wantDefs, wantUses)
	}
}

package docscheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// THE OPT-IN GATE MUST NOT SPREAD BEYOND THE ONE TIMING ARM.
//
// GEMERGE_ASSERT_WALLCLOCK makes RFC-209 §7's merge-over-reference ratio
// advisory by default. That trade is only sound while everything NON-temporal
// keeps asserting on every lane — the four plan-shape claims and the three
// row-count claims — otherwise a mechanism sold as "abstain from the timing"
// has quietly become "abstain from everything", and a green nightly means
// nothing.
//
// This is the same argument the shapeArmsRan/rowArmsRan anti-silence tally
// makes at runtime, and it is deliberately made a SECOND way, structurally,
// because the two catch different failures. The tally proves the
// load-independent arms RAN on a given execution. It cannot prove that a future
// edit did not put one of them behind the gate — a tucked-away arm simply stops
// incrementing, and the floor is a floor, so removing one arm while adding
// another elsewhere keeps it satisfied. This gate reads the source and refuses
// the edit itself.
//
// It also does something the tally structurally cannot: the stress file carries
// a `stress` build tag, so its runtime tally only speaks on nightly-stress. This
// gate parses the file as data and therefore runs in ordinary CI, where the edit
// that would break it is actually made.
func TestGemergeWallClockGateDoesNotSpread(t *testing.T) {
	t.Parallel()

	// sourceTreeRoot, not a relative path: a test that addresses its own package
	// by relative path passes under `go test` and dies in the Bazel sandbox,
	// where the runfiles root is not the package directory.
	root := sourceTreeRoot(t)
	const rel = "pkg/relational/sqldriver/stress/group_existence_plan_cost_test.go"
	fset := token.NewFileSet()
	// The `stress` build tag is irrelevant here — parser.ParseFile reads the file
	// as source and never consults build constraints, which is exactly why this
	// gate can run on a lane that would not compile the file.
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
		case *ast.IfStmt:
			if id, ok := v.Cond.(*ast.Ident); ok && id.Name == "wallClockAsserted" {
				uses++
			}
		}
		return true
	})

	// One definition, and exactly one guarded condition: §7's ratio bound.
	const wantDefs, wantUses = 1, 1
	if defs != wantDefs || uses != wantUses {
		t.Fatalf("wallClockAsserted: %d definition(s) and %d guarded condition(s), want %d and %d.\n"+
			"  MORE uses means the opt-in gate has spread past the single wall-clock criterion it\n"+
			"  is scoped to. Everything else in that test — the four plan-shape claims and the three\n"+
			"  row-count claims — is load-INDEPENDENT and must assert on every lane, gate or no gate.\n"+
			"  A gate that reaches one of them converts 'the ratio is advisory' into 'the test is\n"+
			"  advisory', and a green nightly-stress run would then mean nothing at all.\n"+
			"  FEWER means the ratio stopped being gated and is asserting a quiet-machine criterion\n"+
			"  on a shared self-hosted runner again, which is the defect this change exists to fix:\n"+
			"  the ratio's denominator is the noisier of its two terms, so it reds when the reference\n"+
			"  gets FASTER, which is an improvement rather than a regression.\n"+
			"  Either way: deliberate edit, and update this count with the reason.",
			defs, uses, wantDefs, wantUses)
	}
}

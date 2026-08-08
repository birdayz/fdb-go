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

	const name = "wallClockAsserted"

	// THREE counts, not two, and the third is what makes this a gate rather than
	// a spelling check.
	//
	// An earlier form of this test asked whether an IfStmt's Cond *was* the bare
	// identifier. That recognises `if wallClockAsserted {` and nothing else, so
	// three spreads walked straight through it — verified, not theorised, by
	// inserting each into the probe and watching this test stay green:
	//
	//   if wallClockAsserted && x {   BinaryExpr — the Cond is not the Ident
	//   if !wallClockAsserted { ... } UnaryExpr  — likewise
	//   w := wallClockAsserted; if w  neither count moves at all
	//
	// The middle one is the reason this is worth fixing rather than noting: an
	// early `if !wallClockAsserted { return }` makes the ENTIRE regime opt-in,
	// which is precisely and exactly what this gate exists to forbid, and it is
	// the most plausible accident of the three.
	//
	// So: condRefs walks the Cond SUBTREE instead of matching its root, which
	// catches any boolean shape; and totalRefs counts every mention anywhere,
	// which is the only thing that can catch an alias — an alias moves no
	// def and no cond, but it cannot avoid mentioning the name once.
	var defs, condRefs, totalRefs int
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.Ident:
			if v.Name == name {
				totalRefs++
			}
		case *ast.AssignStmt:
			for _, lhs := range v.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == name {
					defs++
				}
			}
		case *ast.IfStmt:
			ast.Inspect(v.Cond, func(c ast.Node) bool {
				if id, ok := c.(*ast.Ident); ok && id.Name == name {
					condRefs++
				}
				return true
			})
		}
		return true
	})

	// One definition; exactly one guarded condition (§7's ratio bound); and
	// exactly three mentions in total — the definition, that condition, and the
	// anti-silence Fatalf that reports the flag's state in its message.
	const wantDefs, wantCond, wantTotal = 1, 1, 3
	if defs != wantDefs || condRefs != wantCond || totalRefs != wantTotal {
		t.Fatalf("%s: %d definition(s), %d guarded condition(s), %d mention(s) in total; want %d, %d and %d.\n"+
			"  MORE guarded conditions means the opt-in gate has spread past the single wall-clock\n"+
			"  criterion it is scoped to. Everything else in that test — the four plan-shape claims\n"+
			"  and the three row-count claims — is load-INDEPENDENT and must assert on every lane,\n"+
			"  gate or no gate. A gate that reaches one of them converts 'the ratio is advisory' into\n"+
			"  'the test is advisory', and a green nightly-stress run would then mean nothing at all.\n"+
			"  An early `if !%s { return }` is the worst case and counts here: it makes\n"+
			"  the whole regime opt-in in one line.\n"+
			"  MORE total mentions with the other two counts unchanged means the flag was ALIASED\n"+
			"  (`w := %s`), which is how a spread hides from a gate that only reads\n"+
			"  conditions.\n"+
			"  FEWER guarded conditions means the ratio stopped being gated and is asserting a\n"+
			"  quiet-machine criterion on a shared self-hosted runner again, which is the defect this\n"+
			"  change exists to fix: the ratio's denominator is the noisier of its two terms, so it\n"+
			"  reds when the reference gets FASTER, which is an improvement rather than a regression.\n"+
			"  Any of these: deliberate edit, and update the counts with the reason.",
			name, defs, condRefs, totalRefs, wantDefs, wantCond, wantTotal, name, name)
	}
}

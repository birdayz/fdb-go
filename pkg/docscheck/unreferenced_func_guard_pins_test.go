package docscheck

import (
	"fmt"
	"strings"
	"testing"
)

// The vacuity floors and the ledger-shape guards are the arms the CORPUS can
// never exercise in their failing state: the suite only ever runs against a
// tree whose scan is healthy and whose ledger is non-empty. A reversed
// comparison or a dead branch would therefore first be discovered on the day
// the scan actually collapses — the one day the result gets read as "clean".

func TestScanVacuityFloorsFireOnEachAxis(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                 string
		candidates, packages int
		wantProblems         int
		wantSubstring        string
	}{
		{
			// The control. Without it, a checkScanVacuity that always reported
			// a problem would satisfy every other case in this table.
			name:       "a healthy scan trips neither floor",
			candidates: unexportedFuncPopulationMin, packages: unexportedFuncPackageMin,
			wantProblems: 0,
		},
		{
			name:       "the population floor fires one below",
			candidates: unexportedFuncPopulationMin - 1, packages: unexportedFuncPackageMin,
			wantProblems: 1, wantSubstring: "about the instrument and not about the tree",
		},
		{
			name:       "the package floor fires independently of the population",
			candidates: unexportedFuncPopulationMin, packages: unexportedFuncPackageMin - 1,
			wantProblems: 1, wantSubstring: "misses every other directory",
		},
		{
			name:       "a total collapse fires both",
			candidates: 0, packages: 0,
			wantProblems: 2,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems := checkScanVacuity(tc.candidates, tc.packages)
			if len(problems) != tc.wantProblems {
				t.Fatalf("checkScanVacuity(%d, %d) reported %d problem(s), want %d:\n%s",
					tc.candidates, tc.packages, len(problems), tc.wantProblems,
					strings.Join(problems, "\n"))
			}
			if tc.wantSubstring != "" && !strings.Contains(problems[0], tc.wantSubstring) {
				t.Errorf("problem %q does not explain the axis (want it to mention %q)",
					problems[0], tc.wantSubstring)
			}
		})
	}
}

func TestLedgerShapeGuardsFireOnEachArm(t *testing.T) {
	t.Parallel()

	entry := func(tag string) unreferencedFuncDisposition {
		return unreferencedFuncDisposition{tag: tag, why: pinWhy}
	}

	t.Run("an empty ledger is reported, because every per-entry arm goes vacuous", func(t *testing.T) {
		t.Parallel()
		problems := checkLedgerShape(map[string]unreferencedFuncDisposition{})
		if len(problems) != 1 || !strings.Contains(problems[0], "an entry came back") {
			t.Fatalf("an empty ledger must be reported with the INVERTED alarm "+
				"(the danger flips from 'unjustified entry' to 'entry came back'), got %v", problems)
		}
	})

	t.Run("a populated ledger under the cap is clean", func(t *testing.T) {
		t.Parallel()
		ledger := map[string]unreferencedFuncDisposition{"a.go # f": entry(dispositionKeep)}
		if problems := checkLedgerShape(ledger); len(problems) != 0 {
			t.Fatalf("a healthy ledger reported %v", problems)
		}
	})

	t.Run("exactly the cap is allowed", func(t *testing.T) {
		t.Parallel()
		ledger := map[string]unreferencedFuncDisposition{}
		for i := 0; i < unreferencedFuncDefectMax; i++ {
			ledger[fmt.Sprintf("a%d.go # f", i)] = entry(dispositionDefect)
		}
		if problems := checkLedgerShape(ledger); len(problems) != 0 {
			t.Fatalf("%d defects is the cap, not over it; reported %v",
				unreferencedFuncDefectMax, problems)
		}
	})

	t.Run("one past the cap is reported", func(t *testing.T) {
		t.Parallel()
		ledger := map[string]unreferencedFuncDisposition{}
		for i := 0; i <= unreferencedFuncDefectMax; i++ {
			ledger[fmt.Sprintf("a%d.go # f", i)] = entry(dispositionDefect)
		}
		problems := checkLedgerShape(ledger)
		if len(problems) != 1 || !strings.Contains(problems[0], "filing cabinet") {
			t.Fatalf("%d defects must trip the cap, got %v", unreferencedFuncDefectMax+1, problems)
		}
	})

	t.Run("non-defect tags do not count toward the cap", func(t *testing.T) {
		t.Parallel()
		ledger := map[string]unreferencedFuncDisposition{}
		for i := 0; i <= unreferencedFuncDefectMax+3; i++ {
			ledger[fmt.Sprintf("a%d.go # f", i)] = entry(dispositionKeep)
		}
		if problems := checkLedgerShape(ledger); len(problems) != 0 {
			t.Fatalf("keep entries are not capped; reported %v", problems)
		}
	})
}

package explaindiff_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/explaindiff"
)

// A literal and its statement-pool reference render differently; the plan
// around them must not differ.
var renderedLiteral = regexp.MustCompile(`@[0-9]+|'[^']*'(?:[^',)}\]]+')*|(^|[^A-Za-z0-9_#.$@])-?[0-9]+(?:\.[0-9]+)?(?:[eE][-+]?[0-9]+)?`)

func literalAgnostic(plan string) string {
	return renderedLiteral.ReplaceAllString(plan, "${1}?")
}

// REWRITING ranks these tied logical members by a memo hash that includes
// constant values, so a literal and a pool reference may keep different
// (equivalent) members. Physical ties are value-agnostic (tieValueHash).
var boundLiteralTieFlips = map[string]bool{
	"case_when_in_java.yaml#3": true,
	"fold_prune_regime.yaml#6": true,
}

// TestBoundLiteralPlansMatchLiteralPlans pins that the cache's plan form,
// literals as statement-pool references, plans every corpus SELECT exactly
// as literal planning does.
func TestBoundLiteralPlansMatchLiteralPlans(t *testing.T) {
	t.Parallel()
	literal, _ := collectCorpus(t)
	bound, _, err := explaindiff.CollectBoundLiterals(corpusDir)
	if err != nil {
		t.Fatalf("collect bound corpus: %v", err)
	}
	if len(bound) != len(literal) || len(literal) < 1000 {
		t.Fatalf("corpus sizes: literal %d, bound %d", len(literal), len(bound))
	}
	var diffs []string
	for i, want := range literal {
		got := bound[i]
		if want.Key() != got.Key() {
			t.Fatalf("entry %d: key %s vs %s", i, want.Key(), got.Key())
		}
		differs := want.Failed() != got.Failed() || !slices.Equal(want.Shape, got.Shape) ||
			literalAgnostic(want.Plan) != literalAgnostic(got.Plan)
		if differs != boundLiteralTieFlips[want.Key()] {
			diffs = append(diffs, fmt.Sprintf("%s (known tie flip: %v)\n  literal: %s\n  bound:   %s",
				want.Key(), boundLiteralTieFlips[want.Key()], want.Plan, got.Plan))
		}
	}
	if len(diffs) == 0 {
		return
	}
	if dir := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"); dir != "" {
		_ = os.WriteFile(filepath.Join(dir, "bound_literal_diffs.txt"), []byte(strings.Join(diffs, "\n")), 0o644)
	}
	t.Errorf("%d of %d corpus statements plan differently with bound literals; first:\n%s",
		len(diffs), len(literal), strings.Join(diffs[:min(len(diffs), 5)], "\n"))
}

package explaindiff_test

import (
	"flag"
	"os"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/explaindiff"
)

// updateGolden rewrites the committed plan-shape golden instead of asserting
// against it. Bazel's test sandbox is read-only, so regeneration runs under
// plain `go test`:
//
//	go test ./pkg/relational/conformance/explaindiff -run TestPlanShapeGolden -update-golden
//
// or, equivalently, the differ command:
//
//	go run ./cmd/explain-differ dump -out pkg/relational/conformance/explaindiff/testdata/plan_shape.golden
var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/plan_shape.golden from the current planner")

const planShapeGoldenPath = "testdata/plan_shape.golden"

// TestPlanShapeGolden is the standing plan-shape drift net (RFC-190 190.12).
//
// Every prior harness catches a different axis: rowdiff pins ROWS, the live
// Java differential pins Go-vs-Java parity, TestNoUnexpectedPlanFailures pins
// the set of queries that stop planning. NONE of them catches a silent
// change to the physical plan SHAPE of a query that still returns the right
// rows through a worse plan — the exact regression class this whole audit
// surfaced (a sort materialized where the planner used to eliminate it, an
// index scan demoted to a full scan). explaindiff already renders every corpus
// query's plan into a stable, diffable baseline; committing that baseline as a
// golden turns "did any plan shape move?" from a manual before/after run into
// an always-on CI assertion.
//
// The golden pins CURRENT behavior, latent quirks included — that is the point:
// it makes future drift visible so each change is BLESSED, not discovered in
// production. When a commit changes a plan on purpose (a cost fix, a new rule),
// the diff against this golden IS the review artifact: Java-verify it, then
// re-bless with -update-golden.
//
// No FDB required — pure planning under default statistics, same as the rest of
// explaindiff. Fast and always-on.
func TestPlanShapeGolden(t *testing.T) {
	t.Parallel()

	got, _, err := explaindiff.GenerateBaseline(corpusDir)
	if err != nil {
		t.Fatalf("generate baseline: %v", err)
	}

	if *updateGolden {
		if err := os.WriteFile(planShapeGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", planShapeGoldenPath, err)
		}
		t.Logf("re-blessed %s (%d bytes)", planShapeGoldenPath, len(got))
		return
	}

	wantBytes, err := os.ReadFile(planShapeGoldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v — first-time or missing; regenerate with `go run ./cmd/explain-differ dump -out %s`",
			planShapeGoldenPath, err, planShapeGoldenPath)
	}
	want := string(wantBytes)
	if got == want {
		return
	}

	changed, first := diffSummary(want, got)
	t.Errorf("plan-shape golden drift: %d line(s) differ from %s.\n%s",
		changed, planShapeGoldenPath, first)
	t.Errorf("If the change is INTENTIONAL and Java-verified, re-bless the golden:\n"+
		"  go run ./cmd/explain-differ dump -out %s\n"+
		"and include the golden diff in the commit so the plan-shape change is on the record.",
		planShapeGoldenPath)
}

// diffSummary reports how many lines changed and prints the first divergence
// with a few lines of context. A full 16k-line dump is useless in a test log;
// `git diff` on the re-blessed golden shows the complete picture.
func diffSummary(want, got string) (changed int, firstBlock string) {
	wl := strings.Split(want, "\n")
	gl := strings.Split(got, "\n")
	n := len(wl)
	if len(gl) > n {
		n = len(gl)
	}
	firstIdx := -1
	for i := 0; i < n; i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			changed++
			if firstIdx < 0 {
				firstIdx = i
			}
		}
	}
	if firstIdx < 0 {
		return changed, ""
	}
	var b strings.Builder
	lo := firstIdx - 3
	if lo < 0 {
		lo = 0
	}
	b.WriteString("first divergence near line ")
	b.WriteString(itoa(firstIdx + 1))
	b.WriteString(":\n")
	for i := lo; i < firstIdx+4 && i < n; i++ {
		var w, g string
		wok, gok := i < len(wl), i < len(gl)
		if wok {
			w = wl[i]
		}
		if gok {
			g = gl[i]
		}
		if wok && gok && w == g {
			b.WriteString("   " + w + "\n")
			continue
		}
		if wok {
			b.WriteString("  -" + w + "\n")
		}
		if gok {
			b.WriteString("  +" + g + "\n")
		}
	}
	return changed, b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

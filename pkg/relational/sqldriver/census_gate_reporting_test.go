package sqldriver_test

// A whole-corpus census gate that fails must LOOK like a failing test.
//
// THE DEFECT THIS FIXES. These gates are asserted in TestMain, after m.Run(),
// and they used to report by writing prose to stderr and bumping the exit code.
// `go test` renders that as:
//
//	PASS
//	CENSUS FAIL: denominator 586, want 582.
//	FAIL	fdb.dev/pkg/relational/sqldriver	271.639s
//
// There is no `--- FAIL:` line anywhere in it. Every tool and every habit that
// locates a failure keys on that marker — `grep '--- FAIL'`, a CI log scraper, a
// human scrolling — and all of them come back empty on a package that genuinely
// failed. This is the green-from-an-empty-set family in its most dangerous
// direction: the absence of a marker reads as the absence of a failure, so the
// only surviving evidence is a summary line naming the package and a duration.
// It is exactly how one such failure's text was lost, and it would have lost the
// next one.
//
// WHY THE GATES STAY IN TestMain rather than becoming ordinary test functions,
// which is the fix that looks cleaner and does not work. Each gate asserts over
// the population of the WHOLE corpus, so it can only run once every test has
// finished — and no test function can occupy that position. Go releases parallel
// tests only after the sequential ones are done (MEASURED: a sequential test
// polling a counter incremented by parallel siblings observed 0 overlaps), so a
// non-parallel "last" gate test actually runs FIRST, before any parallel test
// body; and a parallel one has no ordering guarantee against its parallel
// siblings. `m.Run()` returning is the only point at which the population is
// complete. So the gates stay where they are and learn to speak go test's
// output format instead.

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// TestCensusGateReportingArms drives every arm of the reporter.
//
// The corpus reaches exactly one of them — all-gates-pass — so the arms that
// decide whether a failure is VISIBLE would otherwise ship having never run.
// That is the same class of hole as the defect being fixed: an instrument whose
// failing path is untested reports nothing when it matters, and nothing is
// indistinguishable from fine.
func TestCensusGateReportingArms(t *testing.T) {
	t.Parallel()

	pass := func(msg string) testkit.CensusGate {
		return testkit.CensusGate{Name: "passing", Run: func(w io.Writer) bool {
			fmt.Fprint(w, msg)
			return false
		}}
	}
	fail := func(name, msg string) testkit.CensusGate {
		return testkit.CensusGate{Name: name, Run: func(w io.Writer) bool {
			fmt.Fprint(w, msg)
			return true
		}}
	}

	t.Run("all gates pass emits no failure marker", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if testkit.RunCensusGates(&buf, false, []testkit.CensusGate{pass("population reached: 512\n")}) {
			t.Fatalf("reported a failure for a clean run")
		}
		got := buf.String()
		if strings.Contains(got, "--- FAIL") {
			t.Fatalf("a clean run emitted a failure marker:\n%s", got)
		}
		if !strings.Contains(got, "population reached: 512") {
			t.Fatalf("a passing gate's report was swallowed; got:\n%s\n"+
				"  Several gates say something useful when they pass — that a filter\n"+
				"  narrowed the corpus, or what population they reached. Dropping it trades\n"+
				"  one silent failure mode for another.", got)
		}
	})

	t.Run("a failing gate is named on a column-zero FAIL marker", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if !testkit.RunCensusGates(&buf, false, []testkit.CensusGate{
			pass("fine\n"),
			fail("exampleGate", "denominator 586, want 582.\n"),
		}) {
			t.Fatalf("did not report a failure for a failing gate")
		}
		got := buf.String()
		// The exact query that returned 0 on the defect this fixes.
		var atColumnZero int
		for _, line := range strings.Split(got, "\n") {
			if strings.HasPrefix(line, "--- FAIL") {
				atColumnZero++
			}
		}
		if atColumnZero == 0 {
			t.Fatalf("no `--- FAIL` at column 0; `grep -c '^--- FAIL'` would return 0, which is\n"+
				"  the entire defect: a package that genuinely failed reports no marker that\n"+
				"  any tool or habit keys on. Got:\n%s", got)
		}
		if !strings.Contains(got, testkit.CensusGateSuiteName+"/exampleGate") {
			t.Fatalf("the failure does not NAME the gate; got:\n%s\n"+
				"  A marker that does not say which gate failed sends the reader back to\n"+
				"  scanning the whole census dump, which is what they were doing before.", got)
		}
		if !strings.Contains(got, "        denominator 586, want 582.") {
			t.Fatalf("the gate's reasoning is not indented under its marker; got:\n%s", got)
		}
	})

	t.Run("every failing gate gets its own line", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		testkit.RunCensusGates(&buf, false, []testkit.CensusGate{
			fail("first", "a\n"), pass("fine\n"), fail("second", "b\n"),
		})
		got := buf.String()
		for _, want := range []string{testkit.CensusGateSuiteName + "/first", testkit.CensusGateSuiteName + "/second"} {
			if !strings.Contains(got, want) {
				t.Fatalf("%q missing; got:\n%s\n"+
					"  Reporting only the first failure hides the rest, and on a corpus-wide\n"+
					"  census the SET of gates that moved is the diagnosis.", want, got)
			}
		}
	})

	t.Run("verbose adds RUN lines, non-verbose does not", func(t *testing.T) {
		t.Parallel()
		gates := []testkit.CensusGate{fail("g", "x\n")}
		var quiet, loud bytes.Buffer
		testkit.RunCensusGates(&quiet, false, gates)
		testkit.RunCensusGates(&loud, true, gates)
		if strings.Contains(quiet.String(), "=== RUN") {
			t.Fatalf("non-verbose emitted RUN lines, which go test does not:\n%s", quiet.String())
		}
		if !strings.Contains(loud.String(), "=== RUN   "+testkit.CensusGateSuiteName+"/g") {
			t.Fatalf("verbose did not emit a RUN line for the gate:\n%s", loud.String())
		}
	})

	t.Run("blank lines in a gate report do not become trailing whitespace", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		testkit.RunCensusGates(&buf, false, []testkit.CensusGate{fail("g", "one\n\ntwo\n")})
		for _, line := range strings.Split(buf.String(), "\n") {
			if line != strings.TrimRight(line, " \t") {
				t.Fatalf("line %q carries trailing whitespace", line)
			}
		}
	})
}

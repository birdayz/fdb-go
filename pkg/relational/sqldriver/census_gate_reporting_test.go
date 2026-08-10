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
)

// censusGateSuiteName is the identity failing gates report under. It is a
// test-shaped name on purpose: it is what appears after `--- FAIL:`, so it is
// what a reader greps for and what a log scraper attributes the failure to.
const censusGateSuiteName = "TestWholeCorpusCensusGates"

// censusGate is one whole-corpus assertion: a name, and a check that writes its
// reasoning to the writer and reports whether it FAILED.
type censusGate struct {
	name string
	run  func(io.Writer) bool
}

// runCensusGates runs every gate and renders the outcome in go test's own output
// format, returning whether any failed.
//
// Passing gates still have their prose emitted verbatim — several report
// something useful when they pass (that a -test.run filter narrowed the corpus,
// or the population they reached), and swallowing that would trade one silent
// failure mode for another.
//
// Failing gates are rendered as a parent failure with one subtest per gate,
// matching what `go test` prints for a table test:
//
//	--- FAIL: TestWholeCorpusCensusGates (0.00s)
//	    --- FAIL: TestWholeCorpusCensusGates/foldStep1Seed (0.00s)
//	        <the gate's own reasoning, indented>
//
// The parent line sits at column 0 so `grep -c '^--- FAIL'` finds it, which is
// the specific query that used to return 0 on a failing run.
func runCensusGates(w io.Writer, verbose bool, gates []censusGate) bool {
	type failure struct {
		name   string
		detail string
	}
	var failures []failure

	for _, g := range gates {
		var buf bytes.Buffer
		failed := g.run(&buf)
		if !failed {
			// Verbatim, so a passing gate's report reads exactly as it did.
			if buf.Len() > 0 {
				_, _ = w.Write(buf.Bytes())
			}
			continue
		}
		failures = append(failures, failure{name: g.name, detail: buf.String()})
	}

	if len(failures) == 0 {
		return false
	}

	// `=== RUN` only under -test.v, mirroring go test: in non-verbose mode it
	// prints the `--- FAIL:` lines and not the RUN lines.
	if verbose {
		fmt.Fprintf(w, "=== RUN   %s\n", censusGateSuiteName)
		for _, f := range failures {
			fmt.Fprintf(w, "=== RUN   %s/%s\n", censusGateSuiteName, f.name)
		}
	}
	fmt.Fprintf(w, "--- FAIL: %s (0.00s)\n", censusGateSuiteName)
	for _, f := range failures {
		fmt.Fprintf(w, "    --- FAIL: %s/%s (0.00s)\n", censusGateSuiteName, f.name)
		fmt.Fprint(w, indentCensusDetail(f.detail, "        "))
	}
	return true
}

// indentCensusDetail indents every non-empty line of a gate's report, the way
// go test indents a failure message under its `--- FAIL:` line. Blank lines stay
// blank rather than becoming trailing whitespace.
func indentCensusDetail(detail, prefix string) string {
	if detail == "" {
		return ""
	}
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(detail, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// TestCensusGateReportingArms drives every arm of the reporter.
//
// The corpus reaches exactly one of them — all-gates-pass — so the arms that
// decide whether a failure is VISIBLE would otherwise ship having never run.
// That is the same class of hole as the defect being fixed: an instrument whose
// failing path is untested reports nothing when it matters, and nothing is
// indistinguishable from fine.
func TestCensusGateReportingArms(t *testing.T) {
	t.Parallel()

	pass := func(msg string) censusGate {
		return censusGate{name: "passing", run: func(w io.Writer) bool {
			fmt.Fprint(w, msg)
			return false
		}}
	}
	fail := func(name, msg string) censusGate {
		return censusGate{name: name, run: func(w io.Writer) bool {
			fmt.Fprint(w, msg)
			return true
		}}
	}

	t.Run("all gates pass emits no failure marker", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if runCensusGates(&buf, false, []censusGate{pass("population reached: 512\n")}) {
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
		if !runCensusGates(&buf, false, []censusGate{
			pass("fine\n"),
			fail("foldStep1Seed", "denominator 586, want 582.\n"),
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
		if !strings.Contains(got, censusGateSuiteName+"/foldStep1Seed") {
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
		runCensusGates(&buf, false, []censusGate{
			fail("first", "a\n"), pass("fine\n"), fail("second", "b\n"),
		})
		got := buf.String()
		for _, want := range []string{censusGateSuiteName + "/first", censusGateSuiteName + "/second"} {
			if !strings.Contains(got, want) {
				t.Fatalf("%q missing; got:\n%s\n"+
					"  Reporting only the first failure hides the rest, and on a corpus-wide\n"+
					"  census the SET of gates that moved is the diagnosis.", want, got)
			}
		}
	})

	t.Run("verbose adds RUN lines, non-verbose does not", func(t *testing.T) {
		t.Parallel()
		gates := []censusGate{fail("g", "x\n")}
		var quiet, loud bytes.Buffer
		runCensusGates(&quiet, false, gates)
		runCensusGates(&loud, true, gates)
		if strings.Contains(quiet.String(), "=== RUN") {
			t.Fatalf("non-verbose emitted RUN lines, which go test does not:\n%s", quiet.String())
		}
		if !strings.Contains(loud.String(), "=== RUN   "+censusGateSuiteName+"/g") {
			t.Fatalf("verbose did not emit a RUN line for the gate:\n%s", loud.String())
		}
	})

	t.Run("blank lines in a gate report do not become trailing whitespace", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		runCensusGates(&buf, false, []censusGate{fail("g", "one\n\ntwo\n")})
		for _, line := range strings.Split(buf.String(), "\n") {
			if line != strings.TrimRight(line, " \t") {
				t.Fatalf("line %q carries trailing whitespace", line)
			}
		}
	})
}

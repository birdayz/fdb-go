package main

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// The exact output the binding tester produced on every CI runner in the fleet
// when the Python client was missing from the host. The harness reported this
// as "exit status 1", fifty times, and the lane read as a client regression.
const missingClientOutput = `Traceback (most recent call last):
  File "/tmp/bt-run/bindingtester/bindingtester.py", line 39, in <module>
    from bindingtester import FDB_API_VERSION
  File "/tmp/bt-run/bindingtester/../bindingtester/__init__.py", line 27, in <module>
    from bindingtester import util
  File "/tmp/bt-run/bindingtester/../bindingtester/util.py", line 26, in <module>
    import fdb
ModuleNotFoundError: No module named 'fdb'
`

func TestSummarizeFailureNamesTheCauseWhenTheTesterNeverReachedAVerdict(t *testing.T) {
	t.Parallel()

	got := summarizeFailure(missingClientOutput, errors.New("exit status 1"))

	if !strings.Contains(got, "ModuleNotFoundError: No module named 'fdb'") {
		t.Fatalf("failure summary hides the cause: %q\n"+
			"a seed that dies before the tester's verdict line must report what the "+
			"tester printed, not just the exec status — reporting only the status is "+
			"what made a fleet-wide missing-client break read as a client regression", got)
	}
	if got == "exit status 1" {
		t.Fatalf("failure summary is the bare exec status: %q", got)
	}
}

func TestSummarizeFailurePrefersTheTestersOwnVerdict(t *testing.T) {
	t.Parallel()

	output := strings.Join([]string{
		"Running api test",
		"Test had 3 incorrect result(s) and 0 error(s)",
		"Test had 7 incorrect result(s) and 1 error(s)",
		"tester exited",
	}, "\n")

	got := summarizeFailure(output, errors.New("exit status 1"))

	// Last verdict wins: the tester prints one per test it ran, and the seed's
	// outcome is the last one, not the first.
	if got != "Test had 7 incorrect result(s) and 1 error(s)" {
		t.Fatalf("verdict line not reported verbatim, got %q", got)
	}
}

func TestSummarizeFailureWithNoOutputAtAll(t *testing.T) {
	t.Parallel()

	if got := summarizeFailure("", errors.New("signal: killed")); got != "signal: killed" {
		t.Fatalf("got %q, want the exec error", got)
	}
	if got := summarizeFailure("", nil); got != "no result line in output" {
		t.Fatalf("got %q, want the no-output sentinel", got)
	}
}

func TestSummarizeFailureBoundsTheTail(t *testing.T) {
	t.Parallel()

	var lines []string
	for i := 0; i < 500; i++ {
		lines = append(lines, "noise")
	}
	lines = append(lines, "the actual last line")

	got := summarizeFailure(strings.Join(lines, "\n"), nil)

	if !strings.HasSuffix(got, "the actual last line") {
		t.Fatalf("summary dropped the last line: %q", got)
	}
	if strings.Count(got, "|") != maxTailLines-1 {
		t.Fatalf("summary is not bounded to %d lines: %q", maxTailLines, got)
	}
}

// The harness unpacks this tar into the tester's PYTHONPATH. It must come from
// the released sdist, not from @foundationdb//:python_fdb: in the FDB source
// tree apiversion.py is a CMake template and fdboptions.py does not exist at
// all (vexillographer generates it at build time), so a package sourced from
// there cannot be imported. These two files are exactly what distinguishes the
// two sources, which is why they are the ones asserted.
func TestPinnedPythonClientIsImportableNotTheCMakeTemplateTree(t *testing.T) {
	t.Parallel()

	// A go_test runs with its package directory as cwd inside the runfiles tree.
	f, err := os.Open("python-fdb.tar")
	if err != nil {
		t.Fatalf("python client tar not staged: %v", err)
	}
	defer f.Close()

	found := map[string]bool{}
	r := tar.NewReader(f)
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		found[h.Name] = true
	}

	for _, want := range []string{
		"fdb/__init__.py",
		"fdb/apiversion.py", // a .py.cmake template in the source tree
		"fdb/fdboptions.py", // absent from the source tree entirely
		"fdb/impl.py",
		"fdb/tuple.py",
	} {
		if !found[want] {
			t.Errorf("pinned Python client is missing %s — it cannot be imported. "+
				"If this now comes from @foundationdb//:python_fdb, revert: that tree "+
				"ships CMake/vexillographer templates, not a usable package. Have: %v",
				want, keys(found))
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

package main

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

// Presence of an entry name proves nothing about whether the tar can be unpacked
// anywhere: Bazel stages an external repo's srcs as symlinks into the execroot,
// so an archive built without --dereference names every file correctly while
// storing an absolute path to the builder's own output_base instead of content.
// That tar unpacks into dangling links on any other machine, `fdb/` ends up a
// directory with no importable __init__.py, and `import fdb` then quietly yields
// an empty implicit namespace package rather than an error — which is why the
// symptom reached the tester as an AttributeError on the first call it made, on
// every seed, with the cluster healthy.
func TestPinnedPythonClientTarCarriesContentNotSymlinksIntoTheBuildTree(t *testing.T) {
	t.Parallel()

	f, err := os.Open("python-fdb.tar")
	if err != nil {
		t.Fatalf("python client tar not staged: %v", err)
	}
	defer f.Close()

	sawInit := false
	r := tar.NewReader(f)
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}

		if h.Typeflag == tar.TypeSymlink || h.Typeflag == tar.TypeLink {
			t.Fatalf("%s is archived as a link to %q, not as content.\n"+
				"The tar must be built with --dereference: a link into the build tree "+
				"resolves only on the machine and output_base that produced the archive, "+
				"and dangles everywhere else. The harness then unpacks an empty fdb/ "+
				"directory, Python treats it as an implicit namespace package, and every "+
				"seed dies on a missing attribute with FDB alive.", h.Name, h.Linkname)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size == 0 {
			t.Errorf("%s is archived with no content (size 0)", h.Name)
		}
		if h.Name == "fdb/__init__.py" {
			sawInit = true
		}
	}

	// Without this the loop above is vacuously satisfied by an empty archive,
	// and the package that must exist for `import fdb` to bind to anything real
	// is exactly fdb/__init__.py.
	if !sawInit {
		t.Fatal("tar contains no fdb/__init__.py regular file")
	}
}

// The preflight exists to convert a broken client into one loud failure before
// any container starts. It was previously a bare `import fdb`, which does not
// fail on the shape that actually broke the lane — an fdb/ directory with no
// __init__.py imports fine as a namespace package. This reproduces that exact
// shape and requires the preflight program to reject it.
func TestPreflightRejectsAnEmptyNamespacePackage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// The defect's shape: the directory exists, nothing importable inside it.
	if err := os.Mkdir(filepath.Join(dir, "fdb"), 0o755); err != nil {
		t.Fatalf("stage namespace package: %v", err)
	}

	// -S drops site-packages, and `python3 -c` already puts the cwd first on
	// sys.path. Both matter: a regular package found anywhere on the path beats
	// a namespace portion, so a host with `pip install foundationdb` would
	// otherwise satisfy the import from site-packages and hide the shape under
	// test. The program itself is passed unmodified — it is what ships.
	cmd := exec.Command("python3", "-S", "-c", preflightPythonProgram)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("preflight accepted an empty namespace package — it would let all "+
			"seeds through to fail one by one on an opaque exec status.\noutput: %s", out)
	}
	if !strings.Contains(string(out), "namespace package") {
		t.Fatalf("preflight failed without naming the cause; a preflight that says "+
			"only that something went wrong is what sent this lane to triage as a "+
			"client regression.\noutput: %s", out)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

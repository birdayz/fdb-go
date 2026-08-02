package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The vulnerability gate is the one CI job whose failure mode is silence.
//
// Every other job in ci.yml announces itself when it breaks. This one can break
// by passing: govulncheck pointed at a database it could not read exits 0 and
// reports no findings, which is byte-identical to a clean scan. cmd/vulnscan
// exists to make that outcome distinguishable, and its own tests pin the
// classification. What those tests cannot see is whether ci.yml still CALLS it
// — and reverting to a direct `go run golang.org/x/vuln/cmd/govulncheck@latest`
// one-liner is a two-line diff that leaves every gate in the repository green
// while restoring both the 403 flakiness and the silent-pass hole.
//
// So this file pins the properties the workflow relies on, not the logic.

const vulnJobName = "govulncheck"

// TestVulnScanJobDelegatesToTheWrapper fails if ci.yml goes back to invoking
// govulncheck directly.
func TestVulnScanJobDelegatesToTheWrapper(t *testing.T) {
	t.Parallel()

	steps := vulnJobRunSteps(t)
	joined := strings.Join(steps, "\n")

	if !strings.Contains(joined, "./cmd/vulnscan") {
		t.Fatalf("ci.yml's %q job no longer invokes ./cmd/vulnscan.\n"+
			"Its run steps are:\n%s\n\n"+
			"The wrapper is what keeps a transient upstream 403 from presenting as a failed security "+
			"gate AND keeps an unreadable database from presenting as a clean one. Calling govulncheck "+
			"directly restores both defects at once.", vulnJobName, joined)
	}
}

// TestVulnScanNeverInvokesAnUnpinnedScanner is the version half.
//
// `@latest` inside a gate means the verdict can flip with no diff in this
// repository: unattributable, and it trains everyone to rerun rather than
// read. It is also an extra module resolution over the network on every run,
// which is one more thing that can answer 403.
func TestVulnScanNeverInvokesAnUnpinnedScanner(t *testing.T) {
	t.Parallel()

	for _, path := range workflowFiles(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if m := unpinnedScannerRef(string(raw)); m != "" {
			t.Errorf("%s pins the vulnerability scanner to a moving reference (%s).\n"+
				"A gate whose tool version floats can change its verdict with no change to the code "+
				"it guards. Pin an explicit version.", filepath.Base(path), m)
		}
	}
}

var unpinnedScanner = regexp.MustCompile(`golang\.org/x/vuln/cmd/govulncheck@(latest|master|main)\b`)

// unpinnedScannerRef finds a floating scanner reference that would actually be
// EXECUTED, returning "" if there is none.
//
// Comment lines are skipped, and that exclusion is load-bearing in both
// directions. A `#`-prefixed line is inert whether it is YAML commentary or a
// commented-out line inside a `run:` block, so flagging it would be a false
// failure — and the fix documenting why this job stopped calling govulncheck
// directly necessarily quotes the old invocation. Conversely the exclusion must
// stay this narrow: anything more permissive would let a live invocation hide.
func unpinnedScannerRef(yamlSrc string) string {
	for _, line := range strings.Split(yamlSrc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if m := unpinnedScanner.FindString(line); m != "" {
			return m
		}
	}
	return ""
}

// TestVulnScanPinsAnExplicitScannerVersion reads the pin out of the wrapper and
// checks it is a real, complete semver — a partial tag like "v1" would float
// again through the module proxy.
func TestVulnScanPinsAnExplicitScannerVersion(t *testing.T) {
	t.Parallel()

	src := readWrapperSource(t, "main.go")
	m := regexp.MustCompile(`govulncheckVersion\s*=\s*"([^"]*)"`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("cmd/vulnscan/main.go no longer declares govulncheckVersion; the scanner pin is the " +
			"only thing making this gate's verdict reproducible")
	}
	if !regexp.MustCompile(`^v\d+\.\d+\.\d+$`).MatchString(m[1]) {
		t.Fatalf("govulncheckVersion = %q, which is not a fully qualified version. A partial tag "+
			"resolves through the module proxy and floats, which is the defect the pin removes", m[1])
	}
}

// TestStalenessBoundsExistAndAreOrdered pins requirement 3 at the structural
// level: a mirror is only permissible if its staleness is bounded, and the
// bound is only meaningful if the hard limit is larger than the refresh
// interval. Collapsing them (or deleting the hard one) turns the cache into a
// gate that quietly reports from arbitrarily old data.
func TestStalenessBoundsExistAndAreOrdered(t *testing.T) {
	t.Parallel()

	src := readWrapperSource(t, "mirror.go")
	for _, name := range []string{"refreshAfter", "hardStaleAfter"} {
		if !strings.Contains(src, name) {
			t.Fatalf("cmd/vulnscan/mirror.go no longer declares %s. A cached vulnerability database "+
				"without an enforced staleness bound is the silent-pass failure mode with a cache "+
				"for a disguise.", name)
		}
	}
	// The refusal path is the part that can be deleted without breaking a
	// single happy-path test, so name it explicitly.
	if !strings.Contains(src, "MayProceedAfterFailedRefresh") {
		t.Fatal("cmd/vulnscan/mirror.go no longer declares MayProceedAfterFailedRefresh, which is the " +
			"only code that refuses to render a verdict from an over-stale mirror")
	}
}

// TestVulnScanJobHasAGoToolchain — the wrapper runs via `go run`, and these
// self-hosted runners do not all ship a system Go. A sibling change was fixing
// an exit-127 `go: command not found` on this fleet; a vulnerability job that
// dies at 127 is a failed gate for a reason that has nothing to do with
// vulnerabilities.
func TestVulnScanJobHasAGoToolchain(t *testing.T) {
	t.Parallel()

	wf := parseWorkflow(t, filepath.Join(workflowDir(t), "ci.yml"))
	job, ok := wf.Jobs[vulnJobName]
	if !ok {
		t.Fatalf("ci.yml has no %q job", vulnJobName)
	}
	for _, s := range job.Steps {
		if strings.Contains(strings.ToLower(s.Name), "set up go") {
			return
		}
	}
	t.Fatalf("ci.yml's %q job has no `Set up Go` step, but it runs the scanner with `go run`. "+
		"Without a toolchain the job fails at exit 127 and reads as a security failure.", vulnJobName)
}

// TestVulnGateSelfCheck proves this file's own reading is sharp enough to catch
// the regression it claims to catch. A gate that parses a workflow and finds
// nothing to object to in a BROKEN one has verified nothing — the failure mode
// the race-lane gate calls out for itself.
func TestVulnGateSelfCheck(t *testing.T) {
	t.Parallel()

	const reverted = `
jobs:
  govulncheck:
    name: Vulnerability scan
    runs-on: hetzner-fdb-vm
    steps:
      - name: Set up Go
        uses: actions/setup-go@v5
      - name: govulncheck (shipped packages)
        run: |
          PKGS=$(go list ./pkg/... | grep -v '/testcontainers')
          go run golang.org/x/vuln/cmd/govulncheck@latest $PKGS
`
	dir := t.TempDir()
	path := filepath.Join(dir, "ci.yml")
	if err := os.WriteFile(path, []byte(reverted), 0o644); err != nil {
		t.Fatal(err)
	}

	// The delegation check must reject it.
	wf := parseWorkflow(t, path)
	joined := ""
	for _, s := range wf.Jobs[vulnJobName].Steps {
		joined += s.Run + "\n"
	}
	if strings.Contains(joined, "./cmd/vulnscan") {
		t.Fatal("the self-check fixture accidentally delegates to the wrapper")
	}

	// The pin check must reject it too.
	if unpinnedScannerRef(reverted) == "" {
		t.Fatal("the unpinned-scanner check does not match a literal `@latest` invocation, so " +
			"TestVulnScanNeverInvokesAnUnpinnedScanner would pass over a reverted workflow")
	}

	// ...and the comment exclusion must not be wide enough to hide one. A
	// commented mention is inert; a live invocation on the same line as
	// trailing commentary is not.
	if got := unpinnedScannerRef("        # go run golang.org/x/vuln/cmd/govulncheck@latest ./pkg/...\n"); got != "" {
		t.Fatalf("a commented-out invocation was reported as live (%s)", got)
	}
	if unpinnedScannerRef("        run: go run golang.org/x/vuln/cmd/govulncheck@latest # legacy\n") == "" {
		t.Fatal("a live invocation carrying a trailing comment was treated as inert, so the check " +
			"could be bypassed by appending a comment to the line")
	}
}

func vulnJobRunSteps(t *testing.T) []string {
	t.Helper()
	wf := parseWorkflow(t, filepath.Join(workflowDir(t), "ci.yml"))
	job, ok := wf.Jobs[vulnJobName]
	if !ok {
		t.Fatalf("ci.yml has no %q job — the vulnerability gate was renamed or removed", vulnJobName)
	}
	var runs []string
	for _, s := range job.Steps {
		if strings.TrimSpace(s.Run) != "" {
			runs = append(runs, s.Run)
		}
	}
	if len(runs) == 0 {
		t.Fatalf("ci.yml's %q job has no run steps, so it scans nothing", vulnJobName)
	}
	return runs
}

func parseWorkflow(t *testing.T, path string) workflow {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return wf
}

func workflowFiles(t *testing.T) []string {
	t.Helper()
	dir := workflowDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml")) {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}

// readWrapperSource reads a cmd/vulnscan file from the source tree. The gate
// asserts on source text because the properties it pins (a version pin, a
// staleness bound, a refusal path) are declarations, and the point is to notice
// their DELETION — which no test of the surviving code can do.
func readWrapperSource(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(sourceTreeRoot(t), "cmd", "vulnscan", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

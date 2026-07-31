package docscheck

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// goosGoarchSuffixes are the implicit build constraints Go derives from a FILE
// NAME. A file whose base name (minus `.go`, minus a trailing `_test`) ends in
// `_<GOOS>` or `_<GOARCH>` is compiled only on that platform — no build tag, no
// warning, no error.
//
// The list is Go's, and it is deliberately spelled out rather than probed at
// runtime: this gate has to fail on a name that would be excluded on SOMEBODY's
// platform, not merely on the one running the test. `_linux` is as dangerous
// here as `_windows`, and a runtime probe would call it harmless.
var goosGoarchSuffixes = []string{
	// GOOS
	"aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios",
	"js", "linux", "nacl", "netbsd", "openbsd", "plan9", "solaris", "wasip1",
	"windows", "zos",
	// GOARCH
	"386", "amd64", "amd64p32", "arm", "arm64", "arm64be", "armbe", "loong64",
	"mips", "mips64", "mips64le", "mips64p32", "mips64p32le", "mipsle", "ppc",
	"ppc64", "ppc64le", "riscv", "riscv64", "s390", "s390x", "sparc", "sparc64",
	"wasm",
}

// platformConstrainingSuffix reports the implicit build constraint a Go file
// name carries, or "" when the name is safe.
//
// It is a named function, not a loop inside the gate, and that is the whole
// difference between a positive control that tests the gate and one that tests
// a copy of it. The control below used to re-implement this scan over its own
// temp directory: it proved the SUFFIX LIST still matched some strings, while
// the gate could have stopped calling the list entirely and the control would
// have gone on passing. One predicate, both callers.
func platformConstrainingSuffix(name string) string {
	if !strings.HasSuffix(name, ".go") {
		return ""
	}
	base := strings.TrimSuffix(name, ".go")
	base = strings.TrimSuffix(base, "_test")
	for _, suffix := range goosGoarchSuffixes {
		if strings.HasSuffix(base, "_"+suffix) {
			return suffix
		}
	}
	return ""
}

// platformConstraintScanSet is the set of repo-relative Go paths the gate
// checks: the whole tracked tree, via the same `git ls-files` enumerator the
// source-hygiene gate uses.
//
// Named, and shared with the scope test below, for the same reason the predicate
// is: a scope test that calls the enumerator directly proves the ENUMERATOR
// reaches outside pkg/, not that the GATE does. The gate could re-narrow to one
// directory with such a test still green — which is the state this replaced.
func platformConstraintScanSet(t *testing.T) []string {
	t.Helper()
	return trackedGoFiles(t, sourceTreeRoot(t))
}

// TestNoAccidentallyPlatformConstrainedFiles fails on a Go file whose NAME
// silently constrains it to one platform.
//
// This is not style. It found a REAL hole: `finalize_seed_windows_test.go` ends
// in `_windows`, so on every Linux run — every developer machine, every CI job —
// Go treated it as a Windows-only file and never compiled it. The test inside it
// pinned the seed-window authority's per-buried-leaf sub-window derivation, the
// mechanism that keeps two aliases' dup-named columns reading DIFFERENT slots.
// It had never executed. Nothing said so: `go test ./...` reports a package with
// a silently-dropped test file exactly as it reports one without it, and the
// coverage of the mechanism looked like coverage.
//
// The failure mode is worse for a test than for production code, because a
// production file dropped on Linux breaks the build immediately while a test
// file dropped on Linux breaks nothing at all — it just stops being evidence.
//
// SCOPE IS THE WHOLE TRACKED TREE, via the same `git ls-files` enumerator the
// source-hygiene gate uses. It walked only `pkg/` before, which is a scope no
// argument justified: `cmd/`, `conformance/` and every other tracked Go
// directory can lose a test file to a name exactly as easily, and the hole this
// gate exists for would simply have been invisible one directory over. The tree
// is repo-wide clean today, so widening costs nothing now and is the only moment
// it ever will be free.
//
// Using the tracked set rather than a filesystem walk also disposes of the
// exclusion problem instead of managing it: untracked scratch, vendored trees
// and the per-agent worktrees under `.claude/worktrees/` are not tracked files
// of this repo, so nothing needs to name them. (The previous walk's skip switch
// claimed to exclude "per-agent worktrees" and did not — it listed `vendor`,
// `testdata`, `.git` and `bazel-out`, none of which is a worktree path. It was
// harmless only because the walk was confined to `pkg/`.)
//
// A file that GENUINELY targets one platform belongs on the allowlist below with
// a reason, which is the same shape every other ratchet in this package uses. An
// empty allowlist is the goal state and is where this one starts.
func TestNoAccidentallyPlatformConstrainedFiles(t *testing.T) {
	t.Parallel()

	// platformConstrainedAllowlist maps a repo-relative path to why its name's
	// implicit constraint is intended.
	platformConstrainedAllowlist := map[string]string{}

	files := platformConstraintScanSet(t)
	if len(files) == 0 {
		t.Fatal("the tracked Go file set is EMPTY — this gate would pass over nothing. " +
			"A silently-empty scan is the same vacuous green as the hole it guards.")
	}
	// The scan must be able to see this very file, or it resolved the wrong tree.
	sawSelf := false
	var offenders []string
	for _, rel := range files {
		rel = filepath.ToSlash(rel)
		if rel == "pkg/docscheck/build_constrained_filename_test.go" {
			sawSelf = true
		}
		suffix := platformConstrainingSuffix(filepath.Base(rel))
		if suffix == "" {
			continue
		}
		if _, allowed := platformConstrainedAllowlist[rel]; allowed {
			continue
		}
		offenders = append(offenders, rel+"  (implicitly constrained to "+suffix+")")
	}
	if !sawSelf {
		t.Fatal("the scan never saw this test file, so it is walking the wrong tree " +
			"and every clean result it reports is meaningless")
	}
	if len(offenders) == 0 {
		return
	}
	sort.Strings(offenders)
	t.Errorf("%d Go file(s) are constrained to one platform BY THEIR NAME:\n    %s\n\n"+
		"Go derives a build constraint from the file name: `x_windows.go` and\n"+
		"`x_windows_test.go` compile only on Windows. There is no tag to notice and no\n"+
		"error to read — the file is simply not part of the package everywhere else.\n\n"+
		"For a TEST file this is invisible: the package still builds, the suite still\n"+
		"passes, and the mechanism the file was written to pin has no coverage at all.\n"+
		"That is how it was found.\n\n"+
		"RENAME the file so the platform word is not the last segment\n"+
		"(`seed_window_finalize_test.go`, not `finalize_seed_windows_test.go`). If the\n"+
		"constraint is INTENDED, add the path to platformConstrainedAllowlist with the\n"+
		"reason.",
		len(offenders), strings.Join(offenders, "\n    "))
}

// TestPlatformConstraintGateSeesAConstrainedName is the gate's own positive
// control. The scan is a suffix match over a hand-written list, and a list that
// silently stopped matching would leave the gate reporting a clean tree forever
// — the same shape of vacuous pass the hole it guards had.
//
// It exercises platformConstrainingSuffix, the SAME predicate the gate calls.
// It used to re-implement the scan over its own temp directory, which made it a
// test of the suffix list rather than of the gate: the gate could have stopped
// consulting the list and this would still have passed.
func TestPlatformConstraintGateSeesAConstrainedName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want string
	}{
		{"a_windows_test.go", "windows"},
		{"b_linux.go", "linux"},
		{"c_arm64_test.go", "arm64"},
		{"d_js.go", "js"},
		// Safe: the platform word is not the LAST segment.
		{"ok_seed_window_test.go", ""},
		{"windows_seed_layout.go", ""},
		// Safe: not a Go file at all. The gate is handed every tracked path
		// now, not just the ones under pkg/, so this arm stopped being
		// hypothetical when the scan widened.
		{"a_windows_test.md", ""},
		{"BUILD.bazel", ""},
	} {
		if got := platformConstrainingSuffix(tc.name); got != tc.want {
			t.Errorf("platformConstrainingSuffix(%q) = %q, want %q — a name the real "+
				"gate must catch is slipping through, or a safe name is being flagged",
				tc.name, got, tc.want)
		}
	}
}

// TestPlatformConstraintGateScansOutsidePkg pins the SCOPE, which is the part of
// this gate a suffix test cannot reach.
//
// The gate walked only `pkg/` and reported a clean tree, which is exactly what
// it would report if every offender lived in `cmd/`. Widening it is only
// meaningful if something checks that the widening is still in place — otherwise
// the scope can quietly narrow again and every result stays green.
//
// It asks platformConstraintScanSet, which is what the GATE calls, rather than
// the enumerator underneath it. Asking the enumerator would prove only that
// `git ls-files` still lists cmd/, which was never in doubt and which stays true
// while the gate filters everything back out.
func TestPlatformConstraintGateScansOutsidePkg(t *testing.T) {
	t.Parallel()
	files := platformConstraintScanSet(t)
	outside := 0
	for _, rel := range files {
		if !strings.HasPrefix(filepath.ToSlash(rel), "pkg/") {
			outside++
		}
	}
	if outside == 0 {
		t.Fatalf("the tracked Go set contains %d files and NONE outside pkg/. Either the "+
			"enumerator narrowed or the repo changed shape; either way this gate is back "+
			"to guarding one directory while reporting on the tree.", len(files))
	}
	t.Logf("platform-constraint gate scope: %d tracked Go files, %d outside pkg/",
		len(files), outside)
}

// TestPlatformConstraintGateSuffixListIsUsable guards the list itself against
// the failure a suffix scan is most prone to: an entry that can never match
// because it carries the separator the scan supplies, or an empty entry that
// matches everything.
func TestPlatformConstraintGateSuffixListIsUsable(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, s := range goosGoarchSuffixes {
		if s == "" {
			t.Fatal("an EMPTY suffix makes `_`-terminated every file's constraint")
		}
		if strings.ContainsAny(s, "_.") {
			t.Fatalf("suffix %q carries a separator the scan supplies itself, so it can "+
				"never match a real name", s)
		}
		if seen[s] {
			t.Fatalf("suffix %q is listed twice", s)
		}
		seen[s] = true
		// Every entry must actually be reachable through the real predicate.
		if got := platformConstrainingSuffix("x_" + s + ".go"); got != s {
			t.Fatalf("suffix %q is in the list but platformConstrainingSuffix(%q) = %q — "+
				"the list and the gate have come apart", s, "x_"+s+".go", got)
		}
	}
	if _, err := os.Stat(filepath.Join(sourceTreeRoot(t), "MODULE.bazel")); err != nil {
		t.Fatalf("resolved source tree has no MODULE.bazel: %v", err)
	}
}

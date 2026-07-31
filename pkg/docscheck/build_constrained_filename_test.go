package docscheck

import (
	"io/fs"
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
// A file that GENUINELY targets one platform belongs on the allowlist below with
// a reason, which is the same shape every other ratchet in this package uses. An
// empty allowlist is the goal state and is where this one starts.
func TestNoAccidentallyPlatformConstrainedFiles(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	// platformConstrainedAllowlist maps a repo-relative path to why its name's
	// implicit constraint is intended.
	platformConstrainedAllowlist := map[string]string{}

	var offenders []string
	walkErr := filepath.WalkDir(filepath.Join(root, "pkg"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Vendored trees and per-agent worktrees are not this repo's source.
			switch d.Name() {
			case "vendor", "testdata", ".git", "bazel-out":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		base := strings.TrimSuffix(d.Name(), ".go")
		base = strings.TrimSuffix(base, "_test")
		for _, suffix := range goosGoarchSuffixes {
			if !strings.HasSuffix(base, "_"+suffix) {
				continue
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			rel = filepath.ToSlash(rel)
			if _, allowed := platformConstrainedAllowlist[rel]; allowed {
				break
			}
			offenders = append(offenders, rel+"  (implicitly constrained to "+suffix+")")
			break
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking pkg/: %v", walkErr)
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
func TestPlatformConstraintGateSeesAConstrainedName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"a_windows_test.go", "b_linux.go", "c_arm64_test.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package p\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "ok_seed_window_test.go"), []byte("package p\n"), 0o600); err != nil {
		t.Fatalf("write control: %v", err)
	}
	var hits []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		base := strings.TrimSuffix(strings.TrimSuffix(e.Name(), ".go"), "_test")
		for _, suffix := range goosGoarchSuffixes {
			if strings.HasSuffix(base, "_"+suffix) {
				hits = append(hits, e.Name())
				break
			}
		}
	}
	sort.Strings(hits)
	want := []string{"a_windows_test.go", "b_linux.go", "c_arm64_test.go"}
	if len(hits) != len(want) {
		t.Fatalf("the suffix scan matched %v, want exactly %v — a name the real gate\n"+
			"  must catch is slipping through, or the control file is being flagged", hits, want)
	}
	for i := range want {
		if hits[i] != want[i] {
			t.Fatalf("the suffix scan matched %v, want %v", hits, want)
		}
	}
}

package docscheck

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fallbackWalkSkippedTrees names the directories the git-unavailable fallback
// walk must not descend into, keyed by path RELATIVE to the repo root.
//
// It replaces a "skip every directory whose name starts with a dot" rule that
// was wrong in the fail-OPEN direction while its own comment claimed the
// opposite. Tracked files do live under dot-directories: measured over the whole
// tree at d482c92f8, `git ls-files -- '*' | grep -E '^\.[^/]+/'` returns 31
// paths (17 under .claude/skills, 14 under .github) out of 5695 tracked, and the
// dot-rule dropped every one of them. For `*.go` and `*_test.go` the same count
// is 0 today — but that zero is INCIDENTAL, not structural: it becomes non-zero
// the moment a tracked Go file lands under .github/ or .claude/, and nothing
// would announce it. Naming the exclusions makes the scope independent of that
// accident.
//
// Naming them also beats consulting .gitignore. The fallback runs precisely
// BECAUSE git is unavailable, so gitignore semantics — nested .gitignore files,
// negations, pattern precedence — would have to be reimplemented here, a second
// implementation free to diverge silently from the one it is standing in for.
//
// The set is CLOSED, and that is the load-bearing property: a tree that is
// ignored but not listed here gets WALKED, so the walk sees more than the
// tracked set and the gate can only get stricter, never quieter. Quieter is the
// direction that ships bugs for a check whose entire purpose is catching files
// nothing else sees. Every entry earns its place by being unbounded or foreign
// rather than merely ignored — walking one costs orders of magnitude of I/O and
// yields hits that belong to another tree.
var fallbackWalkSkippedTrees = map[string]bool{
	".git":              true, // the object store (a FILE, not a dir, in a linked worktree)
	".claude/worktrees": true, // other agents' complete checkouts of this repo — 5.3G at d482c92f8
	".tools":            true, // downloaded toolchains
	"fdb-record-layer":  true, // the untracked Java reference checkout
	"node_modules":      true,
}

// fallbackWalkSkips reports whether rel (slash-separated, relative to the walk
// root) is an excluded tree. bazel-* is matched by prefix because the workspace
// convenience symlink is named after the checkout directory (bazel-bin,
// bazel-out, bazel-<dirname>), so it cannot be enumerated.
func fallbackWalkSkips(rel string) bool {
	if fallbackWalkSkippedTrees[rel] {
		return true
	}
	return strings.HasPrefix(rel, "bazel-") && !strings.Contains(rel, "/")
}

// sortedFallbackWalkSkips renders the excluded trees for the log line the
// fallback branch emits. The log names them because "fell back to a walk" alone
// does not tell a reader what the walk could not see.
func sortedFallbackWalkSkips() []string {
	out := make([]string, 0, len(fallbackWalkSkippedTrees)+1)
	for rel := range fallbackWalkSkippedTrees {
		out = append(out, rel)
	}
	out = append(out, "bazel-*")
	sort.Strings(out)
	return out
}

// fallbackWalk enumerates files under root whose base name satisfies match,
// returning slash-separated paths relative to root. It is the shared body of
// trackedFiles and trackedGoFiles' git-unavailable branch; sharing it keeps the
// two gates from disagreeing about scope, which is the same reason they share a
// pathspec source on the git branch.
//
// Symlinks are never followed and never reported: the bazel-* convenience links
// point back into the output base, and following them would walk the build tree.
func fallbackWalk(root string, match func(name string) bool) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		if fallbackWalkSkips(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !match(d.Name()) {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	return files, err
}

// TestFallbackWalkCoversTrackedDotDirectories drives the fallback walk DIRECTLY
// — never through trackedFiles/trackedGoFiles, whose branch depends on whether
// git happens to be reachable in the running environment, so the arm under test
// would be exercised or not by accident.
//
// Two arms, and they must fail for different reasons:
//
//	A. tracked dot-directory content is REACHED (.claude/skills, .github). This
//	   is the subset bug: the walk called itself a superset and was not one.
//	B. .git and .claude/worktrees are still EXCLUDED. Walking a sibling agent's
//	   checkout pulls in a duplicate copy of the whole repo and produces foreign
//	   hits that read as real findings.
func TestFallbackWalkCoversTrackedDotDirectories(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// Every fixture file has the same base name so the walk's matcher cannot be
	// what distinguishes them — only the directory decision can.
	fixtures := []string{
		"pkg/docscheck/probe.md",
		".claude/skills/query-engine/probe.md",
		".github/workflows/probe.md",
		".git/objects/probe.md",
		".claude/worktrees/agent-sibling/pkg/docscheck/probe.md",
		".tools/vendor/probe.md",
		"bazel-out/k8-fastbuild/probe.md",
		"fdb-record-layer/core/probe.md",
		"node_modules/dep/probe.md",
	}
	for _, rel := range fixtures {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("creating fixture dir for %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte("probe\n"), 0o644); err != nil {
			t.Fatalf("writing fixture %s: %v", rel, err)
		}
	}

	got, err := fallbackWalk(root, func(name string) bool {
		ok, _ := filepath.Match("*.md", name)
		return ok
	})
	if err != nil {
		t.Fatalf("fallbackWalk: %v", err)
	}
	sort.Strings(got)
	seen := map[string]bool{}
	for _, rel := range got {
		seen[rel] = true
	}

	// Vacuity guard: without this, arm A passes for the wrong reason if the
	// fixture tree ever loses its dot-directory files.
	wantIncluded := []string{
		"pkg/docscheck/probe.md",
		".claude/skills/query-engine/probe.md",
		".github/workflows/probe.md",
	}
	dotIncluded := 0
	for _, rel := range wantIncluded {
		if strings.HasPrefix(rel, ".") {
			dotIncluded++
		}
	}
	if dotIncluded < 2 {
		t.Fatalf("fixture carries %d dot-directory files to include; arm A cannot fail with fewer than 2 "+
			"and would be passing vacuously", dotIncluded)
	}

	// Arm A — the subset bug.
	for _, rel := range wantIncluded {
		if !seen[rel] {
			t.Errorf("arm A: fallback walk dropped %s; it claims to be a superset of the tracked set, and "+
				"tracked files live under .claude/skills and .github (31 such paths at d482c92f8). got %v", rel, got)
		}
	}

	// Arm B — the excluded trees. .claude/worktrees is the one that matters most:
	// it holds other agents' full checkouts, so descending into it both explodes
	// the walk and yields hits belonging to a different tree.
	wantExcluded := []string{
		".git/objects/probe.md",
		".claude/worktrees/agent-sibling/pkg/docscheck/probe.md",
		".tools/vendor/probe.md",
		"bazel-out/k8-fastbuild/probe.md",
		"fdb-record-layer/core/probe.md",
		"node_modules/dep/probe.md",
	}
	for _, rel := range wantExcluded {
		if seen[rel] {
			t.Errorf("arm B: fallback walk descended into an excluded tree and returned %s; got %v", rel, got)
		}
	}

	// A VALUE, not only the relationships above: the two arms partition the
	// fixture, so the exact set is derivable and pinning it catches a walk that
	// satisfied both arms while inventing or losing something else.
	want := []string{".claude/skills/query-engine/probe.md", ".github/workflows/probe.md", "pkg/docscheck/probe.md"}
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("fallback walk set mismatch\n got: %v\nwant: %v", got, want)
	}
}

// TestTrackedEnumerationReportsWhichBranchIsLive records whether the gates run
// on `git ls-files` or on the fallback walk in THIS environment. Both answers
// are legitimate — under `go test` git is normally reachable; under Bazel the
// test runs from a runfiles tree and reaches the real checkout only through
// sourceTreeRoot's MODULE.bazel symlink resolution, and git may not be on PATH
// in the sandbox at all. What is NOT legitimate is not knowing, because it is
// the difference between the fallback being latent and being the CI path.
//
// The assertion is deliberately not "git must win": pinning that would red the
// build on a minimal image, which is exactly the environment the fallback is for.
// What is pinned is that whichever branch runs returns a non-empty set — a green
// from an empty enumeration is how a gate reports "nothing to see" while
// scanning nothing.
func TestTrackedEnumerationReportsWhichBranchIsLive(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	gitPath, lookErr := exec.LookPath("git")
	out, runErr := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*.go").Output()
	live := "fallback filesystem walk"
	if runErr == nil && len(out) > 0 {
		live = "git ls-files"
	}
	t.Logf("BRANCH-PROBE root=%s git-on-PATH=%q (lookup err: %v) ls-files err=%v bytes=%d LIVE BRANCH: %s",
		root, gitPath, lookErr, runErr, len(out), live)

	files := trackedGoFiles(t, root)
	t.Logf("BRANCH-PROBE trackedGoFiles returned %d paths", len(files))
	if len(files) == 0 {
		t.Fatalf("BRANCH-PROBE: %s enumerated zero Go files under %s; a gate scanning an empty set "+
			"reports green while checking nothing", live, root)
	}
}

// TestGitGoFilesIncludesTheWholeDeliverable pins the shared-worktree seam: a
// new source file is part of the build before it is staged, so it must already
// be part of every docs census. The fixture also proves ignored scratch stays
// out and the union cannot report one path twice.
//
// A FAILING `git init` IS A HARD FAILURE HERE, not a skip, and the distinction
// is the whole reason this test exists. gitGoFiles IS the subject; without git
// there is nothing left to exercise, so a skip would report the seam as covered
// while covering nothing — the same green-from-an-empty-set the sibling probe
// above guards against, only quieter, because a skipped test still leaves the
// target green. Measured under bazel on this tree: the test RUNS (`--- PASS`,
// not `--- SKIP`), so git is reachable from the sandbox and the skip arm was
// dead weight that would only ever have fired by hiding a real breakage.
//
// This does NOT contradict TestTrackedEnumerationReportsWhichBranchIsLive
// declining to require git. That test asks which enumeration branch is live and
// must stay honest on a minimal image, because the fallback walk exists for
// exactly that image. This one tests the git branch itself.
func TestGitGoFilesIncludesTheWholeDeliverable(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init unavailable: %v (%s); gitGoFiles is the subject of this test, "+
			"so no git means the tracked+untracked union ships unexercised", err, strings.TrimSpace(string(out)))
	}
	write := func(rel, contents string) {
		t.Helper()
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write(".gitignore", "ignored/\n")
	write("pkg/tracked.go", "package pkg\n")
	write("pkg/deleted.go", "package pkg\n")
	write("pkg/untracked.go", "package pkg\n")
	write("ignored/scratch.go", "package ignored\n")
	write("pkg/not_go.txt", "not go\n")
	if out, err := exec.Command("git", "-C", root, "add", ".gitignore", "pkg/tracked.go", "pkg/deleted.go").CombinedOutput(); err != nil {
		t.Fatalf("git add fixture: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	if err := os.Remove(filepath.Join(root, "pkg/deleted.go")); err != nil {
		t.Fatalf("delete tracked fixture: %v", err)
	}

	got, err := gitGoFiles(root)
	if err != nil {
		t.Fatalf("gitGoFiles: %v", err)
	}
	want := []string{"pkg/tracked.go", "pkg/untracked.go"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("tracked + untracked non-ignored union = %v, want %v", got, want)
	}
	seen := map[string]bool{}
	for _, rel := range got {
		if seen[rel] {
			t.Fatalf("gitGoFiles returned duplicate %q in %v", rel, got)
		}
		seen[rel] = true
	}
}

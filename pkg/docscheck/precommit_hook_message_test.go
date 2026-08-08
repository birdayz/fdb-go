package docscheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The pre-commit hook must name the condition it actually detected.
//
// The hook runs `just generate` and then checks the working tree. Its original
// check was `git diff --exit-code --quiet`, which fails on ANY unstaged change
// to a tracked file — not on codegen output — while the message it printed said
// "'just generate' produced changes. Stage them and retry." Those are two
// different conditions with two different fixes, and the message named the wrong
// one whenever the tree was merely dirty. That is worse than no message: it
// sends the reader somewhere confidently wrong, and it cost two agents a full
// cycle chasing codegen drift that did not exist.
//
// Telling them apart requires snapshotting the tree BEFORE codegen and comparing
// after, which is what the hook now does. This gate pins that the two conditions
// produce DISTINGUISHABLE messages, by constructing each condition and reading
// what comes out.
//
// The hook body is extracted from the `install-hooks` recipe in the justfile,
// which is its tracked source of truth — the installed `.git/hooks/pre-commit`
// is per-clone local state that no test can rely on. `just` is stubbed so each
// condition is constructed exactly rather than approximated by whatever the real
// tree happens to contain, and so the gate never invokes the real (multi-minute)
// build.
func TestPreCommitHookNamesTheConditionItDetected(t *testing.T) {
	t.Parallel()
	hook := extractPreCommitHook(t)

	// The hook must not have regressed to a check that cannot tell the two
	// conditions apart. Pinning the mechanism as well as the messages, because a
	// hook that printed both messages unconditionally would pass the cases below.
	if !strings.Contains(hook, `before="$(git diff)"`) || !strings.Contains(hook, `after="$(git diff)"`) {
		t.Fatalf("the hook no longer snapshots the tree before and after codegen; without\n"+
			"that comparison a dirty tree and genuine codegen drift are indistinguishable\n"+
			"and any message naming either one is a guess. hook:\n%s", hook)
	}
	if !strings.Contains(hook, `before_untracked=`) || !strings.Contains(hook, `after_untracked=`) {
		t.Fatalf("the hook no longer snapshots UNTRACKED files around codegen. `git diff`\n"+
			"reports tracked files only, so codegen that CREATES a file is invisible to it\n"+
			"and the drift check passes over uncommitted codegen output. hook:\n%s", hook)
	}
	// The self-check is the only mechanism positioned to notice that the installed
	// hook and the recipe have diverged, because it runs in the worktree doing the
	// committing — where the local state actually lives. core.hooksPath is one
	// path shared by every worktree, so a stale branch can downgrade the guard for
	// all of them, and nothing else would say so.
	if !strings.Contains(hook, "WARNING: the installed pre-commit hook differs") {
		t.Fatalf("the hook no longer self-checks against the install-hooks recipe. The hook is\n"+
			"shared by every worktree via core.hooksPath, so any branch running\n"+
			"`just install-hooks` overwrites it for all of them — silently, and with no\n"+
			"other mechanism able to notice. hook:\n%s", hook)
	}

	cases := []struct {
		name string
		// setup runs inside a fresh repo whose HEAD is clean.
		setup func(t *testing.T, dir string)
		// driftStub makes the stubbed `just generate` modify a tracked file.
		driftStub bool
		// createStub makes the stubbed `just generate` CREATE an untracked file,
		// which a tracked-only diff cannot see.
		createStub bool
		wantExit0  bool
		want       string
		notWant    string
		why        string
	}{
		{
			name:      "clean tree and codegen produces nothing",
			setup:     func(*testing.T, string) {},
			wantExit0: true,
			notWant:   "ERROR",
			why:       "a clean tree must reach the build steps, not fail the drift check",
		},
		{
			name: "tree dirty BEFORE codegen ran",
			setup: func(t *testing.T, dir string) {
				appendTo(t, filepath.Join(dir, "tracked.txt"), "hand edit\n")
			},
			want:    "the tree was already dirty BEFORE",
			notWant: "'just generate' produced changes",
			why: "this is the condition that burned two cycles: codegen changed nothing, " +
				"so blaming codegen sends the reader to regenerate files that are already current",
		},
		{
			name:      "codegen genuinely produced drift",
			setup:     func(*testing.T, string) {},
			driftStub: true,
			want:      "'just generate' produced changes",
			notWant:   "already dirty BEFORE",
			why:       "real drift must still be reported as drift",
		},
		{
			name: "untracked .go file present",
			setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "widget_test.go"), "package p\n")
			},
			wantExit0: true,
			want:      "widget_test.go",
			why: "git diff ignores untracked files, so a new test file whose BUILD entry is " +
				"missing otherwise surfaces minutes later as a Bazel target error",
		},
		{
			name:       "codegen CREATED an untracked file",
			setup:      func(*testing.T, string) {},
			createStub: true,
			want:       "'just generate' produced changes",
			why: "`git diff` reports tracked files only, so codegen that creates a file — " +
				"gazelle writing a BUILD.bazel for a new package — is invisible to a " +
				"tracked-only snapshot and the hook goes green over uncommitted codegen output",
		},
		{
			name: "pre-existing untracked file is NOT drift",
			setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "scratch.go"), "package p\n")
			},
			wantExit0: true,
			notWant:   "ERROR",
			why: "a scratch file present before codegen appears in both snapshots, so it must " +
				"not be mistaken for drift — scratch files are legitimate and must not fail a commit",
		},
		{
			name: "tree dirty AND codegen drifted",
			setup: func(t *testing.T, dir string) {
				appendTo(t, filepath.Join(dir, "tracked.txt"), "hand edit\n")
			},
			driftStub: true,
			want:      "'just generate' produced changes",
			why:       "when both hold, the actionable one is the drift — regenerating is required either way",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := newStubRepo(t, hook, tc.driftStub, tc.createStub)
			tc.setup(t, dir)

			cmd := exec.Command("bash", filepath.Join(dir, "hook.sh"))
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "stub")+":"+os.Getenv("PATH"))
			raw, err := cmd.CombinedOutput()
			out := string(raw)

			if tc.wantExit0 && err != nil {
				t.Fatalf("hook failed but the condition is not a failure (%s):\n%s", tc.why, out)
			}
			if !tc.wantExit0 && err == nil {
				t.Fatalf("hook passed but the condition must fail (%s):\n%s", tc.why, out)
			}
			if tc.want != "" && !strings.Contains(out, tc.want) {
				t.Fatalf("hook message does not name the condition it detected.\n"+
					"  want substring: %q\n  why it matters: %s\n  got:\n%s", tc.want, tc.why, out)
			}
			if tc.notWant != "" && strings.Contains(out, tc.notWant) {
				t.Fatalf("hook message names the WRONG condition — a reader following it is sent\n"+
					"  somewhere confidently wrong.\n  must NOT contain: %q\n  why: %s\n  got:\n%s",
					tc.notWant, tc.why, out)
			}
		})
	}
}

// The INSTALLED hook must match the recipe it is supposed to have come from.
//
// The gate above reads the recipe, which is the tracked source of truth. Nothing
// in it can see the file git actually executes: that is per-clone local state
// written by `just install-hooks`. It is worth checking anyway, because
// core.hooksPath here is one absolute path SHARED by every worktree, so any
// worktree on any branch can overwrite the hook for all of them — and a stale
// branch installing an older body silently downgrades the guard everywhere.
//
// This is the belt to the hook's own self-check (which warns at commit time, in
// the tree doing the committing). When the artifact is genuinely absent — no
// hook installed, or a sandbox with no access to the real .git — there is
// nothing to compare, and the test says so rather than reporting a green it did
// not earn. That is a report of an absent artifact, not a deferred failure.
func TestInstalledPreCommitHookMatchesTheRecipe(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	out, err := exec.Command("git", "-C", root, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		t.Logf("NOT MEASURED: no git common dir reachable from %s (%v). The installed hook "+
			"could not be compared; the recipe-side gate above still ran.", root, err)
		return
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	path := filepath.Join(dir, "hooks", "pre-commit")

	installed, err := os.ReadFile(path)
	if err != nil {
		t.Logf("NOT MEASURED: no pre-commit hook installed at %s (%v). Nothing to compare — "+
			"run `just install-hooks` to install one.", path, err)
		return
	}

	want := extractPreCommitHook(t)
	if strings.TrimRight(string(installed), "\n") != strings.TrimRight(want, "\n") {
		t.Fatalf("the INSTALLED pre-commit hook does not match this tree's install-hooks recipe.\n"+
			"  installed: %s\n"+
			"  The hook is shared by every worktree via core.hooksPath, so a stale branch\n"+
			"  installing an older body silently downgrades the guard for everyone.\n"+
			"  Run `just install-hooks` from this tree to resync.", path)
	}
}

// extractPreCommitHook pulls the hook body out of the justfile's install-hooks
// recipe, dedenting the recipe indentation just as `just` does before writing it.
func extractPreCommitHook(t *testing.T) string {
	t.Helper()
	body := readDoc(t, repoRoot(t), "justfile")
	const open = "<< 'HOOK'"
	i := strings.Index(body, open)
	if i < 0 {
		t.Fatalf("justfile has no install-hooks heredoc (%q); the pre-commit hook's tracked "+
			"source of truth moved and this gate is now measuring nothing", open)
	}
	lines := strings.Split(body[i:], "\n")[1:]
	var out []string
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "HOOK" {
			return strings.Join(out, "\n") + "\n"
		}
		out = append(out, strings.TrimPrefix(ln, "    "))
	}
	t.Fatal("install-hooks heredoc is never terminated by HOOK")
	return ""
}

// newStubRepo builds a throwaway git repo containing the hook and a stubbed
// `just`, and returns its path.
func newStubRepo(t *testing.T, hook string, drift, create bool) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hook.sh"), hook)

	stub := filepath.Join(dir, "stub")
	if err := os.MkdirAll(stub, 0o755); err != nil {
		t.Fatalf("mkdir stub: %v", err)
	}
	gen := "true"
	if drift {
		gen = `echo regenerated >> generated.txt`
	}
	if create {
		gen = `echo 'go_library(name = "x")' > BUILD.bazel`
	}
	writeFile(t, filepath.Join(stub, "just"),
		"#!/usr/bin/env bash\nif [ \"${1:-}\" = generate ]; then "+gen+"; fi\nexit 0\n")
	if err := os.Chmod(filepath.Join(stub, "just"), 0o755); err != nil {
		t.Fatalf("chmod stub just: %v", err)
	}

	writeFile(t, filepath.Join(dir, "tracked.txt"), "base\n")
	writeFile(t, filepath.Join(dir, "generated.txt"), "gen\n")
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"config", "user.email", "gate@example.com"},
		{"config", "user.name", "gate"},
		{"add", "tracked.txt", "generated.txt"},
		{"commit", "-qm", "base"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func appendTo(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}

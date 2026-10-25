package infra

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPrecommitFastLane(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../justfile")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(data), "<< 'HOOK'\n")
	if !ok {
		t.Fatal("missing pre-commit hook")
	}
	hook, _, ok := strings.Cut(rest, "\n    HOOK")
	if !ok {
		t.Fatal("missing hook terminator")
	}
	for _, tc := range []struct {
		name, secretExit, dirtyExit, testExit, want string
		fail                                        bool
	}{
		{"clean", "0", "0", "0", "secret-scan\ntest\n", false},
		{"secret_failure", "1", "0", "0", "secret-scan\n", true},
		{"dirty_tree", "0", "1", "0", "secret-scan\n", true},
		{"test_failure", "0", "0", "1", "secret-scan\ntest\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for name, body := range map[string]string{
				"just": "#!/bin/bash\nprintf '%s\\n' \"$*\" >> \"$CALLS\"\nif [ \"$1\" = secret-scan ]; then exit \"$SECRET_EXIT\"; fi\nexit \"$TEST_EXIT\"\n",
				"git":  "#!/bin/bash\nif [ \"$*\" = 'diff --quiet' ]; then exit \"$DIRTY_EXIT\"; fi\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			calls := filepath.Join(dir, "calls")
			cmd := exec.Command("bash", "-c", hook)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CALLS="+calls, "SECRET_EXIT="+tc.secretExit, "DIRTY_EXIT="+tc.dirtyExit, "TEST_EXIT="+tc.testExit)
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("exit=%v, want failure=%v: %s", err, tc.fail, out)
			}
			got, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("hook commands=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestJustTestLanes(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../justfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, recipe, labels, args, want string
		queryFail, testFail, fail        bool
	}{
		{name: "fast", recipe: "test", want: "test //... --build_tests_only --test_tag_filters=-test-full,-conformance_java,-stress"},
		{name: "full_includes_explicit_manual", recipe: "test-full", labels: "//pkg:unit_test\n//pkg:manual_test", want: "test //pkg:unit_test //pkg:manual_test"},
		{name: "fast_args", recipe: "test", args: "--test_output=errors", want: "test //... --build_tests_only --test_tag_filters=-test-full,-conformance_java,-stress --test_output=errors"},
		{name: "full_args", recipe: "test-full", labels: "//pkg:unit_test", args: "--test_output=errors", want: "test //pkg:unit_test --test_output=errors"},
		{name: "empty_is_not_green", recipe: "test-full", fail: true},
		{name: "query_failure", recipe: "test-full", labels: "//pkg:partial_test", queryFail: true, fail: true},
		{name: "full_test_failure", recipe: "test-full", labels: "//pkg:unit_test", want: "test //pkg:unit_test", testFail: true, fail: true},
		{name: "fast_test_failure", recipe: "test", want: "test //... --build_tests_only --test_tag_filters=-test-full,-conformance_java,-stress", testFail: true, fail: true},
		// test_full_first is stubbed as "//pkg:slow_test //pkg:gone_test": a
		// listed pole moves to the front, a stale entry is dropped, and every
		// queried label is still passed exactly once.
		{name: "full_poles_first", recipe: "test-full", labels: "//pkg:a_test\n//pkg:slow_test\n//pkg:z_test", want: "test //pkg:slow_test //pkg:a_test //pkg:z_test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, rest, ok := strings.Cut(string(data), "\n"+tc.recipe+" *args:\n")
			if !ok {
				t.Fatalf("missing recipe %s", tc.recipe)
			}
			body, _, _ := strings.Cut(rest, "\n\n")
			body = strings.ReplaceAll(body, "{{args}}", tc.args)
			body = strings.ReplaceAll(body, "{{test_full_first}}", "//pkg:slow_test //pkg:gone_test")
			dir := t.TempDir()
			stub := `#!/bin/bash
if [ "$1" = query ]; then
    [ "$2" = 'kind(".*_test", //...) except attr(tags, "realcluster", //...)' ] || exit 91
    printf '%s\n' "$LABELS"
    exit "$QUERY_EXIT"
fi
printf '%s\n' "$*" > "$CALLS"
exit "$TEST_EXIT"
`
			// Written under syscall.ForkLock: these subtests run in parallel, and a
			// fork while the stub's write fd is open hands that fd to the child, so
			// exec'ing the stub fails with ETXTBSY. In the cases that expect a
			// failing lane that looked like a pass with no Bazel invocation.
			syscall.ForkLock.Lock()
			err := os.WriteFile(filepath.Join(dir, "bazelisk"), []byte(stub), 0o700)
			syscall.ForkLock.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			calls := filepath.Join(dir, "calls")
			queryExit, testExit := "0", "0"
			if tc.queryFail {
				queryExit = "1"
			}
			if tc.testFail {
				testExit = "1"
			}
			cmd := exec.Command("bash", "-c", body)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "LABELS="+tc.labels, "CALLS="+calls, "QUERY_EXIT="+queryExit, "TEST_EXIT="+testExit)
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("exit=%v, want failure=%v: %s", err, tc.fail, out)
			}
			got, err := os.ReadFile(calls)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(got)) != tc.want {
				t.Fatalf("Bazel invocation=%q, want %q", got, tc.want)
			}
		})
	}
}

// TestWorkflowsUseTheCIConfig requires every Bazel command a workflow runs to
// pass --config=ci, so the CI-only settings in .bazelrc (test:ci: the runner's
// memory fraction, the detailed summary, the profile) reach every lane rather
// than only the ones someone remembered.
func TestWorkflowsUseTheCIConfig(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("../.github/workflows/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no workflow files found: the check would pass over nothing")
	}
	cmd := regexp.MustCompile(`\bbazelisk((?:\s+--\S+|\s+\\\n\s*)*)\s+(test|build|query|cquery|aquery|coverage|run|info|fetch|mod)\b(\s+--config=ci)?`)
	checked := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range cmd.FindAllStringSubmatchIndex(string(data), -1) {
			lineStart := strings.LastIndex(string(data[:m[0]]), "\n") + 1
			if strings.HasPrefix(strings.TrimSpace(string(data[lineStart:m[0]])), "#") {
				continue // prose in a comment
			}
			checked++
			if m[6] < 0 {
				line := strings.Count(string(data[:m[0]]), "\n") + 1
				t.Errorf("%s:%d: %q runs without --config=ci", filepath.Base(f), line, string(data[m[0]:m[1]]))
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no bazelisk commands in the workflows: the check matched nothing")
	}
	t.Logf("%d workflow Bazel commands pass --config=ci", checked)
}

// laneHarness runs run-verified-lane.sh against a fake just in a throwaway repo.
// The fake starts a lingering daemon, as bazelisk starts a server, and probes the
// lock from inside the lane.
type laneHarness struct {
	t                              *testing.T
	script, bin, repo, lock, state string
}

func newLaneHarness(t *testing.T) *laneHarness {
	t.Helper()
	script, err := filepath.Abs("../tools/lanes/run-verified-lane.sh")
	if err != nil {
		t.Fatal(err)
	}
	h := &laneHarness{t: t, script: script, bin: t.TempDir(), repo: t.TempDir(), state: t.TempDir()}
	h.lock = filepath.Join(h.state, "lane.lock")
	for name, body := range map[string]string{
		"just": `#!/bin/bash
if [ "$1" = --dry-run ]; then echo fake; exit 0; fi
if [ "$1" = --evaluate ]; then echo "$STATE/race_base"; exit 0; fi
sleep 300 </dev/null >/dev/null 2>&1 &
echo $! > "$STATE/daemon.pid"
if flock -n "$FDB_LANE_LOCK" true; then echo free > "$STATE/probe"; else echo held > "$STATE/probe"; fi
echo $$ > "$STATE/just.pid"
printf '%s\n' "$@" > "$STATE/just.args"
if [ -n "$BLOCK" ]; then exec sleep 300; fi
exit "${JUST_EXIT:-0}"
`,
		"bazelisk": `#!/bin/bash
if flock -n "$FDB_LANE_LOCK" true; then echo free > "$STATE/shutdown-probe"; else echo held > "$STATE/shutdown-probe"; fi
printf '%s\n' "$*" > "$STATE/bazelisk.args"
`,
	} {
		if err := os.WriteFile(filepath.Join(h.bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = h.repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	t.Cleanup(func() {
		for _, f := range []string{"daemon.pid", "just.pid"} {
			if pid := h.pid(f); pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	return h
}

func (h *laneHarness) command(block bool, env ...string) *exec.Cmd {
	return h.laneCommand("fast", block, env...)
}

func (h *laneHarness) laneCommand(lane string, block bool, env ...string) *exec.Cmd {
	return h.laneCommandArgs(lane, block, nil, env...)
}

func (h *laneHarness) laneCommandArgs(lane string, block bool, extra []string, env ...string) *exec.Cmd {
	cmd := exec.Command("bash", append([]string{h.script, h.repo, lane}, extra...)...)
	cmd.Env = append(os.Environ(), "PATH="+h.bin+":"+os.Getenv("PATH"), "FDB_LANE_LOCK="+h.lock, "STATE="+h.state, "TMPDIR="+h.t.TempDir())
	if block {
		cmd.Env = append(cmd.Env, "BLOCK=1")
	}
	cmd.Env = append(cmd.Env, env...)
	return cmd
}

func (h *laneHarness) pid(name string) int {
	raw, err := os.ReadFile(filepath.Join(h.state, name))
	if err != nil {
		return 0
	}
	var pid int
	if _, err := fmt.Sscan(string(raw), &pid); err != nil {
		return 0
	}
	return pid
}

func (h *laneHarness) waitFor(name string) int {
	h.t.Helper()
	for i := 0; i < 3000; i++ {
		if pid := h.pid(name); pid > 0 {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("fake just never wrote %s", name)
	return 0
}

// lockFree reports whether the lane lock can be taken now.
func (h *laneHarness) lockFree() bool {
	h.t.Helper()
	f, err := os.OpenFile(h.lock, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		h.t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return true
}

func (h *laneHarness) alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// The lane's Bazel server outlives the lane. If it inherits the lock descriptor,
// a killed lane leaves every later heavy lane blocked on flock.
func TestVerifiedLaneLock(t *testing.T) {
	t.Parallel()
	t.Run("held_during_lane_free_after_with_daemon", func(t *testing.T) {
		t.Parallel()
		h := newLaneHarness(t)
		out, err := h.command(false).CombinedOutput()
		if err != nil {
			t.Fatalf("lane: %v: %s", err, out)
		}
		if args, err := os.ReadFile(filepath.Join(h.state, "bazelisk.args")); err != nil || string(args) != "shutdown\n" {
			t.Fatalf("shutdown args = %q (%v), want \"shutdown\"", args, err)
		}
		if meta := laneMetadata(t, out); !strings.Contains(meta, "shutdown: bazelisk shutdown exit=0\n") {
			t.Fatalf("metadata lacks the fast-lane shutdown line:\n%s", meta)
		}
		probe, err := os.ReadFile(filepath.Join(h.state, "probe"))
		if err != nil || strings.TrimSpace(string(probe)) != "held" {
			t.Fatalf("lock during the lane = %q (%v), want held", probe, err)
		}
		probe, err = os.ReadFile(filepath.Join(h.state, "shutdown-probe"))
		if err != nil || strings.TrimSpace(string(probe)) != "held" {
			t.Fatalf("lock during the server shutdown = %q (%v), want held", probe, err)
		}
		daemon := h.pid("daemon.pid")
		if daemon == 0 || !h.alive(daemon) {
			t.Fatalf("daemon %d is not running, so the check below would be vacuous", daemon)
		}
		if !h.lockFree() {
			t.Fatalf("lane lock still held after the lane exited (by its daemon %d)", daemon)
		}
	})
	t.Run("race_lane_shuts_down_the_race_output_base", func(t *testing.T) {
		t.Parallel()
		h := newLaneHarness(t)
		out, err := h.laneCommand("race", false).CombinedOutput()
		if err != nil {
			t.Fatalf("lane: %v: %s", err, out)
		}
		args, err := os.ReadFile(filepath.Join(h.state, "bazelisk.args"))
		want := "--output_base=" + filepath.Join(h.state, "race_base") + " shutdown"
		if err != nil || strings.TrimSpace(string(args)) != want {
			t.Fatalf("shutdown args = %q (%v), want %q", args, err, want)
		}
		meta := laneMetadata(t, out)
		if !strings.Contains(meta, "shutdown: bazelisk "+want+" exit=0\n") {
			t.Fatalf("metadata lacks the race shutdown line:\n%s", meta)
		}
	})
	t.Run("recipe_and_extra_args_reach_just", func(t *testing.T) {
		t.Parallel()
		h := newLaneHarness(t)
		extra := []string{"--test_arg=a b", "x"}
		if out, err := h.laneCommandArgs("full", false, extra).CombinedOutput(); err != nil {
			t.Fatalf("lane: %v: %s", err, out)
		}
		got, err := os.ReadFile(filepath.Join(h.state, "just.args"))
		if want := "test-full\n--test_arg=a b\nx\n"; err != nil || string(got) != want {
			t.Fatalf("just args = %q (%v), want %q", got, err, want)
		}
	})
	t.Run("failing_lane_exit_status_reaches_caller", func(t *testing.T) {
		t.Parallel()
		h := newLaneHarness(t)
		out, err := h.command(false, "JUST_EXIT=7").CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
			t.Fatalf("lane exit = %v, want 7: %s", err, out)
		}
		if meta := laneMetadata(t, out); !strings.Contains(meta, "lane_exit=7 tee_exit=0 ") {
			t.Fatalf("metadata lacks lane_exit=7:\n%s", meta)
		}
	})
	t.Run("killing_only_the_script_keeps_lock_until_lane_ends", func(t *testing.T) {
		t.Parallel()
		h := newLaneHarness(t)
		cmd := h.command(true)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		just := h.waitFor("just.pid")
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()
		if !h.alive(just) {
			t.Fatalf("lane %d died with the script, so the check below would be vacuous", just)
		}
		if h.lockFree() {
			t.Fatal("lock released while the killed script's lane is still running")
		}
		if err := syscall.Kill(just, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		for i := 0; !h.lockFree(); i++ {
			if i == 3000 {
				t.Fatalf("lock still held after the lane ended (daemon %d alive=%v)", h.pid("daemon.pid"), h.alive(h.pid("daemon.pid")))
			}
			time.Sleep(10 * time.Millisecond)
		}
		if d := h.pid("daemon.pid"); d == 0 || !h.alive(d) {
			t.Fatalf("daemon %d is not running, so the release check was vacuous", d)
		}
	})
}

// laneMetadata reads the evidence metadata of a lane run; the script prints the
// evidence directory on its first output line.
func laneMetadata(t *testing.T, out []byte) string {
	t.Helper()
	dir, _, _ := strings.Cut(string(out), "\n")
	meta, err := os.ReadFile(filepath.Join(dir, "metadata"))
	if err != nil {
		t.Fatalf("evidence metadata: %v (output %q)", err, out)
	}
	return string(meta)
}

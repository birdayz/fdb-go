package infra

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

type fullWorkflowHarness struct {
	*laneHarness
	env     []string
	outputs map[string]string
	sha     string
}

func evidenceFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// Parallel tests can fork while another fixture is writing an executable.
	syscall.ForkLock.Lock()
	err := os.WriteFile(path, data, mode)
	syscall.ForkLock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

func newFullWorkflowHarness(t *testing.T, laneExit string) *fullWorkflowHarness {
	t.Helper()
	h := &fullWorkflowHarness{laneHarness: newLaneHarness(t), outputs: map[string]string{}}
	wrapper, err := os.ReadFile(h.script)
	if err != nil {
		t.Fatal(err)
	}
	evidenceFile(t, filepath.Join(h.repo, "tools/lanes/run-verified-lane.sh"), wrapper, 0o600)
	evidenceFile(t, filepath.Join(h.repo, "tracked-input"), []byte("before\n"), 0o600)
	for _, args := range [][]string{{"add", "."}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = h.repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = h.repo
	head, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	h.sha = strings.TrimSpace(string(head))
	reporter := os.Getenv("EVIDENCE_TEST_REPORTER")
	if reporter == "" {
		t.Fatal("real reporter is not declared as test data")
	}
	if !filepath.IsAbs(reporter) {
		root, workspace := os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE")
		if root == "" || workspace == "" {
			t.Fatal("real reporter requires the Bazel runfiles environment")
		}
		reporter = filepath.Join(root, workspace, reporter)
	}
	for name, body := range map[string]string{
		"docker": "#!/bin/bash\n[ \"$*\" = info ] || exit 90\necho docker-info > \"$STATE/docker.called\"\n",
		"bazelisk": `#!/bin/bash
printf '%s\n' "$*" >> "$STATE/bazel.calls"
if [ -e "$STATE/bazel.stopped" ]; then
 echo 'fixture: Bazel invoked after shutdown' >&2
 exit 94
fi
reporter="$STATE/bin/cmd/test-report/test-report_/test-report"
case "$1" in
 build)
  [ "$*" = 'build --config=ci //cmd/test-report' ] || exit 91
  [ ! -e "$STATE/reporter.built" ] || exit 96
  mkdir -p "$(dirname "$reporter")" || exit 97
  ln -s "$REAL_REPORTER" "$reporter" || exit 97
  : > "$STATE/reporter.built"
  ;;
 info)
  [ "$*" = 'info --config=ci bazel-bin' ] || exit 92
  if [ ! -e "$STATE/reporter.built" ] || [ ! -x "$reporter" ]; then
   echo 'fixture: reporter must be built before info' >&2
   exit 95
  fi
  : > "$STATE/reporter.located"
  echo "$STATE/bin"
  ;;
 shutdown)
  [ -e "$STATE/reporter.located" ] || exit 98
  : > "$STATE/bazel.stopped"
  ;;
 *) exit 93 ;;
esac
`,
		"just": `#!/bin/bash
if [ "$1" = --dry-run ]; then echo fake-full-command; exit 0; fi
reporter="$STATE/bin/cmd/test-report/test-report_/test-report"
[ -x "$reporter" ] || exit 99
rm "$reporter" || exit 99
printf 'removed\n' > "$STATE/reporter.invalidated"
printf '%s\n' "$@" > "$STATE/just.args"
for arg in "$@"; do
 case "$arg" in
  --build_event_json_file=*) bep=${arg#*=} ;;
  --profile=*) profile=${arg#*=} ;;
 esac
done
case "${RESULT_MODE:-normal}" in
 missing) : ;;
 empty) : > "$bep" ;;
 malformed) echo '{}' > "$bep" ;;
 *) cp "$STATE/result.bep" "$bep" ;;
esac
case "${RESULT_MODE:-normal}" in
 missing-profile) : ;;
 empty-profile) : > "$profile" ;;
 *) cp "$STATE/profile.gz" "$profile" ;;
esac
case "${MUTATION:-none}" in
 tracked) echo after > tracked-input ;;
 untracked) echo new > untracked-input ;;
 head) git -c user.name=t -c user.email=t@t commit --allow-empty -qm changed-head ;;
esac
exit "$JUST_EXIT"
`,
	} {
		evidenceFile(t, filepath.Join(h.bin, name), []byte(body), 0o700)
	}
	junit := `<testsuites><testsuite><testcase name="FixtureProof"/></testsuite></testsuites>`
	status := "PASSED"
	if laneExit != "0" {
		junit = `<testsuites><testsuite><testcase name="FixtureProof"><failure message="fixture failure"/></testcase></testsuite></testsuites>`
		status = "FAILED"
	}
	evidenceFile(t, filepath.Join(h.state, "test.xml"), []byte(junit), 0o600)
	bep, err := json.Marshal(map[string]any{
		"id":         map[string]any{"testResult": map[string]any{"label": "//fixture:proof", "run": 1, "shard": 1, "attempt": 1}},
		"testResult": map[string]any{"status": status, "executionInfo": map[string]bool{"cachedRemotely": true}, "testActionOutput": []map[string]string{{"name": "test.xml", "uri": "file://" + filepath.Join(h.state, "test.xml")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	evidenceFile(t, filepath.Join(h.state, "result.bep"), bep, 0o600)
	var profile bytes.Buffer
	gz := gzip.NewWriter(&profile)
	if _, err := gz.Write([]byte(`{"traceEvents":[],"fixture":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	evidenceFile(t, filepath.Join(h.state, "profile.gz"), profile.Bytes(), 0o600)
	h.env = h.command(false, "GITHUB_WORKSPACE="+h.repo, "GITHUB_SHA="+h.sha, "RUNNER_TEMP="+t.TempDir(), "GITHUB_OUTPUT="+filepath.Join(h.state, "outputs"), "GITHUB_SERVER_URL=https://github.com", "GITHUB_REPOSITORY=birdayz/fdb-go", "GITHUB_RUN_ID=12345", "JUST_EXIT="+laneExit, "REAL_REPORTER="+reporter).Env
	return h
}

// step executes a shell body directly for shell-fault controls. The lifecycle
// runner below also enforces the declared stage order and scheduling conditions.
func (h *fullWorkflowHarness) step(id string, extra ...string) (string, error) {
	h.t.Helper()
	return h.stepScript(id, fullLaneStep(h.t, id).Run, extra...)
}

func (h *fullWorkflowHarness) stepScript(id, script string, extra ...string) (string, error) {
	h.t.Helper()
	step := fullLaneStep(h.t, id)
	env := append([]string{}, h.env...)
	for key, value := range step.Env {
		const prefix = "${{ steps.prepare.outputs."
		if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, " }}") {
			h.t.Fatalf("unsupported workflow binding %s=%q", key, value)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(value, prefix), " }}")
		resolved, ok := h.outputs[name]
		if !ok {
			h.t.Fatalf("prepare did not produce %s", name)
		}
		env = append(env, key+"="+resolved)
	}
	env = append(env, extra...)
	// GitHub's default Linux run shell invokes bash with errexit enabled.
	cmd := exec.Command("bash", "-e", "-c", script)
	cmd.Dir = h.repo
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if id == "prepare" {
		data, readErr := os.ReadFile(filepath.Join(h.state, "outputs"))
		if readErr != nil && !os.IsNotExist(readErr) {
			h.t.Fatal(readErr)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				h.outputs[k] = v
			}
		}
	}
	return string(out), err
}

func requireStepExit(t *testing.T, out string, err error, want int) {
	t.Helper()
	if want == 0 {
		if err != nil {
			t.Fatalf("step: %v\n%s", err, out)
		}
		return
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != want {
		t.Fatalf("step exit=%v, want %d\n%s", err, want, out)
	}
}

func (h *fullWorkflowHarness) read(name string) string {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.outputs["evidence"], name))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(data)
}

func (h *fullWorkflowHarness) bazelCalls() string {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.state, "bazel.calls"))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(data)
}

func (h *fullWorkflowHarness) bazelReporter() string {
	return filepath.Join(h.state, "bin/cmd/test-report/test-report_/test-report")
}

func (h *fullWorkflowHarness) requirePreparedReporter() {
	h.t.Helper()
	reporter := h.outputs["reporter"]
	if reporter != filepath.Join(h.outputs["evidence"], "test-report") || !filepath.IsAbs(reporter) {
		h.t.Fatalf("reporter must be retained in external evidence storage, got %q", reporter)
	}
	copied, err := os.Lstat(reporter)
	if err != nil || !copied.Mode().IsRegular() || copied.Mode().Perm()&0o111 == 0 {
		h.t.Fatalf("retained reporter is not a regular executable: %v (%v)", copied, err)
	}
	original, err := os.Stat(h.bazelReporter())
	if err != nil || os.SameFile(copied, original) {
		h.t.Fatalf("reporter must be an independent copy of the built executable: %v", err)
	}
}

func (h *fullWorkflowHarness) requireReporterInvalidated() {
	h.t.Helper()
	if data, err := os.ReadFile(filepath.Join(h.state, "reporter.invalidated")); err != nil || string(data) != "removed\n" {
		h.t.Fatalf("lane did not apply reporter-output invalidation: %q %v", data, err)
	}
	if _, err := os.Lstat(h.bazelReporter()); !os.IsNotExist(err) {
		h.t.Fatalf("Bazel-owned reporter path survived the intervening lane: %v", err)
	}
}

type evidenceStageResult struct {
	scheduled     bool
	output        string
	err           error
	bazelCalls    string
	artifactPath  string
	artifactFiles []string
}

func (h *fullWorkflowHarness) runDeclaredWorkflow() map[string]evidenceStageResult {
	h.t.Helper()
	stages, err := fullLaneStages(readEvidenceWorkflow(h.t).Jobs["full"].Steps)
	if err != nil {
		h.t.Fatal(err)
	}
	state := evidenceScheduleState{prepare: "skipped", full: "skipped", report: "skipped"}
	results := map[string]evidenceStageResult{}
	for _, stage := range stages {
		state.evidence = h.outputs["evidence"] != ""
		result := evidenceStageResult{scheduled: evidenceStageScheduled(stage, state)}
		if !result.scheduled {
			results[stage.name] = result
			continue
		}
		if stage.name == "upload" {
			// No network upload is simulated: inspect the local artifact handoff
			// only after the declared upload condition actually schedules it.
			result.artifactPath = strings.ReplaceAll(stage.With["path"], "${{ steps.prepare.outputs.evidence }}", h.outputs["evidence"])
			entries, err := os.ReadDir(result.artifactPath)
			if err != nil || len(entries) == 0 {
				h.t.Fatalf("scheduled upload has no artifact population: %v (%d entries)", err, len(entries))
			}
			for _, entry := range entries {
				result.artifactFiles = append(result.artifactFiles, entry.Name())
			}
		} else {
			result.output, result.err = h.stepScript(stage.name, stage.Run)
			outcome := "success"
			if result.err != nil {
				outcome = "failure"
			}
			switch stage.name {
			case "prepare":
				state.prepare = outcome
				if result.err == nil {
					h.requirePreparedReporter()
				}
			case "full":
				state.full = outcome
			case "report":
				state.report = outcome
			}
			calls, err := os.ReadFile(filepath.Join(h.state, "bazel.calls"))
			if err != nil && !os.IsNotExist(err) {
				h.t.Fatal(err)
			}
			result.bazelCalls = string(calls)
		}
		results[stage.name] = result
	}
	return results
}

func TestFullLaneWorkflowLifecycle(t *testing.T) {
	t.Parallel()
	for _, exit := range []string{"0", "7"} {
		t.Run("lane_exit_"+exit, func(t *testing.T) {
			t.Parallel()
			h := newFullWorkflowHarness(t, exit)
			results := h.runDeclaredWorkflow()
			h.requireReporterInvalidated()
			for _, name := range []string{"prepare", "full", "report", "upload"} {
				if !results[name].scheduled {
					t.Fatalf("%s was not scheduled after lane exit %s", name, exit)
				}
			}
			prepare, full, report, upload := results["prepare"], results["full"], results["report"], results["upload"]
			requireStepExit(t, prepare.output, prepare.err, 0)
			const preparedCalls = "build --config=ci //cmd/test-report\ninfo --config=ci bazel-bin\n"
			if prepare.bazelCalls != preparedCalls {
				t.Fatalf("prepare Bazel calls=%q, want %q", prepare.bazelCalls, preparedCalls)
			}
			want := 0
			if exit == "7" {
				want = 7
			}
			requireStepExit(t, full.output, full.err, want)
			if want := preparedCalls + "shutdown\n"; full.bazelCalls != want {
				t.Fatalf("full-lane Bazel calls=%q, want %q", full.bazelCalls, want)
			}
			requireStepExit(t, report.output, report.err, 0)
			if report.bazelCalls != full.bazelCalls {
				t.Fatalf("report rendering invoked Bazel after shutdown: before=%q after=%q", full.bazelCalls, report.bazelCalls)
			}
			if upload.artifactPath != h.outputs["evidence"] || !slices.Contains(upload.artifactFiles, "test-report.html") {
				t.Fatalf("scheduled upload lost the rendered report handoff: path=%q files=%v", upload.artifactPath, upload.artifactFiles)
			}
			for _, file := range []string{"dispatch.txt", "test-report", "bep.jsonl", "bazel-profile.json.gz", "test-report.html", "driver.log", "verified-lane/head.before", "verified-lane/head.after", "verified-lane/inputs.before", "verified-lane/inputs.after", "verified-lane/metadata", "verified-lane/lane.rc", "verified-lane/shutdown.rc"} {
				if len(h.read(file)) == 0 {
					t.Errorf("empty artifact %s", file)
				}
			}
			for _, pair := range [][2]string{{"result.bep", "bep.jsonl"}, {"profile.gz", "bazel-profile.json.gz"}} {
				source, err := os.ReadFile(filepath.Join(h.state, pair[0]))
				if err != nil || !bytes.Equal(source, []byte(h.read(pair[1]))) {
					t.Fatalf("artifact %s differs from the controlled lane output: %v", pair[1], err)
				}
			}
			verdict := `stat-pass"><span class="val">1</span>`
			if exit == "7" {
				verdict = `stat-fail"><span class="val">1</span>`
			}
			if !strings.Contains(h.read("test-report.html"), verdict) {
				t.Fatal("report lost the controlled test verdict")
			}
			for _, name := range []string{"verified-lane/head.before", "verified-lane/head.after"} {
				if strings.TrimSpace(h.read(name)) != h.sha {
					t.Errorf("wrong SHA in %s", name)
				}
			}
			if h.read("verified-lane/inputs.before") != h.read("verified-lane/inputs.after") {
				t.Fatal("fixture changed inputs")
			}
			if !strings.Contains(h.read("verified-lane/metadata"), "lane_exit="+exit+" tee_exit=0 inputs_changed=0") {
				t.Fatal("incorrect lane metadata")
			}
			if !strings.Contains(h.read("dispatch.txt"), "sha="+h.sha+"\nrun=https://github.com/birdayz/fdb-go/actions/runs/12345\nlane=full-default-budgets") {
				t.Fatal("incorrect dispatch provenance")
			}
			for _, text := range []string{h.sha, "https://github.com/birdayz/fdb-go/actions/runs/12345", "full-default-budgets", "realcluster targets", "FixtureProof", "Final run/shard results: 0 executed, 1 cached."} {
				if !strings.Contains(h.read("test-report.html"), text) {
					t.Errorf("report lacks %q", text)
				}
			}
			got, err := os.ReadFile(filepath.Join(h.state, "just.args"))
			if err != nil {
				t.Fatal(err)
			}
			wantArgs := "test-full\n--config=ci\n--define\ngotags=fdbdocker\n--build_event_json_file=" + h.outputs["evidence"] + "/bep.jsonl\n--profile=" + h.outputs["evidence"] + "/bazel-profile.json.gz\n"
			if string(got) != wantArgs {
				t.Fatalf("just args=%q want %q", got, wantArgs)
			}
		})
	}
}

func TestFullLaneReporterCopyControls(t *testing.T) {
	t.Parallel()
	for _, fault := range []string{"failed", "missing", "symlink", "non_executable"} {
		t.Run("copy_"+fault, func(t *testing.T) {
			t.Parallel()
			h := newFullWorkflowHarness(t, "0")
			realCP, err := exec.LookPath("cp")
			if err != nil {
				t.Fatal(err)
			}
			evidenceFile(t, filepath.Join(h.bin, "cp"), []byte(`#!/bin/bash
set -euo pipefail
[ "$#" -eq 3 ] && [ "$1" = -L ]
printf 'applied\n' > "$STATE/reporter-copy-fault"
case "$COPY_FAULT" in
 failed) exit 76 ;;
 missing) exit 0 ;;
 symlink) ln -s "$2" "$3" ;;
 non_executable) "$REAL_CP" "$@"; chmod a-x "$3" ;;
esac
`), 0o700)
			h.env = append(h.env, "COPY_FAULT="+fault, "REAL_CP="+realCP)
			results := h.runDeclaredWorkflow()
			want := 1
			if fault == "failed" {
				want = 76
			}
			prepare := results["prepare"]
			requireStepExit(t, prepare.output, prepare.err, want)
			if data, err := os.ReadFile(filepath.Join(h.state, "reporter-copy-fault")); err != nil || string(data) != "applied\n" {
				t.Fatalf("reporter copy fault did not apply: %q %v", data, err)
			}
			if _, ok := h.outputs["reporter"]; ok {
				t.Fatal("failed reporter retention still exported a reporter output")
			}
			if results["full"].scheduled || results["report"].scheduled || !results["upload"].scheduled {
				t.Fatal("failed preparation must skip the lane/report but retain diagnostic upload scheduling")
			}
			if _, err := os.Stat(filepath.Join(h.state, "just.args")); !os.IsNotExist(err) {
				t.Fatal("failed reporter retention reached the full lane")
			}
			if got := h.bazelCalls(); got != "build --config=ci //cmd/test-report\ninfo --config=ci bazel-bin\n" {
				t.Fatalf("failed reporter retention invoked unexpected Bazel commands: %q", got)
			}
		})
	}
	t.Run("original_bazel_path", func(t *testing.T) {
		t.Parallel()
		h := newFullWorkflowHarness(t, "0")
		out, err := h.step("prepare")
		requireStepExit(t, out, err, 0)
		h.requirePreparedReporter()
		// Reproduce retaining the Bazel-owned path instead of the independent copy.
		h.outputs["reporter"] = h.bazelReporter()
		out, err = h.step("full")
		requireStepExit(t, out, err, 0)
		h.requireReporterInvalidated()
		before := h.bazelCalls()
		out, err = h.step("report")
		requireStepExit(t, out, err, 127)
		if !strings.Contains(out, h.bazelReporter()) || h.bazelCalls() != before {
			t.Fatalf("original-path control did not fail without late Bazel calls: %s", out)
		}
		if _, err := os.Stat(filepath.Join(h.outputs["evidence"], "test-report.html")); !os.IsNotExist(err) {
			t.Fatal("invalidated original reporter path still produced a final report")
		}
	})
}

func TestFullLaneReporterBuildOrderControls(t *testing.T) {
	t.Parallel()
	t.Run("missing_prebuild", func(t *testing.T) {
		t.Parallel()
		h := newFullWorkflowHarness(t, "0")
		if _, err := os.Stat(filepath.Join(h.state, "bin/cmd/test-report/test-report_/test-report")); !os.IsNotExist(err) {
			t.Fatal("reporter output already exists before the fixture build")
		}
		script := fullLaneStep(t, "prepare").Run
		const build = "bazelisk build --config=ci //cmd/test-report\n"
		if strings.Count(script, build) != 1 {
			t.Fatal("missing-prebuild control cannot remove exactly one build")
		}
		script = strings.Replace(script, build, "", 1)
		out, err := h.stepScript("prepare", script)
		requireStepExit(t, out, err, 95)
		if got := h.bazelCalls(); got != "info --config=ci bazel-bin\n" {
			t.Fatalf("missing-prebuild control did not reach info without build: %q", got)
		}
		if _, ok := h.outputs["reporter"]; ok {
			t.Fatal("missing build still exported a reporter output")
		}
		if _, err := os.Stat(filepath.Join(h.state, "reporter.built")); !os.IsNotExist(err) {
			t.Fatal("missing-prebuild control unexpectedly built the reporter")
		}
	})
	for _, late := range []string{"build --config=ci //cmd/test-report", "info --config=ci bazel-bin"} {
		t.Run("late_"+strings.Fields(late)[0], func(t *testing.T) {
			t.Parallel()
			h := newFullWorkflowHarness(t, "0")
			out, err := h.step("prepare")
			requireStepExit(t, out, err, 0)
			out, err = h.step("full")
			requireStepExit(t, out, err, 0)
			before := h.bazelCalls()
			if want := "build --config=ci //cmd/test-report\ninfo --config=ci bazel-bin\nshutdown\n"; before != want {
				t.Fatalf("late-call control did not reach a completed shutdown: %q", before)
			}
			script := "bazelisk " + late + "\n" + fullLaneStep(t, "report").Run
			out, err = h.stepScript("report", script)
			requireStepExit(t, out, err, 94)
			if !strings.Contains(out, "fixture: Bazel invoked after shutdown") || h.bazelCalls() != before+late+"\n" {
				t.Fatalf("late-call control did not apply or was not rejected: calls=%q output=%q", h.bazelCalls(), out)
			}
			if _, err := os.Stat(filepath.Join(h.outputs["evidence"], "test-report.html")); !os.IsNotExist(err) {
				t.Fatal("post-shutdown Bazel call did not stop report generation")
			}
		})
	}
}

func TestFullLaneRejectsInvalidInputsAndResults(t *testing.T) {
	t.Parallel()
	t.Run("wrong_dispatch_SHA", func(t *testing.T) {
		t.Parallel()
		h := newFullWorkflowHarness(t, "0")
		out, err := h.step("prepare", "GITHUB_SHA="+strings.Repeat("0", 40))
		requireStepExit(t, out, err, 1)
		if len(h.outputs) != 0 {
			t.Fatalf("wrong SHA created evidence outputs: %v", h.outputs)
		}
		if _, err := os.Stat(filepath.Join(h.state, "bazel.calls")); !os.IsNotExist(err) {
			t.Fatal("wrong SHA reached reporter preparation")
		}
	})
	for _, mutation := range []string{"dirty", "tracked", "untracked", "head"} {
		t.Run(mutation, func(t *testing.T) {
			t.Parallel()
			h := newFullWorkflowHarness(t, "0")
			out, err := h.step("prepare")
			requireStepExit(t, out, err, 0)
			if mutation == "dirty" {
				evidenceFile(t, filepath.Join(h.repo, "initially-dirty"), []byte("dirty"), 0o600)
			}
			out, err = h.step("full", "MUTATION="+mutation)
			want := 4
			if mutation == "dirty" {
				want = 3
			}
			requireStepExit(t, out, err, want)
			if mutation == "dirty" {
				if !strings.Contains(h.read("verified-lane/status.before"), "initially-dirty") {
					t.Fatal("dirty diagnostic missing")
				}
			} else {
				if !strings.Contains(h.read("verified-lane/metadata"), "inputs_changed=1") {
					t.Fatal("input-change veto missing")
				}
				if mutation == "head" {
					if h.read("verified-lane/head.before") == h.read("verified-lane/head.after") {
						t.Fatal("HEAD mutation never applied")
					}
				} else {
					if !strings.Contains(h.read("verified-lane/input.diff"), mutation+"-input") {
						t.Fatal("input mutation never applied")
					}
				}
			}
		})
	}
	for _, mode := range []string{"missing", "empty", "malformed", "missing-profile", "empty-profile"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			h := newFullWorkflowHarness(t, "0")
			out, err := h.step("prepare")
			requireStepExit(t, out, err, 0)
			out, err = h.step("full", "RESULT_MODE="+mode)
			want := 1
			if mode == "malformed" {
				want = 0
			}
			requireStepExit(t, out, err, want)
			out, err = h.step("report")
			if mode == "missing-profile" || mode == "empty-profile" {
				requireStepExit(t, out, err, 0)
				if !strings.Contains(h.read("collection.log"), "missing or empty full-lane result: bazel-profile.json.gz") {
					t.Fatal("missing profile diagnostic")
				}
			} else {
				requireStepExit(t, out, err, 1)
				if _, err := os.Stat(filepath.Join(h.outputs["evidence"], "test-report.html")); !os.IsNotExist(err) {
					t.Fatal("invalid results produced a final report")
				}
			}
		})
	}
}

func TestFullLaneEvidenceCollectionFailures(t *testing.T) {
	t.Parallel()
	for _, fault := range []string{"missing-directory", "wrong-prefix", "copy", "outer-tee", "inner-tee"} {
		t.Run(fault, func(t *testing.T) {
			t.Parallel()
			h := newFullWorkflowHarness(t, "0")
			out, err := h.step("prepare")
			requireStepExit(t, out, err, 0)
			extra := []string{}
			want := 1
			switch fault {
			case "missing-directory", "wrong-prefix":
				path := t.TempDir()
				if fault == "missing-directory" {
					for _, e := range h.env {
						if strings.HasPrefix(e, "TMPDIR=") {
							path = filepath.Join(strings.TrimPrefix(e, "TMPDIR="), "fdb-full-evidence.missing")
						}
					}
				}
				evidenceFile(t, filepath.Join(h.repo, "tools/lanes/run-verified-lane.sh"), []byte("#!/bin/bash\nprintf '%s\\n' \"$INJECTED_PATH\"\necho applied > \"$STATE/fault-applied\"\n"), 0o600)
				extra = append(extra, "INJECTED_PATH="+path)
			case "copy":
				real, err := exec.LookPath("cp")
				if err != nil {
					t.Fatal(err)
				}
				evidenceFile(t, filepath.Join(h.bin, "cp"), []byte("#!/bin/bash\nif [[ \"$*\" = *verified-lane* ]]; then echo applied > \"$STATE/fault-applied\"; exit 75; fi\nexec \"$REAL_CP\" \"$@\"\n"), 0o700)
				extra = append(extra, "REAL_CP="+real)
			case "outer-tee", "inner-tee":
				real, err := exec.LookPath("tee")
				if err != nil {
					t.Fatal(err)
				}
				match := "/driver.log"
				want = 73
				if fault == "inner-tee" {
					match = "/lane.log"
					want = 74
				}
				evidenceFile(t, filepath.Join(h.bin, "tee"), []byte("#!/bin/bash\n\"$REAL_TEE\" \"$@\"\nrc=$?\nfor arg in \"$@\"; do if [[ \"$arg\" = *\"$TEE_MATCH\" ]]; then echo applied > \"$STATE/fault-applied\"; exit \"$TEE_EXIT\"; fi; done\nexit \"$rc\"\n"), 0o700)
				extra = append(extra, "REAL_TEE="+real, "TEE_MATCH="+match, "TEE_EXIT="+itoa(want))
			}
			out, err = h.step("full", extra...)
			requireStepExit(t, out, err, want)
			if data, err := os.ReadFile(filepath.Join(h.state, "fault-applied")); err != nil || string(data) != "applied\n" {
				t.Fatalf("fault did not apply: %q %v", data, err)
			}
			if h.read("driver.log") == "" {
				t.Fatal("lost driver diagnostics")
			}
			if fault == "inner-tee" {
				if !strings.Contains(h.read("verified-lane/metadata"), "lane_exit=0 tee_exit=74") {
					t.Fatal("lost inner tee verdict")
				}
			} else {
				message := "verified lane did not produce its evidence directory"
				if fault == "copy" {
					message = "could not copy verified-lane evidence"
				}
				if fault == "outer-tee" {
					message = "outer tee failed"
				}
				if !strings.Contains(h.read("collection.log"), message) {
					t.Fatal("collection diagnostic missing")
				}
			}
		})
	}
}

package infra

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type clientCorrectnessWorkflow struct {
	On struct {
		Schedule []struct{ Cron string }
		Dispatch yaml.Node `yaml:"workflow_dispatch"`
	} `yaml:"on"`
	Permissions map[string]string
	Concurrency struct {
		Group  string
		Cancel bool `yaml:"cancel-in-progress"`
	}
	Jobs map[string]struct {
		RunsOn   string `yaml:"runs-on"`
		Timeout  int    `yaml:"timeout-minutes"`
		Strategy yaml.Node
		Env      map[string]string
		Steps    []struct {
			Name, Uses, Run, If string
			Timeout             int `yaml:"timeout-minutes"`
			With                map[string]string
			ContinueOnError     bool `yaml:"continue-on-error"`
		}
	}
}

func readClientCorrectnessWorkflow(t *testing.T) (clientCorrectnessWorkflow, string) {
	t.Helper()
	data, err := os.ReadFile("../.github/workflows/client-correctness.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf clientCorrectnessWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	run := ""
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			if step.Name == "Run scheduled BindingTester on frozen inputs" {
				run = step.Run
			}
		}
	}
	if run == "" {
		t.Fatal("scheduled execution step missing")
	}
	return wf, run
}

func clientCorrectnessExit(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

func TestClientCorrectnessWorkflowIsBoundedAndClientOnly(t *testing.T) {
	t.Parallel()
	wf, run := readClientCorrectnessWorkflow(t)
	if len(wf.On.Schedule) != 1 || wf.On.Schedule[0].Cron != "17 * * * *" || wf.On.Dispatch.Kind == 0 {
		t.Fatal("hourly/manual triggers missing")
	}
	if len(wf.Jobs) != 1 {
		t.Fatalf("unexpected fanout: %d", len(wf.Jobs))
	}
	job, ok := wf.Jobs["bindingtester"]
	if !ok || job.RunsOn != "ubuntu-24.04" || job.Timeout != 40 || job.Strategy.Kind != 0 {
		t.Fatal("must be one bounded hosted job without matrix")
	}
	if wf.Concurrency.Group == "" || wf.Concurrency.Cancel {
		t.Fatal("active correctness runs must not cancel each other")
	}
	if len(wf.Permissions) != 1 || wf.Permissions["contents"] != "read" {
		t.Fatal("unexpected write permissions")
	}
	if job.Env["WORKFLOW_SHA"] != "${{ github.workflow_sha }}" {
		t.Fatal("workflow SHA must come from github.workflow_sha")
	}
	upload, python := 0, 0
	for _, step := range job.Steps {
		if step.ContinueOnError {
			t.Fatal("correctness failures must fail the job")
		}
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
			upload++
			if step.If != "always()" || step.With["if-no-files-found"] != "error" || step.With["retention-days"] != "14" {
				t.Fatal("failure artifacts must be mandatory")
			}
		}
		if strings.HasPrefix(step.Uses, "actions/setup-python@") {
			python++
			if step.With["python-version"] != "3.11" {
				t.Fatal("Python version drift")
			}
		}
		if step.Run != "" {
			cmd := exec.Command("bash", "-n")
			cmd.Stdin = strings.NewReader(step.Run)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s shell syntax: %v %s", step.Name, err, out)
			}
			for _, forbidden := range []string{"just test", "//pkg/recordlayer", "//pkg/relational", "go test"} {
				if strings.Contains(step.Run, forbidden) {
					t.Errorf("client-only step %s contains %q", step.Name, forbidden)
				}
			}
		}
	}
	if upload != 1 || python != 1 {
		t.Fatal("artifact/Python setup steps missing or duplicated")
	}
	for _, required := range []string{"timeout --signal=TERM --kill-after=30s 28m", "bazelisk run --config=ci //cmd/fdb-binding-stress", "-scheduled -duration 20m", "-seed-start", "secrets.randbelow", "trap finish EXIT", "test -s", "jq -e"} {
		if !strings.Contains(run, required) {
			t.Errorf("missing contract %q", required)
		}
	}
}

func TestClientCorrectnessEvidenceInitialization(t *testing.T) {
	t.Parallel()
	wf, _ := readClientCorrectnessWorkflow(t)
	job := wf.Jobs["bindingtester"]
	for key, value := range job.Env {
		if strings.Contains(value, "runner.") {
			t.Fatalf("runner context is unavailable in job-level env %s", key)
		}
	}
	if len(job.Steps) == 0 || job.Steps[0].Name != "Initialize evidence" {
		t.Fatal("evidence must be initialized before setup can fail")
	}
	root := t.TempDir()
	envFile := filepath.Join(root, "github-env")
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", job.Steps[0].Run)
	cmd.Env = append(os.Environ(), "RUNNER_TEMP="+root, "GITHUB_ENV="+envFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("initialize evidence: %v %s", err, out)
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(root, "client-correctness-evidence")
	if string(data) != "EVIDENCE="+evidence+"\n" {
		t.Fatalf("evidence not exported for subsequent steps: %q", data)
	}
	if info, err := os.Stat(evidence); err != nil || !info.IsDir() {
		t.Fatalf("evidence directory missing: %v", err)
	}
}

func TestClientCorrectnessEvidenceAndExitGate(t *testing.T) {
	t.Parallel()
	_, script := readClientCorrectnessWorkflow(t)
	type workflowCase struct {
		name, kind string
		exit       int
	}
	cases := []workflowCase{
		{"complete", "pass", 0},
		{"runner_failure", "fail", 7},
		{"missing_report", "missing", 1},
		{"empty_population", "empty", 1},
		{"incomplete_cycle", "incomplete", 1},
		{"dead_fdb", "dead", 1},
		{"wrong_revision", "wrong-sha", 1},
		{"moving_inputs", "drift", 4},
		{"failure_and_drift", "fail-drift", 7},
		{"unscheduled", "unscheduled", 1},
		{"wrong_server", "version", 1},
		{"counter_mismatch", "pass-count", 1},
		{"failure_counter", "nonzero-fail", 1},
		{"death_counter", "death-count", 1},
		{"report_error", "report-error", 1},
		{"array_mismatch", "array-length", 1},
		{"case_failure", "case-fail", 1},
		{"evidence_inside_checkout", "pre-inside", 2},
		{"ambient_override", "pre-override", 2},
		{"empty_manifest", "pre-empty", 1},
		{"checkout_mismatch", "pre-sha", 2},
		{"dirty_checkout", "pre-dirty", 2},
		{"post_override", "override", 4},
		{"failure_post_override", "fail-override", 7},
		{"head_changed", "head", 4},
		{"failure_head_changed", "fail-head", 7},
		{"population_floor_18", "floor-18", 1},
		{"untracked_input", "untracked", 4},
		{"failure_untracked_input", "fail-untracked", 7},
		{"status_only_drift", "status-only", 4},
		{"failure_status_only_drift", "fail-status-only", 7},
	}
	for _, probe := range []string{"manifest", "revparse", "status"} {
		cases = append(cases, workflowCase{"post_" + probe + "_failure", "probe-" + probe, 4}, workflowCase{"runner_and_" + probe + "_failure", "fail-probe-" + probe, 7})
	}
	for _, location := range []string{"repo", "home", "system"} {
		for _, shape := range []string{"file", "link"} {
			for _, phase := range []struct {
				prefix string
				exit   int
			}{{"pre-", 2}, {"", 4}, {"fail-", 7}} {
				kind := phase.prefix + "override-" + location + "-" + shape
				cases = append(cases, workflowCase{kind, kind, phase.exit})
			}
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			bin := filepath.Join(root, "bin")
			evidence := filepath.Join(root, "evidence")
			home := filepath.Join(root, "home")
			system := filepath.Join(root, "system")
			runScript := strings.ReplaceAll(script, "/etc/bazel.bazelrc", "'"+filepath.Join(system, "bazel.bazelrc")+"'")
			for _, dir := range []string{repo, bin, evidence, home, system} {
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(repo, "source"), []byte("frozen\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = repo
				cmd.Env = append(os.Environ(), "HOME="+home, "GIT_CONFIG_NOSYSTEM=1")
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			git("init", "-q")
			git("config", "core.filemode", "true")
			if err := os.WriteFile(filepath.Join(repo, ".git/info/exclude"), []byte("/user.bazelrc\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", "source")
			git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
			sha := git("rev-parse", "HEAD")
			checkoutSHA := sha
			workflowSHA := strings.Repeat("b", 40)
			overridePath := filepath.Join(home, ".bazelrc")
			if strings.Contains(tc.kind, "-repo-") {
				overridePath = filepath.Join(repo, "user.bazelrc")
			}
			if strings.Contains(tc.kind, "-system-") {
				overridePath = filepath.Join(system, "bazel.bazelrc")
			}
			stub := `#!/bin/bash
set -euo pipefail
[[ "$*" == 'run --config=ci //cmd/fdb-binding-stress -- -scheduled -duration 20m -seed-start '* ]]
: > "$EVIDENCE/runner-started"
if [[ "$RESULT_KIND" = missing ]]; then exit 0; fi
mkdir -p "$EVIDENCE/results"
jq -n --arg sha "$GITHUB_SHA" '{scheduled:true, revision:$sha, fdb_version:"7.3.77", total_seeds:19, pass:19, fail:0, fdb_deaths:0, complete_cycles:1, seeds:[range(0;19)|{pass:true,fdb_alive:true}]}' > "$EVIDENCE/results/report.json"
case "$RESULT_KIND" in
 floor-18) filter='.total_seeds=18 | .pass=18 | .seeds |= .[0:18]' ;;
 empty) filter='.total_seeds=0 | .pass=0 | .seeds=[]' ;;
 incomplete) filter='.complete_cycles=0' ;;
 dead) filter='.seeds[0].fdb_alive=false' ;;
 wrong-sha) filter='.revision="wrong"' ;;
 unscheduled) filter='.scheduled=false' ;;
 version) filter='.fdb_version="7.3.63"' ;;
 pass-count) filter='.pass=18' ;;
 nonzero-fail) filter='.fail=1' ;;
 death-count) filter='.fdb_deaths=1' ;;
 report-error) filter='.error="deliberate error"' ;;
 array-length) filter='.seeds |= .[0:18]' ;;
 case-fail) filter='.seeds[0].pass=false' ;;
 *) filter='.' ;;
esac
jq "$filter" "$EVIDENCE/results/report.json" > "$EVIDENCE/modified.json"
mv "$EVIDENCE/modified.json" "$EVIDENCE/results/report.json"
if [[ "$RESULT_KIND" = *drift ]]; then echo changed >> source; fi
if [[ "$RESULT_KIND" = *override ]]; then touch "$HOME/.bazelrc"; fi
if [[ "$RESULT_KIND" = *head ]]; then
 git -c user.name=Fixture -c user.email=fixture@example.invalid -c commit.gpgsign=false commit --allow-empty -qm moved
fi
if [[ "$RESULT_KIND" = *untracked ]]; then printf 'new input\n' > new-input; fi
if [[ "$RESULT_KIND" = *status-only ]]; then git update-index --chmod=+x source; fi
if [[ "$RESULT_KIND" = override-* || "$RESULT_KIND" = fail-override-* ]]; then
 if [[ "$RESULT_KIND" = *-link ]]; then ln -s "$EVIDENCE/missing-override-target" "$OVERRIDE_PATH"; else touch "$OVERRIDE_PATH"; fi
fi
if [[ "$RESULT_KIND" = fail* ]]; then exit 7; fi
`
			if err := os.WriteFile(filepath.Join(bin, "bazelisk"), []byte(stub), 0o700); err != nil {
				t.Fatal(err)
			}
			pre := strings.HasPrefix(tc.kind, "pre-")
			if strings.HasPrefix(tc.kind, "pre-override-") {
				var err error
				if strings.HasSuffix(tc.kind, "-link") {
					err = os.Symlink(filepath.Join(evidence, "missing-override-target"), overridePath)
				} else {
					err = os.WriteFile(overridePath, nil, 0o644)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			probe := ""
			if i := strings.Index(tc.kind, "probe-"); i >= 0 {
				probe = tc.kind[i+len("probe-"):]
				wrapper := "#!/bin/bash\nset -euo pipefail\nif [[ -f \"$EVIDENCE/runner-started\" ]]; then\ncase \"$PROBE_KIND:$1\" in manifest:ls-files|revparse:rev-parse|status:status)\n\"$REAL_GIT\" \"$@\"\nprintf '%s\\n' \"$1\" >> \"$EVIDENCE/probe-injected.txt\"\nexit 9;;\nesac\nfi\nexec \"$REAL_GIT\" \"$@\"\n"
				if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.kind {
			case "pre-inside":
				evidence = filepath.Join(repo, "evidence")
				if err := os.Mkdir(evidence, 0o755); err != nil {
					t.Fatal(err)
				}
			case "pre-override":
				if err := os.WriteFile(filepath.Join(home, ".bazelrc"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			case "pre-sha":
				sha = "wrong-checkout"
			case "pre-dirty":
				if err := os.WriteFile(filepath.Join(repo, "source"), []byte("dirty"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "pre-empty":
				realGit, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				wrapper := "#!/bin/bash\nif [[ $1 = ls-files ]]; then exit 0; fi\nexec " + realGit + " \"$@\"\n"
				if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-c", runScript)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"), "EVIDENCE="+evidence, "GITHUB_WORKSPACE="+repo, "GITHUB_SHA="+sha, "WORKFLOW_SHA="+workflowSHA, "GITHUB_RUN_ID=864207", "GITHUB_RUN_ATTEMPT=3", "RESULT_KIND="+tc.kind, "OVERRIDE_PATH="+overridePath, "PROBE_KIND="+probe)
			if probe != "" {
				realGit, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				cmd.Env = append(cmd.Env, "REAL_GIT="+realGit)
			}
			out, err := cmd.CombinedOutput()
			if got := clientCorrectnessExit(err); got != tc.exit {
				t.Fatalf("exit=%d want %d: %v\n%s", got, tc.exit, err, out)
			}
			if pre {
				if strings.HasSuffix(tc.kind, "-link") {
					if _, err := os.Lstat(overridePath); err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(overridePath); !os.IsNotExist(err) {
						t.Fatalf("override link is not dangling: %v", err)
					}
				}
				if _, err := os.Stat(filepath.Join(evidence, "runner-started")); !os.IsNotExist(err) {
					t.Fatalf("preflight started runner: %v", err)
				}
				if _, err := os.Stat(filepath.Join(evidence, "runner.log")); !os.IsNotExist(err) {
					t.Fatalf("preflight reached runner: %v", err)
				}
				return
			}
			for _, file := range []string{"inputs.before", "inputs.after", "head.before", "head.after", "status.before", "status.after", "workflow-exit.txt", "workflow.txt", "runner.log", "seed-start.txt", "inputs.diff", "head.diff", "status.diff"} {
				if _, err := os.Stat(filepath.Join(evidence, file)); err != nil {
					t.Errorf("missing evidence %s: %v", file, err)
				}
			}
			read := func(name string) string {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(evidence, name))
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
			wantProvenance := "github_sha=" + sha + "\nworkflow_sha=" + workflowSHA + "\ncheckout_sha=" + checkoutSHA + "\nrun_id=864207\nattempt=3\n"
			if got := read("workflow.txt"); got != wantProvenance {
				t.Fatalf("provenance=%q want %q", got, wantProvenance)
			}
			changed, entryExit := 0, tc.exit
			if tc.exit == 4 || (tc.exit == 7 && tc.kind != "fail") {
				changed = 1
			}
			if tc.exit == 4 {
				entryExit = 0
			}
			if got, want := read("workflow-exit.txt"), fmt.Sprintf("runner_exit=%d\ninputs_changed=%d\n", entryExit, changed); got != want {
				t.Fatalf("exit metadata=%q want %q", got, want)
			}
			if changed == 1 && !strings.Contains(string(out), "Checkout changed during the run; evidence is invalid") {
				t.Fatal("missing changed-input diagnostic")
			}
			frozen := fmt.Sprintf("%x  source\n", sha256.Sum256([]byte("frozen\n")))
			if strings.HasSuffix(tc.kind, "untracked") {
				want := fmt.Sprintf("%x  new-input\n", sha256.Sum256([]byte("new input\n"))) + frozen
				if read("inputs.before") != frozen || read("inputs.after") != want || !strings.Contains(read("status.after"), "?? new-input") {
					t.Fatal("untracked file missing from manifest/status")
				}
			}
			if probe != "" || strings.Contains(tc.kind, "override-") || strings.HasSuffix(tc.kind, "status-only") {
				for _, item := range []string{"inputs", "head", "status"} {
					if strings.HasSuffix(tc.kind, "status-only") && item == "status" {
						continue
					}
					if read(item+".before") != read(item+".after") || read(item+".diff") != "" {
						t.Fatalf("%s changed in isolated fault fixture", item)
					}
				}
			}
			if strings.HasSuffix(tc.kind, "status-only") && (read("status.before") != "" || !strings.Contains(read("status.after"), "source") || read("status.diff") == "") {
				t.Fatal("status-only fixture did not change index")
			}
			if probe != "" {
				want := map[string]string{"manifest": "ls-files\n", "revparse": "rev-parse\n", "status": "status\n"}[probe]
				if read("probe-injected.txt") != want {
					t.Fatal("post probe not injected exactly once")
				}
			}
			if strings.HasSuffix(tc.kind, "-link") {
				if _, err := os.Lstat(overridePath); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(overridePath); !os.IsNotExist(err) {
					t.Fatalf("override link is not dangling: %v", err)
				}
			}
			if tc.kind == "floor-18" || tc.kind == "pass" || tc.kind == "incomplete" {
				var report struct {
					Total  int `json:"total_seeds"`
					Pass   int `json:"pass"`
					Cycles int `json:"complete_cycles"`
					Seeds  []struct {
						Pass  bool `json:"pass"`
						Alive bool `json:"fdb_alive"`
					} `json:"seeds"`
				}
				if err := json.Unmarshal([]byte(read("results/report.json")), &report); err != nil {
					t.Fatal(err)
				}
				wantTotal, wantCycles := 19, 1
				if tc.kind == "floor-18" {
					wantTotal = 18
				}
				if tc.kind == "incomplete" {
					wantCycles = 0
				}
				if report.Total != wantTotal || report.Pass != wantTotal || len(report.Seeds) != wantTotal || report.Cycles != wantCycles {
					t.Fatalf("invalid boundary fixture: %+v", report)
				}
				for _, seed := range report.Seeds {
					if !seed.Pass || !seed.Alive {
						t.Fatal("boundary fixture contains failed/dead case")
					}
				}
			}
		})
	}
}

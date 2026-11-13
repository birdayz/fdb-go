package infra

import (
	"context"
	"errors"
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
	for _, tc := range []struct {
		name, kind string
		exit       int
	}{
		{"complete", "pass", 0},
		{"runner_failure", "fail", 7},
		{"missing_report", "missing", 1},
		{"empty_population", "empty", 1},
		{"incomplete_cycle", "incomplete", 1},
		{"dead_fdb", "dead", 1},
		{"wrong_revision", "wrong-sha", 1},
		{"moving_inputs", "drift", 4},
		{"failure_and_drift", "fail-drift", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			bin := filepath.Join(root, "bin")
			evidence := filepath.Join(root, "evidence")
			home := filepath.Join(root, "home")
			for _, dir := range []string{repo, bin, evidence, home} {
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
			git("add", "source")
			git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
			sha := git("rev-parse", "HEAD")
			stub := `#!/bin/bash
set -euo pipefail
[[ "$*" == 'run --config=ci //cmd/fdb-binding-stress -- -scheduled -duration 20m -seed-start '* ]]
if [[ "$RESULT_KIND" = missing ]]; then exit 0; fi
mkdir -p "$EVIDENCE/results"
jq -n --arg sha "$GITHUB_SHA" '{scheduled:true, revision:$sha, fdb_version:"7.3.77", total_seeds:19, pass:19, fail:0, fdb_deaths:0, complete_cycles:1, seeds:[range(0;19)|{pass:true,fdb_alive:true}]}' > "$EVIDENCE/results/report.json"
case "$RESULT_KIND" in
 empty) filter='.total_seeds=0 | .pass=0 | .seeds=[]' ;;
 incomplete) filter='.complete_cycles=0' ;;
 dead) filter='.seeds[0].fdb_alive=false' ;;
 wrong-sha) filter='.revision="wrong"' ;;
 *) filter='.' ;;
esac
jq "$filter" "$EVIDENCE/results/report.json" > "$EVIDENCE/modified.json"
mv "$EVIDENCE/modified.json" "$EVIDENCE/results/report.json"
if [[ "$RESULT_KIND" = *drift ]]; then echo changed >> source; fi
if [[ "$RESULT_KIND" = fail* ]]; then exit 7; fi
`
			if err := os.WriteFile(filepath.Join(bin, "bazelisk"), []byte(stub), 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-c", script)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"), "EVIDENCE="+evidence, "GITHUB_WORKSPACE="+repo, "GITHUB_SHA="+sha, "WORKFLOW_SHA="+sha, "GITHUB_RUN_ID=1", "GITHUB_RUN_ATTEMPT=1", "RESULT_KIND="+tc.kind)
			out, err := cmd.CombinedOutput()
			if got := clientCorrectnessExit(err); got != tc.exit {
				t.Fatalf("exit=%d want %d: %v\n%s", got, tc.exit, err, out)
			}
			for _, file := range []string{"inputs.before", "inputs.after", "head.before", "head.after", "status.before", "status.after", "workflow-exit.txt", "runner.log", "seed-start.txt"} {
				if _, err := os.Stat(filepath.Join(evidence, file)); err != nil {
					t.Errorf("missing evidence %s: %v", file, err)
				}
			}
		})
	}
}

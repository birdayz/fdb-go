package infra

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func evidenceDraftBoundProcess(cmd *exec.Cmd) {
	// CommandContext cancels through its Go-owned process handle, not a raw PID.
	cmd.WaitDelay = 2 * time.Second
}

func evidenceDraftAwait(t *testing.T, done <-chan error, cancel context.CancelFunc) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		t.Fatal("workflow wait exceeded owned-process watchdog")
		return nil
	}
}

func evidenceDraftShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func evidenceDraftWrite(t *testing.T, name, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(name, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func evidenceDraftRead(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestClientCorrectnessInitializationFailureStopsExport(t *testing.T) {
	t.Parallel()
	wf, _ := readClientCorrectnessWorkflow(t)
	steps := wf.Jobs["bindingtester"].Steps
	if len(steps) == 0 || steps[0].Name != "Initialize evidence" {
		t.Fatal("initialization must be first")
	}
	for _, kind := range []string{"success", "mkdir", "export"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			evidence := filepath.Join(root, "client-correctness-evidence")
			envFile := filepath.Join(root, "github-env")
			if kind == "mkdir" {
				evidenceDraftWrite(t, evidence, "occupied", 0o600)
			}
			if kind == "export" {
				if err := os.Mkdir(envFile, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-euo", "pipefail", "-c", steps[0].Run)
			evidenceDraftBoundProcess(cmd)
			cmd.Env = append(os.Environ(), "RUNNER_TEMP="+root, "GITHUB_ENV="+envFile)
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("initialization watchdog expired: %v", ctx.Err())
			}
			if kind == "success" {
				if err != nil {
					t.Fatalf("%v: %s", err, output)
				}
				if got := evidenceDraftRead(t, envFile); got != "EVIDENCE="+evidence+"\n" {
					t.Fatalf("export=%q", got)
				}
				if info, err := os.Stat(evidence); err != nil || !info.IsDir() {
					t.Fatalf("evidence directory missing: %v", err)
				}
			} else {
				if clientCorrectnessExit(err) <= 0 {
					t.Fatalf("failure was tolerated: %v %s", err, output)
				}
				if kind == "mkdir" {
					if _, err := os.Stat(envFile); !os.IsNotExist(err) {
						t.Fatalf("export attempted after mkdir failure: %v", err)
					}
				}
			}
		})
	}
}

type evidenceDraftFixture struct {
	root, repo, bin, evidence, home, system, names, seed, argv, secretsCalls string
	script                                                                   string
}

func newEvidenceDraftFixture(t *testing.T, rawSeed int64, names []string) evidenceDraftFixture {
	t.Helper()
	_, script := readClientCorrectnessWorkflow(t)
	f := evidenceDraftFixture{root: t.TempDir()}
	f.repo = filepath.Join(f.root, "repo")
	f.bin = filepath.Join(f.root, "bin")
	f.evidence = filepath.Join(f.root, "evidence")
	f.home = filepath.Join(f.root, "home")
	f.system = filepath.Join(f.root, "system ' space")
	for _, d := range []string{f.repo, f.bin, f.evidence, f.home, f.system} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.names = filepath.Join(f.root, "names.nul")
	f.argv = filepath.Join(f.root, "argv.nul")
	f.secretsCalls = filepath.Join(f.root, "secrets.calls")
	f.seed = strconv.FormatInt(rawSeed, 10)
	evidenceDraftWrite(t, f.names, strings.Join(names, "\x00")+"\x00", 0o600)
	for _, name := range names {
		evidenceDraftWrite(t, filepath.Join(f.repo, name), "content:"+name+"\n", 0o600)
	}
	f.script = strings.ReplaceAll(script, "/etc/bazel.bazelrc", evidenceDraftShellQuote(filepath.Join(f.system, "bazel.bazelrc")))
	evidenceDraftWrite(t, filepath.Join(f.bin, "sort"), `#!/bin/bash
set -euo pipefail
[[ "${LC_ALL-}" = C ]] || exit 97
exec "$REAL_SORT" "$@"
`, 0o700)
	evidenceDraftWrite(t, filepath.Join(f.bin, "git"), `#!/bin/bash
set -euo pipefail
case "$1" in
 ls-files)
  [[ "$#" = 5 && "$2" = --cached && "$3" = --others && "$4" = --exclude-standard && "$5" = -z ]] || exit 91
  cat "$NAMES_NUL" ;;
 rev-parse) [[ "$#" = 2 && "$2" = HEAD ]] || exit 92; printf '%s\n' "$GITHUB_SHA" ;;
 status) [[ "$#" = 3 && "$2" = --porcelain=v1 && "$3" = --untracked-files=all ]] || exit 93 ;;
 *) exit 94 ;;
esac
`, 0o700)
	evidenceDraftWrite(t, filepath.Join(f.bin, "timeout"), `#!/bin/bash
set -euo pipefail
[[ "$1" = --signal=TERM && "$2" = --kill-after=30s && "$3" = 28m ]] || exit 95
shift 3
exec "$@"
`, 0o700)
	evidenceDraftWrite(t, filepath.Join(f.bin, "bazelisk"), `#!/bin/bash
set -euo pipefail
printf '%s\0' "$@" > "$ARGV_NUL"
mkdir -p "$EVIDENCE/results"
printf '{}\n' > "$EVIDENCE/results/report.json"
if [[ "$SIGNAL_CASE" = 1 ]]; then
 IFS= read -r admitted <&4
 [[ "$admitted" = enter ]] || exit 96
 printf R >&3
 IFS= read -r released <&4
 [[ "$released" = release ]] || exit 96
 printf 'runner gate completed\n'
fi
`, 0o700)
	evidenceDraftWrite(t, filepath.Join(f.bin, "jq"), "#!/bin/bash\nset -euo pipefail\nprintf 'true\\n'\n", 0o700)
	evidenceDraftWrite(t, filepath.Join(f.repo, "secrets.py"), `import os

def randbelow(bound):
    assert bound == 2147483647, bound
    with open(os.environ["SECRETS_CALLS"], "a", encoding="utf-8") as output:
        output.write(str(bound) + "\n")
    return int(os.environ["RAW_SEED"])
`, 0o600)
	return f
}

func (f evidenceDraftFixture) command(t *testing.T) (*exec.Cmd, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	cmd := exec.CommandContext(ctx, "bash", "-euo", "pipefail", "-c", f.script)
	evidenceDraftBoundProcess(cmd)
	realSort, err := exec.LookPath("sort")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cmd.Dir = f.repo
	cmd.Env = append(os.Environ(), "PATH="+f.bin+":"+os.Getenv("PATH"), "HOME="+f.home, "EVIDENCE="+f.evidence, "GITHUB_WORKSPACE="+f.repo, "GITHUB_SHA="+strings.Repeat("a", 40), "WORKFLOW_SHA="+strings.Repeat("b", 40), "GITHUB_RUN_ID=864207", "GITHUB_RUN_ATTEMPT=3", "NAMES_NUL="+f.names, "ARGV_NUL="+f.argv, "SECRETS_CALLS="+f.secretsCalls, "RAW_SEED="+f.seed, "PYTHONPATH="+f.repo, "PYTHONDONTWRITEBYTECODE=1", "SIGNAL_CASE=0", "LC_ALL=", "REAL_SORT="+realSort)
	return cmd, cancel
}

func evidenceDraftGNUHashLine(name string) string {
	sum := sha256.Sum256([]byte("content:" + name + "\n"))
	prefix := ""
	if strings.ContainsAny(name, "\\\n") {
		prefix = "\\"
		name = strings.ReplaceAll(strings.ReplaceAll(name, "\\", "\\\\"), "\n", "\\n")
	}
	return fmt.Sprintf("%s%x  %s\n", prefix, sum, name)
}

func TestClientCorrectnessNULManifestAndSeedArgv(t *testing.T) {
	t.Parallel()
	// Deliberately duplicated, unsorted tracked/untracked records, including literal newline paths.
	names := []string{"z tracked", "line\ntracked", "a untracked", "space untracked", "z tracked", "line\ntracked"}
	for _, raw := range []int64{0, 2147483646} {
		t.Run(strconv.FormatInt(raw, 10), func(t *testing.T) {
			t.Parallel()
			f := newEvidenceDraftFixture(t, raw, names)
			cmd, cancel := f.command(t)
			defer cancel()
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("workflow: %v\n%s", err, output)
			}
			unique := []string{"z tracked", "line\ntracked", "a untracked", "space untracked"}
			sort.Strings(unique)
			var want strings.Builder
			for _, name := range unique {
				want.WriteString(evidenceDraftGNUHashLine(name))
			}
			for _, phase := range []string{"before", "after"} {
				got := evidenceDraftRead(t, filepath.Join(f.evidence, "inputs."+phase))
				if got != want.String() {
					t.Fatalf("%s manifest=%q want %q", phase, got, want.String())
				}
			}
			seed := strconv.FormatInt(raw+1, 10)
			if got := evidenceDraftRead(t, filepath.Join(f.evidence, "seed-start.txt")); got != seed+"\n" {
				t.Fatalf("saved seed=%q want %q", got, seed)
			}
			if got := evidenceDraftRead(t, f.secretsCalls); got != "2147483647\n" {
				t.Fatalf("secrets calls=%q", got)
			}
			args := strings.Split(strings.TrimSuffix(evidenceDraftRead(t, f.argv), "\x00"), "\x00")
			wantArgs := []string{"run", "--config=ci", "//cmd/fdb-binding-stress", "--", "-scheduled", "-duration", "20m", "-seed-start", seed, "-out", filepath.Join(f.evidence, "results")}
			if !reflect.DeepEqual(args, wantArgs) {
				t.Fatalf("argv=%q want %q", args, wantArgs)
			}
		})
	}
}

func TestClientCorrectnessSignalsFinalizeEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		signal syscall.Signal
		exit   int
	}{{"INT", syscall.SIGINT, 130}, {"TERM", syscall.SIGTERM, 143}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newEvidenceDraftFixture(t, 17, []string{"source"})
			cmd, cancel := f.command(t)
			defer cancel()
			cmd.Env = append(cmd.Env, "SIGNAL_CASE=1")
			readyR, readyW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer readyR.Close()
			defer readyW.Close()
			releaseR, releaseW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer releaseR.Close()
			defer releaseW.Close()
			cmd.ExtraFiles = []*os.File{readyW, releaseR}
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			readyW.Close()
			releaseR.Close()
			if _, err := releaseW.WriteString("enter\n"); err != nil {
				cancel()
				cmd.Wait()
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			ready := make(chan error, 1)
			go func() {
				var b [1]byte
				n, err := readyR.Read(b[:])
				if err == nil && (n != 1 || b[0] != 'R') {
					err = fmt.Errorf("bad readiness %q", b[:n])
				}
				ready <- err
			}()
			select {
			case err := <-ready:
				if err != nil {
					releaseW.Close()
					cancel()
					waitErr := evidenceDraftAwait(t, done, cancel)
					t.Fatalf("readiness: %v; wait: %v", err, waitErr)
				}
			case <-time.After(20 * time.Second):
				releaseW.Close()
				cancel()
				waitErr := evidenceDraftAwait(t, done, cancel)
				t.Fatalf("runner readiness watchdog expired; wait: %v", waitErr)
			}
			// Bash defers a trapped signal while its foreground pipeline runs.
			// Release that child, not a read in the signaled shell, to dispatch the trap.
			signalErr := cmd.Process.Signal(tc.signal)
			_, releaseErr := releaseW.WriteString("release\n")
			releaseW.Close()
			err = evidenceDraftAwait(t, done, cancel)
			if signalErr != nil || releaseErr != nil {
				t.Fatalf("signal=%v release=%v", signalErr, releaseErr)
			}
			if got := clientCorrectnessExit(err); got != tc.exit {
				t.Fatalf("exit=%d want %d: %v %s", got, tc.exit, err, output.String())
			}
			if got := evidenceDraftRead(t, filepath.Join(f.evidence, "runner.log")); got != "runner gate completed\n" {
				t.Fatalf("foreground pipeline did not complete before finalization: %q", got)
			}
			if got, want := evidenceDraftRead(t, filepath.Join(f.evidence, "workflow-exit.txt")), fmt.Sprintf("runner_exit=%d\ninputs_changed=0\n", tc.exit); got != want {
				t.Fatalf("exit metadata=%q want %q", got, want)
			}
			for _, item := range []string{"inputs", "head", "status"} {
				before := evidenceDraftRead(t, filepath.Join(f.evidence, item+".before"))
				after := evidenceDraftRead(t, filepath.Join(f.evidence, item+".after"))
				if before != after {
					t.Fatalf("%s snapshots differ: %q %q", item, before, after)
				}
				if got := evidenceDraftRead(t, filepath.Join(f.evidence, item+".diff")); got != "" {
					t.Fatalf("%s diff=%q", item, got)
				}
			}
		})
	}
}

func TestClientCorrectnessArtifactRoutingAndFailureSummary(t *testing.T) {
	t.Parallel()
	wf, _ := readClientCorrectnessWorkflow(t)
	uploads, summaries := 0, 0
	for _, step := range wf.Jobs["bindingtester"].Steps {
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
			uploads++
			if step.With["name"] != "client-correctness-${{ github.run_id }}-${{ github.run_attempt }}" || step.With["path"] != "${{ env.EVIDENCE }}" {
				t.Fatalf("artifact routing=%v", step.With)
			}
		}
		if step.Name == "Failure summary" {
			summaries++
			if step.If != "failure()" || step.Run == "" {
				t.Fatalf("failure condition/body=%q %q", step.If, step.Run)
			}
			summary := filepath.Join(t.TempDir(), "summary")
			const sentinel = "existing summary\n"
			evidenceDraftWrite(t, summary, sentinel, 0o600)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-euo", "pipefail", "-c", step.Run)
			evidenceDraftBoundProcess(cmd)
			cmd.Env = append(os.Environ(), "GITHUB_STEP_SUMMARY="+summary)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("summary: %v %s", err, out)
			}
			got := evidenceDraftRead(t, summary)
			if !strings.HasPrefix(got, sentinel) {
				t.Fatalf("summary did not append: %q", got)
			}
			added := strings.TrimPrefix(got, sentinel)
			for _, want := range []string{"Go client BindingTester failed", "client-correctness artifact", "seed, report, tester output, FDB traces, setup logs, and input hashes", "Setup, build, timeout, incomplete cycle, missing evidence, and moving inputs are failures", "docs/client-correctness.md", "do not rerun to dismiss a failure"} {
				if !strings.Contains(added, want) {
					t.Errorf("missing summary text %q in %q", want, added)
				}
			}
		}
	}
	if uploads != 1 || summaries != 1 {
		t.Fatalf("uploads=%d summaries=%d", uploads, summaries)
	}
}

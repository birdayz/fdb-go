//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	foundationdb "fdb.dev/pkg/testcontainers/foundationdb"
)

func TestTesterTimeoutKillsChildrenHoldingOutputPipes(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.CommandContext(ctx, "python3", "-c", `
import os, subprocess, sys, time
child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])
with open(sys.argv[1] + '.tmp', 'w') as ready:
    ready.write(str(child.pid))
os.replace(sys.argv[1] + '.tmp', sys.argv[1])
time.sleep(60)
`, ready)
	process := configureTesterProcess(cmd)
	t.Cleanup(cancel)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("tester cleanup did not reap leader")
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Python subprocess did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	childPID, err := os.ReadFile(ready)
	if err != nil || len(childPID) == 0 {
		t.Fatalf("child PID not recorded: %q %v", childPID, err)
	}
	cancel()
	select {
	case err := <-done:
		done <- err
		if err == nil {
			t.Fatal("cancelled harness passed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("harness children retained inherited output pipes past cancellation")
	}
	deadline = time.Now().Add(time.Second)
	for {
		stat, err := os.ReadFile("/proc/" + string(childPID) + "/stat")
		// A killed orphan can remain a zombie until the host's init reaps it.
		if os.IsNotExist(err) || (err == nil && strings.Contains(string(stat), ") Z ")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %s survived cancellation: %s (%v)", childPID, stat, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRealRunSeedKillsResidualChildOnSuccess(t *testing.T) {
	t.Parallel()
	harness, seedDir := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(harness, "bindingtester"), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := `import os, subprocess, sys
child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(300)'],
                         stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
with open(os.path.join(os.path.dirname(__file__), '..', 'child.pid'), 'w') as output:
    output.write(str(child.pid))
print('Test had 0 incorrect result(s) and 0 error(s)', flush=True)
`
	if err := os.WriteFile(filepath.Join(harness, "bindingtester", "bindingtester.py"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d := defaultRunnerDeps(realRunSeed)
	startFDB := d.startFDB
	d.startFDB = func(ctx context.Context, image string) (*foundationdb.Container, error) {
		container, err := startFDB(ctx, image)
		requireRunnerDocker(t, err)
		return container, err
	}
	r := realRunSeedWithDeps(ctx, 146, workload{"api", 20, 1, false, 730}, harness, seedDir, d)
	if !r.Pass || !r.FDBAlive || r.Error != "" || ctx.Err() != nil {
		t.Fatalf("successful tester=%+v context=%v", r, ctx.Err())
	}
	data, err := os.ReadFile(filepath.Join(harness, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid child PID %q: %v", data, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if os.IsNotExist(err) || (err == nil && strings.Contains(string(stat), ") Z ")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("successful tester left child alive: %s %v", stat, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ctx.Err() != nil {
		t.Fatalf("context ended rather than normal-exit group cleanup: %v", ctx.Err())
	}
}

func TestRunPlanReplaysCorpusBeforeFreshCompleteCycles(t *testing.T) {
	t.Parallel()
	var got []seedCase
	cycles, replayed, err := runPlan(0, 0xffffffff, true, true, correctnessModes(1000)[0],
		func() bool { return len(got) < 24 },
		func(seed int, w workload) error { got = append(got, seedCase{seed, w}); return nil })
	if err != nil || !replayed || cycles != 2 {
		t.Fatalf("cycles=%d replayed=%v err=%v", cycles, replayed, err)
	}
	want := regressionCases()
	for _, seed := range []int{0xffffffff, 1} {
		for _, w := range correctnessModes(1000) {
			want = append(want, seedCase{seed, w})
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan=%+v want %+v", got, want)
	}
}

func TestRunPlanBudgetAndExecutionFailures(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, 3, 14, 16, 19} {
		count := 0
		cycles, replayed, err := runPlan(0, 1, true, true, correctnessModes(1000)[0], func() bool { return count < limit }, func(int, workload) error { count++; return nil })
		if err != nil || count != limit || replayed != (limit >= 14) || cycles != limit/19 {
			t.Errorf("limit=%d count=%d cycles=%d replayed=%v err=%v", limit, count, cycles, replayed, err)
		}
	}
	for _, scheduled := range []bool{false, true} {
		boom := errors.New("cannot save case")
		_, _, err := runPlan(1, 1, false, scheduled, correctnessModes(1000)[0], func() bool { return true }, func(int, workload) error { return boom })
		if !errors.Is(err, boom) {
			t.Errorf("lost run error: %v", err)
		}
	}
	count := 0
	cycles, replayed, err := runPlan(2, 10, false, false, correctnessModes(1000)[1], func() bool { return true }, func(seed int, w workload) error {
		if seed != 10+count || !w.Compare {
			t.Errorf("wrong single-mode case %d %+v", seed, w)
		}
		count++
		return nil
	})
	if err != nil || cycles != 2 || !replayed || count != 2 {
		t.Fatalf("counted plan: cycles=%d replayed=%v count=%d err=%v", cycles, replayed, count, err)
	}
}

func TestCorrectnessModesAndRegressionPopulation(t *testing.T) {
	t.Parallel()
	want := []workload{{"scripted", 1000, 1, false, 730}, {"api", 1000, 1, true, 730}, {"api", 1000, 5, false, 730}, {"directory", 1000, 1, true, 730}, {"directory_hca", 100, 5, false, 730}}
	if got := correctnessModes(1000); !reflect.DeepEqual(got, want) {
		t.Fatalf("modes = %+v", got)
	}
	cases := regressionCases()
	if len(cases) != 14 {
		t.Fatalf("regression population = %d, want 14", len(cases))
	}
	wantSeeds := []int{3534790651, 3864917676, 3153055325, 2095856910, 2483220251, 241550211, 1508488514, 4203526853, 4142326254, 4275610547, 1951034301, 1700644945, 584458794, 3179331380}
	for i, c := range cases {
		if c.Seed != wantSeeds[i] {
			t.Errorf("case %d seed=%d", i, c.Seed)
		}
		mode := want[3]
		switch {
		case i < 2:
			mode = want[1]
		case i == 2:
			mode = want[2]
		case i == 13:
			mode = want[4]
			mode.APIVersion = 610
		}
		if c.Workload != mode {
			t.Errorf("case %d workload=%+v want %+v", i, c.Workload, mode)
		}
	}
}

func TestTesterArgsPreserveOracleConcurrencyAndFullScriptedSurface(t *testing.T) {
	t.Parallel()
	for _, w := range append(correctnessModes(1000), workload{"directory_hca", 37, 5, false, 610}) {
		got := testerArgs(3864917676, w, "cluster", "harness")
		if got[len(got)-1] != "pure_go" {
			t.Fatal("must use registered ALL_TYPES/2040-bit Go tester")
		}
		joined := " " + strings.Join(got, " ") + " "
		for _, part := range []string{"--seed 3864917676", "--api-version " + strconv.Itoa(w.APIVersion), "--num-ops " + strconv.Itoa(w.Ops), "--test-name " + w.Test, "--no-tenants"} {
			if !strings.Contains(joined, " "+part+" ") {
				t.Errorf("missing %q in %v", part, got)
			}
		}
		if strings.Contains(joined, "--no-threads") {
			t.Fatal("scripted thread coverage disabled")
		}
		if strings.Contains(joined, "--compare python") != w.Compare {
			t.Errorf("oracle mismatch: %v", got)
		}
		concurrent := strings.Contains(joined, "--concurrency 5")
		if concurrent != (w.Concurrency == 5) {
			t.Errorf("concurrency mismatch: %v", got)
		}
		directory := w.Test == "directory" || w.Test == "directory_hca"
		if strings.Contains(joined, "--no-directory-snapshot-ops") != directory {
			t.Errorf("directory snapshot setting: %v", got)
		}
	}
}

func TestTesterArgsLiteralVectors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		w    workload
		want []string
	}{
		{workload{"api", 731, 1, true, 730}, []string{"harness/bindingtester/bindingtester.py", "--cluster-file", "cluster", "--test-name", "api", "--api-version", "730", "--num-ops", "731", "--seed", "42", "--timeout", "300", "--concurrency", "1", "--no-tenants", "--compare", "python", "pure_go"}},
		{workload{"directory_hca", 37, 5, false, 610}, []string{"harness/bindingtester/bindingtester.py", "--cluster-file", "cluster", "--test-name", "directory_hca", "--api-version", "610", "--num-ops", "37", "--seed", "42", "--timeout", "300", "--concurrency", "5", "--no-tenants", "--no-directory-snapshot-ops", "pure_go"}},
	} {
		if got := testerArgs(42, tc.w, "cluster", "harness"); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("args=%q want=%q", got, tc.want)
		}
	}
}

func TestDatabaseAvailabilityFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		output string
		err    error
		want   bool
	}{
		{`{"client":{"database_status":{"available":true}}}`, nil, true},
		{`{"client":{"database_status":{"available":false}}}`, nil, false},
		{`{"client":{"database_status":{"available":true}}}`, errors.New("exec failed"), false},
		{`{}`, nil, false},
		{`{"client":{"database_status":{}}}`, nil, false},
		{`{"client":{"database_status":{"available":"true"}}}`, nil, false},
		{`not json`, nil, false},
		{"", nil, false},
	} {
		if got := databaseAvailable(tc.output, tc.err); got != tc.want {
			t.Errorf("status=%q err=%v got=%v", tc.output, tc.err, got)
		}
	}
}

func TestSuccessfulVerdictFailsClosed(t *testing.T) {
	t.Parallel()
	good := "Test with seed 1 had 0 incorrect result(s) and 0 error(s) at API version 730"
	bad := "Test with seed 1 had 1 incorrect result(s) and 0 error(s)"
	for _, tt := range []struct {
		output string
		err    error
		want   bool
	}{
		{good, nil, true},
		{"", nil, false},
		{"Completed test", nil, false},
		{good, errors.New("timeout"), false},
		{bad, nil, false},
		{bad + "\n" + good, nil, false},
		{good + "\n" + bad, nil, false},
		{"had 0 incorrect result(s) and 1 error(s)", nil, false},
	} {
		if got := successfulVerdict(tt.output, tt.err); got != tt.want {
			t.Errorf("verdict(%q,%v)=%v", tt.output, tt.err, got)
		}
	}
}

func TestRunVerdictRequiresNonemptySuccessfulCompletePopulation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                      string
		report                    Report
		scheduled, replayed, pass bool
	}{
		{"empty", Report{}, false, true, false},
		{"failure", Report{Total: 1, Fail: 1}, false, true, false},
		{"dead", Report{Total: 1, FDBDeaths: 1}, false, true, false},
		{"replay-incomplete", Report{Total: 1, CompleteCycles: 1}, true, false, false},
		{"no-cycle", Report{Total: 14}, true, true, false},
		{"scheduled", Report{Total: 19, CompleteCycles: 1}, true, true, true},
		{"single", Report{Total: 1}, false, true, true},
	} {
		if err := runVerdict(tt.report, tt.scheduled, tt.replayed); (err == nil) != tt.pass {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
}

func TestCorrectnessRejectsServerVersionOverrides(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"", "7.3.77", "7.3.63", "latest"} {
		if err := validateFDBVersion(version); (err == nil) != (version == "" || version == "7.3.77") {
			t.Errorf("version %q: %v", version, err)
		}
	}
}

func TestValidateRunRejectsVacuousAndUnsupportedConfigurations(t *testing.T) {
	t.Parallel()
	valid := workload{"api", 1000, 1, true, 730}
	if err := validateRun(1, 1, 0, false, valid); err != nil {
		t.Fatal(err)
	}
	if err := validateRun(0, 1, 20*time.Minute, true, valid); err != nil {
		t.Fatal(err)
	}
	valid.APIVersion = 510
	if err := validateRun(1, 1, 0, false, valid); err != nil {
		t.Fatalf("minimum supported API rejected: %v", err)
	}
	for _, tt := range []struct {
		seeds, start int
		duration     time.Duration
		scheduled    bool
		w            workload
	}{
		{0, 1, 0, false, valid},
		{1, 0, 0, false, valid},
		{2, 0xffffffff, 0, false, valid},
		{1, 1, -time.Second, false, valid},
		{1, 1, 0, true, valid},
		{1, 1, 0, false, workload{"unknown", 1000, 1, false, 730}},
		{1, 1, 0, false, workload{"api", 0, 1, false, 730}},
		{1, 1, 0, false, workload{"api", 1000, 0, false, 730}},
		{1, 1, 0, false, workload{"api", 1000, 5, true, 730}},
		{1, 1, 0, false, workload{"api", 1000, 1, false, 731}},
		{1, 1, 0, false, workload{"api", 1000, 1, false, 22}},
		{1, 1, 0, false, workload{"api", 1000, 1, false, 509}},
	} {
		if err := validateRun(tt.seeds, tt.start, tt.duration, tt.scheduled, tt.w); err == nil {
			t.Errorf("accepted %+v", tt)
		}
	}
}

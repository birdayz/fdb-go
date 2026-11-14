//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	foundationdb "fdb.dev/pkg/testcontainers/foundationdb"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

type cleanupSuccessContainer struct {
	testcontainers.Container
	execs, logs, starts, terminations int
	logsErr                           error
}

func (*cleanupSuccessContainer) GetContainerID() string { return "cleanup-fixture" }
func (c *cleanupSuccessContainer) Logs(context.Context) (io.ReadCloser, error) {
	c.logs++
	return nil, c.logsErr
}

func (c *cleanupSuccessContainer) Exec(_ context.Context, command []string, _ ...tcexec.ProcessOption) (int, io.Reader, error) {
	c.execs++
	if len(command) < 3 || strings.Join(command[len(command)-3:], " ") != "/usr/bin/fdbcli --exec status json" {
		return 1, nil, errors.New("unexpected liveness command")
	}
	return 0, strings.NewReader(`{"client":{"database_status":{"available":true}}}`), nil
}

func successfulCleanupDeps(t *testing.T, harness string, terminationErr, logsErr error) (runnerDeps, *cleanupSuccessContainer) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(harness, "bindingtester"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(harness, "bindingtester", "bindingtester.py"), []byte("print('Test had 0 incorrect result(s) and 0 error(s)')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &cleanupSuccessContainer{logsErr: logsErr}
	container := &foundationdb.Container{Container: c}
	d := defaultRunnerDeps(realRunSeed)
	d.startFDB = func(context.Context, string) (*foundationdb.Container, error) {
		c.starts++
		return container, nil
	}
	d.collectLogs = func(ctx context.Context, container *foundationdb.Container, dir string) {
		if logsErr != nil {
			collectLogs(ctx, container, dir)
		}
	}
	d.terminateFDB = func(context.Context, *foundationdb.Container) error {
		c.terminations++
		return terminationErr
	}
	return d, c
}

func TestSuccessfulCaseCleanupVerdictAndCampaign(t *testing.T) {
	guardGit(t, false)
	docker := t.TempDir()
	if err := os.WriteFile(filepath.Join(docker, "docker"), []byte("#!/bin/sh\nexit 71\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", docker+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		name              string
		termination, logs error
		pass              bool
	}{
		{name: "successful_control", pass: true},
		{name: "termination_failure", termination: errors.New("injected container removal failure")},
		{name: "log_collection_is_best_effort", logs: errors.New("injected Docker log failure"), pass: true},
		{name: "both_failures", termination: errors.New("injected container removal failure"), logs: errors.New("injected Docker log failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := t.TempDir()
			d, c := successfulCleanupDeps(t, harness, tc.termination, tc.logs)
			check := func(r SeedResult) {
				t.Helper()
				if r.Pass != tc.pass || !r.FDBAlive || len(r.Command) == 0 {
					t.Fatalf("successful tester/live FDB cleanup verdict=%+v", r)
				}
				if tc.termination != nil {
					if r.Error != tc.termination.Error() {
						t.Fatalf("cleanup diagnostic=%q want %q", r.Error, tc.termination)
					}
				} else if r.Error != "" {
					t.Fatalf("best-effort logs changed verdict: %+v", r)
				}
			}
			r := realRunSeedWithDeps(context.Background(), 146, workload{"api", 20, 1, false, 730}, harness, t.TempDir(), d)
			check(r)
			d.seedRunner = func(ctx context.Context, seed int, w workload, harness, dir string) SeedResult {
				return realRunSeedWithDeps(ctx, seed, w, harness, dir, d)
			}
			out := t.TempDir()
			err := executeRunWithDeps(context.Background(), out, 1, 146, 0, false, workload{"api", 20, 1, false, 730}, harness, d)
			report := guardReadReport(t, out)
			if (err == nil) != tc.pass || report.Total != 1 || report.Pass != boolToCount(tc.pass) || report.Fail != boolToCount(!tc.pass) || report.FDBDeaths != 0 || len(report.Seeds) != 1 || (report.Error == "") != tc.pass {
				t.Fatalf("campaign err=%v report=%+v", err, report)
			}
			check(report.Seeds[0])
			if c.starts != 2 || c.execs != 2 || c.terminations != 2 || c.logs != 2*boolToCount(tc.logs != nil) {
				t.Fatalf("fixture reachability: %+v", c)
			}
		})
	}
}

func boolToCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestSuccessfulCaseTerminationFailureCLIExit(t *testing.T) {
	f := newCLIContractFixture(t)
	// The preflight and tester succeed; only injected container termination fails.
	python := "#!/bin/sh\ncase \"$1\" in -c) echo 'fixture preflight client';; */bindingtester.py) echo 'Test had 0 incorrect result(s) and 0 error(s)';; *) exit 91;; esac\n"
	if err := os.WriteFile(filepath.Join(f.bin, "python3"), []byte(python), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FDB_STRESS_CLEANUP_FAILURE", "1")
	out := filepath.Join(f.root, "out")
	status, output := f.run(t, f.ws, out)
	if status != 1 || !strings.Contains(output, "injected container removal failure") {
		t.Fatalf("cleanup failure exit=%d output=%s", status, output)
	}
	report := readCLIContractReport(t, filepath.Join(out, "report.json"))
	if report.Total != 1 || report.Pass != 0 || report.Fail != 1 || report.FDBDeaths != 0 || len(report.Seeds) != 1 || report.Seeds[0].Pass || !report.Seeds[0].FDBAlive || report.Seeds[0].Error != "injected container removal failure" || report.Error == "" {
		t.Fatalf("cleanup failure CLI report=%+v", report)
	}
}

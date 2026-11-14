//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func guardGit(t *testing.T, fail bool) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n[ \"$#\" = 2 ] && [ \"$1\" = rev-parse ] && [ \"$2\" = HEAD ] || exit 91\n"
	if fail {
		script += "exit 73\n"
	} else {
		script += "printf '  guard-revision  \\n'\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func guardReadReport(t *testing.T, out string) Report {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(out, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunnerDependencyDefaults(t *testing.T) {
	t.Parallel()
	d := defaultRunnerDeps(realRunSeed)
	if d.perSeedTimeout != 5*time.Minute || d.admissionReserve != 330*time.Second || d.now == nil || d.seedRunner == nil || d.newTimeoutContext == nil {
		t.Fatalf("defaults=%+v", d)
	}
	if reflect.ValueOf(d.newTimeoutContext).Pointer() != reflect.ValueOf(context.WithTimeout).Pointer() {
		t.Fatal("default timeout source is not context.WithTimeout")
	}
	parent := context.Background()
	ctx, cancel := d.newTimeoutContext(parent, -time.Second)
	defer cancel()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) || parent.Err() != nil {
		t.Fatalf("default timeout context: err=%v parent=%v", ctx.Err(), parent.Err())
	}
	// Positive real-clock control, not equality against a second derivation.
	before := time.Now()
	got := d.now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("default clock=%v outside [%v,%v]", got, before, after)
	}
}

func TestExecuteRunRevisionFailureStartsNoCases(t *testing.T) {
	guardGit(t, true)
	calls := 0
	d := defaultRunnerDeps(func(context.Context, int, workload, string, string) SeedResult {
		calls++
		return SeedResult{Pass: true, FDBAlive: true}
	})
	out := t.TempDir()
	err := executeRunWithDeps(context.Background(), out, 1, 146, 0, false, workload{"api", 20, 1, false, 730}, "unused", d)
	var exit *exec.ExitError
	if err == nil || !strings.HasPrefix(err.Error(), "record revision:") || !errors.As(err, &exit) || exit.ExitCode() != 73 || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	entries, readErr := os.ReadDir(out)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("revision failure wrote evidence/cases: %v %v", entries, readErr)
	}
}

func TestExecuteRunInitialReportFailureStartsNoSeed(t *testing.T) {
	guardGit(t, false)
	for _, mode := range []string{"write", "rename"} {
		t.Run(mode, func(t *testing.T) {
			out := t.TempDir()
			blocked := filepath.Join(out, "report.json")
			if mode == "write" {
				blocked += ".tmp"
				if err := os.WriteFile(filepath.Join(out, "report.json"), []byte("prior report"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(blocked, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(blocked, "sentinel"), []byte("prior evidence"), 0o644); err != nil {
				t.Fatal(err)
			}
			calls := 0
			d := defaultRunnerDeps(func(context.Context, int, workload, string, string) SeedResult {
				calls++
				return SeedResult{Pass: true, FDBAlive: true}
			})
			err := executeRunWithDeps(context.Background(), out, 1, 146, 0, false, workload{"api", 20, 1, false, 730}, "unused", d)
			if err == nil || calls != 0 || !strings.Contains(err.Error(), blocked) {
				t.Fatalf("initial persistence err=%v calls=%d", err, calls)
			}
			cases, globErr := filepath.Glob(filepath.Join(out, "case-*"))
			if globErr != nil || len(cases) != 0 {
				t.Fatalf("initial persistence failure started cases: %v %v", cases, globErr)
			}
			data, readErr := os.ReadFile(filepath.Join(blocked, "sentinel"))
			if readErr != nil || string(data) != "prior evidence" {
				t.Fatalf("lost evidence: %q %v", data, readErr)
			}
			if mode == "write" {
				data, readErr := os.ReadFile(filepath.Join(out, "report.json"))
				if readErr != nil || string(data) != "prior report" {
					t.Fatalf("lost prior report: %q %v", data, readErr)
				}
			}
		})
	}
}

func TestExecuteRunCaseMkdirFailureStartsNoSeed(t *testing.T) {
	guardGit(t, false)
	out := t.TempDir()
	blocked := filepath.Join(out, "case-00000-api-seed-146")
	if err := os.WriteFile(blocked, []byte("prior evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	d := defaultRunnerDeps(func(context.Context, int, workload, string, string) SeedResult {
		calls++
		return SeedResult{Pass: true, FDBAlive: true}
	})
	err := executeRunWithDeps(context.Background(), out, 1, 146, 0, false, workload{"api", 20, 1, false, 730}, "unused", d)
	r := guardReadReport(t, out)
	if !errors.Is(err, fs.ErrExist) || calls != 0 || r.Total != 0 || len(r.Seeds) != 0 || r.CompleteCycles != 0 || r.Error != err.Error() || !strings.Contains(r.Error, blocked) {
		t.Fatalf("err=%v calls=%d report=%+v", err, calls, r)
	}
	data, readErr := os.ReadFile(blocked)
	if readErr != nil || string(data) != "prior evidence" {
		t.Fatalf("prior evidence=%q err=%v", data, readErr)
	}
}

func TestExecuteRunPreservesPlanErrorAfterSuccessfulCase(t *testing.T) {
	guardGit(t, false)
	out := t.TempDir()
	blocked := filepath.Join(out, "case-00001-api-seed-147")
	if err := os.WriteFile(blocked, []byte("prior evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got []int
	d := defaultRunnerDeps(func(_ context.Context, seed int, w workload, _, _ string) SeedResult {
		got = append(got, seed)
		return SeedResult{Seed: seed, Workload: w, Pass: true, FDBAlive: true}
	})
	err := executeRunWithDeps(context.Background(), out, 2, 146, 0, false, workload{"api", 20, 1, false, 730}, "unused", d)
	r := guardReadReport(t, out)
	if !errors.Is(err, fs.ErrExist) || !reflect.DeepEqual(got, []int{146}) || r.Total != 1 || r.Pass != 1 || r.Fail != 0 || r.FDBDeaths != 0 || r.CompleteCycles != 1 || r.Error != err.Error() || !strings.Contains(r.Error, blocked) {
		t.Fatalf("err=%v seeds=%v report=%+v", err, got, r)
	}
	if verdict := runVerdict(r, false, true); verdict != nil {
		t.Fatalf("fixture did not establish otherwise-successful verdict: %v", verdict)
	}
}

type guardDeadlineContext struct{ context.Context }

func (ctx guardDeadlineContext) Err() error {
	if errors.Is(context.Cause(ctx.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ctx.Context.Err()
}

// The deadline timer is armed only after entry, independent of filesystem latency.
func guardTimeoutAfterEntry(entered <-chan struct{}, target time.Duration) func(context.Context, time.Duration) (context.Context, context.CancelFunc) {
	return func(parent context.Context, duration time.Duration) (context.Context, context.CancelFunc) {
		if duration != target {
			return parent, func() {}
		}
		ctx, cancel := context.WithCancelCause(parent)
		go func() {
			select {
			case <-entered:
				timer := time.AfterFunc(0, func() { cancel(context.DeadlineExceeded) })
				defer timer.Stop()
				<-ctx.Done()
			case <-ctx.Done():
			}
		}()
		return guardDeadlineContext{ctx}, func() { cancel(context.Canceled) }
	}
}

func TestExecuteRunPerSeedDeadlineExpiresWithLiveParent(t *testing.T) {
	guardGit(t, false)
	out := t.TempDir()
	parent := context.Background()
	calls := 0
	var atEntry, afterWait error
	entered := make(chan struct{})
	d := defaultRunnerDeps(func(ctx context.Context, seed int, w workload, _, _ string) SeedResult {
		calls++
		atEntry = ctx.Err()
		close(entered)
		watchdog := time.NewTimer(10 * time.Second)
		defer watchdog.Stop()
		select {
		case <-ctx.Done():
			afterWait = ctx.Err()
		case <-watchdog.C:
			afterWait = errors.New("test watchdog expired without seed deadline")
		}
		return SeedResult{Seed: seed, Workload: w, FDBAlive: true, Error: afterWait.Error()}
	})
	d.perSeedTimeout = time.Minute
	d.newTimeoutContext = guardTimeoutAfterEntry(entered, d.perSeedTimeout)
	err := executeRunWithDeps(parent, out, 1, 146, 0, false, workload{"api", 20, 1, false, 730}, "unused", d)
	r := guardReadReport(t, out)
	if err == nil || calls != 1 || atEntry != nil || !errors.Is(afterWait, context.DeadlineExceeded) || parent.Err() != nil || r.Total != 1 || r.Fail != 1 || r.Pass != 0 || r.FDBDeaths != 0 || len(r.Seeds) != 1 || r.Seeds[0].Error != context.DeadlineExceeded.Error() {
		t.Fatalf("err=%v calls=%d entry=%v after=%v parent=%v report=%+v", err, calls, atEntry, afterWait, parent.Err(), r)
	}
}

func TestExecuteRunPositiveDurationAutomaticallyExpiresActiveCase(t *testing.T) {
	guardGit(t, false)
	out := t.TempDir()
	now := time.Unix(1000000, 0)
	calls := 0
	var atEntry, afterWait error
	entered := make(chan struct{})
	d := defaultRunnerDeps(func(ctx context.Context, seed int, w workload, _, _ string) SeedResult {
		calls++
		atEntry = ctx.Err()
		close(entered)
		watchdog := time.NewTimer(10 * time.Second)
		defer watchdog.Stop()
		select {
		case <-ctx.Done():
			afterWait = ctx.Err()
		case <-watchdog.C:
			afterWait = errors.New("test watchdog expired without outer deadline")
		}
		// Accounting/admission is deterministic; the injected timer drives expiry.
		now = now.Add(time.Second)
		return SeedResult{Seed: seed, Workload: w, FDBAlive: true, Error: afterWait.Error()}
	})
	d.now = func() time.Time { return now }
	d.perSeedTimeout = time.Hour
	d.admissionReserve = 0
	d.newTimeoutContext = guardTimeoutAfterEntry(entered, 100*time.Millisecond)
	err := executeRunWithDeps(context.Background(), out, 1, 146, 100*time.Millisecond, false, workload{"api", 20, 1, false, 730}, "unused", d)
	r := guardReadReport(t, out)
	if err == nil || calls != 1 || atEntry != nil || !errors.Is(afterWait, context.DeadlineExceeded) || r.Total != 1 || r.Fail != 1 || r.Pass != 0 || r.FDBDeaths != 0 || len(r.Seeds) != 1 || r.Seeds[0].Error != context.DeadlineExceeded.Error() {
		t.Fatalf("err=%v calls=%d entry=%v after=%v report=%+v", err, calls, atEntry, afterWait, r)
	}
}

func TestExecuteRunAdmitsExactly330SecondsAndRechecksAfterCase(t *testing.T) {
	guardGit(t, false)
	for _, tc := range []struct {
		name     string
		duration time.Duration
		want     int
	}{
		{"exact", 330 * time.Second, 1},
		{"below", 330*time.Second - time.Nanosecond, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			now := time.Unix(1000000, 0)
			calls := 0
			d := defaultRunnerDeps(func(_ context.Context, seed int, w workload, _, _ string) SeedResult {
				calls++
				now = now.Add(time.Nanosecond)
				// Bound a lowered-reserve mutant without millions of unintended cases.
				if calls >= 2 {
					now = now.Add(time.Hour)
				}
				return SeedResult{Seed: seed, Workload: w, Pass: true, FDBAlive: true}
			})
			d.now = func() time.Time { return now }
			err := executeRunWithDeps(context.Background(), out, 1, 146, tc.duration, false, workload{"api", 20, 1, false, 730}, "unused", d)
			r := guardReadReport(t, out)
			if calls != tc.want || r.Total != tc.want || len(r.Seeds) != tc.want || r.Pass != tc.want || r.Fail != 0 || r.FDBDeaths != 0 || r.CompleteCycles != tc.want || (err == nil) != (tc.want > 0) {
				t.Fatalf("err=%v calls=%d report=%+v", err, calls, r)
			}
			if tc.want == 0 && r.Error != "no completed cases" {
				t.Fatalf("refusal error=%q", r.Error)
			}
			if tc.want == 1 && (r.Seeds[0].Seed != 146 || r.Error != "") {
				t.Fatalf("admitted result=%+v", r)
			}
		})
	}
}

func TestExecuteRunTimedWiringOverridesSeedCount(t *testing.T) {
	guardGit(t, false)
	for _, timed := range []bool{false, true} {
		name := "counted"
		if timed {
			name = "timed"
		}
		t.Run(name, func(t *testing.T) {
			out := t.TempDir()
			now := time.Unix(1000000, 0)
			var got []int
			d := defaultRunnerDeps(func(_ context.Context, seed int, w workload, _, _ string) SeedResult {
				got = append(got, seed)
				if len(got) == 1 {
					now = now.Add(time.Second)
				} else {
					now = now.Add(time.Nanosecond)
				}
				if len(got) >= 3 {
					now = now.Add(time.Hour)
				}
				return SeedResult{Seed: seed, Workload: w, Pass: true, FDBAlive: true}
			})
			d.now = func() time.Time { return now }
			duration := time.Duration(0)
			want := []int{146}
			if timed {
				duration = 331 * time.Second
				want = []int{146, 147}
			}
			err := executeRunWithDeps(context.Background(), out, 1, 146, duration, false, workload{"api", 20, 1, false, 730}, "unused", d)
			r := guardReadReport(t, out)
			if err != nil || !reflect.DeepEqual(got, want) || r.Total != len(want) || r.Pass != len(want) || r.Fail != 0 || r.FDBDeaths != 0 || r.CompleteCycles != len(want) || r.Error != "" {
				t.Fatalf("err=%v seeds=%v want=%v report=%+v", err, got, want, r)
			}
		})
	}
}

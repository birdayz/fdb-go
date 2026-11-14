//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDefensiveKillTesterGroup(t *testing.T) {
	t.Parallel()
	// The unstarted-command guard must not invoke a real syscall.
	if err := killTesterGroupWithKill(&exec.Cmd{}, syscall.Kill); err != nil {
		t.Fatalf("unstarted command: %v", err)
	}
	for _, tc := range []struct {
		name             string
		process          *os.Process
		syscallErr, want error
		calls            int
	}{
		{"nil", nil, syscall.EPERM, nil, 0},
		{"success", &os.Process{Pid: 12345}, nil, nil, 1},
		{"missing", &os.Process{Pid: 12345}, syscall.ESRCH, os.ErrProcessDone, 1},
		{"wrapped_missing", &os.Process{Pid: 12345}, fmt.Errorf("kill: %w", syscall.ESRCH), os.ErrProcessDone, 1},
		{"permission", &os.Process{Pid: 12345}, syscall.EPERM, syscall.EPERM, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got := killTesterGroupWithKill(&exec.Cmd{Process: tc.process}, func(pid int, sig syscall.Signal) error {
				calls++
				if pid != -12345 || sig != syscall.SIGKILL {
					t.Fatalf("kill(%d,%v)", pid, sig)
				}
				return tc.syscallErr
			})
			if got != tc.want || calls != tc.calls {
				t.Fatalf("got=%v calls=%d want=%v/%d", got, calls, tc.want, tc.calls)
			}
		})
	}
}

type defensiveRegistryWriter struct {
	data               strings.Builder
	writeErr, closeErr error
	writes, closes     int
}

func (w *defensiveRegistryWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.data.Write(p)
}
func (w *defensiveRegistryWriter) Close() error { w.closes++; return w.closeErr }

func TestDefensiveRegisterTestersErrors(t *testing.T) {
	t.Parallel()
	writeErr, closeErr := errors.New("injected write failure"), errors.New("injected close failure")
	for _, tc := range []struct {
		name                     string
		writeErr, closeErr, want error
	}{
		{"success", nil, nil, nil},
		{"write", writeErr, nil, writeErr},
		{"close", nil, closeErr, closeErr},
		{"write_wins", writeErr, closeErr, writeErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &defensiveRegistryWriter{writeErr: tc.writeErr, closeErr: tc.closeErr}
			err := registerTesters(w, "/private/harness", "/stable/stacktester")
			if (tc.want == nil && err != nil) || (tc.want != nil && (!errors.Is(err, tc.want) || !strings.HasPrefix(err.Error(), "register testers: "))) {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
			if tc.name == "write_wins" && errors.Is(err, closeErr) {
				t.Fatalf("close replaced write: %v", err)
			}
			if w.writes != 1 || w.closes != 1 {
				t.Fatalf("writes/closes=%d/%d", w.writes, w.closes)
			}
			if tc.want == nil {
				want := "\ntesters['python'] = Tester('python', 'python3 /private/harness/python/tester.py', 2040, 23, 730, types=ALL_TYPES, tenants_enabled=True)\ntesters['pure_go'] = Tester('pure_go', '/stable/stacktester', 2040, 510, 730, types=ALL_TYPES)\n"
				if w.data.String() != want {
					t.Fatalf("registry=%q want=%q", w.data.String(), want)
				}
			}
		})
	}
}

func TestDefensiveLivenessSuffix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, output string
		commandErr   error
		alive, pass  bool
		suffix       string
	}{
		{"dead_success", "test had 0 incorrect result(s) and 0 error(s)", nil, false, false, "; FDB liveness check failed"},
		{"dead_failure", "Tests failed", errors.New("exit sentinel"), false, false, "; FDB liveness check failed"},
		{"alive_success", "test had 0 incorrect result(s) and 0 error(s)", nil, true, true, ""},
		{"alive_failure", "Tests failed", errors.New("exit sentinel"), true, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := SeedResult{}
			applyTesterVerdict(&r, tc.output, tc.commandErr, tc.alive)
			if r.Pass != tc.pass || r.FDBAlive != tc.alive {
				t.Fatalf("result=%+v", r)
			}
			want := ""
			if !tc.pass {
				want = summarizeFailure(tc.output, tc.commandErr) + tc.suffix
			}
			if r.Error != want {
				t.Fatalf("error=%q want=%q", r.Error, want)
			}
		})
	}
	// Existing errors are not overwritten or decorated by verdict construction.
	r := SeedResult{Error: "earlier diagnostic"}
	applyTesterVerdict(&r, "test had 0 incorrect result(s) and 0 error(s)", nil, false)
	if r.Error != "earlier diagnostic" || r.Pass || r.FDBAlive {
		t.Fatalf("prior failure=%+v", r)
	}
}

// Runs only in an isolated subprocess: cwd/PATH changes never race parallel tests.
func TestDefensiveSetupChild(t *testing.T) {
	if os.Getenv("DEFENSIVE_SETUP_CHILD") != "1" {
		return
	}
	mode := os.Getenv("DEFENSIVE_MODE")
	opener := func(path string, flags int, perm os.FileMode) (io.WriteCloser, error) {
		if filepath.Base(path) != "known_testers.py" || flags != os.O_APPEND|os.O_WRONLY || perm != 0 {
			t.Fatalf("open(%q,%d,%v)", path, flags, perm)
		}
		w := &defensiveRegistryWriter{}
		switch mode {
		case "registry_write":
			w.writeErr = errors.New("INJECTED_WRITE")
		case "registry_close":
			w.closeErr = errors.New("INJECTED_CLOSE")
		case "registry_both":
			w.writeErr = errors.New("INJECTED_WRITE")
			w.closeErr = errors.New("INJECTED_CLOSE")
		default:
			return os.OpenFile(path, flags, perm)
		}
		return w, nil
	}
	harness, err := setupWithRegistryOpener(opener)
	if err != nil {
		fmt.Fprintln(os.Stderr, "CONTROLLED_SETUP_ERROR="+err.Error())
		os.Exit(42)
	}
	// Read the exact private registry to observe append preservation.
	data, err := os.ReadFile(filepath.Join(harness, "bindingtester/known_testers.py"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("REGISTRY=%s\n", data)
	if err := os.RemoveAll(harness); err != nil {
		t.Fatal(err)
	}
}

func TestDefensiveSetupFailures(t *testing.T) {
	t.Parallel()
	archive, err := filepath.Abs("python-fdb.tar")
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	realCP, err := exec.LookPath("cp")
	if err != nil {
		t.Fatal(err)
	}
	realCP, err = filepath.Abs(realCP)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"success", "oracle_copy", "registry_open", "registry_write", "registry_close", "registry_both"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			ws := filepath.Join(root, "workspace")
			bin := filepath.Join(root, "bin")
			tmp := filepath.Join(root, "tmp")
			base := filepath.Join(root, "selected")
			write := func(path, body string, perm os.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), perm); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(tmp, 0o755); err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(ws, "bazel-bin/cmd/fdb-stacktester/fdb-stacktester_/fdb-stacktester"), "#!/bin/sh\nexit 0\n", 0o755)
			tarPath := filepath.Join(ws, "bazel-bin/cmd/fdb-binding-stress/python-fdb.tar")
			if err := os.MkdirAll(filepath.Dir(tarPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(archive, tarPath); err != nil {
				t.Fatal(err)
			}
			bindings := filepath.Join(base, "external/foundationdb+/bindings")
			write(filepath.Join(bindings, "bindingtester/__init__.py"), bindingTesterImports, 0o644)
			write(filepath.Join(bindings, "bindingtester/bindingtester.py"), "# oracle fixture\n", 0o644)
			known := filepath.Join(bindings, "bindingtester/known_testers.py")
			if mode == "registry_open" {
				if err := os.MkdirAll(known, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				write(known, "# REGISTRY_PREFIX_SENTINEL\n", 0o644)
			}
			write(filepath.Join(bindings, "python/tests/tester.py"), "# Python oracle\n", 0o644)
			write(filepath.Join(bin, "bazelisk"), "#!/bin/sh\ncase \"$1\" in build) exit 0;; info) printf '%s\\n' \"$SELECTED_BASE\";; *) exit 91;; esac\n", 0o755)
			cpLog, phaseLog := filepath.Join(root, "cp-argv"), filepath.Join(root, "copy-phases")
			// Observe exact argument count/order, both phases, private destinations and cwd.
			// Only the second copy may fail; the first copy always invokes real cp.
			write(filepath.Join(bin, "cp"), `#!/bin/sh
[ "$#" = 3 ] && [ "$1" = -rL ] || exit 91
case "$2" in
 "$BINDINGTESTER_SOURCE") phase=bindingtester; suffix=bindingtester ;;
 "$ORACLE_SOURCE") phase=oracle; suffix=python ;;
 *) exit 92 ;;
esac
[ "$PWD" = "$WORKSPACE" ] || exit 93
parent=${3%/*}
[ "$3" = "$parent/$suffix" ] || exit 94
[ "${parent%/*}" = "$TMPDIR" ] || exit 95
case "${parent##*/}" in fdb-binding-harness-*) ;; *) exit 96 ;; esac
printf '%s\000' "$phase" "$1" "$2" "$3" "$PWD" >> "$CP_ARG_LOG"
printf '%s-start\n' "$phase" >> "$COPY_PHASE_LOG"
if [ "$DEFENSIVE_MODE" = oracle_copy ] && [ "$phase" = oracle ]; then
 printf 'oracle-fail\n' >> "$COPY_PHASE_LOG"
 printf 'ORACLE_COPY_SENTINEL\n' >&2
 exit 73
fi
"$REAL_CP" "$@"
status=$?
[ "$status" = 0 ] || exit "$status"
printf '%s-ok\n' "$phase" >> "$COPY_PHASE_LOG"
`, 0o755)
			// Selective phase observer, not a Python correctness/import test.
			preflight := filepath.Join(root, "preflight")
			write(filepath.Join(bin, "python3"), `#!/bin/sh
[ "$#" = 2 ] && [ "$1" = -c ] && [ "$2" = "$PROBE_PROGRAM" ] || exit 91
[ "${PWD%/*}" = "$TMPDIR" ] || exit 92
case "${PWD##*/}" in fdb-binding-harness-*) ;; *) exit 93 ;; esac
[ -f "$PWD/bindingtester/__init__.py" ] && [ -f "$PWD/fdb/__init__.py" ] || exit 94
printf '%s\000' called "$PWD" "$1" "$2" >> "$PREFLIGHT_LOG"
printf 'fixture preflight\n'
`, 0o755)
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestDefensiveSetupChild$", "-test.v")
			process := configureTesterProcess(cmd)
			cmd.Dir = ws
			cmd.Env = append(os.Environ(), "DEFENSIVE_SETUP_CHILD=1", "BUILD_WORKSPACE_DIRECTORY="+ws, "PATH="+bin+":"+os.Getenv("PATH"), "TMPDIR="+tmp, "SELECTED_BASE="+base, "REAL_CP="+realCP, "DEFENSIVE_MODE="+mode, "ORACLE_SOURCE="+filepath.Join(bindings, "python/tests"), "BINDINGTESTER_SOURCE="+filepath.Join(bindings, "bindingtester"), "WORKSPACE="+ws, "CP_ARG_LOG="+cpLog, "COPY_PHASE_LOG="+phaseLog, "PROBE_PROGRAM="+preflightPythonProgram, "PREFLIGHT_LOG="+preflight)
			out, runErr := process.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("setup subprocess exceeded bound: %v\n%s", ctx.Err(), out)
			}
			copyData, err := os.ReadFile(cpLog)
			if err != nil {
				t.Fatalf("copy observer: %v\n%s", err, out)
			}
			fields := strings.Split(string(copyData), "\x00")
			if len(fields) != 11 || fields[10] != "" {
				t.Fatalf("copy records=%q want two five-field NUL records", copyData)
			}
			harness := filepath.Dir(fields[3])
			if filepath.Dir(harness) != tmp || !strings.HasPrefix(filepath.Base(harness), "fdb-binding-harness-") {
				t.Fatalf("unowned harness=%q", harness)
			}
			expectedCopy := strings.Join([]string{"bindingtester", "-rL", filepath.Join(bindings, "bindingtester"), filepath.Join(harness, "bindingtester"), ws, "oracle", "-rL", filepath.Join(bindings, "python/tests"), filepath.Join(harness, "python"), ws, ""}, "\x00")
			if string(copyData) != expectedCopy {
				t.Fatalf("copy argv/cwd=%q want=%q", copyData, expectedCopy)
			}
			phases, err := os.ReadFile(phaseLog)
			if err != nil {
				t.Fatal(err)
			}
			expectedPhases := "bindingtester-start\nbindingtester-ok\noracle-start\noracle-ok\n"
			if mode == "oracle_copy" {
				expectedPhases = "bindingtester-start\nbindingtester-ok\noracle-start\noracle-fail\n"
			}
			if string(phases) != expectedPhases {
				t.Fatalf("copy phases=%q want=%q", phases, expectedPhases)
			}
			if mode == "success" {
				if runErr != nil || !strings.Contains(string(out), "REGISTRY=# REGISTRY_PREFIX_SENTINEL\n\ntesters['python']") || !strings.Contains(string(out), "testers['pure_go']") {
					t.Fatalf("success=%v\n%s", runErr, out)
				}
				data, err := os.ReadFile(preflight)
				wantProbe := strings.Join([]string{"called", harness, "-c", preflightPythonProgram, ""}, "\x00")
				if err != nil || string(data) != wantProbe {
					t.Fatalf("preflight population/argv/cwd=%q err=%v want exactly one %q", data, err, wantProbe)
				}
			} else {
				var exit *exec.ExitError
				if !errors.As(runErr, &exit) || exit.ExitCode() != 42 {
					t.Fatalf("exit=%v\n%s", runErr, out)
				}
				want := "CONTROLLED_SETUP_ERROR=open known testers: "
				if mode == "oracle_copy" {
					want = "CONTROLLED_SETUP_ERROR=copy Python oracle: ORACLE_COPY_SENTINEL\n: exit status 73"
				}
				if mode == "registry_write" || mode == "registry_both" {
					want = "CONTROLLED_SETUP_ERROR=register testers: INJECTED_WRITE"
				}
				if mode == "registry_close" {
					want = "CONTROLLED_SETUP_ERROR=register testers: INJECTED_CLOSE"
				}
				if mode == "registry_both" && strings.Contains(string(out), "INJECTED_CLOSE") {
					t.Fatalf("close displaced write: %s", out)
				}
				if !strings.Contains(string(out), want) {
					t.Fatalf("missing %q\n%s", want, out)
				}
				if _, err := os.Stat(preflight); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("continued to preflight: %v", err)
				}
			}
			entries, err := os.ReadDir(tmp)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("owned harness leaked: %v", entries)
			}
		})
	}
}

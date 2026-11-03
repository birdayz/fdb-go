package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

const terminalHelperEnv = "FDB_TERMINAL_TEST_HELPER"

var terminalCSI = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]")

func init() {
	if mode := os.Getenv(terminalHelperEnv); mode != "" {
		// These subprocesses exercise terminal routing before TestMain starts
		// Docker; quitting or refusing confirmation must not contact FDB.
		if err := runTerminalHelper(mode); err != nil {
			fmt.Fprintln(os.Stderr, "terminal helper:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, "terminal helper complete")
		os.Exit(0)
	}
}

func runTerminalHelper(mode string) error {
	if !isTerminalReader(os.Stdin) || !isTerminalWriter(os.Stdout) {
		return fmt.Errorf("inherited PTY must be recognized as terminal input and output")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	switch mode {
	case "sql":
		root := NewRoot()
		root.SetIn(os.Stdin)
		root.SetOut(os.Stdout)
		root.SetErr(os.Stderr)
		root.SetArgs([]string{"sql", "--database", "/FRL/terminal", "--schema", "MAIN", "--cluster-file", "/unused.cluster"})
		return root.ExecuteContext(ctx)
	case "confirm-yes", "confirm-no":
		cmd := &cobra.Command{Use: "terminal-confirmation"}
		cmd.SetIn(os.Stdin)
		cmd.SetErr(os.Stderr)
		err := confirmWrite(cmd, false, "save terminal record")
		if mode == "confirm-yes" {
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, "confirmation accepted")
			return nil
		}
		if err == nil || !strings.Contains(err.Error(), "not confirmed") {
			return fmt.Errorf("negative terminal confirmation error=%v", err)
		}
		fmt.Fprintln(os.Stdout, "confirmation refused")
		return nil
	case "truncate-mismatch":
		root := NewRoot()
		root.SetIn(os.Stdin)
		root.SetOut(os.Stdout)
		root.SetErr(os.Stderr)
		root.SetArgs([]string{
			"rl", "store", "truncate", "--yes", "--cluster-file", "/unused.cluster",
			"--keyspace-tuple", `["terminal-target"]`, "--meta-file", "/unused.pb",
		})
		err := root.ExecuteContext(ctx)
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			return fmt.Errorf("truncate must reject the mismatched address before opening a store: %v", err)
		}
		fmt.Fprintln(os.Stdout, "truncate refused")
		return nil
	default:
		return fmt.Errorf("unknown terminal helper mode %q", mode)
	}
}

func openTerminalPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	raw, err := master.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var number int
	var ioctlErr error
	err = raw.Control(func(fd uintptr) {
		if ioctlErr = unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0); ioctlErr != nil {
			return
		}
		if number, ioctlErr = unix.IoctlGetInt(int(fd), unix.TIOCGPTN); ioctlErr != nil {
			return
		}
		ioctlErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 100})
	})
	if err != nil || ioctlErr != nil {
		t.Fatalf("configure PTY: control=%v ioctl=%v", err, ioctlErr)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

func awaitTerminalOutput(t *testing.T, master *os.File, transcript *strings.Builder, want string) {
	t.Helper()
	contains := func() bool {
		return strings.Contains(terminalCSI.ReplaceAllString(transcript.String(), ""), want)
	}
	var buf [4096]byte
	for !contains() {
		n, err := master.Read(buf[:])
		transcript.Write(buf[:n])
		if err != nil && !contains() {
			t.Fatalf("terminal ended before %q: %v\ntranscript:\n%s", want, err, transcript.String())
		}
	}
}

func TestTerminalPromptWithReadlineStyling(t *testing.T) {
	t.Parallel()
	const output = "\x1b[J\x1b[2K\r\x1b[1;94mfdb\x1b[m\x1b[90m(\x1b[m\x1b[95mMAIN\x1b[m\x1b[90m)\x1b[m\x1b[1;94m> \x1b[m \b"
	if got := terminalCSI.ReplaceAllString(output, ""); got != "\rfdb(MAIN)>  \b" {
		t.Fatalf("styled terminal prompt = %q", got)
	}
}

func TestTerminalCommandDispatchAndWriteGuards(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode   string
		prompt string
		input  string
		result string
	}{
		{mode: "sql", prompt: "fdb(MAIN)> ", input: "\\q\n", result: "terminal helper complete"},
		{mode: "confirm-yes", prompt: "Type y to confirm: ", input: "yes\n", result: "confirmation accepted"},
		{mode: "confirm-no", prompt: "Type y to confirm: ", input: "n\n", result: "confirmation refused"},
		{mode: "truncate-mismatch", prompt: "Type the store address to confirm: ", input: "wrong-address\n", result: "truncate refused"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			master, slave := openTerminalPTY(t)
			flags := sqlWorkflowDescriptorFlags(t, master)
			if !isTerminalReader(slave) || !isTerminalWriter(slave) {
				t.Fatal("PTY slave must be recognized as a terminal")
			}
			if sqlWorkflowDescriptorFlags(t, master) != flags {
				t.Fatal("terminal probing changed the PTY polling flags")
			}
			if err := master.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatalf("PTY must support bounded reads and writes: %v", err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, executable)
			child.Env = append(os.Environ(), terminalHelperEnv+"="+tc.mode,
				sqlWorkflowStdinHelper+"=", "HOME="+t.TempDir(), "FDB_CONFIG="+filepath.Join(t.TempDir(), "missing.yaml"),
				"TERM=dumb", "NO_COLOR=1")
			child.Stdin, child.Stdout, child.Stderr = slave, slave, slave
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					cancel()
					_ = child.Wait()
				}
			}()
			if err := slave.Close(); err != nil {
				t.Fatal(err)
			}
			var transcript strings.Builder
			// Keep the master open throughout: the prompt must arrive before
			// input or EOF, not after a redirected script has been consumed.
			awaitTerminalOutput(t, master, &transcript, tc.prompt)
			if tc.mode == "sql" && !strings.Contains(transcript.String(), "fdb sql") {
				t.Fatalf("interactive SQL prompt had no banner:\n%s", transcript.String())
			}
			if _, err := io.WriteString(master, tc.input); err != nil {
				t.Fatal(err)
			}
			awaitTerminalOutput(t, master, &transcript, tc.result)
			awaitTerminalOutput(t, master, &transcript, "terminal helper complete")
			waited = true
			if err := child.Wait(); err != nil {
				t.Fatalf("terminal command %s: %v\n%s", tc.mode, err, transcript.String())
			}
		})
	}
}

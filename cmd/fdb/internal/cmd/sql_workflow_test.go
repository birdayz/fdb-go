package cmd

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"fdb.dev/cmd/fdb/internal/config"
	relapi "fdb.dev/pkg/relational/api"
)

func writeSQLWorkflowConfig(t *testing.T, clusterFile, database string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := fmt.Sprintf(`current_context: relational
contexts:
  - name: relational
    cluster_file: %q
    database: %q
    schema: main
  - name: alternate
    cluster_file: /tmp/alternate.cluster
    database: /FRL/alternate
    schema: other
  - name: quoted
    cluster_file: %q
    database: '"/FRL/Mixed"'
    schema: '"MiXeD"'
  - name: plain
    cluster_file: %q
    keyspace_path: /dev
`, clusterFile, database, clusterFile, clusterFile)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FDB_CONFIG", path)
}

func TestSQLWorkflow_AddressDefaultsAndOverrides(t *testing.T) {
	writeSQLWorkflowConfig(t, "/tmp/workflow.cluster", "/FRL/workflow")
	for _, tc := range []struct {
		name, database, schema, clusterFile, wantErr, statsErr string
		args                                                   []string
	}{
		{name: "context defaults", database: "/FRL/workflow", schema: "main"},
		{name: "schema override retains database", args: []string{"--schema", "other"}, database: "/FRL/workflow", schema: "other"},
		{name: "database override clears schema", args: []string{"--database", "/FRL/new"}, database: "/FRL/new", statsErr: "--schema"},
		{name: "even same database is a new address", args: []string{"--database", "/FRL/workflow"}, database: "/FRL/workflow", statsErr: "--schema"},
		{name: "explicit pair", args: []string{"--database", "/FRL/new", "--schema", "new"}, database: "/FRL/new", schema: "new"},
		{name: "empty explicit database", args: []string{"--database", ""}, wantErr: "--database"},
		{name: "empty explicit schema", args: []string{"--schema", ""}, database: "/FRL/workflow", statsErr: "--schema"},
		{name: "selected context", args: []string{"--context", "alternate"}, database: "/FRL/alternate", schema: "other", clusterFile: "/tmp/alternate.cluster"},
		{name: "cluster override", args: []string{"--cluster-file", "/tmp/explicit.cluster"}, database: "/FRL/workflow", schema: "main", clusterFile: "/tmp/explicit.cluster"},
		{name: "quoted context stays quoted until connection", args: []string{"--context", "quoted"}, database: `"/FRL/Mixed"`, schema: `"MiXeD"`},
		{name: "plain context needs database", args: []string{"--context", "plain"}, wantErr: "--database"},
		{name: "schema alone needs database", args: []string{"--context", "plain", "--schema", "main"}, wantErr: "--database"},
		{name: "explicit pair in plain context", args: []string{"--context", "plain", "--database", "/FRL/new", "--schema", "new"}, database: "/FRL/new", schema: "new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newSQLCmd()
			if err := c.ParseFlags(tc.args); err != nil {
				t.Fatal(err)
			}
			flag := func(name string) string { value, _ := c.Flags().GetString(name); return value }
			target, err := resolveSQLAddress(c, flag("context"), flag("cluster-file"), flag("database"), flag("schema"))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("SQL resolve error = %v, want %s", err, tc.wantErr)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if target.database != tc.database || target.schema != tc.schema {
					t.Fatalf("SQL address = %q/%q, want %q/%q", target.database, target.schema, tc.database, tc.schema)
				}
			}

			var addr statsAddressFlags
			stats := &cobra.Command{Use: "workflow"}
			addr.register(stats)
			if err := stats.ParseFlags(tc.args); err != nil {
				t.Fatal(err)
			}
			err = addr.resolve(stats, false)
			wantErr := tc.statsErr
			if tc.wantErr != "" {
				wantErr = tc.wantErr
			}
			if wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), wantErr) {
					t.Fatalf("stats resolve error = %v, want %s", err, wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantCluster := tc.clusterFile
			if wantCluster == "" {
				wantCluster = "/tmp/workflow.cluster"
			}
			if target.clusterFile() != wantCluster || addr.clusterFile != wantCluster ||
				addr.database != tc.database || addr.schema != tc.schema {
				t.Fatalf("SQL cluster=%q; stats address=%+v; want %s/%s on %s", target.clusterFile(), addr, tc.database, tc.schema, wantCluster)
			}
		})
	}
}

func TestSQLWorkflow_NoContextPreservesSentinel(t *testing.T) {
	t.Setenv("FDB_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	c := newSQLCmd()
	_, err := resolveSQLAddress(c, "", "", "", "")
	if !errors.Is(err, config.ErrNoContext) || !strings.Contains(err.Error(), "--database") {
		t.Fatalf("missing context/address error = %v, want --database and ErrNoContext", err)
	}
	_, err = resolveSQLAddress(c, "missing", "", "/FRL/workflow", "main")
	if !errors.Is(err, config.ErrNoContext) {
		t.Fatalf("explicit missing context error = %v, want ErrNoContext", err)
	}
	if err := c.ParseFlags([]string{"--database", "/FRL/workflow"}); err != nil {
		t.Fatal(err)
	}
	target, err := resolveSQLAddress(c, "", "", "/FRL/workflow", "")
	if err != nil || target.database != "/FRL/workflow" || target.schema != "" || target.clusterFile() != "" {
		t.Fatalf("explicit database with default cluster discovery: target=%+v, err=%v", target, err)
	}
}

func TestSQLWorkflow_FleetDefaultsOnlyDatabase(t *testing.T) {
	writeSQLWorkflowConfig(t, "/tmp/workflow.cluster", "/FRL/workflow")
	for _, args := range [][]string{nil, {"--database", "/FRL/new"}, {"--schema", "main"}, {"--schema", ""}} {
		var addr statsAddressFlags
		c := &cobra.Command{Use: "workflow"}
		addr.register(c)
		if err := c.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		err := addr.resolve(c, true)
		if c.Flags().Changed("schema") {
			if err == nil || !strings.Contains(err.Error(), "--all-schemas") || !strings.Contains(err.Error(), "--schema") {
				t.Fatalf("explicit schema %v: error = %v", args, err)
			}
			continue
		}
		wantDB := "/FRL/workflow"
		if len(args) > 0 {
			wantDB = "/FRL/new"
		}
		if err != nil || addr.database != wantDB || addr.schema != "" {
			t.Fatalf("fleet %v: address=%+v, err=%v", args, addr, err)
		}
	}
}

func TestSQLWorkflow_OutputCompletions(t *testing.T) {
	t.Parallel()
	want := []string{"table", "csv", "json", "ndjson"}
	if got := runCompletion(t, "sql", "--output", ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("SQL output completions = %v, want %v", got, want)
	}
	c := &cobra.Command{Use: "custom"}
	c.Flags().String("output", "binary", "custom format")
	if err := c.RegisterFlagCompletionFunc("output", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"binary"}, cobra.ShellCompDirectiveNoFileComp
	}); err != nil {
		t.Fatal(err)
	}
	registerCompletions(c)
	complete, ok := c.GetFlagCompletionFunc("output")
	if !ok {
		t.Fatal("custom output completer missing")
	}
	got, directive := complete(c, nil, "")
	if !reflect.DeepEqual(got, []string{"binary"}) || directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("custom output completer overwritten: %v, %v", got, directive)
	}
}

func TestSQLWorkflow_ScriptReaderErrors(t *testing.T) {
	t.Parallel()
	var r sqlRunner
	if err := r.runScript(strings.NewReader("-- only a comment\n"), "stdin"); err != nil {
		t.Fatal(err)
	}
	readErr := errors.New("input failed")
	if err := r.runScript(iotest.ErrReader(readErr), "stdin"); !errors.Is(err, readErr) || !strings.Contains(err.Error(), "read stdin:") {
		t.Fatalf("read error = %v, want wrapped read stdin error", err)
	}
	path := filepath.Join(t.TempDir(), "absent.sql")
	if err := r.runFile(path); !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "read "+path+":") {
		t.Fatalf("file error = %v, want original path and not-exist sentinel", err)
	}
	if isTerminalReader(strings.NewReader("SELECT 1;")) {
		t.Fatal("redirected input was classified as interactive")
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	r.ctx = ctx
	if err := r.runScript(iotest.ErrReader(readErr), "stdin"); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "read stdin:") {
		t.Fatalf("expired read = %v, want wrapped context.DeadlineExceeded", err)
	}
}

type sqlWorkflowPipeReader struct {
	*os.File
	reading chan struct{}
}

func (r *sqlWorkflowPipeReader) Read(p []byte) (int, error) {
	select {
	case r.reading <- struct{}{}:
	default:
	}
	return r.File.Read(p)
}

// sqlWorkflowDeadlineContext preserves the timeout cause across the readiness gate.
type sqlWorkflowDeadlineContext struct {
	context.Context
}

func (c sqlWorkflowDeadlineContext) Err() error {
	if errors.Is(context.Cause(c.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

func sqlWorkflowGatedDeadline(timeout time.Duration) (context.Context, context.CancelFunc, func()) {
	ctx, cancel := context.WithCancelCause(context.Background())
	arm := func() {
		deadline, stop := context.WithTimeout(ctx, timeout)
		context.AfterFunc(deadline, func() {
			cancel(deadline.Err())
			stop()
		})
	}
	return sqlWorkflowDeadlineContext{ctx}, func() { cancel(context.Canceled) }, arm
}

func TestSQLWorkflow_DeadlineWaitsForReadiness(t *testing.T) {
	t.Parallel()
	// An immediately expiring deadline must still remain live until explicitly armed.
	ctx, cancel, arm := sqlWorkflowGatedDeadline(0)
	defer cancel()
	ready := make(chan struct{})
	release := make(chan struct{})
	go func() {
		close(ready)
		<-release
		arm()
	}()
	<-ready
	if err := ctx.Err(); err != nil {
		t.Errorf("deadline expired before activation: %v", err)
	}
	select {
	case <-ctx.Done():
		t.Error("deadline signaled before activation")
	default:
	}
	close(release)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("activated deadline did not expire")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("activated deadline error = %v", ctx.Err())
	}
}

func TestSQLWorkflow_CancelBlockedPipe(t *testing.T) {
	t.Setenv("FDB_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	for _, deadline := range []bool{false, true} {
		name := "cancel"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			flags := sqlWorkflowDescriptorFlags(t, reader)
			var ctx context.Context
			var cancel context.CancelFunc
			var arm func()
			wantErr := context.Canceled
			if deadline {
				ctx, cancel, arm = sqlWorkflowGatedDeadline(time.Second)
				wantErr = context.DeadlineExceeded
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			in := &sqlWorkflowPipeReader{File: reader, reading: make(chan struct{}, 1)}
			root := NewRoot()
			var out, errOut bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&errOut)
			root.SetIn(in)
			root.SetArgs([]string{"sql", "--database", "/FRL/workflow", "--cluster-file", filepath.Join(t.TempDir(), "unused.cluster")})
			done := make(chan struct{})
			var readErr error
			go func() {
				readErr = root.ExecuteContext(ctx)
				close(done)
			}()
			t.Cleanup(func() {
				cancel()
				reader.Close()
				writer.Close()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("script reader did not stop after pipe cleanup")
				}
			})
			select {
			case <-in.reading:
			case <-done:
				t.Fatalf("script returned before reading: %v", readErr)
			case <-time.After(5 * time.Second):
				t.Fatal("script never started reading the open pipe")
			}
			if got := sqlWorkflowDescriptorFlags(t, reader); got != flags {
				t.Fatalf("terminal inspection changed descriptor flags from %#x to %#x", flags, got)
			}
			if ctx.Err() != nil {
				t.Fatalf("context expired before read-start proof: %v", ctx.Err())
			}
			if deadline {
				arm()
			} else {
				cancel()
			}
			// Keep the writer open: only cancellation/deadline can stop the read.
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("context did not interrupt the blocked pipe")
			}
			if !errors.Is(readErr, wantErr) || !strings.Contains(readErr.Error(), "read stdin:") {
				t.Fatalf("canceled read = %v, want wrapped %v", readErr, wantErr)
			}
			if _, err := reader.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("cancellation did not close the owned read descriptor: %v", err)
			}
			if _, err := writer.Stat(); err != nil {
				t.Fatalf("writer was closed before the cancellation result: %v", err)
			}
			if out.Len() != 0 || errOut.Len() != 0 {
				t.Fatalf("canceled script printed a banner or result: stdout=%q stderr=%q", out.String(), errOut.String())
			}
		})
	}
}

func sqlWorkflowDescriptorFlags(t *testing.T, f *os.File) int {
	t.Helper()
	raw, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	var flagErr error
	err = raw.Control(func(fd uintptr) { flags, flagErr = unix.FcntlInt(fd, unix.F_GETFL, 0) })
	if err != nil || flagErr != nil {
		t.Fatalf("read descriptor flags: control=%v fcntl=%v", err, flagErr)
	}
	return flags
}

func TestSQLWorkflow_TerminalProbePreservesPipeFlags(t *testing.T) {
	t.Parallel()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	readFlags, writeFlags := sqlWorkflowDescriptorFlags(t, reader), sqlWorkflowDescriptorFlags(t, writer)
	if readFlags&unix.O_NONBLOCK == 0 || writeFlags&unix.O_NONBLOCK == 0 {
		t.Fatal("fixture pipes must begin in Go's nonblocking polling mode")
	}
	if isTerminalReader(reader) || isTerminalWriter(writer) {
		t.Fatal("pipe was classified as a terminal")
	}
	if sqlWorkflowDescriptorFlags(t, reader) != readFlags || sqlWorkflowDescriptorFlags(t, writer) != writeFlags {
		t.Fatal("terminal detection changed descriptor flags")
	}
}

const sqlWorkflowStdinHelper = "FDB_SQL_WORKFLOW_STDIN_HELPER"

func init() {
	if mode := os.Getenv(sqlWorkflowStdinHelper); mode != "" {
		// Exit before TestMain starts its Docker fixture: these child processes
		// exercise only inherited stdin, not SQL execution against a cluster.
		os.Exit(runSQLWorkflowStdinHelper(mode))
	}
}

func runSQLWorkflowStdinHelper(mode string) int {
	if mode == "data" {
		data, err := readSQLScript(context.Background(), os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if _, err := os.Stdin.Stat(); err != nil {
			fmt.Fprintln(os.Stderr, "borrowed stdin closed:", err)
			return 1
		}
		if _, err := os.Stdout.Write(data); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	var ctx context.Context
	var cancel context.CancelFunc
	wantErr := context.Canceled
	switch mode {
	case "cancel":
		ctx, cancel = signal.NotifyContext(context.Background(), os.Interrupt)
	case "deadline":
		var arm func()
		ctx, cancel, arm = sqlWorkflowGatedDeadline(2 * time.Second)
		control := os.NewFile(3, "deadline-activation")
		defer control.Close()
		go func() {
			var activation [1]byte
			if _, err := io.ReadFull(control, activation[:]); err != nil {
				cancel()
				return
			}
			arm()
		}()
		wantErr = context.DeadlineExceeded
	default:
		fmt.Fprintln(os.Stderr, "unknown stdin helper mode:", mode)
		return 1
	}
	defer cancel()
	root := NewRoot()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetIn(os.Stdin)
	root.SetArgs([]string{"sql", "--database", "/FRL/workflow", "--cluster-file", "/unused.cluster", "-o", "json"})
	err := root.ExecuteContext(ctx)
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "read stdin:") || out.Len() != 0 || errOut.Len() != 0 {
		fmt.Fprintf(os.Stderr, "stdin result: error=%v stdout=%q stderr=%q\n", err, out.String(), errOut.String())
		return 1
	}
	if _, err := os.Stdin.Stat(); err != nil {
		fmt.Fprintln(os.Stderr, "borrowed stdin closed:", err)
		return 1
	}
	fmt.Fprintln(os.Stdout, wantErr.Error())
	return 0
}

func sqlWorkflowPipeReadable(t *testing.T, f *os.File) bool {
	t.Helper()
	raw, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var readable bool
	var pollErr error
	err = raw.Control(func(fd uintptr) {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			_, pollErr = unix.Poll(fds, 0)
			if !errors.Is(pollErr, unix.EINTR) {
				break
			}
		}
		readable = fds[0].Revents&unix.POLLIN != 0
	})
	if err != nil || pollErr != nil {
		t.Fatalf("pipe readiness: control=%v poll=%v", err, pollErr)
	}
	return readable
}

func TestSQLWorkflow_InheritedStdin(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"cancel", "deadline", "data"} {
		t.Run(mode, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			// Exec inherits blocking descriptors. Deliberately force that state
			// before recording the parent's shared OFD flags.
			reader.Fd()
			flags := sqlWorkflowDescriptorFlags(t, reader)
			if flags&unix.O_NONBLOCK != 0 {
				t.Fatal("inherited-pipe fixture must be blocking")
			}
			input := "-- inherited read-start proof\n"
			if mode == "data" {
				input = strings.Repeat("payload;\n", 100) + "tail without newline"
			}
			if _, err := io.WriteString(writer, input); err != nil {
				t.Fatal(err)
			}
			if !sqlWorkflowPipeReadable(t, reader) {
				t.Fatal("prefix was not buffered before starting the child")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable)
			cmd.Env = append(os.Environ(), sqlWorkflowStdinHelper+"="+mode,
				"FDB_CONFIG="+filepath.Join(t.TempDir(), "missing.yaml"), "HOME="+t.TempDir())
			cmd.Stdin = reader
			var activate *os.File
			if mode == "deadline" {
				control, activation, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer control.Close()
				defer activation.Close()
				cmd.ExtraFiles = []*os.File{control}
				activate = activation
			}
			var out, errOut bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errOut
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					cancel()
					_ = cmd.Wait()
				}
			}()
			if mode == "data" {
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				// Only the child can drain this prefix. This proves its read has
				// started while the writer remains open, before cancellation.
				for sqlWorkflowPipeReadable(t, reader) {
					select {
					case <-ctx.Done():
						waited = true
						err := cmd.Wait()
						t.Fatalf("child never consumed the prefix: %v\nstdout=%s\nstderr=%s", err, out.String(), errOut.String())
					case <-time.After(5 * time.Millisecond):
					}
				}
				if got := sqlWorkflowDescriptorFlags(t, reader); got != flags {
					t.Fatalf("child changed the parent's shared OFD flags: %#x -> %#x", flags, got)
				}
				if mode == "cancel" {
					if err := cmd.Process.Signal(os.Interrupt); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := activate.Write([]byte{1}); err != nil {
						t.Fatal(err)
					}
				}
			}
			waited = true
			if err := cmd.Wait(); err != nil {
				t.Fatalf("inherited stdin %s: %v\nstdout=%s\nstderr=%s", mode, err, out.String(), errOut.String())
			}
			if errOut.Len() != 0 {
				t.Fatalf("inherited stdin diagnostics: %q", errOut.String())
			}
			if got := sqlWorkflowDescriptorFlags(t, reader); got != flags {
				t.Fatalf("child left shared OFD flags changed: %#x -> %#x", flags, got)
			}
			want := input
			if mode != "data" {
				if _, err := writer.Stat(); err != nil {
					t.Fatalf("writer closed before child completed: %v", err)
				}
				want = context.Canceled.Error() + "\n"
				if mode == "deadline" {
					want = context.DeadlineExceeded.Error() + "\n"
				}
			}
			if out.String() != want {
				t.Fatalf("inherited stdin output=%q, want %q", out.String(), want)
			}
		})
	}
}

func runSQLWorkflow(t *testing.T, input string, args ...string) (string, string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin.sql")
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	return runSQLWorkflowReader(t, in, args...)
}

func runSQLWorkflowReader(t *testing.T, input io.Reader, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := NewRoot()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetIn(input)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	for _, stream := range []string{out.String(), errOut.String()} {
		for _, unwanted := range []string{"\x1b", "fdb sql", "fdb(", "\\? for help"} {
			if strings.Contains(stream, unwanted) {
				t.Fatalf("noninteractive output contains %q: %q", unwanted, stream)
			}
		}
	}
	return out.String(), errOut.String(), err
}

func TestIntegration_SQLWorkflow_StdinAndContext(t *testing.T) {
	database := setupStatsDB(t, "sql_workflow", statsOneTableDDL, "INSERT INTO items VALUES (1, 'alpha'), (2, 'beta');")
	alternate := setupStatsDB(t, "sql_workflow_alt", statsOneTableDDL, "INSERT INTO items VALUES (1, 'alternate');")
	writeSQLWorkflowConfig(t, fixture.clusterFilePath, strings.ToLower(database))
	const quotedDatabase = "/FRL/sqlWorkflowQuoted"
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, ctx := range cfg.Contexts {
		switch ctx.Name {
		case "alternate":
			ctx.ClusterFile, ctx.Database, ctx.Schema = fixture.clusterFilePath, strings.ToLower(alternate), "main"
		case "quoted":
			ctx.Database = `"` + quotedDatabase + `"`
		}
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	const query = "SELECT id, name FROM items WHERE id = 1;\n"
	t.Run("empty stdin", func(t *testing.T) {
		out, errOut, err := runSQLWorkflow(t, "-- comment only\n", "sql", "-o", "json")
		if err != nil || out != "" || errOut != "" {
			t.Fatalf("empty stdin: stdout=%q stderr=%q error=%v", out, errOut, err)
		}
	})
	for _, format := range []string{"table", "csv", "json", "ndjson"} {
		t.Run(format, func(t *testing.T) {
			out, errOut, err := runSQLWorkflow(t, query, "sql", "-o", format)
			if err != nil {
				t.Fatalf("stdin -o %s: %v\nstdout=%s\nstderr=%s", format, err, out, errOut)
			}
			switch format {
			case "table":
				if !strings.Contains(out, "alpha") || !strings.Contains(out, "(1 row,") || errOut != "" {
					t.Fatalf("table stdout=%q stderr=%q", out, errOut)
				}
			case "csv":
				rows, err := csv.NewReader(strings.NewReader(out)).ReadAll()
				if err != nil || !reflect.DeepEqual(rows, [][]string{{"ID", "NAME"}, {"1", "alpha"}}) {
					t.Fatalf("CSV rows=%v err=%v", rows, err)
				}
			case "json":
				var rows []map[string]any
				if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 || rows[0]["ID"] != float64(1) || rows[0]["NAME"] != "alpha" {
					t.Fatalf("JSON stdout=%q err=%v", out, err)
				}
			case "ndjson":
				var row map[string]any
				if err := json.Unmarshal([]byte(out), &row); err != nil || row["ID"] != float64(1) || row["NAME"] != "alpha" {
					t.Fatalf("NDJSON stdout=%q err=%v", out, err)
				}
			}
			if format != "table" && !strings.HasPrefix(errOut, "(1 row,") {
				t.Fatalf("machine-format footer missing from stderr: %q", errOut)
			}
		})
	}

	t.Run("address selectors reach SQL and statistics", func(t *testing.T) {
		for _, tc := range []struct {
			name, row, schema, missing string
			args                       []string
		}{
			{name: "same database resets schema", args: []string{"--database", database}, missing: "--schema"},
			{name: "different database resets schema", args: []string{"--database", alternate}, missing: "--schema"},
			{name: "empty database", args: []string{"--database", ""}, missing: "--database"},
			{name: "empty schema", args: []string{"--schema", ""}, missing: "--schema"},
			{name: "alternate context", args: []string{"--context", "alternate"}, row: "alternate", schema: alternate + "/MAIN"},
			{name: "explicit pair", args: []string{"--database", alternate, "--schema", "main"}, row: "alternate", schema: alternate + "/MAIN"},
			{name: "explicit pair from plain context", args: []string{"--context", "plain", "--database", database, "--schema", "main"}, row: "alpha", schema: database + "/MAIN"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				out, _, err := runSQLWorkflow(t, query, append([]string{"sql", "-o", "json"}, tc.args...)...)
				if tc.missing != "" {
					if out != "" || err == nil {
						t.Fatalf("missing address returned inherited rows: stdout=%q error=%v", out, err)
					}
					if tc.missing == "--database" {
						if !strings.Contains(err.Error(), "missing required flag --database") {
							t.Fatalf("empty database error = %v", err)
						}
					} else {
						var sqlErr *relapi.Error
						if !errors.As(err, &sqlErr) || sqlErr.Code != relapi.ErrCodeUnsupportedQuery || sqlErr.Message != "no schema metadata available" {
							t.Fatalf("schema-reset query error = %v", err)
						}
					}
				} else {
					var rows []map[string]any
					if err != nil || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0]["NAME"] != tc.row {
						t.Fatalf("selected SQL target: stdout=%q error=%v, want %s", out, err, tc.row)
					}
				}
				out, _, err = runSQLWorkflow(t, "", append([]string{"rl", "stats", "show", "-o", "json"}, tc.args...)...)
				if tc.missing != "" {
					if err == nil || !strings.Contains(err.Error(), "missing required flag "+tc.missing) || out != "" {
						t.Fatalf("statistics accepted missing %s: stdout=%q error=%v", tc.missing, out, err)
					}
					return
				}
				var status statsShowResult
				if err != nil || json.Unmarshal([]byte(out), &status) != nil || status.Schema != tc.schema {
					t.Fatalf("selected statistics target: stdout=%q error=%v, want %s", out, err, tc.schema)
				}
			})
		}
	})

	t.Run("quoted context reaches mixed-case database and schema", func(t *testing.T) {
		for _, stmt := range []string{
			`CREATE DATABASE "` + quotedDatabase + `"`,
			`CREATE SCHEMA "` + quotedDatabase + `/MiXeD" WITH TEMPLATE fdbstats_sql_workflow_tpl`,
		} {
			if out, _, err := runSQLWorkflow(t, "", "sql", "--database", `"`+quotedDatabase+`"`, "-c", stmt); err != nil {
				t.Fatalf("quoted setup %q: %v\n%s", stmt, err, out)
			}
		}
		out, _, err := runSQLWorkflow(t, "INSERT INTO items VALUES (1, 'quoted');\n"+query, "sql", "--context", "quoted", "-o", "json")
		var rows []map[string]any
		if err != nil || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0]["NAME"] != "quoted" {
			t.Fatalf("quoted SQL context: stdout=%q error=%v", out, err)
		}
		if out, _, err := runSQLWorkflow(t, "", "rl", "stats", "collect", "--context", "quoted", "-o", "json"); err != nil {
			t.Fatalf("quoted statistics collect: %v\n%s", err, out)
		}
		out, _, err = runSQLWorkflow(t, "", "rl", "stats", "show", "--context", "quoted", "-o", "json")
		var status statsShowResult
		if err != nil || json.Unmarshal([]byte(out), &status) != nil || !status.Usable || status.PerType["ITEMS"] != 1 || status.Schema != quotedDatabase+"/MiXeD" {
			t.Fatalf("quoted statistics target: stdout=%q error=%v", out, err)
		}
	})

	t.Run("multiple statements and trailing statement", func(t *testing.T) {
		out, _, err := runSQLWorkflow(t, "-- comment\n"+query+"SELECT name FROM items WHERE id = 2", "sql", "-o", "ndjson")
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(strings.NewReader(out))
		for _, name := range []string{"alpha", "beta"} {
			var row map[string]any
			if err := dec.Decode(&row); err != nil || row["NAME"] != name {
				t.Fatalf("stdin row=%v err=%v, want %s; stdout=%s", row, err, name, out)
			}
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			t.Fatalf("extra script output: %v, %v", extra, err)
		}
	})

	t.Run("same-line queries retain the script splitter boundary", func(t *testing.T) {
		// TODO.md tracks replacing the line-oriented splitter; keep the live
		// refusal explicit rather than claiming arbitrary SQL scripts work.
		out, _, err := runSQLWorkflow(t, "SELECT id FROM items WHERE id = 1; SELECT id FROM items WHERE id = 2;", "sql", "-o", "ndjson")
		if err == nil || !strings.Contains(err.Error(), "statement 1 in stdin:") || !strings.Contains(err.Error(), "multi-statement batches must be DDL/DML only") || out != "" {
			t.Fatalf("same-line query batch: stdout=%q error=%v", out, err)
		}
	})

	t.Run("real pipe succeeds and stops after failure", func(t *testing.T) {
		for _, fail := range []bool{false, true} {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { reader.Close(); writer.Close() })
			script := query
			if fail {
				script += "SELEC broken;\nINSERT INTO items VALUES (4, 'after_pipe_failure');\n"
			}
			if _, err := io.WriteString(writer, script); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			out, _, err := runSQLWorkflowReader(t, reader, "sql", "-o", "json")
			var rows []map[string]any
			if json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0]["NAME"] != "alpha" {
				t.Fatalf("pipe fail=%t: stdout=%q error=%v", fail, out, err)
			}
			if fail {
				var sqlErr *relapi.Error
				if !errors.As(err, &sqlErr) || sqlErr.Code != relapi.ErrCodeSyntaxError || !strings.Contains(err.Error(), "statement 2 in stdin:") {
					t.Fatalf("pipe failure did not preserve statement context and SQLSTATE: %v", err)
				}
			} else if err != nil {
				t.Fatalf("pipe success: %v", err)
			}
		}
		out, _, err := runSQLWorkflow(t, "SELECT count(*) AS n FROM items WHERE id = 4;", "sql", "-o", "json")
		var rows []map[string]int64
		if err != nil || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0]["N"] != 0 {
			t.Fatalf("pipe executed a write after failure: stdout=%q error=%v", out, err)
		}
	})

	t.Run("stdin and file fail at the same statement", func(t *testing.T) {
		script := "-- first query succeeds\n" + query + "SELEC broken;\nINSERT INTO items VALUES (3, 'after_failure');\n"
		path := filepath.Join(t.TempDir(), "workflow.sql")
		if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, label := range []string{"stdin", path} {
			args := []string{"sql", "-o", "json"}
			if label != "stdin" {
				args = append(args, "-f", path)
			}
			out, _, err := runSQLWorkflow(t, script, args...)
			if err == nil || !strings.Contains(err.Error(), "statement 2 in "+label+":") || !strings.Contains(err.Error(), "42601") || !strings.Contains(out, "alpha") {
				t.Fatalf("%s: stdout=%q error=%v", label, out, err)
			}
			out, _, err = runSQLWorkflow(t, "SELECT count(*) AS n FROM items WHERE id = 3;", "sql", "-o", "json")
			var rows []map[string]int64
			if err != nil || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0]["N"] != 0 {
				t.Fatalf("%s executed the INSERT after failure: stdout=%q error=%v", label, out, err)
			}
		}
	})

	t.Run("command takes precedence over stdin", func(t *testing.T) {
		out, _, err := runSQLWorkflow(t, "SELEC broken;", "sql", "-c", query, "-o", "json")
		if err != nil || !strings.Contains(out, "alpha") {
			t.Fatalf("-c did not take precedence: stdout=%q error=%v", out, err)
		}
	})

	t.Run("explicit empty input flags never execute stdin", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "not-selected.sql")
		if err := os.WriteFile(path, []byte("DELETE FROM items;\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name string
			args []string
		}{
			{name: "empty command", args: []string{"-c", ""}},
			{name: "empty command precedes file", args: []string{"-c", "", "-f", path}},
			{name: "empty file", args: []string{"-f", ""}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				out, errOut, err := runSQLWorkflow(t, "DELETE FROM items;\n", append([]string{"sql"}, tc.args...)...)
				if tc.name == "empty file" {
					var pathErr *os.PathError
					if !errors.As(err, &pathErr) || pathErr.Op != "open" || pathErr.Path != "" {
						t.Fatalf("empty -f must fail opening its path: %v", err)
					}
				} else if err != nil {
					t.Fatalf("empty -c must be a no-op: %v", err)
				}
				if out != "" || errOut != "" {
					t.Fatalf("empty input flags produced output: stdout=%q stderr=%q", out, errOut)
				}
				out, _, err = runSQLWorkflow(t, "", "sql", "-c", "SELECT count(*) AS n FROM items;", "-o", "json")
				var rows []map[string]int64
				if err != nil || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0]["N"] != 2 {
					t.Fatalf("empty input flag executed stdin or a lower-priority file: stdout=%q error=%v", out, err)
				}
			})
		}
	})

	t.Run("schema override retains the context database", func(t *testing.T) {
		if out, _, err := runSQLWorkflow(t, "", "sql", "-c", "CREATE SCHEMA "+database+"/other WITH TEMPLATE fdbstats_sql_workflow_tpl"); err != nil {
			t.Fatalf("create other schema: %v\n%s", err, out)
		}
		out, _, err := runSQLWorkflow(t, "INSERT INTO items VALUES (9, 'other_schema');\nSELECT name FROM items WHERE id = 9;", "sql", "--schema", "other", "-o", "json")
		var rows []map[string]any
		if err != nil || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0]["NAME"] != "other_schema" {
			t.Fatalf("schema override: stdout=%q error=%v", out, err)
		}
		out, _, err = runSQLWorkflow(t, "SELECT count(*) AS n FROM items;", "sql", "-o", "json")
		if err != nil || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0]["N"] != float64(2) {
			t.Fatalf("override changed MAIN's rows: stdout=%q error=%v", out, err)
		}
	})

	t.Run("stats uses the relational context", func(t *testing.T) {
		if out, _, err := runSQLWorkflow(t, "", "rl", "stats", "collect", "-o", "json"); err != nil {
			t.Fatalf("collect: %v\n%s", err, out)
		}
		if _, _, err := runSQLWorkflow(t, "", "rl", "stats", "clear"); err == nil || !strings.Contains(err.Error(), database+"/MAIN without --yes") {
			t.Fatalf("unguarded clear did not name the resolved target: %v", err)
		}
		out, _, err := runSQLWorkflow(t, "", "rl", "stats", "show", "-o", "json")
		var status statsShowResult
		if err != nil || json.Unmarshal([]byte(out), &status) != nil || !status.Usable || status.PerType["ITEMS"] != 2 || status.Schema != database+"/MAIN" {
			t.Fatalf("context stats status=%+v stdout=%q error=%v", status, out, err)
		}
		if out, _, err := runSQLWorkflow(t, "", "rl", "stats", "clear", "--yes"); err != nil || !strings.Contains(out, database+"/MAIN") {
			t.Fatalf("clear: %v\n%s", err, out)
		}
		out, _, err = runSQLWorkflow(t, "", "rl", "stats", "show", "-o", "json")
		status = statsShowResult{}
		if err != nil || json.Unmarshal([]byte(out), &status) != nil || status.Found == nil || *status.Found {
			t.Fatalf("clear left statistics: stdout=%q error=%v", out, err)
		}
	})

	t.Run("fleet context and config-free default discovery", func(t *testing.T) {
		for _, configFree := range []bool{false, true} {
			name := "relational context"
			if configFree {
				name = "default cluster discovery"
			}
			t.Run(name, func(t *testing.T) {
				var address []string
				if configFree {
					t.Setenv("FDB_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
					t.Setenv("FDB_CLUSTER_FILE", fixture.clusterFilePath)
					address = []string{"--database", database}
					out, _, err := runSQLWorkflow(t, query, "sql", "--database", database, "--schema", "main", "-o", "json")
					var rows []map[string]any
					if err != nil || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0]["NAME"] != "alpha" {
						t.Fatalf("config-free SQL default discovery: stdout=%q error=%v", out, err)
					}
				}
				for _, schema := range []string{"main", "other"} {
					args := append([]string{"rl", "stats", "clear", "--schema", schema, "--yes"}, address...)
					if out, _, err := runSQLWorkflow(t, "", args...); err != nil {
						t.Fatalf("clear %s before fleet: %v\n%s", schema, err, out)
					}
					out, _, err := runSQLWorkflow(t, "", append([]string{"rl", "stats", "show", "--schema", schema, "-o", "json"}, address...)...)
					var status statsShowResult
					if err != nil || json.Unmarshal([]byte(out), &status) != nil || status.Found == nil || *status.Found {
						t.Fatalf("%s was not cleared before fleet: stdout=%q error=%v", schema, out, err)
					}
				}
				out, errOut, err := runSQLWorkflow(t, "", append([]string{"rl", "stats", "collect", "--all-schemas"}, address...)...)
				if err != nil {
					t.Fatalf("fleet: %v\nstdout=%s\nstderr=%s", err, out, errOut)
				}
				tally := make(map[string]string)
				for _, field := range strings.Fields(out) {
					if key, value, ok := strings.Cut(field, "="); ok {
						tally[key] = value
					}
				}
				if tally["targets"] != "2" || tally["collected"] != "2" || tally["failed"] != "0" || tally["refused"] != "0" {
					t.Fatalf("fleet did not collect exactly MAIN and OTHER: %q", out)
				}
				for schema, count := range map[string]int64{"MAIN": 2, "OTHER": 1} {
					out, _, err := runSQLWorkflow(t, "", append([]string{"rl", "stats", "show", "--schema", schema, "-o", "json"}, address...)...)
					var status statsShowResult
					if err != nil || json.Unmarshal([]byte(out), &status) != nil || !status.Usable || status.PerType["ITEMS"] != count || status.Schema != database+"/"+schema {
						t.Fatalf("fleet %s status: stdout=%q error=%v, want %d rows", schema, out, err, count)
					}
				}
			})
		}
	})
}

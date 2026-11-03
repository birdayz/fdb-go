package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRootCommandNamespaces(t *testing.T) {
	t.Parallel()
	root := NewRoot()
	if got := root.Name(); got != "fdb" {
		t.Fatalf("root name = %q, want fdb", got)
	}
	for _, tc := range []struct {
		path     []string
		children []string
	}{
		{nil, []string{"config", "dev", "rl", "sql", "tx", "version"}},
		{[]string{"rl"}, []string{"index", "keyspace", "meta", "record", "stats", "status", "store"}},
		{[]string{"dev"}, []string{"down", "status", "up"}},
		{[]string{"rl", "store"}, []string{"create", "dump", "info", "lock", "truncate", "unlock"}},
	} {
		c, remaining, err := root.Find(tc.path)
		if err != nil || len(remaining) != 0 {
			t.Fatalf("find %v: command=%v remaining=%v error=%v", tc.path, c, remaining, err)
		}
		var got []string
		for _, child := range c.Commands() {
			got = append(got, child.Name())
			if len(child.Aliases) != 0 {
				t.Errorf("%s has unexpected aliases: %v", child.CommandPath(), child.Aliases)
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, tc.children) {
			t.Errorf("%s children = %v, want %v", c.CommandPath(), got, tc.children)
		}
	}
}

func TestRecordLayerCommandsRequireNamespace(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"record", "index", "store", "meta", "stats", "keyspace", "status", "fdb", "frl"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := runCmd(t, name, "--help")
			if err == nil || !strings.Contains(err.Error(), "unknown command") {
				t.Fatalf("fdb %s --help: error=%v output=%q; want unknown command", name, err, out)
			}
		})
	}
}

func TestRootHelpPaths(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--help"},
		{"rl", "--help"},
		{"rl", "record", "get", "--help"},
		{"rl", "index", "build", "--help"},
		{"rl", "store", "info", "--help"},
		{"rl", "store", "create", "--help"},
		{"rl", "meta", "catalog", "databases", "--help"},
		{"rl", "stats", "show", "--help"},
		{"rl", "keyspace", "resolve", "--help"},
		{"rl", "status", "--help"},
		{"dev", "up", "--help"},
		{"sql", "--help"},
		{"tx", "read-version", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			out, err := runCmd(t, args...)
			if err != nil {
				t.Fatalf("help %v: %v\n%s", args, err, out)
			}
			want := "fdb"
			if len(args) > 1 {
				want += " " + strings.Join(args[:len(args)-1], " ")
			}
			if !strings.Contains(out, "Usage:\n  "+want) {
				t.Errorf("help does not show usage for %q:\n%s", want, out)
			}
		})
	}
}

func TestCommandExamplesUseRegisteredPaths(t *testing.T) {
	t.Parallel()
	root := NewRoot()
	var check func(*cobra.Command)
	examples := 0
	check = func(c *cobra.Command) {
		for line := range strings.SplitSeq(c.Example, "\n") {
			args := strings.Fields(line)
			if len(args) > 0 && args[0] == "frl" {
				t.Errorf("%s example uses the removed frl command: %q", c.CommandPath(), line)
			}
			if len(args) < 2 || args[0] != "fdb" {
				continue
			}
			examples++
			found, _, err := root.Find(args[1:])
			if err != nil {
				t.Errorf("%s example %q: %v", c.CommandPath(), line, err)
				continue
			}
			if !found.Runnable() {
				t.Errorf("%s example %q resolves to non-runnable %s", c.CommandPath(), line, found.CommandPath())
			}
		}
		for _, child := range c.Commands() {
			check(child)
		}
	}
	check(root)
	if examples == 0 {
		t.Fatal("no command examples checked")
	}
}

func TestSQLPromptUsesBinaryName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		schema string
		inTx   bool
		want   string
	}{
		{"", false, "fdb> "},
		{"", true, "fdb*> "},
		{"MAIN", false, "fdb(MAIN)> "},
		{"MAIN", true, "fdb(MAIN)*> "},
	} {
		r := &sqlRunner{schema: tc.schema, inTx: tc.inTx, st: plainSQLStyles()}
		if got := r.prompt(); got != tc.want {
			t.Errorf("prompt(%q, inTx=%t) = %q, want %q", tc.schema, tc.inTx, got, tc.want)
		}
	}
}

func TestSQLHistoryUsesFDBDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, ".fdb", "sql_history")
	if got := historyPath(); got != want {
		t.Fatalf("historyPath = %q, want %q", got, want)
	}
	info, err := os.Stat(filepath.Dir(want))
	if err != nil || !info.IsDir() {
		t.Fatalf("history directory: info=%v error=%v", info, err)
	}
}

func TestCompletionRespectsNamespaces(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{""}, []string{"completion", "config", "dev", "help", "rl", "sql", "tx", "version"}},
		{[]string{"rl", ""}, []string{"index", "keyspace", "meta", "record", "stats", "status", "store"}},
		{[]string{"dev", ""}, []string{"down", "status", "up"}},
	} {
		got := runCompletion(t, tc.args...)
		for i, candidate := range got {
			got[i], _, _ = strings.Cut(candidate, "\t")
		}
		slices.Sort(got)
		if !slices.Equal(got, tc.want) {
			t.Errorf("completion %v = %v, want %v", tc.args, got, tc.want)
		}
	}
}

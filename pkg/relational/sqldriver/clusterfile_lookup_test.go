//go:build !libfdbc

package sqldriver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConnectorClusterFileLookup(t *testing.T) {
	t.Parallel()
	if mode := os.Getenv("FDBSQL_LOOKUP_CHILD"); mode != "" {
		dsn := "fdbsql:///FRL/LOOKUP"
		if mode == "explicit" {
			dsn += "?cluster_file=explicit.cluster"
		}
		connector, err := (&Driver{}).OpenConnector(dsn)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := connector.Connect(ctx)
		if conn != nil {
			_ = conn.Close()
		}
		want := os.Getenv("FDBSQL_LOOKUP_WANT")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("connect error = %v; want %q", err, want)
		}
		return
	}
	for _, mode := range []string{"explicit", "environment", "local", "missing", "empty"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for _, name := range []string{"explicit.cluster", "env.cluster", "fdb.cluster"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("invalid_"+name), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConnectorClusterFileLookup$", "-test.v")
			cmd.Dir = dir
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "FDB_CLUSTER_FILE=") && !strings.HasPrefix(entry, "FDBSQL_LOOKUP_") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			want := "invalid_fdb.cluster"
			switch mode {
			case "explicit":
				cmd.Env = append(cmd.Env, "FDB_CLUSTER_FILE=env.cluster")
				want = "invalid_explicit.cluster"
			case "environment":
				cmd.Env = append(cmd.Env, "FDB_CLUSTER_FILE=env.cluster")
				want = "invalid_env.cluster"
			case "missing":
				cmd.Env = append(cmd.Env, "FDB_CLUSTER_FILE=missing.cluster")
				want = "open missing.cluster:"
			case "empty":
				cmd.Env = append(cmd.Env, "FDB_CLUSTER_FILE=")
				want = "open :"
			}
			cmd.Env = append(cmd.Env, "FDBSQL_LOOKUP_CHILD="+mode, "FDBSQL_LOOKUP_WANT="+want)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("lookup subprocess: %v\n%s", err, out)
			}
		})
	}
}

package fdb

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenDefaultClusterLookup(t *testing.T) {
	t.Parallel()
	if mode := os.Getenv("FDBGO_LOOKUP_CHILD"); mode != "" {
		if err := APIVersion(730); err != nil {
			t.Fatal(err)
		}
		var err error
		if mode == "explicit" {
			_, err = OpenDatabase("explicit.cluster")
		} else {
			_, err = OpenDefault()
		}
		want := os.Getenv("FDBGO_LOOKUP_WANT")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("open error = %v; want %q", err, want)
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
			cmd := exec.Command(os.Args[0], "-test.run=^TestOpenDefaultClusterLookup$", "-test.v")
			cmd.Dir = dir
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "FDB_CLUSTER_FILE=") {
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
			cmd.Env = append(cmd.Env, "FDBGO_LOOKUP_CHILD="+mode, "FDBGO_LOOKUP_WANT="+want)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("lookup subprocess: %v\n%s", err, out)
			}
		})
	}
}

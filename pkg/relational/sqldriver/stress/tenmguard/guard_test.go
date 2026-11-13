// Package tenmguard checks that the 10M stress binary refuses to run without a
// real cluster: its target is tagged realcluster and never runs in a lane, so a
// guard that stopped firing would otherwise go unseen until a 10M run wedged a
// testcontainer.
package tenmguard

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTenMBinaryRefusesWithoutCluster(t *testing.T) {
	t.Parallel()
	bin := os.Getenv("STRESS_10M_BIN")
	if bin == "" {
		t.Fatal("STRESS_10M_BIN is not set; the BUILD file passes the stress_10m_test binary")
	}
	// rootpath is relative to the runfiles root; the test runs in its package dir.
	if !filepath.IsAbs(bin) {
		ws := os.Getenv("TEST_WORKSPACE")
		if ws == "" {
			ws = "_main"
		}
		bin = filepath.Join(os.Getenv("TEST_SRCDIR"), ws, bin)
	}
	cmd := exec.Command(bin, "-test.run=^$")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "FDB_STRESS_CLUSTER_FILE=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("10M binary without a cluster: err %v, want exit status 1\n%s", err, out)
	}
	if want := "the 10M scales need a real cluster"; !strings.Contains(string(out), want) {
		t.Fatalf("10M binary output lacks %q:\n%s", want, out)
	}
}

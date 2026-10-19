package wiregolden_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/transport"
	tcfdb "fdb.dev/pkg/testcontainers/foundationdb"
	"fdb.dev/tools/clientbench/internal/wgscenarios"
	"fdb.dev/tools/clientbench/internal/wiregolden"
)

// The Go client sends exactly go.golden's requests for the wire-golden
// scenarios, recorded in process from the transport's send path; and
// go.golden differs from libfdb_c's golden only by go-divergences.diff. The
// libfdb_c side is captured off the fast lane (scripts/wiregolden.sh).
func TestGoClientMatchesGolden(t *testing.T) {
	want, err := os.ReadFile("go.golden")
	if err != nil {
		t.Fatal(err)
	}
	checkDivergences(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := tcfdb.Run(ctx, "", tcfdb.WithDirectIP())
	if err != nil {
		t.Fatalf("FDB container: %v", err)
	}
	t.Cleanup(func() { tcfdb.TerminateT(t, context.Background(), container) })
	clusterFile, err := container.ClusterFilePath(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := fdb.APIVersion(730); err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	transport.SetWireLog(&log) // before the first connection
	db, err := fdb.OpenDatabase(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	runErr := wgscenarios.Run(db)
	db.Close() // waits for the transport loops, so the log is complete
	transport.SetWireLog(nil)
	if runErr != nil {
		t.Fatal(runErr)
	}

	c, err := wiregolden.DecodeWireLog(log.Bytes())
	if err != nil || !c.Complete {
		t.Fatalf("wire log: %v (complete %v)", err, c != nil && c.Complete)
	}
	got, err := wiregolden.Transcript(c.Requests, "\x02wg/d/", wgscenarios.Names())
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("WIREGOLDEN_GO: the Go client's requests changed; diff against go.golden:\n%s", unifiedDiff(t, string(want), got))
	}
}

// go-divergences.diff is exactly how go.golden differs from libfdbc.golden.
func checkDivergences(t *testing.T) {
	t.Helper()
	want, err := os.ReadFile("go-divergences.diff")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("diff", "-u", "--label", "golden", "--label", "go", "libfdbc.golden", "go.golden").Output()
	if code := exitCode(err); code > 1 {
		t.Fatalf("diff: %v", err)
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("WIREGOLDEN_DIVERGENCES: go-divergences.diff is not diff -u libfdbc.golden go.golden:\n%s", out)
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := err.(*exec.ExitError); ok {
		return e.ExitCode()
	}
	return 2
}

func unifiedDiff(t *testing.T, want, got string) string {
	t.Helper()
	dir := t.TempDir()
	w, g := dir+"/go.golden", dir+"/go.transcript"
	if os.WriteFile(w, []byte(want), 0o644) != nil || os.WriteFile(g, []byte(got), 0o644) != nil {
		return got
	}
	out, _ := exec.Command("diff", "-u", w, g).Output()
	return string(out)
}

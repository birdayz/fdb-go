package wiregolden_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	stop := transport.CaptureWireLog(&log) // before the first connection
	defer stop()
	db, err := fdb.OpenDatabase(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	runErr := runBounded(2*time.Minute, func() error { return wgscenarios.Run(db) }, db.Close)
	db.Close()
	stop() // nothing writes to log after this
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

// runBounded runs fn, and fails after limit, calling abort to unblock it and
// waiting for it to return: a scenario blocked on an unavailable cluster must
// not hold the test until Bazel's timeout.
func runBounded(limit time.Duration, fn func() error, abort func()) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		abort()
		if err := <-done; err != nil {
			return fmt.Errorf("scenarios did not finish within %v: %w", limit, err)
		}
		return fmt.Errorf("scenarios did not finish within %v", limit)
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
	if !diffCompleted(err) {
		t.Fatalf("diff: %v", err)
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("WIREGOLDEN_DIVERGENCES: go-divergences.diff is not diff -u libfdbc.golden go.golden:\n%s", out)
	}
}

// diffCompleted reports whether diff exited normally with 0 (same) or 1
// (different). Anything else, a signal included (ExitCode -1), means its
// output may be partial.
func diffCompleted(err error) bool {
	if err == nil {
		return true
	}
	var e *exec.ExitError
	return errors.As(err, &e) && e.Exited() && e.ExitCode() == 1
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

// A run that blocks past the limit is aborted and reported, not waited for.
func TestRunBoundedAborts(t *testing.T) {
	unblock := make(chan struct{})
	err := runBounded(10*time.Millisecond, func() error { <-unblock; return nil }, func() { close(unblock) })
	if err == nil {
		t.Fatal("WIREGOLDEN_BOUNDED: a blocked run was not reported")
	}
	if err := runBounded(time.Minute, func() error { return nil }, func() { t.Fatal("aborted a finished run") }); err != nil {
		t.Fatal(err)
	}
}

// Only a normal exit with 0 or 1 counts as a completed diff.
func TestDiffCompleted(t *testing.T) {
	run := func(script string) error { return exec.Command("sh", "-c", script).Run() }
	for _, tc := range []struct {
		script string
		want   bool
	}{{"exit 0", true}, {"exit 1", true}, {"exit 2", false}, {"kill -KILL $$", false}} {
		if got := diffCompleted(run(tc.script)); got != tc.want {
			t.Errorf("WIREGOLDEN_DIFF_EXIT %q: %v, want %v", tc.script, got, tc.want)
		}
	}
	if diffCompleted(errors.New("not started")) {
		t.Error("a diff that did not run completed")
	}
}

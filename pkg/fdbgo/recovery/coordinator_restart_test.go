package recovery_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	tcfdb "fdb.dev/pkg/testcontainers/foundationdb"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

//go:embed cluster.bash
var monitorEntrypoint string

// A restarted coordinator seeds its leader monitor from the first
// OpenDatabaseCoordRequest it receives (Coordination.actor.cpp:331-333). A Go
// request without the coordinator list wedged it for every client, libfdb_c
// included. Nothing but the Go client talks to the cluster until it recovers,
// so its request is the first the restarted coordinator sees.
func TestCoordinatorRestart_GoClientFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	c := startCluster(t, ctx)
	generation := waitHealthy(t, ctx, c, 0)

	cf, err := client.ParseClusterString(c.InternalClusterFile())
	if err != nil {
		t.Fatal(err)
	}
	db, err := client.OpenDatabaseFromConfig(ctx, cf, client.WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key := []byte("coordinator-restart")
	setKey(t, ctx, db, key, "before")

	// SIGKILL every fdbserver; fdbmonitor restarts them, coordinator included, and
	// the cluster recovers onto new proxies the Go client can only learn of from
	// the coordinator.
	before := fdbserverPIDs(t, ctx, c)
	if len(before) != 3 {
		t.Fatalf("want three fdbservers before the kill, got %v", before)
	}
	shell(t, ctx, c, "kill -KILL "+strings.Join(before, " "))

	opCtx, opCancel := context.WithTimeout(ctx, 90*time.Second)
	defer opCancel()
	if _, err := db.CreateTransaction().GetReadVersion(opCtx); err != nil {
		t.Fatalf("GRV after coordinator restart: %v", err)
	}
	setKey(t, opCtx, db, key, "after")
	after := fdbserverPIDs(t, ctx, c)
	for _, pid := range before {
		if slices.Contains(after, pid) {
			t.Fatalf("fdbserver %s survived SIGKILL: before=%v after=%v", pid, before, after)
		}
	}

	// libfdb_c must not find the coordinator wedged.
	out, err := c.FDBCLIExec(opCtx, "get "+string(key))
	if err != nil || !strings.Contains(out, "`coordinator-restart' is `after'") {
		t.Fatalf("fdbcli get after Go reached the restarted coordinator first: %v: %s", err, out)
	}
	waitHealthy(t, ctx, c, generation+1) // the kill forced a recovery

	// A fresh Go client bootstraps through the restarted coordinator.
	fresh, err := client.OpenDatabaseFromConfig(opCtx, cf, client.WithAPIVersion(730))
	if err != nil {
		t.Fatalf("fresh client after coordinator restart: %v", err)
	}
	defer fresh.Close()
	got, err := fresh.ReadTransact(opCtx, func(tx *client.Transaction) (any, error) {
		return tx.Get(opCtx, key)
	})
	if err != nil || string(got.([]byte)) != "after" {
		t.Fatalf("fresh client read = %q, %v; want after", got, err)
	}
}

func startCluster(t *testing.T, ctx context.Context) *tcfdb.Container {
	t.Helper()
	startup, stop := context.WithTimeout(ctx, 2*time.Minute)
	defer stop()
	c, err := tcfdb.Run(startup, "", tcfdb.WithDirectIP(), tcfdb.WithStorageEngine("ssd"),
		tcfdb.WithRedundancyMode("double"), testcontainers.WithEntrypoint(
			"/usr/bin/tini", "-g", "--", "/bin/bash", "-ceu", monitorEntrypoint))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := tcfdb.Terminate(cleanup, c); err != nil {
			t.Errorf("terminate container: %v", err)
		}
	})
	return c
}

func setKey(t *testing.T, ctx context.Context, db *client.Database, key []byte, value string) {
	t.Helper()
	if _, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) {
		tx.Set(key, []byte(value))
		return nil, nil
	}); err != nil {
		t.Fatalf("set %s=%s: %v", key, value, err)
	}
}

type clusterStatus struct {
	Cluster struct {
		Available  bool                       `json:"database_available"`
		Replicated bool                       `json:"full_replication"`
		Generation int64                      `json:"generation"`
		Processes  map[string]json.RawMessage `json:"processes"`
		Recovery   struct {
			Name string `json:"name"`
		} `json:"recovery_state"`
	} `json:"cluster"`
}

// waitHealthy waits for a fully recovered, replicated three-process cluster at
// generation >= minimum and returns its generation.
func waitHealthy(t *testing.T, ctx context.Context, c *tcfdb.Container, minimum int64) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var last string
	for {
		attempt, stop := context.WithTimeout(ctx, 10*time.Second)
		out, err := c.FDBCLIExec(attempt, "status json")
		stop()
		var s clusterStatus
		if err == nil && json.Unmarshal([]byte(out), &s) == nil {
			cl := s.Cluster
			if cl.Available && cl.Replicated && len(cl.Processes) == 3 && cl.Generation >= minimum &&
				cl.Recovery.Name == "fully_recovered" {
				return cl.Generation
			}
			last = fmt.Sprintf("%+v", cl)
		} else {
			last = fmt.Sprintf("%v: %.200s", err, out)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("cluster not healthy at generation >= %d: %v (last: %s)", minimum, ctx.Err(), last)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func fdbserverPIDs(t *testing.T, ctx context.Context, c *tcfdb.Container) []string {
	t.Helper()
	return strings.Fields(shell(t, ctx, c, "pgrep -x fdbserver || true"))
}

// shell runs script in the container, bounded on both sides of the exec.
func shell(t *testing.T, ctx context.Context, c *tcfdb.Container, script string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		code, r, err := c.Exec(ctx, []string{"timeout", "-s", "KILL", "10", "/bin/bash", "-ceu", script}, tcexec.Multiplexed())
		if err != nil {
			done <- result{err: err}
			return
		}
		out, err := io.ReadAll(r)
		if err == nil && code != 0 {
			err = fmt.Errorf("exit %d", code)
		}
		done <- result{string(out), err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("container exec %q: %v: %s", script, r.err, r.out)
		}
		return strings.TrimSpace(r.out)
	case <-ctx.Done():
		t.Fatalf("container exec %q: %v", script, ctx.Err())
		return ""
	}
}

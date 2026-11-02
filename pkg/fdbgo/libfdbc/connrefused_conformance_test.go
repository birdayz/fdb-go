//go:build cgo && libfdbc

package libfdbc_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/libfdbc"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// These tests run one fault against both clients: a storage server process is
// killed, so every connect to its address is refused, exactly the transport
// failure libfdb_c turns into request_maybe_delivered and absorbs in loadBalance
// (FlowTransport.actor.cpp:852-869, 933, 1001-1004; genericactors.actor.h:380-385;
// LoadBalance.actor.h:344-377, 604-635). The observable contract both must meet:
// no non-FDB error reaches the application, a read on a shard with a live replica
// completes from it, a read on a shard whose only replica refuses waits until the
// replica is back, and any error code surfaced is an FDB code the transaction's
// OnError retries, with the same final outcome on both clients.

// readKinds are the storage reads the record layer issues.
var readKinds = []string{"get", "getkey", "getrange"}

// crFixture is the seeded keyspace and the cluster file both clients open.
type crFixture struct {
	clusterFile string
	container   *foundationdbtc.Container
	prefix      []byte
	n           int
	valueSize   int
}

func (f crFixture) key(i int) []byte {
	return append(append([]byte{}, f.prefix...), fmt.Sprintf("%04d", i)...)
}

func (f crFixture) value(i int) []byte {
	v := []byte(strings.Repeat(string(rune('a'+i%26)), f.valueSize))
	copy(v, fmt.Sprintf("%04d", i))
	return v
}

func (f crFixture) rangeEnd() []byte { return append(append([]byte{}, f.prefix...), 0xff) }

// digest normalizes a range read for comparison across the two clients.
func digest(keys, values [][]byte) string {
	h := sha256.New()
	for i := range keys {
		fmt.Fprintf(h, "%d:%s=%d:%s;", len(keys[i]), keys[i], len(values[i]), values[i])
	}
	return fmt.Sprintf("%d rows %s", len(keys), hex.EncodeToString(h.Sum(nil))[:16])
}

// want is the result each read kind must return for key k.
func (f crFixture) want(kind string, k int) string {
	switch kind {
	case "get":
		return string(f.value(k))
	case "getkey":
		return string(f.key(k))
	default:
		var keys, values [][]byte
		for i := range f.n {
			keys, values = append(keys, f.key(i)), append(values, f.value(i))
		}
		return digest(keys, values)
	}
}

func cRead(tr cgofdb.ReadTransaction, f crFixture, kind string, k int) (string, error) {
	switch kind {
	case "get":
		v, err := tr.Get(cgofdb.Key(f.key(k))).Get()
		return string(v), err
	case "getkey":
		key, err := tr.GetKey(cgofdb.FirstGreaterOrEqual(cgofdb.Key(f.key(k)))).Get()
		return string(key), err
	default:
		kvs, err := tr.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(f.prefix), End: cgofdb.Key(f.rangeEnd())}, cgofdb.RangeOptions{}).GetSliceWithError()
		var keys, values [][]byte
		for _, kv := range kvs {
			keys, values = append(keys, kv.Key), append(values, kv.Value)
		}
		return digest(keys, values), err
	}
}

func goRead(tr gofdb.ReadTransaction, f crFixture, kind string, k int) (string, error) {
	switch kind {
	case "get":
		v, err := tr.Get(gofdb.Key(f.key(k))).Get()
		return string(v), err
	case "getkey":
		key, err := tr.GetKey(gofdb.FirstGreaterOrEqual(gofdb.Key(f.key(k)))).Get()
		return string(key), err
	default:
		kvs, err := tr.GetRange(gofdb.KeyRange{Begin: gofdb.Key(f.prefix), End: gofdb.Key(f.rangeEnd())}, gofdb.RangeOptions{}).GetSliceWithError()
		var keys, values [][]byte
		for _, kv := range kvs {
			keys, values = append(keys, kv.Key), append(values, kv.Value)
		}
		return digest(keys, values), err
	}
}

// fdbCode reports the FDB error code err carries on either client.
func fdbCode(err error) (int, bool) {
	var ce cgofdb.Error
	if errors.As(err, &ce) {
		return ce.Code, true
	}
	var ge gofdb.Error
	if errors.As(err, &ge) {
		return ge.Code, true
	}
	return 0, false
}

// requireSurfacedRetryable fails on a non-FDB error or a code OnError does not retry.
func requireSurfacedRetryable(t *testing.T, who string, errs []error) {
	t.Helper()
	for _, err := range errs {
		code, ok := fdbCode(err)
		if !ok {
			t.Errorf("%s: non-FDB error reached the application: %v", who, err)
			continue
		}
		if !gofdb.IsOnErrorRetryable(code) {
			t.Errorf("%s: surfaced FDB error %d is not retried by OnError: %v", who, code, err)
		}
	}
}

func startCRCluster(t *testing.T, ctx context.Context, f *crFixture, opts ...testcontainers.ContainerCustomizer) {
	t.Helper()
	startup, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(startup, "", append([]testcontainers.ContainerCustomizer{foundationdbtc.WithDirectIP()}, opts...)...)
	if err != nil {
		if foundationdbtc.SkipWithoutDocker(err) {
			t.Skipf("FDB not available (no Docker): %v", err)
		}
		t.Fatalf("multi-process cluster startup: %v", err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		foundationdbtc.TerminateT(t, stop, container)
	})
	content, err := container.ClusterFile(startup)
	if err != nil {
		t.Fatal(err)
	}
	f.clusterFile = filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(f.clusterFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f.container = container
	// libfdbc.Open selects API 730 on both clients and starts the C network.
	backend, err := libfdbc.Open(f.clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backend.Close)
	db, err := gofdb.OpenDatabase(f.clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for lo := 0; lo < f.n; lo += 10 {
		if _, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
			for i := lo; i < min(lo+10, f.n); i++ {
				tr.Set(gofdb.Key(f.key(i)), f.value(i))
			}
			return nil, nil
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

// storageAddresses returns the addresses of key's replicas, through a fresh
// pure-Go handle so no cached location is consulted.
func storageAddresses(t *testing.T, f crFixture, key []byte) []string {
	t.Helper()
	db, err := gofdb.OpenDatabase(f.clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	addrs, err := tr.LocalityGetAddressesForKey(gofdb.Key(key)).Get()
	if err != nil {
		t.Fatalf("addresses for %q: %v", key, err)
	}
	return addrs
}

// pickVictim returns a seeded key and the address of one of its replicas that
// is a storage-class process (never the primary, which hosts the coordinator),
// waiting for data distribution when needed. minReplicas is the replica count
// the key's team must have.
func pickVictim(t *testing.T, ctx context.Context, f crFixture, minReplicas int) (int, string) {
	t.Helper()
	primary := ":" + strconv.Itoa(f.container.FDBPort())
	deadline := time.Now().Add(2 * time.Minute)
	for {
		for i := range f.n {
			addrs := storageAddresses(t, f, f.key(i))
			if len(addrs) < minReplicas {
				continue
			}
			for _, a := range addrs {
				if !strings.HasSuffix(a, primary) {
					return i, a
				}
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatalf("no seeded key has a replica on a storage-class process")
		}
		time.Sleep(2 * time.Second)
	}
}

func containerExec(ctx context.Context, c *foundationdbtc.Container, cmd string) (string, error) {
	code, r, err := c.Exec(ctx, []string{"bash", "-c", cmd}, tcexec.Multiplexed())
	if err != nil {
		return "", err
	}
	out, _ := io.ReadAll(r)
	if code != 0 {
		return string(out), fmt.Errorf("%q exited %d: %s", cmd, code, out)
	}
	return string(out), nil
}

// killStorage kills the fdbserver listening on addr's port and returns the
// command that restarts it. It waits until the host's connects are refused.
func killStorage(t *testing.T, ctx context.Context, f crFixture, addr string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	// Match the fdbserver itself (-x: process name), not the bash subshell
	// that started it in the background, whose command line also carries
	// the listen address.
	pid, err := containerExec(ctx, f.container, fmt.Sprintf(
		`for p in $(pgrep -x fdbserver); do tr '\0' ' ' < /proc/$p/cmdline | grep -qE -- '--listen-address 0\.0\.0\.0:%s( |$)' && echo $p; done; true`, port))
	pid = strings.TrimSpace(pid)
	if err != nil || pid == "" || strings.Contains(pid, "\n") {
		t.Fatalf("find the fdbserver on port %s: %q %v", port, pid, err)
	}
	args, err := containerExec(ctx, f.container, fmt.Sprintf(`tr '\0' ' ' < /proc/%s/cmdline`, pid))
	if err != nil || !strings.Contains(args, "fdbserver") {
		t.Fatalf("command line of %s: %q %v", pid, args, err)
	}
	if _, err := containerExec(ctx, f.container, "kill -9 "+pid); err != nil {
		t.Fatal(err)
	}
	// Positive control: the fault is a refused connect, not a hang.
	for deadline := time.Now().Add(15 * time.Second); ; {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
		} else if errors.Is(err, syscall.ECONNREFUSED) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("connects to %s were not refused after the kill: %v", addr, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "nohup " + strings.TrimSpace(args) + " >/dev/null 2>&1 &"
}

// A read whose shard has a refusing replica and a live one completes from the
// live one, with no error, on both clients.
func TestLibFDBC_ConnectRefusedReplicaUsesAnotherReplica(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	f := crFixture{prefix: []byte(t.Name() + "/"), n: 20, valueSize: 64}
	startCRCluster(t, ctx, &f, foundationdbtc.WithProcessCount(4), foundationdbtc.WithRedundancyMode("double"))
	k, victim := pickVictim(t, ctx, f, 2)
	killStorage(t, ctx, f, victim)

	cdb, err := cgofdb.OpenDatabase(f.clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	const rounds = 5
	var goFailures int64
	for round := range rounds {
		// A fresh pure-Go handle per round has no failure-monitor memory, so it
		// dials the refusing replica again rather than excluding it.
		godb, err := gofdb.OpenDatabase(f.clusterFile)
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range readKinds {
			gtr, err := godb.CreateTransaction()
			if err != nil {
				t.Fatal(err)
			}
			if err := gtr.Options().SetTimeout(30000); err != nil {
				t.Fatal(err)
			}
			goGot, goErr := goRead(gtr, f, kind, k)
			ctr, err := cdb.CreateTransaction()
			if err != nil {
				t.Fatal(err)
			}
			if err := ctr.Options().SetTimeout(30000); err != nil {
				t.Fatal(err)
			}
			cGot, cErr := cRead(ctr, f, kind, k)
			goCode, _ := fdbCode(goErr)
			cCode, _ := fdbCode(cErr)
			if goErr != nil || cErr != nil {
				t.Errorf("round %d %s: a live replica exists, so no error may surface: pure-Go %v (code %d), libfdb_c %v (code %d)", round, kind, goErr, goCode, cErr, cCode)
				continue
			}
			if want := f.want(kind, k); goGot != want || cGot != want {
				t.Errorf("round %d %s: pure-Go %q, libfdb_c %q, want %q", round, kind, goGot, cGot, want)
			}
		}
		goFailures += godb.Metrics().ClientConnectionFailures
		godb.Close()
	}
	if goFailures == 0 {
		t.Fatal("positive control: the pure-Go client never dialed the refusing replica")
	}

	// A watch on the key fires through the live replica on both clients.
	setter, err := gofdb.OpenDatabase(f.clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	defer setter.Close()
	for _, client := range []string{"pure-Go", "libfdb_c"} {
		var fired func() error
		switch client {
		case "pure-Go":
			godb, err := gofdb.OpenDatabase(f.clusterFile)
			if err != nil {
				t.Fatal(err)
			}
			defer godb.Close()
			tr, err := godb.CreateTransaction()
			if err != nil {
				t.Fatal(err)
			}
			_ = tr.Options().SetTimeout(60000)
			w := tr.Watch(gofdb.Key(f.key(k)))
			if err := tr.Commit().Get(); err != nil {
				t.Fatalf("%s: commit the watch: %v", client, err)
			}
			fired = w.Get
		default:
			tr, err := cdb.CreateTransaction()
			if err != nil {
				t.Fatal(err)
			}
			_ = tr.Options().SetTimeout(60000)
			w := tr.Watch(cgofdb.Key(f.key(k)))
			if err := tr.Commit().Get(); err != nil {
				t.Fatalf("%s: commit the watch: %v", client, err)
			}
			fired = w.Get
		}
		if _, err := setter.Transact(func(tr gofdb.WritableTransaction) (any, error) {
			tr.Set(gofdb.Key(f.key(k)), []byte("changed by "+client))
			return nil, nil
		}); err != nil {
			t.Fatalf("change the watched key: %v", err)
		}
		if err := fired(); err != nil {
			code, _ := fdbCode(err)
			t.Errorf("%s: watch with a refusing replica: %v (code %d)", client, err, code)
		}
	}
}

// A read whose only replica refuses connections waits, on both clients, until the
// replica is back, then completes; nothing but OnError-retryable FDB codes reaches
// the transaction, and the outcome is the same on both clients.
func TestLibFDBC_ConnectRefusedOnlyReplicaWaitsForIt(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	// Small shards spread the seeded keys over every storage process
	// (range_shard_pair_differential_test.go uses the same knobs).
	f := crFixture{prefix: []byte(t.Name() + "/"), n: 100, valueSize: 10000}
	startCRCluster(t, ctx, &f,
		foundationdbtc.WithProcessCount(3), foundationdbtc.WithRedundancyMode("single"),
		foundationdbtc.WithStorageEngine("ssd"),
		foundationdbtc.WithKnob("min_shard_bytes", "10000"),
		foundationdbtc.WithKnob("max_shard_bytes", "50000"),
		foundationdbtc.WithKnob("shard_bytes_ratio", "2"),
		foundationdbtc.WithKnob("storage_metrics_polling_delay", "1"))
	k, victim := pickVictim(t, ctx, f, 1)
	restart := killStorage(t, ctx, f, victim)

	cdb, err := cgofdb.OpenDatabase(f.clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	godb, err := gofdb.OpenDatabase(f.clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	defer godb.Close()

	type outcome struct {
		client, kind string
		got          string
		err          error
		surfaced     []error
	}
	var mu sync.Mutex
	var finished int
	outcomes := make(chan outcome, 2*len(readKinds))
	run := func(client, kind string, loop func(record func(error)) (any, error)) {
		var surfaced []error
		got, err := loop(func(e error) {
			if e != nil {
				surfaced = append(surfaced, e)
			}
		})
		mu.Lock()
		finished++
		mu.Unlock()
		s, _ := got.(string)
		outcomes <- outcome{client, kind, s, err, surfaced}
	}
	for _, kind := range readKinds {
		go run("pure-Go", kind, func(record func(error)) (any, error) {
			return godb.ReadTransact(func(tr gofdb.ReadTransaction) (any, error) {
				_ = tr.Options().SetTimeout(120000)
				got, err := goRead(tr, f, kind, k)
				record(err)
				return got, err
			})
		})
		go run("libfdb_c", kind, func(record func(error)) (any, error) {
			return cdb.ReadTransact(func(tr cgofdb.ReadTransaction) (any, error) {
				_ = tr.Options().SetTimeout(120000)
				got, err := cRead(tr, f, kind, k)
				record(err)
				return got, err
			})
		})
	}

	// While the replica refuses, every read is still waiting on both clients.
	time.Sleep(3 * time.Second)
	mu.Lock()
	early := finished
	mu.Unlock()
	if early != 0 {
		t.Errorf("%d reads finished while their only replica refused connections; libfdb_c waits for it", early)
	}
	if _, err := containerExec(ctx, f.container, restart); err != nil {
		t.Fatalf("restart %s: %v", victim, err)
	}

	results := map[string]map[string]outcome{"pure-Go": {}, "libfdb_c": {}}
	for range 2 * len(readKinds) {
		select {
		case o := <-outcomes:
			results[o.client][o.kind] = o
		case <-ctx.Done():
			t.Fatalf("reads did not complete after %s came back", victim)
		}
	}
	for _, kind := range readKinds {
		goOut, cOut := results["pure-Go"][kind], results["libfdb_c"][kind]
		for _, o := range []outcome{goOut, cOut} {
			requireSurfacedRetryable(t, o.client+" "+kind, o.surfaced)
			if o.err != nil {
				t.Errorf("%s %s: the transaction failed: %v", o.client, kind, o.err)
			}
			if want := f.want(kind, k); o.err == nil && o.got != want {
				t.Errorf("%s %s: got %.40q, want %.40q", o.client, kind, o.got, want)
			}
		}
		t.Logf("%s: surfaced to the retry loop: pure-Go %v, libfdb_c %v", kind, goOut.surfaced, cOut.surfaced)
	}
	if godb.Metrics().ClientConnectionFailures == 0 {
		t.Fatal("positive control: the pure-Go client never dialed the refusing replica")
	}
}

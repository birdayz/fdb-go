//go:build cgo && libfdbc

package libfdbc_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/libfdbc"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// Each read runs once in its own process; endpoint-specific client traces prove
// it encountered the killed storage server before that same read is recovered.

// readKinds are the storage reads the record layer issues.
var readKinds = []string{"get", "getkey", "getrange"}

// ServerKnobs.cpp defaults to five seconds; process recovery must fit inside
// this fixture's MVCC window as well as the unchanged 30-second client timeout.
const crReadLifeVersions = "60000000"

// crFixture is the seeded keyspace and the cluster file both clients open.
type crFixture struct {
	clusterFile string
	container   *foundationdbtc.Container
	prefix      []byte
	n           int
	valueSize   int
	double      bool
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

func startCRCluster(t *testing.T, ctx context.Context, f *crFixture, opts ...testcontainers.ContainerCustomizer) {
	t.Helper()
	startup, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	opts = append(opts, foundationdbtc.WithKnob("max_read_transaction_life_versions", crReadLifeVersions))
	if f.double {
		opts = append(opts, foundationdbtc.WithoutInit())
	}
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
	if f.double {
		bootstrapCRDouble(t, startup, container)
	}
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

// Dedicated logs must exist before initial recruitment: storage-class processes
// otherwise take log roles and cannot safely form the two-process fault team.
func bootstrapCRDouble(t *testing.T, ctx context.Context, container *foundationdbtc.Container) {
	t.Helper()
	host, _, err := net.SplitHostPort(container.InternalAddress())
	if err != nil {
		t.Fatal(err)
	}
	for i, class := range []string{"log", "log", "log", "storage", "storage"} {
		port := container.FDBPort() + i + 1
		dir := fmt.Sprintf("/var/fdb/data/connrefused%d", i+1)
		// Double requires distinct zones (ManagementAPI.actor.cpp:309-313).
		// Defaults share one host-local ID, forcing every team to include the primary.
		cmd := fmt.Sprintf(
			"mkdir -p %s && fdbserver --listen-address 0.0.0.0:%d --public-address %s "+
				"--cluster-file /var/fdb/fdb.cluster --datadir %s --logdir /var/fdb/logs "+
				"--class %s --locality-zoneid=connrefused-%d --locality-machineid=connrefused-%d "+
				"--knob_disable_posix_kernel_aio=1 --knob_max_read_transaction_life_versions=%s "+
				">/var/fdb/logs/connrefused%d.out 2>&1 &",
			dir, port, net.JoinHostPort(host, strconv.Itoa(port)), dir, class, port, port, crReadLifeVersions, i+1)
		if _, err := containerExec(ctx, container, cmd); err != nil {
			t.Fatalf("start dedicated %s process on port %d: %v", class, port, err)
		}
		ready, cancel := context.WithTimeout(ctx, 15*time.Second)
		ticker := time.NewTicker(20 * time.Millisecond)
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		for {
			conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ready, "tcp", addr)
			if err == nil {
				conn.Close()
				break
			}
			select {
			case <-ready.Done():
				ticker.Stop()
				cancel()
				t.Fatalf("dedicated %s process did not listen on %s before configure-new: %v", class, addr, err)
			case <-ticker.C:
			}
		}
		ticker.Stop()
		cancel()
	}
	if err := container.InitializeDatabase(ctx); err != nil {
		t.Fatalf("initialize double cluster with dedicated log and storage processes: %v", err)
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

// pickStorageTeam requires the whole team to be non-primary storage processes;
// killing it must not stop the coordinator, GRV proxy or location proxy.
func pickStorageTeam(t *testing.T, ctx context.Context, f crFixture, replicas int) (int, []string) {
	t.Helper()
	primary := ":" + strconv.Itoa(f.container.FDBPort())
	deadline := time.Now().Add(2 * time.Minute)
	for {
		status, diagnostic, err := crStorageStatus(ctx, f.container)
		if err != nil {
			t.Fatalf("check storage process roles: %v\nstdout:\n%s\nstderr:\n%s", err, status, diagnostic)
		}
		if diagnostic != "" {
			t.Logf("storage role status stderr:\n%s", diagnostic)
		}
		safe, err := crStorageOnlyAddresses([]byte(status))
		if err != nil {
			t.Fatalf("%v\nstdout:\n%s\nstderr:\n%s", err, status, diagnostic)
		}
		for i := range f.n {
			addrs := storageAddresses(t, f, f.key(i))
			if len(addrs) != replicas {
				continue
			}
			storageOnly := true
			for _, a := range addrs {
				if strings.HasSuffix(a, primary) || !safe[a] {
					storageOnly = false
				}
			}
			if storageOnly {
				return i, addrs
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatalf("no seeded key has a team of exactly %d non-primary storage processes; status:\n%s", replicas, status)
		}
		time.Sleep(2 * time.Second)
	}
}

// fdbcli prints JSON to stdout and warnings to stderr (StatusCommand.actor.cpp:212,1302-1307).
// Keep them separate: exec.Multiplexed appends stderr after stdout, invalidating strict JSON.
func crStorageStatus(ctx context.Context, c *foundationdbtc.Container) (string, string, error) {
	argv := []string{"/usr/bin/fdbcli", "--exec", "status json"}
	if deadline, ok := ctx.Deadline(); ok {
		seconds := max(1, int(math.Ceil(time.Until(deadline).Seconds())))
		argv = append([]string{"timeout", "-s", "KILL", strconv.Itoa(seconds)}, argv...)
	}
	type result struct {
		stdout, stderr string
		err            error
	}
	done := make(chan result, 1)
	go func() {
		code, reader, err := c.Exec(ctx, argv)
		var stdout, stderr string
		if err == nil {
			stdout, stderr, err = crStatusStreams(reader)
		}
		if err == nil && code != 0 {
			err = fmt.Errorf("fdbcli status json exited %d", code)
		}
		done <- result{stdout, stderr, err}
	}()
	select {
	case r := <-done:
		return r.stdout, r.stderr, r.err
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

func crStatusStreams(reader io.Reader) (string, string, error) {
	var stdout, stderr bytes.Buffer
	_, err := stdcopy.StdCopy(&stdout, &stderr, reader)
	return stdout.String(), stderr.String(), err
}

func crStorageOnlyAddresses(raw []byte) (map[string]bool, error) {
	var status struct {
		Cluster struct {
			Processes map[string]struct {
				Address string `json:"address"`
				Roles   []struct {
					Role string `json:"role"`
				} `json:"roles"`
			} `json:"processes"`
		} `json:"cluster"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return nil, fmt.Errorf("decode storage roles: %w", err)
	}
	if len(status.Cluster.Processes) == 0 {
		return nil, fmt.Errorf("status has no process population")
	}
	safe := make(map[string]bool)
	for _, process := range status.Cluster.Processes {
		storageOnly := len(process.Roles) > 0
		for _, role := range process.Roles {
			if role.Role != "storage" {
				storageOnly = false
			}
		}
		if storageOnly {
			if process.Address == "" {
				return nil, fmt.Errorf("storage-only process has no address")
			}
			safe[process.Address] = true
		}
	}
	return safe, nil
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

func requireCRRefused(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if conn != nil {
		conn.Close()
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("storage peer %s is no longer refusing connections: %v", addr, err)
	}
}

const crWorkerEnv = "FDBGO_CONNECT_REFUSED_WORKER"

type crWorkerConfig struct {
	Client, Kind, ClusterFile, Prefix, Dir string
	N, ValueSize, Key                      int
}

type crReadResult struct {
	Got, Error                     string
	Code                           int
	FDBError                       bool
	ReadVersion                    int64
	ReadyAt, IssuedAt, CompletedAt time.Time
}

// Native network configuration is process-wide, so each operation gets a fresh
// process with tracing enabled before either client opens a database.
func TestLibFDBC_ConnectRefusedWorker(t *testing.T) {
	raw := os.Getenv(crWorkerEnv)
	if raw == "" {
		return
	}
	var cfg crWorkerConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	f := crFixture{clusterFile: cfg.ClusterFile, prefix: []byte(cfg.Prefix), n: cfg.N, valueSize: cfg.ValueSize}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	var result crReadResult
	issue := func() {
		result.ReadyAt = time.Now()
		crWriteFile(t, filepath.Join(cfg.Dir, "ready"), []byte(result.ReadyAt.Format(time.RFC3339Nano)))
		crWaitFile(t, ctx, filepath.Join(cfg.Dir, "issue"))
		result.IssuedAt = time.Now()
		crWriteFile(t, filepath.Join(cfg.Dir, "issued"), nil)
	}
	var got string
	var readErr error
	switch cfg.Client {
	case "pure-Go":
		log, err := os.Create(filepath.Join(cfg.Dir, "trace", "go.json"))
		if err != nil {
			t.Fatal(err)
		}
		defer log.Close()
		slog.SetDefault(slog.New(slog.NewJSONHandler(log, nil)))
		gofdb.MustAPIVersion(730)
		db, err := gofdb.OpenDatabase(cfg.ClusterFile)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		tr, err := db.CreateTransaction()
		if err != nil {
			t.Fatal(err)
		}
		defer tr.Cancel()
		if err := tr.Options().SetTimeout(30000); err != nil {
			t.Fatal(err)
		}
		version, err := tr.GetReadVersion().Get()
		if err != nil {
			t.Fatal(err)
		}
		result.ReadVersion = version
		issue()
		got, readErr = goRead(tr, f, cfg.Kind, cfg.Key)
	case "libfdb_c":
		cgofdb.MustAPIVersion(730)
		if err := cgofdb.Options().SetTraceEnable(filepath.Join(cfg.Dir, "trace")); err != nil {
			t.Fatal(err)
		}
		if err := cgofdb.Options().SetTraceFormat("json"); err != nil {
			t.Fatal(err)
		}
		db, err := cgofdb.OpenDatabase(cfg.ClusterFile)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		tr, err := db.CreateTransaction()
		if err != nil {
			t.Fatal(err)
		}
		defer tr.Cancel()
		if err := tr.Options().SetTimeout(30000); err != nil {
			t.Fatal(err)
		}
		version, err := tr.GetReadVersion().Get()
		if err != nil {
			t.Fatal(err)
		}
		result.ReadVersion = version
		issue()
		got, readErr = cRead(tr, f, cfg.Kind, cfg.Key)
	default:
		t.Fatalf("unknown client %q", cfg.Client)
	}
	result.Got = got
	result.CompletedAt = time.Now()
	if readErr != nil {
		result.Error = readErr.Error()
		result.Code, result.FDBError = fdbCode(readErr)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	crWriteFile(t, filepath.Join(cfg.Dir, "result"), data)
}

func crWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func crWaitFile(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v", path, ctx.Err())
		case <-ticker.C:
		}
	}
}

type crWorker struct {
	cfg    crWorkerConfig
	done   chan struct{}
	output []byte
	err    error
}

func startCRWorker(t *testing.T, ctx context.Context, f crFixture, client, kind string, key int) *crWorker {
	t.Helper()
	w := &crWorker{cfg: crWorkerConfig{
		Client: client, Kind: kind, ClusterFile: f.clusterFile,
		Prefix: string(f.prefix), Dir: t.TempDir(), N: f.n, ValueSize: f.valueSize, Key: key,
	}, done: make(chan struct{})}
	if err := os.Mkdir(filepath.Join(w.cfg.Dir, "trace"), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(w.cfg)
	if err != nil {
		t.Fatal(err)
	}
	childCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestLibFDBC_ConnectRefusedWorker$", "-test.v")
	cmd.Env = append(os.Environ(), crWorkerEnv+"="+string(data))
	go func() {
		w.output, w.err = cmd.CombinedOutput()
		close(w.done)
	}()
	t.Cleanup(func() { cancel(); <-w.done })
	ready, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(filepath.Join(w.cfg.Dir, "ready")); err == nil {
			t.Logf("%s %s: GRV ready at %s", client, kind, data)
			return w
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-w.done:
			t.Fatalf("%s %s: worker exited before READY: %v\n%s", client, kind, w.err, w.output)
		case <-ready.Done():
			t.Fatalf("%s %s: worker did not prepare a read version: %v", client, kind, ready.Err())
		case <-ticker.C:
		}
	}
}

// A new process has no storage traffic before ISSUE. Match the failed endpoint,
// not a process-wide counter; ignore the final line until its write is complete.
func crRefusedPeer(data []byte, client string, peers []string) (string, error) {
	if client != "pure-Go" && client != "libfdb_c" {
		return "", fmt.Errorf("unknown trace client %q", client)
	}
	lines := bytes.Split(data, []byte{'\n'})
	for _, line := range lines[:len(lines)-1] {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct {
			Type, PeerAddr string
			ErrorCode      json.RawMessage
			Message        string `json:"msg"`
			Address        string `json:"address"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return "", fmt.Errorf("decode client trace: %w", err)
		}
		var addr string
		switch client {
		case "pure-Go":
			if event.Message == "fdbgo: connection to server failed" {
				addr = event.Address
			}
		case "libfdb_c":
			code := string(bytes.TrimSpace(event.ErrorCode))
			if (event.Type == "ConnectionClosed" || event.Type == "IncompatibleConnectionClosed") &&
				(code == "1026" || code == `"1026"`) {
				addr = event.PeerAddr
			}
		}
		for _, peer := range peers {
			if addr != "" && addr == peer {
				return addr, nil
			}
		}
	}
	return "", nil
}

func TestConnectRefusedTraceGate(t *testing.T) {
	t.Parallel()
	const peer = "127.0.0.1:4501"
	for _, tc := range []struct {
		name, client, trace, want string
		peers                     []string
		wantError                 bool
	}{
		{name: "empty", client: "libfdb_c", peers: []string{peer}},
		{name: "blank", client: "libfdb_c", trace: "\n", peers: []string{peer}},
		{name: "partial", client: "libfdb_c", trace: `{`, peers: []string{peer}},
		{name: "malformed", client: "libfdb_c", trace: "{\n", peers: []string{peer}, wantError: true},
		{name: "unknown_client", client: "unknown", wantError: true},
		{name: "no_peers", client: "libfdb_c", trace: `{"Type":"ConnectionClosed","PeerAddr":"127.0.0.1:4501","ErrorCode":"1026"}` + "\n"},
		{name: "wrong_peer", client: "libfdb_c", trace: `{"Type":"ConnectionClosed","PeerAddr":"127.0.0.1:4502","ErrorCode":"1026"}` + "\n", peers: []string{peer}},
		{name: "connect_not_failure", client: "libfdb_c", trace: `{"Type":"ConnectingTo","PeerAddr":"127.0.0.1:4501","ErrorCode":"1026"}` + "\n", peers: []string{peer}},
		{name: "wrong_error", client: "libfdb_c", trace: `{"Type":"ConnectionClosed","PeerAddr":"127.0.0.1:4501","ErrorCode":"1025"}` + "\n", peers: []string{peer}},
		{name: "missing_error", client: "libfdb_c", trace: `{"Type":"ConnectionClosed","PeerAddr":"127.0.0.1:4501"}` + "\n", peers: []string{peer}},
		{name: "native", client: "libfdb_c", trace: `{"Type":"ConnectionClosed","PeerAddr":"127.0.0.1:4501","ErrorCode":"1026"}` + "\n", peers: []string{peer}, want: peer},
		{name: "numeric_error", client: "libfdb_c", trace: `{"Type":"ConnectionClosed","PeerAddr":"127.0.0.1:4501","ErrorCode":1026}` + "\n", peers: []string{peer}, want: peer},
		{name: "incompatible", client: "libfdb_c", trace: `{"Type":"IncompatibleConnectionClosed","PeerAddr":"127.0.0.1:4501","ErrorCode":"1026"}` + "\n", peers: []string{"127.0.0.1:4502", peer}, want: peer},
		{name: "go", client: "pure-Go", trace: `{"msg":"fdbgo: connection to server failed","address":"127.0.0.1:4501"}` + "\n", peers: []string{peer}, want: peer},
		{name: "go_wrong_message", client: "pure-Go", trace: `{"msg":"connected","address":"127.0.0.1:4501"}` + "\n", peers: []string{peer}},
		{name: "go_wrong_peer", client: "pure-Go", trace: `{"msg":"fdbgo: connection to server failed","address":"127.0.0.1:4502"}` + "\n", peers: []string{peer}},
		{name: "go_ignores_native", client: "pure-Go", trace: `{"Type":"ConnectionClosed","PeerAddr":"127.0.0.1:4501","ErrorCode":"1026"}` + "\n", peers: []string{peer}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := crRefusedPeer([]byte(tc.trace), tc.client, tc.peers)
			if (err != nil) != tc.wantError || got != tc.want {
				t.Fatalf("peer=%q err=%v, want peer=%q error=%t", got, err, tc.want, tc.wantError)
			}
		})
	}
}

func TestConnectRefusedStorageStatusStreams(t *testing.T) {
	t.Parallel()
	const valid = `{"cluster":{"processes":{"p":{"address":"a","roles":[{"role":"storage"}]}}}}`
	for _, tc := range []struct {
		name, stdout, stderr string
		wantError            bool
	}{
		{name: "json", stdout: valid},
		{name: "warning", stdout: valid, stderr: "WARNING: Long delay (Ctrl-C to interrupt)\n"},
		{name: "stdout_warning", stdout: valid + "\nWARNING: unexpected stdout", wantError: true},
		{name: "second_json", stdout: valid + "\n{}", wantError: true},
		{name: "truncated_json", stdout: valid[:len(valid)-1], stderr: "WARNING: Long delay\n", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw bytes.Buffer
			for _, stream := range []struct {
				kind stdcopy.StdType
				text string
			}{{stdcopy.Stdout, tc.stdout}, {stdcopy.Stderr, tc.stderr}} {
				if _, err := io.WriteString(stdcopy.NewStdWriter(&raw, stream.kind), stream.text); err != nil {
					t.Fatal(err)
				}
			}
			stdout, stderr, err := crStatusStreams(&raw)
			if err != nil || stdout != tc.stdout || stderr != tc.stderr {
				t.Fatalf("decoded streams stdout=%q stderr=%q err=%v; want stdout=%q stderr=%q", stdout, stderr, err, tc.stdout, tc.stderr)
			}
			safe, err := crStorageOnlyAddresses([]byte(stdout))
			if (err != nil) != tc.wantError || err == nil && !safe["a"] {
				t.Fatalf("safe=%v err=%v, want error=%t and strict storage-only address a", safe, err, tc.wantError)
			}
		})
	}
}

func TestConnectRefusedStorageTopology(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, status string
		want         []string
		wantError    bool
	}{
		{name: "malformed", status: `{`, wantError: true},
		{name: "no_processes", status: `{}`, wantError: true},
		{name: "no_roles", status: `{"cluster":{"processes":{"p":{"address":"a"}}}}`},
		{name: "unknown_role", status: `{"cluster":{"processes":{"p":{"address":"a","roles":[{}]}}}}`},
		{name: "missing_address", status: `{"cluster":{"processes":{"p":{"roles":[{"role":"storage"}]}}}}`, wantError: true},
		{name: "mixed_roles", status: `{"cluster":{"processes":{"p":{"address":"a","roles":[{"role":"storage"},{"role":"grv_proxy"}]}}}}`},
		{name: "storage_only", status: `{"cluster":{"processes":{"p":{"address":"a","roles":[{"role":"storage"},{"role":"storage"}]},"q":{"address":"b","roles":[{"role":"storage"}]}}}}`, want: []string{"a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := crStorageOnlyAddresses([]byte(tc.status))
			if (err != nil) != tc.wantError || len(got) != len(tc.want) {
				t.Fatalf("safe=%v err=%v, want safe=%v error=%t", got, err, tc.want, tc.wantError)
			}
			for _, addr := range tc.want {
				if !got[addr] {
					t.Fatalf("safe=%v is missing %s", got, addr)
				}
			}
		})
	}
}

func (w *crWorker) requirePending(t *testing.T) {
	t.Helper()
	if data, err := os.ReadFile(filepath.Join(w.cfg.Dir, "result")); err == nil {
		t.Fatalf("%s %s: raw read completed before recovery: %s", w.cfg.Client, w.cfg.Kind, data)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	select {
	case <-w.done:
		t.Fatalf("%s %s: worker exited before recovery: %v\n%s", w.cfg.Client, w.cfg.Kind, w.err, w.output)
	default:
	}
}

func (w *crWorker) issueAndAwaitRefusal(t *testing.T, ctx context.Context, peers []string) string {
	t.Helper()
	t.Logf("%s %s: issue at %s", w.cfg.Client, w.cfg.Kind, time.Now().Format(time.RFC3339Nano))
	crWriteFile(t, filepath.Join(w.cfg.Dir, "issue"), nil)
	gate, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	crWaitFile(t, gate, filepath.Join(w.cfg.Dir, "issued"))
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		w.requirePending(t)
		var traceDump bytes.Buffer
		files, err := os.ReadDir(filepath.Join(w.cfg.Dir, "trace"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if file.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(w.cfg.Dir, "trace", file.Name()))
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&traceDump, "\n--- %s ---\n%s", file.Name(), data)
			peer, err := crRefusedPeer(data, w.cfg.Client, peers)
			if err != nil {
				t.Fatal(err)
			}
			if peer != "" {
				w.requirePending(t)
				t.Logf("%s %s: raw read encountered refusing storage peer %s at %s", w.cfg.Client, w.cfg.Kind, peer, time.Now().Format(time.RFC3339Nano))
				return peer
			}
		}
		select {
		case <-gate.Done():
			t.Fatalf("%s %s: no per-operation refusal trace for %v before recovery: %v\n%s", w.cfg.Client, w.cfg.Kind, peers, gate.Err(), traceDump.String())
		case <-ticker.C:
		}
	}
}

func (w *crWorker) requireResult(t *testing.T, ctx context.Context, want string) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatalf("%s %s: raw read did not recover: %v", w.cfg.Client, w.cfg.Kind, ctx.Err())
	case <-w.done:
	}
	if w.err != nil {
		t.Fatalf("%s %s: worker failed: %v\n%s", w.cfg.Client, w.cfg.Kind, w.err, w.output)
	}
	data, err := os.ReadFile(filepath.Join(w.cfg.Dir, "result"))
	if err != nil {
		t.Fatal(err)
	}
	var result crReadResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s %s: read version=%d ready=%s issued=%s completed=%s GRV age=%s", w.cfg.Client, w.cfg.Kind,
		result.ReadVersion, result.ReadyAt.Format(time.RFC3339Nano), result.IssuedAt.Format(time.RFC3339Nano),
		result.CompletedAt.Format(time.RFC3339Nano), result.CompletedAt.Sub(result.ReadyAt))
	if result.Error != "" || result.Got != want {
		t.Fatalf("%s %s: same raw read returned %.80q, error=%q FDB=%t code=%d; want %.80q without an application retry",
			w.cfg.Client, w.cfg.Kind, result.Got, result.Error, result.FDBError, result.Code, want)
	}
}

// Both replicas start down. Once this read's trace identifies a refused peer,
// only the other replica returns; the same future must recover through it.
func TestLibFDBC_ConnectRefusedReplicaUsesAnotherReplica(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	f := crFixture{prefix: []byte(t.Name() + "/"), n: 20, valueSize: 64, double: true}
	startCRCluster(t, ctx, &f, foundationdbtc.WithRedundancyMode("double"))
	for _, client := range []string{"pure-Go", "libfdb_c"} {
		for _, kind := range readKinds {
			k, team := pickStorageTeam(t, ctx, f, 2)
			worker := startCRWorker(t, ctx, f, client, kind, k)
			if client == "libfdb_c" && kind == "get" {
				// Exercise recovery of the original GRV beyond the default five-second
				// MVCC window, without refreshing the version or retrying the read.
				select {
				case <-time.After(6 * time.Second):
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			restart := make(map[string]string, len(team))
			for _, peer := range team {
				restart[peer] = killStorage(t, ctx, f, peer)
			}
			victim := worker.issueAndAwaitRefusal(t, ctx, team)
			for _, peer := range team {
				if peer != victim {
					t.Logf("%s %s: restart alternate %s at %s", client, kind, peer, time.Now().Format(time.RFC3339Nano))
					if _, err := containerExec(ctx, f.container, restart[peer]); err != nil {
						t.Fatalf("restart alternate %s: %v", peer, err)
					}
				}
			}
			worker.requireResult(t, ctx, f.want(kind, k))
			requireCRRefused(t, victim)
			if _, err := containerExec(ctx, f.container, restart[victim]); err != nil {
				t.Fatalf("restore victim %s after the read: %v", victim, err)
			}
		}
	}

	// This watch check asserts availability, not which replica the watch selected.
	k, team := pickStorageTeam(t, ctx, f, 2)
	killStorage(t, ctx, f, team[0])
	cdb, err := cgofdb.OpenDatabase(f.clusterFile)
	if err != nil {
		t.Fatal(err)
	}
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

// Each single-replica read must report a refused-peer trace while still pending,
// then finish from its original transaction after the storage process returns.
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
	for _, client := range []string{"pure-Go", "libfdb_c"} {
		for _, kind := range readKinds {
			k, team := pickStorageTeam(t, ctx, f, 1)
			worker := startCRWorker(t, ctx, f, client, kind, k)
			restart := killStorage(t, ctx, f, team[0])
			victim := worker.issueAndAwaitRefusal(t, ctx, team)
			requireCRRefused(t, victim)
			t.Logf("%s %s: restart sole replica %s at %s", client, kind, victim, time.Now().Format(time.RFC3339Nano))
			if _, err := containerExec(ctx, f.container, restart); err != nil {
				t.Fatalf("restart sole replica %s: %v", victim, err)
			}
			worker.requireResult(t, ctx, f.want(kind, k))
		}
	}
}

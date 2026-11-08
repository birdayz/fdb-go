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
	native      cgofdb.Database // owned uncached handle; never close a borrowed cache entry
	container   *foundationdbtc.Container
	prefix      []byte
	n           int
	valueSize   int
	double      bool
	plan        crBootstrapPlan
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
	opts = append(opts, crClusterOptions()...)
	opts = append(opts, foundationdbtc.WithKnob("max_read_transaction_life_versions", crReadLifeVersions))
	container, err := foundationdbtc.Run(startup, "", append([]testcontainers.ContainerCustomizer{foundationdbtc.WithDirectIP()}, opts...)...)
	if err != nil {
		if foundationdbtc.SkipWithoutDocker(err) {
			t.Skipf("FDB not available (no Docker): %v", err)
		}
		t.Fatalf("multi-process cluster startup: %v", err)
	}
	t.Cleanup(func() { foundationdbtc.TeardownT(t, container) })
	replicas := 1
	if f.double {
		replicas = 2
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
	f.native, err = cgofdb.OpenWithConnectionString(content)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.native.Close)
	f.plan = bootstrapCRCluster(t, startup, container, f.native, replicas)
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

// Stateless is StorageNeverAssign even in critical recruitment (Locality.cpp).
const crPrimaryClass = "stateless"

func crClusterOptions() []testcontainers.ContainerCustomizer {
	return []testcontainers.ContainerCustomizer{
		foundationdbtc.WithoutInit(),
		testcontainers.WithEnv(map[string]string{"FDB_PROCESS_CLASS": crPrimaryClass}),
	}
}

type crProcess struct {
	address, class, locality, directory, command string
}

type crBootstrapPlan struct {
	primary   string
	replicas  int
	processes []crProcess
}

func crPlan(host string, basePort, replicas int) (crBootstrapPlan, error) {
	if replicas != 1 && replicas != 2 {
		return crBootstrapPlan{}, fmt.Errorf("unsupported refusal replica count %d", replicas)
	}
	plan := crBootstrapPlan{primary: net.JoinHostPort(host, strconv.Itoa(basePort)), replicas: replicas}
	for i := 0; i < 3+replicas; i++ {
		class := "log"
		if i >= 3 {
			class = "storage"
		}
		port := basePort + i + 1
		p := crProcess{
			address: net.JoinHostPort(host, strconv.Itoa(port)), class: class,
			locality: fmt.Sprintf("connrefused-%d", port), directory: fmt.Sprintf("/var/fdb/data/connrefused%d", i+1),
		}
		p.command = fmt.Sprintf(
			"mkdir -p %s && fdbserver --listen-address 0.0.0.0:%d --public-address %s "+
				"--cluster-file /var/fdb/fdb.cluster --datadir %s --logdir /var/fdb/logs "+
				"--class %s --locality-zoneid=%s --locality-machineid=%s "+
				"--knob_disable_posix_kernel_aio=1 --knob_max_read_transaction_life_versions=%s "+
				">/var/fdb/logs/connrefused%d.out 2>&1 &",
			p.directory, port, p.address, p.directory, p.class, p.locality, p.locality, crReadLifeVersions, i+1)
		plan.processes = append(plan.processes, p)
	}
	return plan, nil
}

type crRole struct {
	Role string `json:"role"`
}

type crWorkerStatus struct {
	Address  string `json:"address"`
	Class    string `json:"class_type"`
	Locality struct {
		Process string `json:"processid"`
		Zone    string `json:"zoneid"`
		Machine string `json:"machineid"`
	} `json:"locality"`
	Roles []crRole `json:"roles"`
}

type crClusterStatus struct {
	Cluster struct {
		Processes     map[string]crWorkerStatus `json:"processes"`
		Configuration struct {
			Redundancy string `json:"redundancy_mode"`
		} `json:"configuration"`
	} `json:"cluster"`
}

// Postconfiguration status checks public class/locality/roles, not the hidden
// recoveredDiskFiles flag that native recruitment checks.
func crCheckWorkers(raw []byte, plan crBootstrapPlan, recruited bool) (bool, error) {
	var status crClusterStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return false, fmt.Errorf("decode refusal workers: %w", err)
	}
	if len(status.Cluster.Processes) == 0 {
		return false, nil
	}
	expected := map[string]crProcess{plan.primary: {address: plan.primary, class: crPrimaryClass}}
	for _, p := range plan.processes {
		expected[p.address] = p
	}
	addresses, identities := make(map[string]bool), make(map[string]bool)
	zones, machines := make(map[string]bool), make(map[string]bool)
	ready := true
	for id, worker := range status.Cluster.Processes {
		if id == "" || worker.Address == "" || worker.Class == "" || worker.Locality.Process == "" || worker.Locality.Zone == "" || worker.Locality.Machine == "" {
			ready = false
			continue
		}
		if addresses[worker.Address] || identities[worker.Locality.Process] {
			return false, fmt.Errorf("duplicate refusal worker address or process identity: %q", worker.Address)
		}
		addresses[worker.Address], identities[worker.Locality.Process] = true, true
		if zones[worker.Locality.Zone] || machines[worker.Locality.Machine] {
			return false, fmt.Errorf("duplicate refusal worker zone or machine locality: %q", worker.Address)
		}
		zones[worker.Locality.Zone], machines[worker.Locality.Machine] = true, true
		p, ok := expected[worker.Address]
		if !ok {
			return false, fmt.Errorf("unplanned refusal worker %q", worker.Address)
		}
		if worker.Class != p.class || p.locality != "" && (worker.Locality.Zone != p.locality || worker.Locality.Machine != p.locality) {
			return false, fmt.Errorf("worker %s class/locality does not match bootstrap plan", worker.Address)
		}
		if !recruited {
			continue
		}
		if p.class == crPrimaryClass {
			for _, role := range worker.Roles {
				if role.Role == "storage" {
					return false, fmt.Errorf("stateless primary %s acquired storage", worker.Address)
				}
			}
			continue
		}
		if len(worker.Roles) == 0 {
			ready = false
		}
		for _, role := range worker.Roles {
			if role.Role != p.class {
				return false, fmt.Errorf("planned %s worker %s has unexpected role %q", p.class, worker.Address, role.Role)
			}
		}
	}
	for address := range expected {
		if !addresses[address] {
			ready = false
		}
	}
	mode := status.Cluster.Configuration.Redundancy
	if !recruited && mode != "" {
		return false, fmt.Errorf("refusal cluster configured before worker registration check")
	}
	if recruited {
		want := "single"
		if plan.replicas == 2 {
			want = "double"
		}
		if mode != "" && mode != want {
			return false, fmt.Errorf("refusal redundancy %q, want %q", mode, want)
		}
		ready = ready && mode == want
	}
	return ready, nil
}

func crAwaitWorkers(ctx context.Context, plan crBootstrapPlan, recruited bool, status func(context.Context) (string, string, error)) error {
	var stdout, stderr string
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("await refusal workers: %w\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
		}
		var err error
		stdout, stderr, err = status(ctx)
		if err != nil {
			return fmt.Errorf("read refusal worker status: %w\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
		}
		ready, err := crCheckWorkers([]byte(stdout), plan, recruited)
		if err != nil {
			return fmt.Errorf("%w\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

const crWorkerInterfacesPrefix = "\xff\xff/worker_interfaces/"

// API 730 dispatches this exact special-key module to getClientWorkers, not
// status or a system-key read (ReadYourWrites.actor.cpp:1606-1629,1715-1720).
func crNativeWorkerInterfaces(ctx context.Context, db cgofdb.Database) ([]cgofdb.KeyValue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tr, err := db.CreateTransaction()
	if err != nil {
		return nil, err
	}
	defer tr.Cancel()
	stop := context.AfterFunc(ctx, tr.Cancel)
	defer stop()
	if err := tr.Options().SetTimeout(crMetadataTimeout(ctx)); err != nil {
		return nil, err
	}
	return tr.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(crWorkerInterfacesPrefix), End: cgofdb.Key("\xff\xff/worker_interfaces0")}, cgofdb.RangeOptions{}).GetSliceWithError()
}

// The CC map snapshot proves registered addresses only, not class, locality or
// recoveredDiskFiles. Those public postconfiguration fields are checked later.
func crCheckWorkerInterfaces(plan crBootstrapPlan, workers []cgofdb.KeyValue) (bool, error) {
	if plan.primary == "" || (plan.replicas != 1 && plan.replicas != 2) || len(plan.processes) != 3+plan.replicas {
		return false, fmt.Errorf("invalid refusal registration plan")
	}
	expected := map[string]bool{plan.primary: true}
	for _, p := range plan.processes {
		if p.address == "" || expected[p.address] {
			return false, fmt.Errorf("empty or duplicate planned worker address %q", p.address)
		}
		expected[p.address] = true
	}
	seen := make(map[string]bool)
	for _, worker := range workers {
		key := string(worker.Key)
		if !strings.HasPrefix(key, crWorkerInterfacesPrefix) || len(worker.Value) == 0 {
			return false, fmt.Errorf("invalid native worker interface key %q or empty interface", key)
		}
		address := strings.TrimPrefix(key, crWorkerInterfacesPrefix)
		if !expected[address] || seen[address] {
			return false, fmt.Errorf("unplanned or duplicate registered worker %q", address)
		}
		seen[address] = true
	}
	return len(seen) == len(expected), nil
}

func crConfigureRegistered(ctx context.Context, plan crBootstrapPlan, workers func(context.Context) ([]cgofdb.KeyValue, error), status func(context.Context) (string, string, error), configure func(context.Context) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("await native registered worker interfaces: %w", err)
		}
		interfaces, err := workers(ctx)
		if err != nil {
			return fmt.Errorf("read native registered worker interfaces: %w", err)
		}
		ready, err := crCheckWorkerInterfaces(plan, interfaces)
		if err != nil {
			return err
		}
		if ready {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("configure registered refusal workers: %w", err)
	}
	if err := configure(ctx); err != nil {
		return err
	}
	return crAwaitWorkers(ctx, plan, true, status)
}

// LogClass can fall back to StorageWorstFit; reject unexpected roles after recruitment.
func bootstrapCRCluster(t *testing.T, ctx context.Context, container *foundationdbtc.Container, native cgofdb.Database, replicas int) crBootstrapPlan {
	t.Helper()
	host, _, err := net.SplitHostPort(container.InternalAddress())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := crPlan(host, container.FDBPort(), replicas)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plan.processes {
		if _, err := containerExec(ctx, container, p.command); err != nil {
			t.Fatalf("start dedicated %s process at %s: %v", p.class, p.address, err)
		}
		ready, cancel := context.WithTimeout(ctx, 15*time.Second)
		ticker := time.NewTicker(20 * time.Millisecond)
		for {
			conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ready, "tcp", p.address)
			if err == nil {
				conn.Close()
				break
			}
			select {
			case <-ready.Done():
				ticker.Stop()
				cancel()
				t.Fatalf("dedicated %s process did not listen on %s before configure-new: %v", p.class, p.address, err)
			case <-ticker.C:
			}
		}
		ticker.Stop()
		cancel()
	}
	// Start all class/locality listeners before INITIALIZE so the native CC census fences
	// address registration, not disk recovery; query full status only after configuration.
	if err := crConfigureRegistered(ctx, plan, func(ctx context.Context) ([]cgofdb.KeyValue, error) {
		return crNativeWorkerInterfaces(ctx, native)
	}, func(ctx context.Context) (string, string, error) {
		return crStorageStatus(ctx, container)
	}, container.InitializeDatabase); err != nil {
		t.Fatalf("initialize refusal cluster with registered dedicated processes: %v", err)
	}
	return plan
}

// storageAddresses returns the addresses of key's replicas, through a fresh
// pure-Go handle so no cached location is consulted.
func storageAddresses(t *testing.T, ctx context.Context, f crFixture, key []byte) []string {
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
	defer tr.Cancel()
	if err := tr.Options().SetTimeout(crMetadataTimeout(ctx)); err != nil {
		t.Fatal(err)
	}
	addrs, err := tr.LocalityGetAddressesForKey(gofdb.Key(key)).Get()
	if err != nil {
		t.Fatalf("addresses for %q: %v", key, err)
	}
	return addrs
}

func crMetadataTimeout(ctx context.Context) int64 {
	millis := int64(10000)
	if deadline, ok := ctx.Deadline(); ok {
		millis = min(millis, max(int64(1), time.Until(deadline).Milliseconds()))
	}
	return millis
}

func crNativeAddresses(t *testing.T, ctx context.Context, db cgofdb.Database, key []byte) []string {
	t.Helper()
	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Cancel()
	if err := tr.Options().SetTimeout(crMetadataTimeout(ctx)); err != nil {
		t.Fatal(err)
	}
	addrs, err := tr.LocalityGetAddressesForKey(cgofdb.Key(key)).Get()
	if err != nil {
		t.Fatalf("native addresses for %q: %v", key, err)
	}
	return addrs
}

func crSameAddresses(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int)
	for _, address := range a {
		counts[address]++
	}
	for _, address := range b {
		counts[address]--
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

func crCheckTeam(plan crBootstrapPlan, safe map[string]bool, addresses, native []string) error {
	if !crSameAddresses(addresses, native) {
		return fmt.Errorf("Go/native storage addresses disagree: Go=%v native=%v", addresses, native)
	}
	if len(addresses) != plan.replicas {
		return fmt.Errorf("team %v has %d replicas, want %d", addresses, len(addresses), plan.replicas)
	}
	planned := make(map[string]bool)
	for _, p := range plan.processes {
		if p.class == "storage" {
			planned[p.address] = true
		}
	}
	seen := make(map[string]bool)
	for _, address := range addresses {
		if address == plan.primary || !planned[address] || !safe[address] || seen[address] {
			return fmt.Errorf("team %v is not distinct planned non-primary storage-only processes", addresses)
		}
		seen[address] = true
	}
	return nil
}

// Only the actual planned storage-only team may be killed, never a log/primary fallback.
func pickStorageTeam(t *testing.T, ctx context.Context, f crFixture, replicas int) (int, []string) {
	t.Helper()
	if replicas != f.plan.replicas {
		t.Fatalf("requested %d replicas, bootstrap planned %d", replicas, f.plan.replicas)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	native := f.native
	var lastTeam error
	for {
		status, diagnostic, err := crStorageStatus(ctx, f.container)
		if err != nil {
			t.Fatalf("check storage process roles: %v\nstdout:\n%s\nstderr:\n%s", err, status, diagnostic)
		}
		if diagnostic != "" {
			t.Logf("storage role status stderr:\n%s", diagnostic)
		}
		ready, err := crCheckWorkers([]byte(status), f.plan, true)
		if err != nil {
			t.Fatalf("%v\nstdout:\n%s\nstderr:\n%s", err, status, diagnostic)
		}
		safe, err := crStorageOnlyAddresses([]byte(status))
		if err != nil {
			t.Fatalf("%v\nstdout:\n%s\nstderr:\n%s", err, status, diagnostic)
		}
		if ready {
			for i := range f.n {
				addrs := storageAddresses(t, ctx, f, f.key(i))
				caddrs := crNativeAddresses(t, ctx, native, f.key(i))
				if !crSameAddresses(addrs, caddrs) {
					t.Fatalf("seed %q: Go/native storage addresses disagree: Go=%v native=%v\nstatus:\n%s", f.key(i), addrs, caddrs, status)
				}
				lastTeam = crCheckTeam(f.plan, safe, addrs, caddrs)
				if lastTeam == nil {
					t.Logf("seed %q: planned storage-only team Go=%v native=%v", f.key(i), addrs, caddrs)
					return i, addrs
				}
			}
		}
		if ctx.Err() != nil {
			t.Fatalf("no seeded key has a planned team of exactly %d non-primary storage processes: %v; last team: %v; status:\n%s", replicas, ctx.Err(), lastTeam, status)
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
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

func crBootstrapStatus(plan crBootstrapPlan, recruited bool) crClusterStatus {
	var status crClusterStatus
	status.Cluster.Processes = make(map[string]crWorkerStatus)
	processes := append([]crProcess{{address: plan.primary, class: crPrimaryClass, locality: "primary-locality"}}, plan.processes...)
	for i, p := range processes {
		id := fmt.Sprintf("worker-%d", i)
		worker := crWorkerStatus{Address: p.address, Class: p.class}
		worker.Locality.Process, worker.Locality.Zone, worker.Locality.Machine = id, p.locality, p.locality
		if i == 0 {
			worker.Roles = []crRole{{Role: "coordinator"}}
		} else if recruited {
			worker.Roles = []crRole{{Role: p.class}}
		}
		status.Cluster.Processes[id] = worker
	}
	if recruited {
		status.Cluster.Configuration.Redundancy = "single"
		if plan.replicas == 2 {
			status.Cluster.Configuration.Redundancy = "double"
		}
	}
	return status
}

func crStatusJSON(t *testing.T, status crClusterStatus) string {
	t.Helper()
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func crEditWorker(status *crClusterStatus, id string, change func(*crWorkerStatus)) {
	worker := status.Cluster.Processes[id]
	change(&worker)
	status.Cluster.Processes[id] = worker
}

func TestConnectRefusedBootstrapPlan(t *testing.T) {
	t.Parallel()
	var request testcontainers.GenericContainerRequest
	for _, option := range crClusterOptions() {
		if err := option.Customize(&request); err != nil {
			t.Fatal(err)
		}
	}
	if request.Env["FDB_PROCESS_CLASS"] != "stateless" {
		t.Fatalf("primary environment: %v", request.Env)
	}
	for _, replicas := range []int{1, 2} {
		plan, err := crPlan("127.0.0.1", 4500, replicas)
		if err != nil {
			t.Fatal(err)
		}
		if plan.primary != "127.0.0.1:4500" || plan.replicas != replicas || len(plan.processes) != 3+replicas {
			t.Fatalf("unexpected bootstrap plan: %+v", plan)
		}
		seenAddresses, seenDirectories, seenLocalities := make(map[string]bool), make(map[string]bool), make(map[string]bool)
		for i, p := range plan.processes {
			wantClass := "log"
			if i >= 3 {
				wantClass = "storage"
			}
			if p.class != wantClass || p.address != net.JoinHostPort("127.0.0.1", strconv.Itoa(4501+i)) || seenAddresses[p.address] || seenDirectories[p.directory] || seenLocalities[p.locality] {
				t.Fatalf("non-dedicated bootstrap process: %+v", p)
			}
			seenAddresses[p.address], seenDirectories[p.directory], seenLocalities[p.locality] = true, true, true
			for _, argument := range []string{
				"--cluster-file /var/fdb/fdb.cluster", "--class " + wantClass,
				"--public-address " + p.address, "--datadir " + p.directory,
				"--locality-zoneid=" + p.locality, "--locality-machineid=" + p.locality,
				"--knob_disable_posix_kernel_aio=1", "--knob_max_read_transaction_life_versions=" + crReadLifeVersions,
			} {
				if !strings.Contains(p.command, argument) {
					t.Fatalf("bootstrap command lacks %q: %s", argument, p.command)
				}
			}
		}
	}
	for _, replicas := range []int{0, 3} {
		if _, err := crPlan("127.0.0.1", 4500, replicas); err == nil {
			t.Fatalf("accepted unsupported replica count %d", replicas)
		}
	}
}

func crTestWorkerInterfaces(plan crBootstrapPlan) []cgofdb.KeyValue {
	workers := []cgofdb.KeyValue{{Key: cgofdb.Key(crWorkerInterfacesPrefix + plan.primary), Value: []byte("opaque primary interface")}}
	for _, p := range plan.processes {
		workers = append(workers, cgofdb.KeyValue{Key: cgofdb.Key(crWorkerInterfacesPrefix + p.address), Value: []byte("opaque worker interface")})
	}
	return workers
}

func TestConnectRefusedWorkerInterfaces(t *testing.T) {
	t.Parallel()
	for _, replicas := range []int{1, 2} {
		for _, tc := range []struct {
			name             string
			change           func(*crBootstrapPlan, *[]cgofdb.KeyValue)
			ready, wantError bool
		}{
			{name: "complete", ready: true},
			{name: "empty", change: func(_ *crBootstrapPlan, w *[]cgofdb.KeyValue) { *w = nil }},
			{name: "missing_primary", change: func(_ *crBootstrapPlan, w *[]cgofdb.KeyValue) { *w = (*w)[1:] }},
			{name: "missing_log", change: func(_ *crBootstrapPlan, w *[]cgofdb.KeyValue) { *w = append((*w)[:1], (*w)[2:]...) }},
			{name: "missing_storage", change: func(_ *crBootstrapPlan, w *[]cgofdb.KeyValue) { *w = (*w)[:len(*w)-1] }},
			{name: "duplicate", wantError: true, change: func(_ *crBootstrapPlan, w *[]cgofdb.KeyValue) { *w = append(*w, (*w)[0]) }},
			{name: "unexpected", wantError: true, change: func(_ *crBootstrapPlan, w *[]cgofdb.KeyValue) {
				(*w)[0].Key = cgofdb.Key(crWorkerInterfacesPrefix + "127.0.0.1:9999")
			}},
			{name: "wrong_prefix", wantError: true, change: func(_ *crBootstrapPlan, w *[]cgofdb.KeyValue) {
				(*w)[0].Key = cgofdb.Key("worker_interfaces/127.0.0.1:4500")
			}},
			{name: "empty_address", wantError: true, change: func(_ *crBootstrapPlan, w *[]cgofdb.KeyValue) { (*w)[0].Key = cgofdb.Key(crWorkerInterfacesPrefix) }},
			{name: "empty_interface", wantError: true, change: func(_ *crBootstrapPlan, w *[]cgofdb.KeyValue) { (*w)[0].Value = nil }},
			{name: "empty_primary", wantError: true, change: func(p *crBootstrapPlan, _ *[]cgofdb.KeyValue) { p.primary = "" }},
			{name: "invalid_replicas", wantError: true, change: func(p *crBootstrapPlan, _ *[]cgofdb.KeyValue) { p.replicas = 3 }},
			{name: "wrong_process_count", wantError: true, change: func(p *crBootstrapPlan, _ *[]cgofdb.KeyValue) { p.processes = p.processes[1:] }},
			{name: "empty_planned_address", wantError: true, change: func(p *crBootstrapPlan, _ *[]cgofdb.KeyValue) { p.processes[0].address = "" }},
			{name: "duplicate_planned_address", wantError: true, change: func(p *crBootstrapPlan, _ *[]cgofdb.KeyValue) { p.processes[0].address = p.primary }},
		} {
			t.Run(fmt.Sprintf("%d/%s", replicas, tc.name), func(t *testing.T) {
				plan, err := crPlan("127.0.0.1", 4500, replicas)
				if err != nil {
					t.Fatal(err)
				}
				workers := crTestWorkerInterfaces(plan)
				if tc.change != nil {
					tc.change(&plan, &workers)
				}
				ready, err := crCheckWorkerInterfaces(plan, workers)
				if ready != tc.ready || (err != nil) != tc.wantError {
					t.Fatalf("ready=%v err=%v, want ready=%v error=%v", ready, err, tc.ready, tc.wantError)
				}
			})
		}
	}
}

func TestConnectRefusedBootstrapOrder(t *testing.T) {
	t.Parallel()
	plan, err := crPlan("127.0.0.1", 4500, 1)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("phase failed")
	for _, phase := range []string{"complete", "empty", "missing", "native_error", "invalid_interface", "invalid_key", "unexpected_worker", "duplicate_worker", "canceled_census", "configure_error", "missing_post", "malformed_post", "status_error", "expired"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if phase == "expired" {
				cancel()
			}
			var order []string
			err := crConfigureRegistered(ctx, plan, func(context.Context) ([]cgofdb.KeyValue, error) {
				order = append(order, "registered")
				workers := crTestWorkerInterfaces(plan)
				switch phase {
				case "empty":
					cancel()
					return nil, nil
				case "missing":
					cancel()
					return workers[:len(workers)-1], nil
				case "native_error":
					return nil, failure
				case "invalid_interface":
					workers[0].Value = nil
				case "invalid_key":
					workers[0].Key = cgofdb.Key("wrong module")
				case "unexpected_worker":
					workers[0].Key = cgofdb.Key(crWorkerInterfacesPrefix + "127.0.0.1:9999")
				case "duplicate_worker":
					workers = append(workers, workers[0])
				case "canceled_census":
					cancel()
				}
				return workers, nil
			}, func(context.Context) (string, string, error) {
				order = append(order, "recruited")
				switch phase {
				case "missing_post":
					cancel()
					status := crBootstrapStatus(plan, true)
					delete(status.Cluster.Processes, "worker-4")
					return crStatusJSON(t, status), "postconfiguration incomplete", nil
				case "malformed_post":
					return "{", "malformed status", nil
				case "status_error":
					return "partial stdout", "status diagnostic", failure
				}
				return crStatusJSON(t, crBootstrapStatus(plan, true)), "", nil
			}, func(context.Context) error {
				order = append(order, "configure")
				if phase == "configure_error" {
					return failure
				}
				return nil
			})
			wantOrder := "registered"
			switch phase {
			case "expired":
				wantOrder = ""
			case "configure_error":
				wantOrder = "registered,configure"
			case "complete", "missing_post", "malformed_post", "status_error":
				wantOrder = "registered,configure,recruited"
			}
			if strings.Join(order, ",") != wantOrder || (err == nil) != (phase == "complete") {
				t.Fatalf("order=%v err=%v, want order=%s success=%v", order, err, wantOrder, phase == "complete")
			}
			if strings.HasSuffix(phase, "_error") && !errors.Is(err, failure) {
				t.Fatalf("lost phase error: %v", err)
			}
			if phase == "status_error" && (!strings.Contains(err.Error(), "partial stdout") || !strings.Contains(err.Error(), "status diagnostic")) {
				t.Fatalf("lost diagnostics: %v", err)
			}
			if phase == "malformed_post" && !strings.Contains(err.Error(), "decode refusal workers") {
				t.Fatalf("lost decode error: %v", err)
			}
		})
	}
}

func TestConnectRefusedRegisteredWorkers(t *testing.T) {
	t.Parallel()
	plan, err := crPlan("127.0.0.1", 4500, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name             string
		recruited        bool
		change           func(*crClusterStatus)
		raw              string
		ready, wantError bool
	}{
		{name: "registered_empty_roles", ready: true},
		{name: "recruited", recruited: true, ready: true},
		{name: "malformed", raw: `{`, wantError: true},
		{name: "truncated", raw: `{"cluster":{"processes":`, wantError: true},
		{name: "trailing_json", raw: `{} {}`, wantError: true},
		{name: "no_population", raw: `{}`},
		{name: "missing_worker", change: func(s *crClusterStatus) { delete(s.Cluster.Processes, "worker-4") }},
		{name: "missing_address", change: func(s *crClusterStatus) { crEditWorker(s, "worker-4", func(w *crWorkerStatus) { w.Address = "" }) }},
		{name: "missing_identity", change: func(s *crClusterStatus) {
			crEditWorker(s, "worker-4", func(w *crWorkerStatus) { w.Locality.Process = "" })
		}},
		{name: "missing_class", change: func(s *crClusterStatus) { crEditWorker(s, "worker-4", func(w *crWorkerStatus) { w.Class = "" }) }},
		{name: "missing_locality", change: func(s *crClusterStatus) {
			crEditWorker(s, "worker-4", func(w *crWorkerStatus) { w.Locality.Zone = "" })
		}},
		{name: "missing_machine", change: func(s *crClusterStatus) {
			crEditWorker(s, "worker-4", func(w *crWorkerStatus) { w.Locality.Machine = "" })
		}},
		{name: "missing_map_identity", change: func(s *crClusterStatus) {
			s.Cluster.Processes[""] = s.Cluster.Processes["worker-4"]
			delete(s.Cluster.Processes, "worker-4")
		}},
		{name: "missing_primary", change: func(s *crClusterStatus) { delete(s.Cluster.Processes, "worker-0") }},
		{name: "missing_log", change: func(s *crClusterStatus) { delete(s.Cluster.Processes, "worker-1") }},
		{name: "wrong_json_type", raw: `{"cluster":{"processes":[]}}`, wantError: true},
		{name: "duplicate_address", change: func(s *crClusterStatus) {
			crEditWorker(s, "worker-4", func(w *crWorkerStatus) { w.Address = plan.processes[4].address })
		}, wantError: true},
		{name: "duplicate_identity", change: func(s *crClusterStatus) {
			crEditWorker(s, "worker-4", func(w *crWorkerStatus) { w.Locality.Process = "worker-5" })
		}, wantError: true},
		{name: "duplicate_zone", change: func(s *crClusterStatus) {
			crEditWorker(s, "worker-0", func(w *crWorkerStatus) { w.Locality.Zone = plan.processes[0].locality })
		}, wantError: true},
		{name: "duplicate_machine", change: func(s *crClusterStatus) {
			crEditWorker(s, "worker-0", func(w *crWorkerStatus) { w.Locality.Machine = plan.processes[0].locality })
		}, wantError: true},
		{name: "wrong_primary_class", change: func(s *crClusterStatus) { crEditWorker(s, "worker-0", func(w *crWorkerStatus) { w.Class = "unset" }) }, wantError: true},
		{name: "wrong_log_class", change: func(s *crClusterStatus) { crEditWorker(s, "worker-1", func(w *crWorkerStatus) { w.Class = "storage" }) }, wantError: true},
		{name: "wrong_storage_class", change: func(s *crClusterStatus) { crEditWorker(s, "worker-4", func(w *crWorkerStatus) { w.Class = "log" }) }, wantError: true},
		{name: "wrong_zone", change: func(s *crClusterStatus) {
			crEditWorker(s, "worker-4", func(w *crWorkerStatus) { w.Locality.Zone = "other" })
		}, wantError: true},
		{name: "wrong_machine", change: func(s *crClusterStatus) {
			crEditWorker(s, "worker-4", func(w *crWorkerStatus) { w.Locality.Machine = "other" })
		}, wantError: true},
		{name: "unplanned_worker", change: func(s *crClusterStatus) {
			crEditWorker(s, "worker-4", func(w *crWorkerStatus) { w.Address = "127.0.0.1:4599" })
		}, wantError: true},
		{name: "configured_before_registration", change: func(s *crClusterStatus) { s.Cluster.Configuration.Redundancy = "double" }, wantError: true},
		{name: "missing_storage_role", recruited: true, change: func(s *crClusterStatus) { crEditWorker(s, "worker-4", func(w *crWorkerStatus) { w.Roles = nil }) }},
		{name: "missing_log_role", recruited: true, change: func(s *crClusterStatus) { crEditWorker(s, "worker-1", func(w *crWorkerStatus) { w.Roles = nil }) }},
		{name: "unknown_role", recruited: true, change: func(s *crClusterStatus) {
			crEditWorker(s, "worker-4", func(w *crWorkerStatus) { w.Roles = []crRole{{}} })
		}, wantError: true},
		{name: "storage_mixed_roles", recruited: true, change: func(s *crClusterStatus) {
			crEditWorker(s, "worker-4", func(w *crWorkerStatus) { w.Roles = append(w.Roles, crRole{Role: "grv_proxy"}) })
		}, wantError: true},
		{name: "log_storage_fallback", recruited: true, change: func(s *crClusterStatus) {
			crEditWorker(s, "worker-1", func(w *crWorkerStatus) { w.Roles = append(w.Roles, crRole{Role: "storage"}) })
		}, wantError: true},
		{name: "primary_storage", recruited: true, change: func(s *crClusterStatus) {
			crEditWorker(s, "worker-0", func(w *crWorkerStatus) { w.Roles = append(w.Roles, crRole{Role: "storage"}) })
		}, wantError: true},
		{name: "wrong_redundancy", recruited: true, change: func(s *crClusterStatus) { s.Cluster.Configuration.Redundancy = "single" }, wantError: true},
		{name: "configuration_pending", recruited: true, change: func(s *crClusterStatus) { s.Cluster.Configuration.Redundancy = "" }},
		{name: "recovery_flag_is_not_registration", raw: `{"cluster":{"recoveredDiskFiles":true}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := crBootstrapStatus(plan, tc.recruited)
			if tc.change != nil {
				tc.change(&status)
			}
			raw := tc.raw
			if raw == "" {
				raw = crStatusJSON(t, status)
			}
			ready, err := crCheckWorkers([]byte(raw), plan, tc.recruited)
			if ready != tc.ready || (err != nil) != tc.wantError {
				t.Fatalf("ready=%v err=%v, want ready=%v error=%v; status=%s", ready, err, tc.ready, tc.wantError, raw)
			}
		})
	}
}

func TestConnectRefusedMetadataTimeout(t *testing.T) {
	t.Parallel()
	if got := crMetadataTimeout(context.Background()); got != 10000 {
		t.Fatalf("unbounded context timeout=%d, want 10000", got)
	}
	for _, tc := range []struct {
		name      string
		remaining time.Duration
		min, max  int64
	}{
		{name: "capped", remaining: time.Minute, min: 10000, max: 10000},
		{name: "remaining_budget", remaining: 5 * time.Second, min: 1, max: 5000},
		{name: "expired", remaining: -time.Second, min: 1, max: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(tc.remaining))
			defer cancel()
			if got := crMetadataTimeout(ctx); got < tc.min || got > tc.max {
				t.Fatalf("metadata timeout=%d, want [%d,%d]", got, tc.min, tc.max)
			}
		})
	}
}

func TestConnectRefusedPlannedTeam(t *testing.T) {
	t.Parallel()
	plan, err := crPlan("127.0.0.1", 4500, 2)
	if err != nil {
		t.Fatal(err)
	}
	a, b := plan.processes[3].address, plan.processes[4].address
	for _, tc := range []struct {
		name              string
		addresses, native []string
		safe              map[string]bool
		valid             bool
	}{
		{name: "planned", addresses: []string{a, b}, native: []string{b, a}, safe: map[string]bool{a: true, b: true}, valid: true},
		{name: "empty"},
		{name: "wrong_count", addresses: []string{a}, native: []string{a}, safe: map[string]bool{a: true}},
		{name: "native_missing", addresses: []string{a, b}, native: []string{a}, safe: map[string]bool{a: true, b: true}},
		{name: "primary", addresses: []string{plan.primary, b}, native: []string{plan.primary, b}, safe: map[string]bool{plan.primary: true, b: true}},
		{name: "mixed_role", addresses: []string{a, b}, native: []string{a, b}, safe: map[string]bool{a: true}},
		{name: "unplanned_storage", addresses: []string{a, "127.0.0.1:4599"}, native: []string{a, "127.0.0.1:4599"}, safe: map[string]bool{a: true, "127.0.0.1:4599": true}},
		{name: "duplicates", addresses: []string{a, a}, native: []string{a, a}, safe: map[string]bool{a: true}},
		{name: "native_disagreement", addresses: []string{a, b}, native: []string{a, "127.0.0.1:4599"}, safe: map[string]bool{a: true, b: true}},
		{name: "native_duplicates", addresses: []string{a, b}, native: []string{a, a}, safe: map[string]bool{a: true, b: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := crCheckTeam(plan, tc.safe, tc.addresses, tc.native)
			if (err == nil) != tc.valid {
				t.Fatalf("team error=%v, want valid=%v", err, tc.valid)
			}
		})
	}
	// The CI status had two empty storage-only processes but all data on its unsafe primary.
	ci := `{"cluster":{"processes":{"p":{"address":"172.16.0.7:4500","class_type":"unset","roles":[{"role":"storage","stored_bytes":1215968},{"role":"grv_proxy"}]},"a":{"address":"172.16.0.7:4501","class_type":"storage","roles":[{"role":"storage","stored_bytes":0}]},"b":{"address":"172.16.0.7:4502","class_type":"storage","roles":[{"role":"storage","stored_bytes":0}]}}}}`
	safe, err := crStorageOnlyAddresses([]byte(ci))
	if err != nil {
		t.Fatal(err)
	}
	single, err := crPlan("172.16.0.7", 4500, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(safe) != 2 || crCheckTeam(single, safe, []string{single.primary}, []string{single.primary}) == nil {
		t.Fatalf("accepted CI's unsafe primary despite empty registered storage processes: %v", safe)
	}
	sole := single.processes[3].address
	if err := crCheckTeam(single, map[string]bool{sole: true}, []string{sole}, []string{sole}); err != nil {
		t.Fatalf("rejected planned sole storage: %v", err)
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
	cdb := f.native
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
		t.Logf("watch client=%s phase=commit-complete at=%s key=%x", client, time.Now().UTC().Format(time.RFC3339Nano), f.key(k))
		if _, err := setter.Transact(func(tr gofdb.WritableTransaction) (any, error) {
			tr.Set(gofdb.Key(f.key(k)), []byte("changed by "+client))
			return nil, nil
		}); err != nil {
			t.Fatalf("change the watched key: %v", err)
		}
		t.Logf("watch client=%s phase=change-committed at=%s key=%x", client, time.Now().UTC().Format(time.RFC3339Nano), f.key(k))
		if err := fired(); err != nil {
			code, _ := fdbCode(err)
			t.Fatalf("%s: watch with a refusing replica: %v (code %d)", client, err, code)
		}
		t.Logf("watch client=%s phase=fire-complete at=%s key=%x", client, time.Now().UTC().Format(time.RFC3339Nano), f.key(k))
	}
}

// Each single-replica read must report a refused-peer trace while still pending,
// then finish from its original transaction after the storage process returns.
func TestLibFDBC_ConnectRefusedOnlyReplicaWaitsForIt(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	// Dedicated storage is recruited before seeding, without relying on shard migration.
	f := crFixture{prefix: []byte(t.Name() + "/"), n: 100, valueSize: 10000}
	startCRCluster(t, ctx, &f,
		foundationdbtc.WithRedundancyMode("single"),
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

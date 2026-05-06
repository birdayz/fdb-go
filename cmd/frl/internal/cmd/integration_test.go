//go:build integration

// Integration tests for the frl CLI. Spins up an FDB testcontainer once
// per process, seeds a store via the recordlayer API, then drives cobra
// commands end-to-end.
//
// Skipped by default (opt-in build tag) so `go test ./...` and
// `bazelisk test //cmd/frl/...` stay fast. Run with:
//
//	go test -tags=integration ./cmd/frl/internal/cmd/...
package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// integrationFixture is the process-wide test state. Populated once by
// TestMain and shared across all integration tests.
type integrationFixture struct {
	clusterFilePath string
	metaFilePath    string
	configFilePath  string
	keyspacePath    string // operator-facing "/frl/integration"
	cleanupDir      string // removed at process exit
}

var fixture *integrationFixture

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithAPIVersion(730),
		foundationdbtc.WithStorageEngine("memory"),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration setup: start FDB container: %v\n", err)
		os.Exit(1)
	}

	clusterFilePath, err := container.ClusterFilePath(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration setup: cluster file path: %v\n", err)
		os.Exit(1)
	}

	if err := fdb.APIVersion(730); err != nil {
		fmt.Fprintf(os.Stderr, "integration setup: fdb.APIVersion: %v\n", err)
		os.Exit(1)
	}
	db, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration setup: open FDB: %v\n", err)
		os.Exit(1)
	}
	recDB := recordlayer.NewFDBDatabase(db)

	md := buildIntegrationMetaData()
	tmp, err := os.MkdirTemp("", "frl-integration-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration setup: tmpdir: %v\n", err)
		os.Exit(1)
	}
	metaFile := filepath.Join(tmp, "meta.pb")
	mf, err := os.Create(metaFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration setup: create meta.pb: %v\n", err)
		os.Exit(1)
	}
	if err := recordlayer.WriteRecordMetaData(md, mf); err != nil {
		fmt.Fprintf(os.Stderr, "integration setup: write meta.pb: %v\n", err)
		os.Exit(1)
	}
	mf.Close()

	keyspacePath := "/frl/integration"
	ss := subspace.Sub("frl", "integration")
	if err := seedStore(ctx, recDB, md, ss); err != nil {
		fmt.Fprintf(os.Stderr, "integration setup: seed store: %v\n", err)
		os.Exit(1)
	}

	configFile := filepath.Join(tmp, "config.yaml")
	cfgYAML := fmt.Sprintf(`current_context: it
contexts:
  - name: it
    cluster_file: %s
    keyspace_path: %s
    metadata:
      meta_file: %s
`, clusterFilePath, keyspacePath, metaFile)
	if err := os.WriteFile(configFile, []byte(cfgYAML), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "integration setup: write config: %v\n", err)
		os.Exit(1)
	}

	fixture = &integrationFixture{
		clusterFilePath: clusterFilePath,
		metaFilePath:    metaFile,
		configFilePath:  configFile,
		keyspacePath:    keyspacePath,
		cleanupDir:      tmp,
	}

	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

// buildIntegrationMetaData returns the seeded metadata used by all
// integration tests. Single Order index on `price` so index scan /
// index ls have something to show.
func buildIntegrationMetaData() *recordlayer.RecordMetaData {
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	md, err := b.Build()
	if err != nil {
		panic(fmt.Sprintf("buildIntegrationMetaData: %v", err))
	}
	return md
}

// seedStore saves 3 Orders with monotonically-increasing ids + prices so
// the integration tests can assert specific values (price=100 etc).
func seedStore(ctx context.Context, db *recordlayer.FDBDatabase, md *recordlayer.RecordMetaData, ss subspace.Subspace) error {
	_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		for i := int64(1); i <= 3; i++ {
			order := &gen.Order{
				OrderId: proto.Int64(i),
				Price:   proto.Int32(int32(100 * i)),
			}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	return err
}

// countFixture is a second fixture with record counting enabled. Built
// lazily on first access so tests that don't need it pay nothing.
var (
	countFixtureOnce sync.Once
	countFixture     *integrationFixture
	countFixtureErr  error
)

// setupCountFixture builds a store under /frl/integration-count with
// ungrouped record counting enabled (record_count_key = EmptyKeyExpression)
// and one seeded Order so record count returns a non-zero number.
func setupCountFixture(t *testing.T) *integrationFixture {
	t.Helper()
	countFixtureOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		// Reuse the cluster the primary fixture already started.
		db, err := fdb.OpenDatabase(fixture.clusterFilePath)
		if err != nil {
			countFixtureErr = fmt.Errorf("open FDB: %w", err)
			return
		}
		recDB := recordlayer.NewFDBDatabase(db)

		// Count-enabled metadata.
		b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		b.SetRecordCountKey(&recordlayer.EmptyKeyExpression{})
		md, err := b.Build()
		if err != nil {
			countFixtureErr = fmt.Errorf("build count metadata: %w", err)
			return
		}

		tmp, err := os.MkdirTemp("", "frl-integration-count-*")
		if err != nil {
			countFixtureErr = fmt.Errorf("tmpdir: %w", err)
			return
		}
		metaFile := filepath.Join(tmp, "meta.pb")
		mf, err := os.Create(metaFile)
		if err != nil {
			countFixtureErr = fmt.Errorf("create meta.pb: %w", err)
			return
		}
		if err := recordlayer.WriteRecordMetaData(md, mf); err != nil {
			mf.Close()
			countFixtureErr = fmt.Errorf("write meta.pb: %w", err)
			return
		}
		mf.Close()

		keyspacePath := "/frl/integration-count"
		ss := subspace.Sub("frl", "integration-count")
		if err := seedStore(ctx, recDB, md, ss); err != nil {
			countFixtureErr = fmt.Errorf("seed count store: %w", err)
			return
		}

		configFile := filepath.Join(tmp, "config.yaml")
		cfgYAML := fmt.Sprintf(`current_context: count
contexts:
  - name: count
    cluster_file: %s
    keyspace_path: %s
    metadata:
      meta_file: %s
`, fixture.clusterFilePath, keyspacePath, metaFile)
		if err := os.WriteFile(configFile, []byte(cfgYAML), 0o600); err != nil {
			countFixtureErr = fmt.Errorf("write count config: %w", err)
			return
		}

		countFixture = &integrationFixture{
			clusterFilePath: fixture.clusterFilePath,
			metaFilePath:    metaFile,
			configFilePath:  configFile,
			keyspacePath:    keyspacePath,
			cleanupDir:      tmp,
		}
	})
	if countFixtureErr != nil {
		t.Fatalf("setupCountFixture: %v", countFixtureErr)
	}
	return countFixture
}

// runCmd drives one cobra command through NewRoot() with captured IO.
func runCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRoot()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

// bindConfig points FRL_CONFIG at the fixture config for the current test.
// Using t.Setenv keeps tests serial for this bit (t.Setenv forbids Parallel),
// which is fine — integration tests are serialised anyway via the single
// seeded store.
func bindConfig(t *testing.T) {
	t.Helper()
	t.Setenv("FRL_CONFIG", fixture.configFilePath)
}

func TestIntegration_StoreInfo(t *testing.T) {
	bindConfig(t)
	out, err := runCmd(t, "store", "info")
	if err != nil {
		t.Fatalf("store info: %v\nout:\n%s", err, out)
	}
	for _, want := range []string{"Context:", "Keyspace path:", "Format version:"} {
		if !strings.Contains(out, want) {
			t.Errorf("store info output missing %q:\n%s", want, out)
		}
	}
}

func TestIntegration_StoreInfo_JSON(t *testing.T) {
	bindConfig(t)
	out, err := runCmd(t, "store", "info", "-o", "json")
	if err != nil {
		t.Fatalf("store info -o json: %v\nout:\n%s", err, out)
	}
	// protojson shape of DataStoreInfo. We don't assert specific values
	// because format/metadata versions depend on the record-layer
	// defaults at write time; instead, assert that the structure is
	// parseable JSON and contains at least one expected field name.
	trimmed := strings.TrimSpace(out)
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		t.Errorf("-o json output is not a JSON object:\n%s", out)
	}
	if !strings.Contains(out, `"formatVersion"`) {
		t.Errorf("-o json output missing formatVersion key:\n%s", out)
	}
}

func TestIntegration_RecordScan(t *testing.T) {
	bindConfig(t)
	out, err := runCmd(t, "record", "scan", "--type", "Order", "--limit", "10")
	if err != nil {
		t.Fatalf("record scan: %v\nout:\n%s", err, out)
	}
	for _, want := range []string{`"record_type":"Order"`, `"order_id"`} {
		if !strings.Contains(out, want) {
			t.Errorf("record scan output missing %q:\n%s", want, out)
		}
	}
}

// TestIntegration_RecordScan_UnknownTypeLists validates the fail-fast
// path: `--type` with a bogus name must error with the available types,
// not silently emit zero records after a full-store scan. The previous
// code path fell through to ScanRecordsByType's slow-path filter which
// matched nothing for unknown types.
func TestIntegration_RecordScan_UnknownTypeLists(t *testing.T) {
	bindConfig(t)
	_, err := runCmd(t, "record", "scan", "--type", "Orders" /* typo */)
	if err == nil {
		t.Fatal("expected error for unknown --type, got nil (silent empty output?)")
	}
	for _, want := range []string{"Orders", "not found", "Order", "Customer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v missing expected substring %q", err, want)
		}
	}
}

// TestIntegration_RecordScanReverse verifies --reverse walks the tail first.
// The fixture seeds Order records with OrderId 1, 2, 3 — forward scan with
// limit 1 must return order_id=1; reverse with limit 1 must return
// order_id=3. This is the "tail-style inspection" promise in --help.
func TestIntegration_RecordScanReverse(t *testing.T) {
	bindConfig(t)

	forward, err := runCmd(t, "record", "scan", "--type", "Order", "--limit", "1")
	if err != nil {
		t.Fatalf("forward scan: %v\nout:\n%s", err, forward)
	}
	reverse, err := runCmd(t, "record", "scan", "--type", "Order", "--reverse", "--limit", "1")
	if err != nil {
		t.Fatalf("reverse scan: %v\nout:\n%s", err, reverse)
	}

	// Happy-path assertions — each scan returns exactly one record whose
	// order_id matches its end of the [1..3] range.
	if strings.Count(forward, "\n") != 1 {
		t.Errorf("forward --limit 1 returned %d lines, want 1:\n%s",
			strings.Count(forward, "\n"), forward)
	}
	if strings.Count(reverse, "\n") != 1 {
		t.Errorf("reverse --limit 1 returned %d lines, want 1:\n%s",
			strings.Count(reverse, "\n"), reverse)
	}
	if !strings.Contains(forward, `"order_id":"1"`) {
		t.Errorf("forward --limit 1 did not land on order_id=1:\n%s", forward)
	}
	if !strings.Contains(reverse, `"order_id":"3"`) {
		t.Errorf("reverse --limit 1 did not land on order_id=3:\n%s", reverse)
	}
	// Guard against a regression where forward/reverse produce identical
	// output (i.e. --reverse silently ignored — easy mistake).
	if forward == reverse {
		t.Errorf("forward and reverse returned identical output — --reverse ignored?\n%s", forward)
	}
}

func TestIntegration_RecordGet(t *testing.T) {
	bindConfig(t)
	out, err := runCmd(t, "record", "get", "1")
	if err != nil {
		t.Fatalf("record get: %v\nout:\n%s", err, out)
	}
	if !strings.Contains(out, `"order_id"`) || !strings.Contains(out, `"1"`) {
		t.Errorf("record get output missing order_id=1:\n%s", out)
	}
}

// TestIntegration_RecordGet_NotFound locks in the not-found error
// shape. Operators scripting `frl record get | jq` branch on exit code,
// and the message has to name both the PK and the keyspace — otherwise
// logs reading "record not found" across dozens of stores are useless.
func TestIntegration_RecordGet_NotFound(t *testing.T) {
	bindConfig(t)
	_, err := runCmd(t, "record", "get", "999999")
	if err == nil {
		t.Fatal("expected error for missing PK")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v; want 'not found'", err)
	}
	if !strings.Contains(err.Error(), "999999") {
		t.Errorf("error = %v; should echo the PK argument", err)
	}
	if !strings.Contains(err.Error(), fixture.keyspacePath) {
		t.Errorf("error = %v; should name the keyspace_path (%s)",
			err, fixture.keyspacePath)
	}
}

func TestIntegration_IndexLs(t *testing.T) {
	bindConfig(t)
	out, err := runCmd(t, "index", "ls")
	if err != nil {
		t.Fatalf("index ls: %v\nout:\n%s", err, out)
	}
	if !strings.Contains(out, "Order$price") || !strings.Contains(out, "readable") {
		t.Errorf("index ls didn't show Order$price readable:\n%s", out)
	}
}

// TestIntegration_IndexDescribe covers both the text and JSON renderers
// on an actual loaded metadata file. Text mode must show the familiar
// key:value lines; JSON mode must produce an object keyed off the
// documented field names (subset checked — the renderer's contract is
// locked in by unit tests).
func TestIntegration_IndexDescribe(t *testing.T) {
	bindConfig(t)

	text, err := runCmd(t, "index", "describe", "Order$price")
	if err != nil {
		t.Fatalf("index describe: %v\nout:\n%s", err, text)
	}
	for _, want := range []string{"Name:", "Order$price", "Type:", "value", "Expression fields:", "price"} {
		if !strings.Contains(text, want) {
			t.Errorf("index describe text missing %q:\n%s", want, text)
		}
	}

	jsonOut, err := runCmd(t, "index", "describe", "Order$price", "-o", "json")
	if err != nil {
		t.Fatalf("index describe -o json: %v\nout:\n%s", err, jsonOut)
	}
	for _, want := range []string{
		`"name":`, `"Order$price"`,
		`"type":`, `"value"`,
		`"expression_fields":`, `"price"`,
		`"options":`, // must be present even when empty
	} {
		if !strings.Contains(jsonOut, want) {
			t.Errorf("index describe JSON missing %q:\n%s", want, jsonOut)
		}
	}

	// Unknown index name must produce a helpful list of available names,
	// not a stack trace.
	_, err = runCmd(t, "index", "describe", "nope")
	if err == nil {
		t.Fatal("expected error for unknown index name")
	}
	if !strings.Contains(err.Error(), "Order$price") {
		t.Errorf("error %v did not list available index name", err)
	}
}

func TestIntegration_IndexScan(t *testing.T) {
	bindConfig(t)
	out, err := runCmd(t, "index", "scan", "Order$price", "--limit", "10")
	if err != nil {
		t.Fatalf("index scan: %v\nout:\n%s", err, out)
	}
	if !strings.Contains(out, `"index":"Order$price"`) || !strings.Contains(out, `"index_values":"100"`) {
		t.Errorf("index scan output missing expected entries:\n%s", out)
	}
}

// TestIntegration_IndexScan_UnknownLists — guardrail for the operator
// typo experience. Unknown index names must exit with the available
// list in the error, matching `index describe`'s behaviour.
func TestIntegration_IndexScan_UnknownLists(t *testing.T) {
	bindConfig(t)
	_, err := runCmd(t, "index", "scan", "Order$bogus")
	if err == nil {
		t.Fatal("expected error for unknown index, got nil")
	}
	if !strings.Contains(err.Error(), "Order$price") {
		t.Errorf("error = %v; must list available index names", err)
	}
	if !strings.Contains(err.Error(), "Order$bogus") {
		t.Errorf("error = %v; must echo the argument the operator typed", err)
	}
}

func TestIntegration_StoreDump(t *testing.T) {
	bindConfig(t)
	out, err := runCmd(t, "store", "dump", "--limit", "100")
	if err != nil {
		t.Fatalf("store dump: %v\nout:\n%s", err, out)
	}
	for _, want := range []string{"store-info", "record", "index"} {
		if !strings.Contains(out, want) {
			t.Errorf("store dump output missing subspace label %q:\n%s", want, out)
		}
	}
}

// TestIntegration_StoreDump_Subspace is the end-to-end proof that the
// --subspace filter actually narrows the FDB range scan (not just
// post-filters), and that unknown subspace names fail with a helpful
// error listing valid labels.
func TestIntegration_StoreDump_Subspace(t *testing.T) {
	bindConfig(t)

	// Filtering to `record` must yield only record lines — no
	// store-info / index / index-range rows. The fixture populates
	// multiple subspaces so this test has teeth.
	out, err := runCmd(t, "store", "dump", "--subspace", "record", "--limit", "100")
	if err != nil {
		t.Fatalf("store dump --subspace record: %v\nout:\n%s", err, out)
	}
	if !strings.Contains(out, "record ") && !strings.Contains(out, "record-version") {
		t.Errorf("--subspace record produced no record-line output:\n%s", out)
	}
	for _, notWant := range []string{"store-info ", "index ", "index-range"} {
		if strings.Contains(out, notWant) {
			t.Errorf("--subspace record leaked %q rows:\n%s", notWant, out)
		}
	}

	// Unknown subspace name → typed-error with available labels listed.
	// Regression guard for operators mistyping the filter value.
	_, err = runCmd(t, "store", "dump", "--subspace", "does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown --subspace value, got nil")
	}
	if !strings.Contains(err.Error(), "unknown --subspace") {
		t.Errorf("error = %v; want 'unknown --subspace'", err)
	}
	// One of the real labels should appear in the error's candidate list.
	if !strings.Contains(err.Error(), "record") {
		t.Errorf("error = %v; should list valid labels", err)
	}
}

func TestIntegration_TxReadVersion(t *testing.T) {
	bindConfig(t)
	out, err := runCmd(t, "tx", "read-version")
	if err != nil {
		t.Fatalf("tx read-version: %v\nout:\n%s", err, out)
	}
	// Output is just a decimal number + newline. Non-empty integer
	// prefix is enough.
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		t.Errorf("tx read-version produced empty output")
	}
}

// TestIntegration_TxReadVersion_JSON locks in the JSON envelope so
// smoke-check pipelines (`frl tx read-version -o json | jq '.read_version > 0'`)
// stay stable. Previously only the text path was integration-tested.
func TestIntegration_TxReadVersion_JSON(t *testing.T) {
	bindConfig(t)
	out, err := runCmd(t, "tx", "read-version", "-o", "json")
	if err != nil {
		t.Fatalf("tx read-version -o json: %v\nout:\n%s", err, out)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %q", err, out)
	}
	// read_version is an int64 — json decodes that as float64.
	rv, ok := obj["read_version"].(float64)
	if !ok {
		t.Fatalf("read_version not a number: %T %v", obj["read_version"], obj["read_version"])
	}
	if rv <= 0 {
		t.Errorf("read_version = %v; want positive", rv)
	}
}

func TestIntegration_StoreInfo_EmptyKeyspace(t *testing.T) {
	// Point at a keyspace that has no store at it yet. store info should
	// return a clear "no store header" error rather than panic or hang.
	// Reuses the primary fixture's cluster + meta.pb but overrides the
	// keyspace_path to somewhere we never wrote.
	tmp := t.TempDir()
	configFile := filepath.Join(tmp, "config.yaml")
	cfgYAML := fmt.Sprintf(`current_context: empty
contexts:
  - name: empty
    cluster_file: %s
    keyspace_path: /frl/never-written
    metadata:
      meta_file: %s
`, fixture.clusterFilePath, fixture.metaFilePath)
	if err := os.WriteFile(configFile, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("FRL_CONFIG", configFile)

	_, err := runCmd(t, "store", "info")
	if err == nil {
		t.Fatal("expected error for empty keyspace, got nil")
	}
	// Error should explicitly mention "store does not exist" so operators
	// know it's a provisioning question, not a permissions / network issue.
	if !strings.Contains(err.Error(), "store does not exist") {
		t.Errorf("error = %v; want 'store does not exist'", err)
	}
}

func TestIntegration_RecordCount_NotEnabled(t *testing.T) {
	bindConfig(t)
	// Metadata has no record_count_key, so this must error with the
	// "not enabled" message.
	_, err := runCmd(t, "record", "count")
	if err == nil {
		t.Fatal("record count without count_key should error")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("error = %v; want 'not enabled'", err)
	}
}

func TestIntegration_RecordCount_Enabled(t *testing.T) {
	f := setupCountFixture(t)
	t.Setenv("FRL_CONFIG", f.configFilePath)

	out, err := runCmd(t, "record", "count")
	if err != nil {
		t.Fatalf("record count: %v\nout:\n%s", err, out)
	}
	// Seeded three orders in setupCountFixture → seedStore.
	trimmed := strings.TrimSpace(out)
	if trimmed != "3" {
		t.Errorf("record count = %q, want 3 (three seeded orders)", trimmed)
	}
}

// TestIntegration_RecordCount_UnknownTypeLists — fail-fast for --type
// on record count mirrors the behaviour now in record scan. Before
// the fix, a typo surfaced as whatever internal error
// GetSnapshotRecordCountForRecordType returned (e.g. "requires
// RecordTypeKeyExpression" if the count_key was wrong too) rather
// than the straightforward "not found — available: …".
func TestIntegration_RecordCount_UnknownTypeLists(t *testing.T) {
	f := setupCountFixture(t)
	t.Setenv("FRL_CONFIG", f.configFilePath)

	_, err := runCmd(t, "record", "count", "--type", "Orders") // typo
	if err == nil {
		t.Fatal("expected error for unknown --type, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v; want 'not found' fail-fast message", err)
	}
	if !strings.Contains(err.Error(), "Order") {
		t.Errorf("error = %v; should list the real types", err)
	}
}

func TestIntegration_RecordCount_JSON(t *testing.T) {
	f := setupCountFixture(t)
	t.Setenv("FRL_CONFIG", f.configFilePath)

	out, err := runCmd(t, "record", "count", "-o", "json")
	if err != nil {
		t.Fatalf("record count -o json: %v\nout:\n%s", err, out)
	}
	// JSON should contain "count": 3 as a bare int.
	if !strings.Contains(out, `"count"`) {
		t.Errorf("JSON output missing count key:\n%s", out)
	}
	if !strings.Contains(out, `3`) {
		t.Errorf("JSON output missing expected count value (3):\n%s", out)
	}
}

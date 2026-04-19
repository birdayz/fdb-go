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
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

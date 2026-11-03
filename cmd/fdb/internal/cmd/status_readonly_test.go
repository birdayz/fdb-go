package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

func TestRunCatalogRead_RejectsUnsupportedReader(t *testing.T) {
	t.Parallel()
	backend := &statusCatalogReadBackend{tx: statusCatalogReader{}}
	db := recordlayer.NewFDBDatabaseWithTransactor(backend, fdb.Database{})
	called := false
	result, err := runCatalogRead(t.Context(), db, func(*recordlayer.FDBRecordContext) (any, error) {
		called = true
		return 1, nil
	})
	if result != nil || err == nil || !strings.Contains(err.Error(), "requires a record context") {
		t.Errorf("unsupported read transaction = (%v, %v); want explicit record-context error", result, err)
	}
	if called {
		t.Error("catalog callback ran with an unsupported read transaction")
	}
}

func TestRunCatalogRead_ContextAndResult(t *testing.T) {
	t.Parallel()
	tx := statusCatalogWritable{}
	backend := &statusCatalogReadBackend{tx: tx}
	db := recordlayer.NewFDBDatabaseWithTransactor(backend, fdb.Database{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result, err := runCatalogRead(ctx, db, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		if rctx.Context() != ctx || rctx.Transaction() != tx {
			t.Error("catalog record context did not retain its read attempt's transaction/context")
		}
		return 42, nil
	})
	if result != 42 || err != nil {
		t.Errorf("catalog read = (%v, %v); want (42, nil)", result, err)
	}
}

func TestRunCatalogRead_PreservesErrors(t *testing.T) {
	t.Parallel()
	failure := errors.New("catalog read failure")
	for _, tc := range []struct {
		name    string
		backend statusCatalogReadBackend
		bodyErr error
		called  bool
	}{
		{name: "backend", backend: statusCatalogReadBackend{err: failure}},
		{name: "transaction-options", backend: statusCatalogReadBackend{tx: statusCatalogReader{optionErr: failure}}},
		{name: "catalog-body", backend: statusCatalogReadBackend{tx: statusCatalogWritable{}}, bodyErr: failure, called: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := recordlayer.NewFDBDatabaseWithTransactor(&tc.backend, fdb.Database{})
			called := false
			result, err := runCatalogRead(t.Context(), db, func(*recordlayer.FDBRecordContext) (any, error) {
				called = true
				return nil, tc.bodyErr
			})
			if result != nil || !errors.Is(err, failure) {
				t.Errorf("catalog read = (%v, %v); want preserved failure", result, err)
			}
			if called != tc.called {
				t.Errorf("callback called = %t; want %t", called, tc.called)
			}
		})
	}
}

func TestCountCatalogDatabasesRejectsUnboundedBatch(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, -1} {
		if count, err := countCatalogDatabasesBatched(t.Context(), nil, size); count != 0 || err == nil {
			t.Errorf("batch size %d = (%d, %v); want explicit bounded-batch requirement", size, count, err)
		}
	}
}

type statusCatalogCountBackend struct {
	fdb.Database
	attempts int
	injected bool
}

func (b *statusCatalogCountBackend) ReadTransactCtx(ctx context.Context, fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	b.attempts++
	result, err := b.Database.ReadTransactCtx(ctx, fn)
	if b.attempts == 1 && err == nil {
		b.injected = true
		return nil, fdb.Error{Code: 1020}
	}
	return result, err
}

type statusCatalogReadBackend struct {
	tx  fdb.ReadTransaction
	err error
}

func (b *statusCatalogReadBackend) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	if b.err != nil {
		return nil, b.err
	}
	return fn(b.tx)
}

func (*statusCatalogReadBackend) Transact(func(fdb.WritableTransaction) (any, error)) (any, error) {
	return nil, errors.New("catalog read used a committing transaction")
}

type statusCatalogReader struct {
	fdb.ReadTransaction
	optionErr error
}

func (r statusCatalogReader) Options() fdb.TransactionOptions {
	return statusCatalogReadOptions{err: r.optionErr}
}

type statusCatalogWritable struct{ fdb.WritableTransaction }

func (statusCatalogWritable) Options() fdb.TransactionOptions { return statusCatalogReadOptions{} }

type statusCatalogReadOptions struct {
	fdb.TransactionOptions
	err error
}

func (o statusCatalogReadOptions) SetRetryLimit(int64) error { return o.err }
func (statusCatalogReadOptions) SetReadSystemKeys() error    { return nil }

// statusReadOnlyChecks runs on the dedicated dev-up cluster before its first
// application write; the process-wide integration fixture already has a catalog.
func statusReadOnlyChecks(t *testing.T, clusterFile string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := openDatabase(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	rec := recordlayer.NewFDBDatabase(db)
	nativeMetaData := buildIntegrationMetaData()
	var metadataBytes bytes.Buffer
	if err := recordlayer.WriteRecordMetaData(nativeMetaData, &metadataBytes); err != nil {
		t.Fatalf("serialize status metadata: %v", err)
	}
	tmp := t.TempDir()
	metadataFile := filepath.Join(tmp, "metadata.pb")
	if err := os.WriteFile(metadataFile, metadataBytes.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	const database = "/FRL/STATUS_READONLY"
	const schema = "MAIN"
	targets := []struct {
		name        string
		flags       []string
		absentStore string
		absentMeta  string
		presentMeta string
	}{
		{
			name:        "native",
			flags:       []string{"--cluster-file", clusterFile, "--keyspace-tuple", `["status-readonly-native"]`, "--meta-file", metadataFile},
			absentStore: "missing:",
			absentMeta:  "ok (3 record types)",
			presentMeta: "ok (3 record types)",
		},
		{
			name:        "relational",
			flags:       []string{"--cluster-file", clusterFile, "--database", database, "--schema", schema},
			absentStore: "skipped:",
			absentMeta:  "error:",
			presentMeta: "ok (1 record types)",
		},
	}
	root := subspace.Sub()
	pristine := snapshotStoreBytes(t, clusterFile, root)
	if pristine != "" {
		t.Fatalf("status regression requires a pristine user keyspace, got:\n%s", pristine)
	}
	for _, target := range targets {
		t.Run("status-absent/"+target.name, func(t *testing.T) {
			report := runReadOnlyStatus(t, target.flags)
			if report.Catalog != "missing (plain-core cluster)" {
				t.Errorf("catalog = %q; want missing on a pristine cluster", report.Catalog)
			}
			if !strings.HasPrefix(report.Store, target.absentStore) {
				t.Errorf("store = %q; want absent-store classification %q", report.Store, target.absentStore)
			}
			if target.name == "native" && report.Metadata != target.absentMeta || target.name == "relational" && !strings.HasPrefix(report.Metadata, target.absentMeta) {
				t.Errorf("metadata = %q; want %q", report.Metadata, target.absentMeta)
			}
			if after := snapshotStoreBytes(t, clusterFile, root); after != pristine {
				t.Fatalf("status created application/catalog keys on a pristine cluster:\n%s", after)
			}
		})
	}

	for _, command := range []string{"databases", "schemas", "templates"} {
		t.Run("catalog-absent/"+command, func(t *testing.T) {
			assertReadOnlyCatalogList(t, clusterFile, command, "[]")
			if after := snapshotStoreBytes(t, clusterFile, root); after != pristine {
				t.Fatalf("catalog %s created keys on a pristine cluster:\n%s", command, after)
			}
		})
	}

	// Seed an actual catalog and pinned schema, so the relational metadata
	// check succeeds after CreateOrOpen stages reconciliation of an old header.
	script := filepath.Join(tmp, "schema.sql")
	ddl := `CREATE DATABASE /FRL/STATUS_READONLY;
CREATE SCHEMA TEMPLATE STATUS_READONLY_TEMPLATE
CREATE TABLE ITEMS (ID BIGINT, NAME STRING, PRIMARY KEY (ID));
CREATE SCHEMA /FRL/STATUS_READONLY/MAIN WITH TEMPLATE STATUS_READONLY_TEMPLATE;
INSERT INTO ITEMS VALUES (1, 'status');
`
	if err := os.WriteFile(script, []byte(ddl), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runCmdSplit(t, "sql", "--cluster-file", clusterFile,
		"--database", database, "--schema", schema, "-f", script)
	if err != nil {
		t.Fatalf("seed status catalog: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	native := subspace.Sub("status-readonly-native")
	if err := seedStore(ctx, rec, nativeMetaData, native); err != nil {
		t.Fatalf("seed native status store: %v", err)
	}
	relational, err := relationalStoreSubspace(ctx, rec, database, schema)
	if err != nil {
		t.Fatalf("resolve status schema: %v", err)
	}
	for _, store := range []struct {
		name string
		ss   subspace.Subspace
	}{
		{name: "catalog", ss: relationalKeyspace().CatalogSubspace()},
		{name: "native", ss: native},
		{name: "relational", ss: relational},
	} {
		lowerStatusHeader(t, ctx, rec, store.name, store.ss)
		if snapshot := snapshotStoreBytes(t, clusterFile, store.ss); snapshot == "" {
			t.Fatalf("%s store snapshot is empty; old-header comparison would be vacuous", store.name)
		}
	}
	before := snapshotStoreBytes(t, clusterFile, root)
	t.Run("catalog-batched-count", func(t *testing.T) {
		backend := &statusCatalogCountBackend{Database: db}
		paged := recordlayer.NewFDBDatabaseWithTransactor(backend, db)
		count, err := countCatalogDatabasesBatched(ctx, paged, 1)
		if err != nil || count != 2 {
			t.Fatalf("catalog count across one-row batches = (%d, %v); want (2, nil)", count, err)
		}
		if !backend.injected || backend.attempts < 3 {
			t.Errorf("catalog read attempts = %d, failure injected = %t; want a failed attempt and multiple successful batches", backend.attempts, backend.injected)
		}
		if after := snapshotStoreBytes(t, clusterFile, root); after != before {
			t.Fatalf("batched catalog count migrated the catalog\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})
	for _, target := range targets {
		t.Run("status-old-header/"+target.name, func(t *testing.T) {
			report := runReadOnlyStatus(t, target.flags)
			if report.Store != "ok" {
				t.Errorf("store = %q; want ok for the existing old-header store", report.Store)
			}
			if report.Metadata != target.presentMeta {
				t.Errorf("metadata = %q; want %q (successful metadata load must also be read-only)", report.Metadata, target.presentMeta)
			}
			if report.Catalog != "ok (2 databases)" {
				t.Errorf("catalog = %q; want ok (2 databases), including /__SYS", report.Catalog)
			}
			if after := snapshotStoreBytes(t, clusterFile, root); after != before {
				t.Fatalf("status changed user/catalog/store bytes with old headers\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
	for _, listing := range []struct {
		command string
		want    string
	}{
		{command: "databases", want: `[{"id":"/FRL/STATUS_READONLY"},{"id":"/__SYS"}]`},
		{command: "schemas", want: `[{"database":"/FRL/STATUS_READONLY","name":"MAIN","template":"STATUS_READONLY_TEMPLATE","template_version":1},{"database":"/__SYS","name":"CATALOG","template":"CATALOG_TEMPLATE","template_version":1}]`},
		{command: "templates", want: `[{"name":"CATALOG_TEMPLATE","version":1},{"name":"STATUS_READONLY_TEMPLATE","version":1}]`},
	} {
		t.Run("catalog-old-header/"+listing.command, func(t *testing.T) {
			assertReadOnlyCatalogList(t, clusterFile, listing.command, listing.want)
			if after := snapshotStoreBytes(t, clusterFile, root); after != before {
				t.Fatalf("catalog %s migrated the old catalog header\nbefore:\n%s\nafter:\n%s", listing.command, before, after)
			}
		})
	}
}

func assertReadOnlyCatalogList(t *testing.T, clusterFile, command, wantJSON string) {
	t.Helper()
	stdout, stderr, err := runCmdSplit(t, "rl", "meta", "catalog", command, "--cluster-file", clusterFile, "-o", "json")
	if err != nil {
		t.Fatalf("catalog %s: %v\nstdout: %s\nstderr: %s", command, err, stdout, stderr)
	}
	var got, want any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode catalog %s: %v\n%s", command, err, stdout)
	}
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("catalog %s = %s; want %s", command, stdout, wantJSON)
	}
}

func runReadOnlyStatus(t *testing.T, flags []string) statusReport {
	t.Helper()
	args := append([]string{"rl", "status", "-o", "json"}, flags...)
	stdout, stderr, err := runCmdSplit(t, args...)
	if err != nil {
		t.Fatalf("status: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	var report statusReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode status: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if report.Cluster != "ok" || report.ReadVersion <= 0 {
		t.Fatalf("status cluster check = %q, version %d; want successful read version", report.Cluster, report.ReadVersion)
	}
	stdout, stderr, err = runCmdSplit(t, append([]string{"rl", "status", "-o", "text"}, flags...)...)
	if err != nil {
		t.Fatalf("text status: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	for _, line := range []string{"store:     " + report.Store, "metadata:  " + report.Metadata, "catalog:   " + report.Catalog} {
		if !strings.Contains(stdout, line+"\n") {
			t.Errorf("text status missing %q:\n%s", line, stdout)
		}
	}
	return report
}

func lowerStatusHeader(t *testing.T, ctx context.Context, rec *recordlayer.FDBDatabase, name string, ss subspace.Subspace) {
	t.Helper()
	_, err := rec.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		key := ss.Pack(tuple.Tuple{recordlayer.StoreInfoKey})
		raw, err := rctx.Transaction().Get(key).Get()
		if err != nil {
			return nil, err
		}
		info := &gen.DataStoreInfo{}
		if err := recordlayer.UnmarshalAsJava(raw, info); err != nil {
			return nil, err
		}
		version := info.GetMetaDataversion()
		if version <= 0 {
			return nil, fmt.Errorf("%s header version %d cannot be lowered for the regression", name, version)
		}
		info.MetaDataversion = proto.Int32(version - 1)
		old, err := proto.Marshal(info)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(raw, old) {
			return nil, fmt.Errorf("%s old header mutation changed no bytes", name)
		}
		rctx.Transaction().Set(key, old)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("write deliberately old %s header: %v", name, err)
	}
}

package otelsql

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/core/metadata"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// One FDB testcontainer: OTEL_V1 is catalog-global, and these subtests store
// stale versions of it, so they run in order on a cluster of their own.
func TestFDB_EnsureTenant(t *testing.T) {
	t.Parallel()
	setup, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(setup, "", foundationdbtc.WithAPIVersion(730))
	if err != nil {
		if foundationdbtc.SkipWithoutDocker(err) {
			t.Skip("FDB not available (no Docker)")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := container.Terminate(ctx); err != nil {
			t.Error(err)
		}
	})
	wire, err := container.ClusterFile(setup)
	if err != nil {
		t.Fatal(err)
	}
	cluster := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(cluster, []byte(wire), 0o600); err != nil {
		t.Fatal(err)
	}
	fdb.MustAPIVersion(730)
	raw, err := fdb.OpenDatabase(cluster)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(raw.Close)
	db := recordlayer.NewFDBDatabase(raw)
	keyspace.RegisterDomainIfNotExists("OTELT")
	cat, err := catalog.NewRecordLayerStoreCatalog(keyspace.New(subspace.Sub()).CatalogSubspace())
	if err != nil {
		t.Fatal(err)
	}
	catalogRun := func(t *testing.T, fn func(api.Transaction) error) {
		t.Helper()
		if _, err := db.Run(t.Context(), func(rc *recordlayer.FDBRecordContext) (any, error) {
			txn := catalog.NewFDBTransaction(rc)
			if err := cat.Initialize(txn); err != nil {
				return nil, err
			}
			return nil, fn(txn)
		}); err != nil {
			t.Fatal(err)
		}
	}
	requireMismatch := func(t *testing.T, err error, schema string) {
		t.Helper()
		var mismatch *TemplateMismatchError
		if !errors.As(err, &mismatch) || mismatch.Schema != schema {
			t.Fatalf("EnsureTenant = %v, want a template mismatch for schema %q", err, schema)
		}
	}
	dropTemplate := func(t *testing.T) {
		catalogRun(t, func(txn api.Transaction) error {
			return cat.SchemaTemplateCatalog().DeleteTemplate(txn, TemplateName, true)
		})
	}

	t.Run("stale template", func(t *testing.T) {
		stale, err := embedded.BuildSchemaTemplateFromDDL("CREATE SCHEMA TEMPLATE " + TemplateName + " CREATE TABLE spans (trace_id BYTES, PRIMARY KEY (trace_id))")
		if err != nil {
			t.Fatal(err)
		}
		catalogRun(t, func(txn api.Transaction) error { return cat.SchemaTemplateCatalog().CreateTemplate(txn, stale) })
		_, err = EnsureTenant(t.Context(), db, "/OTELT/R_STALE", "T0", nil)
		requireMismatch(t, err, "")
		catalogRun(t, func(txn api.Transaction) error {
			if exists, err := cat.DoesDatabaseExist(txn, "/OTELT/R_STALE"); err != nil || exists {
				return errors.Join(err, errors.New("a refused EnsureTenant created the database"))
			}
			return nil
		})
		dropTemplate(t)
	})
	t.Run("later template version", func(t *testing.T) {
		tmpl, _, err := builtTemplate()
		if err != nil {
			t.Fatal(err)
		}
		v2, err := metadata.NewRecordLayerSchemaTemplateWithVersion(TemplateName, tmpl.Underlying(), TemplateVersion+1)
		if err != nil {
			t.Fatal(err)
		}
		catalogRun(t, func(txn api.Transaction) error {
			if err := cat.SchemaTemplateCatalog().CreateTemplate(txn, tmpl); err != nil {
				return err
			}
			return cat.SchemaTemplateCatalog().CreateTemplate(txn, v2)
		})
		_, err = EnsureTenant(t.Context(), db, "/OTELT/R_LATER", "T0", nil)
		requireMismatch(t, err, "")
		dropTemplate(t)
	})

	// Concurrent openers of a fresh catalog: one creates each catalog row,
	// the others' conflicts retry and verify it.
	const path = "/OTELT/R_RACE"
	stores := make([]*TenantStore, 6)
	errs := make([]error, len(stores))
	var configured sync.Map
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			schema := []string{"T0", "t0", "T1"}[i%3]
			stores[i], errs[i] = EnsureTenant(t.Context(), db, path, schema, func(o fdb.TransactionOptions) error {
				configured.Store(i, true)
				return o.SetTimeout(30_000)
			})
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("opener %d: %v", i, err)
		}
		if _, ok := configured.Load(i); !ok {
			t.Fatalf("opener %d's transactions were not configured", i)
		}
		if same := i % 3; i >= 3 && !bytesEqual(stores[i].Subspace.Bytes(), stores[same].Subspace.Bytes()) {
			t.Fatalf("opener %d resolved another store than opener %d", i, same)
		}
	}
	if bytesEqual(stores[0].Subspace.Bytes(), stores[2].Subspace.Bytes()) {
		t.Fatal("two tenants share a store")
	}
	catalogRun(t, func(txn api.Transaction) error {
		rs, err := cat.SchemaTemplateCatalog().ListTemplates(txn)
		if err != nil {
			return err
		}
		n := 0
		for rs.Next() {
			if name, _ := rs.String(1); name == TemplateName {
				n++
			}
		}
		if n != 1 {
			return errors.New("OTEL_V1 is not stored exactly once")
		}
		return rs.Err()
	})

	t.Run("reopen and write", func(t *testing.T) {
		again, err := EnsureTenant(t.Context(), db, path, "T0", nil)
		if err != nil {
			t.Fatal(err)
		}
		if !bytesEqual(again.Subspace.Bytes(), stores[0].Subspace.Bytes()) {
			t.Fatal("reopen resolved another store")
		}
		row, err := BuildCompiled(Span{Span: &tracepb.Span{TraceId: []byte{1}, SpanId: []byte{2}, StartTimeUnixNano: 3}, Refs: []MessageRef{{Writer: 1, Seq: 1, Attribute: 1, Ordinals: []int{0}}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Run(t.Context(), func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := again.Builder(rc).Open()
			if err != nil {
				return nil, err
			}
			saved, err := store.SaveRecord(row)
			if err != nil {
				return nil, err
			}
			rec, err := store.LoadRecord(saved.PrimaryKey)
			if err != nil || rec == nil || !proto.Equal(rec.Record, row) {
				return nil, errors.Join(err, errors.New("saved row does not load back"))
			}
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("configurer failure", func(t *testing.T) {
		refused := errors.New("refused by the configurer")
		if _, err := EnsureTenant(t.Context(), db, path, "T0", func(fdb.TransactionOptions) error { return refused }); !errors.Is(err, refused) {
			t.Fatalf("EnsureTenant = %v, want the configurer's error", err)
		}
	})
	t.Run("schema of another template", func(t *testing.T) {
		other, err := embedded.BuildSchemaTemplateFromDDL("CREATE SCHEMA TEMPLATE OTHER_T CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
		if err != nil {
			t.Fatal(err)
		}
		catalogRun(t, func(txn api.Transaction) error {
			if err := cat.SchemaTemplateCatalog().CreateTemplate(txn, other); err != nil {
				return err
			}
			return ddl.NewCreateSchemaConstantAction(path, "T9", "OTHER_T", cat, keyspace.New(subspace.Sub())).Execute(txn)
		})
		_, err = EnsureTenant(t.Context(), db, path, "T9", nil)
		requireMismatch(t, err, "T9")
	})
	t.Run("removed store", func(t *testing.T) {
		if _, err := db.Run(t.Context(), func(rc *recordlayer.FDBRecordContext) (any, error) {
			begin, end := stores[2].Subspace.FDBRangeKeys()
			rc.Transaction().ClearRange(fdb.KeyRange{Begin: begin, End: end})
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		_, err := EnsureTenant(t.Context(), db, path, "T1", nil)
		var missing *recordlayer.RecordStoreDoesNotExistError
		if !errors.As(err, &missing) {
			t.Fatalf("EnsureTenant of a schema without its store = %v", err)
		}
	})
}

func bytesEqual(a, b []byte) bool { return string(a) == string(b) }

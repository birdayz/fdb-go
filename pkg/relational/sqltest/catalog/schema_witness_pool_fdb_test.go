package sqltest

// A pooled connection keeps the catalog bytes its schema was loaded from
// across ResetSession, and its next statement reuses the schema only while
// the catalog still holds them (embedded connection.go cachedLoadSchema).
// These tests change the catalog from ANOTHER connection between two
// statements of a one-connection pool and require the next statement to see
// the change: a template version re-pointed to, a template row rewritten in
// place under the same version, and a dropped schema.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

// witnessTemplate is template name@version over T(PK, C, V) with index
// T_BY_C; withW adds column W and index T_BY_W.
func witnessTemplate(t *testing.T, name string, version int, withW bool) *metadata.RecordLayerSchemaTemplate {
	t.Helper()
	cols := []metadata.ColumnSpec{
		metadata.NewColumnSpec("PK", api.NewLongType(false), 1),
		metadata.NewColumnSpec("C", api.NewLongType(true), 2),
		metadata.NewColumnSpec("V", api.NewLongType(true), 3),
	}
	if withW {
		cols = append(cols, metadata.NewColumnSpec("W", api.NewLongType(true), 4))
	}
	b := metadata.NewSchemaTemplateBuilder().SetName(name).SetVersion(version)
	b.AddTable("T", cols, []string{"PK"})
	b.AddIndex("T", "T_BY_C", []string{"C"}, false)
	if withW {
		b.AddIndex("T", "T_BY_W", []string{"W"}, false)
	}
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build %s@%d: %v", name, version, err)
	}
	return tmpl
}

// witnessPool creates dbPath with schema S on tmplName@1, fills T with
// PK = C = 1..5, V = 10*PK, and returns a one-connection pool on S whose
// connection has loaded the schema.
func witnessPool(t *testing.T, h *testkit.EvolHarness, dbPath, tmplName string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	h.MustRun(t, "bootstrap", func(txn api.Transaction) error {
		if err := h.Cat.Initialize(txn); err != nil {
			return err
		}
		if err := ddl.NewCreateDatabaseConstantAction(dbPath, h.Cat).Execute(txn); err != nil {
			return err
		}
		if err := ddl.NewSaveSchemaTemplateConstantAction(witnessTemplate(t, tmplName, 1, false), h.Cat.SchemaTemplateCatalog()).Execute(txn); err != nil {
			return err
		}
		return ddl.NewCreateSchemaConstantAction(dbPath, "S", tmplName, h.Cat, h.Ks).Execute(txn)
	})
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", dbPath, testkit.ClusterFile()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO T (PK, C, V) VALUES (1, 1, 10), (2, 2, 20), (3, 3, 30), (4, 4, 40), (5, 5, 50)")
	if got, err := evolQueryPairs(t, db, ctx, "SELECT PK, V FROM T WHERE PK = 3"); err != nil || got != "[3 30]" {
		t.Fatalf("rows %q, %v; want [3 30]", got, err)
	}
	if rows, err := db.QueryContext(ctx, witnessProbe); err == nil {
		_ = rows.Close()
		t.Fatalf("%s@1 planned a query of column W: the probe cannot tell the schemas apart", tmplName)
	}
	return db
}

const witnessProbe = "SELECT PK, W FROM T WHERE PK = 3"

// wantColumnW asserts the pool's next statement plans over a schema with
// column W, and still answers over the old columns.
func wantColumnW(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	var pk int64
	var w sql.NullInt64
	if err := db.QueryRowContext(ctx, witnessProbe).Scan(&pk, &w); err != nil {
		t.Fatalf("%s on the pooled connection after the change: %v (a stale schema has no column W)", witnessProbe, err)
	}
	if pk != 3 || w.Valid {
		t.Fatalf("%s = (%d, %v), want (3, NULL)", witnessProbe, pk, w)
	}
	if got, err := evolQueryPairs(t, db, ctx, "SELECT PK, V FROM T WHERE PK = 3"); err != nil || got != "[3 30]" {
		t.Fatalf("rows %q, %v after the change; want [3 30]", got, err)
	}
}

func TestFDB_PooledConnectionSeesAnotherConnectionsSchemaChange(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}

	t.Run("schema re-pointed to a new template version", func(t *testing.T) {
		t.Parallel()
		h := testkit.NewEvolHarness(t)
		const dbPath, tmplName = "/FRL/SR_WITNESS_REPOINT", "SR_WITNESS_REPOINT"
		db := witnessPool(t, h, dbPath, tmplName)
		h.MustRun(t, "re-point", func(txn api.Transaction) error {
			if err := ddl.NewSaveSchemaTemplateConstantAction(witnessTemplate(t, tmplName, 2, true), h.Cat.SchemaTemplateCatalog()).Execute(txn); err != nil {
				return err
			}
			return h.Cat.RepairSchema(txn, dbPath, "S")
		})
		wantColumnW(t, db)
	})

	t.Run("template row rewritten in place", func(t *testing.T) {
		t.Parallel()
		h := testkit.NewEvolHarness(t)
		const dbPath, tmplName = "/FRL/SR_WITNESS_REWRITE", "SR_WITNESS_REWRITE"
		db := witnessPool(t, h, dbPath, tmplName)
		// A raw write of tmplName@1's row past the catalog's checks, as a
		// restore or a Java writer makes it: the schema row still names
		// version 1, and only the template row's bytes differ.
		p, err := witnessTemplate(t, tmplName, 1, true).Underlying().ToProto()
		if err != nil {
			t.Fatal(err)
		}
		md, err := proto.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		catalogMD, err := catalog.BuildCatalogMetaData()
		if err != nil {
			t.Fatal(err)
		}
		h.MustRun(t, "rewrite", func(txn api.Transaction) error {
			rctx, ok := txn.Unwrap().(*recordlayer.FDBRecordContext)
			if !ok {
				return fmt.Errorf("transaction unwraps to %T", txn.Unwrap())
			}
			store, err := recordlayer.NewStoreBuilder().SetContext(rctx).SetSubspace(h.Ks.CatalogSubspace()).
				SetMetaDataProvider(catalogMD).CreateOrOpen()
			if err != nil {
				return err
			}
			_, err = store.SaveRecord(&gen.Templates{
				TEMPLATE_NAME: proto.String(tmplName), TEMPLATE_VERSION: proto.Int32(1), META_DATA: md,
			})
			return err
		})
		wantColumnW(t, db)
	})

	t.Run("schema dropped", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		h := testkit.NewEvolHarness(t)
		const dbPath, tmplName = "/FRL/SR_WITNESS_DROP", "SR_WITNESS_DROP"
		db := witnessPool(t, h, dbPath, tmplName)
		admin := testkit.OpenDB(t, dbPath)
		testkit.MustExecCtx(t, admin, ctx, "DROP SCHEMA "+dbPath+"/S")

		_, err := evolQueryPairs(t, db, ctx, "SELECT PK, V FROM T WHERE PK = 3")
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.ErrCodeUndefinedSchema {
			t.Fatalf("a statement on the dropped schema = %v, want %s", err, api.ErrCodeUndefinedSchema)
		}
		// As before witnesses: a pool that never loaded the schema fails the
		// same way.
		fresh := testkit.EvolReopen(t, dbPath, "S")
		_, freshErr := evolQueryPairs(t, fresh, ctx, "SELECT PK, V FROM T WHERE PK = 3")
		if freshErr == nil || freshErr.Error() != err.Error() {
			t.Fatalf("the pooled connection's error %q, a fresh pool's %v", err, freshErr)
		}
	})
}

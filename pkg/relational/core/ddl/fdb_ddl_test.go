package ddl_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/catalog"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/ddl"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/keyspace"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
	"github.com/onsi/gomega"
)

// fdbTestDB is set by TestMain for FDB integration tests.
var fdbTestDB *recordlayer.FDBDatabase

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "")
	if err != nil {
		// No Docker available — in-memory tests still run.
		os.Exit(m.Run())
	}
	defer container.Terminate(context.Background()) //nolint:errcheck

	clusterFile, err := container.ClusterFile(ctx)
	if err != nil {
		panic("ClusterFile: " + err.Error())
	}

	tmpFile, err := os.CreateTemp("", "fdb-ddl-*.cluster")
	if err != nil {
		panic(err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.WriteString(clusterFile); err != nil {
		panic(err)
	}
	tmpFile.Close()

	fdb.MustAPIVersion(720)
	rawDB, err := fdb.OpenDatabase(tmpFile.Name())
	if err != nil {
		panic("fdb.OpenDatabase: " + err.Error())
	}
	fdbTestDB = recordlayer.NewFDBDatabase(rawDB)

	os.Exit(m.Run())
}

func newFDBEnv(t *testing.T) (api.StoreCatalog, *keyspace.RelationalKeyspace, *ddl.RecordLayerMetadataOperationsFactory) {
	t.Helper()
	testSubspace := subspace.Sub([]byte("ddl-test"), []byte(t.Name()))
	ks := keyspace.New(testSubspace)
	cat, err := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
	if err != nil {
		t.Fatalf("NewRecordLayerStoreCatalog: %v", err)
	}
	f := ddl.NewRecordLayerMetadataOperationsFactoryWithKeyspace(cat, ks)
	return cat, ks, f
}

func runFDBTxn(t *testing.T, fn func(txn api.Transaction) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := fdbTestDB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		return nil, fn(catalog.NewFDBTransaction(rctx))
	})
	return err
}

func TestFDB_CreateDatabase(t *testing.T) {
	t.Parallel()
	if fdbTestDB == nil {
		t.Skip("no FDB container")
	}
	g := gomega.NewWithT(t)
	cat, _, f := newFDBEnv(t)

	g.Expect(runFDBTxn(t, func(txn api.Transaction) error {
		return f.CreateDatabase("/testdb", api.Options{}).Execute(txn)
	})).To(gomega.Succeed())

	g.Expect(runFDBTxn(t, func(txn api.Transaction) error {
		exists, err := cat.DoesDatabaseExist(txn, "/testdb")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(exists).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())
}

func TestFDB_CreateAndDropSchema(t *testing.T) {
	t.Parallel()
	if fdbTestDB == nil {
		t.Skip("no FDB container")
	}
	g := gomega.NewWithT(t)
	cat, _, f := newFDBEnv(t)

	tmpl := buildTemplate(t, "FDBSchema", 1)

	g.Expect(runFDBTxn(t, func(txn api.Transaction) error {
		if err := f.SaveSchemaTemplate(tmpl, api.Options{}).Execute(txn); err != nil {
			return err
		}
		return f.CreateDatabase("/fdbdb", api.Options{}).Execute(txn)
	})).To(gomega.Succeed())

	// CreateSchema creates catalog entry + FDB record store.
	g.Expect(runFDBTxn(t, func(txn api.Transaction) error {
		return f.CreateSchema("/fdbdb", "s1", "FDBSchema", api.Options{}).Execute(txn)
	})).To(gomega.Succeed())

	// Schema must now be in catalog.
	g.Expect(runFDBTxn(t, func(txn api.Transaction) error {
		schema, err := cat.LoadSchema(txn, "/fdbdb", "s1")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(schema.MetadataName()).To(gomega.Equal("s1"))
		return nil
	})).To(gomega.Succeed())

	// DropSchema removes catalog entry (and FDB store when ks provided).
	g.Expect(runFDBTxn(t, func(txn api.Transaction) error {
		return f.DropSchema("/fdbdb", "s1", api.Options{}).Execute(txn)
	})).To(gomega.Succeed())

	g.Expect(runFDBTxn(t, func(txn api.Transaction) error {
		_, err := cat.LoadSchema(txn, "/fdbdb", "s1")
		g.Expect(err).To(gomega.HaveOccurred())
		return nil
	})).To(gomega.Succeed())
}

func TestFDB_DropDatabase_Cascade(t *testing.T) {
	t.Parallel()
	if fdbTestDB == nil {
		t.Skip("no FDB container")
	}
	g := gomega.NewWithT(t)
	cat, _, f := newFDBEnv(t)

	tmpl := buildTemplate(t, "CascadeTemplate", 1)

	g.Expect(runFDBTxn(t, func(txn api.Transaction) error {
		if err := f.SaveSchemaTemplate(tmpl, api.Options{}).Execute(txn); err != nil {
			return err
		}
		if err := f.CreateDatabase("/cascadedb", api.Options{}).Execute(txn); err != nil {
			return err
		}
		return f.CreateSchema("/cascadedb", "s1", "CascadeTemplate", api.Options{}).Execute(txn)
	})).To(gomega.Succeed())

	g.Expect(runFDBTxn(t, func(txn api.Transaction) error {
		return f.DropDatabase("/cascadedb", true, api.Options{}).Execute(txn)
	})).To(gomega.Succeed())

	g.Expect(runFDBTxn(t, func(txn api.Transaction) error {
		exists, err := cat.DoesDatabaseExist(txn, "/cascadedb")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(exists).To(gomega.BeFalse())
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_CatalogOnly exercises the factory in catalog-only mode (no
// keyspace) — CreateSchema creates the catalog entry but no FDB store.
func TestFDB_CatalogOnly(t *testing.T) {
	t.Parallel()
	if fdbTestDB == nil {
		t.Skip("no FDB container")
	}
	g := gomega.NewWithT(t)

	testSubspace := subspace.Sub([]byte("ddl-test"), []byte(t.Name()))
	ks := keyspace.New(testSubspace)
	cat, err := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
	g.Expect(err).NotTo(gomega.HaveOccurred())
	// Catalog-only: no keyspace passed to factory.
	f := ddl.NewRecordLayerMetadataOperationsFactory(cat)

	tmpl := buildTemplate(t, "CatalogOnlyTmpl", 1)

	g.Expect(runFDBTxn(t, func(txn api.Transaction) error {
		if err := f.SaveSchemaTemplate(tmpl, api.Options{}).Execute(txn); err != nil {
			return err
		}
		return f.CreateDatabase("/catonly", api.Options{}).Execute(txn)
	})).To(gomega.Succeed())

	g.Expect(runFDBTxn(t, func(txn api.Transaction) error {
		return f.CreateSchema("/catonly", "s1", "CatalogOnlyTmpl", api.Options{}).Execute(txn)
	})).To(gomega.Succeed())

	g.Expect(runFDBTxn(t, func(txn api.Transaction) error {
		schema, err := cat.LoadSchema(txn, "/catonly", "s1")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(schema.MetadataName()).To(gomega.Equal("s1"))
		return nil
	})).To(gomega.Succeed())

	g.Expect(runFDBTxn(t, func(txn api.Transaction) error {
		return f.DropSchema("/catonly", "s1", api.Options{}).Execute(txn)
	})).To(gomega.Succeed())
}

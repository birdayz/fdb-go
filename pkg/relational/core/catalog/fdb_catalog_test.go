package catalog

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"
	"time"

	"github.com/onsi/gomega"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

var testFDB *recordlayer.FDBDatabase

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "", foundationdbtc.WithAPIVersion(720))
	if err != nil {
		log.Fatalf("fdb catalog test: failed to start FDB container: %v", err)
	}

	clusterFile, err := container.ClusterFile(ctx)
	if err != nil {
		log.Fatalf("fdb catalog test: failed to get cluster file: %v", err)
	}

	tmpFile, err := os.CreateTemp("", "fdb_catalog_cluster_*.txt")
	if err != nil {
		log.Fatalf("fdb catalog test: failed to create temp file: %v", err)
	}
	if _, err := tmpFile.WriteString(clusterFile); err != nil {
		log.Fatalf("fdb catalog test: failed to write cluster file: %v", err)
	}
	tmpFile.Close()

	fdb.MustAPIVersion(720)
	rawDB, err := fdb.OpenDatabase(tmpFile.Name())
	if err != nil {
		log.Fatalf("fdb catalog test: failed to open FDB: %v", err)
	}
	testFDB = recordlayer.NewFDBDatabase(rawDB)

	code := m.Run()
	_ = container.Terminate(ctx)
	_ = os.Remove(tmpFile.Name())
	os.Exit(code)
}

// newFDBCatalogInSubspace opens an FDB-backed StoreCatalog rooted at a
// test-unique subspace so parallel tests don't interfere.
func newFDBCatalogInSubspace(t *testing.T) (*RecordLayerStoreCatalog, func(fn func(txn api.Transaction) error) error) {
	t.Helper()
	testSubspace := subspace.Sub([]byte("fdbcat-test"), []byte(t.Name()))
	cat, err := NewRecordLayerStoreCatalog(testSubspace)
	if err != nil {
		t.Fatalf("NewRecordLayerStoreCatalog: %v", err)
	}

	runTxn := func(fn func(txn api.Transaction) error) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := testFDB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
			return nil, fn(NewFDBTransaction(rctx))
		})
		return err
	}

	return cat, runTxn
}

// buildVersionedTemplate builds a named template at an explicit version.
func buildVersionedTemplate(t testing.TB, name string, version int) api.SchemaTemplate {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	tmpl, err := metadata.NewRecordLayerSchemaTemplateWithVersion(name, md, version)
	if err != nil {
		t.Fatalf("NewRecordLayerSchemaTemplateWithVersion: %v", err)
	}
	return tmpl
}

// TestFDB_DatabaseCRUD creates, checks existence, and reads back a database.
func TestFDB_DatabaseCRUD(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.DoesDatabaseExist(tx, "/mydb")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeFalse())
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		return cat.CreateDatabase(tx, "/mydb")
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.DoesDatabaseExist(tx, "/mydb")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_ListDatabases ensures ListDatabases returns all persisted rows.
func TestFDB_ListDatabases(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)

	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(cat.CreateDatabase(tx, "/a")).To(gomega.Succeed())
		g.Expect(cat.CreateDatabase(tx, "/b")).To(gomega.Succeed())
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		rs, err := cat.ListDatabases(tx, nil)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer rs.Close()
		var got []string
		for rs.Next() {
			id, _ := rs.String(1)
			got = append(got, id)
		}
		g.Expect(rs.Err()).ToNot(gomega.HaveOccurred())
		g.Expect(got).To(gomega.ConsistOf("/a", "/b"))
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_TemplateCRUD exercises CreateTemplate / LoadSchemaTemplate /
// DoesSchemaTemplateExist / DeleteTemplate round-trip.
func TestFDB_TemplateCRUD(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()
	tmpl := buildVersionedTemplate(t, "fdb-tmpl", 1)

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := tc.DoesSchemaTemplateExist(tx, "fdb-tmpl")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeFalse())
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		return tc.CreateTemplate(tx, tmpl)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := tc.DoesSchemaTemplateExist(tx, "fdb-tmpl")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		got, err := tc.LoadSchemaTemplate(tx, "fdb-tmpl")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(got.MetadataName()).To(gomega.Equal("fdb-tmpl"))
		g.Expect(got.Version()).To(gomega.Equal(tmpl.Version()))
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		return tc.DeleteTemplate(tx, "fdb-tmpl", true)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := tc.DoesSchemaTemplateExist(tx, "fdb-tmpl")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeFalse())
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_TemplateVersioning verifies that LoadSchemaTemplate returns
// the latest version and LoadSchemaTemplateAtVersion returns exactly
// the requested one.
func TestFDB_TemplateVersioning(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	tmpl1 := buildVersionedTemplate(t, "versioned", 1)
	tmpl2 := buildVersionedTemplate(t, "versioned", 2)

	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(tc.CreateTemplate(tx, tmpl1)).To(gomega.Succeed())
		return tc.CreateTemplate(tx, tmpl2)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		got, err := tc.LoadSchemaTemplate(tx, "versioned")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(got.Version()).To(gomega.Equal(2))
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		got, err := tc.LoadSchemaTemplateAtVersion(tx, "versioned", 1)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(got.Version()).To(gomega.Equal(1))
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_TemplateDuplicateReturnsError: creating (name, version) twice
// returns ErrCodeDuplicateSchemaTemplate.
func TestFDB_TemplateDuplicateReturnsError(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()
	tmpl := buildVersionedTemplate(t, "dup-tmpl", 1)

	g.Expect(run(func(tx api.Transaction) error {
		return tc.CreateTemplate(tx, tmpl)
	})).To(gomega.Succeed())

	err := run(func(tx api.Transaction) error {
		return tc.CreateTemplate(tx, tmpl)
	})
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
	g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeDuplicateSchemaTemplate))
}

// TestFDB_SchemaCRUD exercises the full schema lifecycle on real FDB.
func TestFDB_SchemaCRUD(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()
	tmpl := buildVersionedTemplate(t, "schema-tmpl", 1)

	g.Expect(run(func(tx api.Transaction) error {
		return tc.CreateTemplate(tx, tmpl)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.DoesSchemaExist(tx, "/db", "pub")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeFalse())
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		return cat.SaveSchema(tx, tmpl.GenerateSchema("/db", "pub"), true)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.DoesSchemaExist(tx, "/db", "pub")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		s, err := cat.LoadSchema(tx, "/db", "pub")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(s.MetadataName()).To(gomega.Equal("pub"))
		g.Expect(s.DatabaseName()).To(gomega.Equal("/db"))
		g.Expect(s.SchemaTemplate().MetadataName()).To(gomega.Equal("schema-tmpl"))
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		return cat.DeleteSchema(tx, "/db", "pub")
	})).To(gomega.Succeed())

	err := run(func(tx api.Transaction) error {
		_, err := cat.LoadSchema(tx, "/db", "pub")
		return err
	})
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
	g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeUndefinedSchema))
}

// TestFDB_SaveSchemaWithoutDatabase: createDatabaseIfNecessary=false
// when the database row is missing → ErrCodeUndefinedDatabase.
func TestFDB_SaveSchemaWithoutDatabase(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()
	tmpl := buildVersionedTemplate(t, "no-db-tmpl", 1)

	g.Expect(run(func(tx api.Transaction) error {
		return tc.CreateTemplate(tx, tmpl)
	})).To(gomega.Succeed())

	err := run(func(tx api.Transaction) error {
		return cat.SaveSchema(tx, tmpl.GenerateSchema("/no-such-db", "pub"), false)
	})
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
	g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeUndefinedDatabase))
}

// TestFDB_SaveSchemaWithUnknownTemplate: template not registered →
// ErrCodeUnknownSchemaTemplate.
func TestFDB_SaveSchemaWithUnknownTemplate(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tmpl := buildVersionedTemplate(t, "ghost-tmpl", 1)

	err := run(func(tx api.Transaction) error {
		return cat.SaveSchema(tx, tmpl.GenerateSchema("/db", "pub"), true)
	})
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
	g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeUnknownSchemaTemplate))
}

// TestFDB_ListSchemasInDatabase: multi-schema listing with per-db filter.
func TestFDB_ListSchemasInDatabase(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()
	tmpl := buildVersionedTemplate(t, "list-tmpl", 1)

	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(tc.CreateTemplate(tx, tmpl)).To(gomega.Succeed())
		g.Expect(cat.SaveSchema(tx, tmpl.GenerateSchema("/db1", "s1"), true)).To(gomega.Succeed())
		g.Expect(cat.SaveSchema(tx, tmpl.GenerateSchema("/db1", "s2"), true)).To(gomega.Succeed())
		g.Expect(cat.SaveSchema(tx, tmpl.GenerateSchema("/db2", "s1"), true)).To(gomega.Succeed())
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		rs, err := cat.ListSchemasInDatabase(tx, "/db1", nil)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer rs.Close()
		var got []string
		for rs.Next() {
			name, _ := rs.String(2) // SCHEMA_NAME
			got = append(got, name)
		}
		g.Expect(rs.Err()).ToNot(gomega.HaveOccurred())
		g.Expect(got).To(gomega.ConsistOf("s1", "s2"))
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		rs, err := cat.ListSchemas(tx, nil)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer rs.Close()
		count := 0
		for rs.Next() {
			count++
		}
		g.Expect(rs.Err()).ToNot(gomega.HaveOccurred())
		g.Expect(count).To(gomega.Equal(3))
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_RepairSchema rebinds a schema to the latest template version.
func TestFDB_RepairSchema(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	tmpl1 := buildVersionedTemplate(t, "repair-tmpl", 1)
	tmpl2 := buildVersionedTemplate(t, "repair-tmpl", 2)

	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(tc.CreateTemplate(tx, tmpl1)).To(gomega.Succeed())
		return cat.SaveSchema(tx, tmpl1.GenerateSchema("/db", "pub"), true)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		return tc.CreateTemplate(tx, tmpl2)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		return cat.RepairSchema(tx, "/db", "pub")
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		s, err := cat.LoadSchema(tx, "/db", "pub")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(s.SchemaTemplate().Version()).To(gomega.Equal(2))
		return nil
	})).To(gomega.Succeed())
}

// rawCreateTemplate stores tmpl's bytes as they are, past CreateTemplate's
// checks and carry: a template built any other way than the build path (a
// restore, another writer), which the rebind validator still guards.
func rawCreateTemplate(t *testing.T, cat *RecordLayerStoreCatalog, tx api.Transaction, tmpl api.SchemaTemplate) error {
	t.Helper()
	rl := tmpl.(*metadata.RecordLayerSchemaTemplate)
	store, err := cat.SchemaTemplateCatalog().(*RecordLayerStoreSchemaTemplateCatalog).openStore(tx)
	if err != nil {
		return err
	}
	payload, err := serializeTemplate(rl)
	if err != nil {
		return err
	}
	return writeTemplateRow(store, rl, payload)
}

// TestFDB_SchemaRebindRejectsRecordTypeKeyChange pins the SaveSchema/
// RepairSchema evolution guard on the axis that matters after the RFC-204
// 0-based record-type-key rebase: the record type key is the LEADING tuple
// element of every stored record key, so rebinding a live schema from a
// template whose keys are the pre-RFC 1-based layout to one with 0-based
// keys would make the store read old type A's rows as new type B's —
// silent data corruption. The old-shape template is constructed by
// round-tripping the current build through its proto with the explicit
// keys rewritten to the 1-based layout — the exact bytes a pre-rebase
// catalog persisted.
//
// Through CreateTemplate the new version is carried from the stored one
// (ws-j-design.md section 4), so it keeps key 1 and the rebind is admitted;
// the guard is what refuses a version written past the build path.
func TestFDB_SchemaRebindRejectsRecordTypeKeyChange(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	build := func(name string) *metadata.RecordLayerSchemaTemplate {
		b := metadata.NewSchemaTemplateBuilder().SetName(name)
		b.AddTable("T", []metadata.ColumnSpec{
			metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
			metadata.NewColumnSpec("V", api.NewLongType(true), 2),
		}, []string{"ID"})
		tmpl, err := b.Build()
		g.Expect(err).ToNot(gomega.HaveOccurred())
		return tmpl
	}
	versions := func(name string) (api.SchemaTemplate, api.SchemaTemplate) {
		// v1: the PRE-RFC-204 shape — record type key = union field number
		// (1-based). Produced from real stored-proto bytes, not the builder.
		md1proto, err := build(name).Underlying().ToProto()
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(md1proto.GetRecordTypes()).To(gomega.HaveLen(1))
		md1proto.RecordTypes[0].ExplicitKey = &gen.Value{LongValue: proto.Int64(1)}
		md1, err := recordlayer.RecordMetaDataFromProto(md1proto)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		tmpl1, err := metadata.NewRecordLayerSchemaTemplateWithVersion(name, md1, 1)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		// v2: the current emitter's 0-based key.
		md2proto, err := build(name).Underlying().ToProto()
		g.Expect(err).ToNot(gomega.HaveOccurred())
		md2, err := recordlayer.RecordMetaDataFromProto(md2proto)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		tmpl2, err := metadata.NewRecordLayerSchemaTemplateWithVersion(name, md2, 2)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		return tmpl1, tmpl2
	}
	bound := func(schema string) int {
		var v int
		g.Expect(run(func(tx api.Transaction) error {
			s, lerr := cat.LoadSchema(tx, "/rebinddb", schema)
			g.Expect(lerr).ToNot(gomega.HaveOccurred())
			v = s.SchemaTemplate().Version()
			return nil
		})).To(gomega.Succeed())
		return v
	}

	// Carried: v2 through CreateTemplate keeps v1's key, and the rebind is
	// admitted.
	carried1, carried2 := versions("rebind-key-tmpl")
	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(tc.CreateTemplate(tx, carried1)).To(gomega.Succeed())
		return cat.SaveSchema(tx, carried1.GenerateSchema("/rebinddb", "carried"), true)
	})).To(gomega.Succeed())
	g.Expect(run(func(tx api.Transaction) error {
		return tc.CreateTemplate(tx, carried2)
	})).To(gomega.Succeed())
	g.Expect(run(func(tx api.Transaction) error {
		stored, err := tc.LoadTemplateProto(tx, "rebind-key-tmpl", 2)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(stored.GetRecordTypes()[0].GetExplicitKey().GetLongValue()).To(gomega.Equal(int64(1)), "the stored key is carried")
		return nil
	})).To(gomega.Succeed())
	g.Expect(run(func(tx api.Transaction) error {
		return cat.RepairSchema(tx, "/rebinddb", "carried")
	})).To(gomega.Succeed())
	g.Expect(bound("carried")).To(gomega.Equal(2))

	// Written raw: the rebind must be REJECTED, and the rejection must name
	// the record type key change (the corruption axis), not a generic failure.
	raw1, raw2 := versions("rebind-key-raw")
	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(tc.CreateTemplate(tx, raw1)).To(gomega.Succeed())
		return cat.SaveSchema(tx, raw1.GenerateSchema("/rebinddb", "pub"), true)
	})).To(gomega.Succeed())
	g.Expect(run(func(tx api.Transaction) error {
		return rawCreateTemplate(t, cat, tx, raw2)
	})).To(gomega.Succeed())
	rebindErr := run(func(tx api.Transaction) error {
		return cat.RepairSchema(tx, "/rebinddb", "pub")
	})
	g.Expect(rebindErr).To(gomega.HaveOccurred())
	g.Expect(rebindErr.Error()).To(gomega.ContainSubstring("record type key changed"))
	g.Expect(rebindErr.Error()).To(gomega.ContainSubstring("metadata evolution rejected"))
	// The binding is untouched: still v1.
	g.Expect(bound("pub")).To(gomega.Equal(1))
}

// TestFDB_SchemaRebindRejectsVersionGoingBackwards pins the OTHER arm of the
// rebind guard: the TEMPLATE VERSION must advance.
//
// The two arms fail for unrelated reasons and are not substitutes. The
// evolution validator catches a rebind whose SHAPE is incompatible; this one
// catches a rebind whose shape is perfectly compatible but whose version moves
// BACKWARDS — re-binding a live schema to a superseded template. Nothing else
// in the package exercises it, so deleting the monotonic branch left the
// package green with the guard gone, which is the rot this test closes.
//
// v1 and v2 are structurally IDENTICAL on purpose: if the shape differed, the
// evolution validator could reject the downgrade for its own reasons and the
// test would pass without the version branch existing at all.
func TestFDB_SchemaRebindRejectsVersionGoingBackwards(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	build := func() *metadata.RecordLayerSchemaTemplate {
		b := metadata.NewSchemaTemplateBuilder().SetName("rebind-ver-tmpl")
		b.AddTable("T", []metadata.ColumnSpec{
			metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
			metadata.NewColumnSpec("V", api.NewLongType(true), 2),
		}, []string{"ID"})
		tmpl, err := b.Build()
		g.Expect(err).ToNot(gomega.HaveOccurred())
		return tmpl
	}

	mdFor := func() *recordlayer.RecordMetaData {
		p, err := build().Underlying().ToProto()
		g.Expect(err).ToNot(gomega.HaveOccurred())
		md, err := recordlayer.RecordMetaDataFromProto(p)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		return md
	}

	tmpl1, err := metadata.NewRecordLayerSchemaTemplateWithVersion("rebind-ver-tmpl", mdFor(), 1)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	tmpl2, err := metadata.NewRecordLayerSchemaTemplateWithVersion("rebind-ver-tmpl", mdFor(), 2)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Bind the schema at v2, then try to push it back to v1.
	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(tc.CreateTemplate(tx, tmpl1)).To(gomega.Succeed())
		g.Expect(tc.CreateTemplate(tx, tmpl2)).To(gomega.Succeed())
		return cat.SaveSchema(tx, tmpl2.GenerateSchema("/rebindverdb", "pub"), true)
	})).To(gomega.Succeed())

	downgradeErr := run(func(tx api.Transaction) error {
		return cat.SaveSchema(tx, tmpl1.GenerateSchema("/rebindverdb", "pub"), true)
	})
	g.Expect(downgradeErr).To(gomega.HaveOccurred())
	g.Expect(downgradeErr.Error()).To(gomega.ContainSubstring(
		"cannot rebind schema /rebindverdb/pub to template rebind-ver-tmpl@1: " +
			"version does not advance past the bound rebind-ver-tmpl@2"))

	// The binding is untouched: still v2.
	g.Expect(run(func(tx api.Transaction) error {
		s, lerr := cat.LoadSchema(tx, "/rebindverdb", "pub")
		g.Expect(lerr).ToNot(gomega.HaveOccurred())
		g.Expect(s.SchemaTemplate().Version()).To(gomega.Equal(2))
		return nil
	})).To(gomega.Succeed())

	// An EQUAL version is rejected by the same branch (`<=`), and it is a
	// distinct direction: a guard written `<` would let a same-version rebind
	// to DIFFERENT metadata through, which is the silent-swap case.
	sameErr := run(func(tx api.Transaction) error {
		return cat.SaveSchema(tx, tmpl2.GenerateSchema("/rebindverdb", "pub"), true)
	})
	g.Expect(sameErr).ToNot(gomega.HaveOccurred(),
		"a same-name same-version save is the no-op arm and must be accepted")
}

// TestFDB_DeleteSchemaNotFound: deleting an absent schema →
// ErrCodeUndefinedSchema.
func TestFDB_DeleteSchemaNotFound(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)

	err := run(func(tx api.Transaction) error {
		return cat.DeleteSchema(tx, "/db", "ghost")
	})
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
	g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeUndefinedSchema))
}

// TestFDB_LoadSchemaNotFound: loading an absent schema →
// ErrCodeUndefinedSchema.
func TestFDB_LoadSchemaNotFound(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)

	err := run(func(tx api.Transaction) error {
		_, err := cat.LoadSchema(tx, "/db", "ghost")
		return err
	})
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
	g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeUndefinedSchema))
}

// TestFDB_DeleteTemplateVersionExact deletes a single version while
// leaving others intact.
func TestFDB_DeleteTemplateVersionExact(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	tmpl1 := buildVersionedTemplate(t, "del-ver", 1)
	tmpl2 := buildVersionedTemplate(t, "del-ver", 2)

	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(tc.CreateTemplate(tx, tmpl1)).To(gomega.Succeed())
		return tc.CreateTemplate(tx, tmpl2)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		return tc.DeleteTemplateVersion(tx, "del-ver", 1, true)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := tc.DoesSchemaTemplateExistAtVersion(tx, "del-ver", 1)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeFalse())

		ok2, err := tc.DoesSchemaTemplateExistAtVersion(tx, "del-ver", 2)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok2).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_ListTemplates enumerates persisted templates by name.
func TestFDB_ListTemplates(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	tmplA := buildVersionedTemplate(t, "list-A", 1)
	tmplB := buildVersionedTemplate(t, "list-B", 1)

	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(tc.CreateTemplate(tx, tmplA)).To(gomega.Succeed())
		return tc.CreateTemplate(tx, tmplB)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		rs, err := tc.ListTemplates(tx)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer rs.Close()
		var names []string
		for rs.Next() {
			name, _ := rs.String(1)
			names = append(names, name)
		}
		g.Expect(rs.Err()).ToNot(gomega.HaveOccurred())
		g.Expect(names).To(gomega.ConsistOf("list-A", "list-B"))
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_ClosedTransactionRejected: catalog ops on a closed
// FDBTransaction return ErrCodeTransactionInactive.
func TestFDB_ClosedTransactionRejected(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	ctx30s, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := testFDB.Run(ctx30s, func(ctx *recordlayer.FDBRecordContext) (any, error) {
		tx := NewFDBTransaction(ctx)
		_ = tx.Close()

		cat, cerr := NewRecordLayerStoreCatalog(subspace.Sub([]byte("closed-tx-test")))
		g.Expect(cerr).ToNot(gomega.HaveOccurred())

		_, opErr := cat.DoesDatabaseExist(tx, "/db")
		var apiErr *api.Error
		g.Expect(errors.As(opErr, &apiErr)).To(gomega.BeTrue())
		g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeTransactionInactive))
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
}

// TestFDB_TemplateRoundTripPreservesSchema ensures that LoadSchemaTemplate
// after CreateTemplate round-trips the RecordMetaData correctly — the
// schema's tables must match the original.
func TestFDB_TemplateRoundTripPreservesSchema(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()
	tmpl := buildVersionedTemplate(t, "roundtrip-tmpl", 1)

	g.Expect(run(func(tx api.Transaction) error {
		return tc.CreateTemplate(tx, tmpl)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		got, err := tc.LoadSchemaTemplate(tx, "roundtrip-tmpl")
		g.Expect(err).ToNot(gomega.HaveOccurred())

		// Original template has 3 record types; round-tripped
		// template's generated schema must have the same tables.
		s := got.GenerateSchema("/db", "pub")
		tables, err := s.Tables()
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(tables).To(gomega.HaveLen(3))
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_DeleteDatabase deletes a database and all its schemas, then
// confirms nothing remains.
func TestFDB_DeleteDatabase(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()
	tmpl := buildVersionedTemplate(t, "deldb-tmpl", 1)

	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(tc.CreateTemplate(tx, tmpl)).To(gomega.Succeed())
		g.Expect(cat.SaveSchema(tx, tmpl.GenerateSchema("/deldb", "s1"), true)).To(gomega.Succeed())
		g.Expect(cat.SaveSchema(tx, tmpl.GenerateSchema("/deldb", "s2"), true)).To(gomega.Succeed())
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.DeleteDatabase(tx, "/deldb", true)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())

	// Database and schemas are gone.
	g.Expect(run(func(tx api.Transaction) error {
		dbOK, err := cat.DoesDatabaseExist(tx, "/deldb")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(dbOK).To(gomega.BeFalse())

		s1OK, err := cat.DoesSchemaExist(tx, "/deldb", "s1")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(s1OK).To(gomega.BeFalse())

		s2OK, err := cat.DoesSchemaExist(tx, "/deldb", "s2")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(s2OK).To(gomega.BeFalse())
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_DeleteDatabaseNotFound: throwIfDoesNotExist=true on an
// absent database → ErrCodeUnknownDatabase.
func TestFDB_DeleteDatabaseNotFound(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)

	err := run(func(tx api.Transaction) error {
		_, err := cat.DeleteDatabase(tx, "/no-such", true)
		return err
	})
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
	g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeUnknownDatabase))
}

// TestFDB_DeleteDatabaseSilentOnMissing: throwIfDoesNotExist=false on
// absent database returns (true, nil) — same as Java which relies on
// deleteRecord returning false silently.
func TestFDB_DeleteDatabaseSilentOnMissing(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.DeleteDatabase(tx, "/no-such", false)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_Initialize verifies that Initialize bootstraps the catalog's
// self-referential entries (template + sys database + CATALOG schema).
// Mirrors Java's RecordLayerStoreCatalogTestBase.testListSchemasEmptyResult
// which asserts /__SYS?schema=CATALOG is present after init.
func TestFDB_Initialize(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)

	// Initialize.
	g.Expect(run(func(tx api.Transaction) error {
		return cat.Initialize(tx)
	})).To(gomega.Succeed())

	// Template CATALOG_TEMPLATE v1 must exist.
	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.SchemaTemplateCatalog().DoesSchemaTemplateExistAtVersion(tx, CatalogTemplateName, CatalogTemplateVersion)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())

	// /__SYS database must exist.
	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.DoesDatabaseExist(tx, SysDatabaseID)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())

	// /__SYS/CATALOG schema must exist.
	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.DoesSchemaExist(tx, SysDatabaseID, CatalogConstant)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())

	// Initialize is idempotent.
	g.Expect(run(func(tx api.Transaction) error {
		return cat.Initialize(tx)
	})).To(gomega.Succeed())

	// ListSchemas shows only the catalog schema.
	g.Expect(run(func(tx api.Transaction) error {
		rs, err := cat.ListSchemas(tx, nil)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer rs.Close()
		var schemas []string
		for rs.Next() {
			db, _ := rs.String(1)
			name, _ := rs.String(2)
			schemas = append(schemas, db+"?schema="+name)
		}
		g.Expect(rs.Err()).ToNot(gomega.HaveOccurred())
		g.Expect(schemas).To(gomega.ConsistOf(SysDatabaseID + "?schema=" + CatalogConstant))
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_SchemaRebindOfALiteralCarrierChange pins, on real FDB, that a
// literal's carrier is part of an index's key, as Java's evolution validator
// reads it (LiteralKeyExpression.equals compares the literal's proto,
// LiteralKeyExpression.java:204-215): a long_value against an int_value of the
// same number is a changed key. Through CreateTemplate the new version carries
// the index as CHANGED, above the stored meta-data version, so the rebind
// admits it as an index rebuild; a version written past the build path, whose
// index keeps its last-modified version, is refused like a changed value, and
// the schema stays bound where it was.
func TestFDB_SchemaRebindOfALiteralCarrierChange(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	// version tags the template; lit is the literal of the index `add(V, lit)`.
	build := func(name string, version int, lit *gen.Value) api.SchemaTemplate {
		b := metadata.NewSchemaTemplateBuilder().SetName(name)
		b.AddTable("T", []metadata.ColumnSpec{
			metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
			metadata.NewColumnSpec("V", api.NewLongType(true), 2),
		}, []string{"ID"})
		tmpl, err := b.Build()
		g.Expect(err).ToNot(gomega.HaveOccurred())
		mdProto, err := tmpl.Underlying().ToProto()
		g.Expect(err).ToNot(gomega.HaveOccurred())
		mdProto.Indexes = append(mdProto.Indexes, &gen.Index{
			Name:                proto.String("VPLUS"),
			RecordType:          []string{"T"},
			AddedVersion:        proto.Int32(1),
			LastModifiedVersion: proto.Int32(1),
			RootExpression: &gen.KeyExpression{Function: &gen.Function{
				Name: proto.String("add"),
				Arguments: &gen.KeyExpression{Then: &gen.Then{Child: []*gen.KeyExpression{
					{Field: &gen.Field{FieldName: proto.String("V"), FanType: gen.Field_SCALAR.Enum()}},
					{Value: lit},
				}}},
			}},
		})
		md, err := recordlayer.RecordMetaDataFromProto(mdProto)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		out, err := metadata.NewRecordLayerSchemaTemplateWithVersion(name, md, version)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		return out
	}

	bound := func(schema string) int {
		var v int
		g.Expect(run(func(tx api.Transaction) error {
			s, err := cat.LoadSchema(tx, "/widendb", schema)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			v = s.SchemaTemplate().Version()
			return nil
		})).To(gomega.Succeed())
		return v
	}

	// Carried: v2 differs only in the carrier, and is CHANGED.
	carried1 := build("widen-carried", 1, &gen.Value{LongValue: proto.Int64(1)})
	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(tc.CreateTemplate(tx, carried1)).To(gomega.Succeed())
		return cat.SaveSchema(tx, carried1.GenerateSchema("/widendb", "carried"), true)
	})).To(gomega.Succeed())
	g.Expect(run(func(tx api.Transaction) error {
		return tc.CreateTemplate(tx, build("widen-carried", 2, &gen.Value{IntValue: proto.Int32(1)}))
	})).To(gomega.Succeed())
	g.Expect(run(func(tx api.Transaction) error {
		v1, err := tc.LoadTemplateProto(tx, "widen-carried", 1)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		v2, err := tc.LoadTemplateProto(tx, "widen-carried", 2)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		for _, idx := range v2.GetIndexes() {
			if idx.GetName() == "VPLUS" {
				g.Expect(idx.GetLastModifiedVersion()).To(gomega.BeNumerically(">", v1.GetVersion()), "CHANGED: rebuilt when a store opens")
				g.Expect(idx.GetRootExpression().GetFunction().GetArguments().GetThen().GetChild()[1].GetValue().IntValue).ToNot(gomega.BeNil())
			}
		}
		return nil
	})).To(gomega.Succeed())
	g.Expect(run(func(tx api.Transaction) error {
		return cat.RepairSchema(tx, "/widendb", "carried")
	})).To(gomega.Succeed())
	g.Expect(bound("carried")).To(gomega.Equal(2))

	// Written raw: v2 differs only in the carrier, v3 in the value, each with
	// the index's last-modified version unchanged: both refused, the binding
	// stays at v1.
	raw1 := build("widen-raw", 1, &gen.Value{LongValue: proto.Int64(1)})
	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(tc.CreateTemplate(tx, raw1)).To(gomega.Succeed())
		return cat.SaveSchema(tx, raw1.GenerateSchema("/widendb", "pub"), true)
	})).To(gomega.Succeed())
	for _, c := range []struct {
		version int
		lit     *gen.Value
	}{{2, &gen.Value{IntValue: proto.Int32(1)}}, {3, &gen.Value{LongValue: proto.Int64(2)}}} {
		g.Expect(run(func(tx api.Transaction) error {
			return rawCreateTemplate(t, cat, tx, build("widen-raw", c.version, c.lit))
		})).To(gomega.Succeed())
		rebindErr := run(func(tx api.Transaction) error {
			return cat.RepairSchema(tx, "/widendb", "pub")
		})
		g.Expect(rebindErr).To(gomega.HaveOccurred(), "version %d", c.version)
		g.Expect(rebindErr.Error()).To(gomega.ContainSubstring("key expression changed"), "version %d", c.version)
		g.Expect(bound("pub")).To(gomega.Equal(1), "version %d", c.version)
	}
}

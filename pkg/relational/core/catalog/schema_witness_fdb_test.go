package catalog

import (
	"bytes"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// witnessFixture is a catalog holding template w@1 and schema /FRL/db/s bound
// to it, and the witness of loading that schema.
func witnessFixture(t *testing.T) (*RecordLayerStoreCatalog, func(fn func(txn api.Transaction) error) error, *SchemaWitness) {
	t.Helper()
	cat, run := newFDBCatalogInSubspace(t)
	mustRun(t, run, func(tx api.Transaction) error {
		if err := cat.SchemaTemplateCatalog().CreateTemplate(tx, buildVersionedTemplate(t, "w", 1)); err != nil {
			return err
		}
		return cat.SaveSchema(tx, buildVersionedTemplate(t, "w", 1).GenerateSchema("/FRL/db", "s"), true, api.SchemaExistsError)
	})
	var w *SchemaWitness
	mustRun(t, run, func(tx api.Transaction) error {
		s, got, err := cat.LoadSchemaWitnessed(tx, "/FRL/db", "s")
		if err != nil {
			return err
		}
		if s.DatabaseName() != "/FRL/db" || s.MetadataName() != "s" ||
			s.SchemaTemplate().MetadataName() != "w" || s.SchemaTemplate().Version() != 1 {
			t.Fatalf("LoadSchemaWitnessed loaded %s/%s on %s@%d, want /FRL/db/s on w@1",
				s.DatabaseName(), s.MetadataName(), s.SchemaTemplate().MetadataName(), s.SchemaTemplate().Version())
		}
		if got == nil {
			t.Fatal("LoadSchemaWitnessed returned a schema without a witness")
		}
		w = got
		return nil
	})
	return cat, run, w
}

// schemaUnchanged runs SchemaUnchanged in a transaction of its own.
func schemaUnchanged(t *testing.T, cat *RecordLayerStoreCatalog, run func(fn func(txn api.Transaction) error) error, w *SchemaWitness) bool {
	t.Helper()
	var same bool
	mustRun(t, run, func(tx api.Transaction) error {
		var err error
		same, err = cat.SchemaUnchanged(tx, w)
		return err
	})
	return same
}

// templateRowBytes is the stored META_DATA of (name, version), nil when absent.
func templateRowBytes(t *testing.T, cat *RecordLayerStoreCatalog, run func(fn func(txn api.Transaction) error) error, name string, version int) []byte {
	t.Helper()
	var out []byte
	mustRun(t, run, func(tx api.Transaction) error {
		store, err := cat.openStore(tx)
		if err != nil {
			return err
		}
		rec, err := store.LoadRecord(templateKeyAtVersion(name, version))
		if err != nil || rec == nil {
			return err
		}
		out = rec.Record.(*gen.Templates).GetMETA_DATA()
		return nil
	})
	return out
}

// TestFDB_SchemaUnchanged is the witness's contract: it holds while the
// catalog holds the bytes the schema was loaded from, whatever else the
// catalog takes meanwhile, and fails on any change to the catalog store's
// header, the schema's row or its template version's row, a deletion
// included. Each change is made in its own committed transaction, as another
// connection (Go or Java) would make it.
func TestFDB_SchemaUnchanged(t *testing.T) {
	t.Parallel()

	t.Run("unchanged catalog", func(t *testing.T) {
		t.Parallel()
		cat, run, w := witnessFixture(t)
		if !schemaUnchanged(t, cat, run, w) {
			t.Fatal("SchemaUnchanged = false over the catalog the schema was just loaded from")
		}
		// Writes the schema's load never read: a new template version, a
		// second schema, a second database.
		mustRun(t, run, func(tx api.Transaction) error {
			if err := cat.SchemaTemplateCatalog().CreateTemplate(tx, buildVersionedTemplate(t, "w", 2)); err != nil {
				return err
			}
			if err := cat.SaveSchema(tx, buildVersionedTemplate(t, "w", 1).GenerateSchema("/FRL/db", "s2"), true, api.SchemaExistsError); err != nil {
				return err
			}
			return cat.CreateDatabase(tx, "/FRL/other")
		})
		if !schemaUnchanged(t, cat, run, w) {
			t.Fatal("SchemaUnchanged = false after writes to rows the schema's load did not read")
		}
		if v := boundVersion(t, cat, run, "/FRL/db", "s"); v != 1 {
			t.Fatalf("schema s bound to w@%d, want w@1: the unchanged case changed it", v)
		}
	})

	t.Run("catalog store header changed", func(t *testing.T) {
		t.Parallel()
		cat, run, w := witnessFixture(t)
		mustRun(t, run, func(tx api.Transaction) error {
			store, err := cat.openStore(tx)
			if err != nil {
				return err
			}
			return store.SetHeaderUserField("witness-test", []byte("changed"))
		})
		if schemaUnchanged(t, cat, run, w) {
			t.Fatal("SchemaUnchanged = true after the catalog store's header changed")
		}
	})

	t.Run("schema row rebound to a new template version", func(t *testing.T) {
		t.Parallel()
		cat, run, w := witnessFixture(t)
		mustRun(t, run, func(tx api.Transaction) error {
			if err := cat.SchemaTemplateCatalog().CreateTemplate(tx, buildVersionedTemplate(t, "w", 2)); err != nil {
				return err
			}
			return cat.RepairSchema(tx, "/FRL/db", "s")
		})
		if v := boundVersion(t, cat, run, "/FRL/db", "s"); v != 2 {
			t.Fatalf("RepairSchema left s on w@%d, want w@2", v)
		}
		if schemaUnchanged(t, cat, run, w) {
			t.Fatal("SchemaUnchanged = true after the schema row was rebound from w@1 to w@2")
		}
	})

	t.Run("schema row deleted", func(t *testing.T) {
		t.Parallel()
		cat, run, w := witnessFixture(t)
		mustRun(t, run, func(tx api.Transaction) error { return cat.DeleteSchema(tx, "/FRL/db", "s") })
		if schemaUnchanged(t, cat, run, w) {
			t.Fatal("SchemaUnchanged = true after the schema row was deleted")
		}
	})

	t.Run("template row rewritten in place", func(t *testing.T) {
		t.Parallel()
		cat, run, w := witnessFixture(t)
		before := templateRowBytes(t, cat, run, "w", 1)
		// A raw write past the version guard, as a restore or a Java writer
		// makes it: same (name, version), different META_DATA.
		md := demoMetaData(t, 1, withPriceIndex(1, 1, recordlayer.Field("price"), nil))
		mustRun(t, run, func(tx api.Transaction) error {
			store, err := cat.openStore(tx)
			if err != nil {
				return err
			}
			_, err = store.SaveRecord(&gen.Templates{
				TEMPLATE_NAME: proto.String("w"), TEMPLATE_VERSION: proto.Int32(1), META_DATA: md,
			})
			return err
		})
		if after := templateRowBytes(t, cat, run, "w", 1); bytes.Equal(before, after) {
			t.Fatal("the rewrite left w@1's META_DATA as it was: the case would test nothing")
		}
		if v := boundVersion(t, cat, run, "/FRL/db", "s"); v != 1 {
			t.Fatalf("schema s bound to w@%d, want w@1: only the template row may differ", v)
		}
		if schemaUnchanged(t, cat, run, w) {
			t.Fatal("SchemaUnchanged = true after w@1's row was rewritten in place")
		}
	})

	t.Run("template row deleted", func(t *testing.T) {
		t.Parallel()
		cat, run, w := witnessFixture(t)
		eraseTemplateRow(t, cat, run, "w", 1)
		if schemaUnchanged(t, cat, run, w) {
			t.Fatal("SchemaUnchanged = true after w@1's row was deleted")
		}
	})

	t.Run("nil witness", func(t *testing.T) {
		t.Parallel()
		cat, run, _ := witnessFixture(t)
		mustRun(t, run, func(tx api.Transaction) error {
			same, err := cat.SchemaUnchanged(tx, nil)
			if err != nil || same {
				t.Fatalf("SchemaUnchanged(nil) = %v, %v; want false, nil", same, err)
			}
			return nil
		})
	})
}

// TestFDB_LoadSchemaWitnessed_MissingSchema: a schema LoadSchema cannot load
// has no witness, and the error is LoadSchema's.
func TestFDB_LoadSchemaWitnessed_MissingSchema(t *testing.T) {
	t.Parallel()
	cat, run, _ := witnessFixture(t)
	var loadErr, witnessedErr error
	var w *SchemaWitness
	mustRun(t, run, func(tx api.Transaction) error {
		_, loadErr = cat.LoadSchema(tx, "/FRL/db", "nope")
		_, w, witnessedErr = cat.LoadSchemaWitnessed(tx, "/FRL/db", "nope")
		return nil
	})
	var ae *api.Error
	if !errors.As(witnessedErr, &ae) || ae.Code != api.ErrCodeUndefinedSchema {
		t.Fatalf("LoadSchemaWitnessed of a missing schema = %v, want %s", witnessedErr, api.ErrCodeUndefinedSchema)
	}
	if loadErr == nil || loadErr.Error() != witnessedErr.Error() {
		t.Fatalf("LoadSchemaWitnessed error %q, LoadSchema's %v", witnessedErr, loadErr)
	}
	if w != nil {
		t.Fatalf("a failed load returned a witness %+v", w)
	}
}

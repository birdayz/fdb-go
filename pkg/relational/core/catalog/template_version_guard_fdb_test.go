package catalog

import (
	"errors"
	"strconv"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// The version guard and the gone-version refusal (template_bindings.go) on the
// FDB-backed catalog, where the guard is a range read of TEMPLATES_VALUE_INDEX
// and so serializes against a concurrent bind.

// wantAPIError fails unless err is an *api.Error with exactly this code and
// message.
func wantAPIError(t *testing.T, err error, code api.ErrorCode, message string) {
	t.Helper()
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want %s %q", err, code, message)
	}
	if apiErr.Code != code || apiErr.Message != message {
		t.Fatalf("err = %s %q, want %s %q", apiErr.Code, apiErr.Message, code, message)
	}
}

// mustRun runs fn in its own transaction and fails the test on an error.
func mustRun(t *testing.T, run func(fn func(txn api.Transaction) error) error, fn func(txn api.Transaction) error) {
	t.Helper()
	if err := run(fn); err != nil {
		t.Fatal(err)
	}
}

// boundVersion is the template version the schema row binds.
func boundVersion(t *testing.T, cat *RecordLayerStoreCatalog, run func(fn func(txn api.Transaction) error) error, db, schema string) int {
	t.Helper()
	var v int
	mustRun(t, run, func(tx api.Transaction) error {
		s, err := cat.LoadSchema(tx, db, schema)
		if err != nil {
			return err
		}
		v = s.SchemaTemplate().Version()
		return nil
	})
	return v
}

// eraseTemplateRow deletes the template row (name, version) past the guard:
// the state a pre-guard DeleteTemplateVersion left behind, a schema bound to a
// version above the latest stored one.
func eraseTemplateRow(t *testing.T, cat *RecordLayerStoreCatalog, run func(fn func(txn api.Transaction) error) error, name string, version int) {
	t.Helper()
	mustRun(t, run, func(tx api.Transaction) error {
		store, err := cat.openStore(tx)
		if err != nil {
			return err
		}
		deleted, err := store.DeleteRecord(templateKeyAtVersion(name, version))
		if err != nil {
			return err
		}
		if !deleted {
			t.Fatalf("template row (%s, %d) was not stored", name, version)
		}
		return nil
	})
}

// A dropped template whose schemas still bind it cannot be created afresh, at
// the dropped version or any other, including version 0; the refusal names the
// first bound schema in index order. With the schemas dropped it is accepted.
func TestFDB_VersionGuard_FreshTemplateRefusedWhileDroppedVersionBound(t *testing.T) {
	t.Parallel()
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	mustRun(t, run, func(tx api.Transaction) error {
		if err := tc.CreateTemplate(tx, buildVersionedTemplate(t, "g", 3)); err != nil {
			return err
		}
		if err := cat.SaveSchema(tx, buildVersionedTemplate(t, "g", 3).GenerateSchema("/db2", "b"), true, api.SchemaExistsError); err != nil {
			return err
		}
		return cat.SaveSchema(tx, buildVersionedTemplate(t, "g", 3).GenerateSchema("/db1", "a"), true, api.SchemaExistsError)
	})
	// The target's DROP SCHEMA TEMPLATE drops regardless of bindings.
	mustRun(t, run, func(tx api.Transaction) error { return tc.DeleteTemplate(tx, "g", true) })

	for _, v := range []int{0, 1, 3, 7} {
		err := run(func(tx api.Transaction) error { return tc.CreateTemplate(tx, buildVersionedTemplate(t, "g", v)) })
		wantAPIError(t, err, api.ErrCodeInvalidSchemaTemplate,
			"schema template g version "+strconv.Itoa(v)+" cannot be created: schemas are still bound to its dropped version 3 (/db1/a)")
	}

	mustRun(t, run, func(tx api.Transaction) error { return cat.DeleteSchema(tx, "/db1", "a") })
	err := run(func(tx api.Transaction) error { return tc.CreateTemplate(tx, buildVersionedTemplate(t, "g", 1)) })
	wantAPIError(t, err, api.ErrCodeInvalidSchemaTemplate,
		"schema template g version 1 cannot be created: schemas are still bound to its dropped version 3 (/db2/b)")

	mustRun(t, run, func(tx api.Transaction) error { return cat.DeleteSchema(tx, "/db2", "b") })
	mustRun(t, run, func(tx api.Transaction) error { return tc.CreateTemplate(tx, buildVersionedTemplate(t, "g", 1)) })
}

// The bindings are read from TEMPLATES_VALUE_INDEX, which holds none or only
// some of them unless it is READABLE: with the index write-only, the guard of a
// template save and of a version delete fails closed instead of answering "no
// schema binds it".
func TestFDB_VersionGuard_FailsClosedOverAnUnreadableIndex(t *testing.T) {
	t.Parallel()
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()
	mustRun(t, run, func(tx api.Transaction) error {
		if err := tc.CreateTemplate(tx, buildVersionedTemplate(t, "u", 1)); err != nil {
			return err
		}
		return cat.SaveSchema(tx, buildVersionedTemplate(t, "u", 1).GenerateSchema("/db", "s"), true, api.SchemaExistsError)
	})
	mustRun(t, run, func(tx api.Transaction) error {
		store, err := cat.openStore(tx)
		if err != nil {
			return err
		}
		_, err = store.MarkIndexWriteOnly(IdxTemplatesValue)
		return err
	})
	const want = "catalog index " + IdxTemplatesValue + " is WRITE_ONLY, so the schemas bound to template u cannot be read"
	wantAPIError(t, run(func(tx api.Transaction) error { return tc.CreateTemplate(tx, buildVersionedTemplate(t, "u", 2)) }),
		api.ErrCodeInternalError, want)
	wantAPIError(t, run(func(tx api.Transaction) error { return tc.DeleteTemplateVersion(tx, "u", 1, true) }),
		api.ErrCodeInternalError, want)
}

// A binding of version 0 is a binding: the fresh-template guard reads the
// whole name, not from version 1.
func TestFDB_VersionGuard_VersionZeroBindingBlocksFreshTemplate(t *testing.T) {
	t.Parallel()
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	mustRun(t, run, func(tx api.Transaction) error {
		if err := tc.CreateTemplate(tx, buildVersionedTemplate(t, "z", 0)); err != nil {
			return err
		}
		return cat.SaveSchema(tx, buildVersionedTemplate(t, "z", 0).GenerateSchema("/db", "s"), true, api.SchemaExistsError)
	})
	mustRun(t, run, func(tx api.Transaction) error { return tc.DeleteTemplate(tx, "z", true) })
	err := run(func(tx api.Transaction) error { return tc.CreateTemplate(tx, buildVersionedTemplate(t, "z", 1)) })
	wantAPIError(t, err, api.ErrCodeInvalidSchemaTemplate,
		"schema template z version 1 cannot be created: schemas are still bound to its dropped version 0 (/db/s)")
}

// A schema bound to the latest or a lower stored version does not block a new
// version, and stays bound where it was.
func TestFDB_VersionGuard_BoundLowerVersionDoesNotBlockNewVersion(t *testing.T) {
	t.Parallel()
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	mustRun(t, run, func(tx api.Transaction) error {
		if err := tc.CreateTemplate(tx, buildVersionedTemplate(t, "n", 1)); err != nil {
			return err
		}
		return cat.SaveSchema(tx, buildVersionedTemplate(t, "n", 1).GenerateSchema("/db", "s"), true, api.SchemaExistsError)
	})
	mustRun(t, run, func(tx api.Transaction) error { return tc.CreateTemplate(tx, buildVersionedTemplate(t, "n", 2)) })
	mustRun(t, run, func(tx api.Transaction) error { return tc.CreateTemplate(tx, buildVersionedTemplate(t, "n", 5)) })
	if v := boundVersion(t, cat, run, "/db", "s"); v != 1 {
		t.Fatalf("schema bound to version %d, want 1", v)
	}
}

// A binding of a dropped version ABOVE the latest stored one refuses every new
// version, since a new version at or below it would put a second numbering
// history under the name. The gone-version refusal answers the bound schema's
// loads with Java's text; dropping that schema admits the new version.
func TestFDB_VersionGuard_DanglingBindingAboveLatestRefusesNewVersion(t *testing.T) {
	t.Parallel()
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	mustRun(t, run, func(tx api.Transaction) error {
		for _, v := range []int{1, 3} {
			if err := tc.CreateTemplate(tx, buildVersionedTemplate(t, "d", v)); err != nil {
				return err
			}
		}
		if err := cat.SaveSchema(tx, buildVersionedTemplate(t, "d", 1).GenerateSchema("/db", "low"), true, api.SchemaExistsError); err != nil {
			return err
		}
		return cat.SaveSchema(tx, buildVersionedTemplate(t, "d", 3).GenerateSchema("/db", "high"), true, api.SchemaExistsError)
	})
	eraseTemplateRow(t, cat, run, "d", 3)

	for _, v := range []int{2, 3, 4} {
		err := run(func(tx api.Transaction) error { return tc.CreateTemplate(tx, buildVersionedTemplate(t, "d", v)) })
		wantAPIError(t, err, api.ErrCodeInvalidSchemaTemplate,
			"schema template d version "+strconv.Itoa(v)+" cannot be created: schemas are still bound to its dropped version 3 (/db/high)")
	}

	gone := "SchemaTemplate=d, version=3 is not in catalog"
	err := run(func(tx api.Transaction) error { _, err := cat.LoadSchema(tx, "/db", "high"); return err })
	wantAPIError(t, err, api.ErrCodeUnknownSchemaTemplate, gone)
	err = run(func(tx api.Transaction) error { return cat.RepairSchema(tx, "/db", "high") })
	wantAPIError(t, err, api.ErrCodeUnknownSchemaTemplate, gone)
	if v := boundVersion(t, cat, run, "/db", "low"); v != 1 {
		t.Fatalf("schema low bound to version %d, want 1", v)
	}

	mustRun(t, run, func(tx api.Transaction) error { return cat.DeleteSchema(tx, "/db", "high") })
	mustRun(t, run, func(tx api.Transaction) error { return tc.CreateTemplate(tx, buildVersionedTemplate(t, "d", 2)) })
}

// SaveSchema and RepairSchema over a row whose bound version is gone are
// refused with Java's text, as Java's saveSchema refuses them when it loads the
// existing row with its template: a rebind has no metadata to validate against.
func TestFDB_VersionGuard_SaveAndRepairOverGoneVersionRefused(t *testing.T) {
	t.Parallel()
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	mustRun(t, run, func(tx api.Transaction) error {
		if err := tc.CreateTemplate(tx, buildVersionedTemplate(t, "old", 1)); err != nil {
			return err
		}
		if err := tc.CreateTemplate(tx, buildVersionedTemplate(t, "other", 1)); err != nil {
			return err
		}
		return cat.SaveSchema(tx, buildVersionedTemplate(t, "old", 1).GenerateSchema("/db", "s"), true, api.SchemaExistsError)
	})
	mustRun(t, run, func(tx api.Transaction) error { return tc.DeleteTemplate(tx, "old", true) })

	gone := "SchemaTemplate=old, version=1 is not in catalog"
	err := run(func(tx api.Transaction) error {
		return cat.SaveSchema(tx, buildVersionedTemplate(t, "other", 1).GenerateSchema("/db", "s"), false, api.SchemaExistsError)
	})
	wantAPIError(t, err, api.ErrCodeUnknownSchemaTemplate, gone)
	err = run(func(tx api.Transaction) error { return cat.RepairSchema(tx, "/db", "s") })
	wantAPIError(t, err, api.ErrCodeUnknownSchemaTemplate, gone)
	// The row is untouched: still bound to the gone version.
	err = run(func(tx api.Transaction) error { _, err := cat.LoadSchema(tx, "/db", "s"); return err })
	wantAPIError(t, err, api.ErrCodeUnknownSchemaTemplate, gone)
}

// SaveSchema's template-existence and database texts are Java's
// (RecordLayerStoreCatalog.saveSchema).
func TestFDB_SaveSchemaMissingTemplateAndDatabaseTexts(t *testing.T) {
	t.Parallel()
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()
	mustRun(t, run, func(tx api.Transaction) error { return tc.CreateTemplate(tx, buildVersionedTemplate(t, "tx", 1)) })

	err := run(func(tx api.Transaction) error {
		return cat.SaveSchema(tx, buildVersionedTemplate(t, "tx", 2).GenerateSchema("/db", "s"), true, api.SchemaExistsError)
	})
	wantAPIError(t, err, api.ErrCodeUnknownSchemaTemplate,
		"Cannot create schema s because schema template tx version 2 does not exist.")
	err = run(func(tx api.Transaction) error {
		return cat.SaveSchema(tx, buildVersionedTemplate(t, "tx", 1).GenerateSchema("/nodb", "s"), false, api.SchemaExistsError)
	})
	wantAPIError(t, err, api.ErrCodeUndefinedDatabase,
		"Cannot create schema s because database /nodb does not exist.")
}

// DeleteTemplateVersion refuses a version a schema binds, naming the schema,
// and leaves the version stored; an unbound version, or the bound one once its
// schema is dropped, is deleted.
func TestFDB_VersionGuard_DeleteBoundVersionRefused(t *testing.T) {
	t.Parallel()
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	mustRun(t, run, func(tx api.Transaction) error {
		for _, v := range []int{1, 2} {
			if err := tc.CreateTemplate(tx, buildVersionedTemplate(t, "del", v)); err != nil {
				return err
			}
		}
		return cat.SaveSchema(tx, buildVersionedTemplate(t, "del", 2).GenerateSchema("/db", "s"), true, api.SchemaExistsError)
	})

	err := run(func(tx api.Transaction) error { return tc.DeleteTemplateVersion(tx, "del", 2, true) })
	wantAPIError(t, err, api.ErrCodeInvalidSchemaTemplate,
		"schema template del version 2 cannot be deleted: schemas are still bound to it (/db/s)")
	mustRun(t, run, func(tx api.Transaction) error {
		ok, err := tc.DoesSchemaTemplateExistAtVersion(tx, "del", 2)
		if err == nil && !ok {
			t.Fatal("refused delete removed version 2")
		}
		return err
	})

	mustRun(t, run, func(tx api.Transaction) error { return tc.DeleteTemplateVersion(tx, "del", 1, true) })
	mustRun(t, run, func(tx api.Transaction) error { return cat.DeleteSchema(tx, "/db", "s") })
	mustRun(t, run, func(tx api.Transaction) error { return tc.DeleteTemplateVersion(tx, "del", 2, true) })
}

// openRaced opens a catalog transaction the test commits by hand, so two of
// them can read before either commits.
func openRaced(t *testing.T) (*FDBTransaction, func() error) {
	t.Helper()
	tr, err := testFDB.CreateWritableTransaction()
	if err != nil {
		t.Fatal(err)
	}
	rc := testFDB.NewRecordContext(tr)
	return NewFDBTransaction(rc), rc.Commit
}

func wantNotCommitted(t *testing.T, err error) {
	t.Helper()
	var fe fdb.Error
	if !errors.As(err, &fe) || fe.Code != 1020 {
		t.Fatalf("commit err = %v, want not_committed (1020)", err)
	}
}

// DeleteTemplateVersion and a SaveSchema binding the same version, both read
// before either commits: whichever commits second conflicts, the delete on its
// read of the binding range, the bind on its read of the template row, and its
// retry sees the winner. Either way no schema is left bound to a deleted
// version. Deleting a DIFFERENT, unbound version beside the bind commits both,
// which shows the conflicts come from those reads and not from opening the
// catalog store.
func TestFDB_VersionGuard_DeleteRacesBind(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*RecordLayerStoreCatalog, func(fn func(txn api.Transaction) error) error) {
		cat, run := newFDBCatalogInSubspace(t)
		mustRun(t, run, func(tx api.Transaction) error {
			for _, v := range []int{1, 2} {
				if err := cat.SchemaTemplateCatalog().CreateTemplate(tx, buildVersionedTemplate(t, "race", v)); err != nil {
					return err
				}
			}
			return cat.CreateDatabase(tx, "/db")
		})
		return cat, run
	}
	bindV2 := func(cat *RecordLayerStoreCatalog, tx api.Transaction) error {
		return cat.SaveSchema(tx, buildVersionedTemplate(t, "race", 2).GenerateSchema("/db", "s"), false, api.SchemaExistsError)
	}
	stage := func(t *testing.T, cat *RecordLayerStoreCatalog, deleteVersion int) (func() error, func() error) {
		delTx, delCommit := openRaced(t)
		bindTx, bindCommit := openRaced(t)
		if err := cat.SchemaTemplateCatalog().DeleteTemplateVersion(delTx, "race", deleteVersion, true); err != nil {
			t.Fatal(err)
		}
		if err := bindV2(cat, bindTx); err != nil {
			t.Fatal(err)
		}
		return delCommit, bindCommit
	}

	t.Run("delete commits first", func(t *testing.T) {
		t.Parallel()
		cat, run := setup(t)
		delCommit, bindCommit := stage(t, cat, 2)
		if err := delCommit(); err != nil {
			t.Fatal(err)
		}
		wantNotCommitted(t, bindCommit())
		err := run(func(tx api.Transaction) error { return bindV2(cat, tx) })
		wantAPIError(t, err, api.ErrCodeUnknownSchemaTemplate,
			"Cannot create schema s because schema template race version 2 does not exist.")
	})

	t.Run("bind commits first", func(t *testing.T) {
		t.Parallel()
		cat, run := setup(t)
		delCommit, bindCommit := stage(t, cat, 2)
		if err := bindCommit(); err != nil {
			t.Fatal(err)
		}
		wantNotCommitted(t, delCommit())
		err := run(func(tx api.Transaction) error {
			return cat.SchemaTemplateCatalog().DeleteTemplateVersion(tx, "race", 2, true)
		})
		wantAPIError(t, err, api.ErrCodeInvalidSchemaTemplate,
			"schema template race version 2 cannot be deleted: schemas are still bound to it (/db/s)")
		if v := boundVersion(t, cat, run, "/db", "s"); v != 2 {
			t.Fatalf("schema bound to version %d, want 2", v)
		}
	})

	t.Run("unrelated version commits both", func(t *testing.T) {
		t.Parallel()
		cat, run := setup(t)
		delCommit, bindCommit := stage(t, cat, 1)
		if err := bindCommit(); err != nil {
			t.Fatal(err)
		}
		if err := delCommit(); err != nil {
			t.Fatalf("delete of unbound version 1 beside a bind of version 2: %v", err)
		}
		if v := boundVersion(t, cat, run, "/db", "s"); v != 2 {
			t.Fatalf("schema bound to version %d, want 2", v)
		}
	})
}

// A guard that read a binding a concurrent DROP SCHEMA then removes refuses
// once (its read predates the drop, and a refused write commits nothing, so
// there is no conflict to report), and the retry is accepted.
func TestFDB_VersionGuard_ConcurrentDropSchemaConverges(t *testing.T) {
	t.Parallel()
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()
	mustRun(t, run, func(tx api.Transaction) error {
		if err := tc.CreateTemplate(tx, buildVersionedTemplate(t, "c", 1)); err != nil {
			return err
		}
		return cat.SaveSchema(tx, buildVersionedTemplate(t, "c", 1).GenerateSchema("/db", "s"), true, api.SchemaExistsError)
	})
	mustRun(t, run, func(tx api.Transaction) error { return tc.DeleteTemplate(tx, "c", true) })

	createTx, _ := openRaced(t)
	dropTx, dropCommit := openRaced(t)
	// Fix the create's read version before the drop commits.
	if _, err := tc.DoesSchemaTemplateExist(createTx, "c"); err != nil {
		t.Fatal(err)
	}
	if err := cat.DeleteSchema(dropTx, "/db", "s"); err != nil {
		t.Fatal(err)
	}
	if err := dropCommit(); err != nil {
		t.Fatal(err)
	}
	err := tc.CreateTemplate(createTx, buildVersionedTemplate(t, "c", 1))
	wantAPIError(t, err, api.ErrCodeInvalidSchemaTemplate,
		"schema template c version 1 cannot be created: schemas are still bound to its dropped version 1 (/db/s)")
	_ = createTx.Abort()

	mustRun(t, run, func(tx api.Transaction) error { return tc.CreateTemplate(tx, buildVersionedTemplate(t, "c", 1)) })
}

// LoadTemplateProto returns the row's META_DATA as stored: a deprecated
// value_expression and an unknown field on an Index survive, where a load
// through the metadata loader folds the one and drops the other.
func TestFDB_LoadTemplateProtoReturnsTheStoredBytes(t *testing.T) {
	t.Parallel()
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	md, err := buildVersionedTemplate(t, "raw", 1).(*metadata.RecordLayerSchemaTemplate).Underlying().ToProto()
	if err != nil {
		t.Fatal(err)
	}
	ix := &gen.Index{
		Name:            proto.String("legacy_ix"),
		RecordType:      []string{"Order"},
		RootExpression:  recordlayer.Field("order_id").ToKeyExpression(),
		ValueExpression: recordlayer.Field("customer_id").ToKeyExpression(),
		AddedVersion:    proto.Int32(1), LastModifiedVersion: proto.Int32(1),
	}
	ix.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 50, protowire.VarintType), 7))
	md.Indexes = append(md.Indexes, ix)
	payload, err := proto.Marshal(md)
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, run, func(tx api.Transaction) error {
		store, err := cat.openStore(tx)
		if err != nil {
			return err
		}
		_, err = store.SaveRecord(&gen.Templates{
			TEMPLATE_NAME: proto.String("raw"), TEMPLATE_VERSION: proto.Int32(1), META_DATA: payload,
		})
		return err
	})

	mustRun(t, run, func(tx api.Transaction) error {
		got, err := tc.LoadTemplateProto(tx, "raw", 1)
		if err != nil {
			return err
		}
		if !proto.Equal(got, md) {
			t.Fatalf("LoadTemplateProto differs from the stored MetaData")
		}
		var stored *gen.Index
		for _, i := range got.GetIndexes() {
			if i.GetName() == "legacy_ix" {
				stored = i
			}
		}
		if stored == nil || len(stored.ProtoReflect().GetUnknown()) == 0 ||
			!stored.ProtoReflect().Has(stored.ProtoReflect().Descriptor().Fields().ByName("value_expression")) {
			t.Fatalf("stored index lost its value_expression or unknown field: %v", stored)
		}
		return nil
	})
	err = run(func(tx api.Transaction) error { _, err := tc.LoadTemplateProto(tx, "raw", 2); return err })
	wantAPIError(t, err, api.ErrCodeUnknownSchemaTemplate, "SchemaTemplate=raw, version=2 is not in catalog")
}

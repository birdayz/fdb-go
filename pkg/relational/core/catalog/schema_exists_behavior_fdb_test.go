package catalog

import (
	"testing"

	"fdb.dev/pkg/relational/api"
)

// A save that its SchemaExistsBehavior makes a no-op writes nothing, so it adds
// no write conflict range (RecordLayerStoreCatalog.java:250-256): the no-op
// saver reads the stored row, another transaction rewrites that row and commits
// first, and the no-op saver still commits. The control is a save that writes:
// the same interleaving conflicts, which shows the no-op's commit comes from
// its empty write set and not from the reads missing each other.
func TestFDB_SaveSchema_ANoOpWritesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		behavior api.SchemaExistsBehavior
		save     int
		writes   bool
	}{
		{"ERROR_IF_DIFFERENT over the same version", api.SchemaExistsErrorIfDifferent, 2, false},
		{"DO_NOTHING over another version", api.SchemaExistsDoNothing, 1, false},
		{"UPGRADE over the same version", api.SchemaExistsUpgrade, 2, false},
		{"UPGRADE to a higher version writes", api.SchemaExistsUpgrade, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cat, run := newFDBCatalogInSubspace(t)
			tpl := cat.SchemaTemplateCatalog()
			stored := func(tx api.Transaction, v int) api.Schema {
				s, err := tpl.LoadSchemaTemplateAtVersion(tx, "n", v)
				if err != nil {
					t.Fatal(err)
				}
				return s.GenerateSchema("/db", "s")
			}
			mustRun(t, run, func(tx api.Transaction) error {
				for v := 1; v <= 3; v++ {
					if err := tpl.CreateTemplate(tx, buildVersionedTemplate(t, "n", v)); err != nil {
						return err
					}
				}
				return nil
			})
			mustRun(t, run, func(tx api.Transaction) error {
				return cat.SaveSchema(tx, stored(tx, 2), true, api.SchemaExistsError)
			})

			a, aCommit := openRaced(t)
			if err := cat.SaveSchema(a, stored(a, tc.save), false, tc.behavior); err != nil {
				t.Fatal(err)
			}
			mustRun(t, run, func(tx api.Transaction) error {
				return cat.SaveSchema(tx, stored(tx, 3), false, api.SchemaExistsUpgrade)
			})
			err := aCommit()
			if tc.writes {
				wantNotCommitted(t, err)
				return
			}
			if err != nil {
				t.Fatalf("the no-op save's commit: %v", err)
			}
		})
	}
}

// Initialize over an initialized catalog is a no-op (ERROR_IF_DIFFERENT over the
// identical row), so two concurrent ones both commit, as Java's comment at
// RecordLayerStoreCatalog.java:184-188 intends.
func TestFDB_Initialize_ConcurrentOverAnInitializedCatalogBothCommit(t *testing.T) {
	t.Parallel()
	cat, run := newFDBCatalogInSubspace(t)
	mustRun(t, run, cat.Initialize)
	a, aCommit := openRaced(t)
	b, bCommit := openRaced(t)
	if err := cat.Initialize(a); err != nil {
		t.Fatal(err)
	}
	if err := cat.Initialize(b); err != nil {
		t.Fatal(err)
	}
	if err := aCommit(); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := bCommit(); err != nil {
		t.Fatalf("second: %v", err)
	}
}

// A first-time initialization writes the template, database and schema rows, in
// Java too (RecordLayerStoreCatalog.java:180-189), so two concurrent ones
// conflict: the second commit fails with not_committed once, and its retry finds
// the catalog initialized and commits.
func TestFDB_Initialize_FirstTimePairConflictsOnceAndConverges(t *testing.T) {
	t.Parallel()
	cat, run := newFDBCatalogInSubspace(t)
	a, aCommit := openRaced(t)
	b, bCommit := openRaced(t)
	if err := cat.Initialize(a); err != nil {
		t.Fatal(err)
	}
	if err := cat.Initialize(b); err != nil {
		t.Fatal(err)
	}
	if err := aCommit(); err != nil {
		t.Fatalf("first: %v", err)
	}
	wantNotCommitted(t, bCommit())
	mustRun(t, run, cat.Initialize)
	mustRun(t, run, func(tx api.Transaction) error {
		s, err := cat.LoadSchema(tx, SysDatabaseID, CatalogConstant)
		if err != nil {
			return err
		}
		if s.SchemaTemplate().MetadataName() != CatalogTemplateName || s.SchemaTemplate().Version() != CatalogTemplateVersion {
			t.Fatalf("the catalog schema binds %s@%d", s.SchemaTemplate().MetadataName(), s.SchemaTemplate().Version())
		}
		return nil
	})
}

// Initialize saves the catalog schema with ERROR_IF_DIFFERENT: a catalog row
// bound to another template is refused with Java's text, not overwritten.
func TestFDB_Initialize_RefusesACatalogRowOfAnotherTemplate(t *testing.T) {
	t.Parallel()
	cat, run := newFDBCatalogInSubspace(t)
	other := buildVersionedTemplate(t, "other", 1)
	mustRun(t, run, func(tx api.Transaction) error {
		if err := cat.SchemaTemplateCatalog().CreateTemplate(tx, other); err != nil {
			return err
		}
		return cat.SaveSchema(tx, other.GenerateSchema(SysDatabaseID, CatalogConstant), true, api.SchemaExistsError)
	})
	wantAPIError(t, run(cat.Initialize), api.ErrCodeSchemaAlreadyExists,
		"Schema /__SYS/CATALOG already exists with a different template (other@1 vs CATALOG_TEMPLATE@1).")
}

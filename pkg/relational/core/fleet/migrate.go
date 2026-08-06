package fleet

import (
	"context"
	"fmt"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/keyspace"
)

// NextTemplateVersion returns one past the latest stored version of
// templateName, i.e. the version a new template must carry to be accepted.
// Returns 1 when the template does not exist yet.
func NextTemplateVersion(ctx context.Context, db *recordlayer.FDBDatabase, cat api.StoreCatalog, templateName string) (int, error) {
	next := 1
	err := inCatalogTx(ctx, db, func(txn api.Transaction) error {
		exists, err := cat.SchemaTemplateCatalog().DoesSchemaTemplateExist(txn, templateName)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		cur, err := cat.SchemaTemplateCatalog().LoadSchemaTemplate(txn, templateName)
		if err != nil {
			return err
		}
		next = cur.Version() + 1
		return nil
	})
	if err != nil {
		return 0, err
	}
	return next, nil
}

// SaveTemplate persists tmpl as a new template version in ONE catalog
// transaction.
//
// It goes through SaveSchemaTemplateConstantAction rather than the template
// catalog's CreateTemplate directly, because only the constant action applies
// the version-monotonicity gate and the metadata-evolution validator;
// CreateTemplate deliberately applies neither and would happily store a
// template that no schema could then legally rebind onto.
//
// This is the ONE step of a migration that is legitimately fleet-wide and
// atomic: it writes a single catalog row, independent of tenant count.
func SaveTemplate(ctx context.Context, db *recordlayer.FDBDatabase, cat api.StoreCatalog, tmpl api.SchemaTemplate) error {
	return inCatalogTx(ctx, db, func(txn api.Transaction) error {
		return ddl.NewSaveSchemaTemplateConstantAction(tmpl, cat.SchemaTemplateCatalog()).Execute(txn)
	})
}

// Migrate rebinds every target onto template version targetVersion, ONE
// TRANSACTION PER SCHEMA.
//
// Targets already at or above targetVersion are skipped without opening a
// transaction — that skip is the resume mechanism, and it is reported as
// OutcomeSkipped so a caller can observe that a re-run really did less work
// rather than silently repeating it.
//
// A rebind that commits without advancing the bound version is treated as a
// FAILURE, not a success: RepairSchema rebinds to whatever the catalog's
// latest version happens to be, so if the template save did not land, every
// tenant would otherwise report "migrated" while still pinned to the old
// metadata. The landed version is read back inside the same transaction and
// asserted.
//
// targetVersion is a THRESHOLD, not a destination. RepairSchema has no
// "rebind to version N" form — it always moves to the catalog's latest — so
// targetVersion decides only which tenants are already done (skipped) and how
// far a rebind must have got to count as success. Passing a targetVersion
// below the latest stored version will rebind tenants PAST it.
func Migrate(
	ctx context.Context,
	db *recordlayer.FDBDatabase,
	cat api.StoreCatalog,
	ks *keyspace.RelationalKeyspace,
	targets []Target,
	targetVersion int,
	opts Options,
) (Result, error) {
	return fanOut(ctx, ks, targets, opts, func(ctx context.Context, t Target) (Event, error) {
		if t.TemplateVersion >= targetVersion {
			return Event{
				Outcome:     OutcomeSkipped,
				FromVersion: t.TemplateVersion,
				ToVersion:   t.TemplateVersion,
			}, nil
		}
		landed := 0
		err := inCatalogTx(ctx, db, func(txn api.Transaction) error {
			if err := cat.RepairSchema(txn, t.DatabaseID, t.SchemaName); err != nil {
				return err
			}
			s, err := cat.LoadSchema(txn, t.DatabaseID, t.SchemaName)
			if err != nil {
				return fmt.Errorf("read back rebound schema: %w", err)
			}
			landed = s.SchemaTemplate().Version()
			if landed < targetVersion {
				return fmt.Errorf(
					"rebind did not advance %s: still bound to %s@%d, wanted >= %d "+
						"(is the new template version actually saved?)",
					t, s.SchemaTemplate().MetadataName(), landed, targetVersion)
			}
			return nil
		})
		if err != nil {
			return Event{}, err
		}
		return Event{
			Outcome:     OutcomeMigrated,
			FromVersion: t.TemplateVersion,
			ToVersion:   landed,
		}, nil
	})
}

// MigrateTemplate is the whole migration in one call: save tmpl as a new
// version (one catalog transaction), then rebind every schema bound to that
// template name (one transaction each).
//
// databaseID narrows the fan-out to a single database; empty means every
// database on the cluster.
//
// The whole call is resumable. A template version that is ALREADY stored is
// not re-saved: the save is a create, and re-running a migration that died
// partway through the fan-out would otherwise fail on the template step and
// never reach the tenants still owed a rebind. Skipping the save and going
// straight to the fan-out is what makes "run it again" the recovery
// procedure.
func MigrateTemplate(
	ctx context.Context,
	db *recordlayer.FDBDatabase,
	cat api.StoreCatalog,
	ks *keyspace.RelationalKeyspace,
	databaseID string,
	tmpl api.SchemaTemplate,
	opts Options,
) (Result, error) {
	saved, err := templateVersionExists(ctx, db, cat, tmpl.MetadataName(), tmpl.Version())
	if err != nil {
		return Result{}, err
	}
	if !saved {
		if err := SaveTemplate(ctx, db, cat, tmpl); err != nil {
			return Result{}, fmt.Errorf("save template %s@%d: %w", tmpl.MetadataName(), tmpl.Version(), err)
		}
	}
	targets, err := ListTargets(ctx, db, cat, databaseID)
	if err != nil {
		return Result{}, fmt.Errorf("list fan-out targets: %w", err)
	}
	return Migrate(ctx, db, cat, ks, FilterByTemplate(targets, tmpl.MetadataName()), tmpl.Version(), opts)
}

// templateVersionExists reports whether (name, version) is already stored.
func templateVersionExists(ctx context.Context, db *recordlayer.FDBDatabase, cat api.StoreCatalog, name string, version int) (bool, error) {
	exists := false
	err := inCatalogTx(ctx, db, func(txn api.Transaction) error {
		got, err := cat.SchemaTemplateCatalog().DoesSchemaTemplateExistAtVersion(txn, name, version)
		if err != nil {
			return err
		}
		exists = got
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("check template %s@%d: %w", name, version, err)
	}
	return exists, nil
}

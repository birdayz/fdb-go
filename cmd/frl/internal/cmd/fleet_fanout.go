package cmd

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/fleet"
)

// Fleet fan-out: the multi-tenant half of the index/migration write surface.
//
// Every other store command in frl addresses exactly ONE store via
// --database + --schema. Rolling a new index or a new template version across
// a tenant fleet was therefore N invocations of an N-tenant loop the operator
// had to write. These paths do it in one, and they do it the way a fleet job
// has to: one transaction per tenant, per-tenant failure isolation, and a
// resumable pass rather than one giant transaction.

// fleetProgressPrinter renders per-tenant progress on stderr, leaving stdout
// for the machine-usable summary. Calls from the fan-out are already
// serialised, so no locking is needed here.
func fleetProgressPrinter(cmd *cobra.Command) func(fleet.Event) {
	return func(ev fleet.Event) {
		out := cmd.ErrOrStderr()
		switch ev.Outcome {
		case fleet.OutcomeBuilt:
			fmt.Fprintf(out, "%s: built %v (%d records scanned)\n", ev.Target, ev.Indexes, ev.Records)
		case fleet.OutcomeNoWork:
			fmt.Fprintf(out, "%s: nothing to build\n", ev.Target)
		case fleet.OutcomeMigrated:
			fmt.Fprintf(out, "%s: rebound %d -> %d\n", ev.Target, ev.FromVersion, ev.ToVersion)
		case fleet.OutcomeSkipped:
			fmt.Fprintf(out, "%s: already at version %d, skipped\n", ev.Target, ev.FromVersion)
		case fleet.OutcomeRefused:
			fmt.Fprintf(out, "%s: REFUSED: %v\n", ev.Target, ev.Err)
		case fleet.OutcomeFailed:
			fmt.Fprintf(out, "%s: FAILED: %v\n", ev.Target, ev.Err)
		}
	}
}

// fleetSummary is the stdout line: one tally an operator can grep or diff
// between passes to see a fleet converging.
func fleetSummary(cmd *cobra.Command, res fleet.Result) {
	fmt.Fprintf(cmd.OutOrStdout(),
		"targets=%d migrated=%d skipped=%d built=%d no-work=%d failed=%d refused=%d\n",
		res.Total, res.Migrated, res.Skipped, res.Built, res.NoWork, res.Failed, res.Refused)
}

// fleetOpen dials FDB and opens the relational catalog the sqldriver writes
// under — the same place `meta catalog` reads from.
func fleetOpen(clusterFile string) (*recordlayer.FDBDatabase, *catalog.RecordLayerStoreCatalog, error) {
	db, err := openDatabase(clusterFile)
	if err != nil {
		return nil, nil, err
	}
	cat, err := catalog.NewRecordLayerStoreCatalog(relationalKeyspace().CatalogSubspace())
	if err != nil {
		return nil, nil, fmt.Errorf("open relational catalog: %w", err)
	}
	return recordlayer.NewFDBDatabase(db), cat, nil
}

// fleetDescribeTargets lists what a fan-out would touch, for the confirmation
// prompt. An operator confirming a fleet write should see the tenant count
// before it happens, not after.
func fleetDescribeTargets(targets []fleet.Target) string {
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.SchemaName)
	}
	sort.Strings(names)
	if len(names) > 6 {
		return fmt.Sprintf("%d schemas (%v …)", len(names), names[:6])
	}
	return fmt.Sprintf("%d schemas (%v)", len(names), names)
}

// runFleetIndexBuild is `index build --all-schemas`: build the pending indexes
// of every schema in one database.
//
// indexNames empty means "everything each tenant owes"; a name restricts the
// roll-out. A tenant that does not carry the named index is NOT an error —
// mid-migration a fleet legitimately straddles template versions.
func runFleetIndexBuild(
	cmd *cobra.Command,
	addr *storeAddressFlags,
	databaseID string,
	indexNames []string,
	yes bool,
	opts indexBuildOptions,
	concurrency int,
) error {
	// Resolve the CLUSTER only. storeAddressFlags.validate insists on
	// --database and --schema as a pair because it addresses one store; a
	// fan-out deliberately has no single schema, so it resolves the context
	// the same way the catalog commands do.
	cf, err := resolveCatalogContext(addr.contextName, addr.clusterFile)
	if err != nil {
		return err
	}
	db, cat, err := fleetOpen(cf)
	if err != nil {
		return err
	}
	ctx := cmd.Context()

	targets, err := fleet.ListTargets(ctx, db, cat, databaseID)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("no schemas found in database %q", databaseID)
	}
	what := fmt.Sprintf("build indexes across %s in %s", fleetDescribeTargets(targets), databaseID)
	if len(indexNames) > 0 {
		what = fmt.Sprintf("build %v across %s in %s", indexNames, fleetDescribeTargets(targets), databaseID)
	}
	if err := confirmWrite(cmd, yes, what); err != nil {
		return err
	}

	res, runErr := fleet.BuildIndexes(ctx, db, cat, relationalKeyspace(), targets, fleet.BuildOptions{
		Options: fleet.Options{
			Concurrency: concurrency,
			Progress:    fleetProgressPrinter(cmd),
		},
		Limit:            opts.limit,
		RecordsPerSecond: opts.rps,
		MaxRetries:       opts.maxRetries,
		TimeLimit:        opts.timeLimit,
		IndexNames:       indexNames,
		Logger:           slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil)),
	})
	// The summary prints even on failure: a partially-converged fleet is
	// exactly the state an operator needs the numbers for.
	fleetSummary(cmd, res)
	return runErr
}

// newMetaCatalogRepairCmd is the migration fan-out: rebind every schema in a
// database onto the latest version of its template, one transaction per
// schema.
//
// This is `RepairSchema` applied across a fleet. The single-schema operation
// already existed in the library and had no CLI surface at all; the fleet
// version is the one an operator actually needs after a template evolves,
// because a template save alone leaves every existing tenant pinned to the
// version it was created under.
func newMetaCatalogRepairCmd() *cobra.Command {
	var (
		contextName string
		clusterFile string
		databaseID  string
		concurrency int
		allSchemas  bool
		yes         bool
	)
	c := &cobra.Command{
		Use:   "repair",
		Short: "Rebind schemas onto the latest template version (write)",
		Example: `  frl meta catalog repair --database /tenants --all-schemas --yes
  frl meta catalog repair --database /tenants --all-schemas --concurrency 8 --yes`,
		Long: "Rebinds every schema in --database onto the latest version of its " +
			"schema template, the fleet-wide form of the catalog's RepairSchema.\n\n" +
			"Saving a new template version does NOT move existing schemas: each " +
			"schema row is pinned to the template version it was bound at, and " +
			"stays there until it is rebound. This is the rebind.\n\n" +
			"ONE TRANSACTION PER SCHEMA, deliberately. A fleet-wide rebind cannot " +
			"fit in a single FDB transaction (5 s / 10 MB), and a single " +
			"transaction would also mean one poisoned tenant rolls back every " +
			"healthy one. Atomicity is traded for idempotence instead: the pass " +
			"skips schemas already at the target version, so an interrupted run " +
			"is resumed simply by running it again.\n\n" +
			"A tenant that fails is reported and does NOT stop the fleet; the " +
			"command exits non-zero with every failure listed. The relational " +
			"catalog itself is never a target.\n\n" +
			"The destination is the LATEST stored version of the template " +
			"EACH SCHEMA IS BOUND TO, resolved per template rather than once " +
			"for the database: templates advance independently, and a database " +
			"mixing them must not judge every tenant against whichever template " +
			"happens to be furthest ahead.\n\n" +
			"There is deliberately no --to-version flag, because " +
			"RepairSchema has no 'rebind to version N' form. A flag that " +
			"appeared to pin an older version would silently rebind to the " +
			"latest anyway.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !allSchemas {
				return fmt.Errorf("`meta catalog repair` only has a fleet form — pass --all-schemas " +
					"to rebind every schema in --database")
			}
			if databaseID == "" {
				return fmt.Errorf("--database is required")
			}
			cf, err := resolveCatalogContext(contextName, clusterFile)
			if err != nil {
				return err
			}
			db, cat, err := fleetOpen(cf)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			targets, err := fleet.ListTargets(ctx, db, cat, databaseID)
			if err != nil {
				return err
			}
			if len(targets) == 0 {
				return fmt.Errorf("no schemas found in database %q", databaseID)
			}

			// Destinations are resolved PER TEMPLATE, not once for the
			// database. A database may hold schemas bound to different
			// templates, which advance independently; one fleet-wide
			// number would fail every tenant whose own template has not
			// reached it, and those tenants would never migrate.
			latest, err := fleet.LatestVersions(ctx, db, cat, targets)
			if err != nil {
				return err
			}

			if err := confirmWrite(cmd, yes, fmt.Sprintf(
				"rebind %s in %s to %s",
				fleetDescribeTargets(targets), databaseID, fleetDescribeVersions(latest))); err != nil {
				return err
			}

			res, runErr := fleet.MigrateToLatest(ctx, db, cat, relationalKeyspace(), targets, fleet.Options{
				Concurrency: concurrency,
				Progress:    fleetProgressPrinter(cmd),
			})
			fleetSummary(cmd, res)
			return runErr
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&clusterFile, "cluster-file", "", "FDB cluster file; overrides the context's cluster_file")
	c.Flags().StringVar(&databaseID, "database", "", "relational database URI whose schemas to rebind")
	c.Flags().BoolVar(&allSchemas, "all-schemas", false, "rebind every schema in --database")
	c.Flags().IntVar(&concurrency, "concurrency", 0, "schemas rebound in parallel (0 = default)")
	c.Flags().BoolVar(&yes, "yes", false, "skip the interactive confirmation")
	return c
}

// fleetDescribeVersions renders the per-template destinations for the
// confirmation prompt, so an operator confirming a mixed-template database
// sees that each template has its OWN destination rather than one number the
// whole fleet is being pushed to.
func fleetDescribeVersions(latest map[string]int) string {
	parts := make([]string, 0, len(latest))
	for name, v := range latest {
		parts = append(parts, fmt.Sprintf("%s@%d", name, v))
	}
	sort.Strings(parts)
	return "template version " + strings.Join(parts, ", ")
}

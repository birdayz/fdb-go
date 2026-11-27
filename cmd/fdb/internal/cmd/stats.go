package cmd

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"fdb.dev/internal/relational/functions"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/sqldriver"
)

// `fdb rl stats` — the offline planner-statistics maintainer (RFC-236).
//
// EVERY SUBCOMMAND GOES THROUGH THE SQL DRIVER, not through withStore, and
// that is the whole design. Statistics live at a location derived from the
// relational keyspace root and the schema's store subspace; the planner
// derives it one way, inside EmbeddedConnection. If this CLI derived it a
// second way it would be two pieces of code hoping they agree, and the failure
// mode is silent: the collector writes somewhere the planner never looks, every
// command reports success, and the only symptom is that plans never change.
// Routing through Conn.Raw means the CLI and the planner cannot disagree about
// the SINGLE-SCHEMA path: collect, clear and the planner's read all call
// EmbeddedConnection.statisticsLocation, which is the one derivation.
//
// There is a SECOND derivation, and it is named here rather than glossed:
// `--all-schemas` goes through fleet.CollectStatistics, which cannot use a
// connection because it has no single schema to bind one to. It calls the same
// two keyspace methods, and the agreement between the two is asserted by
// TestIntegration_Stats_FleetCollectIsReadableByTheConnection — the fan-out
// writes, the connection reads — because that disagreement is invisible
// everywhere else.
//
// It also means `fdb rl stats collect` exercises the exact path a library user
// gets from conn.Raw — the CLI is a thin wrapper over the library call, not a
// parallel implementation of it.
func newStatsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "stats",
		Short: "Collect and inspect planner statistics for a schema",
		Long: "Planner statistics are per-record-type row counts, collected " +
			"OFFLINE by scanning the store and read by the query planner to " +
			"order joins by real table sizes instead of a constant. They are " +
			"exact for a store at rest; a record whose primary key moves during " +
			"a scan can be counted twice or missed, since the scan spans " +
			"transactions.\n\n" +
			"Collection is a maintenance job, not a query path: it reads " +
			"every record. Run it on a schedule sized to how fast the data " +
			"changes shape — statistics expire after ~24h and the planner " +
			"silently falls back to its constant when they do.\n\n" +
			"The planner only reads them when the connection opts in:\n" +
			"  fdbsql:///FRL/MYAPP?schema=MAIN&planner_statistics=true\n\n" +
			"Statistics are stored OUTSIDE every record store's subspace, so " +
			"a Java client sharing this cluster neither sees nor is disturbed " +
			"by them.",
		Example: `  fdb rl stats collect --database /FRL/myapp --schema MAIN
  fdb rl stats show --database /FRL/myapp --schema MAIN
  fdb rl stats show --database /FRL/myapp --schema MAIN -o json | jq '.per_type'
  fdb rl stats clear --database /FRL/myapp --schema MAIN --yes`,
	}
	c.AddCommand(newStatsCollectCmd())
	c.AddCommand(newStatsShowCmd())
	c.AddCommand(newStatsClearCmd())
	return c
}

// statsAddressFlags is the addressing every `fdb rl stats` subcommand shares.
//
// Relational addressing is REQUIRED — no --keyspace-path, no --meta-file. The
// consumer of these statistics is the SQL planner, and the planner locates them
// through the relational keyspace. Writing them for a store addressed any other
// way would produce bytes nothing reads, which is worse than an error because
// it looks like it worked.
type statsAddressFlags struct {
	contextName string
	database    string
	schema      string
	clusterFile string
}

func (f *statsAddressFlags) register(c *cobra.Command) {
	c.Flags().StringVar(&f.contextName, "context", "", "context name to use")
	c.Flags().StringVar(&f.database, "database", "", "relational database URI (defaults to the relational context, e.g. /FRL/myapp)")
	c.Flags().StringVar(&f.schema, "schema", "", "relational schema name (defaults to the relational context)")
	c.Flags().StringVar(&f.clusterFile, "cluster-file", "", "FDB cluster file; overrides the context's cluster_file — chains with `fdb dev up`")
}

func (f *statsAddressFlags) resolve(cmd *cobra.Command, allSchemas bool) error {
	if allSchemas && cmd.Flags().Changed("schema") {
		return fmt.Errorf("conflicting targets: --all-schemas covers every schema in the database, so it cannot be combined with --schema")
	}
	target, err := resolveSQLAddress(cmd, f.contextName, f.clusterFile, f.database, f.schema)
	if err != nil {
		return err
	}
	if allSchemas {
		// The context's schema is only a default, not a fan-out restriction.
		target.schema = ""
	} else if target.schema == "" {
		return fmt.Errorf("missing required flag --schema (statistics are per-schema)")
	}
	f.database, f.schema = target.database, target.schema
	f.clusterFile = target.clusterFile()
	return nil
}

// describe renders the resolved target for messages and confirmation prompts.
func (f *statsAddressFlags) describe() string {
	return functions.NormalizeIdentifier(f.database) + "/" + functions.NormalizeIdentifier(f.schema)
}

// withStatsConn opens the resolved address on one pinned SQL connection and
// hands the caller the embedded connection underneath it.
//
// The *sql.Conn is pinned rather than taken per statement because the raw
// escape hatch is only meaningful against a specific connection: db.Conn gives
// us one, Raw exposes its driver value, and the callback owns it for exactly
// the duration of the call (database/sql invalidates the value afterwards, so
// nothing may escape it).
func (f *statsAddressFlags) withStatsConn(
	ctx context.Context,
	fn func(context.Context, *embedded.EmbeddedConnection) error,
) error {
	// The schema is an SQL identifier: unquoted folds to upper case (the same
	// rule CREATE SCHEMA applies), so `--schema main` addresses the schema that
	// `create schema /FRL/db/main` created.
	schema := functions.NormalizeIdentifier(f.schema)
	dbPath := functions.NormalizeIdentifier(f.database)
	engine, err := sqldriver.NewEngine(sqldriver.EngineConfig{
		ClusterFile: f.clusterFile,
		Domains:     []string{"FRL"},
		Limits:      sqldriver.UnboundedJavaCompatLimits(),
	})
	if err != nil {
		return fmt.Errorf("open sql engine: %w", err)
	}
	defer engine.Close()
	db, err := engine.OpenDB(dbPath, sqldriver.WithSchema(schema))
	if err != nil {
		return fmt.Errorf("open fdbsql %s: %w", f.describe(), err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", f.describe(), err)
	}
	defer conn.Close()

	return conn.Raw(func(dc any) error {
		ec, ok := dc.(*embedded.EmbeddedConnection)
		if !ok {
			return fmt.Errorf("driver connection is %T, not *embedded.EmbeddedConnection", dc)
		}
		return fn(ctx, ec)
	})
}

func newStatsCollectCmd() *cobra.Command {
	var (
		addr              statsAddressFlags
		batchSize         int
		maxRecordsPerType int64
		allSchemas        bool
		concurrency       int
		outputFmt         string
	)
	c := &cobra.Command{
		Use:   "collect",
		Short: "Scan the schema and write per-type row counts",
		Long: "Reads EVERY record in the schema's store, tallies by record " +
			"type, and replaces the stored statistics in one transaction.\n\n" +
			"Cost is proportional to the store: this is an offline job. It " +
			"scans in continuation-driven batches, each ending at whichever " +
			"comes first of --batch-size records, a " +
			recordlayer.DefaultCollectTimeLimit.String() + " time limit or a " +
			strconv.FormatInt(recordlayer.DefaultCollectScannedBytesLimit/(1<<20), 10) +
			"MB read limit. The last two are what hold whatever the records " +
			"weigh, so no single transaction approaches FDB's 5s limit even " +
			"when rows are large.\n\n" +
			"--max-records-per-type ABORTS the collection as soon as any one " +
			"table exceeds that many rows, and stores nothing. It aborts rather " +
			"than skipping the table because skipping cannot help: the planner " +
			"needs every table present, so a skipped one disables statistics " +
			"for the whole schema anyway — the old behaviour paid for a full " +
			"scan and produced nothing usable. Aborting reaches the same place " +
			"having read far less, and names the table that blew the budget.\n\n" +
			"NOTE: an aborted run leaves any PREVIOUS statistics in place. " +
			"`stats show` will keep reporting those until they expire, so a " +
			"failed collection does not disable a working schema — but it also " +
			"does not refresh it. Clear them if that is not what you want.\n\n" +
			"Collection does not invalidate already-cached plans; connections " +
			"pick the new counts up as their cached plans age out.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			collect := recordlayer.CollectOptions{
				BatchSize:         batchSize,
				MaxRecordsPerType: maxRecordsPerType,
			}
			if allSchemas {
				// The fan-out emits per-schema progress and a tally, not one
				// report, so there is nothing for -o json to render. Accepting
				// the flag and printing text anyway hands a script unparseable
				// output while reporting success.
				if outputFmt != "text" {
					// Leads with a sentence word, not the flag: fang title-cases the
					// first rune of an error banner and would render "--Output".
					return fmt.Errorf("the --all-schemas fan-out emits per-schema progress and a tally, not a single report, so --output %s has nothing to render", outputFmt)
				}
				if err := addr.resolve(cmd, true); err != nil {
					return err
				}
				return runFleetStatsCollect(cmd, &addr, collect, concurrency)
			}
			if err := addr.resolve(cmd, false); err != nil {
				return err
			}
			return addr.withStatsConn(cmd.Context(),
				func(ctx context.Context, ec *embedded.EmbeddedConnection) error {
					started := time.Now()
					report, err := ec.CollectStatistics(ctx, collect)
					if err != nil {
						return fmt.Errorf("collect statistics for %s: %w", addr.describe(), err)
					}
					// An ABORTED run stored nothing. Rendering the report and
					// exiting 0 tells scheduled automation the refresh
					// succeeded while the previous statistics sit there going
					// stale — and the fleet path already treats the same report
					// as a per-target failure, so exiting 0 here made one
					// outcome mean two things depending on which flag was used.
					if len(report.Collected) == 0 && len(report.Skipped) > 0 {
						if rErr := renderCollectReport(cmd, outputFmt, addr.describe(), report, time.Since(started)); rErr != nil {
							return rErr
						}
						return fmt.Errorf("collection aborted for %s and stored nothing: %s",
							addr.describe(), describeSkippedTypes(
								safeDecoderOver(append(storedKeysOf(report.Collected), storedKeysOf(report.Skipped)...), nil),
								report.Skipped))
					}
					return renderCollectReport(cmd, outputFmt, addr.describe(), report, time.Since(started))
				})
		},
	}
	addr.register(c)
	c.Flags().IntVar(&batchSize, "batch-size", 0, "records scanned per transaction (0 = library default, 1000)")
	c.Flags().Int64Var(&maxRecordsPerType, "max-records-per-type", 0, "abort the collection and store nothing once any one table exceeds this many rows (0 = no cap)")
	c.Flags().BoolVar(&allSchemas, "all-schemas", false, "collect for EVERY schema in --database, one scan per schema, with per-schema failure isolation")
	c.Flags().IntVar(&concurrency, "concurrency", 0, "schemas collected in parallel with --all-schemas (0 = fleet default)")
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}

func newStatsShowCmd() *cobra.Command {
	var (
		addr      statsAddressFlags
		outputFmt string
	)
	c := &cobra.Command{
		Use:   "show",
		Short: "Report the stored statistics and whether the planner would use them",
		Long: "Prints what was collected AND the planner's verdict on it, from " +
			"the same code the planner runs — so 'usable' here means the " +
			"planner accepts them, not that this command found some bytes.\n\n" +
			"A verdict of 'not usable' names which gate refused:\n" +
			"  not collected   nothing has been written for this schema\n" +
			"  expired         older than the freshness bound (~24h)\n" +
			"  stamped ahead   the entry's version is ahead of the cluster's " +
			"(a restore from backup moves versions backwards)\n" +
			"  incomplete      at least one record type has no entry\n\n" +
			"The verdict is independent of the connection's " +
			"planner_statistics flag: it reports whether the DATA is good, " +
			"which is what you want to know before turning the flag on.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			if err := addr.resolve(cmd, false); err != nil {
				return err
			}
			return addr.withStatsConn(cmd.Context(),
				func(ctx context.Context, ec *embedded.EmbeddedConnection) error {
					st, err := ec.StatisticsStatus(ctx)
					if err != nil {
						return fmt.Errorf("read statistics for %s: %w", addr.describe(), err)
					}
					return renderStatsStatus(cmd, outputFmt, addr.describe(), st)
				})
		},
	}
	addr.register(c)
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}

func newStatsClearCmd() *cobra.Command {
	var (
		addr statsAddressFlags
		yes  bool
	)
	c := &cobra.Command{
		Use:   "clear",
		Short: "Remove the schema's collected statistics",
		Long: "Deletes the stored statistics. The record store is untouched — " +
			"statistics live outside every store's subspace, so this can " +
			"never reach record or index data.\n\n" +
			"Queries keep working: the planner falls back to its constant, " +
			"which is exactly what it does when statistics are absent, " +
			"expired, or incomplete. Use this to undo a collection taken " +
			"against data you have since reshaped, rather than waiting out " +
			"the freshness bound.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := addr.resolve(cmd, false); err != nil {
				return err
			}
			if err := confirmWrite(cmd, yes,
				fmt.Sprintf("clear planner statistics for %s", addr.describe())); err != nil {
				return err
			}
			return addr.withStatsConn(cmd.Context(),
				func(ctx context.Context, ec *embedded.EmbeddedConnection) error {
					if err := ec.ClearStatistics(ctx); err != nil {
						return fmt.Errorf("clear statistics for %s: %w", addr.describe(), err)
					}
					_, err := fmt.Fprintf(cmd.OutOrStdout(),
						"cleared planner statistics for %s\n", addr.describe())
					return err
				})
		},
	}
	addr.register(c)
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return c
}

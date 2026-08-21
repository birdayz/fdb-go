package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/functions"
	_ "fdb.dev/pkg/relational/sqldriver"
)

// `frl stats` — the offline planner-statistics maintainer (RFC-236).
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
// It also means `frl stats collect` exercises the exact path a library user
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
			"  fdbsql:///myapp?schema=MAIN&planner_statistics=true\n\n" +
			"Statistics are stored OUTSIDE every record store's subspace, so " +
			"a Java client sharing this cluster neither sees nor is disturbed " +
			"by them.",
		Example: `  frl stats collect --database /myapp --schema MAIN
  frl stats show --database /myapp --schema MAIN
  frl stats show --database /myapp --schema MAIN -o json | jq '.per_type'
  frl stats clear --database /myapp --schema MAIN --yes`,
	}
	c.AddCommand(newStatsCollectCmd())
	c.AddCommand(newStatsShowCmd())
	c.AddCommand(newStatsClearCmd())
	return c
}

// statsAddressFlags is the addressing every `frl stats` subcommand shares.
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
	c.Flags().StringVar(&f.database, "database", "", "relational database URI (required, e.g. /myapp)")
	c.Flags().StringVar(&f.schema, "schema", "", "relational schema name (required)")
	c.Flags().StringVar(&f.clusterFile, "cluster-file", "", "FDB cluster file; overrides the context's cluster_file — chains with `frl fdb up`")
}

// describe renders the target for messages and confirmation prompts.
func (f *statsAddressFlags) describe() string {
	return f.database + "/" + functions.StripIdentifierQuotes(f.schema)
}

// withStatsConn resolves the address, opens one pinned SQL connection, and
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
	if f.database == "" {
		// Leading sentence word: fang capitalizes the first rune of an error
		// banner, which would garble a leading flag name into "--Database".
		return fmt.Errorf("missing required flag --database (e.g. --database /myapp)")
	}
	if f.schema == "" {
		return fmt.Errorf("missing required flag --schema (statistics are per-schema)")
	}
	target, err := (&storeAddressFlags{
		contextName: f.contextName,
		clusterFile: f.clusterFile,
	}).resolve()
	if err != nil {
		return err
	}
	// The schema is an SQL identifier: unquoted folds to upper case (the same
	// rule CREATE SCHEMA applies), so `--schema main` addresses the schema that
	// `create schema /db/main` created.
	schema := functions.StripIdentifierQuotes(f.schema)
	dsn := buildFDBSQLDSN(target.clusterFile(), f.database, schema)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		return fmt.Errorf("open fdbsql %q: %w", dsn, err)
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
				if addr.schema != "" {
					return fmt.Errorf("conflicting targets: --all-schemas covers every schema in the database, so it cannot be combined with --schema")
				}
				if addr.database == "" {
					return fmt.Errorf("missing required flag --database (--all-schemas fans out within ONE database)")
				}
				return runFleetStatsCollect(cmd, &addr, collect, concurrency)
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
							addr.describe(), describeSkippedTypes(report.Skipped))
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

// statsCollectResult is the typed JSON shape of `stats collect -o json`.
type statsCollectResult struct {
	Schema         string           `json:"schema"`
	RecordsScanned int64            `json:"records_scanned"`
	DurationMS     int64            `json:"duration_ms"`
	Collected      map[string]int64 `json:"collected"`
	// Skipped maps a record type to WHY it has no statistic. Distinct from a
	// zero count: "no rows" and "not counted" are different facts and only one
	// of them describes an empty table.
	Skipped map[string]string `json:"skipped"`
}

func renderCollectReport(
	cmd *cobra.Command,
	outputFmt, schema string,
	report *recordlayer.CollectionReport,
	elapsed time.Duration,
) error {
	// Same as renderStatsStatus: decoded keys with no gate here, safe because
	// collection refuses on an ambiguous schema before producing a report
	// (fleet's ambiguousRefusal, and connection.go's check on the single-target
	// path).
	collected := make(map[string]int64, len(report.Collected))
	for name, st := range report.Collected {
		collected[name] = st.Count
	}
	collected = userKeyedCounts(collected)
	if outputFmt == "json" {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		skipped := report.Skipped
		if skipped == nil {
			skipped = map[string]string{}
		}
		return enc.Encode(statsCollectResult{
			Schema:         schema,
			RecordsScanned: report.RecordsScanned,
			DurationMS:     elapsed.Milliseconds(),
			Collected:      collected,
			Skipped:        userKeyed(skipped),
		})
	}
	out := cmd.OutOrStdout()
	// An ABORTED run stored nothing, and this renderer is reached on that path
	// too -- RunE prints the report before returning its non-zero error. An
	// unconditional "collected" headline then announces success for a run whose
	// own later line says the types were NOT collected, which is the reading an
	// operator takes away from a scrollback.
	if len(collected) == 0 && len(report.Skipped) > 0 {
		fmt.Fprintf(out, "collection ABORTED for %s — nothing was stored\n", schema)
	} else {
		fmt.Fprintf(out, "collected statistics for %s\n", schema)
	}
	fmt.Fprintf(out, "  records scanned: %d in %s\n", report.RecordsScanned, elapsed.Round(time.Millisecond))
	fmt.Fprintln(out)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tROWS")
	for _, name := range sortedKeys(collected) {
		fmt.Fprintf(tw, "%s\t%d\n", name, collected[name])
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(report.Skipped) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "not collected (the planner refuses statistics until every type has one):")
		skippedByUser := userKeyed(report.Skipped)
		for _, name := range sortedKeys(skippedByUser) {
			fmt.Fprintf(out, "  %s: %s\n", name, skippedByUser[name])
		}
	}
	return nil
}

// statsShowResult is the typed JSON shape of `stats show -o json`.
type statsShowResult struct {
	Schema string `json:"schema"`
	// Usable is the planner's own verdict, from the planner's own code.
	Usable  bool   `json:"usable"`
	Refusal string `json:"refusal,omitempty"`
	// Found is TRI-STATE, so it is a pointer: true when statistics are known to
	// be stored, false when the store is known to hold none, and NULL when
	// existence was not established.
	//
	// A plain bool collapsed the third case into the second and contradicted the
	// field's own contract: a TORN set is known to hold entries, while a failed
	// read and the synthetic preflight establish nothing -- the first could not
	// read, the second deliberately did not look. Serialising all three as
	// `found: false` tells a machine consumer the store is empty in two cases
	// where that is not known and one where it is known to be wrong.
	Found *bool `json:"found"`
	// PerType is present whenever statistics were found, usable or not — a
	// stale count is still the number an operator wants to look at.
	PerType              map[string]int64 `json:"per_type,omitempty"`
	CollectedAtVersion   int64            `json:"collected_at_version,omitempty"`
	CollectedAtUnixNanos int64            `json:"collected_at_unix_nanos,omitempty"`
	CurrentVersion       int64            `json:"current_version,omitempty"`
	AgeVersions          int64            `json:"age_versions,omitempty"`
	MaxAgeVersions       int64            `json:"max_age_versions"`
	SyntheticTypes       []string         `json:"synthetic_types,omitempty"`
	AmbiguousTypes       []string         `json:"ambiguous_types,omitempty"`
	// ReadRefusal names WHICH way a stored set is torn, when Refusal is
	// StatisticsTorn. Present on BOTH render paths deliberately: the text path
	// named it and JSON did not, so an automated caller could see THAT the set
	// was unusable and never WHY -- and a diagnosis on one render path is half a
	// diagnosis, which is the same reason the synthetic and ambiguous names are
	// on both.
	ReadRefusal string `json:"read_refusal,omitempty"`
	// ReadError is the underlying failure when Refusal is StatisticsReadFailed,
	// where existence is UNKNOWN rather than absent. Same reasoning as above:
	// it was reaching only the text path.
	ReadError    string   `json:"read_error,omitempty"`
	MissingTypes []string `json:"missing_types,omitempty"`
	ExtraTypes   []string `json:"extra_types,omitempty"`
}

func renderStatsStatus(
	cmd *cobra.Command,
	outputFmt, schema string,
	st embedded.StatisticsStatus,
) error {
	// Keyed by the SQL identifier from here down, so BOTH renderings get it from
	// one decode. Decoding per consumer is what left the text path leaking the
	// storage name while JSON was correct -- the same one-of-two-consumers shape
	// that put ReadRefusal on the text path only.
	// Keyed by the DECODED name with NO ambiguity gate, which is safe only
	// because the reader refuses first: under a collision two stored names decode
	// alike and would collapse into one row, pricing one table with the other's
	// count. This function holds a status, not a metadata, so it cannot check --
	// the dependency is pinned by embedded's TestAmbiguityRefusalCarriesNoPerTypeMap.
	perType := make(map[string]int64, len(st.Stats.PerType))
	for name, s := range st.Stats.PerType {
		perType[name] = s.Count
	}
	perType = userKeyedCounts(perType)
	if outputFmt == "json" {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(statsShowResult{
			Schema:               schema,
			Usable:               st.Usable,
			Refusal:              string(st.Refusal),
			Found:                foundTriState(st),
			PerType:              perType,
			CollectedAtVersion:   st.Stats.CollectedAtVersion,
			CollectedAtUnixNanos: st.Stats.CollectedAtUnixNanos,
			CurrentVersion:       st.CurrentVersion,
			AgeVersions:          st.AgeVersions,
			MaxAgeVersions:       st.MaxAgeVersions,
			SyntheticTypes:       st.SyntheticTypes, // VERBATIM -- see userName's note
			AmbiguousTypes:       st.AmbiguousTypes,
			ReadRefusal:          string(st.ReadRefusal),
			ReadError:            errString(st.ReadErr),
			MissingTypes:         userNames(st.MissingTypes),
			ExtraTypes:           userNames(st.ExtraTypes),
		})
	}

	out := cmd.OutOrStdout()
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Schema:\t%s\n", schema)
	if st.Usable {
		fmt.Fprintf(tw, "Planner verdict:\tUSABLE\n")
	} else {
		fmt.Fprintf(tw, "Planner verdict:\tNOT USABLE — %s\n", st.Refusal)
	}
	if st.Found {
		fmt.Fprintf(tw, "Collected at:\tversion %d", st.Stats.CollectedAtVersion)
		if st.Stats.CollectedAtUnixNanos > 0 {
			fmt.Fprintf(tw, " (%s)", time.Unix(0, st.Stats.CollectedAtUnixNanos).UTC().Format(time.RFC3339))
		}
		fmt.Fprintln(tw)
		if st.CurrentVersion > 0 {
			fmt.Fprintf(tw, "Age:\t%s of %s allowed\n",
				renderVersionAge(st.AgeVersions), renderVersionAge(st.MaxAgeVersions))
		}
	}
	if len(st.SyntheticTypes) > 0 {
		// Naming them is the difference between a verdict and an instruction.
		fmt.Fprintf(tw, "Synthetic types:\t%s (unmodeled by this port; statistics are refused for the schema)\n",
			strings.Join(st.SyntheticTypes, ", ")) // VERBATIM
	}
	if len(st.AmbiguousTypes) > 0 {
		// Same reason as synthetic types: a verdict an operator cannot act on is
		// half a diagnosis. Both names are shown because either one of the two
		// tables can be renamed or quoted differently to break the collision.
		fmt.Fprintf(tw, "Ambiguous types:\t%s (one name is the other's escaped form, so a "+
			"lookup cannot say which table is meant; statistics are refused for the schema)\n",
			strings.Join(st.AmbiguousTypes, ", "))
	}
	if len(st.MissingTypes) > 0 {
		fmt.Fprintf(tw, "Missing types:\t%s\n", strings.Join(userNames(st.MissingTypes), ", "))
	}
	if len(st.ExtraTypes) > 0 {
		// Orphans never refuse: the planner asks by declared type name and
		// simply never names a dropped table. Reported so an operator can tell
		// a stale entry from a schema they misremembered.
		fmt.Fprintf(tw, "Orphan types:\t%s (dropped from the schema; harmless)\n",
			strings.Join(userNames(st.ExtraTypes), ", "))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if !st.Found {
		fmt.Fprintln(out)
		// The hint is gated on the REFUSAL, not on Found. A synthetic-type
		// refusal also leaves Found false — it returns before any read — but
		// collection rejects those schemas too, so recommending it would send an
		// operator to run a command that cannot succeed, from the one verdict
		// that is permanent.
		switch st.Refusal {
		case embedded.StatisticsNotCollected:
			fmt.Fprintln(out, "run `frl stats collect` to gather them")
		case embedded.StatisticsAmbiguousNames:
			// Same shape as the synthetic arm: decided from metadata WITHOUT a read,
			// so claiming absence would assert a fact this path went out of its way
			// not to look up. What is certain is that collection cannot help.
			fmt.Fprintf(out, "this schema declares record types whose names collide across "+
				"the SQL and storage namespaces (%s), so a lookup cannot say which "+
				"table is meant; statistics can never be used for it and collection "+
				"is refused too\n", strings.Join(st.AmbiguousTypes, " and "))
			fmt.Fprintln(out, "whether any are still stored was not read")
		case embedded.StatisticsSyntheticTypes:
			// This verdict is reached WITHOUT READING -- the synthetic gate decides
			// before any I/O, deliberately, since the answer cannot depend on it. So
			// existence is unknown, and entries can genuinely still be there: a
			// schema collected before its metadata was rebound to a version
			// declaring a joined type leaves its old entries in place. Saying
			// "nothing is stored" asserts a fact this path went out of its way not to
			// look up. What IS certain is that collection cannot help.
			fmt.Fprintln(out, "this schema declares record types this port does not model, so "+
				"statistics can never be used for it; collection is refused too")
			fmt.Fprintln(out, "whether any are still stored was not read")
		case embedded.StatisticsTorn:
			// Stored, and unusable — the opposite of absent, and the opposite of
			// permanent. The default arm below says "nothing is stored, and
			// collection will not help", which is wrong TWICE for this refusal:
			// something is stored, and a collect is exactly the repair, because it
			// ClearRanges the range and rewrites header and entries in one
			// transaction.
			fmt.Fprintf(out, "statistics ARE stored but cannot be vouched for: %s\n", st.ReadRefusal)
			fmt.Fprintln(out, "run `frl stats collect` to replace them")
		case embedded.StatisticsReadFailed:
			// Found is false here because existence is UNKNOWN, not because the
			// store is empty: the read itself failed. Saying "nothing is stored"
			// turns a transient, permission or cluster fault into a confident
			// statement of absence -- and an operator who believes it collects
			// again, which does not diagnose the fault either.
			fmt.Fprintf(out, "could not read them, so whether any are stored is UNKNOWN: %s\n",
				st.Refusal)
			if st.ReadErr != nil {
				fmt.Fprintf(out, "  %v\n", st.ReadErr)
			}
		default:
			fmt.Fprintf(out, "nothing is stored, and collection will not help: %s\n", st.Refusal)
		}
		return nil
	}
	fmt.Fprintln(out)
	rows := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(rows, "TYPE\tROWS")
	for _, name := range sortedKeys(perType) {
		fmt.Fprintf(rows, "%s\t%d\n", name, perType[name])
	}
	if err := rows.Flush(); err != nil {
		return err
	}
	if st.Usable {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "the planner uses these only when the connection opts in:")
		fmt.Fprintln(out, "  ?planner_statistics=true")
	}
	return nil
}

// renderVersionAge turns an FDB version delta into something an operator reads
// without doing arithmetic. FDB advances ~1,000,000 versions per second, so the
// conversion is exact enough to be useful and approximate enough to be marked
// as such.
func renderVersionAge(versions int64) string {
	if versions < 0 {
		return fmt.Sprintf("%d versions (ahead of the cluster)", versions)
	}
	d := time.Duration(versions) * time.Microsecond
	return fmt.Sprintf("%d versions (~%s)", versions, d.Round(time.Second))
}

// sortedKeys returns a map's keys in sorted order, so text and JSON output are
// byte-stable across runs — a CLI whose output reorders cannot be diffed.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// describeSkippedTypes renders why a collection abandoned its run, sorted so
// repeated invocations are diffable.
func describeSkippedTypes(skipped map[string]string) string {
	// DECODED, like every other operator-facing surface. This one is the error
	// banner of the abort path, printed directly beneath a report body that was
	// already decoded -- so leaving it raw made ONE run of `frl stats collect`
	// print MY$TABLE in the not-collected block and MY__1TABLE in the error line
	// under it. Two spellings of one table, three lines apart.
	byUser := userKeyed(skipped)
	parts := make([]string, 0, len(byUser))
	for _, name := range sortedKeys(byUser) {
		parts = append(parts, name+": "+byUser[name])
	}
	return strings.Join(parts, "; ")
}

// errString renders an error for a JSON field, empty when there is none. Kept
// separate so the omitempty tag means "no error" rather than "an error whose
// text happened to be empty".
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// foundTriState renders existence as known-true, known-false or unknown.
//
// The gate's Found is a plain bool and cannot carry the third state, so the
// mapping is made here from the REFUSAL, which is what actually distinguishes
// them: a torn set is known to hold entries; a failed read and the synthetic
// preflight establish nothing, the first because it could not read and the
// second because it deliberately did not look.
func foundTriState(st embedded.StatisticsStatus) *bool {
	yes, no := true, false
	switch st.Refusal {
	case embedded.StatisticsTorn:
		return &yes // entries were read; only the header is unusable
	case embedded.StatisticsReadFailed,
		embedded.StatisticsSyntheticTypes,
		embedded.StatisticsAmbiguousNames:
		// Existence NOT established. The first could not read; the other two are
		// metadata-only verdicts that short-circuit before the read on purpose,
		// so Found is false because nobody looked -- and entries from an earlier
		// metadata version may well still be there.
		//
		// Ambiguity joined this list the moment the read was skipped for it. That
		// is the coupling worth naming: making a verdict cheaper by not reading
		// turns its Found into an unknown, and any refusal that gains a
		// short-circuit has to be added here in the same change.
		return nil
	}
	if st.Found {
		return &yes
	}
	return &no
}

// userName converts a STORAGE record-type name to the SQL identifier an
// operator wrote, at the rendering boundary and nowhere earlier.
//
// Metadata and the collector both key by storage names — a table quoted as
// "MY$TABLE" is stored as MY__1TABLE — and the operator-facing surfaces in this
// file were copying those keys verbatim. That names a table the operator does
// not have, and `per_type` is a documented interface, so a script keying by
// table name silently misses rather than failing.
//
// NOT every name reaching this file is storage-keyed, and the difference is not
// visible by looking. StatisticsStatus.AmbiguousTypes is ALREADY decoded, at its
// source in RecordMetaData.AmbiguousDeclaredNames, so passing it through here
// would decode twice -- and ToUserIdentifier is NOT idempotent:
// MY__01TABLE -> MY__1TABLE -> MY$TABLE, pinned by
// TestToUserIdentifierIsNotIdempotent. A second decode does not fail, it renames
// the table. Check the field's documented namespace before wrapping it.
//
// Decoding HERE, not upstream, is deliberate: the collision the reader detects
// lives in storage space, and the map the planner is handed must stay keyed the
// way the planner asks. Only what a human or a script reads gets translated.
//
// DECODES ONLY WHEN THE RESULT PROVABLY MAPS BACK. This port's metadata does not
// only come from the SQL layer -- RecordMetaDataBuilder.SetRecords copies
// protobuf identifiers verbatim -- so a record type may legally be named
// __0Order without ever having been escaped from anything. Decoding that yields
// __Order, which re-encodes to __Order and NOT to __0Order, so the name shown to
// the operator would resolve to nothing: GetRecordType misses on the direct key
// and then skips its escape retry, because the escape is a no-op.
//
// The round trip IS the provenance test, and it needs no extra bookkeeping: if
// encode(decode(s)) == s then the decoded spelling addresses exactly the same
// type, which is the whole promise made to whoever copies a name out of the
// output. When it does not hold, the stored name is shown unchanged -- uglier,
// and correct.
//
// WHAT THIS DOES NOT PROVE. The round trip shows encode(decode(s)) == s, which
// makes GetRecordType's SECOND step land on s. It says nothing about the FIRST
// step: if some other record type is stored under the decoded spelling itself,
// the direct-key hit answers first and returns that one instead. That is the
// non-injectivity hazard, it is a property of the lookup rather than of this
// decode, and AmbiguousDeclaredNames is what detects it -- see
// TestGetRecordTypeMisResolvesAnAmbiguousPair.
//
// So this guard is not the whole story, and deliberately so: it is applied
// unconditionally, because a name that resolves to NOTHING is always wrong and
// needs no context to rule out. The ambiguous-pair case needs the declared set,
// so it lives in userNamesFor/userNameFor -- which every renderer holding an md
// now routes through, not just the completer. An earlier round gated only the
// completer on the argument that a read-only renderer showing an ambiguous name
// is merely misleading; that was wrong twice over, because `record scan -o
// json`'s record_type is documented as feeding --type, and because a listing an
// operator copies from is not read-only in any useful sense.
func userName(storage string) string {
	user := recordlayer.ToUserIdentifier(storage)
	if user == storage {
		return storage
	}
	back, err := recordlayer.ToProtoBufCompliantName(user)
	if err != nil || back != storage {
		return storage
	}
	return user
}

// userNames is userName over a slice, re-sorted BY THE DECODED NAME.
//
// The sort has to happen after decoding, not before, because the two orders
// differ: storage-sorted [A__0B, A__1B] prints as [A__B, A$B], while a reader
// scanning the output expects [A$B, A__B]. The gate sorts these lists in storage
// space -- correctly, since that is the namespace it holds them in -- so the
// renderer is where the order has to be restated in the namespace it prints.
//
// DECODING GOES THROUGH THESE HELPERS AND NOWHERE ELSE, which is the point:
// a list of call SITES has been wrong three times here -- written as FOUR,
// repaired to THREE by subtracting rather than re-sweeping, and still missing
// the sql.go sites and meta_diff's sortSection. A set of functions is closed
// and the compiler keeps it honest; a set of call sites is open and rots.
//
//	userName        one name, round-trip guarded, no declared-set context
//	userNames       a slice, decoded then RE-SORTED in the printed namespace
//	userNamesFor    userNames plus the ambiguity gate, for callers holding md
//	userNameFor     userNamesFor's single-name form
//	userFieldName   userFieldNames's single-name form
//	userFieldNames  key-expression fields: decoded, ORDER PRESERVED, no gate
//	userKeyed       a map re-keyed by the decoded name
//
// The sort has to be restated after decoding because the two orders differ, and
// order has to be LEFT ALONE for key expressions because position is semantic
// there. fleet's describeSkipped is the one decoder outside this file; it sorts
// after decoding for the same reason userNames does.
//
// SyntheticRecordTypesNotModeledError.Error() decodes NOTHING and must not:
// Java stores a synthetic type's name verbatim, so decoding invents a
// declaration. It also must not re-sort -- a test asserts it preserves input
// order -- because with no decode there is only one namespace.
func userNames(storage []string) []string {
	if storage == nil {
		return nil
	}
	out := make([]string, len(storage))
	for i, s := range storage {
		out[i] = userName(s)
	}
	sort.Strings(out)
	return out
}

// userKeyed is userName over a map's keys.
// Two distinct storage keys CAN decode to the same user identifier -- that is
// exactly the collision StatisticsAmbiguousNames refuses -- and this map would
// silently keep one of them. It is unreachable rather than handled: the
// ambiguity gate refuses such a schema before any of these renderers run, so a
// colliding pair never reaches a report. If that gate is ever relaxed, this
// collapses two tables into one row and says nothing.
func userKeyed[V any](m map[string]V) map[string]V {
	if m == nil {
		return nil
	}
	out := make(map[string]V, len(m))
	for k, v := range m {
		out[userName(k)] = v
	}
	return out
}

// userNamesFor is userNames with the ambiguity gate applied, for callers that
// hold the metadata.
//
// userName takes a STRING, so it cannot see whether some other record type is
// declared under the decoded spelling — and that is the case where a decoded
// name resolves to the WRONG type rather than to none. Anything holding an
// md should come through here instead.
//
// Under a collision the STORED names are returned, sorted in storage space.
// They are deliberately the less pleasant answer: they are the only spellings
// that resolve at GetRecordType's first step, so they are the only ones that
// mean what they say. A schema in this state wants renaming, not a prettier
// listing — which is the same call the statistics reader makes when it refuses
// outright on the identical condition.
func userNamesFor(md *recordlayer.RecordMetaData, names []string) []string {
	if md != nil {
		if _, ambiguous := md.AmbiguousDeclaredNames(); ambiguous {
			out := append([]string(nil), names...)
			sort.Strings(out)
			return out
		}
	}
	return userNames(names)
}

// userNameFor is userNamesFor for a single name.
func userNameFor(md *recordlayer.RecordMetaData, name string) string {
	if md != nil {
		if _, ambiguous := md.AmbiguousDeclaredNames(); ambiguous {
			return name
		}
	}
	return userName(name)
}

// userFieldNames decodes a key expression's field names for display, PRESERVING
// ORDER.
//
// A column name is a protobuf FIELD name and is escaped exactly like a record
// type name, so `frl sql`'s \d printing COL$X while `frl meta types describe`
// printed COL__1X for that same column was one defect wearing two spellings.
//
// Two deliberate differences from userNamesFor. Order is never touched: these
// are key expressions, where position is semantic — sorting a primary key would
// misreport the key. And the ambiguity gate does not apply: the collision it
// guards is a property of the declared RECORD TYPE set, which is what
// AmbiguousDeclaredNames inspects, and no destructive command resolves a target
// by column name.
func userFieldNames(fields []string) []string {
	if fields == nil {
		return nil
	}
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = userName(f)
	}
	return out
}

// userFieldName is userFieldNames for a single column name.
//
// Separate from userName so the CALL SITE says which namespace it is in. A
// column takes no ambiguity gate (that collision is a property of the declared
// record-type set) while a record type does, and a bare userName at a site
// holding an md is exactly the bug that let `record scan -o json` print a
// record_type resolving to the wrong table.
func userFieldName(field string) string { return userName(field) }

// userKeyedCounts re-keys a per-type count map to SQL identifiers, falling back
// to the STORED keys if any two of them decode alike.
//
// Self-contained on purpose. These renderers hold a report or a status, never a
// metadata, so they cannot ask AmbiguousDeclaredNames. They used to rely on the
// reader and both collect paths refusing an ambiguous schema first -- true, but
// non-local: remove or bypass any of those guards and this silently overwrote
// one colliding type with another's count, one row short and no error.
//
// The collapse needs no metadata to detect: if decoding produces fewer keys
// than it consumed, two stored names landed on one. Same all-or-nothing rule as
// everywhere else -- under a collision, stored names for every row.
func userKeyedCounts(byStorage map[string]int64) map[string]int64 {
	stored := func() map[string]int64 {
		out := make(map[string]int64, len(byStorage))
		for k, v := range byStorage {
			out[k] = v
		}
		return out
	}

	out := make(map[string]int64, len(byStorage))
	for s, v := range byStorage {
		d := userName(s)
		// AMBIGUOUS: this row would print under a name that is a DIFFERENT
		// entry's stored name, so a reader cannot tell which table it means.
		if _, isOthersStoredName := byStorage[d]; isOthersStoredName && d != s {
			return stored()
		}
		out[d] = v
	}
	// COLLAPSE: two stored names decoding to one key would lose a row outright.
	// Unreachable while userName round-trip guards -- if s1 != s2 both decoded to
	// d, encode(d) equals at most one of them, so the guard suppresses the other
	// -- but checked rather than argued, because that guard lives one function
	// away and non-local arguments are what this file keeps getting wrong.
	if len(out) != len(byStorage) {
		return stored()
	}
	return out
}

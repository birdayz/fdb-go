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
		Long: "Planner statistics are exact per-record-type row counts, " +
			"collected OFFLINE by scanning the store, and read by the query " +
			"planner to order joins by real table sizes instead of a " +
			"constant.\n\n" +
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
		Short: "Scan the schema and write exact per-type row counts",
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
	collected := make(map[string]int64, len(report.Collected))
	for name, st := range report.Collected {
		collected[name] = st.Count
	}
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
			Skipped:        skipped,
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
		for _, name := range sortedKeys(report.Skipped) {
			fmt.Fprintf(out, "  %s: %s\n", name, report.Skipped[name])
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
	Found   bool   `json:"found"`
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
	perType := make(map[string]int64, len(st.Stats.PerType))
	for name, s := range st.Stats.PerType {
		perType[name] = s.Count
	}
	if outputFmt == "json" {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(statsShowResult{
			Schema:               schema,
			Usable:               st.Usable,
			Refusal:              string(st.Refusal),
			Found:                st.Found,
			PerType:              perType,
			CollectedAtVersion:   st.Stats.CollectedAtVersion,
			CollectedAtUnixNanos: st.Stats.CollectedAtUnixNanos,
			CurrentVersion:       st.CurrentVersion,
			AgeVersions:          st.AgeVersions,
			MaxAgeVersions:       st.MaxAgeVersions,
			SyntheticTypes:       st.SyntheticTypes,
			AmbiguousTypes:       st.AmbiguousTypes,
			ReadRefusal:          string(st.ReadRefusal),
			ReadError:            errString(st.ReadErr),
			MissingTypes:         st.MissingTypes,
			ExtraTypes:           st.ExtraTypes,
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
			strings.Join(st.SyntheticTypes, ", "))
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
		fmt.Fprintf(tw, "Missing types:\t%s\n", strings.Join(st.MissingTypes, ", "))
	}
	if len(st.ExtraTypes) > 0 {
		// Orphans never refuse: the planner asks by declared type name and
		// simply never names a dropped table. Reported so an operator can tell
		// a stale entry from a schema they misremembered.
		fmt.Fprintf(tw, "Orphan types:\t%s (dropped from the schema; harmless)\n",
			strings.Join(st.ExtraTypes, ", "))
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
	parts := make([]string, 0, len(skipped))
	for _, name := range sortedKeys(skipped) {
		parts = append(parts, name+": "+skipped[name])
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

// otelbench is an immutable synthetic-span experiment, not an OTLP receiver.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"fdb.dev/layers/otel"
	"fdb.dev/layers/otel/collector"
	"fdb.dev/layers/otel/sqlbackend"
)

// runCollectorBenchmark is the collector-bench entry point; tests replace it.
var runCollectorBenchmark = func(ctx context.Context, cfg collector.CollectorBenchConfig, ingestStart func()) (collector.CollectorBenchReport, error) {
	return collector.RunBenchmark(collector.WithBenchmarkIngestStart(ctx, ingestStart), cfg)
}

type backendOpener func(context.Context, otel.Config, string, string) (otel.Backend, error)

// sqlOptions are the --sql-* flags, read by openBackend for the sql backend.
var sqlOptions = sqlbackend.Options{Ingest: sqlbackend.IngestDirect, Query: sqlbackend.QuerySQL, PageRows: sqlbackend.DefaultPageRows}

func openBackend(ctx context.Context, c otel.Config, cluster, chURL string) (otel.Backend, error) {
	if c.Backend == "fdb" {
		return otel.OpenFDBContext(ctx, c, cluster)
	}
	if c.Backend == "sql" {
		return sqlbackend.Open(ctx, c, cluster, sqlOptions)
	}
	return otel.OpenCHContext(ctx, c, chURL, c.Backend == "ch-attrs")
}

func execute(args []string, stdout, stderr io.Writer, open backendOpener) int {
	return executeContext(context.Background(), args, stdout, stderr, open)
}

func executeContext(parent context.Context, args []string, stdout, stderr io.Writer, open backendOpener) (code int) {
	flags := flag.NewFlagSet("otelbench", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var profiling profileFlags
	profiling.register(flags)
	c := otel.DefaultConfig()
	var backend, mode, cluster, chURL, output, suite, revision string
	var confirm bool
	var timeout time.Duration
	flags.StringVar(&backend, "backend", "", "required: fdb, sql, ch-map, or ch-attrs")
	flags.StringVar(&sqlOptions.Ingest, "sql-ingest", sqlOptions.Ingest, "sql backend ingest: direct (Record Layer saves into the OTEL_V1 schema stores) or insert (SQL INSERT)")
	flags.StringVar(&sqlOptions.Query, "sql-query", sqlOptions.Query, "sql backend queries: sql (through the SQL driver) or direct (Record Layer scans of the same OTEL_V1 indexes)")
	flags.StringVar(&sqlOptions.Rows, "sql-rows", sqlbackend.RowsCompiled, "sql backend direct ingest rows: compiled (otelv1pb types) or dynamic (dynamic messages of the catalog descriptor)")
	flags.BoolVar(&sqlOptions.PinnedConn, "sql-pinned-conn", sqlOptions.PinnedConn, "sql backend: run SQL queries on one pinned connection, keeping its session schema cache (no per-statement catalog read)")
	flags.IntVar(&sqlOptions.PageRows, "sql-page-rows", sqlOptions.PageRows, "sql backend: scanned rows per SQL page (EXECUTION_SCANNED_ROWS_LIMIT); negative is unbounded")
	flags.StringVar(&mode, "mode", "", "required: ingest, query, audit, drop, or collector-bench (drives otel.Exporter; takes no --backend but fdb)")
	flags.StringVar(&c.Run, "run", "", "required fresh owned namespace [a-z0-9_]{8,48}; no destructive default")
	flags.Uint64Var(&c.Seed, "seed", c.Seed, "deterministic corpus seed")
	flags.IntVar(&c.Count, "count", c.Count, "unique synthetic span population")
	flags.IntVar(&c.Tenants, "tenants", c.Tenants, "tenant population")
	flags.IntVar(&c.Attributes, "attrs", c.Attributes, "scoped typed attributes per span, 4..128")
	flags.IntVar(&c.MatchEvery, "match-every", c.MatchEvery, "matching traces every N traces within each tenant; two spans per trace")
	flags.IntVar(&c.Batch, "batch", c.Batch, "FDB maximum spans per transaction, 1..64; byte guards also apply")
	flags.StringVar(&c.SaveMode, "save-mode", c.SaveMode, "FDB save mode: serial, pipelined, or batch")
	flags.StringVar(&c.RecordCompression, "record-compression", c.RecordCompression, "Record Layer primary storage: none, zlib-1, zlib-default, brotli-0, brotli-1, brotli-3, or brotli-4; Brotli is Go-only (fdb backend), zlib is Java- and SQL-readable")
	flags.StringVar(&c.PostingValueFormat, "posting-value-format", c.PostingValueFormat, "FDB covering values: json or tuple; immutable fresh-run layout")
	flags.BoolVar(&c.BulkIndexConflicts, "bulk-index-conflicts", c.BulkIndexConflicts, "FDB batch saves conservatively coalesce index write-conflict ranges; read conflicts unchanged")
	flags.IntVar(&c.Workers, "workers", c.Workers, "FDB ingest workers, 1..8")
	flags.BoolVar(&c.Diagnostics, "diagnostics", c.Diagnostics, "include ingest diagnostics in evidence")
	flags.IntVar(&c.CHBatch, "ch-batch", c.CHBatch, "CH offline bulk insert row cap; separately reported, not matched live ack batching")
	flags.IntVar(&c.CHBatchBytes, "ch-batch-bytes", c.CHBatchBytes, "CH offline bulk insert byte cap, at most 64MiB")
	flags.IntVar(&c.CHIndexGranularity, "ch-index-granularity", c.CHIndexGranularity, "fresh-table MergeTree index granularity, 1..8192")
	flags.IntVar(&c.Tenant, "tenant", c.Tenant, "query tenant ordinal")
	flags.IntVar(&c.K, "k", c.K, "newest K DISTINCT traces, time DESC then trace ID DESC")
	flags.IntVar(&c.Iterations, "iterations", c.Iterations, "query sample count; raw samples retained")
	flags.IntVar(&c.MaxCandidates, "max-candidates", c.MaxCandidates, "FDB consumed candidates / CH max_rows_to_read; failures are not partial answers")
	flags.Int64Var(&c.MaxBytes, "max-bytes", c.MaxBytes, "FDB consumed KV logical-byte / CH max_bytes_to_read budget")
	flags.IntVar(&c.HashBytes, "hash-bytes", c.HashBytes, "FDB digest bytes (1 for collision testing, 32 normally)")
	flags.StringVar(&c.Order, "order", c.Order, "trace (complete candidate topK) or time (ordered distinct frontier); one layout per run")
	flags.StringVar(&suite, "suite", "selective", "selective (varying predicates), mixed (also common resource term), rare (one hot predicate), trace, span (standalone span ID), time (intrinsic half-open range), genai (typed/scoped attributes), all (trace, span, genai, time)")
	flags.StringVar(&cluster, "cluster-file", "", "explicit owned FDB cluster file; CLI never launches containers")
	flags.StringVar(&chURL, "ch-url", "", "explicit owned ClickHouse HTTP endpoint")
	flags.StringVar(&c.CHQueryTransport, "ch-query-transport", c.CHQueryTransport, "ClickHouse query transport: http or native")
	flags.StringVar(&c.CHNativeAddr, "ch-native-addr", c.CHNativeAddr, "explicit loopback host:port required for native ClickHouse queries; no DSN or credentials")
	flags.StringVar(&output, "output", "-", "JSON evidence path, created exclusively; - writes stdout")
	flags.StringVar(&revision, "revision", "unrecorded", "actual source revision for evidence receipt")
	flags.DurationVar(&timeout, "timeout", 10*time.Minute, "whole-command context deadline")
	flags.BoolVar(&confirm, "confirm-drop", false, "required with drop; only matching owned run manifest may be removed")
	bench := collector.DefaultCollectorBenchConfig()
	flags.IntVar(&bench.Conversations, "conversations", bench.Conversations, "collector-bench: synthetic multi-turn GenAI conversations, one trace each; ignored with --duration")
	flags.IntVar(&bench.TurnsPerConversation, "turns", bench.TurnsPerConversation, "collector-bench: turns per conversation; one span per turn, each turn the parent of the next")
	flags.IntVar(&bench.Producers, "producers", bench.Producers, "collector-bench: concurrent ConsumeTraces callers")
	flags.DurationVar(&bench.Duration, "duration", bench.Duration, "collector-bench: sustained ingest duration, at most 1h (e.g. 10m); 0 ingests the fixed --conversations corpus")
	flags.DurationVar(&bench.ReportInterval, "report-interval", bench.ReportInterval, "collector-bench: time-series window length")
	flags.DurationVar(&bench.Warmup, "warmup", bench.Warmup, "collector-bench: initial load excluded from the steady-state rate; must be shorter than --duration")
	flags.IntVar(&bench.QueryWorkers, "query-workers", bench.QueryWorkers, "collector-bench: goroutines running verified queries against committed data during ingest")
	flags.IntVar(&bench.NumConsumers, "consumers", bench.NumConsumers, "collector-bench: sending_queue.num_consumers export workers")
	flags.BoolVar(&bench.EnableMessageDeltas, "enable-message-deltas", bench.EnableMessageDeltas, "collector-bench: content-addressed GenAI message delta storage")
	flags.BoolVar(&bench.EnableMessageFullText, "enable-message-fts", bench.EnableMessageFullText, "collector-bench: GenAI message full-text index and fts_* query classes")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	// collector-bench drives otel.Exporter, so no backend applies; --backend
	// fdb is accepted for the command lines of earlier receipts.
	collectorBench := mode == "collector-bench" && (backend == "" || backend == "fdb")
	if !collectorBench {
		c.Backend = backend
		if err := c.Validate(); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	if flags.NArg() != 0 || timeout <= 0 || (mode != "ingest" && mode != "query" && mode != "audit" && mode != "drop" && !collectorBench) || !collectorBench && backend != "fdb" && backend != "sql" && backend != "ch-map" && backend != "ch-attrs" {
		fmt.Fprintln(stderr, "explicit valid backend/mode required; see -help")
		return 2
	}
	if err := otel.ValidateQuerySuite(c, suite); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if mode == "drop" && !confirm {
		fmt.Fprintln(stderr, "drop requires --confirm-drop")
		return 2
	}
	if err := profiling.validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if collectorBench {
		var err error
		if bench, err = collectorBenchConfig(flags, bench, c, cluster, revision); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if err := bench.Validate(); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if timeout, err = collectorBenchTimeout(flags, timeout, bench.Duration); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		// By default the profiles cover the steady phase.
		explicit := false
		flags.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "profile-start" })
		if !explicit {
			profiling.start = bench.Warmup
		}
	}
	var prof *profiler
	if profiling.enabled() {
		var err error
		if prof, err = newProfiler(profiling); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	var writer io.Writer = stdout
	var file *os.File
	if output != "-" {
		var err error
		file, err = os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if prof != nil {
				prof.abort()
			}
			fmt.Fprintln(stderr, err)
			return 2
		}
		defer file.Close()
		writer = file
	}
	if prof != nil {
		if !collectorBench {
			prof.begin()
		}
		// The run's report is written either way; a profiling failure only
		// changes the exit status.
		defer func() {
			if err := prof.close(); err != nil {
				fmt.Fprintln(stderr, "profiling:", err)
				if code == 0 {
					code = 1
				}
			}
		}()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if collectorBench {
		ingestStart := func() {}
		if prof != nil {
			ingestStart = prof.begin
		}
		return writeCollectorBench(ctx, bench, writer, file, stderr, ingestStart)
	}
	e := otel.NewEvidence(c, backend, mode, suite, revision)
	started := time.Now()
	b, err := open(ctx, c, cluster, chURL)
	if err == nil {
		defer b.Close()
		switch mode {
		case "ingest":
			e.Stats, err = b.Ingest(ctx)
		case "query":
			err = otel.MeasureQueries(ctx, b, c, suite, &e)
		case "audit":
			e.Stats, err = b.Audit(ctx)
			if err == nil {
				err = otel.AuditQueries(ctx, b, c)
			}
		case "drop":
			err = b.Drop(ctx)
		}
	}
	if mode != "query" {
		e.Elapsed = time.Since(started).Nanoseconds()
	}
	if err != nil {
		e.Error = err.Error()
	}
	enc := json.NewEncoder(writer)
	enc.SetIndent("", "  ")
	if encodeErr := enc.Encode(e); encodeErr != nil {
		fmt.Fprintln(stderr, encodeErr)
		return 1
	}
	if file != nil {
		if syncErr := file.Sync(); syncErr != nil {
			fmt.Fprintln(stderr, syncErr)
			return 1
		}
	}
	if err != nil {
		return 1
	}
	return 0
}

// collectorBenchConfig applies the shared flags that were set explicitly;
// unset ones keep the collector-bench defaults, which differ from ingest's.
// The custom store's layout flags have no Exporter setting (D2).
func collectorBenchConfig(flags *flag.FlagSet, bench collector.CollectorBenchConfig, c otel.Config, cluster, revision string) (collector.CollectorBenchConfig, error) {
	bench.ClusterFile, bench.Run, bench.Revision = cluster, c.Run, revision
	var err error
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "seed":
			bench.Seed = c.Seed
		case "tenant":
			bench.Tenant = c.Tenant
		case "record-compression":
			bench.RecordCompression = c.RecordCompression
		case "posting-value-format", "hash-bytes", "order":
			if err == nil {
				err = fmt.Errorf("--%s applies to --backend fdb only; the Exporter has one time order and no posting values", f.Name)
			}
		case "iterations":
			bench.QueryIterations = c.Iterations
		case "batch":
			bench.ExporterBatchSize = c.Batch
		case "workers":
			bench.ExporterWorkers = c.Workers
		}
	})
	return bench, err
}

// collectorBenchDrain is the default allowance beyond --duration for the final
// flush, the fidelity audit and the post-ingest query phase.
const collectorBenchDrain = 10 * time.Minute

// collectorBenchTimeout extends the default deadline to cover a sustained run;
// an explicit --timeout must exceed --duration.
func collectorBenchTimeout(flags *flag.FlagSet, timeout, duration time.Duration) (time.Duration, error) {
	explicit := false
	flags.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "timeout" })
	if !explicit {
		return max(timeout, duration+collectorBenchDrain), nil
	}
	if duration > 0 && timeout <= duration {
		return 0, fmt.Errorf("--timeout %s must exceed --duration %s", timeout, duration)
	}
	return timeout, nil
}

func writeCollectorBench(ctx context.Context, bench collector.CollectorBenchConfig, writer io.Writer, file *os.File, stderr io.Writer, ingestStart func()) int {
	report, err := runCollectorBenchmark(ctx, bench, ingestStart)
	if err != nil && report.Error == "" {
		report.Error = err.Error()
	}
	enc := json.NewEncoder(writer)
	enc.SetIndent("", "  ")
	if encodeErr := enc.Encode(report); encodeErr != nil {
		fmt.Fprintln(stderr, encodeErr)
		return 1
	}
	if file != nil {
		if syncErr := file.Sync(); syncErr != nil {
			fmt.Fprintln(stderr, syncErr)
			return 1
		}
	}
	if err != nil {
		return 1
	}
	return 0
}

func main() { os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr, openBackend)) }

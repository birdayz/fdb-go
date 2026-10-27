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
)

type backendOpener func(context.Context, otel.Config, string, string) (otel.Backend, error)

func openBackend(ctx context.Context, c otel.Config, cluster, chURL string) (otel.Backend, error) {
	if c.Backend == "fdb" {
		return otel.OpenFDBContext(ctx, c, cluster)
	}
	return otel.OpenCHContext(ctx, c, chURL, c.Backend == "ch-attrs")
}

func execute(args []string, stdout, stderr io.Writer, open backendOpener) int {
	return executeContext(context.Background(), args, stdout, stderr, open)
}

func executeContext(parent context.Context, args []string, stdout, stderr io.Writer, open backendOpener) int {
	flags := flag.NewFlagSet("otelbench", flag.ContinueOnError)
	flags.SetOutput(stderr)
	c := otel.DefaultConfig()
	var backend, mode, cluster, chURL, output, suite, revision string
	var confirm bool
	var timeout time.Duration
	flags.StringVar(&backend, "backend", "", "required: fdb, ch-map, or ch-attrs")
	flags.StringVar(&mode, "mode", "", "required: ingest, query, audit, drop")
	flags.StringVar(&c.Run, "run", "", "required fresh owned namespace [a-z0-9_]{8,48}; no destructive default")
	flags.Uint64Var(&c.Seed, "seed", c.Seed, "deterministic corpus seed")
	flags.IntVar(&c.Count, "count", c.Count, "unique synthetic span population")
	flags.IntVar(&c.Tenants, "tenants", c.Tenants, "tenant population")
	flags.IntVar(&c.Attributes, "attrs", c.Attributes, "scoped typed attributes per span, 4..128")
	flags.IntVar(&c.MatchEvery, "match-every", c.MatchEvery, "matching traces every N traces within each tenant; two spans per trace")
	flags.IntVar(&c.Batch, "batch", c.Batch, "FDB maximum spans per transaction, 1..64; byte guards also apply")
	flags.StringVar(&c.SaveMode, "save-mode", c.SaveMode, "FDB save mode: serial or pipelined")
	flags.IntVar(&c.CHBatch, "ch-batch", c.CHBatch, "CH offline bulk insert row cap; separately reported, not matched live ack batching")
	flags.IntVar(&c.CHBatchBytes, "ch-batch-bytes", c.CHBatchBytes, "CH offline bulk insert byte cap, at most 64MiB")
	flags.IntVar(&c.Tenant, "tenant", c.Tenant, "query tenant ordinal")
	flags.IntVar(&c.K, "k", c.K, "newest K DISTINCT traces, time DESC then trace ID DESC")
	flags.IntVar(&c.Iterations, "iterations", c.Iterations, "query sample count; raw samples retained")
	flags.IntVar(&c.MaxCandidates, "max-candidates", c.MaxCandidates, "FDB consumed candidates / CH max_rows_to_read; failures are not partial answers")
	flags.Int64Var(&c.MaxBytes, "max-bytes", c.MaxBytes, "FDB consumed KV logical-byte / CH max_bytes_to_read budget")
	flags.IntVar(&c.HashBytes, "hash-bytes", c.HashBytes, "FDB digest bytes (1 for collision testing, 32 normally)")
	flags.StringVar(&c.Order, "order", c.Order, "trace (complete candidate topK) or time (ordered distinct frontier); one layout per run")
	flags.StringVar(&suite, "suite", "selective", "selective (varying predicates), mixed (also common resource term), rare (one hot predicate), trace")
	flags.StringVar(&cluster, "cluster-file", "", "explicit owned FDB cluster file; CLI never launches containers")
	flags.StringVar(&chURL, "ch-url", "", "explicit owned ClickHouse HTTP endpoint")
	flags.StringVar(&output, "output", "-", "JSON evidence path, created exclusively; - writes stdout")
	flags.StringVar(&revision, "revision", "unrecorded", "actual source revision for evidence receipt")
	flags.DurationVar(&timeout, "timeout", 10*time.Minute, "whole-command context deadline")
	flags.BoolVar(&confirm, "confirm-drop", false, "required with drop; only matching owned run manifest may be removed")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	c.Backend = backend
	if err := c.Validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if flags.NArg() != 0 || timeout <= 0 || (mode != "ingest" && mode != "query" && mode != "audit" && mode != "drop") || backend != "fdb" && backend != "ch-map" && backend != "ch-attrs" {
		fmt.Fprintln(stderr, "explicit valid backend/mode required; see -help")
		return 2
	}
	if suite != "selective" && suite != "mixed" && suite != "rare" && suite != "trace" {
		fmt.Fprintln(stderr, "suite must be selective, mixed, rare or trace")
		return 2
	}
	if mode == "drop" && !confirm {
		fmt.Fprintln(stderr, "drop requires --confirm-drop")
		return 2
	}
	var writer io.Writer = stdout
	var file *os.File
	if output != "-" {
		var err error
		file, err = os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		defer file.Close()
		writer = file
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
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
func main() { os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr, openBackend)) }

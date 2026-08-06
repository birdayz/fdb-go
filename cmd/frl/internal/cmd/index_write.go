package cmd

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"fdb.dev/pkg/recordlayer"
)

// Index write commands (RFC-174 §3.3): build / rebuild / set-state.
// This is the operator task the read surface can see (`index ls` shows
// WRITE_ONLY) but couldn't fix before the write wave.

// defaultBuildMaxRetries is Java's OnlineIndexer default
// (OnlineIndexOperationConfig.DEFAULT_MAX_RETRIES). It now merely restates the
// library default rather than correcting it: the Go builder used to default to 0,
// which disabled the adaptive limit-halving, so a single transaction_too_large
// escaping the client retry loop aborted the whole build with no back-off. The CLI
// papered over that for its own users while every library caller stayed exposed, so
// the default moved into NewOnlineIndexerBuilder where it belongs.
//
// Kept explicit anyway: an operator reading `--help` should see the retry budget the
// build will actually use, not have to infer it from the library.
const defaultBuildMaxRetries = 100

func newIndexBuildCmd() *cobra.Command {
	var (
		addr        storeAddressFlags
		yes         bool
		limit       int
		rps         int
		maxRetries  int
		timeLimit   time.Duration
		allSchemas  bool
		concurrency int
	)
	c := &cobra.Command{
		Use:   "build [name]",
		Short: "Build an index online (write)",
		Example: `  frl index build Order$price --yes
  frl index build Order$price --rps 5000 --limit 200 --yes
  frl index build IDX --time-limit 30s --yes   # partial pass; rerun resumes
  frl index build --database /tenants --all-schemas --yes          # whole fleet
  frl index build IDX --database /tenants --all-schemas --yes      # one index, whole fleet`,
		ValidArgsFunction: indexNameCompletion,
		Long: "Drives the online indexer over the store: scans records in " +
			"batched transactions, writes index entries, tracks progress in " +
			"the store's range-set, and marks the index READABLE when the " +
			"whole range is built. Safe to interrupt — per-range progress " +
			"commits atomically, and a rerun resumes from the ranges already " +
			"done. A build interrupted by --time-limit resumes the same way.\n\n" +
			"--max-retries defaults to 100 (Java's default), bounding the " +
			"adaptive batch-halving on transient errors. --rps caps records " +
			"scanned per second and applies regardless of the retry budget; " +
			"--limit is the per-transaction record batch.\n\n" +
			"Resuming with different indexing settings than the interrupted " +
			"build fails with the saved vs requested stamps — rerun with " +
			"matching settings to take over, or `frl index rebuild` to start " +
			"over from scratch.\n\n" +
			"--all-schemas builds across every schema in --database instead of " +
			"one store, with the index name optional: given, it rolls that one " +
			"index across the fleet; omitted, each tenant builds whatever it " +
			"owes. The fan-out uses ONE TRANSACTION PER TENANT — a fleet build " +
			"cannot fit in a single transaction, and one poisoned tenant must " +
			"not roll back the healthy ones. Failures are reported per tenant " +
			"and do not stop the pass; rerun to resume, since a completed index " +
			"is simply no longer pending.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if allSchemas {
				if addr.schema != "" {
					return fmt.Errorf("--all-schemas fans out over every schema in --database — " +
						"drop --schema (it addresses a single store)")
				}
				if addr.database == "" {
					return fmt.Errorf("--all-schemas needs --database to know which fleet to build")
				}
				return runFleetIndexBuild(cmd, &addr, addr.database, args, yes, indexBuildOptions{
					limit: limit, rps: rps, maxRetries: maxRetries, timeLimit: timeLimit,
				}, concurrency)
			}
			if len(args) != 1 {
				return fmt.Errorf("accepts 1 arg (the index name), received %d — "+
					"or pass --all-schemas to build across a whole database", len(args))
			}
			target, err := addr.resolve()
			if err != nil {
				return err
			}
			ss, err := target.subspace()
			if err != nil {
				return err
			}
			if err := guardNotCatalog(ss); err != nil {
				return err
			}
			if err := confirmWrite(cmd, yes, fmt.Sprintf("build index %q in %s", args[0], target.describe())); err != nil {
				return err
			}
			return runIndexBuild(cmd, target, args[0], indexBuildOptions{
				limit: limit, rps: rps, maxRetries: maxRetries, timeLimit: timeLimit,
			})
		},
	}
	addr.register(c, true)
	c.Flags().BoolVar(&yes, "yes", false, "skip the interactive confirmation")
	c.Flags().IntVar(&limit, "limit", 0, "records per transaction (0 = indexer default)")
	c.Flags().IntVar(&rps, "rps", 0, "records-per-second throttle (0 = indexer default)")
	c.Flags().IntVar(&maxRetries, "max-retries", defaultBuildMaxRetries, "retry budget for adaptive batch-halving on transient errors")
	c.Flags().DurationVar(&timeLimit, "time-limit", 0, "stop after this duration (partial build; rerun resumes)")
	c.Flags().BoolVar(&allSchemas, "all-schemas", false, "build across every schema in --database (fleet fan-out)")
	c.Flags().IntVar(&concurrency, "concurrency", 0, "schemas built in parallel with --all-schemas (0 = default)")
	return c
}

func newIndexRebuildCmd() *cobra.Command {
	var (
		addr       storeAddressFlags
		yes        bool
		limit      int
		rps        int
		maxRetries int
	)
	c := &cobra.Command{
		Use:               "rebuild <name>",
		Short:             "Clear an index and build it from scratch (write)",
		Example:           `  frl index rebuild Order$price --yes`,
		ValidArgsFunction: indexNameCompletion,
		Long: "Clears every entry and all build progress for the index, marks " +
			"it WRITE_ONLY, then runs a full online build. The escape hatch " +
			"for a build stamped with different settings, or an index you " +
			"have reason to distrust.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := addr.resolve()
			if err != nil {
				return err
			}
			ss, err := target.subspace()
			if err != nil {
				return err
			}
			if err := guardNotCatalog(ss); err != nil {
				return err
			}
			if err := confirmWrite(cmd, yes, fmt.Sprintf("CLEAR and rebuild index %q in %s", args[0], target.describe())); err != nil {
				return err
			}
			if _, err := withStore(cmd.Context(), target,
				func(store *recordlayer.FDBRecordStore) (bool, error) {
					if _, err := lookupIndex(store.GetRecordMetaData(), args[0]); err != nil {
						return false, err
					}
					return store.ClearAndMarkIndexWriteOnly(args[0])
				}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "cleared %q — rebuilding\n", args[0])
			return runIndexBuild(cmd, target, args[0], indexBuildOptions{
				limit: limit, rps: rps, maxRetries: maxRetries,
			})
		},
	}
	addr.register(c, true)
	c.Flags().BoolVar(&yes, "yes", false, "skip the interactive confirmation")
	c.Flags().IntVar(&limit, "limit", 0, "records per transaction (0 = indexer default)")
	c.Flags().IntVar(&rps, "rps", 0, "records-per-second throttle (0 = indexer default)")
	c.Flags().IntVar(&maxRetries, "max-retries", defaultBuildMaxRetries, "retry budget for adaptive batch-halving on transient errors")
	return c
}

func newIndexSetStateCmd() *cobra.Command {
	var (
		addr storeAddressFlags
		yes  bool
	)
	c := &cobra.Command{
		Use:   "set-state <name> <readable|readable-unique-pending|write-only|disabled>",
		Short: "Change an index's state (write)",
		Example: `  frl index set-state Order$price write-only --yes
  frl index set-state Order$price readable --yes   # fails unless fully built`,
		ValidArgsFunction: indexNameCompletion,
		Long: "Flips the index-state key. `readable` refuses when the index " +
			"is not fully built (the record layer verifies the range-set) — " +
			"use `index build` to finish it first. `disabled` stops " +
			"maintenance entirely; `write-only` resumes maintenance without " +
			"exposing the index to queries.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := addr.resolve()
			if err != nil {
				return err
			}
			ss, err := target.subspace()
			if err != nil {
				return err
			}
			if err := guardNotCatalog(ss); err != nil {
				return err
			}
			if err := confirmWrite(cmd, yes, fmt.Sprintf("set index %q to %s in %s", args[0], args[1], target.describe())); err != nil {
				return err
			}
			changed, err := withStore(cmd.Context(), target,
				func(store *recordlayer.FDBRecordStore) (bool, error) {
					if _, err := lookupIndex(store.GetRecordMetaData(), args[0]); err != nil {
						return false, err
					}
					switch args[1] {
					case "readable":
						return store.MarkIndexReadable(args[0])
					case "readable-unique-pending":
						return store.MarkIndexReadableOrUniquePending(args[0])
					case "write-only":
						return store.MarkIndexWriteOnly(args[0])
					case "disabled":
						return store.MarkIndexDisabled(args[0])
					default:
						return false, fmt.Errorf("unknown state %q — want readable, readable-unique-pending, write-only, or disabled", args[1])
					}
				})
			if err != nil {
				return err
			}
			if changed {
				fmt.Fprintf(cmd.OutOrStdout(), "%s → %s\n", args[0], args[1])
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s already %s\n", args[0], args[1])
			}
			return nil
		},
	}
	addr.register(c, true)
	c.Flags().BoolVar(&yes, "yes", false, "skip the interactive confirmation")
	return c
}

type indexBuildOptions struct {
	limit      int
	rps        int
	maxRetries int
	timeLimit  time.Duration
}

// runIndexBuild wires the OnlineIndexer for one target index and drives
// the build, rendering progress (the indexer's own log lines) and the
// final count on stderr; stdout carries the machine-usable summary line.
func runIndexBuild(cmd *cobra.Command, target *storeTarget, indexName string, opts indexBuildOptions) error {
	ss, err := target.subspace()
	if err != nil {
		return err
	}
	db, err := openDatabase(target.clusterFile())
	if err != nil {
		return err
	}
	rec := recordlayer.NewFDBDatabase(db)

	src, err := resolveMetaSource(target, rec)
	if err != nil {
		return err
	}
	md, err := src.Load(cmd.Context())
	if err != nil {
		return err
	}
	idx, err := lookupIndex(md, indexName)
	if err != nil {
		return err
	}

	b := recordlayer.NewOnlineIndexerBuilder().
		SetDatabase(rec).
		SetMetaData(md).
		SetSubspace(ss).
		SetIndex(idx).
		SetMarkReadable(true).
		SetMaxRetries(opts.maxRetries).
		SetLogger(slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil)))
	if opts.limit > 0 {
		b.SetLimit(opts.limit)
	}
	if opts.rps > 0 {
		b.SetRecordsPerSecond(opts.rps)
	}
	if opts.timeLimit > 0 {
		b.SetTimeLimit(opts.timeLimit)
	}
	oi, err := b.Build()
	if err != nil {
		return err
	}
	n, err := oi.BuildIndex(cmd.Context())
	if err != nil {
		var partly *recordlayer.PartlyBuiltError
		if errors.As(err, &partly) {
			return formatPartlyBuilt(partly, indexName)
		}
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "built %s (%d records scanned)\n", indexName, n)
	return nil
}

// formatPartlyBuilt renders a PartlyBuiltError with its escape hatches
// (FDB C++ dev C2): the raw stamps mean nothing to an operator without
// the two ways out — take the build over with matching settings, or
// start from scratch with rebuild.
func formatPartlyBuilt(partly *recordlayer.PartlyBuiltError, indexName string) error {
	return fmt.Errorf("index %q has a partial build with DIFFERENT settings — saved stamp %q, this invocation would stamp %q.\n"+
		"Either rerun with the same settings to take the build over, or start from scratch with `frl index rebuild %s`",
		partly.IndexName, partly.SavedStamp, partly.ExpectedStamp, indexName)
}

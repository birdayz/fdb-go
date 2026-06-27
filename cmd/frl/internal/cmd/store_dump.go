package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"fdb.dev/cmd/frl/internal/config"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// subspaceLabel maps a record-layer subspace ID (first tuple element
// under the store prefix) to a human-readable name. These match the
// constants exported from pkg/recordlayer/constants.go.
var subspaceLabel = map[int64]string{
	recordlayer.StoreInfoKey:                 "store-info",
	recordlayer.RecordKey:                    "record",
	recordlayer.IndexKey:                     "index",
	recordlayer.IndexSecondarySpaceKey:       "index-secondary",
	recordlayer.RecordCountKey:               "record-count",
	recordlayer.IndexStateSpaceKey:           "index-state",
	recordlayer.IndexRangeSpaceKey:           "index-range",
	recordlayer.IndexUniquenessViolationsKey: "uniq-violations",
	recordlayer.RecordVersionKey:             "record-version",
	recordlayer.IndexBuildSpaceKey:           "index-build",
}

func newStoreDumpCmd() *cobra.Command {
	var (
		contextName string
		subspaceSel string
		limit       int
	)
	c := &cobra.Command{
		Use:   "dump",
		Short: "Dump raw FDB bytes under the store, tuple-decoded + labeled",
		Example: `  frl store dump --limit 50
  frl store dump --subspace index --limit 0    # just index entries
  frl store dump | grep '^index '
  frl store dump --limit 0 | awk '{print $1}' | sort -u   # populated subspaces`,
		Long: "Forensic view of a record store: scans the store's keyspace " +
			"range and prints one line per key, labeled with the subspace " +
			"name (store-info / record / index / …) and tuple-decoded " +
			"suffix. Unlike `fdbcli getrange`, keys are decoded into their " +
			"logical tuple structure so you can see which record, which " +
			"index, or which header field a byte belongs to.\n\n" +
			"--subspace: narrow the scan to a single subspace (e.g. record, " +
			"index, record-version). Scans only that FDB range rather than " +
			"the whole store, so it's cheap to use on large stores.\n\n" +
			"Read-only. No metadata required — works on any store at the " +
			"configured keyspace path. --limit defaults to 100; 0 = unlimited.",
		ValidArgsFunction: cobra.NoFileCompletions,
		Args:              cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			cfgCtx, err := config.ResolveContext(cfg, contextName)
			if err != nil {
				if errors.Is(err, config.ErrNoContext) {
					path, _ := config.Path()
					return fmt.Errorf("%w (config: %s)", err, path)
				}
				return err
			}
			if cfgCtx.GetKeyspacePath() == "" {
				return fmt.Errorf("context %q has empty keyspace_path", cfgCtx.GetName())
			}
			db, err := openDatabase(cfgCtx.GetClusterFile())
			if err != nil {
				return err
			}
			ss, err := parseKeyspacePath(cfgCtx.GetKeyspacePath())
			if err != nil {
				return err
			}
			scanSS := ss
			if subspaceSel != "" {
				id, ok := subspaceIDByLabel(subspaceSel)
				if !ok {
					return fmt.Errorf("unknown --subspace %q: want one of %s",
						subspaceSel, strings.Join(knownSubspaceLabels(), ", "))
				}
				scanSS = ss.Sub(id)
			}
			return runStoreDump(cmd.Context(), cmd.OutOrStdout(), db, ss, scanSS, limit)
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&subspaceSel, "subspace", "",
		"limit dump to one subspace (record / index / record-version / …)")
	c.Flags().IntVar(&limit, "limit", defaultScanLimit, "max keys to dump; 0 means unlimited")
	_ = c.RegisterFlagCompletionFunc("subspace",
		func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			return knownSubspaceLabels(), cobra.ShellCompDirectiveNoFileComp
		})
	return c
}

// subspaceIDByLabel reverses the subspaceLabel map. Returns (id, true)
// when the label is known. Called only from --subspace flag parsing, so
// the linear scan is fine — map size is fixed and tiny.
func subspaceIDByLabel(label string) (int64, bool) {
	for id, name := range subspaceLabel {
		if name == label {
			return id, true
		}
	}
	return 0, false
}

// knownSubspaceLabels returns every label in deterministic order for
// error messages and shell completion. Sorted so `frl store dump --help`
// and tab-completion produce stable output across invocations.
func knownSubspaceLabels() []string {
	labels := make([]string, 0, len(subspaceLabel))
	for _, name := range subspaceLabel {
		labels = append(labels, name)
	}
	sort.Strings(labels)
	return labels
}

// runStoreDump opens a read-only snapshot range read over scanSS (a subset
// of store ss when --subspace is used) and renders one human-friendly line
// per key-value pair. renderKV decodes keys relative to `ss` (the full
// store prefix) so subspace labels render correctly regardless of whether
// the caller narrowed the scan.
//
// Uses snapshot reads so we don't create conflict ranges during operator
// inspection — matches fdbcli's default behavior.
func runStoreDump(ctx context.Context, out io.Writer, db fdb.Database, ss, scanSS subspace.Subspace, limit int) error {
	begin, end := scanSS.FDBRangeKeys()
	ropts := fdb.RangeOptions{Mode: fdb.StreamingModeIterator}
	if limit > 0 {
		ropts.Limit = limit
	}

	_, err := db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		snap := rtx.Snapshot()
		iter := snap.GetRange(fdb.KeyRange{Begin: begin, End: end}, ropts).Iterator()
		// Limit enforcement lives entirely in ropts.Limit — the FDB iterator
		// stops after the requested number of entries, so the previous local
		// counter was dead code (same pattern already cleaned up in record.go).
		for iter.Advance() {
			// Observe SIGINT/SIGTERM mid-iteration so long dumps terminate
			// promptly. iter.Advance() itself blocks on FDB network traffic
			// and doesn't know about Go contexts, so this is the only place
			// cancellation can surface between batches.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			kv, err := iter.Get()
			if err != nil {
				return nil, fmt.Errorf("iterate: %w", err)
			}
			line, lerr := renderKV(ss, kv)
			if lerr != nil {
				return nil, lerr
			}
			if _, werr := fmt.Fprintln(out, line); werr != nil {
				return nil, werr
			}
		}
		return nil, nil
	})
	return err
}

// renderKV decodes one fdb.KeyValue under the store subspace and returns
// a compact labeled string:
//
//	store-info          ()
//	record              (order_id=42)   value=123 bytes
//	index               (Order$price, price=100, 42)   value=0 bytes
func renderKV(ss subspace.Subspace, kv fdb.KeyValue) (string, error) {
	t, err := ss.Unpack(fdb.Key(kv.Key))
	if err != nil {
		return fmt.Sprintf("(unpack failed: %v) raw=%x value=%d bytes", err, kv.Key, len(kv.Value)), nil
	}
	label := "unknown"
	if len(t) > 0 {
		if id, ok := toInt64(t[0]); ok {
			if name, ok := subspaceLabel[id]; ok {
				label = name
			}
		}
	}
	suffix := tuple.Tuple{}
	if len(t) > 1 {
		suffix = t[1:]
	}
	return fmt.Sprintf("%-18s %v   value=%d bytes", label, suffix, len(kv.Value)), nil
}

// toInt64 coerces a tuple element into int64 when possible. The tuple
// layer stores small integers as int64 but may surface them via other
// numeric types depending on API version.
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case uint64:
		return int64(n), true
	}
	return 0, false
}

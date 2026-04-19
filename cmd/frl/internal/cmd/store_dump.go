package cmd

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/config"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
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
		limit       int
	)
	c := &cobra.Command{
		Use:   "dump",
		Short: "Dump raw FDB bytes under the store, tuple-decoded + labeled",
		Long: "Forensic view of a record store: scans the store's keyspace " +
			"range and prints one line per key, labeled with the subspace " +
			"name (store-info / record / index / …) and tuple-decoded " +
			"suffix. Unlike `fdbcli getrange`, keys are decoded into their " +
			"logical tuple structure so you can see which record, which " +
			"index, or which header field a byte belongs to.\n\n" +
			"Read-only. No metadata required — works on any store at the " +
			"configured keyspace path. --limit defaults to 100; 0 = unlimited.",
		Args: cobra.NoArgs,
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
			return runStoreDump(cmd.OutOrStdout(), db, ss, limit)
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().IntVar(&limit, "limit", defaultScanLimit, "max keys to dump; 0 means unlimited")
	return c
}

// runStoreDump opens a read-only snapshot range read over the store's
// subspace and renders one human-friendly line per key-value pair.
// Uses snapshot reads so we don't create conflict ranges during
// operator inspection — matches fdbcli's default behavior.
func runStoreDump(out io.Writer, db fdb.Database, ss subspace.Subspace, limit int) error {
	begin, end := ss.FDBRangeKeys()
	ropts := fdb.RangeOptions{Mode: fdb.StreamingModeIterator}
	if limit > 0 {
		ropts.Limit = limit
	}

	_, err := db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		snap := rtx.Snapshot()
		iter := snap.GetRange(fdb.KeyRange{Begin: begin, End: end}, ropts).Iterator()
		count := 0
		for iter.Advance() {
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
			count++
			if limit > 0 && count >= limit {
				return nil, nil
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

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/catalog"
)

// newStatusCmd is the "is everything wired?" one-shot (RFC-174 §3.4):
// cluster reachable (GRV), store header present for the active target,
// metadata source loadable, relational catalog present. Each check
// reports independently so one broken leg doesn't hide the others.
func newStatusCmd() *cobra.Command {
	var addr storeAddressFlags
	var outputFmt string
	c := &cobra.Command{
		Use:   "status",
		Short: "One-shot wiring check: cluster, store, metadata, catalog",
		Example: `  fdb rl status
  fdb rl status --context prod -o json`,
		Long: "Runs the four wiring checks in order and reports each:\n" +
			"  cluster   — GRV fetch (cluster file + coordinators reachable)\n" +
			"  store     — DataStoreInfo header present at the target keyspace\n" +
			"  metadata  — the context's metadata source loads\n" +
			"  catalog   — a relational catalog exists on the cluster\n\n" +
			"Exit is non-zero when the cluster check fails; the other three " +
			"report ok/missing/error individually (a plain-core cluster " +
			"without a catalog is not an error, and a catalog-only setup " +
			"has no record-layer store to check).\n\n" +
			"--output / -o: 'text' (default) or 'json' " +
			"({cluster, read_version, store, metadata, catalog}).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			target, err := addr.resolve()
			if err != nil {
				return err
			}
			return runStatus(cmd.Context(), cmd.OutOrStdout(), target, outputFmt)
		},
	}
	addr.register(c, true)
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}

// statusReport is the JSON shape of `fdb rl status -o json`. Text mode
// renders the same fields, one line each.
type statusReport struct {
	Cluster     string `json:"cluster"`      // ok | error: …
	ReadVersion int64  `json:"read_version"` // 0 when cluster failed
	Store       string `json:"store"`        // ok | missing | skipped: … | error: …
	Metadata    string `json:"metadata"`     // ok (N record types) | skipped: … | error: …
	Catalog     string `json:"catalog"`      // ok (N databases) | missing | error: …
}

func runStatus(ctx context.Context, out io.Writer, target *storeTarget, outputFmt string) error {
	r := statusReport{}

	// 1. Cluster: GRV. Everything else depends on this; bail early.
	db, err := openDatabase(target.clusterFile())
	if err != nil {
		return err
	}
	rec := recordlayer.NewFDBDatabase(db)
	grv, err := rec.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		return rtx.Transaction().GetReadVersion().Get()
	})
	if err != nil {
		r.Cluster = "error: " + err.Error()
		writeStatus(out, r, outputFmt)
		return fmt.Errorf("cluster unreachable: %w", err)
	}
	r.Cluster = "ok"
	r.ReadVersion, _ = grv.(int64)

	// 2. Store header at the target keyspace.
	if ss, serr := target.subspace(); serr != nil {
		r.Store = "skipped: " + serr.Error()
	} else if _, ierr := readStoreInfo(ctx, rec, ss); ierr != nil {
		r.Store = "missing: " + ierr.Error()
	} else {
		r.Store = "ok"
	}

	// 3. Metadata source.
	if md, merr := loadTargetMetadata(ctx, target); merr != nil {
		r.Metadata = "error: " + merr.Error()
	} else {
		r.Metadata = fmt.Sprintf("ok (%d record types)", len(md.RecordTypes()))
	}

	// 4. Inspect the catalog without creating or migrating it.
	n, cerr := countCatalogDatabases(ctx, rec)
	var missing *recordlayer.RecordStoreDoesNotExistError
	switch {
	case errors.As(cerr, &missing):
		r.Catalog = "missing (plain-core cluster)"
	case cerr != nil:
		r.Catalog = "error: " + cerr.Error()
	default:
		r.Catalog = fmt.Sprintf("ok (%d databases)", n)
	}

	return writeStatus(out, r, outputFmt)
}

func countCatalogDatabases(ctx context.Context, rec *recordlayer.FDBDatabase) (int, error) {
	return countCatalogDatabasesBatched(ctx, rec, 256)
}

func countCatalogDatabasesBatched(ctx context.Context, rec *recordlayer.FDBDatabase, batchSize int) (int, error) {
	if batchSize <= 0 {
		return 0, fmt.Errorf("catalog count batch size must be positive")
	}
	md, err := catalog.BuildCatalogMetaData()
	if err != nil {
		return 0, err
	}
	type batchResult struct {
		count        int
		continuation []byte
		done         bool
	}
	var continuation []byte
	total := 0
	for {
		result, err := runCatalogRead(ctx, rec, func(rctx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rctx).
				SetSubspace(relationalKeyspace().CatalogSubspace()).SetMetaDataProvider(md).
				SetSkipPossiblyRebuild(true).Open()
			if err != nil {
				return nil, err
			}
			props := recordlayer.ScanProperties{
				ExecuteProperties: recordlayer.DefaultExecutePropertiesIn(rctx.Env()).
					WithIsolationLevel(recordlayer.IsolationLevelSnapshot).
					WithReturnedRowLimit(batchSize).
					WithTimeLimit(3 * time.Second).
					WithScannedBytesLimit(1 << 20),
				CursorStreamingMode: recordlayer.StreamingModeIterator,
			}
			cursor := store.ScanRecordsByType(catalog.DatabasesRecordName, continuation, props)
			defer cursor.Close()
			batch := batchResult{}
			for {
				row, err := cursor.OnNext(ctx)
				if err != nil {
					return nil, err
				}
				if row.HasNext() {
					batch.count++
					continue
				}
				batch.done = !row.HasStoppedBeforeEnd()
				if !batch.done {
					batch.continuation, err = row.GetContinuation().ToBytes()
					if err != nil {
						return nil, err
					}
					if len(batch.continuation) == 0 {
						return nil, fmt.Errorf("catalog scan stopped without a continuation")
					}
				}
				return batch, nil
			}
		})
		if err != nil {
			return 0, err
		}
		batch, ok := result.(batchResult)
		if !ok {
			return 0, fmt.Errorf("catalog count returned unexpected batch %T", result)
		}
		// A retried read attempt must not contribute its count or continuation.
		total += batch.count
		if batch.done {
			return total, nil
		}
		continuation = batch.continuation
	}
}

func writeStatus(out io.Writer, r statusReport, outputFmt string) error {
	if outputFmt == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	fmt.Fprintf(out, "cluster:   %s", r.Cluster)
	if r.ReadVersion > 0 {
		fmt.Fprintf(out, " (read version %d)", r.ReadVersion)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "store:     %s\n", r.Store)
	fmt.Fprintf(out, "metadata:  %s\n", r.Metadata)
	_, err := fmt.Fprintf(out, "catalog:   %s\n", r.Catalog)
	return err
}

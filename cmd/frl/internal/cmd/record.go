package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	configv1 "github.com/birdayz/fdb-record-layer-go/cmd/frl/gen/frl/config/v1"
	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/config"
	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/meta"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// defaultScanLimit caps how many records `record scan` returns per
// invocation when --limit is not set. 100 matches the Java Record Layer
// default and keeps ad-hoc scans bounded — users reach for --limit 0
// (unlimited) explicitly when they want everything.
const defaultScanLimit = 100

func newRecordCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "record",
		Short: "Read records from the current context's store",
	}
	c.AddCommand(
		newRecordGetCmd(),
		newRecordScanCmd(),
		newRecordCountCmd(),
	)
	return c
}

func newRecordGetCmd() *cobra.Command {
	var contextName, metaFile string
	c := &cobra.Command{
		Use:   "get <primary-key>",
		Short: "Load a single record by primary key",
		Long: "Primary keys are parsed as int64 if the argument is a valid " +
			"signed 64-bit integer, otherwise as a string. Values above " +
			"math.MaxInt64 (9223372036854775807) are treated as strings, " +
			"which will miss records saved with uint64 PKs in that range. " +
			"Composite primary keys are not yet supported — open an issue " +
			"if you need them.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgCtx, override, err := resolveContextAndOverride(contextName, metaFile)
			if err != nil {
				return err
			}
			pk := parsePrimaryKey(args[0])
			rec, err := withStore(cmd.Context(), cfgCtx, override,
				func(store *recordlayer.FDBRecordStore) (*recordlayer.FDBStoredRecord[proto.Message], error) {
					return store.LoadRecord(pk)
				})
			if err != nil {
				return err
			}
			if rec == nil {
				return fmt.Errorf("record %v not found in %s", pk, cfgCtx.GetKeyspacePath())
			}
			return writeRecordAsJSON(cmd.OutOrStdout(), rec)
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&metaFile, "meta-file", "", "path to MetaData.pb; overrides context.metadata")
	return c
}

func newRecordScanCmd() *cobra.Command {
	var (
		contextName string
		metaFile    string
		recordType  string
		limit       int
	)
	c := &cobra.Command{
		Use:   "scan",
		Short: "Scan records from the current context's store",
		Long: "Forward scan over the whole store (or a single --type). " +
			"Output is one JSON-encoded record per line (newline-delimited " +
			"JSON) so it can be piped into `jq -s .` or tools like `mlr`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgCtx, override, err := resolveContextAndOverride(contextName, metaFile)
			if err != nil {
				return err
			}
			_, err = withStore(cmd.Context(), cfgCtx, override,
				func(store *recordlayer.FDBRecordStore) (struct{}, error) {
					return struct{}{}, scanAndRender(cmd.Context(), cmd.OutOrStdout(), store, recordType, limit)
				})
			return err
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&metaFile, "meta-file", "", "path to MetaData.pb; overrides context.metadata")
	c.Flags().StringVar(&recordType, "type", "", "filter by record type name; empty means all types")
	c.Flags().IntVar(&limit, "limit", defaultScanLimit, "max records to return; 0 means unlimited")
	return c
}

// parsePrimaryKey produces a single-element tuple. Integers go in as int64
// (record layer stores them in the tuple layer's int representation, and
// `42` from an operator should match a record saved with int64(42) PK).
// Everything else is a string. Composite PKs require explicit tuple syntax
// — deferred until there's a real demand.
func parsePrimaryKey(raw string) tuple.Tuple {
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return tuple.Tuple{n}
	}
	return tuple.Tuple{raw}
}

// resolveContextAndOverride is the shared prelude for record/index
// commands: load the config, pick the context (by --context or current),
// and build the meta-file override Source if --meta-file was supplied.
// Returns the context, an optional meta.Source override, or an error.
func resolveContextAndOverride(contextName, metaFile string) (*configv1.Context, meta.Source, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	cfgCtx, err := config.ResolveContext(cfg, contextName)
	if err != nil {
		if errors.Is(err, config.ErrNoContext) && metaFile == "" {
			path, _ := config.Path()
			return nil, nil, fmt.Errorf("%w (config: %s)", err, path)
		}
		if metaFile == "" {
			return nil, nil, err
		}
		cfgCtx = &configv1.Context{Name: "(cli-flag)"}
	}
	var override meta.Source
	if metaFile != "" {
		override = &meta.FileSource{Path: metaFile}
	}
	return cfgCtx, override, nil
}

// writeRecordAsJSON renders a stored record as a JSON object with three
// fields: primary_key, record_type, and record (the proto-encoded message
// marshalled via protojson so nested messages, enums, and oneofs show up
// with their canonical JSON names).
func writeRecordAsJSON(out io.Writer, rec *recordlayer.FDBStoredRecord[proto.Message]) error {
	payload, err := protojson.MarshalOptions{}.Marshal(rec.Record)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	// We emit an envelope rather than the raw record so downstream tools
	// can tell which type a row is and what its PK was — useful when the
	// scan spans multiple record types.
	rt := ""
	if rec.RecordType != nil {
		rt = rec.RecordType.Name
	}
	_, err = fmt.Fprintf(out,
		`{"primary_key":%q,"record_type":%q,"record":%s}`+"\n",
		formatPK(rec.PrimaryKey), rt, string(payload))
	return err
}

func formatPK(t tuple.Tuple) string {
	parts := make([]string, len(t))
	for i, e := range t {
		parts[i] = fmt.Sprintf("%v", e)
	}
	return strings.Join(parts, ",")
}

func scanAndRender(
	ctx context.Context,
	out io.Writer,
	store *recordlayer.FDBRecordStore,
	recordType string,
	limit int,
) error {
	scanProps := recordlayer.ScanProperties{}
	if limit > 0 {
		scanProps.ExecuteProperties.ReturnedRowLimit = limit
	}

	var cursor recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]]
	if recordType != "" {
		cursor = store.ScanRecordsByType(recordType, nil, scanProps)
	} else {
		cursor = store.ScanRecords(nil, scanProps)
	}
	defer cursor.Close()

	// Limit enforcement lives entirely in ScanProperties.ReturnedRowLimit
	// — the cursor returns HasNext()=false after emitting exactly `limit`
	// rows. No local counter is needed (and would be dead code: the
	// cursor always terminates the loop first).
	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		if !result.HasNext() {
			return nil
		}
		if err := writeRecordAsJSON(out, result.GetValue()); err != nil {
			return err
		}
	}
}

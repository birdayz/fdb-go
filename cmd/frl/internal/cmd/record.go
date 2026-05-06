package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

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
		Example: `  frl record get 42
  frl record get customer-0001
  frl record get 42 --meta-file ./meta.pb`,
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
		reverse     bool
	)
	c := &cobra.Command{
		Use:   "scan",
		Short: "Scan records from the current context's store",
		Example: `  frl record scan --limit 10
  frl record scan --type Order --limit 100 | jq -s .
  frl record scan --reverse --limit 5         # last 5 by PK order
  frl record scan --type Order | wc -l`,
		Long: "Scan over the whole store (or a single --type) in primary-key " +
			"order. Use --reverse to walk the tail first — useful for tail-style " +
			"inspection of the most recently-keyed records. Output is one " +
			"JSON-encoded record per line (newline-delimited JSON) so it can " +
			"be piped into `jq -s .` or tools like `mlr`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgCtx, override, err := resolveContextAndOverride(contextName, metaFile)
			if err != nil {
				return err
			}
			return withStoreE(cmd.Context(), cfgCtx, override,
				func(store *recordlayer.FDBRecordStore) error {
					return scanAndRender(cmd.Context(), cmd.OutOrStdout(), store, recordType, limit, reverse)
				})
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&metaFile, "meta-file", "", "path to MetaData.pb; overrides context.metadata")
	c.Flags().StringVar(&recordType, "type", "", "filter by record type name; empty means all types")
	c.Flags().IntVar(&limit, "limit", defaultScanLimit, "max records to return; 0 means unlimited")
	c.Flags().BoolVar(&reverse, "reverse", false, "scan in reverse primary-key order")
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

// writeRecordAsJSON renders a stored record as a JSON object with three
// fields: primary_key, record_type, and record (the proto-encoded message
// marshalled via protojson so nested messages, enums, and oneofs show up
// with their canonical JSON names).
//
// Uses json.Marshal for the string fields instead of fmt.Sprintf("%q", …):
// %q produces Go-quoted strings which escape NULs as `\x00` (invalid JSON).
// If a PK ever contains a byte that needs \uXXXX encoding, the envelope
// must still parse as JSON — otherwise `jq` breaks mid-pipeline.
func writeRecordAsJSON(out io.Writer, rec *recordlayer.FDBStoredRecord[proto.Message]) error {
	// UseProtoNames: operators wrote their .proto files with snake_case —
	// render field names the same way they declared them, so grep / jq on
	// scan output matches their schema. protojson defaults to lowerCamel
	// which forces a mental translation.
	payload, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(rec.Record)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	rt := ""
	if rec.RecordType != nil {
		rt = rec.RecordType.Name
	}
	// Envelope rather than raw record — downstream tools need the type and
	// PK when a scan spans multiple record types.
	pk, _ := json.Marshal(formatPK(rec.PrimaryKey))
	rtJSON, _ := json.Marshal(rt)
	_, err = fmt.Fprintf(out,
		`{"primary_key":%s,"record_type":%s,"record":%s}`+"\n",
		pk, rtJSON, payload)
	return err
}

// formatPK renders a tuple as a comma-separated string for NDJSON output.
// Each element is formatted per its runtime type so binary keys, UUIDs,
// and versionstamps produce meaningful text rather than Go's default
// "%v" which prints byte slices as `[1 2 3]` (unusable for grep/jq).
//
// Elements we care about:
//   - []byte        → hex (binary PKs are common with UUID / hash PKs)
//   - tuple.UUID    → canonical 8-4-4-4-12 form
//   - tuple.Versionstamp → its own String() which is already compact
//   - string/int64/float/bool → %v (round-trips through parsePrimaryKey)
//   - tuple.Tuple   → recurse, so nested tuples render somewhat sensibly
//
// The output is not intended to round-trip back into a tuple — it's for
// human inspection and downstream text tools. Operators who need the
// exact wire bytes should reach for `store dump` instead.
func formatPK(t tuple.Tuple) string {
	parts := make([]string, len(t))
	for i, e := range t {
		parts[i] = formatTupleElement(e)
	}
	return strings.Join(parts, ",")
}

func formatTupleElement(e any) string {
	switch v := e.(type) {
	case []byte:
		return fmt.Sprintf("%x", v)
	case tuple.UUID:
		return v.String()
	case tuple.Versionstamp:
		return v.String()
	case tuple.Tuple:
		return "(" + formatPK(v) + ")"
	case nil:
		return "<nil>"
	default:
		return fmt.Sprintf("%v", v)
	}
}

func scanAndRender(
	ctx context.Context,
	out io.Writer,
	store *recordlayer.FDBRecordStore,
	recordType string,
	limit int,
	reverse bool,
) error {
	// Up-front type validation: ScanRecordsByType with an unknown name
	// silently falls through to a full-store filter that matches nothing.
	// Operators typo'ing `--type Orders` would get empty output instead
	// of a clear error — expensive too (slow-path full scan). Fail fast.
	if recordType != "" {
		if err := validateRecordType(store.GetRecordMetaData(), recordType); err != nil {
			return err
		}
	}

	scanProps := recordlayer.ForwardScan()
	if reverse {
		scanProps = recordlayer.ReverseScan()
	}
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

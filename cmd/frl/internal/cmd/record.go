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

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
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
		newRecordPutCmd(),
		newRecordDeleteCmd(),
	)
	return c
}

func newRecordGetCmd() *cobra.Command {
	var addr storeAddressFlags
	var recordType string
	c := &cobra.Command{
		Use:   "get <primary-key>",
		Short: "Load a single record by primary key",
		Example: `  frl record get 42
  frl record get customer-0001
  frl record get 1,1 --database /myapp --schema main   # PK as shown by record scan
  frl record get 1 --type ITEMS --database /myapp --schema main`,
		Long: "The primary key is comma-separated tuple elements — exactly " +
			"the form `record scan` prints in its primary_key field, so scan " +
			"output round-trips into get. Each element parses as int64 if it " +
			"is a valid signed 64-bit integer, otherwise as a string (values " +
			"above math.MaxInt64 are treated as strings, which will miss " +
			"records saved with uint64 PKs in that range).\n\n" +
			"--type prepends the record type's key when the type's primary " +
			"key carries a record-type prefix (as relational-layer tables " +
			"do) — `--type ITEMS 1` addresses the same record as `1,1`.\n\n" +
			"--database/--schema address a relational store: keyspace and " +
			"metadata come from the catalog (schema-pinned template version).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := addr.resolve()
			if err != nil {
				return err
			}
			pk := parsePrimaryKey(args[0])
			rec, err := withStore(cmd.Context(), target,
				func(store *recordlayer.FDBRecordStore) (*recordlayer.FDBStoredRecord[proto.Message], error) {
					if recordType != "" {
						rt, err := lookupRecordType(store.GetRecordMetaData(), recordType)
						if err != nil {
							return nil, err
						}
						// Same rule Java callers follow: a PK expression
						// with a record-type prefix means the stored key
						// starts with the type key — prepend it so the
						// operator passes only the logical key part.
						if rt.PrimaryKeyHasRecordTypePrefix() {
							pk = append(tuple.Tuple{rt.GetRecordTypeKey()}, pk...)
						}
					}
					return store.LoadRecord(pk)
				})
			if err != nil {
				return err
			}
			if rec == nil {
				return fmt.Errorf("record %v not found in %s", pk, target.describe())
			}
			return writeRecordAsJSON(cmd.OutOrStdout(), rec)
		},
	}
	addr.register(c, true)
	c.Flags().StringVar(&recordType, "type", "", "record type; prepends its type key for prefix-keyed types")
	return c
}

func newRecordScanCmd() *cobra.Command {
	var (
		addr       storeAddressFlags
		recordType string
		limit      int
		reverse    bool
	)
	c := &cobra.Command{
		Use:   "scan",
		Short: "Scan records from the current context's store",
		Example: `  frl record scan --limit 10
  frl record scan --type Order --limit 100 | jq -s .
  frl record scan --reverse --limit 5         # last 5 by PK order
  frl record scan --database /myapp --schema main --limit 10`,
		Long: "Scan over the whole store (or a single --type) in primary-key " +
			"order. Use --reverse to walk the tail first — useful for tail-style " +
			"inspection of the most recently-keyed records. Output is one " +
			"JSON-encoded record per line (newline-delimited JSON) so it can " +
			"be piped into `jq -s .` or tools like `mlr`.\n\n" +
			"--database/--schema address a relational store: keyspace and " +
			"metadata come from the catalog (schema-pinned template version), " +
			"so SQL-created tables are scannable with zero config.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			target, err := addr.resolve()
			if err != nil {
				return err
			}
			return withStoreE(cmd.Context(), target,
				func(store *recordlayer.FDBRecordStore) error {
					return scanAndRender(cmd.Context(), cmd.OutOrStdout(), store, recordType, limit, reverse)
				})
		},
	}
	addr.register(c, true)
	c.Flags().StringVar(&recordType, "type", "", "filter by record type name; empty means all types")
	c.Flags().IntVar(&limit, "limit", defaultScanLimit, "max records to return; 0 means unlimited")
	c.Flags().BoolVar(&reverse, "reverse", false, "scan in reverse primary-key order")
	return c
}

// parsePrimaryKey parses a comma-separated primary key into a tuple —
// the same comma-separated form formatPK renders in scan envelopes, so
// `record get <primary_key-as-shown-by-scan>` round-trips. Each element
// is int64 if it parses as a signed 64-bit integer (record layer stores
// them in the tuple layer's int representation), otherwise a string.
// String elements containing a literal comma aren't addressable with
// this syntax.
func parsePrimaryKey(raw string) tuple.Tuple {
	parts := strings.Split(raw, ",")
	t := make(tuple.Tuple, len(parts))
	for i, p := range parts {
		if n, err := strconv.ParseInt(p, 10, 64); err == nil {
			t[i] = n
		} else {
			t[i] = p
		}
	}
	return t
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

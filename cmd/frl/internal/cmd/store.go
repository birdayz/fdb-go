package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// The DataStoreInfo header lives at recordlayer.StoreInfoKey — we read
// that constant rather than hard-coding 0 so any future rearrangement
// of subspace IDs flows through transparently.

// newStoreCmd is the `store` noun. v1 ships only `info`.
func newStoreCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "store",
		Short: "Inspect a record store's header and raw contents",
	}
	c.AddCommand(
		newStoreInfoCmd(),
		newStoreDumpCmd(),
	)
	return c
}

func newStoreInfoCmd() *cobra.Command {
	var addr storeAddressFlags
	var outputFmt string
	c := &cobra.Command{
		Use:   "info",
		Short: "Print DataStoreInfo for the current context's store",
		Example: `  frl store info
  frl store info --context prod
  frl store info --database /myapp --schema main
  frl store info -o json | jq '.formatVersion'`,
		Long: "Reads the store header (format version, metadata version, " +
			"user version, record count state, lock state, user fields) " +
			"directly from FDB at the keyspace path in the active context " +
			"(or the relational store addressed by --database/--schema). " +
			"No metadata is loaded — this command works even against a " +
			"store whose metadata isn't yet configured in frl.\n\n" +
			"--output / -o: 'text' (default, human-readable) or 'json' " +
			"(protojson of the raw DataStoreInfo, suitable for jq / " +
			"monitoring systems).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			target, err := addr.resolve()
			if err != nil {
				return err
			}
			return runStoreInfo(cmd.Context(), cmd.OutOrStdout(), target, outputFmt)
		},
	}
	addr.register(c, false)
	c.Flags().StringVarP(&outputFmt, "output", "o", "text",
		"output format: text or json")
	return c
}

func runStoreInfo(ctx context.Context, out io.Writer, target *storeTarget, outputFmt string) error {
	ss, err := target.subspace()
	if err != nil {
		return err
	}

	db, err := openDatabase(target.cfgCtx.GetClusterFile())
	if err != nil {
		return err
	}
	rec := recordlayer.NewFDBDatabase(db)

	info, err := readStoreInfo(ctx, rec, ss)
	if err != nil {
		return err
	}
	if outputFmt == "json" {
		return writeStoreInfoJSON(out, info)
	}
	return writeStoreInfo(out, target, info, ss.Bytes())
}

// writeStoreInfoJSON emits the raw DataStoreInfo proto as indented JSON.
// Uses protojson so enums render as canonical names (e.g.
// "RECORD_COUNT_STATE_READABLE") rather than integer codes. Context
// identity is intentionally omitted — the caller already knows which
// context they asked about; the JSON output is the store's own data.
func writeStoreInfoJSON(out io.Writer, info *gen.DataStoreInfo) error {
	bytes, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(info)
	if err != nil {
		return fmt.Errorf("marshal DataStoreInfo: %w", err)
	}
	_, err = fmt.Fprintln(out, string(bytes))
	return err
}

// fdbAPIVersion is the wire protocol version frl talks to FDB with.
// Pinned to 730 to match pkg/relational/sqldriver, which now opens via
// fdbclient.Open — that selects the default API version 730 (the 7.3.77 server
// version) when the process hasn't already chosen one, instead of the old
// unconditional 720 pin. When `frl sql` and `frl meta catalog` share a process,
// the second call to fdb.APIVersion() errors if the version differs — so both
// paths must agree. (Completes the former "lift both to 730 together" TODO; the
// recordlayer tests already use 730.)
const fdbAPIVersion = 730

// openDatabase opens an FDB connection via the pure-Go client. The API
// version is idempotently set on every call — the pure-Go client accepts
// re-setting to the same version.
func openDatabase(clusterFile string) (fdb.Database, error) {
	// Sentence-leading "FDB API" capitalises naturally under fang's
	// banner, unlike "fdb.APIVersion(…)" which would render as
	// "Fdb.APIVersion …" (ugly).
	if err := fdb.APIVersion(fdbAPIVersion); err != nil {
		return fdb.Database{}, fmt.Errorf("FDB API version %d: %w", fdbAPIVersion, err)
	}
	if clusterFile == "" {
		return fdb.OpenDefault()
	}
	return fdb.OpenDatabase(clusterFile)
}

// parseKeyspacePath parses the operator-facing "/foo/bar/baz" syntax into a
// tuple-based FDB subspace. Each path segment is packed as a string element,
// which covers the common case of directory-style keyspaces. Apps with
// typed keyspaces (int/UUID components) need more than this; left for v2.
func parseKeyspacePath(path string) (subspace.Subspace, error) {
	// The error messages quote `keyspace_path` (the config key name) so
	// fang's capitalized banner doesn't turn the prose into "Keyspace_path
	// …" — which looks like a typo.
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil, fmt.Errorf("empty `keyspace_path` — %q resolves to an empty tuple", path)
	}
	parts := strings.Split(trimmed, "/")
	elems := make([]tuple.TupleElement, len(parts))
	for i, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("bad `keyspace_path` — %q has an empty segment", path)
		}
		elems[i] = p
	}
	return subspace.Sub(elems...), nil
}

// keyHex renders an fdb.Key as plain lowercase hex of its raw bytes —
// paste-able into `fdbcli getrange`. NEVER format an fdb.Key with %x
// directly: fdb.Key implements Stringer (Printable escaping), and fmt
// routes %x through String(), so `%x` on the key hexes the *escaped
// text* ("\x02dev\x00" → "5c7830326465765c783030…") instead of the key
// bytes ("026465760014").
func keyHex(k fdb.Key) string {
	return fmt.Sprintf("%x", []byte(k))
}

// readStoreInfo reads the DataStoreInfo proto directly from FDB at the
// store's StoreInfoKey subspace, without going through a FDBRecordStore
// builder (which would require metadata we intentionally don't load here).
func readStoreInfo(ctx context.Context, rec *recordlayer.FDBDatabase, ss subspace.Subspace) (*gen.DataStoreInfo, error) {
	key := ss.Pack(tuple.Tuple{recordlayer.StoreInfoKey})
	result, err := rec.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		return rtx.Transaction().Get(key).Get()
	})
	if err != nil {
		return nil, fmt.Errorf("read store header at %s: %w", keyHex(key), err)
	}
	bytes, _ := result.([]byte)
	if len(bytes) == 0 {
		return nil, fmt.Errorf("no store header at keyspace %s — store does not exist", keyHex(key))
	}
	info := &gen.DataStoreInfo{}
	if err := proto.Unmarshal(bytes, info); err != nil {
		return nil, fmt.Errorf("unmarshal DataStoreInfo: %w", err)
	}
	return info, nil
}

// writeStoreInfo renders a DataStoreInfo as a compact human-readable text
// block on out. Not JSON — JSON output is a future -o=json concern.
// subspacePrefix is the packed FDB byte prefix of the store's keyspace;
// rendered in hex so operators can paste it directly into `fdbcli getrange`.
// Nil means the caller didn't resolve a prefix (rare — tests only).
func writeStoreInfo(out io.Writer, target *storeTarget, info *gen.DataStoreInfo, subspacePrefix []byte) error {
	cfgCtx := target.cfgCtx
	var b strings.Builder
	fmt.Fprintf(&b, "Context:           %s\n", cfgCtx.GetName())
	fmt.Fprintf(&b, "Cluster file:      %s\n", orDefault(cfgCtx.GetClusterFile(), "(default)"))
	if target.relational() {
		fmt.Fprintf(&b, "Database/schema:   %s/%s\n", target.database, target.schema)
	} else {
		fmt.Fprintf(&b, "Keyspace path:     %s\n", cfgCtx.GetKeyspacePath())
	}
	if len(subspacePrefix) > 0 {
		fmt.Fprintf(&b, "FDB prefix (hex):  %x\n", subspacePrefix)
	}
	fmt.Fprintln(&b, "")
	fmt.Fprintf(&b, "Format version:    %d\n", info.GetFormatVersion())
	fmt.Fprintf(&b, "Metadata version:  %d\n", info.GetMetaDataversion())
	fmt.Fprintf(&b, "User version:      %d\n", info.GetUserVersion())
	fmt.Fprintf(&b, "Cacheable:         %t\n", info.GetCacheable())
	fmt.Fprintf(&b, "Record count:      %s\n", recordCountStateName(info.GetRecordCountState()))
	fmt.Fprintf(&b, "Lock state:        %s\n", lockStateDescription(info.GetStoreLockState()))
	// Incarnation is only interesting when non-zero — it's bumped on
	// cross-cluster data migrations so that `version()` values don't
	// collide with the old cluster's. Hide the default so operators
	// aren't distracted by it on stores that never migrated.
	if incarnation := info.GetIncarnation(); incarnation != 0 {
		fmt.Fprintf(&b, "Incarnation:       %d\n", incarnation)
	}
	// omit_unsplit_record_suffix gates whether unsplit records carry a
	// trailing split suffix byte. Legacy stores created before
	// FormatVersion.SAVE_UNSPLIT_WITH_SUFFIX set this to true and can
	// never upgrade to split-long-records. Shown only when set, because
	// the overwhelming majority of new stores leave it false.
	if info.GetOmitUnsplitRecordSuffix() {
		fmt.Fprintf(&b, "Unsplit suffix:    omitted (legacy store, can't enable split-long-records)\n")
	}
	if ts := info.GetLastUpdateTime(); ts != 0 {
		// Record-layer writes LastUpdateTime as ms-epoch (int64). Render
		// both the raw value and an RFC3339 human timestamp so operators
		// can spot-check staleness at a glance.
		fmt.Fprintf(&b, "Last updated:      %s (%d ms epoch)\n",
			time.UnixMilli(int64(ts)).UTC().Format(time.RFC3339), ts)
	}
	if fields := info.GetUserField(); len(fields) > 0 {
		fmt.Fprintln(&b, "User fields:")
		for _, f := range fields {
			fmt.Fprintf(&b, "  %s: %d bytes\n", f.GetKey(), len(f.GetValue()))
		}
	}
	_, err := out.Write([]byte(b.String()))
	return err
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func recordCountStateName(s gen.DataStoreInfo_RecordCountState) string {
	switch s {
	case gen.DataStoreInfo_READABLE:
		return "readable"
	case gen.DataStoreInfo_WRITE_ONLY:
		return "write-only"
	case gen.DataStoreInfo_DISABLED:
		return "disabled"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

func lockStateDescription(lock *gen.DataStoreInfo_StoreLockState) string {
	if lock == nil || lock.GetLockState() == gen.DataStoreInfo_StoreLockState_UNSPECIFIED {
		return "unlocked"
	}
	name := lock.GetLockState().String()
	if reason := lock.GetReason(); reason != "" {
		return fmt.Sprintf("%s (%s)", name, reason)
	}
	return name
}

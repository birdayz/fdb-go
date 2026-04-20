package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	configv1 "github.com/birdayz/fdb-record-layer-go/cmd/frl/gen/frl/config/v1"
	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/config"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// The DataStoreInfo header lives at recordlayer.StoreInfoKey — we read
// that constant rather than hard-coding 0 so any future rearrangement
// of subspace IDs flows through transparently.

// newStoreCmd is the `store` noun. v1 ships only `info`.
func newStoreCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "store",
		Short: "Inspect a record store (header, lifecycle)",
	}
	c.AddCommand(
		newStoreInfoCmd(),
		newStoreDumpCmd(),
	)
	return c
}

func newStoreInfoCmd() *cobra.Command {
	var contextName, outputFmt string
	c := &cobra.Command{
		Use:   "info",
		Short: "Print DataStoreInfo for the current context's store",
		Example: `  frl store info
  frl store info --context prod
  frl store info -o json | jq '.formatVersion'`,
		Long: "Reads the store header (format version, metadata version, " +
			"user version, record count state, lock state, user fields) " +
			"directly from FDB at the keyspace path in the active context. " +
			"No metadata is loaded — this command works even against a " +
			"store whose metadata isn't yet configured in frl.\n\n" +
			"--output / -o: 'text' (default, human-readable) or 'json' " +
			"(protojson of the raw DataStoreInfo, suitable for jq / " +
			"monitoring systems).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch outputFmt {
			case "", "text", "json":
				// ok
			default:
				return fmt.Errorf("invalid --output %q: want text or json", outputFmt)
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx, err := config.ResolveContext(cfg, contextName)
			if err != nil {
				if errors.Is(err, config.ErrNoContext) {
					path, _ := config.Path()
					return fmt.Errorf("%w (config: %s)", err, path)
				}
				return err
			}
			return runStoreInfo(cmd.Context(), cmd.OutOrStdout(), ctx, outputFmt)
		},
	}
	c.Flags().StringVar(&contextName, "context", "",
		"context name to use (default: Config.current_context)")
	c.Flags().StringVarP(&outputFmt, "output", "o", "text",
		"output format: text or json")
	return c
}

func runStoreInfo(ctx context.Context, out interface{ Write([]byte) (int, error) }, cfgCtx *configv1.Context, outputFmt string) error {
	if cfgCtx.GetKeyspacePath() == "" {
		return fmt.Errorf("context %q has empty keyspace_path; add it to the config",
			cfgCtx.GetName())
	}

	db, err := openDatabase(cfgCtx.GetClusterFile())
	if err != nil {
		return err
	}
	rec := recordlayer.NewFDBDatabase(db)

	ss, err := parseKeyspacePath(cfgCtx.GetKeyspacePath())
	if err != nil {
		return err
	}

	info, err := readStoreInfo(ctx, rec, ss)
	if err != nil {
		return err
	}
	if outputFmt == "json" {
		return writeStoreInfoJSON(out, info)
	}
	return writeStoreInfo(out, cfgCtx, info)
}

// writeStoreInfoJSON emits the raw DataStoreInfo proto as indented JSON.
// Uses protojson so enums render as canonical names (e.g.
// "RECORD_COUNT_STATE_READABLE") rather than integer codes. Context
// identity is intentionally omitted — the caller already knows which
// context they asked about; the JSON output is the store's own data.
func writeStoreInfoJSON(out interface{ Write([]byte) (int, error) }, info *gen.DataStoreInfo) error {
	bytes, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(info)
	if err != nil {
		return fmt.Errorf("marshal DataStoreInfo: %w", err)
	}
	_, err = fmt.Fprintln(&writerAdapter{out}, string(bytes))
	return err
}

// writerAdapter bridges the minimal "io.Writer-ish" interface the store
// helpers accept to io.Writer so fmt.Fprintln can use it. Cheap enough
// that defining a whole io.Writer-typed parameter isn't worth the churn.
type writerAdapter struct {
	inner interface{ Write([]byte) (int, error) }
}

func (w *writerAdapter) Write(p []byte) (int, error) { return w.inner.Write(p) }

// fdbAPIVersion is the wire protocol version frl talks to FDB with. Must
// match what the record-layer library and testcontainers use (730 today;
// the lib tests pin this value — see pkg/fdbgo/fdb/testmain_test.go).
const fdbAPIVersion = 730

// openDatabase opens an FDB connection via the pure-Go client. The API
// version is idempotently set on every call — the pure-Go client accepts
// re-setting to the same version.
func openDatabase(clusterFile string) (fdb.Database, error) {
	if err := fdb.APIVersion(fdbAPIVersion); err != nil {
		return fdb.Database{}, fmt.Errorf("fdb.APIVersion(%d): %w", fdbAPIVersion, err)
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
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil, fmt.Errorf("keyspace_path %q resolves to an empty tuple", path)
	}
	parts := strings.Split(trimmed, "/")
	elems := make([]tuple.TupleElement, len(parts))
	for i, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("keyspace_path %q has an empty segment", path)
		}
		elems[i] = p
	}
	return subspace.Sub(elems...), nil
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
		return nil, fmt.Errorf("read store header at %x: %w", key, err)
	}
	bytes, _ := result.([]byte)
	if len(bytes) == 0 {
		return nil, fmt.Errorf("no store header at keyspace %x — store does not exist", key)
	}
	info := &gen.DataStoreInfo{}
	if err := proto.Unmarshal(bytes, info); err != nil {
		return nil, fmt.Errorf("unmarshal DataStoreInfo: %w", err)
	}
	return info, nil
}

// writeStoreInfo renders a DataStoreInfo as a compact human-readable text
// block on out. Not JSON — JSON output is a future -o=json concern.
func writeStoreInfo(out interface{ Write([]byte) (int, error) }, cfgCtx *configv1.Context, info *gen.DataStoreInfo) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Context:           %s\n", cfgCtx.GetName())
	fmt.Fprintf(&b, "Cluster file:      %s\n", orDefault(cfgCtx.GetClusterFile(), "(default)"))
	fmt.Fprintf(&b, "Keyspace path:     %s\n", cfgCtx.GetKeyspacePath())
	fmt.Fprintln(&b, "")
	fmt.Fprintf(&b, "Format version:    %d\n", info.GetFormatVersion())
	fmt.Fprintf(&b, "Metadata version:  %d\n", info.GetMetaDataversion())
	fmt.Fprintf(&b, "User version:      %d\n", info.GetUserVersion())
	fmt.Fprintf(&b, "Cacheable:         %t\n", info.GetCacheable())
	fmt.Fprintf(&b, "Record count:      %s\n", recordCountStateName(info.GetRecordCountState()))
	fmt.Fprintf(&b, "Lock state:        %s\n", lockStateDescription(info.GetStoreLockState()))
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

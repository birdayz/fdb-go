package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	configv1 "github.com/birdayz/fdb-record-layer-go/cmd/frl/gen/frl/config/v1"
	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/config"
	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/meta"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// newMetaCmd is the `meta` noun.
func newMetaCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "meta",
		Short: "Inspect the RecordMetaData for the current context",
	}
	c.AddCommand(
		newMetaGetCmd(),
		newMetaTypesCmd(),
		newMetaValidateCmd(),
		newMetaEvolveCheckCmd(),
		newMetaDiffCmd(),
	)
	return c
}

// newMetaValidateCmd validates a standalone MetaData .pb file. Run it on
// the artifact your deploy pipeline produced before shipping — if this
// passes, the file parses AND passes all structural invariants the
// record-layer enforces at Build() time. No FDB needed.
func newMetaValidateCmd() *cobra.Command {
	var path string
	c := &cobra.Command{
		Use:   "validate",
		Short: "Validate a standalone MetaData.pb file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			src := &meta.FileSource{Path: path}
			if _, err := src.Load(cmd.Context()); err != nil {
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "ok: %s parses and validates\n", path)
			return err
		},
	}
	c.Flags().StringVar(&path, "file", "", "path to MetaData.pb (required)")
	_ = c.MarkFlagRequired("file")
	return c
}

// newMetaEvolveCheckCmd compares two MetaData files and reports whether
// evolving from --old to --new is safe (Java-compatible per
// MetaDataEvolutionValidator). Intended for CI pre-merge checks — catch
// migration bugs before they hit FDBMetaDataStore.saveRecordMetaData().
func newMetaEvolveCheckCmd() *cobra.Command {
	var oldPath, newPath string
	c := &cobra.Command{
		Use:   "evolve-check",
		Short: "Verify an evolution from --old to --new metadata is safe",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			oldMeta, err := (&meta.FileSource{Path: oldPath}).Load(cmd.Context())
			if err != nil {
				return fmt.Errorf("load --old: %w", err)
			}
			newMeta, err := (&meta.FileSource{Path: newPath}).Load(cmd.Context())
			if err != nil {
				return fmt.Errorf("load --new: %w", err)
			}
			validator := recordlayer.NewMetaDataEvolutionValidator().Build()
			if err := validator.Validate(oldMeta, newMeta); err != nil {
				return fmt.Errorf("incompatible evolution: %w", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "ok: %s -> %s is a valid evolution\n", oldPath, newPath)
			return err
		},
	}
	c.Flags().StringVar(&oldPath, "old", "", "path to the existing MetaData.pb (required)")
	c.Flags().StringVar(&newPath, "new", "", "path to the proposed MetaData.pb (required)")
	_ = c.MarkFlagRequired("old")
	_ = c.MarkFlagRequired("new")
	return c
}

// newMetaTypesCmd covers `meta types {ls}` — record-type introspection.
// Kept nested under `meta` rather than promoted to a top-level noun because
// record types live inside metadata; treating them separately would suggest
// they have independent lifecycle which they don't.
func newMetaTypesCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "types",
		Short: "Inspect record types declared in the metadata",
	}
	c.AddCommand(
		newMetaTypesLsCmd(),
		newMetaTypesDescribeCmd(),
	)
	return c
}

func newMetaTypesLsCmd() *cobra.Command {
	var contextName, metaFile, outputFmt string
	c := &cobra.Command{
		Use:   "ls",
		Short: "List record types with primary-key fields",
		Long: "Lists every record type in the metadata with its primary-key " +
			"fields. Note: FDB-store metadata sources are not yet supported " +
			"by this command; configure `meta_file` in your context or use " +
			"--meta-file.\n\n" +
			"--output / -o: 'text' (default, tabwriter) or 'json' (array of " +
			"{name, primary_key, since_version}).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch outputFmt {
			case "", "text", "json":
				// ok
			default:
				return fmt.Errorf("invalid --output %q: want text or json", outputFmt)
			}
			cfgCtx, override, err := resolveContextAndOverride(contextName, metaFile)
			if err != nil {
				return err
			}
			if override == nil {
				src, err := meta.FromContext(cfgCtx, nil, nil)
				if err != nil {
					if errors.Is(err, meta.ErrMissingSource) {
						return fmt.Errorf("%w (context %q)", err, cfgCtx.GetName())
					}
					return err
				}
				override = src
			}
			md, err := override.Load(cmd.Context())
			if err != nil {
				return err
			}
			if outputFmt == "json" {
				return writeTypesListJSON(cmd.OutOrStdout(), md)
			}
			return writeTypesList(cmd.OutOrStdout(), md)
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&metaFile, "meta-file", "", "path to MetaData.pb; overrides context.metadata")
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}

// typesListRow is the JSON row shape emitted by `meta types ls -o json`.
type typesListRow struct {
	Name         string `json:"name"`
	PrimaryKey   string `json:"primary_key"`
	SinceVersion int    `json:"since_version,omitempty"` // 0 elided per convention
}

// writeTypesListJSON renders the record-type list as a JSON array sorted
// by name. Shape matches writeTypesList's columns one-to-one so operators
// can switch between formats without re-learning fields.
func writeTypesListJSON(out io.Writer, md *recordlayer.RecordMetaData) error {
	rts := md.RecordTypes()
	names := make([]string, 0, len(rts))
	for n := range rts {
		names = append(names, n)
	}
	sort.Strings(names)

	rows := make([]typesListRow, 0, len(names))
	for _, name := range names {
		rt := rts[name]
		pk := "(unset)"
		if rt.PrimaryKey != nil {
			if fn := rt.PrimaryKey.FieldNames(); len(fn) > 0 {
				pk = strings.Join(fn, ",")
			}
		}
		rows = append(rows, typesListRow{
			Name:         name,
			PrimaryKey:   pk,
			SinceVersion: rt.SinceVersion,
		})
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

func newMetaGetCmd() *cobra.Command {
	var contextName, metaFile string
	c := &cobra.Command{
		Use:   "get",
		Short: "Dump the loaded RecordMetaData as JSON",
		Long: "Loads the current context's MetaData and prints it as JSON. " +
			"--meta-file overrides the context's metadata source with a " +
			"file on disk (useful for ad-hoc inspection without editing " +
			"the config file). Note: FDB-store metadata sources are not " +
			"yet supported by this command; configure `meta_file` in your " +
			"context or pass --meta-file.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx, err := config.ResolveContext(cfg, contextName)
			if err != nil {
				if errors.Is(err, config.ErrNoContext) && metaFile == "" {
					path, _ := config.Path()
					return fmt.Errorf("%w (config: %s)", err, path)
				}
				if metaFile == "" {
					return err
				}
				// --meta-file was supplied and no context resolves; fall
				// back to a synthetic context that uses only the file.
				ctx = &configv1.Context{Name: "(cli-flag)"}
			}
			if metaFile != "" {
				ctx = applyMetaFileOverride(ctx, metaFile)
			}
			return runMetaGet(cmd, ctx)
		},
	}
	c.Flags().StringVar(&contextName, "context", "",
		"context name to use (default: Config.current_context)")
	c.Flags().StringVar(&metaFile, "meta-file", "",
		"path to a serialized MetaData.pb file; overrides context.metadata")
	return c
}

// applyMetaFileOverride returns a proto.Clone of ctx with its metadata
// source replaced by a file-backed one. Cloning (rather than a shallow
// struct copy) is required because protobuf messages embed a MessageState
// containing a sync.Mutex — copylocks nogo analyzer catches raw struct
// copies as a bug.
func applyMetaFileOverride(ctx *configv1.Context, path string) *configv1.Context {
	cp := proto.Clone(ctx).(*configv1.Context)
	cp.Metadata = &configv1.MetadataSource{
		Source: &configv1.MetadataSource_MetaFile{MetaFile: path},
	}
	return cp
}

// writeTypesList renders one row per record type: name, primary-key
// fields (comma-joined), "since" version (when the type entered the
// metadata). Since-version 0 is treated as "never upgraded" — suppress it.
func writeTypesList(out interface{ Write([]byte) (int, error) }, md *recordlayer.RecordMetaData) error {
	rts := md.RecordTypes()
	if len(rts) == 0 {
		_, err := fmt.Fprintln(out, "(no record types in metadata)")
		return err
	}
	names := make([]string, 0, len(rts))
	for n := range rts {
		names = append(names, n)
	}
	sort.Strings(names)

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPRIMARY KEY\tSINCE VERSION")
	for _, name := range names {
		rt := rts[name]
		pk := "(unset)"
		if rt.PrimaryKey != nil {
			fn := rt.PrimaryKey.FieldNames()
			if len(fn) > 0 {
				pk = strings.Join(fn, ",")
			}
		}
		since := ""
		if rt.SinceVersion > 0 {
			since = fmt.Sprintf("%d", rt.SinceVersion)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, pk, since)
	}
	return tw.Flush()
}

func runMetaGet(cmd *cobra.Command, cfgCtx *configv1.Context) error {
	// Build a Source using only the file-source path (no DB). Any
	// fdb_store context would require a keyspace resolver and DB handle;
	// for now `meta get` only supports file sources without opening FDB.
	// FDB-backed metadata reads are wired when the store-opening plumbing
	// is shared with `store info` / `record *` in a later step.
	src, err := meta.FromContext(cfgCtx, nil, nil)
	if err != nil {
		if errors.Is(err, meta.ErrMissingSource) {
			return fmt.Errorf("%w (context %q)", err, cfgCtx.GetName())
		}
		return err
	}
	md, err := src.Load(cmd.Context())
	if err != nil {
		return err
	}
	mdProto, err := md.ToProto()
	if err != nil {
		return fmt.Errorf("render metadata: %w", err)
	}
	out, err := protojson.MarshalOptions{
		Multiline: true,
		Indent:    "  ",
	}.Marshal(mdProto)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(out))
	return err
}

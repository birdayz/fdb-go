package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	configv1 "github.com/birdayz/fdb-record-layer-go/cmd/frl/gen/frl/config/v1"
	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/config"
	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/meta"
)

// newMetaCmd is the `meta` noun. v1 ships only `get` — dump the loaded
// RecordMetaData from the current context's metadata source as JSON.
func newMetaCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "meta",
		Short: "Inspect the RecordMetaData for the current context",
	}
	c.AddCommand(newMetaGetCmd())
	return c
}

func newMetaGetCmd() *cobra.Command {
	var contextName, metaFile string
	c := &cobra.Command{
		Use:   "get",
		Short: "Dump the loaded RecordMetaData as JSON",
		Long: "Resolves the current context's metadata source (file or " +
			"FDBMetaDataStore), loads the MetaData, and prints it as JSON. " +
			"--meta-file overrides the context's metadata source with a " +
			"file on disk (useful for ad-hoc inspection without editing " +
			"the config file).",
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

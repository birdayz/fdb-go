package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"fdb.dev/cmd/frl/internal/meta"
	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// newMetaApplyCmd is the schema-evolution write (RFC-174 §3.3): validate
// a new MetaData file against what an FDBMetaDataStore currently holds
// (the exact MetaDataEvolutionValidator gate `meta evolve-check` runs),
// then persist it with SaveRecordMetaData.
//
// Write target (codex P2-1): the command writes to an FDBMetaDataStore,
// so it needs one — the context's `meta_store_keyspace` (Path B) or an
// explicit --meta-store-keyspace. A Path A context (meta_file only) has
// no store in FDB to apply to; the error says exactly that and names the
// options. Catalog-backed (relational) metadata is NOT applicable here
// by design — templates evolve through SQL DDL, never behind the
// relational layer's back.
func newMetaApplyCmd() *cobra.Command {
	var (
		contextName          string
		clusterFile          string
		file                 string
		storeKeyspace        string
		forceInitial         bool
		yes                  bool
		allowNoVersionChange bool
		allowIndexRebuilds   bool
		allowUnsplitToSplit  bool
	)
	c := &cobra.Command{
		Use:   "apply --file <new.pb>",
		Short: "Validate and persist metadata into an FDBMetaDataStore (write)",
		Example: `  frl meta apply --file new.pb --yes
  frl meta apply --file new.pb --meta-store-keyspace /myapp/_meta --yes
  frl meta apply --file first.pb --force-initial --yes   # empty store bootstrap`,
		Long: "Loads the CURRENT metadata from the FDBMetaDataStore, runs " +
			"MetaDataEvolutionValidator (same gate and --allow-* knobs as " +
			"`meta evolve-check`) against --file, and on pass persists the " +
			"new metadata via SaveRecordMetaData. Refuses on validation " +
			"failure — this command cannot apply an evolution the record " +
			"layer itself would reject.\n\n" +
			"The write target is the context's `meta_store_keyspace` or " +
			"--meta-store-keyspace. Path A setups (meta_file shipped with " +
			"binaries) have nothing in FDB to apply to — see the operator " +
			"guide for migrating Path A → Path B.\n\n" +
			"--force-initial covers the first write into an empty store " +
			"(no old metadata to validate against).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return fmt.Errorf("missing required flag --file (the new MetaData.pb to apply)")
			}
			f := storeAddressFlags{contextName: contextName, clusterFile: clusterFile}
			target, err := f.resolve()
			if err != nil {
				return err
			}
			ksPath := storeKeyspace
			if ksPath == "" {
				ksPath = target.cfgCtx.GetMetadata().GetMetaStoreKeyspace()
			}
			if ksPath == "" {
				return fmt.Errorf("no FDBMetaDataStore to apply to: the context has no `meta_store_keyspace` and --meta-store-keyspace was not given. Path A setups (meta_file) keep metadata outside FDB — see the operator guide's Path A → Path B migration section")
			}
			ss, err := parseKeyspacePath(ksPath)
			if err != nil {
				return err
			}
			if err := guardNotCatalog(ss); err != nil {
				return err
			}

			newMeta, err := (&meta.FileSource{Path: file}).Load(cmd.Context())
			if err != nil {
				return fmt.Errorf("load --file: %w", err)
			}
			newProto, err := newMeta.ToProto()
			if err != nil {
				return fmt.Errorf("serialize new metadata: %w", err)
			}

			db, err := openDatabase(target.clusterFile())
			if err != nil {
				return err
			}
			rec := recordlayer.NewFDBDatabase(db)
			metaStore := recordlayer.NewFDBMetaDataStore(ss)

			// Load current metadata for the validation diff.
			result, err := rec.Run(cmd.Context(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
				return metaStore.LoadRecordMetaDataProto(rtx.Transaction())
			})
			if err != nil {
				return fmt.Errorf("read current metadata from %s: %w", ksPath, err)
			}
			oldProto, _ := result.(*gen.MetaData)
			switch {
			case oldProto == nil && !forceInitial:
				return fmt.Errorf("no metadata stored at %s — pass --force-initial for the first write into an empty store", ksPath)
			case oldProto != nil:
				oldMeta, err := recordlayer.RecordMetaDataFromProto(oldProto)
				if err != nil {
					return fmt.Errorf("current metadata at %s does not build: %w", ksPath, err)
				}
				validator := recordlayer.NewMetaDataEvolutionValidator().
					SetAllowNoVersionChange(allowNoVersionChange).
					SetAllowIndexRebuilds(allowIndexRebuilds).
					SetAllowUnsplitToSplit(allowUnsplitToSplit).
					Build()
				if err := validator.Validate(oldMeta, newMeta); err != nil {
					return fmt.Errorf("incompatible evolution — refusing to apply: %w", err)
				}
			}

			action := fmt.Sprintf("apply metadata version %d from %s to %s", newMeta.Version(), file, ksPath)
			if oldProto != nil {
				action = fmt.Sprintf("apply metadata %s (version %d → %d)", ksPath, oldProto.GetVersion(), newMeta.Version())
			}
			if err := confirmWrite(cmd, yes, action); err != nil {
				return err
			}

			if _, err := rec.Run(cmd.Context(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
				return nil, metaStore.SaveRecordMetaData(rtx.Transaction(), newProto)
			}); err != nil {
				return fmt.Errorf("save metadata: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "applied metadata version %d to %s\n", newMeta.Version(), ksPath)
			return nil
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&clusterFile, "cluster-file", "", "FDB cluster file; overrides the context's cluster_file")
	c.Flags().StringVar(&file, "file", "", "new MetaData.pb to validate and persist (required)")
	c.Flags().StringVar(&storeKeyspace, "meta-store-keyspace", "", "FDBMetaDataStore keyspace path; overrides the context's meta_store_keyspace")
	c.Flags().BoolVar(&forceInitial, "force-initial", false, "allow the first write into an empty metadata store")
	c.Flags().BoolVar(&yes, "yes", false, "skip the interactive confirmation")
	c.Flags().BoolVar(&allowNoVersionChange, "allow-no-version-change", false, "permit equal versions (validator knob)")
	c.Flags().BoolVar(&allowIndexRebuilds, "allow-index-rebuilds", false, "permit changes that force index rebuilds (validator knob)")
	c.Flags().BoolVar(&allowUnsplitToSplit, "allow-unsplit-to-split", false, "permit the unsplit→split format migration (validator knob)")
	return c
}

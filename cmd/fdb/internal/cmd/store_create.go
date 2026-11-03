package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"fdb.dev/pkg/recordlayer"
)

func newStoreCreateCmd() *cobra.Command {
	var addr storeAddressFlags
	var yes bool
	c := &cobra.Command{
		Use:   "create",
		Short: "Create an empty native Record Layer store (write)",
		Long: "Creates the store header and initializes its indexes from the selected " +
			"metadata source. Refuses an existing store or a nonempty prefix; " +
			"it never replaces records or changes an existing store's metadata.\n\n" +
			"Definitions remain in the selected descriptor set, metadata file, or " +
			"FDBMetaDataStore; they are not copied into the record-store subspace. " +
			"SQL-managed stores must be created through SQL DDL instead.",
		Example: `  fdb rl store create --yes
  fdb rl store create --keyspace-tuple '["myapp", "contacts"]' --descriptor-set schema.binpb --yes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			target, err := addr.resolve()
			if err != nil {
				return err
			}
			if target.relational() {
				return fmt.Errorf("SQL-managed stores are created through SQL DDL, not `fdb rl store create`")
			}
			ss, err := target.subspace()
			if err != nil {
				return err
			}
			if err := guardNotCatalog(ss); err != nil {
				return err
			}
			db, err := openDatabase(target.clusterFile())
			if err != nil {
				return err
			}
			rec := recordlayer.NewFDBDatabase(db)
			src, err := resolveMetaSource(target, rec)
			if err != nil {
				return err
			}
			md, err := src.Load(cmd.Context())
			if err != nil {
				return err
			}
			if err := confirmWrite(cmd, yes, "create store "+target.describe()); err != nil {
				return err
			}
			_, err = rec.Run(cmd.Context(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
				return recordlayer.NewStoreBuilder().SetContext(rtx).SetSubspace(ss).SetMetaDataProvider(md).Create()
			})
			if err != nil {
				return fmt.Errorf("create store at %s: %w", target.describe(), err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Created store at %s (metadata version %d).\n", target.describe(), md.Version())
			return err
		},
	}
	addr.register(c, true)
	c.Flags().BoolVar(&yes, "yes", false, "skip the interactive confirmation")
	return c
}

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// The record write commands (RFC-174 §3.3). Both carry the full guarded
// model: --dry-run exercises the store's dry-run primitives (all
// validation and index maintenance planning, no writes), and the real
// mutation requires --yes or an interactive confirm — put included
// (codex P2-2: put overwrites and bypasses SQL-level constraints, it
// gets the same gate as delete, not a lighter one).
//
// Store opens here go through withStore and keep
// SetSkipPossiblyRebuild(true): record put/delete never migrate the
// store's format as a side effect — `meta apply` is the explicit
// evolution path. `index build` is the one deliberate exception: it
// hands the store builder to OnlineIndexer, whose internal opens run
// the regular check-version path exactly like Java's
// IndexingBase.openRecordStore — building against newer metadata
// migrates the store first, just as deploying that metadata in an app
// would.

func newRecordPutCmd() *cobra.Command {
	var (
		addr       storeAddressFlags
		recordType string
		dryRun     bool
		yes        bool
	)
	c := &cobra.Command{
		Use:   "put --type <T> <json>",
		Short: "Save one record (write)",
		Example: `  frl record put --type Order '{"order_id": 9, "price": 150}' --dry-run
  frl record put --type Order '{"order_id": 9, "price": 150}' --yes
  frl record put --type ITEMS '{"ID": 7, "NAME": "eta"}' --database /myapp --schema main --yes`,
		Long: "Parses <json> as protojson against the record type's descriptor " +
			"and saves it — index maintenance and uniqueness checks run " +
			"transactionally, exactly as an app write would. Overwrites any " +
			"existing record with the same primary key (the confirm prompt " +
			"names it).\n\n" +
			"--dry-run runs every validation without writing and prints the " +
			"would-be-saved envelope. The real write requires --yes or an " +
			"interactive confirmation.\n\n" +
			"NOTE: record-layer writes bypass SQL-level constraints not " +
			"encoded in RecordMetaData — see the operator guide's write-" +
			"commands section before using this against relational stores.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if recordType == "" {
				return fmt.Errorf("missing required flag --type (the record type to parse <json> as)")
			}
			target, err := addr.resolve()
			if err != nil {
				return err
			}
			ss, err := target.subspace()
			if err != nil {
				return err
			}
			if err := guardNotCatalog(ss); err != nil {
				return err
			}

			// Dry-run pass first, always: it validates the JSON against
			// the descriptor and computes the PK — which the confirm
			// prompt needs — without writing anything.
			staged, err := withStore(cmd.Context(), target,
				func(store *recordlayer.FDBRecordStore) (*recordlayer.FDBStoredRecord[proto.Message], error) {
					msg, err := parseRecordJSON(store.GetRecordMetaData(), recordType, args[0])
					if err != nil {
						return nil, err
					}
					return store.DryRunSaveRecord(msg, recordlayer.RecordExistenceCheckNone)
				})
			if err != nil {
				return err
			}
			if dryRun {
				fmt.Fprintln(cmd.ErrOrStderr(), "dry run — nothing written; would save:")
				return writeRecordAsJSON(cmd.OutOrStdout(), staged)
			}
			if err := confirmWrite(cmd, yes, fmt.Sprintf("save %s record with primary key %s into %s",
				recordType, formatPK(staged.PrimaryKey), target.describe())); err != nil {
				return err
			}
			saved, err := withStore(cmd.Context(), target,
				func(store *recordlayer.FDBRecordStore) (*recordlayer.FDBStoredRecord[proto.Message], error) {
					msg, err := parseRecordJSON(store.GetRecordMetaData(), recordType, args[0])
					if err != nil {
						return nil, err
					}
					return store.SaveRecord(msg)
				})
			if err != nil {
				return err
			}
			return writeRecordAsJSON(cmd.OutOrStdout(), saved)
		},
	}
	addr.register(c, true)
	c.Flags().StringVar(&recordType, "type", "", "record type to parse <json> as (required)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "validate + print the would-be-saved record; write nothing")
	c.Flags().BoolVar(&yes, "yes", false, "skip the interactive confirmation")
	return c
}

func newRecordDeleteCmd() *cobra.Command {
	var (
		addr       storeAddressFlags
		recordType string
		dryRun     bool
		yes        bool
	)
	c := &cobra.Command{
		Use:   "delete <primary-key>",
		Short: "Delete one record by primary key (write)",
		Example: `  frl record delete 42 --dry-run
  frl record delete 42 --yes
  frl record delete 1 --type ITEMS --database /myapp --schema main --yes`,
		Long: "Deletes the record at <primary-key> (comma-separated tuple " +
			"elements, same form record scan prints; --type prepends the " +
			"record-type key for prefix-keyed types). Index entries are " +
			"removed transactionally.\n\n" +
			"--dry-run reports whether the record exists without writing. " +
			"The real delete requires --yes or an interactive confirmation.\n\n" +
			"An already-absent record deletes successfully (exit 0, " +
			"'already absent') — after a maybe-committed retry the first " +
			"attempt may have landed; re-running must not report failure.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := addr.resolve()
			if err != nil {
				return err
			}
			ss, err := target.subspace()
			if err != nil {
				return err
			}
			if err := guardNotCatalog(ss); err != nil {
				return err
			}
			pk := parsePrimaryKey(args[0])
			if dryRun {
				exists, err := withStore(cmd.Context(), target,
					func(store *recordlayer.FDBRecordStore) (bool, error) {
						var err error
						if pk, err = applyTypePrefix(store, recordType, pk); err != nil {
							return false, err
						}
						return store.DryRunDeleteRecord(pk)
					})
				if err != nil {
					return err
				}
				if exists {
					fmt.Fprintf(cmd.OutOrStdout(), "dry run — nothing deleted; record %s exists and would be deleted\n", formatPK(pk))
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "dry run — record %s does not exist\n", formatPK(pk))
				}
				return nil
			}
			if err := confirmWrite(cmd, yes, fmt.Sprintf("delete record %s from %s",
				args[0], target.describe())); err != nil {
				return err
			}
			deleted, err := withStore(cmd.Context(), target,
				func(store *recordlayer.FDBRecordStore) (bool, error) {
					var err error
					if pk, err = applyTypePrefix(store, recordType, pk); err != nil {
						return false, err
					}
					return store.DeleteRecord(pk)
				})
			if err != nil {
				return err
			}
			if deleted {
				fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", formatPK(pk))
			} else {
				// Already gone — success, not failure: after a
				// maybe-committed retry the first attempt may have landed
				// (FDB C++ dev C3).
				fmt.Fprintf(cmd.OutOrStdout(), "record %s already absent\n", formatPK(pk))
			}
			return nil
		},
	}
	addr.register(c, true)
	c.Flags().StringVar(&recordType, "type", "", "record type; prepends its type key for prefix-keyed types")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "report existence; delete nothing")
	c.Flags().BoolVar(&yes, "yes", false, "skip the interactive confirmation")
	return c
}

// applyTypePrefix prepends the record type's key when the type's PK
// carries a record-type prefix — same convenience as `record get`. An
// unknown type is an error, never a silent fall-through: on a
// prefix-keyed store the unprefixed key can address a DIFFERENT record,
// and this feeds `record delete` (codex P1: a --type typo must not
// delete the wrong record).
func applyTypePrefix(store *recordlayer.FDBRecordStore, recordType string, pk tuple.Tuple) (tuple.Tuple, error) {
	if recordType == "" {
		return pk, nil
	}
	rt, err := lookupRecordType(store.GetRecordMetaData(), recordType)
	if err != nil {
		return nil, err
	}
	if rt.PrimaryKeyHasRecordTypePrefix() {
		return append(tuple.Tuple{rt.GetRecordTypeKey()}, pk...), nil
	}
	return pk, nil
}

// parseRecordJSON builds a dynamic message for the named record type and
// unmarshals protojson into it. The descriptor comes from the loaded
// metadata, so field names match the operator's .proto source
// (snake_case) — the same convention frl's own output uses.
func parseRecordJSON(md *recordlayer.RecordMetaData, recordType, raw string) (proto.Message, error) {
	rt, err := lookupRecordType(md, recordType)
	if err != nil {
		return nil, err
	}
	msg := dynamicpb.NewMessage(rt.Descriptor)
	if err := protojson.Unmarshal([]byte(raw), msg); err != nil {
		return nil, fmt.Errorf("parse record JSON as %s: %w", recordType, err)
	}
	return msg, nil
}

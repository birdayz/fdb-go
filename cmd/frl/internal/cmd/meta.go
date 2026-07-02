package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	configv1 "fdb.dev/cmd/frl/gen/frl/config/v1"
	"fdb.dev/cmd/frl/internal/meta"
	"fdb.dev/pkg/recordlayer"
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
		newMetaCatalogCmd(),
		newMetaValidateCmd(),
		newMetaEvolveCheckCmd(),
		newMetaDiffCmd(),
		newMetaApplyCmd(),
	)
	return c
}

// newMetaValidateCmd validates a standalone MetaData .pb file. Run it on
// the artifact your deploy pipeline produced before shipping — if this
// passes, the file parses AND passes all structural invariants the
// record-layer enforces at Build() time. No FDB needed.
func newMetaValidateCmd() *cobra.Command {
	var path, outputFmt string
	c := &cobra.Command{
		Use:   "validate",
		Short: "Validate a standalone MetaData.pb file",
		Example: `  frl meta validate --file ./meta.pb
  # In CI:
  #   frl meta validate --file artifacts/meta.pb || exit 1`,
		Long: "Parses the given .pb file as a RecordMetaDataProto.MetaData " +
			"and runs the same structural invariants the record layer " +
			"enforces at Build() time. No FDB connection needed — intended " +
			"for CI pipelines that build metadata.pb as a deploy artifact.\n\n" +
			"Exit 0 on success; any parse / build error exits non-zero with " +
			"the error on stderr (never `{\"valid\": false}` at exit 0).\n\n" +
			"--output / -o: 'text' (default, single \"ok:\" line) or 'json' " +
			"(`{valid: true, file: <path>}`).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			src := &meta.FileSource{Path: path}
			if _, err := src.Load(cmd.Context()); err != nil {
				return err
			}
			if outputFmt == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(validateResult{Valid: true, File: path})
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "ok: %s parses and validates\n", path)
			return err
		},
	}
	c.Flags().StringVar(&path, "file", "", "path to MetaData.pb (required)")
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	_ = c.MarkFlagRequired("file")
	return c
}

// newMetaEvolveCheckCmd compares two MetaData files and reports whether
// evolving from --old to --new is safe (Java-compatible per
// MetaDataEvolutionValidator). Intended for CI pre-merge checks — catch
// migration bugs before they hit FDBMetaDataStore.saveRecordMetaData().
func newMetaEvolveCheckCmd() *cobra.Command {
	var (
		oldPath, newPath, outputFmt string
		allowNoVersionChange        bool
		allowIndexRebuilds          bool
		allowUnsplitToSplit         bool
	)
	c := &cobra.Command{
		Use:   "evolve-check",
		Short: "Verify an evolution from --old to --new metadata is safe",
		Example: `  frl meta evolve-check --old previous.pb --new current.pb
  # CI gate — allow identical-version re-deploys + index rebuilds:
  #   frl meta evolve-check --allow-no-version-change --allow-index-rebuilds \
  #       --old baseline.pb --new $(build-meta.sh) || exit 1`,
		Long: "Runs MetaDataEvolutionValidator on a pair of MetaData.pb " +
			"files. Default semantics match Java's strict mode: version " +
			"must advance, indexes can't be rebuilt, split-long-records " +
			"can't change. Loosen via the --allow-* flags when the strict " +
			"default rejects a transition you know is safe.\n\n" +
			"--output / -o: 'text' (default) or 'json' " +
			"({old, new, valid}).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			oldMeta, err := (&meta.FileSource{Path: oldPath}).Load(cmd.Context())
			if err != nil {
				return fmt.Errorf("load --old: %w", err)
			}
			newMeta, err := (&meta.FileSource{Path: newPath}).Load(cmd.Context())
			if err != nil {
				return fmt.Errorf("load --new: %w", err)
			}
			validator := recordlayer.NewMetaDataEvolutionValidator().
				SetAllowNoVersionChange(allowNoVersionChange).
				SetAllowIndexRebuilds(allowIndexRebuilds).
				SetAllowUnsplitToSplit(allowUnsplitToSplit).
				Build()
			if err := validator.Validate(oldMeta, newMeta); err != nil {
				return fmt.Errorf("incompatible evolution: %w", err)
			}
			if outputFmt == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(evolveCheckResult{Valid: true, Old: oldPath, New: newPath})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "ok: %s -> %s is a valid evolution\n", oldPath, newPath)
			return err
		},
	}
	c.Flags().StringVar(&oldPath, "old", "", "path to the existing MetaData.pb (required)")
	c.Flags().StringVar(&newPath, "new", "", "path to the proposed MetaData.pb (required)")
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	c.Flags().BoolVar(&allowNoVersionChange, "allow-no-version-change", false,
		"accept evolutions where the metadata version hasn't advanced")
	c.Flags().BoolVar(&allowIndexRebuilds, "allow-index-rebuilds", false,
		"accept changes that trigger a full index rebuild")
	c.Flags().BoolVar(&allowUnsplitToSplit, "allow-unsplit-to-split", false,
		"accept toggling split_long_records from false to true")
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
	var addr storeAddressFlags
	var outputFmt string
	c := &cobra.Command{
		Use:   "ls",
		Short: "List record types with primary-key fields",
		Example: `  frl meta types ls
  frl meta types ls -o json | jq -r '.[].name'`,
		Long: "Lists every record type in the metadata with its primary-key " +
			"fields. All metadata sources work: meta_file, " +
			"meta_store_keyspace (Path B), --meta-file, --database/--schema.\n\n" +
			"--output / -o: 'text' (default, tabwriter) or 'json' (array of " +
			"{name, primary_key, since_version}).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			target, err := addr.resolve()
			if err != nil {
				return err
			}
			md, err := loadTargetMetadata(cmd.Context(), target)
			if err != nil {
				return err
			}
			if outputFmt == "json" {
				return writeTypesListJSON(cmd.OutOrStdout(), md)
			}
			return writeTypesList(cmd.OutOrStdout(), md)
		},
	}
	addr.register(c, true)
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
	var addr storeAddressFlags
	var outputFmt string
	c := &cobra.Command{
		Use:   "get",
		Short: "Dump the loaded RecordMetaData",
		Annotations: map[string]string{
			// Tells registerFormatCompletion to suggest json/yaml (this
			// command has no text form — protojson is the "default").
			AnnotationOutputYAML: "true",
		},
		Long: "Loads the current context's MetaData and prints it. All " +
			"metadata sources work: `meta_file` (offline), " +
			"`meta_store_keyspace` (FDBMetaDataStore — Path B), --meta-file, " +
			"and --database/--schema (the catalog's schema-pinned template).\n\n" +
			"--output / -o: 'json' (default — protojson, multiline) or " +
			"'yaml' (protoyaml, more compact for large schemas).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "json", "yaml"); err != nil {
				return err
			}
			target, err := addr.resolve()
			if err != nil {
				return err
			}
			md, err := loadTargetMetadata(cmd.Context(), target)
			if err != nil {
				return err
			}
			return writeMetaDataRendered(cmd.OutOrStdout(), md, outputFmt)
		},
	}
	addr.register(c, true)
	c.Flags().StringVarP(&outputFmt, "output", "o", "json",
		"output format: json or yaml")
	return c
}

// writeTypesList renders one row per record type: name, primary-key
// fields (comma-joined), "since" version (when the type entered the
// metadata). Since-version 0 is treated as "never upgraded" — suppress it.
func writeTypesList(out io.Writer, md *recordlayer.RecordMetaData) error {
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
		pk := pkFieldsOrUnset(rt.PrimaryKey)
		since := ""
		if rt.SinceVersion > 0 {
			since = fmt.Sprintf("%d", rt.SinceVersion)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, pk, since)
	}
	return tw.Flush()
}

// runMetaGet loads and renders metadata for a context — retained for
// tests that drive the render path with a synthetic context.
func runMetaGet(cmd *cobra.Command, cfgCtx *configv1.Context, outputFmt string) error {
	md, err := loadTargetMetadata(cmd.Context(), &storeTarget{cfgCtx: cfgCtx})
	if err != nil {
		return err
	}
	return writeMetaDataRendered(cmd.OutOrStdout(), md, outputFmt)
}

// validateResult / evolveCheckResult are the typed JSON shapes of
// `meta validate` / `meta evolve-check` — consistent with the rest of
// frl's structured output (no ad-hoc maps).
type validateResult struct {
	Valid bool   `json:"valid"`
	File  string `json:"file"`
}

type evolveCheckResult struct {
	Valid bool   `json:"valid"`
	Old   string `json:"old"`
	New   string `json:"new"`
}

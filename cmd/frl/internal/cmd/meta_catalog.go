package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"buf.build/go/protoyaml"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	configv1 "fdb.dev/cmd/frl/gen/frl/config/v1"
	"fdb.dev/pkg/recordlayer"
	relapi "fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
)

// newMetaCatalogCmd is the `meta catalog` noun — a read-only view of
// the relational-layer catalog at tuple(nil, nil, 0) (`__SYS/__SYS/CATALOG`).
// Commands under this node discover schemas/databases/templates without
// the operator having to configure `meta_file` or `meta_store_keyspace`.
//
// Plain-core clusters (no relational layer deployed) have an empty
// `__SYS/CATALOG` subspace; the subcommands surface the "no catalog
// on this cluster" error cleanly and suggest the non-catalog flows.
func newMetaCatalogCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "catalog",
		Short: "Inspect the relational-layer catalog at tuple(nil,nil,0) (__SYS/CATALOG)",
		Long: "Reads the relational catalog, a fixed subspace where " +
			"fdb-relational stores every database / schema / template on " +
			"the cluster. Operators on a relational cluster get schema " +
			"auto-discovery without wiring `meta_file` or " +
			"`meta_store_keyspace`; plain-core clusters (no relational " +
			"layer) return an empty-catalog error so you know to fall " +
			"back to --meta-file.\n\n" +
			"Never writes to `__SYS/CATALOG` — that namespace is owned " +
			"by the relational layer and mutating it from `frl` would " +
			"corrupt the cluster for real relational clients.",
	}
	c.AddCommand(
		newMetaCatalogDatabasesCmd(),
		newMetaCatalogSchemasCmd(),
		newMetaCatalogTemplatesCmd(),
		newMetaCatalogGetCmd(),
	)
	return c
}

// newMetaCatalogDatabasesCmd lists every database URI in the catalog.
// Schemas and templates are grouped under databases in the Java
// relational model; this command is the "what namespaces exist?"
// entry point.
func newMetaCatalogDatabasesCmd() *cobra.Command {
	var contextName, outputFmt string
	c := &cobra.Command{
		Use:   "databases",
		Short: "List databases in the relational catalog",
		Example: `  frl meta catalog databases
  frl meta catalog databases -o json | jq -r '.[].id'`,
		Long: "Scans the DATABASES table in `__SYS/CATALOG` and prints one " +
			"row per database URI. Read-only; issues one snapshot range " +
			"scan against FDB.\n\n" +
			"--output / -o: 'text' (default, bare URIs, one per line) or " +
			"'json' (array of `{id}` objects).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			cfgCtx, err := resolveCatalogContext(contextName)
			if err != nil {
				return err
			}
			ids, err := runCatalogQuery(cmd.Context(), cfgCtx,
				func(ctx context.Context, cat *catalog.RecordLayerStoreCatalog, txn relapi.Transaction) ([]string, error) {
					return collectStrings(cat.ListDatabases(txn, nil))
				})
			if err != nil {
				return err
			}
			sort.Strings(ids)
			return writeCatalogDatabases(cmd.OutOrStdout(), ids, outputFmt)
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}

// newMetaCatalogSchemasCmd lists schemas. --database narrows to one
// database URI; the default enumerates schemas across every database.
// Each schema is (database_id, schema_name, template_name, template_version).
func newMetaCatalogSchemasCmd() *cobra.Command {
	var contextName, outputFmt, databaseID string
	c := &cobra.Command{
		Use:   "schemas",
		Short: "List schemas in the relational catalog",
		Example: `  frl meta catalog schemas
  frl meta catalog schemas --database /myapp
  frl meta catalog schemas -o json | jq '.[] | select(.template == "orders_v2")'`,
		Long: "Scans the SCHEMAS table in `__SYS/CATALOG`. Each row carries " +
			"the owning database, schema name, and the template (name + " +
			"version) the schema points at — which is what `meta catalog " +
			"get <template>` reads to render the on-disk MetaData.\n\n" +
			"--database narrows to a single database URI; omit to see " +
			"every schema on the cluster.\n\n" +
			"--output / -o: 'text' (default, tabwriter — DATABASE / SCHEMA " +
			"/ TEMPLATE / VERSION columns) or 'json' (array of " +
			"`{database, name, template, template_version}` objects).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			cfgCtx, err := resolveCatalogContext(contextName)
			if err != nil {
				return err
			}
			rows, err := runCatalogQuery(cmd.Context(), cfgCtx,
				func(ctx context.Context, cat *catalog.RecordLayerStoreCatalog, txn relapi.Transaction) ([]schemaRow, error) {
					var (
						rs      relapi.ResultSet
						listErr error
					)
					if databaseID != "" {
						rs, listErr = cat.ListSchemasInDatabase(txn, databaseID, nil)
					} else {
						rs, listErr = cat.ListSchemas(txn, nil)
					}
					if listErr != nil {
						return nil, listErr
					}
					return collectSchemaRows(rs)
				})
			if err != nil {
				return err
			}
			sort.Slice(rows, func(i, j int) bool {
				if rows[i].Database != rows[j].Database {
					return rows[i].Database < rows[j].Database
				}
				return rows[i].Name < rows[j].Name
			})
			return writeCatalogSchemas(cmd.OutOrStdout(), rows, outputFmt)
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&databaseID, "database", "", "narrow to schemas in this database URI")
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}

// newMetaCatalogTemplatesCmd lists every (template_name, version) tuple
// in the catalog. A single template is reused by multiple schemas —
// this is the "what schemas have been uploaded?" entry point, while
// `schemas` is "what schemas are currently bound?".
func newMetaCatalogTemplatesCmd() *cobra.Command {
	var contextName, outputFmt string
	c := &cobra.Command{
		Use:   "templates",
		Short: "List schema templates in the relational catalog",
		Example: `  frl meta catalog templates
  frl meta catalog templates -o json | jq 'group_by(.name) | map({name: .[0].name, versions: [.[].version]})'`,
		Long: "Scans the TEMPLATES table in `__SYS/CATALOG`. Each row is " +
			"one (name, version) tuple — templates can have multiple " +
			"versions simultaneously; the META_DATA blob lives on the " +
			"row and is what `meta catalog get` extracts.\n\n" +
			"--output / -o: 'text' (default, tabwriter — NAME / VERSION " +
			"columns) or 'json' (array of `{name, version}` objects).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			cfgCtx, err := resolveCatalogContext(contextName)
			if err != nil {
				return err
			}
			rows, err := runCatalogQuery(cmd.Context(), cfgCtx,
				func(ctx context.Context, cat *catalog.RecordLayerStoreCatalog, txn relapi.Transaction) ([]templateRow, error) {
					rs, err := cat.SchemaTemplateCatalog().ListTemplates(txn)
					if err != nil {
						return nil, err
					}
					return collectTemplateRows(rs)
				})
			if err != nil {
				return err
			}
			sort.Slice(rows, func(i, j int) bool {
				if rows[i].Name != rows[j].Name {
					return rows[i].Name < rows[j].Name
				}
				return rows[i].Version < rows[j].Version
			})
			return writeCatalogTemplates(cmd.OutOrStdout(), rows, outputFmt)
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}

// newMetaCatalogGetCmd loads a template's MetaData blob and renders it
// the same way `meta get` does — protojson by default, protoyaml on
// --output yaml. Complements `meta get --meta-file` / with-context by
// pulling the proto straight out of the relational catalog.
func newMetaCatalogGetCmd() *cobra.Command {
	var contextName, outputFmt string
	var version int
	c := &cobra.Command{
		Use:   "get <template-name>",
		Short: "Render a template's RecordMetaData proto from the catalog",
		Annotations: map[string]string{
			// Same yaml|json set as `meta get` — both render a proto.
			AnnotationOutputYAML: "true",
		},
		Example: `  frl meta catalog get orders_v2
  frl meta catalog get orders_v2 --version 1
  frl meta catalog get orders_v2 -o yaml | yq '.records'`,
		Long: "Loads a template's MetaData proto from `__SYS/CATALOG.TEMPLATES` " +
			"and renders it. Default output is the latest version; pass " +
			"--version to pin a specific version.\n\n" +
			"--output / -o: 'json' (default — protojson, multiline) or " +
			"'yaml' (protoyaml). Same format options as `meta get`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateOutputFormat(outputFmt, "json", "yaml"); err != nil {
				return err
			}
			cfgCtx, err := resolveCatalogContext(contextName)
			if err != nil {
				return err
			}
			md, err := runCatalogQuery(cmd.Context(), cfgCtx,
				func(ctx context.Context, cat *catalog.RecordLayerStoreCatalog, txn relapi.Transaction) (*recordlayer.RecordMetaData, error) {
					tc := cat.SchemaTemplateCatalog()
					var (
						tpl     relapi.SchemaTemplate
						loadErr error
					)
					if version > 0 {
						tpl, loadErr = tc.LoadSchemaTemplateAtVersion(txn, args[0], version)
					} else {
						tpl, loadErr = tc.LoadSchemaTemplate(txn, args[0])
					}
					if loadErr != nil {
						return nil, loadErr
					}
					// Every template impl carries a *recordlayer.RecordMetaData
					// underneath — that's the thing protojson understands.
					type underlyingProvider interface {
						Underlying() *recordlayer.RecordMetaData
					}
					up, ok := tpl.(underlyingProvider)
					if !ok {
						return nil, fmt.Errorf("template %q is not backed by a record-layer MetaData — catalog entry type %T", args[0], tpl)
					}
					return up.Underlying(), nil
				})
			if err != nil {
				return err
			}
			return writeMetaDataRendered(cmd.OutOrStdout(), md, outputFmt)
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().IntVar(&version, "version", 0, "template version (0 = latest)")
	c.Flags().StringVarP(&outputFmt, "output", "o", "json", "output format: json or yaml")
	return c
}

// --- shared helpers --------------------------------------------------------

// resolveCatalogContext is a thin wrapper over resolveContextAndOverride
// that requires a context (catalog queries touch FDB) and discards the
// meta-source override — the catalog is its own metadata source.
func resolveCatalogContext(contextName string) (*configv1.Context, error) {
	cfgCtx, _, err := resolveContextAndOverride(contextName, "")
	if err != nil {
		return nil, err
	}
	return cfgCtx, nil
}

// runCatalogQuery encapsulates the "open FDB → open the catalog →
// wrap in a relational transaction → run the closure" dance every
// catalog command runs. Parameterised on the return type so each
// subcommand can return its own concrete row struct instead of
// funnelling through `any`.
//
// The catalog itself is at the fixed DefaultCatalogSubspace() —
// tuple(nil, nil, 0). No keyspace_path from the context is needed,
// but we still dial FDB via the context's cluster_file.
func runCatalogQuery[T any](
	ctx context.Context,
	cfgCtx *configv1.Context,
	fn func(ctx context.Context, cat *catalog.RecordLayerStoreCatalog, txn relapi.Transaction) (T, error),
) (T, error) {
	var zero T

	db, err := openDatabase(cfgCtx.GetClusterFile())
	if err != nil {
		return zero, err
	}
	rec := recordlayer.NewFDBDatabase(db)

	// The sqldriver writes its catalog at
	// keyspace.New(subspace.Sub()).CatalogSubspace() =
	// ("__SYS", "__SYS", "CATALOG") — three strings, not the two-string
	// DefaultCatalogSubspace() catalog.OpenRecordLayerStoreCatalog uses.
	// Read from the same place the driver writes so `meta catalog` sees
	// what `frl sql` just created.
	cat, err := catalog.NewRecordLayerStoreCatalog(relationalKeyspace().CatalogSubspace())
	if err != nil {
		return zero, fmt.Errorf("open relational catalog: %w", err)
	}

	result, err := rec.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		txn := catalog.NewFDBTransaction(rctx)
		return fn(ctx, cat, txn)
	})
	if err != nil {
		return zero, wrapMissingCatalogErr(err)
	}
	v, _ := result.(T)
	return v, nil
}

// wrapMissingCatalogErr turns "no store header at keyspace …" into an
// operator-friendly "catalog not found on this cluster" — the
// plain-core branch. Shared by every catalog reader (`meta catalog`,
// catalogSource for --database/--schema addressing) so the message
// can't drift.
func wrapMissingCatalogErr(err error) error {
	if strings.Contains(err.Error(), "store does not exist") ||
		strings.Contains(err.Error(), "no store header") {
		return fmt.Errorf("no relational catalog on this cluster — `__SYS/CATALOG` is empty; this is plain-core, not fdb-relational. Use `frl meta get --meta-file <path>` instead")
	}
	return err
}

// --- rendering -------------------------------------------------------------

type schemaRow struct {
	Database        string `json:"database"`
	Name            string `json:"name"`
	Template        string `json:"template"`
	TemplateVersion int    `json:"template_version"`
}

type templateRow struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

func writeCatalogDatabases(out io.Writer, ids []string, outputFmt string) error {
	if outputFmt == "json" {
		rows := make([]map[string]string, len(ids))
		for i, id := range ids {
			rows[i] = map[string]string{"id": id}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(ids) == 0 {
		_, err := fmt.Fprintln(out, "(no databases in catalog)")
		return err
	}
	for _, id := range ids {
		if _, err := fmt.Fprintln(out, id); err != nil {
			return err
		}
	}
	return nil
}

func writeCatalogSchemas(out io.Writer, rows []schemaRow, outputFmt string) error {
	if outputFmt == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if rows == nil {
			rows = []schemaRow{}
		}
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(out, "(no schemas in catalog)")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DATABASE\tSCHEMA\tTEMPLATE\tVERSION")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", r.Database, r.Name, r.Template, r.TemplateVersion)
	}
	return tw.Flush()
}

func writeCatalogTemplates(out io.Writer, rows []templateRow, outputFmt string) error {
	if outputFmt == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if rows == nil {
			rows = []templateRow{}
		}
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(out, "(no templates in catalog)")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVERSION")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%d\n", r.Name, r.Version)
	}
	return tw.Flush()
}

// writeMetaDataRendered is shared with `meta get` — both commands load
// a *recordlayer.RecordMetaData from different sources and need to
// render it the same way.
func writeMetaDataRendered(out io.Writer, md *recordlayer.RecordMetaData, outputFmt string) error {
	mdProto, err := md.ToProto()
	if err != nil {
		return fmt.Errorf("render metadata: %w", err)
	}
	var bytes []byte
	switch outputFmt {
	case "yaml":
		bytes, err = protoyaml.MarshalOptions{Indent: 2}.Marshal(mdProto)
	default:
		bytes, err = protojson.MarshalOptions{
			Multiline: true,
			Indent:    "  ",
		}.Marshal(mdProto)
	}
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	_, err = fmt.Fprintln(out, strings.TrimRight(string(bytes), "\n"))
	return err
}

// --- ResultSet scrapers ----------------------------------------------------

// collectStrings iterates a ResultSet whose first column is a string and
// returns all values. Used for `databases` (ListDatabases returns a
// single-column result set of URIs).
func collectStrings(rs relapi.ResultSet, firstErr error) ([]string, error) {
	if firstErr != nil {
		return nil, firstErr
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		v, err := rs.String(1)
		if err != nil {
			return nil, fmt.Errorf("read column 1: %w", err)
		}
		if !rs.WasNull() {
			out = append(out, v)
		}
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// collectSchemaRows parses ListSchemas[InDatabase] results. The Java
// schema catalog exposes at least (database_id, schema_name,
// template_name, template_version) — we read by column name so we don't
// break on column-order changes.
func collectSchemaRows(rs relapi.ResultSet) ([]schemaRow, error) {
	defer rs.Close()
	md := rs.MetaData()
	idx := make(map[string]int, md.ColumnCount())
	for i := 1; i <= md.ColumnCount(); i++ {
		name, err := md.ColumnName(i)
		if err != nil {
			return nil, fmt.Errorf("column %d name: %w", i, err)
		}
		idx[strings.ToUpper(name)] = i
	}
	var out []schemaRow
	for rs.Next() {
		var r schemaRow
		if c, ok := idx["DATABASE_ID"]; ok {
			r.Database, _ = rs.String(c)
		}
		if c, ok := idx["SCHEMA_NAME"]; ok {
			r.Name, _ = rs.String(c)
		}
		if c, ok := idx["TEMPLATE_NAME"]; ok {
			r.Template, _ = rs.String(c)
		}
		if c, ok := idx["TEMPLATE_VERSION"]; ok {
			v, _ := rs.Long(c)
			r.TemplateVersion = int(v)
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// collectTemplateRows parses ListTemplates results by column name —
// same rationale as collectSchemaRows (don't break on column-order
// reshuffles).
func collectTemplateRows(rs relapi.ResultSet) ([]templateRow, error) {
	defer rs.Close()
	md := rs.MetaData()
	idx := make(map[string]int, md.ColumnCount())
	for i := 1; i <= md.ColumnCount(); i++ {
		name, err := md.ColumnName(i)
		if err != nil {
			return nil, fmt.Errorf("column %d name: %w", i, err)
		}
		idx[strings.ToUpper(name)] = i
	}
	var out []templateRow
	for rs.Next() {
		var r templateRow
		if c, ok := idx["TEMPLATE_NAME"]; ok {
			r.Name, _ = rs.String(c)
		}
		if c, ok := idx["TEMPLATE_VERSION"]; ok {
			v, _ := rs.Long(c)
			r.Version = int(v)
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

package embedded

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	apiddl "fdb.dev/pkg/relational/api/ddl"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/functions"
	"fdb.dev/pkg/relational/core/metadata"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	queryddl "fdb.dev/pkg/relational/core/query/ddl"
	"github.com/antlr4-go/antlr/v4"
)

// DDL execution: CREATE / DROP DATABASE / SCHEMA / SCHEMA TEMPLATE +
// parseTableDefinition / parseIndexDefinition / parseColumnType.
//
// Every DDL statement resolves to an apiddl.ConstantAction obtained
// from c.sess.Factory and executed in its own auto-commit
// transaction via runDDL, which also gates on ensureCatalogInit to
// make sure the root catalog state is bootstrapped before the
// first DDL on a fresh cluster.

func (c *EmbeddedConnection) execCreate(ctx context.Context, cs antlrgen.ICreateStatementContext) (int64, error) {
	switch t := cs.(type) {
	case *antlrgen.CreateDatabaseStatementContext:
		return c.execCreateDatabase(ctx, t)
	case *antlrgen.CreateSchemaStatementContext:
		return c.execCreateSchema(ctx, t)
	case *antlrgen.CreateSchemaTemplateStatementContext:
		return c.execCreateSchemaTemplate(ctx, t)
	default:
		return 0, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported CREATE statement: %T", cs)
	}
}

func (c *EmbeddedConnection) execDrop(ctx context.Context, ds antlrgen.IDropStatementContext) (int64, error) {
	switch t := ds.(type) {
	case *antlrgen.DropDatabaseStatementContext:
		return c.execDropDatabase(ctx, t)
	case *antlrgen.DropSchemaStatementContext:
		return c.execDropSchema(ctx, t)
	case *antlrgen.DropSchemaTemplateStatementContext:
		return c.execDropSchemaTemplate(ctx, t)
	default:
		return 0, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported DROP statement: %T", ds)
	}
}

func (c *EmbeddedConnection) execCreateDatabase(ctx context.Context, s *antlrgen.CreateDatabaseStatementContext) (int64, error) {
	dbPath := s.Path().GetText()
	if err := validateDatabasePath(dbPath); err != nil {
		return 0, err
	}
	action := c.sess.Factory.CreateDatabase(dbPath, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execDropDatabase(ctx context.Context, s *antlrgen.DropDatabaseStatementContext) (int64, error) {
	dbPath := s.Path().GetText()
	if err := validateDatabasePath(dbPath); err != nil {
		return 0, err
	}
	throwIfNotExist := s.IfExists() == nil
	action := c.sess.Factory.DropDatabase(dbPath, throwIfNotExist, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execCreateSchema(ctx context.Context, s *antlrgen.CreateSchemaStatementContext) (int64, error) {
	schemaText := s.SchemaId().GetText()
	dbPath, schemaName, err := parseSchemaIdentifier(schemaText, c.sess.DBPath)
	if err != nil {
		return 0, err
	}
	// The SCHEMA segment is an SQL identifier: unquoted names normalize to
	// upper case (Java's visitUid normalization) — `create schema /db/test`
	// creates TEST, which is how a `schema=TEST` connection then finds it.
	// The database PATH is not an identifier and stays verbatim.
	schemaName = functions.StripIdentifierQuotes(schemaName)
	templateID := s.SchemaTemplateId().GetText()
	action := c.sess.Factory.CreateSchema(dbPath, schemaName, templateID, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execDropSchema(ctx context.Context, s *antlrgen.DropSchemaStatementContext) (int64, error) {
	// DROP SCHEMA deliberately does NOT honor IF EXISTS — this matches Java exactly.
	// Java's DdlVisitor.visitDropSchemaStatement (DdlVisitor.java:472) never reads
	// ctx.ifExists(): it builds getDropSchemaConstantAction(db, schema, Options.NONE),
	// so `DROP SCHEMA IF EXISTS <nonexistent>` errors (schema does not exist) just like
	// the bare form. Only DROP DATABASE (visitDropDatabaseStatement:466) and DROP SCHEMA
	// TEMPLATE (visitDropSchemaTemplateStatement:483) thread throwIfDoesNotExist from
	// ifExists(); DROP SCHEMA does not. Do NOT "fix" this to honor IF EXISTS — that would
	// DIVERGE from Java. Pinned by drop_schema_ifexists_conformance_probe_test.go.
	schemaText := s.Uid().GetText()
	dbPath, schemaName, err := parseSchemaIdentifier(schemaText, c.sess.DBPath)
	if err != nil {
		return 0, err
	}
	// Same identifier normalization as execCreateSchema: DROP SCHEMA
	// /db/test drops TEST.
	schemaName = functions.StripIdentifierQuotes(schemaName)
	if dbPath == "" {
		return 0, api.NewErrorf(api.ErrCodeUnknownDatabase,
			"invalid database identifier in %q", schemaText)
	}
	action := c.sess.Factory.DropSchema(dbPath, schemaName, *api.NoOptions())
	if err := c.runDDL(ctx, action); err != nil {
		return 0, err
	}
	c.invalidateSchemaCache(dbPath, schemaName)
	return 0, nil
}

func (c *EmbeddedConnection) execDropSchemaTemplate(ctx context.Context, s *antlrgen.DropSchemaTemplateStatementContext) (int64, error) {
	templateID := s.Uid().GetText()
	throwIfNotExist := s.IfExists() == nil
	action := c.sess.Factory.DropSchemaTemplate(templateID, throwIfNotExist, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execCreateSchemaTemplate(ctx context.Context, s *antlrgen.CreateSchemaTemplateStatementContext) (int64, error) {
	templateID := trimIdentifierQuotes(s.SchemaTemplateId().GetText())
	b := metadata.NewSchemaTemplateBuilder().SetName(templateID)

	// WITH OPTIONS(...) — ENABLE_LONG_ROWS / INTERMINGLE_TABLES / STORE_ROW_VERSIONS.
	// Mirrors Java's DdlVisitor.visitCreateSchemaTemplateStatement: applied before
	// the table/index passes below, since intermingleTbls changes how AddTable's
	// primary keys are compiled at Build() time (buildPrimaryKeyExpression prepends
	// RecordTypeKey() unless intermingled).
	if oc := s.OptionsClause(); oc != nil {
		for _, opt := range oc.AllOption() {
			switch {
			case opt.ENABLE_LONG_ROWS() != nil:
				b.SetEnableLongRows(opt.BooleanLiteral().TRUE() != nil)
			case opt.INTERMINGLE_TABLES() != nil:
				b.SetIntermingleTables(opt.BooleanLiteral().TRUE() != nil)
			case opt.STORE_ROW_VERSIONS() != nil:
				b.SetStoreRowVersions(opt.BooleanLiteral().TRUE() != nil)
			default:
				// Unreachable through the grammar (option's three alternatives are
				// exhaustive) — defensive default matching Java's
				// Assert.failUnchecked(ErrorCode.SYNTAX_ERROR, ...).
				return 0, api.NewErrorf(api.ErrCodeSyntaxError,
					"unknown option in schema template creation: %s", opt.GetText())
			}
		}
	}

	if err := rejectUnsupportedTemplateClauses(s.AllTemplateClause()); err != nil {
		return 0, err
	}

	if err := registerStructDefinitions(s.AllTemplateClause(), b); err != nil {
		return 0, err
	}

	// First pass: register tables (indexes reference them by name).
	for _, clause := range s.AllTemplateClause() {
		td := clause.TableDefinition()
		if td == nil {
			continue
		}
		tableName := functions.StripIdentifierQuotes(td.Uid().GetText())
		cols, pkCols, err := parseTableDefinition(td, b)
		if err != nil {
			// Propagate a specific *api.Error (e.g. 42701 duplicate column, 42703 PK over an
			// unknown column) as its OWN SQLSTATE instead of masking it under 42F59
			// (ErrCodeInvalidSchemaTemplate) — 42F59 means "invalid schema template", the
			// wrong code for a duplicate column. Java's DdlVisitor does not wrap in-template
			// errors either; ExceptionUtil maps each exception to its specific ErrorCode. A
			// non-structured parse error still wraps (it carries no SQLSTATE to surface).
			var apiErr *api.Error
			if errors.As(err, &apiErr) {
				return 0, err
			}
			return 0, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"table %q: %v", tableName, err)
		}
		b.AddTablePrimaryKeyPaths(tableName, cols, pkCols)
	}

	// Second pass: register indexes.
	for _, clause := range s.AllTemplateClause() {
		idxDef := clause.IndexDefinition()
		if idxDef == nil {
			continue
		}
		if err := parseIndexDefinition(idxDef, b); err != nil {
			// Propagate a specific *api.Error (e.g. 0A000 for an unsupported INCLUDE /
			// covering index) as its OWN SQLSTATE instead of masking it under 42F59. Java
			// does not wrap in-template index errors either. A non-structured error wraps.
			var apiErr *api.Error
			if errors.As(err, &apiErr) {
				return 0, err
			}
			return 0, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate, "index: %v", err)
		}
	}

	tmpl, err := b.Build()
	if err != nil {
		return 0, err
	}
	action := c.sess.Factory.SaveSchemaTemplate(tmpl, *api.NoOptions())
	if err := c.runDDL(ctx, action); err != nil {
		return 0, err
	}
	// Template change may affect any schema using it — flush the whole cache.
	c.sess.ResetSchemaCache()
	return 0, nil
}

// trimIdentifierQuotes removes surrounding double/back quotes VERBATIM,
// without the case fold StripIdentifierQuotes applies to unquoted names.
// Template names historically keep their raw unquoted spelling (a template
// created as `create schema template foo` is stored "foo"); a QUOTED name
// must not keep its quote characters — they would leak into the persisted
// descriptor's FILE name, which is wire.
func trimIdentifierQuotes(s string) string {
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '`' && s[len(s)-1] == '`')) {
		return s[1 : len(s)-1]
	}
	return s
}

// registerStructDefinitions is the struct pass: CREATE TYPE AS STRUCT
// registers an auxiliary type (Java's DdlVisitor.visitStructDefinition
// builds a table-without-primary-key through the SAME column parser and
// keeps only its StructType, registered via addAuxiliaryType). A struct
// field may reference a type declared LATER — the unresolved placeholder is
// fixed up at Build() by the ported resolveTypes pass. Struct clauses run
// before the table pass so AddAuxiliaryType's collision check sees other
// structs; table-vs-type collisions are caught regardless of order
// (verifyNameIsNotUsed scans both sides). Shared by the production DDL
// executor and BuildSchemaTemplateFromDDL — one pipeline.
func registerStructDefinitions(clauses []antlrgen.ITemplateClauseContext, b *metadata.Builder) error {
	for _, clause := range clauses {
		sd := clause.StructDefinition()
		if sd == nil {
			continue
		}
		structName := functions.StripIdentifierQuotes(sd.Uid().GetText())
		cols, err := parseColumnDefinitions(sd.AllColumnDefinition(), b)
		if err != nil {
			var apiErr *api.Error
			if errors.As(err, &apiErr) {
				return err
			}
			return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"struct type %q: %v", structName, err)
		}
		fields := make([]api.StructField, len(cols))
		for i, c := range cols {
			fields[i] = api.NewStructField(c.Name(), c.DataType(), i)
		}
		b.AddAuxiliaryType(api.NewStructType(structName, fields, true))
	}
	return nil
}

// rejectUnsupportedTemplateClauses fails closed on schema-template clause
// kinds the builder below does not read. Silently skipping one builds a
// DIFFERENT template than the DDL declared — a view or SQL function would
// simply vanish, and every later reference to it surfaces as a misleading
// "table does not exist" — the accept-and-drop failure mode this file bans.
// Java supports all of these (DdlVisitor visitEnumDefinition /
// visitSqlInvokedFunction / visitViewDefinition), so each rejection is a
// named parity gap, not a divergence: SQL functions are RFC-201 Phase 4.
// Struct definitions are handled by the struct pass above (RFC-204).
func rejectUnsupportedTemplateClauses(clauses []antlrgen.ITemplateClauseContext) error {
	for _, clause := range clauses {
		switch {
		case clause.EnumDefinition() != nil:
			return api.NewError(api.ErrCodeUnsupportedOperation,
				"enum types (CREATE TYPE AS ENUM) are not yet supported in a schema template")
		case clause.SqlInvokedFunction() != nil:
			return api.NewError(api.ErrCodeUnsupportedOperation,
				"SQL functions (CREATE FUNCTION) are not yet supported in a schema template")
		case clause.ViewDefinition() != nil:
			return api.NewError(api.ErrCodeUnsupportedOperation,
				"views (CREATE VIEW) are not yet supported in a schema template")
		}
	}
	return nil
}

// parseIndexDefinition handles a single CREATE INDEX clause within a schema template.
func parseIndexDefinition(idxDef antlrgen.IIndexDefinitionContext, b *metadata.Builder) error {
	switch def := idxDef.(type) {
	case *antlrgen.IndexOnSourceDefinitionContext:
		return parseOnSourceIndexDefinition(def, b)
	case *antlrgen.IndexAsSelectDefinitionContext:
		return parseAsSelectIndexDefinition(def, b)
	case *antlrgen.VectorIndexDefinitionContext:
		return parseVectorIndexDefinition(def, b)
	default:
		return api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported index definition type %T", idxDef)
	}
}

// rejectIndexOrderClause fails closed on a per-column ASC/DESC/NULLS clause in
// a VECTOR index column list (indexed column and PARTITION BY alike).
//
// The ordinary ON-source path honours the clause through the generator front
// end (index_onsource.go — Java wraps an ordered column in an
// OrderFunctionKeyExpression, and dropping the clause would be a WIRE
// divergence: a plain ascending field index where Java writes the
// order-inverted encoding). The vector path keeps its own construction
// (RFC-202 §10) and reads only columnName, so an order clause there would
// still be silently dropped; Java's vector path parses it through the same
// IndexedColumn.parseColSpec and honours it. Fail closed until the vector
// path routes through the generator too — explicit ASC / ASC NULLS FIRST are
// wire-identical to no clause in Java and are knowingly swept into the
// rejection, since narrowing the guard would duplicate Java's
// default-resolution logic inside throwaway code.
func rejectIndexOrderClause(specs []antlrgen.IIndexColumnSpecContext, kind, indexName string) error {
	for _, spec := range specs {
		sc, ok := spec.(*antlrgen.IndexColumnSpecContext)
		if !ok || sc.OrderClause() == nil {
			continue
		}
		return api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"%s %q: per-column ordering (ASC/DESC/NULLS) on column %q is not yet supported",
			kind, indexName, functions.StripIdentifierQuotes(sc.GetColumnName().GetText()))
	}
	return nil
}

// parseVectorIndexDefinition handles
// CREATE VECTOR INDEX name USING HNSW ON table(vectorCol) PARTITION BY (cols) OPTIONS(...).
// Mirrors Java's DdlVisitor.visitVectorIndexDefinition: exactly one indexed
// (vector) column, the PARTITION BY columns form the HNSW partition prefix,
// INCLUDE is unsupported, and the dimension count is derived from the
// indexed column's VECTOR type (in metadata.Builder.AddVectorIndex).
func parseVectorIndexDefinition(def *antlrgen.VectorIndexDefinitionContext, b *metadata.Builder) error {
	indexName := functions.StripIdentifierQuotes(def.GetIndexName().GetText())
	// Match the sibling IndexOnSourceDefinition path, which registers and
	// looks up the table by the raw (unnormalized) source text.
	tableName := functions.StripIdentifierQuotes(def.GetSource().GetText())

	if def.IncludeClause() != nil {
		return api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"vector index %q: INCLUDE clause is not supported", indexName)
	}

	// Exactly one indexed (vector) column.
	var vecCols []string
	if cl := def.IndexColumnList(); cl != nil {
		if err := rejectIndexOrderClause(cl.AllIndexColumnSpec(), "vector index", indexName); err != nil {
			return err
		}
		for _, spec := range cl.AllIndexColumnSpec() {
			vecCols = append(vecCols, functions.StripIdentifierQuotes(spec.GetColumnName().GetText()))
		}
	}
	if len(vecCols) != 1 {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"vector index %q: exactly one indexed column is supported, found %d",
			indexName, len(vecCols))
	}

	// PARTITION BY prefix columns (optional).
	var partitionCols []string
	if pc := def.IndexPartitionClause(); pc != nil {
		if err := rejectIndexOrderClause(pc.AllIndexColumnSpec(), "vector index PARTITION BY", indexName); err != nil {
			return err
		}
		for _, spec := range pc.AllIndexColumnSpec() {
			partitionCols = append(partitionCols, functions.StripIdentifierQuotes(spec.GetColumnName().GetText()))
		}
	}

	method := "HNSW"
	if def.GetMethod() != nil {
		method = strings.ToUpper(def.GetMethod().GetText())
	}
	options, err := parseVectorIndexOptions(def.VectorIndexOptions(), indexName, method)
	if err != nil {
		return err
	}

	b.AddVectorIndexUsing(method, tableName, indexName, vecCols[0], partitionCols, options)
	return nil
}

// parseVectorIndexOptions parses the OPTIONS(...) clause of a vector index
// into recordlayer HNSW option keys. Mirrors Java's
// DdlVisitor.parseVectorOptions (CONNECTIVITY→HNSW_M, METRIC→enum name, ...).
func parseVectorIndexOptions(ctx antlrgen.IVectorIndexOptionsContext, indexName, method string) (map[string]string, error) {
	opts := map[string]string{}
	if ctx == nil {
		return opts, nil
	}
	octx, ok := ctx.(*antlrgen.VectorIndexOptionsContext)
	if !ok {
		return opts, nil
	}
	for _, o := range octx.AllVectorIndexOption() {
		oc, ok := o.(*antlrgen.VectorIndexOptionContext)
		if !ok {
			continue
		}
		switch {
		case oc.EF_CONSTRUCTION() != nil:
			opts[recordlayer.IndexOptionHNSWEfConstruction] = oc.GetEfConstruction().GetText()
		case oc.CONNECTIVITY() != nil:
			opts[recordlayer.IndexOptionHNSWM] = oc.GetConnectivity().GetText()
		case oc.M_MAX() != nil:
			opts[recordlayer.IndexOptionHNSWMMax] = oc.GetMMax().GetText()
		case oc.M_MAX_0() != nil:
			opts[recordlayer.IndexOptionHNSWMMax0] = oc.GetMMaxZero().GetText()
		case oc.MAINTAIN_STATS_PROBABILITY() != nil:
			opts[recordlayer.IndexOptionHNSWMaintainStatsProbability] = oc.GetMaintainStatsProbability().GetText()
		case oc.METRIC() != nil:
			metric, err := vectorMetricName(oc.GetMetric())
			if err != nil {
				return nil, api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate,
					"vector index %q", indexName)
			}
			if method == "SPFRESH" {
				opts[recordlayer.IndexOptionSPFreshMetric] = metric
			} else {
				opts[recordlayer.IndexOptionVectorMetric] = metric
			}
		case oc.RABITQ_NUM_EX_BITS() != nil:
			// Both methods support it; each reads its own option namespace
			// (the residual quantizer for SPFresh, the node codes for HNSW) —
			// routing it to the hnsw key made the loud SPFRESH rejection
			// below swallow a knob SPFresh actually has.
			if method == "SPFRESH" {
				opts[recordlayer.IndexOptionSPFreshRaBitQNumExBits] = oc.GetRabitQNumExBits().GetText()
			} else {
				opts[recordlayer.IndexOptionHNSWRaBitQNumExBits] = oc.GetRabitQNumExBits().GetText()
			}
		case oc.SAMPLE_VECTOR_STATS_PROBABILITY() != nil:
			opts[recordlayer.IndexOptionHNSWSampleVectorStatsProbability] = oc.GetStatsProbability().GetText()
		case oc.STATS_THRESHOLD() != nil:
			opts[recordlayer.IndexOptionHNSWStatsThreshold] = oc.GetStatsThreshold().GetText()
		case oc.USE_RABITQ() != nil:
			opts[recordlayer.IndexOptionHNSWUseRaBitQ] = oc.GetUseRabitQ().GetText()
		}
	}
	if method == "SPFRESH" {
		for k := range opts {
			if strings.HasPrefix(k, "hnsw") {
				return nil, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
					"vector index %q: option %q is not supported with USING SPFRESH", indexName, k)
			}
		}
	}
	return opts, nil
}

// vectorMetricName maps an hnswMetric parse node to the Java metric enum
// name the maintainer's config reader expects (e.g. "EUCLIDEAN_METRIC").
func vectorMetricName(m antlrgen.IHnswMetricContext) (string, error) {
	mc, ok := m.(*antlrgen.HnswMetricContext)
	if !ok || m == nil {
		return "", api.NewError(api.ErrCodeInvalidSchemaTemplate, "missing metric")
	}
	switch {
	case mc.EUCLIDEAN_METRIC() != nil:
		return "EUCLIDEAN_METRIC", nil
	case mc.EUCLIDEAN_SQUARE_METRIC() != nil:
		return "EUCLIDEAN_SQUARE_METRIC", nil
	case mc.COSINE_METRIC() != nil:
		return "COSINE_METRIC", nil
	case mc.DOT_PRODUCT_METRIC() != nil:
		return "DOT_PRODUCT_METRIC", nil
	default:
		return "", api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported vector metric %q", m.GetText())
	}
}

// parseAsSelectIndexDefinition handles CREATE INDEX name AS SELECT … — the
// materialized-view index form (RFC-202).
//
// Mirrors Java's DdlVisitor.visitIndexAsSelectDefinition
// (DdlVisitor.java:205-219): build the metadata registered so far, plan the
// index's SELECT with the ordinary query front end against it, and hand the
// logical plan to the MaterializedViewIndexGenerator port
// (pkg/relational/core/query/ddl) — value and aggregate forms alike; the
// value/aggregate split is the generator's (RFC-202 D1, the internal branch
// at MaterializedViewIndexGenerator.java:187).
func parseAsSelectIndexDefinition(def *antlrgen.IndexAsSelectDefinitionContext, b *metadata.Builder) error {
	indexName := functions.StripIdentifierQuotes(def.GetIndexName().GetText())
	qt := def.QueryTerm()
	if qt == nil {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"index %q: missing query term", indexName)
	}
	// WITH ATTRIBUTES: the grammar's only attribute is LEGACY_EXTREMUM_EVER
	// (RelationalParser.g4:233-235); Java reads it as a presence flag
	// selecting the LONG-based extremum-ever maintainer (DdlVisitor.java:214).
	useLegacyExtremum := false
	if ia, ok := def.IndexAttributes().(*antlrgen.IndexAttributesContext); ok {
		for _, attr := range ia.AllIndexAttribute() {
			if ac, ok := attr.(*antlrgen.IndexAttributeContext); ok && ac.LEGACY_EXTREMUM_EVER() != nil {
				useLegacyExtremum = true
			}
		}
	}
	unique := def.UNIQUE() != nil
	// A WHERE clause makes the index SPARSE: the plan visitor installs the
	// resolved predicate on the plan's LogicalFilter, and the generator's
	// predicate arm (RFC-202 S5) serializes it into the index metadata
	// exactly as Java does (MaterializedViewIndexGenerator.java:169-172).

	// Plan the index's SELECT against the metadata built so far — Java's
	// metadataBuilder.build() + replaceSchemaTemplate (DdlVisitor.java:208-210).
	tmpl, err := b.Build()
	if err != nil {
		return err
	}
	md := tmpl.Underlying()
	if md == nil {
		// The generator over a metadata-less plan would build from unresolved
		// names (the catalog-less visitor fallback) — fail loudly (RFC-202 D4).
		return api.NewErrorf(api.ErrCodeInternalError,
			"index %q: schema template built without metadata", indexName)
	}
	// The same front-end pre-pass the production query path runs before it
	// lowers anything (planQuery, planDML). An index definition is a query, so
	// it is subject to the identical correct-or-loud boundary; without this the
	// generator would build a bare aggregate index from a windowed declaration
	// and persist it.
	if err := rejectWindowedAggregate(qt); err != nil {
		return fmt.Errorf("index %q: %w", indexName, err)
	}
	op, err := NewPlanVisitor(md).VisitQueryTerm(qt)
	if err != nil {
		return fmt.Errorf("index %q: %w", indexName, err)
	}
	if op == nil {
		return api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"index %q: unsupported index definition query", indexName)
	}
	// The mandatory FROM-resolution post-passes, the ONE sequence the
	// production query path also runs (runFromResolutionPostPasses,
	// cascades_generator.go) — column validation there is the source of
	// UNDEFINED_COLUMN for `AS SELECT nonexistent_col` (Java pin:
	// IndexTest.java:702-708). RFC-202 D4.
	if err := runFromResolutionPostPasses(op, defaultEmbeddedSchema, md, md); err != nil {
		return fmt.Errorf("index %q: %w", indexName, err)
	}

	gi, err := queryddl.Generate(op, md, queryddl.Options{UseLegacyExtremumEver: useLegacyExtremum})
	if err != nil {
		return fmt.Errorf("index %q: %w", indexName, err)
	}
	b.AddGeneratedIndex(gi.TableName, indexName, gi.Root, gi.IndexType, unique, gi.Options, gi.Predicate)
	return nil
}

// windowedAggregateInTree reports whether the parse tree contains an aggregate
// function with an OVER clause (a windowed aggregate, e.g. `SUM(v) OVER (PARTITION
// BY g)`). General window functions are unsupported (Java has no general window
// operator either — only the vector ROW_NUMBER QUALIFY case works). Without this
// check the aggregate planner silently DROPS the OVER clause and computes a bare
// aggregate, returning WRONG results (a single SUM instead of per-partition
// window values), so the query is rejected up front.
// rejectWindowedAggregate is the front-end pre-pass every surface that lowers a
// parse tree into a logical plan must run. It exists as one function rather
// than as an `if` repeated per call site because the OVER clause is destroyed
// by lowering: a surface that forgets the check cannot recover the distinction
// later, it can only produce a silently wrong plan. Index DDL is exactly that
// case — `CREATE INDEX i AS SELECT SUM(v) OVER (PARTITION BY g) FROM t` used to
// drop the OVER and PERSIST a global SUM index whose semantics are unrelated to
// the declaration.
func rejectWindowedAggregate(node antlr.Tree) error {
	if windowedAggregateInTree(node) {
		return api.NewError(api.ErrCodeUnsupportedQuery,
			"windowed aggregate (aggregate function with an OVER clause) is not supported")
	}
	return nil
}

func windowedAggregateInTree(node antlr.Tree) bool {
	if node == nil {
		return false
	}
	if awf, ok := node.(*antlrgen.AggregateWindowedFunctionContext); ok && awf.OverClause() != nil {
		return true
	}
	for i := 0; i < node.GetChildCount(); i++ {
		if windowedAggregateInTree(node.GetChild(i)) {
			return true
		}
	}
	return false
}

// parseColumnDefinitions parses an ordered columnDefinition list into
// ColumnSpecs — shared by the table pass and the struct pass exactly as
// Java's visitColumnDefinition serves visitTableDefinition and
// visitStructDefinition alike. b provides the custom-type lookup
// (tables-then-auxiliary-types, Java's findType order).
func parseColumnDefinitions(colDefs []antlrgen.IColumnDefinitionContext, b *metadata.Builder) ([]metadata.ColumnSpec, error) {
	var cols []metadata.ColumnSpec
	seen := make(map[string]bool)
	foldedSeen := make(map[string]string)

	for i, colDef := range colDefs {
		colName := functions.StripIdentifierQuotes(colDef.Uid().GetText())
		// Reject a duplicate column name with a clean 42701 here, before the proto
		// descriptor build would surface a leaky internal error (XX000
		// "protodesc.NewFile: descriptor already declared").
		if seen[colName] {
			return nil, api.NewErrorf(api.ErrCodeColumnAlreadyExists,
				"duplicate column name %q in table definition", colName)
		}
		seen[colName] = true
		// CASE-COLLIDING quoted names ("x" alongside X) are legitimately
		// distinct columns in Java, but Go's positional row layout folds
		// identifiers to upper case (PositionalTypeForDescriptor), so the
		// collision would panic deep in planning. Until the layout is
		// case-preserving (WS-N Phase D), reject the schema loudly at
		// CREATE instead of failing as XX000 on the first statement.
		if prev, dup := foldedSeen[strings.ToUpper(colName)]; dup {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"column names %q and %q collide case-insensitively — the positional row layout folds identifiers, so case-colliding quoted columns are not supported", prev, colName)
		}
		foldedSeen[strings.ToUpper(colName)] = colName
		ct := colDef.ColumnType()
		if ct == nil {
			return nil, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"column %q has no type", colName)
		}
		isRepeated := colDef.ARRAY() != nil
		nullable := true
		if cc := colDef.ColumnConstraint(); cc != nil {
			if nc, ok := cc.(*antlrgen.NullColumnConstraintContext); ok {
				if nn := nc.NullNotnull(); nn != nil && nn.NOT() != nil {
					nullable = false
				}
			}
		}
		// NOT NULL is rejected except on ARRAY — Java parity, ported
		// verbatim (DdlVisitor.visitColumnDefinition:
		// Assert.thatUnchecked(isRepeated || isNullable,
		// ErrorCode.UNSUPPORTED_OPERATION, ...)). The restriction is not a
		// grammar nicety: RecordMetaData has no way to represent scalar
		// non-nullability (every non-array field is stored LABEL_OPTIONAL),
		// so accepting NOT NULL here would create a constraint the stored
		// descriptor cannot carry — it silently vanished on every catalog
		// round-trip. For ARRAY the wrapper makes it representable
		// (flat repeated = NOT NULL, NullableArrayWrapper = nullable).
		if !isRepeated && !nullable {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation,
				"NOT NULL is only allowed for ARRAY column type")
		}
		dt, err := parseColumnType(ct, nullable, b)
		if err != nil {
			return nil, api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate,
				"column %q", colName)
		}
		if isRepeated {
			// Java: ArrayType.from(elementType.withNullable(false), isNullable)
			// The element type is always NOT NULL; the array itself carries nullability.
			dt = api.NewArrayType(dt.WithNullable(false), nullable)
		}
		cols = append(cols, metadata.NewColumnSpec(colName, dt, int32(i+1))) //nolint:gosec
	}
	return cols, nil
}

// parseTableDefinition extracts column specs and primary key column
// PATHS from a TableDefinitionContext. Each path is the key part's uid
// segments — Java's Identifier.fullyQualifiedName fed to
// RecordLayerTable.Builder.addPrimaryKeyPart (DdlVisitor.java:183-188,
// RecordLayerTable.java:295). The segments come from the parse tree, never
// from splitting a joined name: a quoted identifier may itself contain a
// literal '.', and once the segments are joined that dot is
// indistinguishable from a nested-path separator.
func parseTableDefinition(td antlrgen.ITableDefinitionContext, b *metadata.Builder) ([]metadata.ColumnSpec, [][]string, error) {
	cols, err := parseColumnDefinitions(td.AllColumnDefinition(), b)
	if err != nil {
		return nil, nil, err
	}
	seen := make(map[string]bool, len(cols))
	for _, c := range cols {
		seen[c.Name()] = true
	}

	var pkCols [][]string
	if pkDef := td.PrimaryKeyDefinition(); pkDef != nil {
		for _, fullID := range pkDef.FullIdList().AllFullId() {
			// The SEGMENTS are the parse tree's uid children. Joining them
			// (FullIdToName) and re-splitting on '.' would treat a quoted
			// column name containing a literal dot ("a.b") as a two-segment
			// nested path and reject the valid DDL.
			uids := fullID.AllUid()
			segments := make([]string, len(uids))
			for i, u := range uids {
				segments[i] = functions.StripIdentifierQuotes(u.GetText())
			}
			if len(segments) == 0 {
				continue
			}
			// Reject a PRIMARY KEY over an undefined column with a clean 42703 here,
			// before the metadata builder would surface a leaky internal error
			// (XX000 "build RecordMetaData: ... field not found in message").
			// A MULTI-SEGMENT part (id.a — a nested primary key through a
			// struct column, Java's RecordLayerTable.Builder.toKeyExpression
			// walk) is validated by its head segment; the struct-field descent
			// is checked when the key expression is built at Build() time.
			if !seen[segments[0]] {
				return nil, nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
					"primary key column %q is not a defined column",
					strings.Join(segments, "."))
			}
			pkCols = append(pkCols, segments)
		}
	}

	return cols, pkCols, nil
}

// parseColumnType maps a ColumnTypeContext to an api.DataType. A
// non-primitive type is a CUSTOM type reference (grammar: columnType :
// primitiveType | customType=uid): Java resolves it against the metadata
// under construction (SemanticAnalyzer.lookupType with
// metadataBuilder::findType) and falls back to an UnresolvedType
// placeholder for a forward reference, fixed up at build time.
func parseColumnType(ct antlrgen.IColumnTypeContext, nullable bool, b *metadata.Builder) (api.DataType, error) {
	pt := ct.PrimitiveType()
	if pt == nil {
		custom := ct.GetCustomType()
		if custom == nil {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"unsupported column type: %s", ct.GetText())
		}
		typeName := functions.StripIdentifierQuotes(custom.GetText())
		if found, ok := b.FindType(typeName); ok {
			return found.WithNullable(nullable), nil
		}
		return api.NewUnresolvedType(typeName, nullable), nil
	}
	switch {
	case pt.BOOLEAN() != nil:
		return api.NewBooleanType(nullable), nil
	case pt.INTEGER() != nil:
		return api.NewIntegerType(nullable), nil
	case pt.BIGINT() != nil:
		return api.NewLongType(nullable), nil
	case pt.FLOAT() != nil:
		return api.NewFloatType(nullable), nil
	case pt.DOUBLE() != nil:
		return api.NewDoubleType(nullable), nil
	case pt.STRING() != nil:
		return api.NewStringType(nullable), nil
	case pt.BYTES() != nil:
		return api.NewBytesType(nullable), nil
	case pt.UUID() != nil:
		return api.NewUUIDType(nullable), nil
	case pt.DATE() != nil:
		return api.NewDateType(nullable), nil
	case pt.TIMESTAMP() != nil:
		return api.NewTimestampType(nullable), nil
	case pt.VectorType() != nil:
		return parseVectorColumnType(pt.VectorType(), nullable)
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported column type: %s", ct.GetText())
	}
}

// parseVectorColumnType parses a VECTOR(dimensions, elementType) column
// type into an api.VectorType. Element-type precision: HALF=16, FLOAT=32,
// DOUBLE=64 bits per element. Mirrors Java's DataType.VectorType.
func parseVectorColumnType(vt antlrgen.IVectorTypeContext, nullable bool) (api.DataType, error) {
	vtc, ok := vt.(*antlrgen.VectorTypeContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported vector type context %T", vt)
	}
	dimsTok := vtc.GetDimensions()
	if dimsTok == nil {
		return nil, api.NewError(api.ErrCodeInvalidSchemaTemplate,
			"vector type requires a dimension count")
	}
	dims, err := strconv.Atoi(dimsTok.GetText())
	if err != nil || dims <= 0 {
		return nil, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"invalid vector dimension count %q", dimsTok.GetText())
	}
	precision, err := vectorPrecisionBits(vtc.VectorElementType())
	if err != nil {
		return nil, err
	}
	return api.NewVectorType(precision, dims, nullable), nil
}

// vectorPrecisionBits maps a vectorElementType to its bit precision.
func vectorPrecisionBits(et antlrgen.IVectorElementTypeContext) (int, error) {
	etc, ok := et.(*antlrgen.VectorElementTypeContext)
	if !ok || et == nil {
		return 0, api.NewError(api.ErrCodeInvalidSchemaTemplate,
			"vector type requires an element type")
	}
	switch {
	case etc.HALF() != nil:
		return 16, nil
	case etc.FLOAT() != nil:
		return 32, nil
	case etc.DOUBLE() != nil:
		return 64, nil
	default:
		return 0, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported vector element type %q", et.GetText())
	}
}

// ensureCatalogInit bootstraps the catalog. Retries on transient failure
// (unlike sync.Once, a mutex+bool allows retry when the previous attempt failed).
func (c *EmbeddedConnection) ensureCatalogInit(ctx context.Context) error {
	c.sess.CatalogMu.Lock()
	defer c.sess.CatalogMu.Unlock()
	if c.sess.CatalogReady {
		return nil
	}
	_, err := c.sess.DB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		txn := catalog.NewFDBTransaction(rctx)
		if initErr := c.sess.Catalog.Initialize(txn); initErr != nil {
			return nil, initErr
		}
		return nil, txn.Commit()
	})
	if err != nil {
		return err
	}
	c.sess.CatalogReady = true
	return nil
}

// Ping implements driver.Pinger. Bootstraps the catalog on first call.
func (c *EmbeddedConnection) Ping(ctx context.Context) error {
	if c.closed.Load() {
		return driver.ErrBadConn
	}
	return c.ensureCatalogInit(ctx)
}

// runDDL bootstraps the catalog on first call, then executes action.
func (c *EmbeddedConnection) runDDL(ctx context.Context, action apiddl.ConstantAction) error {
	if err := c.ensureCatalogInit(ctx); err != nil {
		return err
	}
	_, err := c.sess.DB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		txn := catalog.NewFDBTransaction(rctx)
		execErr := action.Execute(txn)
		if execErr != nil {
			return nil, execErr
		}
		return nil, txn.Commit()
	})
	return err
}

// parseSchemaIdentifier splits "/dbpath/schemaname" into its parts.
// If the identifier has no leading slash, the current dbPath is used.
// Mirrors Java's SemanticAnalyzer.parseSchemaIdentifier.
func parseSchemaIdentifier(id, currentDB string) (dbPath, schemaName string, err error) {
	if strings.HasPrefix(id, "/") {
		idx := strings.LastIndex(id, "/")
		if idx == len(id)-1 {
			return "", "", api.NewErrorf(api.ErrCodeInvalidParameter,
				"schema identifier %q must not end with /", id)
		}
		if idx == 0 {
			return "", "", api.NewErrorf(api.ErrCodeInvalidParameter,
				"schema identifier %q must include both database and schema segments", id)
		}
		return id[:idx], id[idx+1:], nil
	}
	return currentDB, id, nil
}

// validateDatabasePath checks that the path starts with / and has a non-empty name.
func validateDatabasePath(p string) error {
	if !strings.HasPrefix(p, "/") || len(p) < 2 || strings.HasSuffix(p, "/") {
		return api.NewErrorf(api.ErrCodeInvalidParameter,
			"database path must be /name (not empty, bare /, or trailing /): %q", p)
	}
	return nil
}

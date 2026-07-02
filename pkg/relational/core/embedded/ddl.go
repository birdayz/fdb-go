package embedded

import (
	"context"
	"database/sql/driver"
	"errors"
	"strconv"
	"strings"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	apiddl "fdb.dev/pkg/relational/api/ddl"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/functions"
	"fdb.dev/pkg/relational/core/metadata"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
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
	templateID := s.SchemaTemplateId().GetText()
	b := metadata.NewSchemaTemplateBuilder().SetName(templateID)

	// First pass: register tables (indexes reference them by name).
	for _, clause := range s.AllTemplateClause() {
		td := clause.TableDefinition()
		if td == nil {
			continue
		}
		tableName := functions.StripIdentifierQuotes(td.Uid().GetText())
		cols, pkCols, err := parseTableDefinition(td)
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
		b.AddTable(tableName, cols, pkCols)
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

// parseIndexDefinition handles a single CREATE INDEX clause within a schema template.
func parseIndexDefinition(idxDef antlrgen.IIndexDefinitionContext, b *metadata.Builder) error {
	switch def := idxDef.(type) {
	case *antlrgen.IndexOnSourceDefinitionContext:
		indexName := functions.StripIdentifierQuotes(def.GetIndexName().GetText())
		tableName := functions.StripIdentifierQuotes(def.GetSource().GetText())
		unique := def.UNIQUE() != nil
		// INCLUDE (covering index) is not yet wired through the SQL DDL layer. Java
		// SUPPORTS it (DdlVisitor.java:249 → addValueColumn, building a KeyWithValue
		// covering index), and Go's record layer HAS the machinery
		// (KeyWithValueExpression; index_maintainer.go), but Builder.AddIndex doesn't
		// carry included columns yet. Silently dropping INCLUDE would create a PLAIN
		// index where Java creates a COVERING one — a wire/DDL-portability divergence
		// (the same CREATE INDEX yields different index structures across engines). Fail
		// closed until covering-index DDL is implemented (TODO.md), matching the vector
		// path's own INCLUDE rejection above. ErrCodeUnsupportedOperation mirrors Java's
		// UNSUPPORTED_OPERATION used for unsupported INCLUDE (DdlVisitor.java:297).
		if def.IncludeClause() != nil {
			return api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"index %q: INCLUDE clause (covering index) is not yet supported", indexName)
		}
		var cols []string
		if cl := def.IndexColumnList(); cl != nil {
			for _, spec := range cl.AllIndexColumnSpec() {
				cols = append(cols, functions.StripIdentifierQuotes(spec.GetColumnName().GetText()))
			}
		}
		if len(cols) == 0 {
			return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"index %q has no columns", indexName)
		}
		b.AddIndex(tableName, indexName, cols, unique)
		return nil
	case *antlrgen.IndexAsSelectDefinitionContext:
		return parseAggregateIndexDefinition(def, b)
	case *antlrgen.VectorIndexDefinitionContext:
		return parseVectorIndexDefinition(def, b)
	default:
		return api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported index definition type %T", idxDef)
	}
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

// parseAggregateIndexDefinition handles CREATE INDEX name AS SELECT AGG(col) FROM table GROUP BY cols.
// Matches Java's MaterializedViewIndexGenerator: extracts the aggregate function,
// column, table, and grouping columns, then registers via Builder.AddAggregateIndex.
func parseAggregateIndexDefinition(def *antlrgen.IndexAsSelectDefinitionContext, b *metadata.Builder) error {
	indexName := functions.StripIdentifierQuotes(def.GetIndexName().GetText())

	qt := def.QueryTerm()
	if qt == nil {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"aggregate index %q: missing query term", indexName)
	}
	st, ok := qt.(*antlrgen.SimpleTableContext)
	if !ok {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"aggregate index %q: only simple SELECT queries are supported", indexName)
	}

	fc := st.FromClause()
	if fc == nil {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"aggregate index %q: FROM clause required", indexName)
	}
	ts := fc.TableSources()
	if ts == nil {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"aggregate index %q: no table source", indexName)
	}
	sources := ts.AllTableSource()
	if len(sources) != 1 {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"aggregate index %q: exactly one table source required", indexName)
	}
	tsb, ok := sources[0].(*antlrgen.TableSourceBaseContext)
	if !ok {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"aggregate index %q: unsupported table source type", indexName)
	}
	// An aggregate index is over a single table; parseAggregateIndexDefinition reads only the
	// leading table source and would otherwise silently drop any JOIN (and any AT-ordinality
	// clause on a joined source). Reject JOINs explicitly rather than ignore them.
	if len(tsb.AllJoinPart()) > 0 {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"aggregate index %q: JOIN is not supported in an aggregate index definition", indexName)
	}
	ati, ok := tsb.TableSourceItem().(*antlrgen.AtomTableItemContext)
	if !ok {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"aggregate index %q: table source must be a simple table reference", indexName)
	}
	// The grammar parses `AT atAlias` here too (RFC-140 / R3); reject it rather than silently
	// ignore the ordinal clause, which would otherwise build an index grouped by a same-named
	// real column instead. Removed in R5 when the ordinal is bound.
	if err := rejectAtOrdinality(ati); err != nil {
		return err
	}
	tableName := functions.FullIdToName(ati.TableName().FullId())

	selElems := st.SelectElements()
	if selElems == nil {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"aggregate index %q: SELECT clause required", indexName)
	}
	allElems := selElems.AllSelectElement()
	if len(allElems) != 1 {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"aggregate index %q: exactly one aggregate expression required in SELECT", indexName)
	}

	// CARDINALITY() index: `CREATE INDEX … AS SELECT CARDINALITY(arr) AS c …
	// ORDER BY c`. This is a VALUE index whose key is a function over the
	// array column (not a GROUP BY aggregate), so it routes to its own builder
	// entry. Mirrors Java's MaterializedViewIndexGenerator recognising a
	// CardinalityValue select element and emitting
	// function("cardinality", field("arr", …)).
	if cardColumn, ok := extractCardinalityColumnFromSelectElement(allElems[0]); ok {
		// GROUP BY is not meaningful for a cardinality value index (Java emits
		// a plain value index); reject it rather than silently drop it.
		if gb := st.GroupByClause(); gb != nil {
			return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"cardinality index %q: GROUP BY is not supported", indexName)
		}
		b.AddCardinalityIndex(tableName, indexName, cardColumn)
		return nil
	}

	aggType, aggColumn, err := extractAggregateFromSelectElement(allElems[0])
	if err != nil {
		return api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate,
			"aggregate index %q", indexName)
	}

	var groupColumns []string
	if gb := st.GroupByClause(); gb != nil {
		for _, item := range gb.AllGroupByItem() {
			colName, err := extractColumnNameFromExpression(item.Expression())
			if err != nil {
				return api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate,
					"aggregate index %q GROUP BY", indexName)
			}
			groupColumns = append(groupColumns, colName)
		}
	}

	b.AddAggregateIndex(tableName, indexName, groupColumns, aggType, aggColumn)
	return nil
}

// extractCardinalityColumnFromSelectElement detects a `CARDINALITY(field)`
// select element and returns the array column name (possibly dotted, e.g.
// "struct.int_arr"). CARDINALITY has no grammar token, so it parses as a
// bare-ID UserDefinedScalarFunctionCall — the SAME node walkUserDefinedScalarFunction
// matches for queries — and we detect it by typed node, never by GetText() on
// the SQL. Returns (col, true) on a match, ("", false) otherwise (the caller
// then tries the aggregate path).
func extractCardinalityColumnFromSelectElement(elem antlrgen.ISelectElementContext) (string, bool) {
	see, ok := elem.(*antlrgen.SelectExpressionElementContext)
	if !ok {
		return "", false
	}
	udf := findCardinalityCall(see.Expression())
	if udf == nil {
		return "", false
	}
	// The single argument is the array field reference; reuse the column-name
	// extractor (which walks for a FullColumnName), matching how the query
	// path resolves the CARDINALITY argument.
	fa := udf.FunctionArgs()
	if fa == nil {
		return "", false
	}
	fac, ok := fa.(*antlrgen.FunctionArgsContext)
	if !ok || len(fac.AllFunctionArg()) != 1 {
		return "", false
	}
	argCtx, ok := fac.AllFunctionArg()[0].(*antlrgen.FunctionArgContext)
	if !ok || argCtx.Expression() == nil {
		return "", false
	}
	fcn := findFullColumnName(argCtx.Expression())
	if fcn == nil {
		return "", false
	}
	return functions.FullIdToName(fcn.FullId()), true
}

// findCardinalityCall walks an expression tree for a bare-ID
// UserDefinedScalarFunctionCall whose name is CARDINALITY (the way the function
// parses, lacking a grammar token). Detection is by typed node + the resolved
// name, never by string-matching the SQL text.
func findCardinalityCall(node antlr.Tree) *antlrgen.UserDefinedScalarFunctionCallContext {
	if node == nil {
		return nil
	}
	if udf, ok := node.(*antlrgen.UserDefinedScalarFunctionCallContext); ok {
		if nameCtx, ok := udf.UserDefinedScalarFunctionName().(*antlrgen.UserDefinedScalarFunctionNameContext); ok &&
			nameCtx.ID() != nil &&
			strings.EqualFold(nameCtx.ID().GetText(), "CARDINALITY") {
			return udf
		}
	}
	for i := 0; i < node.GetChildCount(); i++ {
		if result := findCardinalityCall(node.GetChild(i)); result != nil {
			return result
		}
	}
	return nil
}

// extractAggregateFromSelectElement walks a select element's expression tree to find
// the aggregate function call and returns (aggType, aggColumn). aggColumn is empty for COUNT(*).
func extractAggregateFromSelectElement(elem antlrgen.ISelectElementContext) (string, string, error) {
	see, ok := elem.(*antlrgen.SelectExpressionElementContext)
	if !ok {
		return "", "", api.NewError(api.ErrCodeInvalidSchemaTemplate,
			"select element must be an expression")
	}
	awf := findAggregateWindowedFunction(see.Expression())
	if awf == nil {
		return "", "", api.NewError(api.ErrCodeInvalidSchemaTemplate,
			"select element must contain an aggregate function (SUM, COUNT, MIN, MAX)")
	}

	fnName := strings.ToUpper(awf.GetFunctionName().GetText())

	switch fnName {
	case "COUNT":
		if awf.GetStarArg() != nil {
			return "COUNT", "", nil
		}
		if fa := awf.FunctionArg(); fa != nil {
			col, err := extractColumnNameFromExpression(fa.Expression())
			if err != nil {
				return "", "", err
			}
			return "COUNT_NOT_NULL", col, nil
		}
		return "", "", api.NewError(api.ErrCodeInvalidSchemaTemplate,
			"COUNT requires * or a column argument")
	case "SUM", "MIN", "MAX":
		fa := awf.FunctionArg()
		if fa == nil {
			return "", "", api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"%s requires a column argument", fnName)
		}
		col, err := extractColumnNameFromExpression(fa.Expression())
		if err != nil {
			return "", "", err
		}
		return fnName, col, nil
	case "MIN_EVER":
		fa := awf.FunctionArg()
		if fa == nil {
			return "", "", api.NewError(api.ErrCodeInvalidSchemaTemplate,
				"MIN_EVER requires a column argument")
		}
		col, err := extractColumnNameFromExpression(fa.Expression())
		if err != nil {
			return "", "", err
		}
		return "MIN_EVER_TUPLE", col, nil
	case "MAX_EVER":
		fa := awf.FunctionArg()
		if fa == nil {
			return "", "", api.NewError(api.ErrCodeInvalidSchemaTemplate,
				"MAX_EVER requires a column argument")
		}
		col, err := extractColumnNameFromExpression(fa.Expression())
		if err != nil {
			return "", "", err
		}
		return "MAX_EVER_TUPLE", col, nil
	default:
		return "", "", api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"unsupported aggregate function %q; supported: SUM, COUNT, MIN, MAX, MIN_EVER, MAX_EVER", fnName)
	}
}

// findAggregateWindowedFunction walks an expression tree to find an AggregateWindowedFunctionContext.
func findAggregateWindowedFunction(expr antlrgen.IExpressionContext) *antlrgen.AggregateWindowedFunctionContext {
	if expr == nil {
		return nil
	}
	return findAggregateInTree(expr)
}

func findAggregateInTree(node antlr.Tree) *antlrgen.AggregateWindowedFunctionContext {
	if awf, ok := node.(*antlrgen.AggregateWindowedFunctionContext); ok {
		return awf
	}
	for i := 0; i < node.GetChildCount(); i++ {
		if result := findAggregateInTree(node.GetChild(i)); result != nil {
			return result
		}
	}
	return nil
}

// windowedAggregateInTree reports whether the parse tree contains an aggregate
// function with an OVER clause (a windowed aggregate, e.g. `SUM(v) OVER (PARTITION
// BY g)`). General window functions are unsupported (Java has no general window
// operator either — only the vector ROW_NUMBER QUALIFY case works). Without this
// check the aggregate planner silently DROPS the OVER clause and computes a bare
// aggregate, returning WRONG results (a single SUM instead of per-partition
// window values), so the query is rejected up front.
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

// extractColumnNameFromExpression extracts a simple column name from an expression
// that is a bare column reference (fullColumnName).
func extractColumnNameFromExpression(expr antlrgen.IExpressionContext) (string, error) {
	if expr == nil {
		return "", api.NewError(api.ErrCodeInvalidSchemaTemplate, "expression is nil")
	}
	fcn := findFullColumnName(expr)
	if fcn == nil {
		return "", api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"expected a column reference, got %q", expr.GetText())
	}
	return functions.FullIdToName(fcn.FullId()), nil
}

func findFullColumnName(node antlr.Tree) *antlrgen.FullColumnNameContext {
	if fcn, ok := node.(*antlrgen.FullColumnNameContext); ok {
		return fcn
	}
	for i := 0; i < node.GetChildCount(); i++ {
		if result := findFullColumnName(node.GetChild(i)); result != nil {
			return result
		}
	}
	return nil
}

// parseTableDefinition extracts column specs and primary key column
// names from a TableDefinitionContext.
func parseTableDefinition(td antlrgen.ITableDefinitionContext) ([]metadata.ColumnSpec, []string, error) {
	var cols []metadata.ColumnSpec
	var pkCols []string
	seen := make(map[string]bool)

	for i, colDef := range td.AllColumnDefinition() {
		colName := functions.StripIdentifierQuotes(colDef.Uid().GetText())
		// Reject a duplicate column name with a clean 42701 here, before the proto
		// descriptor build would surface a leaky internal error (XX000
		// "protodesc.NewFile: descriptor already declared").
		if seen[colName] {
			return nil, nil, api.NewErrorf(api.ErrCodeColumnAlreadyExists,
				"duplicate column name %q in table definition", colName)
		}
		seen[colName] = true
		ct := colDef.ColumnType()
		if ct == nil {
			return nil, nil, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
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
		// Go extension: NOT NULL on any column type is valid (SQL standard).
		// Java 4.11.1.0 restricts NOT NULL to ARRAY only due to a
		// RecordMetaData limitation (see TODO #50). We intentionally
		// don't replicate that restriction.
		dt, err := parseColumnType(ct, nullable)
		if err != nil {
			return nil, nil, api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate,
				"column %q", colName)
		}
		if isRepeated {
			// Java: ArrayType.from(elementType.withNullable(false), isNullable)
			// The element type is always NOT NULL; the array itself carries nullability.
			dt = api.NewArrayType(dt.WithNullable(false), nullable)
		}
		cols = append(cols, metadata.NewColumnSpec(colName, dt, int32(i+1)))
	}

	if pkDef := td.PrimaryKeyDefinition(); pkDef != nil {
		for _, fullID := range pkDef.FullIdList().AllFullId() {
			pkCol := functions.FullIdToName(fullID)
			// Reject a PRIMARY KEY over an undefined column with a clean 42703 here,
			// before the metadata builder would surface a leaky internal error
			// (XX000 "build RecordMetaData: ... field not found in message").
			if !seen[pkCol] {
				return nil, nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
					"primary key column %q is not a defined column", pkCol)
			}
			pkCols = append(pkCols, pkCol)
		}
	}

	return cols, pkCols, nil
}

// parseColumnType maps a ColumnTypeContext to an api.DataType.
func parseColumnType(ct antlrgen.IColumnTypeContext, nullable bool) (api.DataType, error) {
	pt := ct.PrimitiveType()
	if pt == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"only primitive column types are supported")
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

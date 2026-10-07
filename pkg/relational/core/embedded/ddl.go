package embedded

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	apiddl "fdb.dev/pkg/relational/api/ddl"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/functions"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	queryddl "fdb.dev/pkg/relational/core/query/ddl"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/logical"
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

// databasePathOf is a DDL statement's database path as Java reads it: the
// path's uid normalized as an identifier (DdlVisitor's visitUid(ctx.path()
// .uid()), IdentifierVisitor.visitUid: normalizeString of the uid's text), so
// an unquoted path folds to upper case whole and a quoted one is kept as
// written (measured against the JVM: `create database /test/x` stores
// /TEST/X; conformance "the DSN's schema option reaches the schema Java's
// does"). Go stored the path verbatim, a database row Java cannot reach.
//
// Every statement that takes a path reads it this way in Java: CREATE/DROP
// DATABASE, CREATE/DROP SCHEMA, SHOW DATABASES WITH PREFIX
// (MetadataPlanVisitor.visitShowDatabasesStatement, whose prefix Java then
// ignores), DESCRIBE SCHEMA and COPY. The first five go through here; DESCRIBE
// SCHEMA (0A000 in Go, RFC-257 WS-E) and COPY (no Go route, WS-K) must when
// they are ported.
func databasePathOf(text string) string {
	return functions.NormalizeIdentifier(text)
}

// templateNameOf is a schema template's name as Java reads it: the uid
// normalized as an identifier, at every statement that names a template
// (DdlVisitor.visitUid at :495 CREATE SCHEMA TEMPLATE, :573 CREATE SCHEMA …
// WITH TEMPLATE, :607 DROP SCHEMA TEMPLATE). An unquoted name folds to upper
// case and a quoted one keeps its case without its quotes, so `create schema
// template t1` stores the row and the records file Java's DDL writes (T1).
func templateNameOf(text string) string {
	return functions.NormalizeIdentifier(text)
}

func (c *EmbeddedConnection) execCreateDatabase(ctx context.Context, s *antlrgen.CreateDatabaseStatementContext) (int64, error) {
	dbPath := databasePathOf(s.Path().GetText())
	if err := validateDatabasePath(dbPath); err != nil {
		return 0, err
	}
	if err := c.checkDDLDatabaseScope("CREATE DATABASE", dbPath); err != nil {
		return 0, err
	}
	action := c.sess.Factory.CreateDatabase(dbPath, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execDropDatabase(ctx context.Context, s *antlrgen.DropDatabaseStatementContext) (int64, error) {
	dbPath := databasePathOf(s.Path().GetText())
	if err := validateDatabasePath(dbPath); err != nil {
		return 0, err
	}
	if err := c.checkDDLDatabaseScope("DROP DATABASE", dbPath); err != nil {
		return 0, err
	}
	throwIfNotExist := s.IfExists() == nil
	action := c.sess.Factory.DropDatabase(dbPath, throwIfNotExist, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execCreateSchema(ctx context.Context, s *antlrgen.CreateSchemaStatementContext) (int64, error) {
	// Java normalizes the whole uid, a path or a bare name, then splits it
	// (visitUid, then SemanticAnalyzer.parseSchemaIdentifier): `create schema
	// /FRL/db/test` creates TEST in /FRL/DB, and a quoted path keeps both segments.
	schemaText := databasePathOf(s.SchemaId().GetText())
	dbPath, schemaName, err := parseSchemaIdentifier(schemaText, c.sess.DBPath)
	if err != nil {
		return 0, err
	}
	if err := c.checkDDLDatabaseScope("CREATE SCHEMA", dbPath); err != nil {
		return 0, err
	}
	templateID := templateNameOf(s.SchemaTemplateId().GetText())
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
	// Same normalization as execCreateSchema (DdlVisitor.visitDropSchemaStatement
	// reads visitUid(ctx.uid())): DROP SCHEMA /FRL/db/test drops TEST in /FRL/DB.
	//
	// Unlike CREATE SCHEMA, DROP SCHEMA takes a path: a bare uid names no
	// database, and Java refuses it whatever database the connection is on
	// (DdlVisitor.java:598-600, the raw uid text in single quotes). Resolving
	// it against the session's database dropped a schema Java keeps.
	rawUid := s.Uid().GetText()
	schemaText := databasePathOf(rawUid)
	if !strings.HasPrefix(schemaText, "/") {
		return 0, api.NewErrorf(api.ErrCodeUnknownDatabase,
			"invalid database identifier in '%s'", rawUid)
	}
	dbPath, schemaName, err := parseSchemaIdentifier(schemaText, c.sess.DBPath)
	if err != nil {
		return 0, err
	}
	if err := c.checkDDLDatabaseScope("DROP SCHEMA", dbPath); err != nil {
		return 0, err
	}
	action := c.sess.Factory.DropSchema(dbPath, schemaName, *api.NoOptions())
	if err := c.runDDL(ctx, action); err != nil {
		return 0, err
	}
	c.invalidateSchemaCache(dbPath, schemaName)
	return 0, nil
}

func (c *EmbeddedConnection) execDropSchemaTemplate(ctx context.Context, s *antlrgen.DropSchemaTemplateStatementContext) (int64, error) {
	if err := c.checkSchemaTemplateDDLAllowed("DROP SCHEMA TEMPLATE"); err != nil {
		return 0, err
	}
	templateID := templateNameOf(s.Uid().GetText())
	throwIfNotExist := s.IfExists() == nil
	action := c.sess.Factory.DropSchemaTemplate(templateID, throwIfNotExist, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execCreateSchemaTemplate(ctx context.Context, s *antlrgen.CreateSchemaTemplateStatementContext) (int64, error) {
	if err := c.checkSchemaTemplateDDLAllowed("CREATE SCHEMA TEMPLATE"); err != nil {
		return 0, err
	}
	tmpl, err := buildSchemaTemplate(s)
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

// buildSchemaTemplate is Go's one DDL front end, Java's
// DdlVisitor.visitCreateSchemaTemplateStatement (:493-566): it builds the
// schema template a CREATE SCHEMA TEMPLATE statement declares, and nothing
// else. The execution path (execCreateSchemaTemplate) saves what it returns;
// the tooling path (buildSchemaTemplateFromDDL, the planner harness and the
// conformance oracle) returns it, so both build the same metadata by
// construction (RFC-257 WS-J section 3.4; they were two copies that had
// already diverged once, over WITH OPTIONS).
func buildSchemaTemplate(s *antlrgen.CreateSchemaTemplateStatementContext) (*metadata.RecordLayerSchemaTemplate, error) {
	// Java's AstNormalizer walks the whole statement before DdlVisitor builds
	// any of it, so its faults win over every clause of the template.
	if err := rejectNormalizerFaults(s); err != nil {
		return nil, err
	}
	templateID := templateNameOf(s.SchemaTemplateId().GetText())
	b := metadata.NewSchemaTemplateBuilder().SetName(templateID)

	// WITH OPTIONS(...) — ENABLE_LONG_ROWS / INTERMINGLE_TABLES / STORE_ROW_VERSIONS.
	// Mirrors Java's DdlVisitor.visitCreateSchemaTemplateStatement: applied before
	// the table/index passes below, since intermingleTbls changes how AddTable's
	// primary keys are compiled at Build() time (buildPrimaryKeyExpression prepends
	// RecordTypeKey() unless intermingled), and store_row_versions decides whether
	// the __ROW_VERSION pseudo-column exists for index planning.
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
				return nil, api.NewErrorf(api.ErrCodeSyntaxError,
					"unknown option in schema template creation: %s", opt.GetText())
			}
		}
	}

	registerEnumDefinitions(s.AllTemplateClause(), b)
	if err := registerStructDefinitions(s.AllTemplateClause(), b); err != nil {
		return nil, err
	}

	// First pass: register tables (indexes reference them by name).
	for _, clause := range s.AllTemplateClause() {
		td := clause.TableDefinition()
		if td == nil {
			continue
		}
		tableName := functions.NormalizeIdentifier(td.Uid().GetText())
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
				return nil, err
			}
			return nil, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"table %q: %v", tableName, err)
		}
		b.AddTablePrimaryKeyPaths(tableName, cols, pkCols)
	}

	// Stored queries, with their DECLAREd functions rewritten to standalone
	// temporary functions (DdlVisitor.rewriteDeclaredFunctionToStandalone).
	for _, clause := range s.AllTemplateClause() {
		sq := clause.StoredQueryDefinition()
		if sq == nil {
			continue
		}
		var temps []string
		if db := sq.DeclareBlock(); db != nil {
			for _, df := range db.AllDeclaredFunction() {
				temps = append(temps, "CREATE TEMPORARY FUNCTION "+ctxText(df.GetFunctionName())+
					ctxText(df.SqlParameterDeclarationList())+" ON COMMIT DROP FUNCTION AS "+ctxText(df.GetFunctionBody()))
			}
		}
		b.AddStoredQuery(functions.NormalizeIdentifier(sq.GetQueryName().GetText()), ctxText(sq.GetStoredQuery()), temps)
	}

	// SQL functions, then views, in clause order, each compiled against the
	// template so far (DdlVisitor.java:551-558).
	for _, clause := range s.AllTemplateClause() {
		if fd := clause.SqlInvokedFunction(); fd != nil {
			if err := registerFunction(fd, b); err != nil {
				return nil, err
			}
		}
	}

	// Views, in clause order, each compiled against the template so far.
	for _, clause := range s.AllTemplateClause() {
		if vd := clause.ViewDefinition(); vd != nil {
			if err := registerView(vd, b); err != nil {
				return nil, err
			}
		}
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
				return nil, err
			}
			return nil, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate, "index: %v", err)
		}
	}
	// Java generates every index first and then moves each index's table to the
	// end, in clause order (DdlVisitor.java:559-564), which decides the record
	// type keys, union field numbers and index versions the template stores.
	b.MoveIndexedTablesToEnd()

	return b.Build()
}

// registerView is Java's DdlVisitor.getViewMetadata: the query text as
// written, no prepared parameters, compiled against the template so far.
func registerView(vd antlrgen.IViewDefinitionContext, b *metadata.Builder) error {
	name := functions.FullIdToName(vd.GetViewName())
	q := vd.GetViewQuery()
	if containsPreparedParameter(q) {
		return api.NewError(api.ErrCodeSyntaxError, "found prepared parameter(s) in SQL statement")
	}
	definition := q.GetStart().GetInputStream().GetText(q.GetStart().GetStart(), q.GetStop().GetStop())
	tmpl, err := b.Build()
	if err != nil {
		return err
	}
	if md := tmpl.Underlying(); md != nil {
		parsed, err := parseQueryWithFunctions(definition, metaDataFunctions(md))
		if err != nil {
			return err
		}
		// A sliding-window QUALIFY is kept by the vector index over the view
		// (its RowNumberWindowPredicate), not evaluated; the rest compiles.
		if _, rest, ok := slidingWindowQualify(parsed, definition); ok {
			if parsed, err = parseQueryWithFunctions(rest, metaDataFunctions(md)); err != nil {
				return err
			}
		}
		if _, err := NewPlanVisitorWithTemplate(md, b.Name()).VisitQuery(parsed); err != nil {
			return err
		}
	}
	return b.AddView(name, definition)
}

// registerFunction is Java's DdlVisitor.getInvokedRoutineMetadata: a
// table-valued function is stored as a RawSqlFunction, `CREATE ` plus the text
// as written, after its body compiles against the template so far; a macro
// (a RETURN or AS expression body) as its serialized body Value.
func registerFunction(fd antlrgen.ISqlInvokedFunctionContext, b *metadata.Builder) error {
	if containsPreparedParameter(fd) {
		return api.NewError(api.ErrCodeSyntaxError, "found prepared parameter(s) in SQL statement")
	}
	if err := checkRoutineCharacteristics(fd.FunctionSpecification()); err != nil {
		return err
	}
	tmpl, err := b.Build()
	if err != nil {
		return err
	}
	md := tmpl.Underlying()
	if body, isMacro := fd.RoutineBody().(*antlrgen.UserDefinedMacroFunctionStatementBodyContext); isMacro {
		macro, err := buildMacroFunction(fd.FunctionSpecification(), body, md, b.AuxiliaryStructDescriptor)
		if err != nil {
			return err
		}
		stored, err := macro.ToProto()
		if err != nil {
			return api.WrapErrorf(err, api.ErrCodeUnsupportedOperation, "function %s", macro.Name)
		}
		return b.AddFunction(macro.Name, stored)
	}
	if fd.FunctionSpecification().ReturnsClause() != nil {
		return api.NewError(api.ErrCodeUnsupportedOperation, "unsupported explicit return type for SQL table function")
	}
	fn, err := sqlFunctionOf(fd.FunctionSpecification(), fd.RoutineBody())
	if err != nil {
		return err
	}
	if md != nil {
		if err := compileSQLFunction(fn, md, b.Name()); err != nil {
			return err
		}
	}
	name, definition := fn.name, "CREATE "+ctxText(fd)
	return b.AddFunction(name, &gen.PUserDefinedFunction{SpecificFunction: &gen.PUserDefinedFunction_SqlFunction{
		SqlFunction: &gen.PRawSqlFunction{Name: &name, Definition: &definition},
	}})
}

// checkRoutineCharacteristics is visitSqlInvokedFunction's validations.
func checkRoutineCharacteristics(spec antlrgen.IFunctionSpecificationContext) error {
	props := spec.RoutineCharacteristics()
	if nc := props.NullCallClause(); nc != nil && nc.RETURNS() != nil {
		return api.NewError(api.ErrCodeUnsupportedOperation, "only CALLED ON NULL INPUT clause is supported")
	}
	if ps := props.ParameterStyle(); ps != nil && ps.SQL() == nil {
		return api.NewError(api.ErrCodeUnsupportedOperation, "only sql-style parameters are supported")
	}
	if lc := props.LanguageClause(); lc != nil && lc.LanguageName().JAVA() != nil {
		return api.NewError(api.ErrCodeUnsupportedOperation, "only sql-language functions are supported")
	}
	return nil
}

// compileSQLFunction plans the body with every parameter bound to a NULL of
// its declared type, as Java compiles it against a typed parameter row.
func compileSQLFunction(fn *sqlFunction, md *recordlayer.RecordMetaData, templateName string) error {
	cols := make([]string, len(fn.params))
	for i, p := range fn.params {
		cols[i] = p.column("NULL", true)
	}
	sql := "SELECT * FROM (" + fn.body + ") AS F"
	if len(cols) > 0 {
		sql = "SELECT F.* FROM (SELECT " + strings.Join(cols, ", ") + ") AS P, (" + fn.body + ") AS F"
	}
	q, err := parseQueryWithFunctions(sql, metaDataFunctions(md))
	if err != nil {
		return err
	}
	_, err = NewPlanVisitorWithTemplate(md, templateName).VisitQuery(q)
	return err
}

func containsPreparedParameter(n antlr.Tree) bool {
	if _, ok := n.(*antlrgen.PreparedStatementParameterContext); ok {
		return true
	}
	for i := 0; i < n.GetChildCount(); i++ {
		if containsPreparedParameter(n.GetChild(i)) {
			return true
		}
	}
	return false
}

// registerEnumDefinitions is the enum pass: CREATE TYPE AS ENUM registers an
// auxiliary type as Java's DdlVisitor.visitEnumDefinition does (:480-490):
// the name an identifier, the values the string literals as written
// (normalizeStringLiteral), numbered 0..n-1 in declaration order, the type
// not nullable (a column's nullability is the column's). Java registers it
// inside the clause loop that partitions the other clauses (:519-521), so
// before any struct or table is visited: it runs first here too, which is what
// makes a later struct or table of the same name the one refused. The builder
// emits the enum only where a table's closure reaches it
// (fileEmitter.enums).
func registerEnumDefinitions(clauses []antlrgen.ITemplateClauseContext, b *metadata.Builder) {
	for _, clause := range clauses {
		ed := clause.EnumDefinition()
		if ed == nil {
			continue
		}
		literals := ed.AllSTRING_LITERAL()
		enumValues := make([]api.EnumValue, len(literals))
		for i, l := range literals {
			enumValues[i] = api.NewEnumValue(functions.StripStringLiteralQuotes(l.GetText()), i)
		}
		b.AddAuxiliaryType(api.NewEnumType(functions.NormalizeIdentifier(ed.Uid().GetText()), enumValues, false))
	}
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
		structName := functions.NormalizeIdentifier(sd.Uid().GetText())
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
			kind, indexName, functions.NormalizeIdentifier(sc.GetColumnName().GetText()))
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
	indexName := functions.NormalizeIdentifier(def.GetIndexName().GetText())
	// Match the sibling IndexOnSourceDefinition path, which registers and
	// looks up the table by the raw (unnormalized) source text.
	tableName := functions.NormalizeIdentifier(def.GetSource().GetText())

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
			vecCols = append(vecCols, functions.NormalizeIdentifier(spec.GetColumnName().GetText()))
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
			partitionCols = append(partitionCols, functions.NormalizeIdentifier(spec.GetColumnName().GetText()))
		}
	}

	method := "HNSW"
	if def.GetEngine() != nil {
		method = strings.ToUpper(def.GetEngine().GetText())
	}
	options, order, err := parseVectorIndexOptions(def.VectorIndexOptions(), indexName, method)
	if err != nil {
		return err
	}

	var predicate *gen.Predicate
	if !b.HasTable(tableName) {
		table, mapping, window, err := plainProjectionView(b, tableName)
		if err != nil {
			return err
		}
		predicate = window
		if mapping != nil {
			tableName = table
			vecCols[0] = mapping[vecCols[0]]
			for i, c := range partitionCols {
				partitionCols[i] = mapping[c]
			}
			if vecCols[0] == "" || slices.Contains(partitionCols, "") {
				return api.NewErrorf(api.ErrCodeUndefinedColumn, "vector index %q: column not in view", indexName)
			}
		}
	}
	b.AddVectorIndexOrdered(method, tableName, indexName, vecCols[0], partitionCols, options, order)
	b.SetIndexPredicate(tableName, indexName, predicate)
	return nil
}

// plainProjectionView maps a view that plainly projects columns of one table
// (no filter, no computation) to that table and its view-to-column names; a
// vector index over it indexes the same records. Any other view is refused.
func plainProjectionView(b *metadata.Builder, name string) (string, map[string]string, *gen.Predicate, error) {
	tmpl, err := b.Build()
	if err != nil {
		return "", nil, nil, err
	}
	md := tmpl.Underlying()
	if md == nil {
		return "", nil, nil, nil
	}
	view := findView(md, name)
	if view == nil {
		return "", nil, nil, nil
	}
	q, err := parser.ParseView(view.GetDefinition())
	if err != nil {
		return "", nil, nil, err
	}
	window, rest, isWindow := slidingWindowQualify(q, view.GetDefinition())
	if isWindow {
		if q, err = parser.ParseView(rest); err != nil {
			return "", nil, nil, err
		}
	}
	op, err := NewPlanVisitorWithTemplate(md, b.Name()).VisitQuery(q)
	if err != nil {
		return "", nil, nil, err
	}
	proj, ok := op.(*logical.LogicalProject)
	var scan *logical.LogicalScan
	if ok {
		scan, ok = proj.Input.(*logical.LogicalScan)
	}
	if !ok || scan.Source.Producer() != nil {
		return "", nil, nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"a vector index over view %q is supported only when the view plainly projects one table", name)
	}
	mapping := make(map[string]string, len(proj.Projections))
	for i, col := range proj.Projections {
		if i < len(proj.IsComputed) && proj.IsComputed[i] {
			continue
		}
		out := col
		if i < len(proj.Aliases) && proj.Aliases[i] != "" {
			out = proj.Aliases[i]
		}
		mapping[strings.ToUpper(out)] = parseColRef(strings.ToUpper(col)).bare()
	}
	return scan.Table, mapping, window, nil
}

type vectorOptionKind int

const (
	vectorOptInt vectorOptionKind = iota
	vectorOptDouble
	vectorOptBool
	vectorOptMetric
)

type vectorSQLOption struct {
	key     string // canonical index option; for SPFRESH the spfresh key, "" if unsupported
	spfresh string
	kind    vectorOptionKind
	engines string // "*" both Java engines, "HNSW", "GUARDIANN"
}

// vectorSQLOptions is Java's DdlVisitor.SUPPORTED_VECTOR_OPTIONS; the spfresh
// column is Go's SPFRESH engine.
var vectorSQLOptions = map[string]vectorSQLOption{
	"metric":                              {recordlayer.IndexOptionVectorMetric, recordlayer.IndexOptionSPFreshMetric, vectorOptMetric, "*"},
	"use_rabitq":                          {recordlayer.IndexOptionHNSWUseRaBitQ, "", vectorOptBool, "*"},
	"rabitq_num_ex_bits":                  {recordlayer.IndexOptionHNSWRaBitQNumExBits, recordlayer.IndexOptionSPFreshRaBitQNumExBits, vectorOptInt, "*"},
	"maintain_stats_probability":          {recordlayer.IndexOptionHNSWMaintainStatsProbability, "", vectorOptDouble, "*"},
	"sample_vector_stats_probability":     {recordlayer.IndexOptionHNSWSampleVectorStatsProbability, "", vectorOptDouble, "*"},
	"stats_threshold":                     {recordlayer.IndexOptionHNSWStatsThreshold, "", vectorOptInt, "*"},
	"connectivity":                        {recordlayer.IndexOptionHNSWM, "", vectorOptInt, "HNSW"},
	"ef_construction":                     {recordlayer.IndexOptionHNSWEfConstruction, "", vectorOptInt, "HNSW"},
	"m_max":                               {recordlayer.IndexOptionHNSWMMax, "", vectorOptInt, "HNSW"},
	"m_max_0":                             {recordlayer.IndexOptionHNSWMMax0, "", vectorOptInt, "HNSW"},
	"primary_cluster_min":                 {recordlayer.IndexOptionGuardiannPrimaryClusterMin, "", vectorOptInt, "GUARDIANN"},
	"primary_cluster_hard_max":            {recordlayer.IndexOptionGuardiannPrimaryClusterHardMax, "", vectorOptInt, "GUARDIANN"},
	"primary_cluster_max":                 {recordlayer.IndexOptionGuardiannPrimaryClusterMax, "", vectorOptInt, "GUARDIANN"},
	"underreplicated_primary_cluster_max": {recordlayer.IndexOptionGuardiannUnderreplicatedPrimaryClusterMax, "", vectorOptInt, "GUARDIANN"},
	"replicated_cluster_max_writes":       {recordlayer.IndexOptionGuardiannReplicatedClusterMaxWrites, "", vectorOptInt, "GUARDIANN"},
	"replicated_cluster_target":           {recordlayer.IndexOptionGuardiannReplicatedClusterTarget, "", vectorOptInt, "GUARDIANN"},
	"replication_priority_min":            {recordlayer.IndexOptionGuardiannReplicationPriorityMin, "", vectorOptDouble, "GUARDIANN"},
	"insert_max_candidate_clusters":       {recordlayer.IndexOptionGuardiannInsertMaxCandidateClusters, "", vectorOptInt, "GUARDIANN"},
	"delete_max_candidate_clusters":       {recordlayer.IndexOptionGuardiannDeleteMaxCandidateClusters, "", vectorOptInt, "GUARDIANN"},
	"split_num_nearest_clusters":          {recordlayer.IndexOptionGuardiannSplitNumNearestClusters, "", vectorOptInt, "GUARDIANN"},
	"merge_num_nearest_clusters":          {recordlayer.IndexOptionGuardiannMergeNumNearestClusters, "", vectorOptInt, "GUARDIANN"},
	"reassign_num_neighboring_clusters":   {recordlayer.IndexOptionGuardiannReassignNumNeighboringClusters, "", vectorOptInt, "GUARDIANN"},
	"collapse_min_duplicates":             {recordlayer.IndexOptionGuardiannCollapseMinDuplicates, "", vectorOptInt, "GUARDIANN"},
}

// parseVectorIndexOptions is Java's DdlVisitor.parseVectorOptions: an unknown
// option or one for another engine is 0A000, a duplicate or unparsable value
// 42601, and values are written as Java's String.valueOf writes them.
// The keys come back in insertion order, which fixes Java's stored order.
func parseVectorIndexOptions(ctx antlrgen.IVectorIndexOptionsContext, indexName, engine string) (map[string]string, []string, error) {
	opts := map[string]string{}
	var order []string
	if engine == "GUARDIANN" {
		opts[recordlayer.IndexOptionVectorEngine] = "GUARDIANN"
		order = append(order, recordlayer.IndexOptionVectorEngine)
	}
	octx, ok := ctx.(*antlrgen.VectorIndexOptionsContext)
	if !ok || octx == nil {
		return opts, order, nil
	}
	seen := map[string]bool{}
	for _, o := range octx.AllVectorIndexOption() {
		oc, ok := o.(*antlrgen.VectorIndexOptionContext)
		if !ok {
			continue
		}
		name := strings.ToLower(oc.GetOptionName().GetText())
		spec, ok := vectorSQLOptions[name]
		key := spec.key
		if engine == "SPFRESH" {
			key = spec.spfresh
		}
		if !ok || key == "" {
			return nil, nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported vector index option '%s'", name)
		}
		if engine != "SPFRESH" && spec.engines != "*" && spec.engines != engine {
			return nil, nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"vector index option '%s' is not valid for the %s vector engine", name, engine)
		}
		if seen[name] {
			return nil, nil, api.NewErrorf(api.ErrCodeSyntaxError, "duplicate vector index option '%s'", name)
		}
		seen[name] = true
		order = append(order, key)
		vc, _ := oc.GetOptionValue().(*antlrgen.VectorIndexOptionValueContext)
		text := oc.GetOptionValue().GetText()
		bad := api.NewErrorf(api.ErrCodeSyntaxError, "invalid value '%s' for vector index option '%s'", text, name)
		switch spec.kind {
		case vectorOptInt:
			n, err := strconv.ParseInt(text, 10, 32)
			if err != nil {
				return nil, nil, bad
			}
			opts[key] = strconv.FormatInt(n, 10)
		case vectorOptDouble:
			f, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return nil, nil, bad
			}
			opts[key] = values.JavaDoubleToString(f)
		case vectorOptBool:
			opts[key] = strconv.FormatBool(strings.EqualFold(text, "true"))
		case vectorOptMetric:
			if vc == nil || vc.HnswMetric() == nil {
				return nil, nil, bad
			}
			metric, err := vectorMetricName(vc.HnswMetric())
			if err != nil {
				return nil, nil, api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate, "vector index %q", indexName)
			}
			opts[key] = metric
		}
	}
	return opts, order, nil
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
	indexName := functions.NormalizeIdentifier(def.GetIndexName().GetText())
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
	// The query resolves against the template being created, so a table it
	// names is qualified by that template's name, as Java's DdlVisitor plans
	// it against the catalog it is building (measured in
	// conformance/ws_f_table_qualifier_conformance_test.go: `FROM <tmpl>.w`
	// is accepted, `FROM S.w` refused).
	visitor := NewPlanVisitorWithTemplate(md, b.Name())
	op, err := visitor.VisitQueryTerm(qt)
	if err == nil {
		err = rejectArrayAggOrderBy(qt)
	}
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
	if err := runFromResolutionPostPasses(op, visitor.templateName, md, md); err != nil {
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
	if err := validateArrayAggCalls(node); err != nil {
		return err
	}
	if windowedAggregateInTree(node) {
		return api.NewError(api.ErrCodeUnsupportedQuery,
			"windowed aggregate (aggregate function with an OVER clause) is not supported")
	}
	return nil
}

// validateArrayAggCalls applies Java's visitAggregateWindowedFunction checks
// that precede argument resolution: aggregator, OVER, LIMIT.
func validateArrayAggCalls(node antlr.Tree) error {
	if node == nil {
		return nil
	}
	if awf, ok := node.(*antlrgen.AggregateWindowedFunctionContext); ok && awf.ARRAY_AGG() != nil {
		if awf.DISTINCT() != nil {
			return api.NewError(api.ErrCodeUnsupportedQuery, "aggregator DISTINCT is not supported")
		}
		if awf.OverClause() != nil {
			return api.NewError(api.ErrCodeUnsupportedQuery, "an OVER clause is not supported for ARRAY_AGG()")
		}
		if _, _, err := expr.ArrayAggOptions(awf); err != nil {
			return err
		}
	}
	for i := 0; i < node.GetChildCount(); i++ {
		if err := validateArrayAggCalls(node.GetChild(i)); err != nil {
			return err
		}
	}
	return nil
}

// rejectArrayAggOrderBy runs after the logical build, because Java resolves
// the arguments before refusing an in-call ORDER BY.
func rejectArrayAggOrderBy(node antlr.Tree) error {
	if node == nil {
		return nil
	}
	if awf, ok := node.(*antlrgen.AggregateWindowedFunctionContext); ok && awf.ARRAY_AGG() != nil && awf.OrderByClause() != nil {
		return api.NewError(api.ErrCodeUnsupportedQuery, "an ORDER BY clause is not supported for ARRAY_AGG()")
	}
	for i := 0; i < node.GetChildCount(); i++ {
		if err := rejectArrayAggOrderBy(node.GetChild(i)); err != nil {
			return err
		}
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
		colName := functions.NormalizeIdentifier(colDef.Uid().GetText())
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
				segments[i] = functions.NormalizeIdentifier(u.GetText())
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
		typeName := functions.NormalizeIdentifier(custom.GetText())
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
		// Run commits; a commit in its body is refused (RecordContextNotActiveError).
		return nil, c.sess.Catalog.Initialize(txn)
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
	// One attempt, as the target's relational layer runs a statement in its one
	// transaction: a conflicting DDL surfaces 40001 and a maybe-committed one its
	// 1021, instead of being re-executed.
	_, err := c.sess.DB.RunWithMaxAttempts(ctx, 1, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		txn := catalog.NewFDBTransaction(rctx)
		// Run commits; a commit in its body is refused (RecordContextNotActiveError).
		return nil, action.Execute(txn)
	})
	return err
}

// checkDDLDatabaseScope rejects a DDL statement whose resolved database path
// lies outside the connection's own database, when the connection has
// RESTRICT_DDL_TO_SESSION_DATABASE set. With the option unset (the default) it
// is a no-op and behaviour is exactly Java's.
//
// This is the single chokepoint for the check, and it takes the ALREADY
// RESOLVED database path — the value the ConstantAction will act on — rather
// than the statement text. parseSchemaIdentifier is a lexical split on the last
// "/", so `CREATE SCHEMA /FRL/other/S` and `DROP SCHEMA /FRL/other/S` reach the catalog
// with a database the connection never opened; DROP/CREATE DATABASE take theirs
// straight off the parse tree. Checking the resolved path covers all four
// without any string matching on SQL.
//
// Java has no equivalent: SemanticAnalyzer.parseSchemaURI splits the identifier
// and returns it without ever comparing against the connection. The only
// reserved-path guard on the Java side is DropDatabaseConstantAction's exact
// "/__SYS" check; CreateDatabaseConstantAction has no such guard at all. Java
// assumes authorization happens above the SQL engine. This option is for
// deployments where the connection itself is the trust boundary, which is why
// it is opt-in rather than the default.
//
// Scope is containment, not equality: a connection on /tenant-a may operate on
// /tenant-a and anything nested under it, the same predicate SHOW DATABASES
// uses for its prefix. Comparison is at path-segment granularity, so /tenant-a
// gets no authority over /tenant-abc.
func (c *EmbeddedConnection) checkDDLDatabaseScope(operation, dbPath string) error {
	if !optBool(c.Options(), api.OptRestrictDDLToSessionDatabase, false) {
		return nil
	}
	// No separate check for an unscopable session path: databaseInPrefix
	// rejects "" and the bare root "/" outright, so a session that scopes
	// nothing confers authority over nothing — not, as a raw prefix test would
	// have it, authority over every database on the cluster.
	if databaseInPrefix(dbPath, c.sess.DBPath) {
		return nil
	}
	return api.NewCrossDatabaseDDLError(operation, c.sess.DBPath, dbPath)
}

// checkSchemaTemplateDDLAllowed refuses CREATE / DROP SCHEMA TEMPLATE outright
// on a connection with RESTRICT_DDL_TO_SESSION_DATABASE set. With the option
// unset (the default) it is a no-op and behaviour is exactly Java's.
//
// Refusal rather than scoping, because there is nothing to scope against. A
// schema template is cluster-global in the catalog wire format — the Templates
// record has no owning database — so checkDDLDatabaseScope has no database to
// compare and the template namespace is shared by every tenant. That sharing is
// what makes template DDL a cross-tenant operation even though it names no
// database:
//
//   - DROP SCHEMA TEMPLATE removes ALL versions of the template. A schema
//     resolves its stored template version when it is loaded, so dropping a
//     template another tenant's schema was created from leaves that schema
//     unloadable.
//   - CREATE SCHEMA TEMPLATE can re-mint a name at a version another tenant's
//     schemas already reference, changing the metadata those schemas resolve to.
//
// Neither is expressible as "inside or outside my database", so a restricted
// connection gets no template DDL at all. A deployment that needs per-tenant
// templates namespaces the template NAME and performs template DDL on an
// unrestricted administrative connection.
func (c *EmbeddedConnection) checkSchemaTemplateDDLAllowed(operation string) error {
	if !optBool(c.Options(), api.OptRestrictDDLToSessionDatabase, false) {
		return nil
	}
	return api.NewSchemaTemplateDDLRestrictedError(operation, c.sess.DBPath)
}

// parseSchemaIdentifier splits "/dbpath/schemaname" into its parts.
// If the identifier has no leading slash, the current dbPath is used.
// Mirrors Java's SemanticAnalyzer.parseSchemaIdentifier.
func parseSchemaIdentifier(id, currentDB string) (dbPath, schemaName string, err error) {
	if strings.HasPrefix(id, "/") {
		// SemanticAnalyzer.parseSchemaURI validates the whole identifier.
		if err := validateDatabasePath(id); err != nil {
			return "", "", err
		}
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

// databaseURIPattern is SemanticAnalyzer.validateDatabaseUri's pattern
// (Java's \w is ASCII [A-Za-z0-9_]).
var databaseURIPattern = regexp.MustCompile(`^/[A-Za-z0-9_][-a-zA-Z0-9_/]*[A-Za-z0-9_]$`)

// validateDatabasePath is SemanticAnalyzer.validateDatabaseUri, which Java's
// DDL runs on every database path it parses (CREATE/DROP DATABASE, SHOW
// DATABASES WITH PREFIX, a schema identifier with a path). Whether the path
// names a database of a registered domain is decided later, where Java
// resolves it (keyspace.ToDatabasePath).
func validateDatabasePath(p string) error {
	if !databaseURIPattern.MatchString(p) {
		return api.NewErrorf(api.ErrCodeInvalidPath, "invalid database path '%s'", p)
	}
	return nil
}

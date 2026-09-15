package embedded

// Catalog-aware logical-builder seam.
//
// logical_builder.go ports parse trees into LogicalOperator trees with
// WHERE clauses carried as canonical source text — adequate for the
// pre-cascades Explain output but blind to identifier resolution and
// type information.
//
// This file is the catalog-aware variant: when a *recordlayer.RecordMetaData
// is in scope, WHERE clauses walk through expr.WalkPredicate (via
// rlcatalog → semantic.Analyzer + Scope) and produce a real
// predicates.QueryPredicate tree on LogicalFilter.Predicate alongside
// the source text. Best-effort throughout — any walker error,
// catalog miss, ambiguous column ref, or shape outside the walker's
// support degrades to text fallback rather than failing the build.
//
// Wiring map (catalog-aware → text fallback):
//
//   buildLogicalPlanForSelectWithCatalog → buildLogicalPlanForSelect
//   buildLogicalPlanForDeleteWithCatalog → buildLogicalPlanForDelete
//   buildLogicalPlanForUpdateWithCatalog → buildLogicalPlanForUpdate
//   buildLogicalPlanForInsertWithCatalog → buildLogicalPlanForInsert
//   buildLogicalPlanForQueryWithCatalog (CTE/UNION/SELECT recursion)
//
// Predicate-extraction helpers:
//
//   buildWherePredicate          (selectQuery shape, dispatches)
//   buildWherePredicateForTable  (single source — primary table)
//   buildWherePredicateForJoins  (multi source — JOIN chain)
//
// Plumbed into the connection's Explain path via
// EmbeddedConnection.cachedMetaData() — when the session schema cache
// already holds the active schema, Explain upgrades to predicate-tree
// rendering; cold cache stays on the text-builder path so EXPLAIN
// remains deterministic and IO-free.

import (
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/antlr4-go/antlr/v4"

	recordlayer "fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/protoname"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
	"fdb.dev/pkg/relational/core/query/semantic/rlcatalog"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// CorrelatedExistsError is returned when buildCorrelatedExists fails.
// Detected via errors.As at the caller to propagate as
// ErrCodeUndefinedColumn for fallback to a richer outer scope.
//
// Unsupported distinguishes a DELIBERATE decline of an unsupported correlated-
// EXISTS shape (an intentional CORRECT-or-CONSERVATIVE rejection — surfaced as
// 0A000 unsupported-operation) from a resolution failure that should read as an
// undefined column (42703). The WHERE-EXISTS and projected paths both key on
// this so a decline reports the same 0A000 in either position.
type CorrelatedExistsError struct {
	Message     string
	Cause       error
	Unsupported bool
	// NotCorrelated marks the arms that establish the subquery is not a
	// correlated one at all, as opposed to being a correlated one this engine
	// cannot yet plan. The correlated builder is entered SPECULATIVELY — an
	// undefined column inside a subquery MIGHT be an outer reference — so a
	// failure that disproves the speculation must not replace the diagnosis
	// that prompted it. See BuildScalar.
	NotCorrelated bool
}

func (e *CorrelatedExistsError) Error() string { return e.Message }
func (e *CorrelatedExistsError) Unwrap() error { return e.Cause }

// wrapCorrelatedExistsWalkErr wraps a predicate-walk failure in a
// CorrelatedExistsError, PROPAGATING the Unsupported flag when the wrapped error
// is itself an Unsupported decline (e.g. a NESTED correlated EXISTS whose JOIN ON
// hit the RIGHT/FULL / nested-subquery decline). Without this, the outer wrapper
// defaults Unsupported=false, so mapPredicateWalkError matches it first and
// reports 42703 (undefined-column) instead of the intended 0A000
// (unsupported-operation) for the deliberate decline.
func wrapCorrelatedExistsWalkErr(msg string, err error) *CorrelatedExistsError {
	unsupported := false
	var inner *CorrelatedExistsError
	if errors.As(err, &inner) {
		unsupported = inner.Unsupported
	}
	return &CorrelatedExistsError{Message: msg, Cause: err, Unsupported: unsupported}
}

// buildWherePredicateForTable converts a WHERE expression context
// into a predicates.QueryPredicate using the expr walker, with a
// single-source scope over the named table. Returns (nil, false) on
// any shape the walker can't handle, on a catalog lookup miss, or
// when metadata is nil.
//
// The (pred, true) branch is what callers attach to a LogicalFilter;
// the (nil, false) branch is the signal to fall back to the
// canonical source text. Error discrimination is intentionally
// coarse — unsupported shape, catalog miss, nil metadata all land
// in the same (nil, false) bucket — because every error at this
// boundary has the same handling: use text.
//
// tableAlias may be empty; the table's own name fills in.
func buildWherePredicateForTable(
	md *recordlayer.RecordMetaData,
	tableName, tableAlias string,
	whereExpr antlrgen.IWhereExprContext,
) (predicates.QueryPredicate, bool) {
	pred, ok, _ := buildWherePredicateForTableE(md, tableName, tableAlias, whereExpr)
	return pred, ok
}

// buildWherePredicateForTableE is buildWherePredicateForTable that also carries
// a structured *api.Error from the predicate walk (e.g. DATATYPE_MISMATCH from a
// bare non-boolean WHERE, RFC-146). The DML (DELETE/UPDATE) paths use it so
// `DELETE FROM t WHERE <non-boolean>` surfaces 42804 — the same SQLSTATE the
// SELECT/ON paths give — instead of swallowing it into a generic DML translation
// error. A non-api walk failure (an unhandled shape that should soft-fall-back
// to the text builder) returns a nil error, preserving existing behaviour.
func buildWherePredicateForTableE(
	md *recordlayer.RecordMetaData,
	tableName, tableAlias string,
	whereExpr antlrgen.IWhereExprContext,
) (predicates.QueryPredicate, bool, error) {
	if md == nil || tableName == "" || whereExpr == nil || whereExpr.Expression() == nil {
		return nil, false, nil
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	// Split on '.' so a schema-qualified table name like "schema.t"
	// reaches FromSegments as ["schema", "t"] rather than as a single
	// dotted segment that would never resolve in the catalog.
	tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
	if err != nil {
		return nil, false, nil
	}
	alias := semantic.FromNormalized(tableAlias)
	if tableAlias == "" {
		alias = semantic.FromNormalized(tableName)
	}
	scope := semantic.NewScope(nil)
	if err := scope.AddSource(semantic.ScopeSource{
		Table:           tbl,
		Alias:           alias,
		CorrelationName: alias.Name(),
	}); err != nil {
		return nil, false, nil
	}
	resolver := expr.New(analyzer, scope)
	pred, err := resolver.WalkPredicate(whereExpr.Expression())
	if err != nil {
		// Classify with the shared mapper (the same the SELECT WHERE / JOIN-ON paths
		// use) so a bare semantic failure — undefined column (ColumnNotFoundError →
		// 42703), ambiguous column, bad source — surfaces the specific SQLSTATE the
		// SELECT path gives, plus structured *api.Error (e.g. 42804 DATATYPE_MISMATCH).
		// The DML caller returns it instead of swallowing it into a generic 0AF00 "DML
		// Cascades translation failed". An unclassifiable shape failure (mapped == nil)
		// still soft-falls-back, preserving existing behaviour.
		if mapped := mapPredicateWalkError(err); mapped != nil {
			return nil, false, mapped
		}
		return nil, false, nil
	}
	// Plan-time fold of constant Value sub-trees inside the predicate
	// (`name = 1+2` → `name = 3`). Best-effort — SimplifyPredicateValues
	// is pointer-stable when nothing folds.
	pred = predicates.SimplifyPredicateValues(pred)
	return pred, true, nil
}

// buildWherePredicate is the selectQuery-shaped adapter over the
// walker. Single-table FROM uses buildWherePredicateForTable;
// JOIN-shape FROM (sq.joins non-empty) builds a multi-source scope
// — one ScopeSource per primary + JOIN. Derived-table FROM routes
// through buildWherePredicateForDerived which synthesises a virtual
// ScopeSource from the inner query's projection schema (basic
// shapes only — see buildDerivedTableSource).
func buildWherePredicate(
	md *recordlayer.RecordMetaData,
	schemaName string,
	sq *selectQuery,
	whereExpr antlrgen.IWhereExprContext,
) (predicates.QueryPredicate, bool) {
	if sq == nil {
		return nil, false
	}
	if selectHasInlineValuesSource(sq) {
		resolver := buildSelectScope(sq, md, schemaName, nil)
		if resolver == nil || whereExpr == nil || whereExpr.Expression() == nil {
			return nil, false
		}
		pred, err := resolver.WalkPredicate(whereExpr.Expression())
		if err != nil {
			return nil, false
		}
		return predicates.SimplifyPredicateValues(pred), true
	}
	if sq.derivedQuery != nil {
		return buildWherePredicateForDerived(md, sq, whereExpr)
	}
	if len(sq.joins) == 0 {
		return buildWherePredicateForTable(md, sq.tableName, sq.tableAlias, whereExpr)
	}
	return buildWherePredicateForJoins(md, schemaName, sq, whereExpr)
}

// buildWherePredicateForDerived handles `FROM (SELECT ...) AS alias`.
// Synthesises a virtual ScopeSource from the inner query's projection
// schema (via buildDerivedTableSource — basic shapes only) and then
// walks the WHERE under that scope.
//
// Anything richer than `(SELECT col1, col2 FROM realtable) AS alias`
// — joins, derived-of-derived, SELECT *, aggregates, computed
// projections — declines and the caller falls back to the text
// builder. Phase 4.0 Type hierarchy port unlocks computed
// projections (the seed has no way to infer the projected
// expression's result type).
func buildWherePredicateForDerived(
	md *recordlayer.RecordMetaData,
	sq *selectQuery,
	whereExpr antlrgen.IWhereExprContext,
) (predicates.QueryPredicate, bool) {
	if md == nil || sq == nil || sq.tableName == "" || sq.derivedQuery == nil ||
		whereExpr == nil || whereExpr.Expression() == nil {
		return nil, false
	}
	src, sourceErr := boundDerivedSource(md, sq.tableName, sq.bindingID, sq.derivedQuery, sq.catalogAwareInnerPlan, sq.enclosingScope, defaultEmbeddedSchema, nil)
	if sourceErr != nil {
		return nil, false
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	scope := semantic.NewScope(sq.enclosingScope)
	if err := scope.AddSource(src); err != nil {
		return nil, false
	}
	resolver := expr.New(analyzer, scope)
	pred, err := resolver.WalkPredicate(whereExpr.Expression())
	if err != nil {
		return nil, false
	}
	pred = predicates.SimplifyPredicateValues(pred)
	return pred, true
}

// renameCarriedColumn re-labels a resolved source column as one output column
// of a derived table / CTE, carrying EVERYTHING ELSE across.
//
// The whole point is what it does NOT drop. A virtual column minted as
// `semantic.Column{Id, Type, Nullable}` loses StructFields and IsArray, and a
// STRUCT column then types UNKNOWN (structColumnType returns UNKNOWN on an
// empty field list) instead of RECORD. UNKNOWN is deliberately ADMITTED by the
// comparison operand gate — bound parameters need that carve-out — so a
// whole-struct comparison that rejects 0AF00 on the base table planned
// straight through a derived table and returned SILENT WRONG ROWS. Java never
// has this hole: a derived table's columns are the body's Values and carry
// their real Type, so the operand check (RelOpValue.isSupportedOperandType,
// RelOpValue.java:320-322) sees the record either way.
//
// Ephemeral is CLEARED rather than carried: an explicitly projected item is a
// visible output of the derived table, whatever its source column's star
// visibility was.
func renameCarriedColumn(src semantic.Column, outName string) semantic.Column {
	src.Id = semantic.FromNormalized(outName)
	src.Ephemeral = false
	return src
}

// lookupSourceColumn finds the column a derived-table projection item reads,
// trying the structured bare segment first and the full spelling second (a
// rebased or computed item has no bare segment).
func lookupSourceColumn(cols []semantic.Column, bare, full string) (semantic.Column, bool) {
	for _, name := range [2]string{bare, full} {
		if name == "" {
			continue
		}
		id := semantic.FromNormalized(name)
		for _, c := range cols {
			if c.Id.Name() == id.Name() {
				return c, true
			}
		}
	}
	return semantic.Column{}, false
}

// exactVirtualScopeSource projects the exact result row of a logical body into
// the semantic scope representation used while resolving an enclosing query.
// This is the bridge for computed derived-table and CTE columns: their type is
// owned by the resolved Value in the body, not by any catalog column that a
// parse-tree-only derivation could copy.
//
// preferredNames is the body's SQL output-name authority. ExactLogicalResultType
// also carries names, but it deliberately normalizes logical projection labels;
// retaining the parsed names here preserves quoted output aliases at the
// semantic boundary. A width disagreement declines instead of pairing a type
// with the wrong output slot.
func exactVirtualScopeSource(
	alias string,
	op logical.LogicalOperator,
	md *recordlayer.RecordMetaData,
	preferredNames []string,
	cteScopes map[string]semantic.ScopeSource,
) (semantic.ScopeSource, bool) {
	if alias == "" || op == nil || md == nil {
		return semantic.ScopeSource{}, false
	}
	typ, err := query.ExactLogicalResultTypeWithCTEs(op, md, cteRowTypes(cteScopes))
	if err != nil {
		return semantic.ScopeSource{}, false
	}
	record, ok := typ.(*values.RecordType)
	if !ok {
		return semantic.ScopeSource{}, false
	}
	if len(preferredNames) > 0 && len(preferredNames) != len(record.Fields) {
		return semantic.ScopeSource{}, false
	}
	// With no parsed projection to take names from — `SELECT *` has none — the
	// labels come from the derivation's own label authority, NOT from the exact
	// row's field names. A join row qualifies every leg column with its source
	// alias so the executor's row map can tell A.K from B.K; those are datum
	// keys. Publishing them here made `WITH d AS (SELECT * FROM a, b) SELECT
	// d.aid FROM d` fail with 42703, because the scope had registered a column
	// named A.AID under source D.
	//
	// A width disagreement declines rather than pairing a label with the wrong
	// slot, the same rule preferredNames follows.
	labels := preferredNames
	if len(labels) == 0 {
		derived, labelErr := query.ExactLogicalOutputLabels(op, md, cteRowTypes(cteScopes))
		if labelErr != nil || len(derived) != len(record.Fields) {
			return semantic.ScopeSource{}, false
		}
		labels = derived
	}
	columns := make([]semantic.Column, len(record.Fields))
	// The FLOWED layout is the exact row itself, field names included: the
	// names the plan flows are the names this source's quantified object must
	// state, or the scope and the plan disagree about one row. A projection
	// over a join names a repeated bare leaf by its qualified datum key
	// (GA.G beside G) and a record constructor names a repeated output by the
	// name-addressability suffix (K, K_2); the exact derivation of a
	// PROJECTION or an AGGREGATE mirrors the record constructor the body
	// flows, and only there is it stated as the layout. A projection-less
	// join's exact type names its fields by the leg-qualified datum keys of
	// the retired row map (A.AID), which is not the row the executor's
	// positional merge flows, so such a body keeps the SQL labels as its only
	// row — the behaviour every read of it had before — rather than a layout
	// no binding declares. Publishing the SQL labels
	// as the layout instead made every read bound to the quantified object —
	// a WHERE, a sort key, an aggregate key or operand over a unique column of
	// such a body — refused at execution as `edge lookup U: read as
	// RECORD(G,G,W), declared RECORD(GA.G,G,W)` or as an undeclared binding,
	// where the projection the translator inlines answered. The SQL labels
	// stay the columns exposed for resolution, so a read that spells a
	// repeated name is ambiguous (42702) and a unique name resolves to its
	// position; a column whose label and flowed name differ is, by that same
	// rule, never resolved by name.
	var flowed []semantic.Column
	if bodyFlowsARecordConstructor(op) {
		flowed = make([]semantic.Column, len(record.Fields))
	}
	for i, field := range record.Fields {
		name := field.Name
		if len(labels) > 0 {
			name = labels[i]
		}
		column, exact := semanticColumnFromExactType(name, field.FieldType)
		if !exact {
			return semantic.ScopeSource{}, false
		}
		columns[i] = column
		if flowed != nil {
			flowed[i] = column
			flowed[i].Id = semantic.FromNormalized(field.Name)
		}
	}
	aliasID := semantic.FromNormalized(alias)
	source := semantic.ScopeSource{
		Table: &semantic.StaticTable{
			TableName:    semantic.FromSegments([]string{alias}, false),
			TableColumns: columns,
		},
		Alias:           aliasID,
		CorrelationName: aliasID.Name(),
	}
	if flowed != nil {
		source.FlowedColumns = flowed
		source.FlowedNullable = record.Nullable
	}
	return source, true
}

// bodyFlowsARecordConstructor reports whether a body's row is a record
// constructor's — a projection or an aggregate at the top, under the
// row-preserving wrappers (filter, sort, limit, distinct) — so that the exact
// type's field names are the names the executor emits and may be stated as the
// source's flowed layout. A join, a scan or a set operation at the top flows a
// row the exact type does not name the same way.
func bodyFlowsARecordConstructor(op logical.LogicalOperator) bool {
	for {
		switch typed := op.(type) {
		case *logical.LogicalFilter:
			op = typed.Input
		case *logical.LogicalSort:
			op = typed.Input
		case *logical.LogicalLimit:
			op = typed.Input
		case *logical.LogicalDistinct:
			op = typed.Input
		case *logical.LogicalUnion:
			// A union flows its FIRST leg's row: the translator aligns every
			// other leg onto it (RFC-242), so the first leg's constructor is
			// the row the union emits.
			if len(typed.Inputs) == 0 {
				return false
			}
			op = typed.Inputs[0]
		case *logical.LogicalProject, *logical.LogicalAggregate:
			return true
		default:
			return false
		}
	}
}

// semanticColumnFromExactType is the lossless subset of the rich cascades type
// hierarchy representable by semantic.Column. Unsupported shapes decline;
// critically, they never become UNKNOWN. The strings below are the inverse of
// expr.columnCascadesType's admitted spellings, so a later expression walk
// reconstructs the same code, nullability, and record/array shape.
func semanticColumnFromExactType(name string, typ values.Type) (semantic.Column, bool) {
	// Validate the complete graph once before inspecting methods or recursing:
	// malformed transported graphs (including typed nils and cycles) decline.
	if _, err := values.SnapshotExactType(typ); err != nil {
		return semantic.Column{}, false
	}
	return semanticColumnFromValidatedExactType(name, typ)
}

func semanticColumnFromValidatedExactType(name string, typ values.Type) (semantic.Column, bool) {
	column := semantic.Column{Id: semantic.FromNormalized(name), Nullable: typ.IsNullable()}
	switch typed := typ.(type) {
	case *values.PrimitiveType:
		switch typed.Code() {
		case values.TypeCodeBoolean:
			column.Type = "BOOL"
		case values.TypeCodeInt:
			column.Type = "INT"
		case values.TypeCodeLong:
			column.Type = "BIGINT"
		case values.TypeCodeFloat:
			column.Type = "FLOAT"
		case values.TypeCodeDouble:
			column.Type = "DOUBLE"
		case values.TypeCodeString:
			column.Type = "STRING"
		case values.TypeCodeBytes:
			column.Type = "BYTES"
		case values.TypeCodeVersion:
			column.Type = "VERSION"
		case values.TypeCodeUuid:
			column.Type = "UUID"
		default:
			// NULL/placeholders and primitive codes not understood by the
			// semantic expression bridge have no lossless representation.
			return semantic.Column{}, false
		}
		return column, true
	case *values.EnumType:
		if typed.EnumName == "" || len(typed.Values) == 0 {
			return semantic.Column{}, false
		}
		column.Type = "ENUM"
		column.EnumTypeName = typed.EnumName
		column.EnumMembers = make([]semantic.EnumMember, len(typed.Values))
		for i, member := range typed.Values {
			if member.Name == "" {
				return semantic.Column{}, false
			}
			column.EnumMembers[i] = semantic.EnumMember{Name: member.Name, Number: member.Number}
		}
		return column, true
	case *values.RecordType:
		if len(typed.Fields) == 0 {
			// expr.structColumnType deliberately treats a RECORD with no
			// StructFields as unresolved, so a fieldless record has no
			// representation that survives the round trip.
			return semantic.Column{}, false
		}
		// Type is the SQL kind, literally "RECORD", which is what takes the
		// expr.structColumnType bridge; the record's NAME travels in
		// StructTypeName, the carrier that bridge reads — a declared STRUCT
		// column's descriptor name, or nothing for an anonymous record, which
		// the bridge then rebuilds anonymous — so the round trip mints the same
		// values.RecordType under the same name. The kind is never a name. Declining every
		// nominal record here left a projected STRUCT-typed nested field
		// (`SELECT x.child.v FROM (SELECT t.p.child FROM t) x`, child a
		// declared STRUCT) with no exact row to publish: the whole derived
		// source declined, and the read was refused as a projection slot
		// with no resolved Value.
		column.Type = "RECORD"
		column.StructTypeName = typed.RecordName
		column.StructFields = make([]semantic.Column, len(typed.Fields))
		seen := make(map[string]struct{}, len(typed.Fields))
		for i, field := range typed.Fields {
			if field.Name != "" {
				if _, duplicate := seen[field.Name]; duplicate {
					// The semantic-to-cascades bridge declines duplicate nested
					// field names, so this direction must decline them too.
					return semantic.Column{}, false
				}
				seen[field.Name] = struct{}{}
			}
			child, exact := semanticColumnFromValidatedExactType(field.Name, field.FieldType)
			if !exact {
				return semantic.Column{}, false
			}
			column.StructFields[i] = child
		}
		return column, true
	case *values.ArrayType:
		if typed.ElementType == nil || typed.ElementType.IsNullable() {
			// semantic.Column carries the array container's nullability but has
			// no independent nullable-element bit. Its reverse bridge always
			// forces array elements NOT NULL, so accepting a nullable element
			// here would silently change the exact type.
			return semantic.Column{}, false
		}
		element, exact := semanticColumnFromValidatedExactType(name, typed.ElementType)
		if !exact || element.IsArray {
			// semantic.Column has one IsArray bit and cannot represent a
			// nested array without erasing a dimension.
			return semantic.Column{}, false
		}
		element.Id = semantic.FromNormalized(name)
		element.IsArray = true
		element.Nullable = typed.Nullable
		return element, true
	default:
		// Relation/erased-record types need richer semantic carriers.
		return semantic.Column{}, false
	}
}

func projectionOutputNames(sq *selectQuery) []string {
	if sq == nil || sq.projCols == nil {
		return nil
	}
	// A `<qualifier>.*` item is ONE sentinel slot here and EVERY column of that
	// source in the built body, so a name list drawn from these slots has the
	// wrong width for a mixed star (`SELECT a.*, b.y`) and the exact derivation
	// declines it. The body's own labels are the authority for such a list.
	for _, qualifier := range sq.projStarQualifiers {
		if qualifier != "" {
			return nil
		}
	}
	names := make([]string, len(sq.projCols))
	for i, column := range sq.projCols {
		name := column.bare
		if name == "" && column.bound == nil && (i >= len(sq.projExprs) || sq.projExprs[i] == nil) {
			name = column.name
		}
		if i < len(sq.projAliases) && sq.projAliases[i] != "" {
			name = sq.projAliases[i]
		}
		names[i] = name
	}
	return names
}

func buildExactVirtualScopeSourceForBody(
	md *recordlayer.RecordMetaData,
	alias string,
	body antlrgen.IQueryExpressionBodyContext,
	cteScopes map[string]semantic.ScopeSource,
	preferredNames []string,
) (semantic.ScopeSource, bool) {
	if body == nil {
		return semantic.ScopeSource{}, false
	}
	op, err := buildLogicalPlanForQueryBodyWithCTECatalog(
		body, md, defaultEmbeddedSchema, cteScopes, nil,
	)
	if err != nil || op == nil {
		return semantic.ScopeSource{}, false
	}
	src, ok := exactVirtualScopeSource(alias, op, md, preferredNames, cteScopes)
	return src, ok
}

// cteRowTypes lifts the enclosing WITH bindings out of the semantic scope into
// the row types the exact logical derivation needs. A body reading one of those
// names builds to a bare LogicalScan whose "table" has no catalog descriptor,
// so without this the derivation reports `scan table "C" has no record
// descriptor` and the whole derived source declines.
func cteRowTypes(cteScopes map[string]semantic.ScopeSource) map[string]values.Type {
	if len(cteScopes) == 0 {
		return nil
	}
	rows := make(map[string]values.Type, len(cteScopes))
	for name, src := range cteScopes {
		if row := expr.SourceRowType(src); row != nil {
			rows[name] = row
		}
	}
	return rows
}

func buildExactVirtualScopeSourceForSelect(
	md *recordlayer.RecordMetaData,
	alias string,
	sq *selectQuery,
	cteScopes map[string]semantic.ScopeSource,
	preferredNames []string,
) (semantic.ScopeSource, bool) {
	src, ok, _ := buildExactScopeSourceOrBodyError(md, alias, sq, cteScopes, preferredNames)
	return src, ok
}

// buildExactScopeSourceOrBodyError is buildExactVirtualScopeSourceForSelect
// with the two ways it can fail kept apart.
//
// A body that BUILDS but whose row semantic.Column cannot carry losslessly is a
// DECLINE: nothing is wrong with the query, this derivation just has nothing to
// publish, and the caller has a fallback.
//
// A body that does not BUILD is a user error about the body itself — an
// ambiguous or unknown column inside the CTE. Swallowing that into a decline
// replaces the body's own 42702/42703 with the reader's generic "cannot resolve
// the join's sources" 0AF00: a worse message for a real mistake, and one that
// names the wrong query.
func buildExactScopeSourceOrBodyError(
	md *recordlayer.RecordMetaData,
	alias string,
	sq *selectQuery,
	cteScopes map[string]semantic.ScopeSource,
	preferredNames []string,
) (semantic.ScopeSource, bool, error) {
	if sq == nil {
		return semantic.ScopeSource{}, false, nil
	}
	op, err := buildLogicalPlanForSelectWithCTECatalog(
		sq, md, defaultEmbeddedSchema, cteScopes, nil,
	)
	if err != nil {
		return semantic.ScopeSource{}, false, err
	}
	if op == nil {
		return semantic.ScopeSource{}, false, nil
	}
	src, ok := exactVirtualScopeSource(alias, op, md, preferredNames, cteScopes)
	return src, ok, nil
}

// buildDerivedTableSource synthesises a virtual ScopeSource for
// `FROM (SELECT col1, col2 FROM realtable) AS alias`. Walks the inner
// query's parse tree via extractFromQueryTerm, then builds a
// semantic.StaticTable whose columns inherit the inner-table column
// types. Anything outside the basic shape — derived-of-derived,
// joins, SELECT *, aggregates, computed projections, qualified-star
// projections — declines with (zero, false).
//
// alias is the outer FROM clause's alias for the derived table; the
// virtual table's name + visibility are bound to that alias.
func buildDerivedTableSource(
	md *recordlayer.RecordMetaData,
	alias string,
	inner antlrgen.IQueryContext,
) (semantic.ScopeSource, bool) {
	return buildDerivedTableSourceWithCTEs(md, alias, inner, nil)
}

// buildDerivedTableSourceWithCTEs is buildDerivedTableSource with the enclosing
// WITH bindings in hand, so a derived body that READS a CTE (`FROM (SELECT *
// FROM c) c` under `WITH c AS (...)`) can be typed at all. Without them the body
// resolves against the CATALOG only, `c` is not a table, the whole source
// declines, and the outer projection over it comes back with no resolved Value.
func buildDerivedTableSourceWithCTEs(
	md *recordlayer.RecordMetaData,
	alias string,
	inner antlrgen.IQueryContext,
	cteScopes map[string]semantic.ScopeSource,
) (semantic.ScopeSource, bool) {
	source, err := buildDerivedTableSourceWithCTEsChecked(md, alias, inner, defaultEmbeddedSchema, cteScopes)
	return source, err == nil
}

// boundDerivedSource applies lexical qualification to a fully upgraded body.
// The early catalog-aware carrier can predate projection-value/name publication;
// deriving its schema would expose physical X.K/Y.K keys instead of the body's
// duplicate SQL labels K/K. Rebuild through the full visitor with the SAME
// parent scope and CTE bindings, so this is not the old rootless reconstruction
// that lost outer references.
func boundDerivedSource(md *recordlayer.RecordMetaData, alias, binding string, inner antlrgen.IQueryContext, body logical.LogicalOperator, parent *semantic.Scope, schemaName string, cteScopes map[string]semantic.ScopeSource) (semantic.ScopeSource, error) {
	if inner != nil {
		visitor := NewPlanVisitor(md)
		visitor.schemaName = schemaName
		visitor.enclosingScope = parent
		visitor.cteScopes = maps.Clone(cteScopes)
		var err error
		body, err = visitor.VisitQuery(inner)
		if err != nil {
			return semantic.ScopeSource{}, err
		}
	}
	source, exact := exactVirtualScopeSource(alias, body, md, nil, cteScopes)
	if !exact && inner != nil {
		// A UNION can have an exact SQL output even when its branches require
		// numeric promotion, so the ordinary exact-type equality check declines.
		// The checked parse-tree path folds those branch types with MaximumType;
		// retain it as the fallback rather than dropping the derived source.
		var err error
		source, err = buildDerivedTableSourceWithCTEsChecked(md, alias, inner, schemaName, cteScopes)
		if err != nil {
			return semantic.ScopeSource{}, err
		}
		exact = true
	}
	if !exact {
		return semantic.ScopeSource{}, api.NewErrorf(api.ErrCodeUnsupportedQuery, "derived source %q has no representable exact semantic schema", alias)
	}
	aliasID := semantic.FromNormalized(alias)
	return cteSourceAs(source, aliasID, bindingOrAlias(binding, aliasID)), nil
}

// buildDerivedTableSourceWithCTEsChecked preserves body errors separately from
// an exact row that the semantic column representation cannot carry.
func buildDerivedTableSourceWithCTEsChecked(
	md *recordlayer.RecordMetaData,
	alias string,
	inner antlrgen.IQueryContext,
	schemaName string,
	cteScopes map[string]semantic.ScopeSource,
) (semantic.ScopeSource, error) {
	if md == nil || alias == "" || inner == nil {
		return semantic.ScopeSource{}, api.NewError(api.ErrCodeUnsupportedQuery, "derived source has no exact semantic schema")
	}
	op, err := buildLogicalPlanForQueryWithCTECatalog(inner, md, schemaName, cteScopes, nil)
	if err != nil {
		return semantic.ScopeSource{}, err
	}
	// Building validates the body; it does not replace SQL publication rules.
	// Bare stars hide ephemeral/shadowed columns and explicit projections keep
	// duplicate SQL labels distinct from their deduplicated physical names.
	switch body := inner.QueryExpressionBody().(type) {
	case *antlrgen.QueryTermDefaultContext:
		// The built logical body is the output-name authority. In particular,
		// ExactLogicalOutputLabels preserves repeated SQL labels while the
		// physical record type deduplicates its field names. The parse-only term
		// derivation cannot represent that split and turned a duplicate K/K into
		// K/K_2, so a qualified reference reported 42703 instead of 42702.
		if exact, ok := exactVirtualScopeSource(alias, op, md, nil, cteScopes); ok {
			return exact, nil
		}
		if source, ok := buildDerivedTableSourceFromTerm(md, alias, body, schemaName, cteScopes); ok {
			return source, nil
		}
	case *antlrgen.SetQueryContext:
		if source, ok := buildDerivedTableSourceFromUnion(md, alias, body, schemaName, cteScopes); ok {
			columns := source.Table.Columns()
			names := make([]string, len(columns))
			for i, column := range columns {
				names[i] = column.Id.Name()
			}
			if exact, ok := exactVirtualScopeSource(alias, op, md, names, cteScopes); ok {
				return exact, nil
			}
			return source, nil
		}
	}
	source, exact := exactVirtualScopeSource(alias, op, md, nil, cteScopes)
	if !exact {
		return semantic.ScopeSource{}, api.NewErrorf(api.ErrCodeUnsupportedQuery, "derived source %q has no representable exact semantic schema", alias)
	}
	return source, nil
}

// buildDerivedTableSourceFromUnion types a UNION-ALL-bodied derived table so a
// reference to its columns carries a real type instead of falling to the
// untyped text path. SQL exposes the FIRST branch's output names; each
// column's type is the fold of the branch types at that position under Java's
// Type.maximumType (SemanticAnalyzer resolves the union row type exactly so,
// SemanticAnalyzer.java:802-818, over PromoteValue's numeric promotion
// lattice INT→LONG→FLOAT→DOUBLE, PromoteValue.java:76-81; equal TypeCodes
// keep the type, nullability ORs).
//
// The width is not cosmetic: SUM over a union of INTEGER branches must keep
// the INT TypeCode so the SUM_I int32-overflow lane fires exactly as it does
// over the base table — an untyped union column silently rode the int64 SUM_L
// lane where Java raises "integer overflow".
//
// A pair with no defined maximum degrades that COLUMN to UNKNOWN rather than
// declining the whole source: Java rejects such a union outright
// (UNION_INCOMPATIBLE_COLUMNS), and that rejection belongs to the union
// type-checking path, not to this scope derivation — an UNKNOWN column keeps
// today's lazy-loud behavior. UNION DISTINCT declines exactly as the logical
// builder does (buildLogicalPlanForUnion requires ALL).
func buildDerivedTableSourceFromUnion(
	md *recordlayer.RecordMetaData,
	alias string,
	setQ *antlrgen.SetQueryContext,
	schemaName string,
	cteScopes map[string]semantic.ScopeSource,
) (semantic.ScopeSource, bool) {
	if setQ.ALL() == nil {
		return semantic.ScopeSource{}, false
	}
	branches, ok := collectUnionBranchTerms(setQ)
	if !ok || len(branches) == 0 {
		return semantic.ScopeSource{}, false
	}
	var cols []semantic.Column
	for i, br := range branches {
		src, brOK := buildDerivedTableSourceFromTerm(md, alias, br, schemaName, cteScopes)
		if !brOK || src.Table == nil {
			return semantic.ScopeSource{}, false
		}
		bc := src.Table.Columns()
		if i == 0 {
			cols = append([]semantic.Column(nil), bc...)
			continue
		}
		if len(bc) != len(cols) {
			return semantic.ScopeSource{}, false
		}
		for j := range cols {
			cols[j] = unionMaximumColumn(cols[j], bc[j])
		}
	}
	aliasID := semantic.FromNormalized(alias)
	return semantic.ScopeSource{
		Table: &semantic.StaticTable{
			TableName:    semantic.FromSegments([]string{alias}, false),
			TableColumns: cols,
		},
		Alias:           aliasID,
		CorrelationName: aliasID.Name(),
	}, true
}

// collectUnionBranchTerms flattens a SetQuery's branch terms left-to-right,
// mirroring buildLogicalPlanForUnion's association (the grammar nests
// SetQuery(SetQuery(A, B), C) for A UNION B UNION C). Any non-ALL level or
// non-term branch declines.
func collectUnionBranchTerms(setQ *antlrgen.SetQueryContext) ([]*antlrgen.QueryTermDefaultContext, bool) {
	var out []*antlrgen.QueryTermDefaultContext
	switch l := setQ.GetLeft().(type) {
	case *antlrgen.QueryTermDefaultContext:
		out = append(out, l)
	case *antlrgen.SetQueryContext:
		if l.ALL() == nil {
			return nil, false
		}
		inner, ok := collectUnionBranchTerms(l)
		if !ok {
			return nil, false
		}
		out = inner
	default:
		return nil, false
	}
	r, ok := setQ.GetRight().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return nil, false
	}
	return append(out, r), true
}

// unionMaximumColumn folds one later-branch column into the accumulated union
// output column: the FIRST branch names the output; the type is Java's
// Type.maximumType over the two SQL types; nullability ORs (Type.java:608
// isResultNullable). An unfoldable pair degrades to UNKNOWN — see
// buildDerivedTableSourceFromUnion.
func unionMaximumColumn(acc, br semantic.Column) semantic.Column {
	t, ok := sqlMaximumType(baseSQLType(acc.Type), baseSQLType(br.Type))
	if !ok {
		t = "UNKNOWN"
	}
	return semantic.Column{Id: acc.Id, Type: t, Nullable: acc.Nullable || br.Nullable}
}

// baseSQLType strips the catalog's embedded " NOT NULL" suffix — the folded
// column's nullability is carried by Column.Nullable alone
// (columnCascadesType re-applies it to the cascades type).
func baseSQLType(t string) string {
	return strings.TrimSuffix(t, " NOT NULL")
}

// sqlMaximumType is Java Type.maximumType restricted to the primitive SQL
// type strings this catalog carries: an equal pair keeps its type
// (Type.java:621-623); a numeric pair takes the promotion-lattice maximum
// (INT→LONG→FLOAT→DOUBLE, PromoteValue.java:76-81); any other pair has no
// defined maximum.
func sqlMaximumType(a, b string) (string, bool) {
	if a == b {
		return a, true
	}
	ra, aNum := sqlNumericPromotionRank(a)
	rb, bNum := sqlNumericPromotionRank(b)
	if !aNum || !bNum {
		return "", false
	}
	if ra >= rb {
		return a, true
	}
	return b, true
}

// sqlNumericPromotionRank orders the numeric SQL types by Java's promotion
// lattice (PromoteValue.java:76-81: INT→LONG, INT→FLOAT, INT→DOUBLE,
// LONG→FLOAT, LONG→DOUBLE, FLOAT→DOUBLE — a total order).
func sqlNumericPromotionRank(t string) (int, bool) {
	switch t {
	case "INT", "INTEGER":
		return 1, true
	case "BIGINT":
		return 2, true
	case "FLOAT":
		return 3, true
	case "DOUBLE":
		return 4, true
	}
	return 0, false
}

func buildDerivedTableSourceFromTerm(
	md *recordlayer.RecordMetaData,
	alias string,
	body *antlrgen.QueryTermDefaultContext,
	schemaName string,
	cteScopes map[string]semantic.ScopeSource,
) (semantic.ScopeSource, bool) {
	innerSQ, err := extractFromQueryTerm(body)
	if err != nil || innerSQ == nil {
		return semantic.ScopeSource{}, false
	}
	if len(innerSQ.aggCols) > 0 || innerSQ.countStar {
		// The BODY is the type authority, always. aggOutputCols remains the SQL
		// output-NAME authority; ExactLogicalResultType — the same derivation the
		// translator runs — supplies every field type and nullability, so the
		// scope publishes what the body provably emits rather than what a manual
		// schema can guess about it.
		//
		// This used to be attempted only for a post-aggregate EXPRESSION
		// (`SUM(v)*2`) over a SINGLE table, and to fall through to the manual
		// schema otherwise. Both restrictions dropped an exactly-derivable row on
		// the floor: the manual schema has no expression evaluator, so it also
		// publishes UNKNOWN for an aggregate over a computed ARGUMENT
		// (`SUM(price * qty)`), and a JOINED body declined outright. An UNKNOWN
		// column then made the WHOLE derived row inexact — even a
		// perfectly-known grouping key beside it could no longer resolve — which
		// surfaced as `ORDER BY key "TOTAL_VALUE" has no resolved Value` on
		// queries whose every type is derivable.
		//
		// The manual schema stays as the fallback for a body that cannot prove one
		// complete representable row; partial exactness is never manufactured.
		columns := aggOutputCols(innerSQ, md)
		names := make([]string, len(columns))
		for i, column := range columns {
			names[i] = column.name
		}
		if source, exact := buildExactVirtualScopeSourceForBody(
			md, alias, body, cteScopes, names,
		); exact {
			return source, true
		}
		if len(innerSQ.joins) == 0 && innerSQ.tableName != "" {
			return buildDerivedTableSourceFromAgg(alias, innerSQ, md)
		}
		return semantic.ScopeSource{}, false
	}
	// An inline VALUES source is a virtual relation, not a catalog table. Its
	// exact LogicalInlineValues row (and any projection above it) is the only
	// type authority for an enclosing derived-table scope. Falling through to
	// ResolveTable(innerSQ.tableName) treats the authored alias as a catalog
	// name, silently drops the scope, and leaves an outer WHERE as text-only.
	// Rebuild the body through the same exact logical path execution uses; the
	// parsed projection names remain the SQL output-name authority.
	if innerSQ.inlineValues != nil {
		return buildExactVirtualScopeSourceForBody(
			md, alias, body, cteScopes, projectionOutputNames(innerSQ),
		)
	}
	// Derived-of-derived: recursively build the inner scope.
	if innerSQ.derivedQuery != nil {
		for _, projected := range innerSQ.projExprs {
			if projected != nil {
				// A computed output is typed by its resolved Value. Rebuilding
				// it as UNKNOWN would make the enclosing scope claim a type
				// authority it does not have.
				return buildExactVirtualScopeSourceForBody(
					md, alias, body, cteScopes, projectionOutputNames(innerSQ),
				)
			}
		}
		innerSrc, ok := buildDerivedTableSourceWithCTEs(md, innerSQ.tableName, innerSQ.derivedQuery, cteScopes)
		if !ok {
			return semantic.ScopeSource{}, false
		}
		aliasID := semantic.FromNormalized(alias)
		// Apply inner projection aliases if present.
		srcCols := innerSrc.Table.Columns()
		cols := srcCols
		if innerSQ.projCols != nil {
			cols = make([]semantic.Column, 0, len(innerSQ.projCols))
			for i, col := range innerSQ.projCols {
				// An unaliased QUALIFIED reference (`u.w`) is output under its
				// bare name, as every projection labels it; naming the column by
				// its display spelling published U.W, and `x.w` over
				// `(SELECT u.w FROM (…) u) x` was 42703.
				// A nested path into the inner derived row (`u.w.x`) is decided
				// by its shape, before any lookup, as in the single-table arm.
				if nestedProjectedPath(col, innerSQ.tableAlias) {
					return buildExactVirtualScopeSourceForBody(
						md, alias, body, cteScopes, projectionOutputNames(innerSQ),
					)
				}
				name := col.bare
				if name == "" {
					name = col.name
				}
				if i < len(innerSQ.projAliases) && innerSQ.projAliases[i] != "" {
					name = innerSQ.projAliases[i]
				}
				// CARRY THE WHOLE RESOLVED COLUMN, rename only. Minting a bare
				// {Id, Type:"UNKNOWN", Nullable} drops StructFields and IsArray,
				// and every gate keyed on the flowed type then reads a struct
				// column as UNKNOWN — which comparisonOperandSupported
				// DELIBERATELY admits (bound parameters need that). The result
				// was a whole-struct comparison that planned through a derived
				// table and answered SILENT WRONG ROWS while the same predicate
				// on the base table rejected 0AF00. The type is not decoration:
				// it is what makes the operand gate, nested-field resolution
				// (x.h.city) and array typing work at all.
				resolved, found := lookupSourceColumn(srcCols, col.bare, col.name)
				if !found {
					// The same net as the single-table arm: the alias that names a
					// struct column of the inner derived row.
					if len(col.segs) >= 2 {
						return buildExactVirtualScopeSourceForBody(
							md, alias, body, cteScopes, projectionOutputNames(innerSQ),
						)
					}
					return semantic.ScopeSource{}, false
				}
				cols = append(cols, renameCarriedColumn(resolved, name))
			}
		}
		virtualTable := &semantic.StaticTable{
			TableName:    semantic.FromSegments([]string{alias}, false),
			TableColumns: cols,
		}
		return semantic.ScopeSource{
			Table:           virtualTable,
			Alias:           aliasID,
			CorrelationName: aliasID.Name(),
		}, true
	}
	if len(innerSQ.joins) > 0 {
		return buildDerivedTableSourceFromJoinBody(md, alias, innerSQ, schemaName, cteScopes)
	}
	if innerSQ.tableName == "" {
		return semantic.ScopeSource{}, false
	}
	for _, e := range innerSQ.projExprs {
		if e != nil {
			return buildExactVirtualScopeSourceForBody(
				md, alias, body, cteScopes, projectionOutputNames(innerSQ),
			)
		}
	}
	// A body reading an enclosing WITH-CTE has no CATALOG table to resolve, and
	// the manual per-column derivation below is built entirely on one. Take the
	// body's own exact result row instead — the same derivation the translator
	// runs, which knows the CTE binding. Without this, `WITH c AS (...) SELECT
	// c.fname FROM (SELECT * FROM c) c` declined the whole derived source, the
	// scope builder returned no resolver at all, and the OUTER projection was
	// left with no resolved Value on any slot.
	//
	// projectionOutputNames is the output-name authority only when the body
	// SPELLS its projection; a bare star has no name list here (the expansion
	// happens later), so the exact row's own field names stand.
	if _, bodyReadsCTE := cteScopes[strings.ToUpper(innerSQ.tableName)]; bodyReadsCTE {
		return buildExactVirtualScopeSourceForBody(
			md, alias, body, cteScopes, projectionOutputNames(innerSQ),
		)
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	innerTbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(innerSQ.tableName, "."), false))
	if err != nil {
		return semantic.ScopeSource{}, false
	}

	projCols := innerSQ.projCols
	if projCols == nil {
		// SELECT * — use all columns from the inner table in schema order.
		// Star semantics: ephemeral columns (the __ROW_VERSION pseudo-column)
		// stay hidden (Java's nonEphemeralVisible).
		allCols := semantic.NonEphemeral(innerTbl.Columns())
		projCols = make([]projCol, len(allCols))
		for i, c := range allCols {
			projCols[i] = projCol{name: c.Id.Name(), bare: c.Id.Name()}
		}
	}
	// The body's own visible source names, so a qualified-star slot can be
	// expanded here: the body is single-source at this point (joins declined
	// above), so `x.*` is the whole table exactly when `x` names it.
	bodySourceName := innerSQ.tableAlias
	if bodySourceName == "" {
		segs := strings.Split(innerSQ.tableName, ".")
		bodySourceName = segs[len(segs)-1]
	}

	columns := make([]semantic.Column, 0, len(projCols))
	for i, col := range projCols {
		// A qualified-star slot expands to the body source's columns — the
		// SAME expansion the plan build performs (expandQualifiedStars), done
		// here so the derived table's schema is the row the body really emits.
		//
		// Declining instead was silent, and silently WRONG: the caller drops
		// the whole resolver on a decline, so the outer SELECT's references
		// were never adjudicated at all. `SELECT id FROM (SELECT a.*, a.* FROM
		// a) nested` answered rows off the first ID where Java raises 42702
		// (live-JVM measured), because the ambiguity only exists in a schema
		// nothing built.
		if i < len(innerSQ.projStarQualifiers) && innerSQ.projStarQualifiers[i] != "" {
			if !strings.EqualFold(innerSQ.projStarQualifiers[i], bodySourceName) {
				return semantic.ScopeSource{}, false
			}
			for _, c := range semantic.NonEphemeral(innerTbl.Columns()) {
				columns = append(columns, c)
			}
			continue
		}
		// A NESTED path — the body source's qualifier stripped, two or more
		// segments remain (`t1.w.x` is `w.x`: the struct column w's field x) —
		// is decided by its SHAPE, before any lookup, and goes to the exact
		// derivation, which resolves the path and types the slot. Deciding it
		// after a lookup by the leaf name re-committed RFC-238's error: a leaf
		// with a top-level homonym (`st2.p.sk` beside a STRING column sk) was
		// typed as that column, and the read was refused. An unqualified
		// `w.x` cannot be told from a qualifier here either, and takes the
		// same door.
		// A decline here is FINAL. The exact derivation declines a body whose
		// row it cannot state exactly: a slot the semantic column model has no
		// carrier for (semanticColumnFromExactType), a result type that is not
		// exact (a NULL literal beside the path: "placeholder type is not
		// exact"), a width or label disagreement. Handing such a path to the
		// walk below would look its leaf up by name — LookupColumnRelaxed matches
		// names, never struct fields, so a post-decline HIT is always a top-level
		// homonym — and type it as that column: the error the shape rule exists
		// to prevent. Nominal records and enums publish their complete
		// declarations through the exact path. An unrepresentable row must
		// remain a decline, not acquire the type of a top-level homonym.
		if nestedProjectedPath(col, bodySourceName) {
			return buildExactVirtualScopeSourceForBody(
				md, alias, body, cteScopes, projectionOutputNames(innerSQ),
			)
		}
		// Structured segments; a rebased/computed name is one opaque label.
		bareName := col.bare
		if bareName == "" {
			bareName = col.name
		}
		innerCol, found := semantic.LookupColumnRelaxed(innerTbl, semantic.FromNormalized(bareName))
		if !found {
			// The net under the shape rule: a body source whose alias equals
			// a struct column's name (`st2 AS p`, column p) makes `p.co` the
			// struct's field — Java's lookupNestedField resolves P.CO through
			// the attribute P when the qualified form P.P fails — while the
			// shape rule read P as the qualifier and stripped it. A reference
			// of two or more segments whose leaf is not a column goes to the
			// exact derivation; a one-segment miss is a mistyped column and
			// declines without a body build.
			if len(col.segs) >= 2 {
				return buildExactVirtualScopeSourceForBody(
					md, alias, body, cteScopes, projectionOutputNames(innerSQ),
				)
			}
			return semantic.ScopeSource{}, false
		}
		outName := bareName
		if i < len(innerSQ.projAliases) && innerSQ.projAliases[i] != "" {
			outName = innerSQ.projAliases[i]
		}
		// The virtual column carries the OUTPUT name the derived-table
		// projection emits (Java resolves references to the output column
		// verbatim — no reverse-map to the underlying source column) and
		// EVERYTHING ELSE from the source column unchanged. See
		// renameCarriedColumn: rebuilding it field-by-field is what dropped
		// StructFields and let a struct comparison bypass the operand gate.
		columns = append(columns, renameCarriedColumn(innerCol, outName))
	}

	aliasID := semantic.FromNormalized(alias)
	virtualTable := &semantic.StaticTable{
		TableName:    semantic.FromSegments([]string{alias}, false),
		TableColumns: columns,
	}
	return semantic.ScopeSource{
		Table:           virtualTable,
		Alias:           aliasID,
		CorrelationName: aliasID.Name(),
	}, true
}

// buildDerivedTableSourceFromJoinBody types `FROM (SELECT … FROM a, b …) AS d`
// — a derived table whose BODY is a join. Its output row is the body's select
// list read against the body's own legs, so the alias `d` exposes exactly the
// columns the body emits, in body order.
//
// Declining this shape was not a neutral gap. The caller drops the WHOLE
// resolver when a FROM source cannot be typed (buildSelectScope), so an outer
// query over a join-bodied derived table was never adjudicated at all — and a
// join body is precisely the shape that can emit ONE NAME TWICE
// (`SELECT x.k, y.k FROM zn AS x, zn AS y` outputs K, K). With no schema, the
// duplicate existed only in a row nothing described: `d.k` reached the executor
// and died as a malformed plan, and a bare `k` over `SELECT *` answered off the
// first match. Java never has that hole — the derived quantifier's output is a
// real attribute LIST, and SemanticAnalyzer.lookup counts every attribute whose
// name equals the reference (SemanticAnalyzer.java:441-466), raising
// AMBIGUOUS_COLUMN "Ambiguous reference D.K" on the second
// (SemanticAnalyzer.java:417/422). Building the list here — duplicates
// INCLUDED, because the duplicate is the fact being reported — routes these
// references into the same per-attribute check every other 42702 comes from.
//
// A duplicated output name is only an error to REFERENCE, never to declare:
// `(SELECT x.k, y.k …) AS d` is a legal derived table whose unreferenced
// columns are nobody's problem, so this returns the source rather than
// rejecting the construction.
//
// The legs must be plain catalog tables: a lateral array unnest, a nested
// derived leg, a correlated array source and a USING join's hidden right copy
// each derive their output by a rule this does not implement, and typing them
// wrong is worse than declining — the outer references would be adjudicated
// against a row the body does not emit.
func buildDerivedTableSourceFromJoinBody(
	md *recordlayer.RecordMetaData,
	alias string,
	innerSQ *selectQuery,
	schemaName string,
	cteScopes map[string]semantic.ScopeSource,
) (semantic.ScopeSource, bool) {
	if innerSQ.tableName == "" {
		return semantic.ScopeSource{}, false
	}
	// The body's EXACT row first — the same order the aggregate arm and the CTE
	// arms take — so the source carries the flowed layout the plan flows
	// (exactVirtualScopeSource) beside the SQL names. The catalog walk below
	// states SQL names only; a body that repeats a bare leaf (`ga.g` beside
	// `c.id AS g`) then minted a quantified object over [G G W] for a row the
	// plan declares as [GA.G G W], and every read bound to it — a WHERE, a sort
	// key, an aggregate key — was refused at execution as an edge-layout
	// mismatch while the CTE spelling of the same body answered. The walk
	// remains the fallback for a row the exact derivation cannot state, and it
	// is the ONLY fallback: a walk that cannot describe a leg has nothing
	// further to try, because the exact derivation already declined. An
	// aggregate body never reaches this builder (buildDerivedTableSourceFromTerm
	// takes its aggregate arm first).
	//
	//
	// A STAR body's SQL columns are derived by the star-expansion rules, which
	// the exact labels do not apply: an unnest AS/AT alias shadows a same-named
	// outer column, and the ephemeral __ROW_VERSION pseudo-column stays hidden
	// (Java's nonEphemeralVisible). So the same order the CTE arm takes: the
	// unnest builder, which knows the shadowing rule, answers the star over a
	// base table and its lateral unnests first (exact-first made `d.x` over
	// `(SELECT * FROM things, things.arr AS x)` ambiguous), and an exact row
	// that carries the pseudo-column is declined in favour of the catalog walk
	// below, which hides it (exact-first made a star over row-versioned tables
	// state two hidden slots the reader's row does not carry).
	names := projectionOutputNames(innerSQ)
	if names == nil {
		if src, ok := buildDerivedUnnestScopeSource(md, alias, innerSQ, schemaName, cteScopes); ok {
			return src, true
		}
	}
	if src, ok := buildExactVirtualScopeSourceForSelect(md, alias, innerSQ, nil, names); ok && !exactStarRowCarriesAnEphemeral(innerSQ, src) {
		return src, true
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)

	// One entry per body leg, in FROM order: the name it answers to and the
	// columns it contributes to a star expansion.
	type bodyLeg struct {
		alias string
		cols  []semantic.Column
		// nullSupplying: an OUTER join pads this leg with NULLs for unmatched
		// rows, so every column it contributes is nullable in the body's output
		// REGARDLESS of what the catalog declares. Derived algebraically from
		// the join flavours, never read off the base table — see the wrap below.
		nullSupplying bool
	}
	resolveLeg := func(tableName, legAlias string, segments []string) (bodyLeg, bool) {
		if len(segments) > 1 {
			// A dotted source is a correlated array unnest, not a table.
			return bodyLeg{}, false
		}
		tbl, terr := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if terr != nil {
			return bodyLeg{}, false
		}
		name := legAlias
		if name == "" {
			segs := strings.Split(tableName, ".")
			name = segs[len(segs)-1]
		}
		return bodyLeg{alias: strings.ToUpper(name), cols: semantic.NonEphemeral(tbl.Columns())}, true
	}
	// A leg this catalog walk cannot describe — a LATERAL ARRAY UNNEST
	// (`C."ARR" AS "X"`, which resolveLeg declines as a dotted source), a
	// derived leg, a USING leg — is a decline: the exact derivation above has
	// already had the body, and this walk is what remains for a row it could
	// not state.
	primary, ok := resolveLeg(innerSQ.tableName, innerSQ.tableAlias, innerSQ.sourceSegments)
	if !ok {
		return semantic.ScopeSource{}, false
	}
	legs := []bodyLeg{primary}
	for _, j := range innerSQ.joins {
		if j.derivedQuery != nil || len(j.usingHiddenCols) > 0 || j.usingUids != nil {
			return semantic.ScopeSource{}, false
		}
		leg, legOK := resolveLeg(j.tableName, j.alias, j.segments)
		if !legOK {
			return semantic.ScopeSource{}, false
		}
		legs = append(legs, leg)
	}
	padded := nullSupplyingFromLegs(innerSQ.joins)
	for li := range legs {
		if li < len(padded) {
			legs[li].nullSupplying = padded[li]
		}
	}
	// Applied once the whole FROM list is known: a RIGHT JOIN in position 3
	// changes legs 0..2, so no leg's nullability is final until the last join
	// clause has been read. Copy-on-wrap — the Column values must not be shared
	// back to the catalog.
	for li := range legs {
		if !legs[li].nullSupplying {
			continue
		}
		wrapped := make([]semantic.Column, len(legs[li].cols))
		for ci, c := range legs[li].cols {
			c.Nullable = true
			wrapped[ci] = c
		}
		legs[li].cols = wrapped
	}

	// SELECT * over the body: every leg's columns, concatenated in FROM order.
	// This is the row the body emits, duplicate names and all.
	if innerSQ.projCols == nil {
		if innerSQ.projQualifier != "" {
			// `SELECT x.*` — one named leg's columns.
			for _, leg := range legs {
				if leg.alias == strings.ToUpper(innerSQ.projQualifier) {
					return derivedJoinBodySource(alias, append([]semantic.Column(nil), leg.cols...)), true
				}
			}
			return semantic.ScopeSource{}, false
		}
		var columns []semantic.Column
		for _, leg := range legs {
			columns = append(columns, leg.cols...)
		}
		return derivedJoinBodySource(alias, columns), true
	}

	var columns []semantic.Column
	for i, col := range innerSQ.projCols {
		if i < len(innerSQ.projStarQualifiers) && innerSQ.projStarQualifiers[i] != "" {
			found := false
			for _, leg := range legs {
				if leg.alias == strings.ToUpper(innerSQ.projStarQualifiers[i]) {
					columns = append(columns, leg.cols...)
					found = true
					break
				}
			}
			if !found {
				return semantic.ScopeSource{}, false
			}
			continue
		}
		bareName := col.bare
		if bareName == "" {
			bareName = col.name
		}
		id := semantic.FromNormalized(bareName)
		var (
			resolved semantic.Column
			hits     int
		)
		for _, leg := range legs {
			if col.qualified && leg.alias != strings.ToUpper(col.qualifier) {
				continue
			}
			for _, c := range leg.cols {
				if c.Id.Name() == id.Name() {
					resolved = c
					hits++
				}
			}
		}
		// hits != 1 is an ambiguity or a miss INSIDE the body, which belongs to
		// the body's own resolution, not to this schema derivation. Decline
		// rather than guess a row the body may never produce.
		if hits != 1 {
			return semantic.ScopeSource{}, false
		}
		outName := bareName
		if i < len(innerSQ.projAliases) && innerSQ.projAliases[i] != "" {
			outName = innerSQ.projAliases[i]
		}
		columns = append(columns, renameCarriedColumn(resolved, outName))
	}
	return derivedJoinBodySource(alias, columns), true
}

// buildDerivedUnnestScopeSource publishes a star over a base/UNNEST spine
// from the same typed semantic sources used by expression resolution. SQL
// attributes are not the internal whole-element slots of the physical seed.
func buildDerivedUnnestScopeSource(md *recordlayer.RecordMetaData, alias string, innerSQ *selectQuery, schemaName string, cteScopes map[string]semantic.ScopeSource) (semantic.ScopeSource, bool) {
	if md == nil || innerSQ == nil || len(innerSQ.joins) == 0 || innerSQ.derivedQuery != nil || innerSQ.tableName == "" || innerSQ.projCols != nil {
		return semantic.ScopeSource{}, false
	}
	tables := newUnnestTableResolver(md, schemaName)
	for i, join := range innerSQ.joins {
		visible := visibleFromAliases(innerSQ.tableName, innerSQ.tableAlias, innerSQ.joins[:i], tables)
		if !isLateralUnnestJoin(join, visible, tables) {
			return semantic.ScopeSource{}, false
		}
	}
	resolver, err := buildSelectScopeChecked(innerSQ, md, schemaName, cteScopes)
	if err != nil {
		return semantic.ScopeSource{}, false
	}
	var columns []semantic.Column
	for _, source := range resolver.Scope().Sources() {
		if innerSQ.projQualifier != "" && source.Alias.Name() != innerSQ.projQualifier {
			continue
		}
		for _, column := range semantic.NonEphemeral(source.Table.Columns()) {
			if _, hidden := source.HiddenColumns[strings.ToUpper(column.Id.Name())]; !hidden {
				columns = append(columns, column)
			}
		}
		if innerSQ.projQualifier != "" {
			break
		}
	}
	return derivedJoinBodySource(alias, columns), len(columns) != 0
}

// derivedJoinBodySource wraps a join body's derived output columns as the
// virtual scope source the outer FROM alias exposes.
// nullSupplyingFromLegs derives, PER FROM POSITION, whether an outer join pads
// that leg with NULLs. Index 0 is the primary source; index i+1 is joins[i].
//
// NULLABILITY IS DERIVED FROM THE JOIN ALGEBRA, not copied from the catalog. An
// outer join pads its null-supplying side, so that side's columns are nullable
// in the join's output whatever the base table declares —
// `FROM a LEFT JOIN b ON …` serves NULL for `b.y` on every unmatched `a` row.
// This is Java's pullUpResultColumnsWithNullability, and the physical side does
// the same (exactJoinResultType widens the padded leg's column types); the
// derivations must agree, because this one is what adjudicates the SQL
// references against the row that side produces.
//
// LEFT pads the RIGHT leg. RIGHT pads everything to its LEFT — a later RIGHT
// JOIN makes the whole accumulated left side null-supplying, which is why the
// answer is only final after the LAST join clause has been read, and why this
// returns the finished vector rather than a per-leg verdict.
func nullSupplyingFromLegs(joins []joinClause) []bool {
	padded := make([]bool, len(joins)+1)
	for i, j := range joins {
		switch j.joinType {
		case joinTypeLeft:
			padded[i+1] = true
		case joinTypeRight:
			for k := 0; k <= i; k++ {
				padded[k] = true
			}
		case joinTypeFull:
			for k := 0; k <= i+1; k++ {
				padded[k] = true
			}
		case joinTypeInner:
			// A comma join or an explicit INNER JOIN pads nothing.
		}
	}
	return padded
}

// nullSupplyingTable is a catalog table seen through an outer join's padding:
// every column it contributes is nullable in the join's output. Name, Indexes
// and the underlying identity delegate unchanged, so index selection and
// diagnostics see the same table — only the nullability the join actually
// changes is different.
type nullSupplyingTable struct {
	semantic.Table
}

func (t nullSupplyingTable) Columns() []semantic.Column {
	source := t.Table.Columns()
	widened := make([]semantic.Column, len(source))
	for i, c := range source {
		c.Nullable = true
		widened[i] = c
	}
	return widened
}

func (t nullSupplyingTable) LookupColumn(id semantic.Identifier) (semantic.Column, bool) {
	c, found := t.Table.LookupColumn(id)
	if !found {
		return c, false
	}
	c.Nullable = true
	return c, true
}

func derivedJoinBodySource(alias string, columns []semantic.Column) semantic.ScopeSource {
	aliasID := semantic.FromNormalized(alias)
	return semantic.ScopeSource{
		Table: &semantic.StaticTable{
			TableName:    semantic.FromSegments([]string{alias}, false),
			TableColumns: columns,
		},
		Alias:           aliasID,
		CorrelationName: aliasID.Name(),
	}
}

// aggOutputCol is one VISIBLE output column of an aggregate SELECT body.
//
// carried, when set, is the SOURCE column this output column IS — a grouping
// key, or a type-preserving aggregate over a bare column. It is carried WHOLE
// rather than reduced to (typ, nullable) for the reason renameCarriedColumn
// documents: a rebuilt {Id, Type, Nullable} drops StructFields and IsArray, a
// struct column then types UNKNOWN, and UNKNOWN is deliberately admitted by the
// comparison operand gate — so a whole-struct comparison that rejects 0AF00 on
// the base table plans straight through the derived table.
type aggOutputCol struct {
	name     string
	typ      string
	nullable bool
	carried  semantic.Column
	hasCol   bool
}

// column renders one output column as the semantic.Column the derived-table
// schema installs.
func (c aggOutputCol) column() semantic.Column {
	if c.hasCol {
		return renameCarriedColumn(c.carried, c.name)
	}
	return semantic.Column{Id: semantic.FromNormalized(c.name), Type: c.typ, Nullable: c.nullable}
}

// aggBodySourceColumns resolves the aggregate body's single FROM table to its
// declared columns, which is what typing an aggregate's OUTPUT needs: Java
// derives the result type from the ARGUMENT's type, so the argument has to be
// resolvable. A nil result means "not derivable here", and every caller must
// treat that as UNKNOWN rather than as an empty column list — the two are
// different claims and only one of them is honest.
//
// A body with joins is declined rather than searched: with more than one source
// a bare argument name can be ambiguous, and answering it by first-match would
// type an output column off the wrong table.
func aggBodySourceColumns(sq *selectQuery, md *recordlayer.RecordMetaData) []semantic.Column {
	if sq == nil || md == nil || sq.tableName == "" || len(sq.joins) > 0 {
		return nil
	}
	// A DERIVED body is not the rare case, it is the corpus's case:
	// `SELECT MIN(x.col2) … FROM (SELECT col1, col2 FROM t1) AS x GROUP BY x.col1`
	// aggregates over a subquery, and tableName then holds that subquery's
	// ALIAS rather than a catalog table. Resolving the alias against the
	// catalog fails, so typing off it alone would have declined exactly the
	// shape this typing exists for — measured: with only the base-table arm,
	// the corpus row still reported INTEGER.
	if sq.inlineValues != nil {
		src, ok := parsedInlineValuesScopeSource(sq.inlineValues, sq.tableAlias, "", md)
		if !ok || src.Table == nil {
			return nil
		}
		return src.Table.Columns()
	} else if sq.derivedQuery != nil {
		src, ok := buildDerivedTableSource(md, sq.tableName, sq.derivedQuery)
		if !ok || src.Table == nil {
			return nil
		}
		return src.Table.Columns()
	}
	analyzer := semantic.NewAnalyzer(rlcatalog.Wrap(md), false)
	tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(sq.tableName, "."), false))
	if err != nil || tbl == nil {
		return nil
	}
	return tbl.Columns()
}

// aggregateOutputColumn types ONE aggregate output column the way Java does.
// Java has no table of SQL-level aggregate result types; the type falls out of
// which PhysicalOperator `encapsulate` selects for the (function, argument
// TypeCode) pair (NumericAggregationValue.java:194-213), and that lookup is
// EXACT — there is no widening, so SUM over an INT column is INT and not
// BIGINT. Reading the operators off the enum:
//
//   - COUNT, COUNT(*): always LONG. CountValue's two operators both carry
//     TypeCode.LONG (CountValue.java:241-243) and getResultType returns it
//     unconditionally (:140-141). The argument's type is irrelevant.
//   - AVG: always DOUBLE. AVG_I, AVG_L, AVG_F and AVG_D all declare
//     TypeCode.DOUBLE as their result (NumericAggregationValue.java:634-676),
//     so an integer average widens.
//   - SUM, MIN, MAX: the ARGUMENT's type, exactly. SUM_I/L/F/D and the MIN_*
//     and MAX_* families each pair an argument TypeCode with the SAME result
//     TypeCode (:629-632, :679-687).
//
// NULLABILITY is `true` for every aggregate, which is also Java's:
// getResultType builds `Type.primitiveType(resultTypeCode)` and the one-argument
// overload defaults isNullable to true (Type.java:404-405). That includes COUNT.
//
// UNKNOWN is the answer whenever the argument's type is not in hand — a computed
// argument expression, or a body whose source columns could not be resolved. It
// is what this function returned for EVERY aggregate before, so it is the
// established meaning of "no type", not a new state.
func aggregateOutputColumn(ac aggSelectCol, name string, srcCols []semantic.Column) aggOutputCol {
	unknown := aggOutputCol{name: name, typ: "UNKNOWN", nullable: true}
	switch strings.ToUpper(ac.aggFunc) {
	case "COUNT":
		return aggOutputCol{name: name, typ: "BIGINT", nullable: true}
	case "AVG":
		return aggOutputCol{name: name, typ: "DOUBLE", nullable: true}
	case "SUM", "MIN", "MAX":
		if ac.aggExpr != nil || srcCols == nil {
			return unknown
		}
		arg, found := lookupSourceColumn(srcCols, ac.aggArgBare, ac.aggArg)
		if !found {
			return unknown
		}
		// The whole column, renamed — the result type IS the argument type, so
		// the argument column IS the output column. Carrying it keeps
		// StructFields and IsArray, and NULLABILITY is forced to true
		// independently of the argument's: an aggregate over an empty group
		// yields NULL even when the column is declared NOT NULL.
		arg.Nullable = true
		arg.Type = baseSQLType(arg.Type)
		return aggOutputCol{name: name, typ: arg.Type, nullable: true, carried: arg, hasCol: true}
	}
	return unknown
}

// aggOutputCols returns the aggregate body's VISIBLE output columns in install
// order (the SELECT-list COUNT(*) first, then the aggregate/group columns) — the
// SINGLE authority both buildDerivedTableSourceFromAgg (to build the schema) and
// the ON-only complete-or-decline gate (to dedup) consume, so the derivation is
// never twice-written. HIDDEN aggregates (a HAVING/ORDER-BY COUNT(*) harvested
// into aggCols with visible=false) are NOT output columns — they must not be
// advertised in the schema nor counted by the dup gate (else e.g.
// `SELECT COUNT(*) … HAVING COUNT(*) > 0` false-collides its lone output).
// countStar is set only for a SELECT-list COUNT(*), so it is always visible.
//
// EVERY OUTPUT IS TYPED, and it used to be that only the SELECT-list COUNT(*)
// was: every other entry was minted "UNKNOWN". That is not a missing-metadata
// state, it is a dropped one — the body's source table is right there — and it
// crossed the derived-table boundary as a loss of the column's real type. The
// visible symptom is arithmetic: `SELECT G + 4 FROM (SELECT MIN(col2) AS G …)
// AS Y` returned an Integer where Java returns a Long, because G arrived
// untyped and the addition fell back to the narrow lane.
//
// md may be nil — the type simply stays UNKNOWN then, exactly as before. It is
// never a reason to decline the source: the NAMES are what the caller's dedup
// gate and the enclosing resolver need, and they do not depend on the types.
func aggOutputCols(sq *selectQuery, md *recordlayer.RecordMetaData) []aggOutputCol {
	var out []aggOutputCol
	if sq.countStar {
		name := sq.countStarAlias
		// NULLABLE, exactly like every other aggregate output below. Java's
		// CountValue.getResultType is `Type.primitiveType(TypeCode.LONG)`
		// (CountValue.java:140-141) and the one-argument overload hardcodes
		// isNullable=true (Type.java:404-405), so COUNT(*) is LONG NULLABLE
		// there. This arm minted it NOT NULL, which contradicted the COUNT arm
		// of aggregateOutputColumn twenty lines down — the same function, the
		// same Java rule, two answers.
		out = append(out, aggOutputCol{name: name, typ: "BIGINT", nullable: true})
	}
	srcCols := aggBodySourceColumns(sq, md)
	for _, index := range aggregateColumnsInSelectOrder(sq.aggCols) {
		ac := sq.aggCols[index]
		name := aggregateOutputSQLName(ac)
		if ac.aggFunc == "" {
			// A GROUPING KEY, not an aggregate: the output column IS the source
			// column, so it is carried whole under the output name. Grouping
			// does not change a value's type, and a key over a struct column
			// (`GROUP BY n`) must keep its StructFields for the same reason
			// renameCarriedColumn exists.
			if srcCols != nil {
				if src, found := lookupSourceColumn(srcCols, ac.groupColBare, ac.groupCol); found {
					out = append(out, aggOutputCol{
						name: name, typ: baseSQLType(src.Type), nullable: src.Nullable,
						carried: src, hasCol: true,
					})
					continue
				}
			}
			out = append(out, aggOutputCol{name: name, typ: "UNKNOWN", nullable: true})
			continue
		}
		out = append(out, aggregateOutputColumn(ac, name, srcCols))
	}
	return out
}

func buildDerivedTableSourceFromAgg(alias string, sq *selectQuery, md *recordlayer.RecordMetaData) (semantic.ScopeSource, bool) {
	cols := aggOutputCols(sq, md)
	if len(cols) == 0 {
		return semantic.ScopeSource{}, false
	}
	columns := make([]semantic.Column, len(cols))
	for i, c := range cols {
		columns[i] = c.column()
	}
	aliasID := semantic.FromNormalized(alias)
	virtualTable := &semantic.StaticTable{
		TableName:    semantic.FromSegments([]string{alias}, false),
		TableColumns: columns,
	}
	return semantic.ScopeSource{
		Table:           virtualTable,
		Alias:           aliasID,
		CorrelationName: aliasID.Name(),
	}, true
}

// mapPredicateWalkError converts a resolver.WalkPredicate failure into the
// SQLSTATE-classified *api.Error it should surface as, or nil when the error is
// not one of the recognized semantic / IN-shape errors (the caller then decides
// whether to fall back to a text predicate or fail closed). Shared by the
// WHERE-clause and JOIN-ON resolution paths so both classify column, ambiguity,
// source, and IN-shape failures identically — and a structured *api.Error from a
// nested subquery build surfaces verbatim.
//
// A bare ColumnNotFoundError maps to ErrCodeUndefinedColumn so a WHERE-clause
// correlated subquery's BuildExists can fall back to buildCorrelatedExists with
// its richer outer scope (RFC-141/RFC-142); in the JOIN-ON path the same mapping
// is simply the correct 42703 for an ON column that does not exist.
func mapPredicateWalkError(walkErr error) *api.Error {
	var tableNotFound *semantic.TableNotFoundError
	if errors.As(walkErr, &tableNotFound) {
		return api.WrapErrorf(walkErr, api.ErrCodeUndefinedTable, "Unknown table %s", tableNotFound.Name.Name())
	}
	var ambigErr *semantic.AmbiguousColumnError
	if errors.As(walkErr, &ambigErr) {
		// Java's exact SemanticAnalyzer text, from the reference as written.
		return api.NewErrorf(api.ErrCodeAmbiguousColumn, "Ambiguous reference %s", ambigErr.Reference())
	}
	var inListNull *expr.InListNullError
	if errors.As(walkErr, &inListNull) {
		return api.NewError(api.ErrCodeWrongObjectType, "NULL values are not allowed in the IN list")
	}
	var srcNotFound *semantic.SourceNotFoundError
	if errors.As(walkErr, &srcNotFound) {
		return api.NewErrorf(api.ErrCodeUndefinedColumn, "no FROM source aliased as %s", srcNotFound.Alias.Name())
	}
	var colNotFound *semantic.ColumnNotFoundError
	if errors.As(walkErr, &colNotFound) {
		return api.NewErrorf(api.ErrCodeUndefinedColumn, "column %q does not exist", colNotFound.Id.Name())
	}
	var shadowErr *semantic.CorrelatedShadowError
	if errors.As(walkErr, &shadowErr) {
		// A correlated reference shadowed by a same-named FROM source that lacks the
		// column is a RESOLUTION failure (undefined column in the bound scope) →
		// 42703, recognized by type BEFORE the CorrelatedExistsError fallback.
		return api.NewError(api.ErrCodeUndefinedColumn, shadowErr.Error())
	}
	var inColRef *expr.InColumnRefError
	if errors.As(walkErr, &inColRef) {
		return api.NewError(api.ErrCodeUnsupportedOperation, inColRef.Error())
	}
	var enumErr *values.InvalidEnumValueError
	if errors.As(walkErr, &enumErr) {
		// Java ExceptionUtil leaves INVALID_ENUM_VALUE in INTERNAL_ERROR.
		return api.WrapError(api.ErrCodeInternalError, enumErr.Error(), walkErr)
	}
	var binErr *expr.InvalidBinaryLiteralError
	if errors.As(walkErr, &binErr) {
		return api.NewError(api.ErrCodeInvalidBinaryRepresentation, binErr.Error())
	}
	// A nested planner may deliberately carry the more specific
	// UnsupportedQuery (0AF00) classification through a correlation wrapper.
	// Preserve it before the generic CorrelatedExistsError fallback maps
	// message-only unsupported shapes to UnsupportedOperation (0A000).
	var carriedAPI *api.Error
	if errors.As(walkErr, &carriedAPI) && carriedAPI.Code == api.ErrCodeUnsupportedQuery {
		return carriedAPI
	}
	var corrExistsErr *CorrelatedExistsError
	if errors.As(walkErr, &corrExistsErr) {
		// Every GENUINE semantic resolution error (Ambiguous / ColumnNotFound /
		// SourceNotFound / …) is mapped to 42703/42702 ABOVE via the cause chain.
		// A CorrelatedExistsError reaching HERE therefore wraps NO recognized
		// resolution error — it is a deliberate unsupported-shape decline (a
		// Message-only rejection, an Unsupported=true decline, or one wrapping a
		// NON-semantic unsupported cause like COUNT(DISTINCT …)) → 0A000. Classifying
		// by the recognized-cause TYPE (not the Unsupported flag or a Cause==nil
		// heuristic) is what keeps every path's SQLSTATE consistent.
		return api.NewError(api.ErrCodeUnsupportedOperation, corrExistsErr.Error())
	}
	var apiErr *api.Error
	if errors.As(walkErr, &apiErr) {
		return apiErr
	}
	return nil
}

// bindingOrAlias resolves a FROM leg's binding correlation name: the
// parser-minted duplicate-leg id when present, else the alias. The single
// mint authority (assignFromLegBindingIDs)
// sets bindingID only on LATER duplicate legs; every non-duplicate leg keeps
// its alias as the correlation, so the resolver emits QOV(binding) addressing
// the leg's own quantifier — never the colliding alias namespace. Every scope
// builder reads THIS one helper so no site re-derives the fallback.
func bindingOrAlias(bindingID string, aliasID semantic.Identifier) string {
	if bindingID != "" {
		return bindingID
	}
	return aliasID.Name()
}

// upgradeJoinOnPredicates walks the logical plan tree to find LogicalJoin
// nodes and upgrades their OnText to OnPredicate using the full join scope.
// The join nodes are created in order matching sq.joins, so we match
// them sequentially by walking the left-child spine (the builder chains
// joins left-to-right with op = NewJoin(op, right, ...)).
func upgradeJoinOnPredicates(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource, cteOnScopes map[string]semantic.ScopeSource) error {
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)

	// isDeclaredCTE: the name IS a WITH-declared CTE, even when its
	// column-schema derivation declined and cteScopes has no entry (every
	// declared CTE not in cteScopes gets a cteOnScopes entry at WITH
	// registration — a derived source or a nil-Table marker). The distinction
	// is load-bearing for the drop-risk taxonomy below: an unresolvable REAL
	// table errors precisely downstream, but a declared CTE resolves fine at
	// translation — nothing downstream errors, so a silent scope decline here
	// silently DROPS the join's ON and the query returns cross-product rows.
	isDeclaredCTE := func(tableName string) bool {
		key := strings.ToUpper(tableName)
		if _, ok := cteOnScopes[key]; ok {
			return true
		}
		_, ok := cteScopes[key]
		return ok
	}

	resolveTable := func(tableName string) semantic.Table {
		// CTE-FIRST (execution's shadowing order — the same ordering
		// cteLegKind and buildSelectScope apply): a declared CTE shadows a
		// same-named catalog table; the prior analyzer-first order resolved
		// an ON through a shadowing CTE against the TABLE's schema —
		// over-declining valid ONs (42703 on the CTE's own columns) and, for
		// an ON naming a table-only column, ADMITTING the upgrade and moving
		// the failure to a runtime malformed plan (review-caught). The
		// ON-ONLY scope (join/unnest bodies kept out of the GLOBAL cteScopes
		// — the flatten-evasion class) resolves here so the enclosing join's
		// ON is never silently dropped; a marker entry (nil Table) falls
		// through to the loud drop-risk arm in addTableSource.
		if src, found := cteOnScopes[strings.ToUpper(tableName)]; found {
			return src.Table
		}
		if cteScopes != nil {
			if src, found := cteScopes[strings.ToUpper(tableName)]; found {
				return src.Table
			}
		}
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err == nil {
			return tbl
		}
		return nil
	}

	// Collect LogicalJoin nodes from the left-child spine. The builder
	// chains joins left-to-right: Join(Join(Scan, R0), R1), so the
	// outermost join wraps the LAST sq.joins entry. We collect them
	// and then match in reverse.
	var joins []*logical.LogicalJoin
	for cur := op; cur != nil; {
		j, ok := cur.(*logical.LogicalJoin)
		if !ok {
			ch := cur.Children()
			if len(ch) > 0 {
				cur = ch[0]
				continue
			}
			break
		}
		joins = append(joins, j)
		cur = j.Left
	}

	// Build the full scope for predicate resolution. A lateral array unnest
	// leg (`FROM T1 INNER JOIN U ON …, T1.ARR AS V`) is NOT a real table —
	// resolveTable("T1.ARR") fails. Without registering its virtual element/
	// ordinal source, the scope build would abort, the ON resolver would never
	// run, and the EXPLICIT JOIN's ON predicate (`U.ID = T1.ID`) would be silently
	// DROPPED → the T1/U join degrades to a CROSS join (silent-wrong). Register the
	// unnest leg via the SAME shared helpers every other scope builder uses so the
	// ON predicate still resolves against the real-table legs. RFC-142.
	scope := semantic.NewScope(sq.enclosingScope)
	addUnnestSourceRaw := unnestScopeSourceAdder(scope)
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	// scopeDropRisk marks scope failures where the query could still PLAN and
	// return silently-wrong cross-product rows if we fall through: a
	// resolvable-but-unscopable source (derived-table decline, duplicate
	// alias). An UNRESOLVABLE table (resolveTable nil) is NOT a drop risk —
	// the downstream scan produces its precise UndefinedDatabase/Table error,
	// which the fail-closed check below must not preempt with a generic one.
	var scopeDropRisk bool
	addTableSource := func(tableName, alias, bindingID string) bool {
		// ACTIVE-SCHEMA-QUALIFIED source (`"s"."LA"`): the visitor path's sq
		// keeps the dotted spelling (normalizeSchemaQualifiedSelectSources
		// runs only on the catalog sub-build path), so resolveTable failed
		// and the silent unresolvable-table decline below dropped the ON —
		// but the downstream scan SUCCEEDS after the tree-side demotion, so
		// the "unresolvable table errors precisely downstream" assumption
		// that keeps the decline silent is FALSE for this form: every
		// explicit join with a schema-qualified leg silently cross-producted
		// (review-caught by the Q37 pin). Strip the schema segment the same
		// way the normalizer does, keeping a defaulted alias in lockstep.
		if segs := strings.Split(tableName, "."); len(segs) == 2 && resolvesToTable(segs) {
			if alias == tableName {
				alias = segs[1]
			}
			tableName = segs[1]
		}
		tbl := resolveTable(tableName)
		if tbl == nil {
			// A DECLARED CTE whose schema derivation declined (join/unnest
			// body the deriver cannot type) is resolvable-but-unscopable —
			// the same drop-risk class as a JOIN-bodied derived table: the
			// query still PLANS (translation resolves the CTE body), no
			// downstream error fires, and the dropped ON turns the join into
			// a silent cross product. An unresolvable REAL table stays a
			// silent decline (the downstream scan raises the precise
			// UndefinedTable error this generic one must not preempt).
			if isDeclaredCTE(tableName) {
				scopeDropRisk = true
			}
			return false
		}
		aliasID := semantic.FromNormalized(alias)
		if alias == "" {
			aliasID = semantic.FromNormalized(tableName)
		}
		// The binding correlation: the parser-minted duplicate-leg id when
		// present, else the alias. Duplicate
		// PLAIN aliases REGISTER (per-attribute resolution owns the
		// ambiguity); only a shadowing (unnest) duplicate still errors, and
		// that keeps the drop-risk taxonomy exactly as before for the class
		// AddSource can still reject.
		binding := bindingOrAlias(bindingID, aliasID)
		if scope.AddSource(semantic.ScopeSource{
			Table:           tbl,
			Alias:           aliasID,
			CorrelationName: binding,
		}) != nil {
			scopeDropRisk = true // shadowing-duplicate alias: resolvable, unscopable
			return false
		}
		return true
	}
	// A derived-table JOIN source (`... JOIN (SELECT ...) AS x ON ...`) is NOT a
	// real table — register its virtual column schema (derived from the
	// subquery body) so the ON predicate referencing `x.col` resolves. Without
	// this the scope build aborts, the ON resolver never runs, and the join's
	// ON predicate is silently DROPPED → the outer join degrades to a cartesian
	// product that still null-pads (a wrong result). Mirrors the lateral-unnest
	// leg registration above.
	addDerivedSource := func(j joinClause) bool {
		src, err := boundDerivedSource(md, j.alias, j.bindingID, j.derivedQuery, j.catalogAwareInnerPlan, sq.enclosingScope, schemaName, cteScopes)
		if err != nil {
			scopeDropRisk = true // join-bodied derived decline: plans, then cross-products
			return false
		}
		if j.bindingID != "" {
			src.CorrelationName = j.bindingID
		}
		if scope.AddSource(src) != nil {
			scopeDropRisk = true
			return false
		}
		return true
	}
	addInlineSource := func(item *antlrgen.InlineTableItemContext, alias, bindingID string, hidden []string) bool {
		src, ok := parsedInlineValuesScopeSource(item, alias, bindingID, md)
		if !ok {
			scopeDropRisk = true
			return false
		}
		src.HiddenColumns = hiddenColumnSet(hidden)
		if scope.AddSource(src) != nil {
			scopeDropRisk = true
			return false
		}
		return true
	}
	// A shape that RESOLVED as a lateral unnest but cannot be scoped (its
	// AS/AT alias collides with an existing source — AddSource duplicate) is
	// resolvable-but-unscopable: a drop risk by the same taxonomy (review
	// catch: the shared adder closure predates the flag and its dup-alias arm
	// escaped it — `FROM t AS x, t.arr AS x JOIN u ON …` silently dropped the
	// ON).
	addUnnestSource := func(j joinClause) bool {
		if !addUnnestSourceRaw(j) {
			scopeDropRisk = true
			return false
		}
		return true
	}
	var scopeOK bool
	if sq.inlineValues != nil {
		scopeOK = addInlineSource(sq.inlineValues, sq.tableAlias, "", nil)
	} else if sq.derivedQuery != nil {
		// Primary FROM source is a derived table (`FROM (SELECT ...) x JOIN ...`).
		if src, err := boundDerivedSource(md, sq.tableAlias, sq.bindingID, sq.derivedQuery, sq.catalogAwareInnerPlan, sq.enclosingScope, schemaName, cteScopes); err == nil {
			scopeOK = scope.AddSource(src) == nil
			if !scopeOK {
				scopeDropRisk = true
			}
		} else {
			scopeDropRisk = true
		}
	} else {
		scopeOK = addTableSource(sq.tableName, sq.tableAlias, "")
	}
	for i, j := range sq.joins {
		if !scopeOK {
			break
		}
		if j.inlineValues != nil {
			scopeOK = addInlineSource(j.inlineValues, j.alias, j.bindingID, j.usingHiddenCols)
			continue
		}
		if j.derivedQuery != nil {
			scopeOK = addDerivedSource(j)
			continue
		}
		visible := visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i], resolvesToTable)
		if isLateralUnnestJoin(j, visible, resolvesToTable) {
			scopeOK = addUnnestSource(j)
			continue
		}
		scopeOK = addTableSource(j.tableName, j.alias, j.bindingID)
	}
	if !scopeOK {
		// FAIL-CLOSED (guards a silent-wrong-rows bug class): the scope could
		// not be built, so no
		// ON predicate can be resolved. When the failure is a DROP RISK — a
		// resolvable-but-unscopable source (JOIN-bodied derived-table
		// decline, duplicate alias) — returning nil here would leave
		// OnPredicate nil and the translator silently degrades the join to a
		// CROSS PRODUCT (it never reads OnText for predicates): the same
		// failure class as the fixed subquery-in-ON bug and this function's
		// own fail-closed backstop below, which this early return used to
		// bypass. ON-less joins (comma cross joins, lateral unnest legs)
		// have nothing to drop, and UNRESOLVABLE-table failures keep the
		// silent decline — the downstream scan raises the precise
		// UndefinedDatabase/Table error this generic one must not preempt.
		if scopeDropRisk {
			for _, j := range sq.joins {
				if j.onExpr != nil {
					return api.NewErrorf(api.ErrCodeUnsupportedQuery,
						"unsupported FROM shape: cannot resolve the join's sources for its ON clause (e.g. a JOIN-bodied derived table or duplicate unaliased source); dropping the ON condition would return cross-product rows")
				}
			}
		}
		return nil
	}
	resolver := expr.New(analyzer, scope)

	// Match collected joins with sq.joins in reverse order.
	for i, j := range joins {
		sqIdx := len(sq.joins) - 1 - i
		if sqIdx < 0 || sqIdx >= len(sq.joins) {
			break
		}
		if sq.joins[sqIdx].onExpr != nil && j.OnPredicate == nil {
			// EXISTS in a JOIN ON clause (RFC-154 §5, Java parity). For an INNER
			// join this IS EXISTS in WHERE (no null-extension): install a
			// SubqueryPlanner so WalkPredicate builds the ON predicate's
			// ExistentialValuePredicate, then park the collected EXISTS subqueries
			// on the join for foldInnerOnExistsIntoWhere to move into the WHERE.
			//
			// OUTER joins are deferred (RFC-154 §5.2b): the ON-EXISTS is correlated
			// to the PRESERVED side and gates null-extension, which the semi-join
			// shape cannot express (the existential peel would drop preserved
			// rows whose EXISTS is false instead of null-extending). Reject
			// fail-closed so OUTER EXISTS-in-ON never returns wrong rows.
			if expr.ContainsExistsAtom(sq.joins[sqIdx].onExpr) {
				if j.Kind != logical.JoinInner {
					return api.NewError(api.ErrCodeUnsupportedQuery,
						"EXISTS in an OUTER JOIN ON clause is not yet supported")
				}
				onPlanner := &existsSubqueryPlanner{
					md:          md,
					schemaName:  schemaName,
					outerScopes: buildOuterScopeSources(sq, md, schemaName, cteScopes),
					cteScopes:   cteScopes,
					cteOnScopes: cteOnScopes,
				}
				resolver.SetSubqueryPlanner(onPlanner)
				pred, walkErr := resolver.WalkPredicate(sq.joins[sqIdx].onExpr)
				resolver.SetSubqueryPlanner(nil) // don't leak into the next join's walk
				if walkErr != nil {
					if apiErr := mapPredicateWalkError(walkErr); apiErr != nil {
						return apiErr
					}
					return api.NewErrorf(api.ErrCodeUnsupportedQuery,
						"unsupported EXISTS in JOIN ON clause: %v", walkErr)
				}
				// Parked on the join for the block's last step,
				// foldInnerOnExistsIntoWhere, which moves the markers and the
				// subqueries into the WHERE (on_exists_fold.go): several EXISTS in
				// one ON are several WHERE-EXISTS, exactly as Java has them.
				j.OnPredicate = predicates.SimplifyPredicateValues(pred)
				j.OnExistsSubqueries = onPlanner.subqueries
				continue
			}
			// A scalar `(SELECT ...)` or `x IN (SELECT ...)` subquery in the ON
			// clause: Go (like Java) does not support correlated scalar subqueries
			// or IN-subqueries anywhere. The ON resolver installs no SubqueryPlanner,
			// so WalkPredicate would decline with UnsupportedExpressionShapeError and
			// the fail-closed backstop below would surface it — but detect it
			// structurally first to emit a clear, position-specific message (mirroring
			// the EXISTS-in-ON rejection above) rather than leaking the resolver's
			// internal shape string.
			if expr.ContainsSubqueryAtom(sq.joins[sqIdx].onExpr) {
				return api.NewError(api.ErrCodeUnsupportedQuery,
					"subquery in a JOIN ON clause is not supported")
			}
			pred, walkErr := resolver.WalkPredicate(sq.joins[sqIdx].onExpr)
			if walkErr != nil {
				// A recognized semantic / shape error (undefined or ambiguous column,
				// unknown source, bad IN list, a structured api.Error from a non-boolean
				// bare ON predicate like `ON a.amount`, RFC-146) is a real user error —
				// surface it with its correct SQLSTATE rather than dropping the ON
				// condition, which the translator silently degrades to a cross join (it
				// ignores OnText once OnPredicate is nil).
				if apiErr := mapPredicateWalkError(walkErr); apiErr != nil {
					return apiErr
				}
				// FAIL-CLOSED: any other resolver failure means we could not build this
				// ON predicate (e.g. an UnsupportedExpressionShapeError from a shape this
				// resolver has no planner for). Dropping it is NEVER safe — it degrades
				// the join to a CROSS PRODUCT (silent wrong rows, the pre-existing bug in
				// TODO.md "Known gaps"). Surface a clean error instead of the historical
				// silent `continue`.
				return api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"unsupported expression in JOIN ON clause: %v", walkErr)
			}
			j.OnPredicate = predicates.SimplifyPredicateValues(pred)
		}
	}
	return nil
}

// buildWherePredicateFromCTEScope builds a predicate using a CTE-derived
// ScopeSource. Used when the main query's FROM references a CTE — the
// CTE's column schema was derived from its body's SELECT projection and
// the underlying table's metadata.
func buildWherePredicateFromCTEScope(
	src semantic.ScopeSource,
	tableAlias string,
	whereExpr antlrgen.IWhereExprContext,
	md *recordlayer.RecordMetaData,
) (predicates.QueryPredicate, bool) {
	if whereExpr == nil || whereExpr.Expression() == nil || md == nil {
		return nil, false
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	scope := semantic.NewScope(nil)
	if tableAlias != "" {
		src = cteSourceAs(src, semantic.FromNormalized(tableAlias), tableAlias)
	}
	if err := scope.AddSource(src); err != nil {
		return nil, false
	}
	resolver := expr.New(analyzer, scope)
	pred, err := resolver.WalkPredicate(whereExpr.Expression())
	if err != nil {
		return nil, false
	}
	pred = predicates.SimplifyPredicateValues(pred)
	return pred, true
}

// buildCTEColumnSource derives the ScopeSource a CTE body publishes to the
// enclosing query: the row the body really emits, name for name, ordinal for
// ordinal. Ordinary, joined and computed bodies are built and publish their
// exact result row separately from their semantic names, so the two spellings of one
// body resolve identically. A repeated output name is published as stated:
// the semantic scope counts every same-named column of one source as a
// separate candidate, so a reader that names it reports 42702 in every read
// path (measured across SELECT, WHERE, ON, ORDER BY, GROUP BY, HAVING, EXISTS
// and a scalar subquery, byte-identical to the derived-table form). Declining
// the registration instead never made the reader loud: the CTE fell to the
// ON-only class and a reference to the repeated name bound whichever
// duplicate that class happened to find first.
func buildCTEColumnSource(
	md *recordlayer.RecordMetaData,
	cteName string,
	cteQuery antlrgen.IQueryContext,
	priorCTEs map[string]semantic.ScopeSource,
) (semantic.ScopeSource, bool, error) {
	if md == nil || cteName == "" || cteQuery == nil {
		return semantic.ScopeSource{}, false, nil
	}
	// A NESTED WITH on the body (`c2 AS (WITH c3 … SELECT … FROM c3)`): the
	// body's FROM names resolve against the nested CTEs FIRST (lexical
	// scoping — the same shadowing the plan build applies via
	// buildCTEBodyQuery). Derive each nested CTE's schema recursively into a
	// SCOPED extension of priorCTEs (declaration order, so a later nested CTE
	// sees an earlier one) and resolve the body against that. Without this the
	// registration declined (body table `c3` unknown), the enclosing CTE fell
	// to the ON-only class, and every later NAMED read of it failed to plan.
	if ctes := cteQuery.Ctes(); ctes != nil {
		scoped := make(map[string]semantic.ScopeSource, len(priorCTEs)+2)
		for k, vv := range priorCTEs {
			scoped[k] = vv
		}
		for _, nq := range ctes.AllNamedQuery() {
			nname := functions.FullIdToName(nq.GetName())
			if src, ok, nestedErr := buildCTEColumnSource(md, nname, nq.Query(), scoped); nestedErr != nil {
				return semantic.ScopeSource{}, false, nestedErr
			} else if ok {
				scoped[strings.ToUpper(nname)] = applyCTEColumnAliases(src, nq.GetColumnAliases())
			} else {
				// A DECLARED nested name SHADOWS an outer same-name CTE even
				// when its schema is not derivable (join-shaped body):
				// leaving the cloned outer entry in place validated the
				// enclosing body against the OUTER schema and baked its
				// ordinals over the inner's row — silent wrong slot. A
				// TOMBSTONE (nil Table), not deletion: absence falls back to
				// the CATALOG, and a same-named base table would bind its
				// ordinals onto the CTE's rows just as silently. The
				// tombstone hard-declines both resolution paths.
				scoped[strings.ToUpper(nname)] = semantic.ScopeSource{}
			}
		}
		priorCTEs = scoped
	}
	// The CTE body is either a simple QueryTermDefault (non-recursive) or a
	// SetQuery / UNION ALL (recursive). For recursive CTEs, derive the column
	// schema from the seed (left) branch of the UNION.
	var body *antlrgen.QueryTermDefaultContext
	switch b := cteQuery.QueryExpressionBody().(type) {
	case *antlrgen.QueryTermDefaultContext:
		body = b
	case *antlrgen.SetQueryContext:
		seed, ok := b.GetLeft().(*antlrgen.QueryTermDefaultContext)
		if !ok {
			return semantic.ScopeSource{}, false, nil
		}
		body = seed
	default:
		return semantic.ScopeSource{}, false, nil
	}
	innerSQ, err := extractFromQueryTerm(body)
	if err != nil || innerSQ == nil {
		return semantic.ScopeSource{}, false, nil
	}
	// Join and UNNEST bodies publish their final projected row. Star expansion
	// has already separated SQL attributes from internal seed slots, so the
	// exact body schema also preserves ambiguous duplicate output labels.
	if innerSQ.derivedQuery != nil ||
		len(innerSQ.joins) > 0 ||
		innerSQ.tableName == "" {
		// A JOIN/derived/lateral-unnest-legged body has no schema a NAME-keyed
		// walk of its FROM legs can derive: such a walk advertises a PARTIAL
		// row, and a partial row turns an ambiguous reference into a silent
		// bind. Build the body instead and publish its EXACT result type — the
		// same row execution flows, so every admitted name binds the ordinal
		// the body really emits and there is no partial schema to mis-bind
		// against. Repeated output names included: a row with two columns
		// named G is published with both, and the reader's own ambiguity
		// check answers 42702 for either spelling of the body. The uniqueness
		// gate this arm once had was the silent bind it claimed to prevent:
		// the declined CTE fell to the ON-only class, and u.g then bound one
		// duplicate in SELECT and the other in ORDER BY. A body whose row has
		// a shape semantic.Column cannot carry losslessly declines: the
		// enclosing join's ON clause then reads the separate cteOnScopes marker
		// (registerCTEOnOnlyScope) and goes LOUD on drop risk, never a silent
		// ON drop. A body that does not BUILD raises its OWN error instead —
		// the mistake is inside the CTE, and reporting it as the reader's
		// generic drop-risk names the wrong query.
		//
		// The parsed projection is the output-NAME authority, exactly as it is
		// for the derived-table spelling of the same body: the exact derivation
		// labels a qualified reference by its datum key (`ga.g` is GA.G there),
		// and under that label the row `SELECT ga.g, c.id AS g` carried GA.G and
		// G — two distinct names for what SQL calls G twice, so u.g bound the
		// second and never met the ambiguity check. A star body spells no
		// projection and keeps the derivation's labels.
		src, ok, bodyErr := buildExactScopeSourceOrBodyError(md, cteName, innerSQ, priorCTEs, projectionOutputNames(innerSQ))
		if bodyErr != nil {
			return semantic.ScopeSource{}, false, bodyErr
		}
		if !ok || exactStarRowCarriesAnEphemeral(innerSQ, src) {
			return semantic.ScopeSource{}, false, nil
		}
		return src, true, nil
	}
	if len(innerSQ.aggCols) > 0 || innerSQ.countStar {
		// Same order as the derived-table path (buildDerivedTableSourceWithCTEs):
		// build the body and publish its EXACT row first, and only fall back to
		// the parse-tree derivation when the exact one has nothing to publish.
		// The parse-tree derivation types an aggregate from its argument's
		// CATALOG column, so an expression argument (`SUM(v * 2) AS s`) came out
		// UNKNOWN, and a reader binding `u.s` by plan-time ordinal then found a
		// source that could not state its row — a CTE that failed as a join leg
		// while the identical body worked as a derived table. A body that does
		// not build raises its own error, exactly as the join-bodied arm above.
		// aggOutputCols is the output-name authority, as it is for the
		// derived-table spelling (buildDerivedTableSourceFromTerm), so a
		// grouping key spelled `ga.g` is published as G in both forms.
		aggColumns := aggOutputCols(innerSQ, md)
		aggNames := make([]string, len(aggColumns))
		for i, column := range aggColumns {
			aggNames[i] = column.name
		}
		src, ok, bodyErr := buildExactScopeSourceOrBodyError(md, cteName, innerSQ, priorCTEs, aggNames)
		if bodyErr != nil {
			return semantic.ScopeSource{}, false, bodyErr
		}
		if ok {
			// Published as the exact derivation states it, repeated output
			// names included: a reader that names a repeated column meets the
			// row's own ambiguity check and reports 42702, Java's
			// AMBIGUOUS_COLUMN — measured, and the same answer the derived-table
			// form of the body gives. A registration-time decline never made the
			// reader loud: measured on the join-bodied arm before it published
			// its row, the declined CTE bound one duplicate in SELECT and the
			// other in ORDER BY. The parse-tree fallback below is reached only for
			// a row the exact derivation could not carry, never for one it
			// published.
			return src, true, nil
		}
		src, ok = buildDerivedTableSourceFromAgg(cteName, innerSQ, md)
		if !ok {
			return semantic.ScopeSource{}, false, nil
		}
		return src, true, nil
	}
	// A simple SELECT owns a new output row too: repeated references,
	// renames, and stars over earlier CTEs need the same exact publication as
	// joined/computed bodies. Copying only the input's SQL columns loses the
	// physical field names and binds consumers to a row no plan emits.
	return buildExactScopeSourceOrBodyError(
		md, cteName, innerSQ, priorCTEs, projectionOutputNames(innerSQ),
	)
}

// buildCTEOnOnlySource derives the ON-RESOLUTION-ONLY ScopeSource for a
// declared CTE that buildCTEColumnSource keeps OUT of the global cteScopes (a
// join/lateral-unnest-legged body — see the decline comment there). It is
// registered in the separate cteOnScopes map at WITH registration. An enclosing
// explicit join's ON resolves against it through upgradeJoinOnPredicates. A
// complete entry can also be admitted locally by singleSourceQueryBlockCTEScopes
// when that CTE is the query block's sole source; multi-leg blocks retain the
// clean decline that prevents flatten-evasion misbinding.
//
// Output-name authority (must match what execution actually EMITS, or the
// fabricated "CTE.col" merge keys miss): an explicit projection alias
// (executeProjection always writes the alias key) or a BARE unqualified
// non-computed reference (the runtime key mirrors the SQL spelling — a bare
// ref plans as Project([AID],…) and keys bare). The bare-ref arm additionally
// requires every FROM leg to be ENUMERABLE — a base table, a DERIVABLE CTE
// (the resolver sees those via addSource's cteScopes fallback), or a lateral
// unnest leg (binds one alias via the unnest source adder): the ambiguity
// backstop (the body build 42702s an ambiguous bare ref before it can
// execute) only holds when the resolver can see every leg's columns. A
// derived-table leg among several hides its columns from that check, so a
// textually-bare-but-ambiguous ref would silently resolve against the wrong
// leg (review-caught, pinned by Q18) — and an ON-ONLY CTE leg is worse:
// buildSelectScope hands the body a NIL resolver, which kills the 42703
// unknown-column gate along with the ambiguity gate (review-caught,
// Q27/Q28). Bodies whose single source IS a derived
// table stay derivable, but every projection/aggregate INPUT read must
// resolve in the derived source's provably-readable name set
// (derivedEmittedBareNames): a join-shaped derived row keys by the INNER
// spelling, so an inner qualified-spelled item makes an outer `D.col` read a
// runtime malformed-plan failure (and an aggregate over it a silent NULL) —
// decline to the plan-time marker instead (Q19/Q20). A single-BASE-TABLE
// inner stays on the POSITIONAL frontier, where qualified items are readable
// by last segment (review-caught over-decline, Q33). Everything else DECLINES
// to the loud marker:
//   - an unaliased QUALIFIED reference resolves to a FieldValue whose Field
//     is the dotted source name ("D.ID" — see values.ProjectionColumnName),
//     so the row carries no bare key and an advertised bare name would read
//     a column the merged row never has;
//   - `WITH c(x, y)` column aliases rename the SCOPE view only — the runtime
//     row still keys by the body's own output names, so resolving `c.x` here
//     would turn today's loud 42703 into a silent runtime miss (worse);
//   - computed items without an alias key by their explain rendering.
//
// Aggregate bodies derive via buildDerivedTableSourceFromAgg (agg outputs key
// by their canonical names at runtime — the existing derived-table pathway).
// Columns type UNKNOWN/nullable (the same precedent — the scope needs NAMES,
// not exact types). A false return means the caller registers a nil-Table
// MARKER instead: the declared name still routes to the loud drop-risk 0AF00,
// never a silent ON drop. Widening the derivable set (qualified/renamed
// output schemas) is booked with the derived-table-twin item.
// cteScopePreState snapshots a name's scope-map state as it was BEFORE the
// CTE's own registration — what SQL scoping says the body sees: outer
// scopes and earlier siblings, never itself. had=false is the common case
// (the name was absent); the preserved VALUE is the nested-shadowing case —
// a subquery WITH reusing an OUTER CTE's name overwrites the level map's
// outer entry at registration, and a plain self-DELETE then lost BOTH
// bindings, sending the inner body's reads to the base table (42703 on the
// outer CTE's own column, review-caught).
type cteScopePreState struct {
	scopeVal semantic.ScopeSource
	scopeHad bool
	onVal    semantic.ScopeSource
	onHad    bool
}

// buildCTEBodySelfHidden runs a CTE body build with the CTE's name mapped
// to its PRE-REGISTRATION state in both scope maps: non-recursive SQL
// scoping makes `FROM <own-name>` inside the body the outer binding (an
// enclosing CTE) or the TABLE — never the CTE being defined. With CTE-FIRST
// scope resolution a visible self entry resolves the body against its own
// OUTPUT schema — on the chain paths that surfaced as a bogus
// correlated-fallback misroute AND a silent base-table value substitution
// through BuildScalar's 42703 arm; on the visitor path the R5a shadow pin
// caught it (review-caught on all three, one shared helper so the pipelines
// cannot diverge again). pre carries the pre-registration snapshots (nil ⇒
// absent for every name — the top-level visitor case). Recursive bodies
// keep self visible — their union machinery consumes the self-reference.
// Restores are deferred (error-path safe).
func buildCTEBodySelfHidden(
	cteScopes, cteOnScopes map[string]semantic.ScopeSource,
	upper string,
	pre map[string]cteScopePreState,
	recursive bool,
	build func() (logical.LogicalOperator, error),
) (logical.LogicalOperator, error) {
	if !recursive {
		st := pre[upper] // zero value: absent in both maps pre-registration
		if cur, ok := cteScopes[upper]; ok || st.scopeHad {
			if st.scopeHad {
				cteScopes[upper] = st.scopeVal
			} else {
				delete(cteScopes, upper)
			}
			defer func() {
				if ok {
					cteScopes[upper] = cur
				} else {
					delete(cteScopes, upper)
				}
			}()
		}
		if cteOnScopes != nil {
			if cur, ok := cteOnScopes[upper]; ok || st.onHad {
				if st.onHad {
					cteOnScopes[upper] = st.onVal
				} else {
					delete(cteOnScopes, upper)
				}
				defer func() {
					if ok {
						cteOnScopes[upper] = cur
					} else {
						delete(cteOnScopes, upper)
					}
				}()
			}
		}
	}
	return build()
}

// cteLegKind classifies a NAMED FROM leg of a CTE ON-only body by what
// EXECUTION will resolve it to. Declared CTE names come FIRST — a CTE
// shadows a same-named catalog table (review-caught: a metadata-first lookup
// classified a shadowed leg by the TABLE's schema while runtime rows came
// from the CTE). cteLegOpaque: an ON-ONLY CTE name (or unknown) — addSource
// returns false and buildSelectScope hands the body a NIL resolver, which
// skips BOTH the 42702 ambiguity gate and the 42703 unknown-column gate for
// the WHOLE body (the backstop every bare-ref admission rests on).
// cteLegDerivableCTE: a DERIVABLE CTE — addSource falls back to cteScopes,
// so the resolver still sees its columns. cteLegBase: a base table — the
// analyzer resolves it (the same ResolveTable call addSource makes), or the
// active-schema-qualified form of one (this derivation runs at WITH
// registration, BEFORE normalizeSchemaQualifiedSelectSources strips the
// schema segment — mirror that strip or valid "s"."T" legs classify opaque,
// review-caught).
type cteLegKindT int

const (
	cteLegOpaque cteLegKindT = iota
	cteLegBase
	cteLegDerivableCTE
)

func cteLegKind(md *recordlayer.RecordMetaData, schemaName string, cteScopes, cteOnScopes map[string]semantic.ScopeSource, name string) cteLegKindT {
	if name == "" || md == nil {
		return cteLegOpaque
	}
	upper := strings.ToUpper(name)
	if _, on := cteOnScopes[upper]; on {
		return cteLegOpaque
	}
	if _, ok := cteScopes[upper]; ok {
		return cteLegDerivableCTE
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	if _, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(name, "."), false)); err == nil {
		return cteLegBase
	}
	if segs := strings.Split(name, "."); len(segs) == 2 && newUnnestTableResolver(md, schemaName)(segs) {
		return cteLegBase
	}
	return cteLegOpaque
}

// cteBodyLegsEnumerable reports whether every named FROM leg of a multi-leg
// body is visible to the resolver (base table or derivable CTE) — the
// precondition for the 42702/42703 backstop the bare-ref admission relies
// on. Comma legs classified as lateral unnests (segments[0] names a prior
// source alias — RFC-142 R5: typed segments, never a tableName re-split) are
// enumerable by construction: the element alias binds one name and
// buildSelectScope adds it via the unnest source adder. An unnest leg's
// binding name is its EFFECTIVE alias (unnestAliases: the explicit AS, else
// the last segment) — recording the flattened dotted name instead broke
// chained no-AS unnests (`FROM T4, T4.SARR, SARR.SUB AS Y`: the scope
// exposes SARR, review-caught). Derived legs are the caller's decline, not
// this check's.
func cteBodyLegsEnumerable(md *recordlayer.RecordMetaData, schemaName string, cteScopes, cteOnScopes map[string]semantic.ScopeSource, sq *selectQuery) bool {
	if sq.derivedQuery == nil && cteLegKind(md, schemaName, cteScopes, cteOnScopes, sq.tableName) == cteLegOpaque {
		return false
	}
	tableFirst := newUnnestTableResolver(md, schemaName)
	prior := map[string]bool{strings.ToUpper(sq.tableAlias): true}
	for _, jc := range sq.joins {
		bind := jc.alias
		if bind == "" {
			bind = jc.tableName
		}
		if jc.derivedQuery == nil {
			priorHit := len(jc.segments) > 1 && prior[strings.ToUpper(jc.segments[0])]
			switch {
			case priorHit && len(jc.segments) == 2 && tableFirst(jc.segments):
				// ALIAS-EQUALS-SCHEMA collision: buildSelectScope keeps its
				// nil-resolver leniency for this class (the R5b Java-parity
				// pins), so the 42702/42703 backstop is DEAD for the body —
				// the enumerability premise fails; decline to the marker.
				return false
			case jc.fromComma && priorHit:
				// genuine lateral unnest: binds its effective alias
				if as, _ := unnestAliases(jc); as != "" {
					bind = as
				}
			case cteLegKind(md, schemaName, cteScopes, cteOnScopes, jc.tableName) == cteLegOpaque:
				return false
			}
		}
		prior[strings.ToUpper(bind)] = true
	}
	return true
}

// derivedEmittedBareNames computes the set of names a derived source's
// runtime row provably answers reads for — the read-authority for a CTE
// ON-only body whose single FROM source is that derived table. ok=false
// means the set is not statically closed and the caller must decline to the
// loud marker: SELECT * (names unknown here — no catalog access),
// aggregate/set-query bodies (their materialized-row keying is unverified on
// this path), a derived leg among multiple legs (the same ambiguity-backstop
// hole as the caller's own arm, one level down), any OPAQUE leg (an ON-only
// CTE gives the body build a NIL resolver — no 42702/42703 backstop), or an
// opaque/ON-only SINGLE source. The per-item rules mirror the caller's
// admission loop: an explicit alias is always emitted (executeProjection
// writes the alias key); a bare unqualified non-computed ref keys by its
// spelling; a QUALIFIED-spelled item over a single-BASE-TABLE body is
// readable by its LAST SEGMENT — and not merely because that body's
// projection row stays positional: the resolver's SINGLE-SOURCE resolution
// rewrites the projected FieldValue's Field to the BARE name at build time
// (expr.go ResolveIdentifier, needsQualification = len(sources) > 1; pinned
// by TestWalkExpression_SingleVsMultiSourceFieldQualification), so the key
// is bare in BOTH representations — the positional row AND the name-keyed
// Datum — which is what lets the claim survive a sort-continuation resume
// that rebuilds rows without positional state (review-verified: only
// join/merge-shaped inner rows are name-keyed; declining qualified items
// here over-declined the positional class). Computed unaliased items key by
// their explain rendering — nothing readable. Input reads recurse: when this
// level's single source is itself derived, every item's read target must
// resolve in the deeper set, else the body can never execute (a decline
// beats the runtime malformed-plan error it would otherwise be); a
// scalar-subquery's LOCAL refs are excluded from that check
// (harvestColumnRefsOutsideSubqueries) — its own build resolves them in its
// own scope, and a correlated read into the derived source surfaces loud at
// translation.
func derivedEmittedBareNames(md *recordlayer.RecordMetaData, schemaName string, cteScopes, cteOnScopes map[string]semantic.ScopeSource, q antlrgen.IQueryContext) (map[string]bool, bool) {
	if q == nil {
		return nil, false
	}
	body, ok := q.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return nil, false
	}
	sq, err := extractFromQueryTerm(body)
	if err != nil || sq == nil {
		return nil, false
	}
	if len(sq.aggCols) > 0 || sq.countStar || sq.projCols == nil {
		return nil, false
	}
	legDerived := sq.derivedQuery != nil
	for _, jc := range sq.joins {
		if jc.derivedQuery != nil {
			legDerived = true
		}
	}
	if len(sq.joins) > 0 && (legDerived || !cteBodyLegsEnumerable(md, schemaName, cteScopes, cteOnScopes, sq)) {
		return nil, false
	}
	positionalFrontier := false
	if sq.derivedQuery == nil && len(sq.joins) == 0 {
		switch cteLegKind(md, schemaName, cteScopes, cteOnScopes, sq.tableName) {
		case cteLegBase:
			positionalFrontier = true
		case cteLegDerivableCTE:
			// derivable CTE rows answer alias/bare-spelling reads (the same
			// contract this function claims); NOT known-positional.
		default:
			return nil, false // ON-only CTE or unknown single source: opaque
		}
	}
	var deeper map[string]bool
	if sq.derivedQuery != nil {
		if deeper, ok = derivedEmittedBareNames(md, schemaName, cteScopes, cteOnScopes, sq.derivedQuery); !ok {
			return nil, false
		}
	}
	set := make(map[string]bool, len(sq.projCols))
	for i, col := range sq.projCols {
		isComputed := i < len(sq.projExprs) && sq.projExprs[i] != nil
		if deeper != nil {
			if isComputed {
				for _, r := range harvestBareColumnRefsOutsideSubqueries(sq.projExprs[i]) {
					if !deeper[r] {
						return nil, false
					}
				}
			} else if !deeper[colBareOrName(col)] {
				return nil, false
			}
		}
		switch {
		case i < len(sq.projAliases) && sq.projAliases[i] != "":
			set[sq.projAliases[i]] = true
		case isComputed:
			// unaliased computed: keys by its rendering — nothing readable
		case !col.qualified && col.bare != "":
			// the "" guard: a mixed-star sentinel slot (name=="") must
			// not deposit a junk claim in a soundness-critical set
			set[col.bare] = true
		case col.qualified && positionalFrontier:
			set[col.bare] = true
		}
	}
	return set, true
}

// cteBodyReadsResolvable reports whether every projection and aggregate INPUT
// read of a single-derived-source CTE body resolves in the derived source's
// emitted bare-key set. Aggregate outExpr entries are skipped: their refs
// read the POST-aggregation rowMap (agg outputs and group columns), not the
// input row — the group/arg inputs they depend on arrive via sibling aggCols
// entries, which ARE checked.
func cteBodyReadsResolvable(sq *selectQuery, emitted map[string]bool) bool {
	for _, ac := range sq.aggCols {
		if ac.groupCol != "" {
			bare := ac.groupColBare
			if bare == "" {
				bare = ac.groupCol
			}
			if !emitted[bare] {
				return false
			}
		}
		if ac.aggArg != "" {
			bare := ac.aggArgBare
			if bare == "" {
				bare = ac.aggArg
			}
			if !emitted[bare] {
				return false
			}
		}
		for _, r := range harvestBareColumnRefsOutsideSubqueries(ac.aggExpr) {
			if !emitted[r] {
				return false
			}
		}
	}
	for i, col := range sq.projCols {
		if i < len(sq.projExprs) && sq.projExprs[i] != nil {
			for _, r := range harvestBareColumnRefsOutsideSubqueries(sq.projExprs[i]) {
				if !emitted[r] {
					return false
				}
			}
			continue
		}
		if !emitted[colBareOrName(col)] {
			return false
		}
	}
	return true
}

func buildCTEOnOnlySource(
	cteName string,
	cteQuery antlrgen.IQueryContext,
	colAliases antlrgen.IFullIdListContext,
	md *recordlayer.RecordMetaData,
	schemaName string,
	cteScopes map[string]semantic.ScopeSource,
	cteOnScopes map[string]semantic.ScopeSource,
) (semantic.ScopeSource, bool) {
	if cteName == "" || cteQuery == nil {
		return semantic.ScopeSource{}, false
	}
	if colAliases != nil {
		// WITH c(x, y) renames are scope-level only; the runtime row keeps the
		// body's keys — decline to the loud marker rather than resolve names
		// the merged row will never carry.
		return semantic.ScopeSource{}, false
	}
	var body *antlrgen.QueryTermDefaultContext
	switch b := cteQuery.QueryExpressionBody().(type) {
	case *antlrgen.QueryTermDefaultContext:
		body = b
	case *antlrgen.SetQueryContext:
		seed, ok := b.GetLeft().(*antlrgen.QueryTermDefaultContext)
		if !ok {
			return semantic.ScopeSource{}, false
		}
		body = seed
	default:
		return semantic.ScopeSource{}, false
	}
	innerSQ, err := extractFromQueryTerm(body)
	if err != nil || innerSQ == nil {
		return semantic.ScopeSource{}, false
	}
	if len(innerSQ.joins) == 0 && innerSQ.derivedQuery == nil {
		// Plain single-table bodies are buildCTEColumnSource territory; if
		// THAT declined, this name-only derivation has nothing better to
		// offer. Derived-source bodies (`FROM (SELECT …) d` — zero joins but
		// declined globally for the derivedQuery reason) DO derive here: their
		// projection names key the runtime row the same way.
		return semantic.ScopeSource{}, false
	}
	legDerived := innerSQ.derivedQuery != nil
	for _, jc := range innerSQ.joins {
		if jc.derivedQuery != nil {
			legDerived = true
		}
	}
	if len(innerSQ.joins) > 0 && legDerived {
		// A derived-table leg among MULTIPLE legs: the resolver cannot
		// enumerate its columns, so the 42702 ambiguity backstop the bare-ref
		// arm rests on does not run — a textually-bare-but-ambiguous ref
		// silently resolves against the wrong leg (Q18). Decline the whole
		// body to the loud marker.
		return semantic.ScopeSource{}, false
	}
	if len(innerSQ.joins) > 0 && !cteBodyLegsEnumerable(md, schemaName, cteScopes, cteOnScopes, innerSQ) {
		// An OPAQUE leg — an ON-ONLY CTE name — is worse than a derived leg:
		// buildSelectScope's addSource knows base tables and cteScopes only,
		// so the body gets a NIL resolver and BOTH the 42702 ambiguity gate
		// and the 42703 unknown-column gate are skipped for the whole body
		// (review-caught: an ambiguous bare ref AND a nonexistent column both
		// planned fine). Decline — covers the aggregate arm below too (Q27,
		// Q28).
		return semantic.ScopeSource{}, false
	}
	var innerEmitted map[string]bool
	if innerSQ.derivedQuery != nil {
		// Single derived source: the runtime row keys by the INNER spelling.
		// Every input read below must resolve in its provably-emitted set —
		// a miss is a runtime malformed plan (projection read, Q19) or a
		// silent NULL (aggregate arg, Q20) if admitted.
		var ok bool
		if innerEmitted, ok = derivedEmittedBareNames(md, schemaName, cteScopes, cteOnScopes, innerSQ.derivedQuery); !ok {
			return semantic.ScopeSource{}, false
		}
	}
	if innerEmitted != nil && !cteBodyReadsResolvable(innerSQ, innerEmitted) {
		return semantic.ScopeSource{}, false
	}
	if len(innerSQ.aggCols) > 0 || innerSQ.countStar {
		// COMPLETE-SCHEMA-OR-DECLINE applies here too: a DUPLICATE output name
		// would silently mis-resolve an enclosing ON ref by first-matching one
		// of the two. Decline the whole source on that obstruction, exactly
		// like the projection path below. The dup check consumes aggOutputCols
		// — the SAME visible-only authority buildDerivedTableSourceFromAgg
		// builds from — so it counts exactly the names installed (a hidden
		// HAVING aggregate is neither advertised nor counted).
		//
		// The CASE-SENSITIVITY obstruction that used to sit beside it is
		// RETIRED with the fold it was built on: an output name is now emitted
		// verbatim, so a quoted `AS "x"` is nameable and only a genuine
		// repetition is ambiguous. The count is keyed verbatim for the same
		// reason — `AS "x"` and `AS "X"` are two columns, not one collision.
		aggSeen := make(map[string]int)
		for _, c := range aggOutputCols(innerSQ, md) {
			aggSeen[c.name]++
		}
		for _, n := range aggSeen {
			if n > 1 {
				return semantic.ScopeSource{}, false
			}
		}
		return buildDerivedTableSourceFromAgg(cteName, innerSQ, md)
	}
	if innerSQ.projCols == nil {
		return semantic.ScopeSource{}, false // SELECT * over a multi-leg/derived body: no name authority
	}
	// COMPLETE-SCHEMA-OR-DECLINE. This schema is installed as ONE source of the
	// enclosing join; the resolver decides bare-ref ambiguity by which SOURCES
	// carry a name (scope.ResolveColumn). A PARTIAL install — advertising some
	// runtime columns and dropping others — is therefore UNSOUND: a dropped
	// column whose runtime key another enclosing source ALSO carries would let a
	// bare ref bind silently to that other source (the ref should be ambiguous),
	// and this function cannot see the enclosing scope to know. So we install
	// ONLY when every runtime column is advertised correctly and unambiguously;
	// any obstruction declines the WHOLE source (caller's loud 0AF00), never a
	// partial table. Two obstructions, each keyed by the RUNTIME-emitted name
	// (executeProjection uppercases every output key):
	//   (1) a quoted CASE-SENSITIVE alias (`AS "x"`, outName != its fold): the
	//       runtime key is "X" but no correct-case ref can name it (a `C."x"`
	//       plans then runtime-fails against the uppercased row; a `C."X"`
	//       silently resolves the wrong case). Can't advertise it truthfully.
	//   (2) a DUPLICATE runtime name (`… AS X, … AS X`, or `AS "x", AS "X"` —
	//       both emit "X"): the schema is AMBIGUOUS on that name; advertising one
	//       column silently joins on an arbitrary one and, when dropped, rebinds.
	// (A partial "keep the unique columns, drop the bad one" was tried and is
	// unsound for the rebind reason above — review-caught. The full-reach fix —
	// keep unique columns AND make the bad name resolve ambiguous via a
	// per-source poison marker in the resolver — is a booked conformance slice;
	// until then a body with ANY obstruction declines wholesale, correct-or-loud.)
	names := make([]string, 0, len(innerSQ.projCols))
	seen := make(map[string]int, len(innerSQ.projCols))
	for i, col := range innerSQ.projCols {
		// The output name must be one execution PROVABLY emits: the explicit
		// alias (executeProjection always writes the alias key), or a BARE
		// unqualified non-computed reference (the runtime key mirrors the SQL
		// spelling — a bare ref keys bare, verified by plan shape
		// Project([AID],…)). The bare arm is sound here because every leg is
		// enumerable at this point — multi-leg bodies with a derived leg
		// declined above, so an ambiguous bare ref never EXECUTES: the body
		// build 42702s it and the wrap rebuild re-raises the swallowed error.
		// A QUALIFIED unaliased ref keys by its dotted source name and a
		// computed item by its explain rendering — both decline (no bare key
		// on the runtime row).
		outName := ""
		if i < len(innerSQ.projAliases) && innerSQ.projAliases[i] != "" {
			outName = innerSQ.projAliases[i]
		} else {
			isComputed := i < len(innerSQ.projExprs) && innerSQ.projExprs[i] != nil
			if !isComputed && !col.qualified && col.bare != "" {
				outName = col.bare
			}
		}
		if outName == "" {
			return semantic.ScopeSource{}, false
		}
		// The runtime name is the output name VERBATIM. Obstruction (1) — a
		// quoted alias whose fold differs from itself — is RETIRED: it existed
		// because execution keyed its output slots upper-cased, so `AS "x"`
		// emitted X and no reference could name it. Nothing folds an output
		// name any more, so `AS "x"` emits x and `C."x"` resolves; the gate
		// would now decline a source that works.
		//
		// Obstruction (2) survives, and its counting changes with it: two
		// aliases that differ only by case are two DISTINCT columns now, not
		// one ambiguous name, so the count is keyed verbatim.
		seen[outName]++
		names = append(names, outName)
	}
	for _, n := range seen {
		if n > 1 { // obstruction (2): duplicate runtime name
			return semantic.ScopeSource{}, false
		}
	}
	if len(names) == 0 {
		return semantic.ScopeSource{}, false
	}
	// The NAMES are decided above, by what execution provably emits. The TYPES
	// come from the body's exact logical result type — the authority that
	// actually produces the rows — never from a name-keyed walk of the body's
	// legs. That walk had to mint UNKNOWN for every item it could not attribute
	// to a source column (a computed item, an unnest element, an aliasless
	// schema-qualified leg), and a single UNKNOWN field makes the WHOLE
	// published row inexact: resolving ANY column of this source then fails,
	// not merely the unattributed one. Deriving from the built body types the
	// computed items correctly and declines wholesale where it cannot —
	// semanticColumnFromExactType never publishes a placeholder.
	return buildExactVirtualScopeSourceForSelect(md, cteName, innerSQ, cteScopes, names)
}

// registerCTEOnOnlyScope stores the ON-only source (or the nil-Table marker)
// for a declared CTE that did NOT make it into the global cteScopes — the ONE
// registration authority both build pipelines (the plan visitor and the
// CTECatalog chain) share, so a declared CTE can never reach
// upgradeJoinOnPredicates untracked (the silent ON-drop class).
func registerCTEOnOnlyScope(dst map[string]semantic.ScopeSource, upperName string, cteQuery antlrgen.IQueryContext, colAliases antlrgen.IFullIdListContext, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) error {
	// Column-alias arity for underivable bodies is validated at the POINT OF
	// TRUTH instead of here: translateCTE checks the BUILT body's real output
	// width against the alias list (42F10) — a static width predictor at
	// registration kept re-implementing source resolution (stars, shadowing,
	// unnest, nested WITH) and drifting from the real resolver, the exact
	// two-authorities anti-pattern.
	if src, ok := buildCTEOnOnlySource(upperName, cteQuery, colAliases, md, schemaName, cteScopes, dst); ok {
		dst[upperName] = src
		return nil
	}
	dst[upperName] = semantic.ScopeSource{} // marker: declared, underivable → loud drop risk
	return nil
}

// applyCTEColumnAliases renames the columns of a CTE ScopeSource
// according to the explicit column alias list: WITH c1(x, y) AS (...).
// Matches Java's QueryVisitor.visitNamedQuery column-alias handling.
func applyCTEColumnAliases(src semantic.ScopeSource, colAliases antlrgen.IFullIdListContext) semantic.ScopeSource {
	list, ok := colAliases.(*antlrgen.FullIdListContext)
	if !ok || list == nil {
		return src
	}
	aliases := list.AllFullId()
	if len(aliases) == 0 {
		return src
	}
	tbl := src.Table
	if tbl == nil {
		return src
	}
	origCols := tbl.Columns()

	newCols := make([]semantic.Column, len(origCols))
	for i, col := range origCols {
		if i < len(aliases) {
			// The renamed column exposes the explicit CTE column alias as its
			// OUTPUT name — references (a.node) resolve to it verbatim.
			newName := functions.FullIdToName(aliases[i])
			newCols[i] = renameCarriedColumn(col, newName)
		} else {
			newCols[i] = col
		}
	}

	newTable := &semantic.StaticTable{
		TableName:    tbl.Name(),
		TableColumns: newCols,
	}
	// The column-list projection renames the physical row as well as its SQL
	// labels. Preserve each complete type; rebuilding scalar fields loses ARRAY
	// and nominal STRUCT metadata. Repeated labels remain ambiguous in Table,
	// while the record constructor's deduplicated keys name its physical slots.
	src.Table = newTable
	src.FlowedColumns = append([]semantic.Column(nil), newCols...)
	names := make([]string, len(newCols))
	for i, column := range newCols {
		names[i] = column.Id.Name()
	}
	for i, name := range values.DedupFieldNames(names) {
		src.FlowedColumns[i].Id = semantic.FromNormalized(name)
	}
	src.FlowedNullable = false
	return src
}

// buildWherePredicateForJoinsWithCTEScopes is like
// buildWherePredicateForJoins but resolves CTE table references
// using pre-derived column schemas when metadata lookup fails.
func buildWherePredicateForJoinsWithCTEScopes(
	md *recordlayer.RecordMetaData,
	schemaName string,
	sq *selectQuery,
	whereExpr antlrgen.IWhereExprContext,
	cteScopes map[string]semantic.ScopeSource,
) (predicates.QueryPredicate, bool) {
	if md == nil || sq == nil || sq.tableName == "" || whereExpr == nil || whereExpr.Expression() == nil {
		return nil, false
	}
	if selectHasInlineValuesSource(sq) {
		resolver := buildSelectScope(sq, md, schemaName, cteScopes)
		if resolver == nil {
			return nil, false
		}
		pred, err := resolver.WalkPredicate(whereExpr.Expression())
		if err != nil {
			return nil, false
		}
		return predicates.SimplifyPredicateValues(pred), true
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	scope := semantic.NewScope(sq.enclosingScope)

	addSource := func(tableName, alias, bindingID string) bool {
		aliasID := semantic.FromNormalized(alias)
		if alias == "" {
			aliasID = semantic.FromNormalized(tableName)
		}
		// The binding correlation: the parser-minted duplicate-leg id when
		// present, else the alias.
		binding := bindingOrAlias(bindingID, aliasID)
		// Try metadata first, then CTE scopes.
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err == nil {
			return scope.AddSource(semantic.ScopeSource{
				Table:           tbl,
				Alias:           aliasID,
				CorrelationName: binding,
			}) == nil
		}
		if src, found := cteScopes[strings.ToUpper(tableName)]; found && src.Table != nil {
			// found-with-nil-Table is a TOMBSTONE (declared CTE, schema
			// underivable) — decline instead of AddSource(nil) nil-deref.
			return scope.AddSource(cteSourceAs(src, aliasID, binding)) == nil
		}
		return false
	}
	// A lateral array unnest leg is not a real table / CTE — register its virtual
	// element/ordinal source via the SAME shared helpers buildWherePredicateForJoins
	// (the non-CTE twin) uses, so a CTE-bearing query with an unnest WHERE on the
	// element/ordinal resolves here instead of declining and degrading to text. RFC-142.
	addUnnestSource := unnestScopeSourceAdder(scope)
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	if !addSource(sq.tableName, sq.tableAlias, "") {
		return nil, false
	}
	for i, j := range sq.joins {
		visible := visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i], resolvesToTable)
		if isLateralUnnestJoin(j, visible, resolvesToTable) {
			if !addUnnestSource(j) {
				return nil, false
			}
			continue
		}
		if !addSource(j.tableName, j.alias, j.bindingID) {
			return nil, false
		}
	}
	resolver := expr.New(analyzer, scope)
	pred, err := resolver.WalkPredicate(whereExpr.Expression())
	if err != nil {
		return nil, false
	}
	pred = predicates.SimplifyPredicateValues(pred)
	return pred, true
}

// buildWherePredicateForJoins handles the JOIN case: builds a scope
// with one source per (primary table, joined tables) entry, then
// runs the walker. Bare columns ambiguous across sources fail at
// scope resolution → walker returns an error → fall back to text.
// Qualified columns (`Order.price`) resolve via ScopeSource alias.
//
// Each source needs a Table from the catalog. A miss on any one
// declines the whole predicate (the walker would have failed on
// the missing-table column ref anyway).
func buildWherePredicateForJoins(
	md *recordlayer.RecordMetaData,
	schemaName string,
	sq *selectQuery,
	whereExpr antlrgen.IWhereExprContext,
) (predicates.QueryPredicate, bool) {
	if md == nil || sq == nil || sq.tableName == "" || whereExpr == nil || whereExpr.Expression() == nil {
		return nil, false
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	scope := semantic.NewScope(sq.enclosingScope)

	addSource := func(tableName, alias, bindingID string) bool {
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err != nil {
			return false
		}
		aliasID := semantic.FromNormalized(alias)
		if alias == "" {
			aliasID = semantic.FromNormalized(tableName)
		}
		binding := bindingOrAlias(bindingID, aliasID)
		return scope.AddSource(semantic.ScopeSource{
			Table:           tbl,
			Alias:           aliasID,
			CorrelationName: binding,
		}) == nil
	}
	addUnnestSource := unnestScopeSourceAdder(scope)
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	if !addSource(sq.tableName, sq.tableAlias, "") {
		return nil, false
	}
	for i, j := range sq.joins {
		visible := visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i], resolvesToTable)
		if isLateralUnnestJoin(j, visible, resolvesToTable) {
			if !addUnnestSource(j) {
				return nil, false
			}
			continue
		}
		if !addSource(j.tableName, j.alias, j.bindingID) {
			return nil, false
		}
	}
	resolver := expr.New(analyzer, scope)
	pred, err := resolver.WalkPredicate(whereExpr.Expression())
	if err != nil {
		return nil, false
	}
	pred = predicates.SimplifyPredicateValues(pred)
	return pred, true
}

// isLateralUnnestJoin reports whether a joinClause should register a virtual
// unnest scope source in the WHERE/projection scope binding. It delegates to the
// SAME `unnestCandidateShape` predicate the logical lowering
// (lateralUnnestCandidate) uses, with ONE scope-only refinement: a
// schema-qualified TABLE source is NOT registered as an unnest source — it is a
// table cross join (or, with an AT alias, a WRONG_OBJECT_TYPE the demotion pass
// rejects). unnestCandidateShape keeps an AT-on-a-table source as a LogicalUnnest
// so the AT survives to that rejection, but the scope must resolve its columns as
// a table, never an unnest binding. RFC-142.
func isLateralUnnestJoin(j joinClause, visible map[string]struct{}, resolvesToTable tableResolver) bool {
	if j.derivedQuery != nil || j.catalogAwareInnerPlan != nil || j.onExpr != nil {
		return false
	}
	if schemaQualifiedTableUnnest(j, resolvesToTable) {
		return false
	}
	return unnestCandidateShape(j, visible, resolvesToTable)
}

// unnestElementColumn resolves the declared collection through the current SQL
// scope and returns its element column, preserving nested record/enum metadata.
// Nested accessors identify the leaf collection, not the root record. Repetition
// and container nullability are consumed here; stored array elements are non-null.
// A missing or non-array source declines without manufacturing an element type.
func unnestElementColumn(scope *semantic.Scope, j joinClause) (semantic.Column, bool) {
	if scope == nil || len(j.segments) < 2 {
		return semantic.Column{}, false
	}
	path := unnestSemanticPath(scope, j)
	col, _, accessors, err := scope.ResolvePathNested(path)
	if err != nil {
		return semantic.Column{}, false
	}
	// A descent denotes its LEAF; the root is the struct it was reached through.
	if len(accessors) > 0 {
		col = accessors[len(accessors)-1].Col
	}
	if !col.IsArray {
		return semantic.Column{}, false
	}
	// semantic.Column.Type names the ARRAY ELEMENT when IsArray is true. The
	// unnest binding flows that element itself, so repetition and the array's
	// own nullability are consumed here; stored array elements are non-null.
	col.IsArray = false
	col.Nullable = false
	return col, true
}

func unnestElementColumnFromSources(sources []semantic.ScopeSource, j joinClause) (semantic.Column, bool) {
	scope := semantic.NewScope(nil)
	for _, src := range sources {
		if src.Table == nil {
			continue
		}
		_ = scope.AddSource(src)
	}
	return unnestElementColumn(scope, j)
}

// unnestVirtualScopeSourceWithElement publishes a lateral source from its bound
// element declaration. Non-ordinal record elements expose their fields; scalar
// elements expose one column, and WITH ORDINALITY adds a separate integer slot.
// Display names are independent of the parser-carried runtime binding identity.
func unnestVirtualScopeSourceWithElement(j joinClause, element *semantic.Column) (semantic.ScopeSource, bool) {
	// The (AS, AT) pair MUST come from the same normalization the logical
	// lowering uses (unnestAliases) — otherwise the WHERE/projection scope
	// binds the unnest column under the parser's DEFAULTED alias (the joined
	// segment name `T1.ARR1`) while the inner Explode quantifier is bound under
	// the real alias (the AT alias for the AT-only form), so a WHERE-on-ordinal
	// predicate never pushes into the inner Explode filter. RFC-142.
	asAlias, atAlias := unnestAliases(j)
	var cols []semantic.Column
	corr := asAlias
	if asAlias != "" {
		// The virtual column is the element itself. Carry both its exact scalar
		// type and its record fields from the array authority; UNKNOWN is only
		// the honest fallback for name-only callers which lack a scope.
		elemCol := semantic.Column{Id: semantic.FromNormalized(asAlias), Type: "UNKNOWN", Nullable: true}
		if element != nil {
			elemCol = renameCarriedColumn(*element, asAlias)
		}
		if atAlias == "" && elemCol.Type == "RECORD" && !elemCol.IsArray {
			elemCol.Ephemeral = true
			cols = append(cols, elemCol)
			cols = append(cols, elemCol.StructFields...)
		} else {
			cols = append(cols, elemCol)
		}
	}
	if atAlias != "" {
		// The unnest WITH ORDINALITY ordinal is a 1-based, NON-NULL INT
		// (Java's Type.primitiveType(INT, false); the executor yields a 1-based
		// int per element). Register it with the recognized NON-NULL spelling so
		// sqlTypeToCascadesType resolves it to values.NotNullInt — matching the
		// translator's ordinal FieldValue type — and a PROJECT/COMPUTE over the AT
		// alias reports INT, not UNKNOWN. RFC-142.
		cols = append(cols, semantic.Column{Id: semantic.FromNormalized(atAlias), Type: "INT NOT NULL", Nullable: false})
		if corr == "" {
			corr = atAlias
		}
	}
	if corr == "" {
		return semantic.ScopeSource{}, false
	}
	corrID := semantic.FromNormalized(corr)
	virtual := &semantic.StaticTable{
		TableName:    semantic.FromSegments([]string{corr}, false),
		TableColumns: cols,
	}
	var flowedColumns []semantic.Column
	var flowedObject *semantic.Column
	var columnOrdinals []int
	if atAlias == "" && element != nil {
		flowedObject = element
		columnOrdinals = []int{-1}
		if element.Type == "RECORD" && !element.IsArray {
			flowedColumns = element.StructFields
			for i := range element.StructFields {
				columnOrdinals = append(columnOrdinals, i)
			}
		}
	}
	if atAlias != "" {
		// WITH ORDINALITY flows a genuine two-slot record even in the AT-only
		// form. Keep that physical row separate from the SQL-visible virtual
		// table: without AS, slot 0 is intentionally not a resolvable column,
		// while AT still owns physical ordinal 1.
		elementFlowed := semantic.Column{Id: semantic.FromNormalized(values.OrdinalFieldName(0)), Type: "UNKNOWN", Nullable: true}
		if asAlias != "" {
			elementFlowed.Id = semantic.FromNormalized(asAlias)
		}
		if element != nil {
			elementFlowed = renameCarriedColumn(*element, elementFlowed.Id.Name())
		}
		names := logical.UnnestOrdinalityNames(asAlias, atAlias)
		elementFlowed.Id = semantic.FromNormalized(names[0])
		flowedColumns = []semantic.Column{
			elementFlowed,
			{Id: semantic.FromNormalized(names[1]), Type: "INT NOT NULL", Nullable: false},
		}
		if asAlias != "" {
			columnOrdinals = append(columnOrdinals, 0)
		}
		columnOrdinals = append(columnOrdinals, 1)
	}
	return semantic.ScopeSource{
		Table:           virtual,
		Alias:           corrID,
		CorrelationName: logical.UnnestBindingName(j.bindingID, asAlias, atAlias),
		Shadowing:       true,
		FlowedColumns:   flowedColumns,
		FlowedObject:    flowedObject,
		ColumnOrdinals:  columnOrdinals,
	}, true
}

// unnestScopeSourceAdder returns a closure that registers the VIRTUAL scope
// source (unnestVirtualScopeSourceWithElement) for a lateral array unnest into the SELECT
// scope so a WHERE / projection / ORDER BY reference to the AS/AT column
// resolves (RFC-142).
func unnestScopeSourceAdder(scope *semantic.Scope) func(j joinClause) bool {
	return func(j joinClause) bool {
		element, typed := unnestElementColumn(scope, j)
		var elementPtr *semantic.Column
		if typed {
			elementPtr = &element
		}
		src, ok := unnestVirtualScopeSourceWithElement(j, elementPtr)
		if !ok {
			return false
		}
		return scope.AddSource(src) == nil
	}
}

// buildLogicalPlanForSelectWithCatalog is the catalog-aware variant
// of buildLogicalPlanForSelect. It walks the WHERE predicate through
// the expr package and attaches a predicates.QueryPredicate tree to
// LogicalFilter when the walker succeeds; on any walker failure the
// filter falls back to the canonical source text (identical output
// to buildLogicalPlanForSelect for the WHERE shape alone).
//
// All non-WHERE operators (Scan / Join / Aggregate / Sort / Limit /
// Project) are identical to the text-only builder — only the
// LogicalFilter node differs when the walker succeeds. Passing md=nil
// is equivalent to calling buildLogicalPlanForSelect: every WHERE
// degrades to text.
func buildLogicalPlanForSelectWithCatalog(sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string) (logical.LogicalOperator, error) {
	return buildLogicalPlanForSelectWithCTECatalog(sq, md, schemaName, nil, nil)
}

func buildLogicalPlanForSelectWithCTECatalog(sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource, cteOnScopes map[string]semantic.ScopeSource) (logical.LogicalOperator, error) {
	if schemaName == "" {
		schemaName = defaultEmbeddedSchema
	}
	if sq == nil {
		return nil, nil
	}
	visitor := NewPlanVisitorWithSchema(md, schemaName)
	visitor.enclosingScope = sq.enclosingScope
	visitor.cteScopes, visitor.cteOnScopes = maps.Clone(cteScopes), maps.Clone(cteOnScopes)
	fs := &fromSource{
		tableName: sq.tableName, tableAlias: sq.tableAlias, bindingID: sq.bindingID,
		derivedQuery: sq.derivedQuery, catalogAwareInnerPlan: sq.catalogAwareInnerPlan,
		joins: sq.joins, enclosingScope: sq.enclosingScope,
	}
	visitor.assignDerivedSourceBindings(fs)
	if err := visitor.prepareDerivedSourceBodies(fs); err != nil {
		return nil, err
	}
	sq.bindingID, sq.catalogAwareInnerPlan = fs.bindingID, fs.catalogAwareInnerPlan
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	if err := retargetUsingJoins(sq.tableName, sq.tableAlias,
		sq.derivedQuery == nil && sq.inlineValues == nil && sq.tableName != "",
		sq.derivedQuery, sq.catalogAwareInnerPlan, sq.joins, md, schemaName,
		cteNamePredicate(cteScopes), cteScopes); err != nil {
		return nil, err
	}
	rememberSchemaAliasTableQualifiers(sq, resolvesToTable)
	if sq.derivedQuery != nil && md != nil && len(sq.joins) == 0 {
		op := buildOuterPlanOnDerived(sq, sq.catalogAwareInnerPlan)
		if op == nil {
			return nil, nil
		}
		return buildLogicalPlanForSelectWithCTECatalog_postBuild(op, sq, md, schemaName, cteScopes, cteOnScopes)
	}

	// Strip the session-schema qualifier off the parser's schema-qualified FROM
	// sources (`s.PB` → `PB`) BEFORE the logical tree is built. The semantic
	// analyzer's ResolveTable does not strip a schema qualifier, so without this a
	// schema-qualified table inside a SUBQUERY fails to register a scope source, the
	// projection resolver degrades to nil, and translation fails (the same class
	// demoteSchemaQualifiedUnnest / resolveQualifiedTableNames cover for the logical
	// tree). This is the catalog sub-build path (subqueries, derived tables) only —
	// the top-level query builds its scope through the PlanVisitor, untouched.
	//
	// Running BEFORE buildLogicalPlanForSelect (not after) is the ROOT fix for the
	// alias desync: a no-alias schema-qualified source `s.PB` parses with
	// alias == tableName == "S.PB", so the built LogicalScan would carry Alias
	// "S.PB" while normalize strips sq's source alias to "PB". The post-build SCOPE
	// (which reads the normalized sq) then resolves a predicate `PB.ID = PA.ID` to
	// QOV(PB) while the scan binds under "S.PB" → the predicate reads NULL and
	// misfilters rows. Normalizing FIRST makes the scan carry the SAME alias "PB"
	// the resolver uses, so resolver and scan never disagree. RFC-142.
	normalizeSchemaQualifiedSelectSources(sq, schemaName, md)

	op := buildLogicalPlanForSelect(sq)
	if op == nil || md == nil {
		// Returned WITHOUT the ON-EXISTS fold (_postBuild) — sound only because
		// the fold has nothing to do here: the one producer of a parked
		// ON-EXISTS, upgradeJoinOnPredicates, runs inside _postBuild and needs
		// the catalog, so a plan built with no md never carries one. Should a
		// producer ever run before this point, route this return through the
		// fold too; the translator's translateJoin assertion refuses an unfolded
		// join rather than planning it without its quantifier.
		return op, nil
	}
	// Java's generateAccess resolves a FROM identifier table-first at EVERY
	// FROM-source point. buildLogicalPlanForSelect (no metadata in scope) runs the
	// lateral-unnest classifier with a nil resolver, so a schema-qualified table
	// whose qualifier also names a prior alias (`FROM PA AS s, s.PB`) is tentatively
	// emitted as a LogicalUnnest. Demote it back to a Scan HERE — with metadata in
	// scope — BEFORE the post-build scope/projection-value resolution runs, so the
	// subquery's projections resolve against the correct table cross join rather than
	// degrading on the would-be unnest. This is the subquery analog of the
	// top-level demoteSchemaQualifiedUnnest pass (which mutates the logical tree only
	// — too late to recover the projection Values this nested build computes).
	// RFC-142 (P2: schema-qualified table inside a subquery).
	if err := demoteSchemaQualifiedUnnest(op, schemaName, md); err != nil {
		return nil, err
	}
	// Reject AT-ordinality on a TABLE / non-array source (`FROM t, U AT O`, a
	// present-scalar correlated field, …) HERE — at FROM-source analysis time,
	// before _postBuild resolves this (sub)query's WHERE / projection columns. This
	// is the catalog SELECT-build path's copy of the top-level PlanVisitor's early
	// pass (plan_visitor.go's rejectAtOrdinalityOnTableWithCTEs after visitFrom): a
	// subquery / derived-table / INSERT…SELECT body whose OWN predicate resolves
	// first masks the intended WRONG_OBJECT_TYPE (42809) with a scope-level
	// undefined-column (42703) — the AT source registers a virtual unnest binding
	// that SHADOWS the real table, so `U.ID` fails to resolve during _postBuild's
	// WalkPredicate. The post-attach backstop (cascades_generator.go) only walks an
	// already-attached subquery tree, so it never sees a subquery whose construction
	// fails first; running the same early rejection on the built FROM tree here, in
	// EVERY SELECT build path, surfaces 42809 regardless of which path plans the
	// SELECT. Reuses the same rejectAtOrdinalityOnTableWithCTEs helper, threading the
	// in-scope WITH-CTE names from cteScopes (a CTE source is the translator's
	// outerSourceIsCTE territory, never a base-table AT — same as the PlanVisitor
	// seeds from v.cteScopes). RFC-142.
	cteNames := make(map[string]struct{}, len(cteScopes))
	for name := range cteScopes {
		cteNames[strings.ToUpper(name)] = struct{}{}
	}
	if err := rejectAtOrdinalityOnTableWithCTEs(op, md, cteNames); err != nil {
		return nil, err
	}
	return buildLogicalPlanForSelectWithCTECatalog_postBuild(op, sq, md, schemaName, cteScopes, cteOnScopes)
}

// normalizeSchemaQualifiedSelectSources strips the session-schema qualifier off
// a selectQuery's primary + join FROM-source table names AND, in lockstep, off
// the matching join leg's un-flattened uid segments, when the source is a real
// schema-qualified table (`s.PB` where `s` is the session schema and `PB`
// resolves). It mirrors resolveQualifiedTableNames (which strips the logical
// scan's `schema.`), applied to the parser struct the scope builders AND the
// (metadata-less) rebuild classifier read. The segments MUST move with the
// tableName: the lateral-unnest classifier resolves segment 0 against the
// visible FROM aliases, and a leftover `[schema, table]` segment slice whose
// schema also happens to name a prior alias would mis-classify the real table
// as a correlated unnest on a later rebuild (`SELECT B.*` etc.).
// Sources that do not resolve to a schema-qualified table are left untouched —
// in particular a dotted reference whose qualifier is a prior FROM alias (a
// lateral unnest candidate) is NOT a `[schema, table]` pair the resolver
// matches, so its segments survive for the unnest classifier. RFC-142.
func normalizeSchemaQualifiedSelectSources(sq *selectQuery, schemaName string, md *recordlayer.RecordMetaData) {
	if sq == nil || md == nil {
		return
	}
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	strip := func(name string) string {
		segs := strings.Split(name, ".")
		if len(segs) == 2 && resolvesToTable(segs) {
			return segs[1]
		}
		return name
	}
	if sq.derivedQuery == nil {
		bare := strip(sq.tableName)
		if bare != sq.tableName {
			if sq.tableAlias == sq.tableName {
				sq.tableAlias = bare
			}
			sq.tableName = bare
			if len(sq.sourceSegments) == 2 && strings.EqualFold(sq.sourceSegments[0], schemaName) {
				sq.sourceSegments = sq.sourceSegments[1:]
			}
		}
	}
	for i := range sq.joins {
		j := &sq.joins[i]
		if j.derivedQuery != nil || j.catalogAwareInnerPlan != nil {
			continue
		}
		bare := strip(j.tableName)
		if bare != j.tableName {
			if j.alias == j.tableName {
				j.alias = bare
			}
			j.tableName = bare
			// Drop the leading schema segment in LOCKSTEP with the tableName
			// strip. The lateral-unnest classifier reads j.segments — NOT
			// j.tableName — so leaving `['main','PB']` here while tableName
			// became the bare `PB` would let a later metadata-less REBUILD
			// (`buildLogicalPlanForSelect`, e.g. forced by `SELECT B.*`)
			// see segment 0 (`main`) as a visible FROM alias (the alias of
			// `PA AS main`) and reclassify the real schema-qualified table
			// `main.PB` as a correlated unnest of `MAIN.PB`. The strip ran
			// IFF the dotted name was `[schema, table]` (resolvesToTable),
			// so segment 0 is the schema qualifier, not a prior FROM alias:
			// dropping it yields the single-segment table name the rebuild
			// classifier reads as a plain table. A genuine lateral unnest
			// `alias.field` (segment 0 a prior FROM alias) does not resolve
			// to a table, never enters this branch, and keeps its segments.
			// RFC-142.
			if len(j.segments) == 2 && strings.EqualFold(j.segments[0], schemaName) {
				j.segments = j.segments[1:]
			}
		}
	}
}

func buildLogicalPlanForSelectWithCTECatalog_postBuild(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource, cteOnScopes map[string]semantic.ScopeSource, cteBodies ...map[string]logical.LogicalOperator) (logical.LogicalOperator, error) {
	built, err := buildLogicalPlanForSelectWithCTECatalog_postBuildUnfolded(op, sq, md, schemaName, cteScopes, cteOnScopes, cteBodies...)
	if err != nil {
		return nil, err
	}
	// The block's last step: an inner join's ON-clause EXISTS becomes a
	// WHERE-EXISTS (on_exists_fold.go), so no plan leaves the builder with a
	// join carrying its own existential.
	return foldInnerOnExistsIntoWhere(built)
}

// buildLogicalPlanForSelectWithCTECatalog_postBuildUnfolded runs the upgrades;
// buildLogicalPlanForSelectWithCTECatalog_postBuild folds the block's
// ON-clause EXISTS afterwards.
func buildLogicalPlanForSelectWithCTECatalog_postBuildUnfolded(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource, cteOnScopes map[string]semantic.ScopeSource, cteBodies ...map[string]logical.LogicalOperator) (logical.LogicalOperator, error) {
	queryCTEScopes := singleSourceQueryBlockCTEScopes(sq, cteScopes, cteOnScopes)
	// Build the semantic scope once. All identifier resolution below
	// goes through this scope — same architecture as Java's
	// QueryVisitor holding a SemanticAnalyzer.
	resolver := buildSelectScope(sq, md, schemaName, queryCTEScopes)

	// Expand qualified stars (a.*) in the projection list. Replaces each
	// qualified-star slot with explicit column names from the source.
	// Matches Java's SemanticAnalyzer.expandStar.
	//
	// Two shapes:
	//  1. projQualifier != "" && projCols == nil — `SELECT a.*` alone.
	//     The parser sets projQualifier but leaves projCols nil (which
	//     buildLogicalPlanForSelect treats as SELECT *, emitting no
	//     LogicalProject). For JOINs this is wrong — it must project
	//     only the qualifier's columns. Expand into explicit projCols.
	//  2. projStarQualifiers slots — `SELECT a.*, b.label` mixed.
	//     Handled by expandQualifiedStars (rewrites star slots in-place).
	needRebuild := false
	if sq.projQualifier != "" && sq.projCols == nil {
		normalizeSoleQualifiedStar(sq)
		needRebuild = true
	}
	// A bare `SELECT *` over a JOIN … USING expands explicitly so the
	// right-side USING copies drop out (Java hides them; expandStar
	// filters hidden).
	if expandBareStarOverUsingJoins(sq, md, schemaName, queryCTEScopes) {
		needRebuild = true
	}
	if expanded, err := expandBareStarFromScope(sq, md, schemaName, queryCTEScopes); err != nil {
		return nil, err
	} else if expanded {
		needRebuild = true
	}
	if hasAnyQualifiedStar(sq) {
		if starErr := expandQualifiedStars(sq, md, schemaName, queryCTEScopes); starErr != nil {
			return nil, starErr
		}
		needRebuild = true
	}
	if needRebuild {
		op = buildLogicalPlanForSelect(sq)
		if op == nil {
			return op, nil
		}
	}

	if err := bindLateralCollections(op, sq, md, schemaName, queryCTEScopes); err != nil {
		return nil, err
	}

	// Resolve projection columns through the scope. Only plain column
	// references (projExprs[i] == nil) are resolved — computed
	// expressions / literals have non-nil projExprs entries and go
	// through the expression walker instead. Skip aggregate queries
	// (aggCols / countStar) — their projection names are aggregate
	// output labels, not column references.
	if resolver != nil && sq.projCols != nil && len(sq.aggCols) == 0 && !sq.countStar {
		proj := findProjection(op)
		for i, col := range sq.projCols {
			if col.bound != nil {
				if proj == nil || i >= len(proj.Projections) {
					return nil, api.NewError(api.ErrCodeInternalError, "star attribute has no logical projection slot")
				}
				if proj.ProjectedValues == nil {
					proj.ProjectedValues = make([]values.Value, len(proj.Projections))
				}
				proj.ProjectedValues[i] = col.bound
				continue
			}

			if i < len(sq.projExprs) && sq.projExprs[i] != nil {
				if proj != nil {
					v, walkErr := resolver.WalkExpression(sq.projExprs[i])
					if walkErr != nil {
						var corrErr *CorrelatedExistsError
						if errors.As(walkErr, &corrErr) {
							return nil, walkErr
						}
					}
					if walkErr == nil && v != nil {
						if proj.ProjectedValues == nil {
							proj.ProjectedValues = make([]values.Value, len(proj.Projections))
						}
						if i < len(proj.ProjectedValues) {
							proj.ProjectedValues[i] = v
						}
					}
				}
				continue
			}
			if col.bare != "" {
				if err := resolveColumnRefStructural(resolver, col.bare, col.qualifier, col.qualified, col.segs); err != nil {
					return nil, err
				}
			} else if err := resolveColumnName(resolver, col.name); err != nil {
				return nil, err
			}
			// A BARE column that binds to a lateral-unnest SHADOWING source
			// (`FROM t, t.arr AS v, …`) must be projected QUALIFIED to the unnest
			// correlation (`v.v`), not as a bare `v`. The unnest element flows the
			// merged row under both bare `v` and qualified `v.v`, but a LATER FROM
			// item with its own `v` overwrites the bare key last-leg-wins in
			// mergeRows; the qualified `v.v` survives (dotted keys preserved
			// verbatim). This is the SUBQUERY / DML / derived-table SELECT-build path,
			// the twin of the PlanVisitor's bare-projection step — both reuse
			// ResolveColumnShadowingQualified so the catalog and top-level paths shadow
			// identically. Without this a shadowed unnest projection inside a subquery
			// reads the wrong column (silent-wrong). RFC-142.
			if !col.qualified && col.bare != "" && proj != nil {
				id := semantic.FromNormalized(col.bare)
				qv, ok, qerr := resolver.ResolveColumnShadowingQualified(semantic.Identifier{}, id)
				if qerr == nil && ok {
					if proj.ProjectedValues == nil {
						proj.ProjectedValues = make([]values.Value, len(proj.Projections))
					}
					if i < len(proj.ProjectedValues) {
						proj.ProjectedValues[i] = qv
					}
				}
				var unresShadow *expr.UnresolvableOrdinalError
				if errors.As(qerr, &unresShadow) {
					// Born-baked (slice 2): the scope bound the name but the
					// source cannot answer a plan-time ordinal — never fall
					// through to the name channel.
					return nil, unresShadow
				}
				// A BARE non-shadowed column resolves through the scope so the
				// projection carries the construction-bound ordinal, by the SAME
				// shape rule as every other binding site (resolveBaked). An
				// unresolvable name or a lazy result keeps the translator's name
				// emission unchanged.
				//
				// This is the twin of the PlanVisitor's bare-projection bind, and it
				// used to state the retired rule: that a MULTI-SOURCE QOV-correlated
				// resolution also falls through to the name. It no longer does —
				// resolveBaked's child-bearing arm admits exactly that shape, and it
				// FIRES here on the existing corpus (RFC-223).
				if proj.ProjectedValues == nil || (i < len(proj.ProjectedValues) && proj.ProjectedValues[i] == nil) {
					rv, rerr := resolveBareProjectionValue(resolver, col.bare)
					if rerr == nil {
						if resolved := resolveProjectionValue(rv); resolved != nil {
							if proj.ProjectedValues == nil {
								proj.ProjectedValues = make([]values.Value, len(proj.Projections))
							}
							if i < len(proj.ProjectedValues) {
								proj.ProjectedValues[i] = resolved
							}
						}
					}
					var unresIdent *expr.UnresolvableOrdinalError
					if errors.As(rerr, &unresIdent) {
						return nil, unresIdent
					}
				}
			}
			if col.qualified && proj != nil {
				if proj.ProjectedValues == nil {
					proj.ProjectedValues = make([]values.Value, len(proj.Projections))
				}
				if len(sq.joins) > 0 {
					if i < len(proj.ProjectedValues) {
						// A qualified projection over a join
						// emits the resolver's QUANTIFIER-ADDRESSED
						// source-relative baked reference when resolvable (the
						// executor binds the leg window off the merged row's
						// own leg boundaries — rowLegsBinder); a flat dotted
						// "ALIAS.COL" name lookup does not run this path.
						// Twin of the PlanVisitor's qualified-projection bind
						// (incl. the DUPLICATED-bare-leaf qualified output pin).
						cr := colRef{table: col.qualifier, col: col.bare}
						if bv := resolveQualifiedProjectionValuePath(resolver,
							colRefIdentifiers(col.bare, col.qualifier, col.qualified, col.segs)); bv != nil {
							// A qualified projection's structural bake —
							// duplicated qualifiers included (per-attribute
							// resolution addresses one leg by its binding;
							// the display-keyed carve-out this arm once
							// deferred to is retired).
							proj.ProjectedValues[i] = bv
							if bareLeafDuplicated(sq.projCols, sq.projAliases, i) {
								mintQualifiedDatumKey(proj, i, col)
							}
						} else {
							// Born-baked (slice 3; the dup-alias flat-name
							// carve-out is RETIRED — a duplicated qualifier
							// bakes QOV(binding) per-attribute above, first
							// leg included, since only later duplicates were
							// renamed and QOV(alias) addresses exactly one
							// leg; ambiguous dup reads die 42702 upstream):
							// a validated qualified projection that cannot
							// bake a leg-window ordinal must fail the plan,
							// never mint a lazy name read.
							return nil, &expr.UnresolvableOrdinalError{Field: cr.col, Source: cr.table}
						}
					}
				} else {
					if v, err := resolver.ResolveIdentifierPath(
						colRefIdentifiers(colBareOrName(col), col.qualifier, col.qualified, col.segs)); err == nil {
						if i < len(proj.ProjectedValues) {
							proj.ProjectedValues[i] = v
						}
					}
				}
			}
		}
	}

	// ORDER BY: Java's ExpressionVisitor.visitOrderByExpression walks each
	// ORDER BY expression through the expression visitor. Do the same —
	// the resolver detects ambiguous/undefined column references.
	// Build a set of projection aliases for ORDER BY resolution.
	projAliasSet := make(map[string]bool)
	if sq.projAliases != nil {
		for _, a := range sq.projAliases {
			if a != "" {
				projAliasSet[strings.ToUpper(a)] = true
			}
		}
	}
	for _, ac := range sq.aggCols {
		if ac.outName != "" {
			projAliasSet[strings.ToUpper(ac.outName)] = true
		}
	}

	for _, ob := range sq.orderBy {
		if ob.rawExpr != nil {
			hasSubquery := false
			walkScalarSubqueries(ob.rawExpr, func(_ antlrgen.IQueryContext) {
				hasSubquery = true
			})
			if hasSubquery {
				return nil, api.NewError(api.ErrCodeUnsupportedSort,
					"ORDER BY with scalar subquery is not supported")
			}
			// RFC-141 R4: EXISTS in an ORDER BY key is NOT a
			// directly-handled position. The sort-key resolver carries no
			// SubqueryPlanner, so the EXISTS would fail to resolve, the key would
			// keep its raw text form, and the existential would never be
			// evaluated → a silent WRONG ORDERING (all rows tie on a constant).
			// Reject cleanly rather than mis-order (mirrors the scalar-subquery
			// rejection above).
			if expr.ContainsExistsAtom(ob.rawExpr) {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery,
					"EXISTS in an ORDER BY clause is not yet supported")
			}
		}
	}
	if resolver != nil {
		for _, ob := range sq.orderBy {
			if ob.rawExpr != nil {
				if _, walkErr := resolver.WalkExpression(ob.rawExpr); walkErr != nil {
					var ambigErr *semantic.AmbiguousColumnError
					if errors.As(walkErr, &ambigErr) {
						// Output-alias precedence, mirrored from the visitor
						// path's arm (review-caught: this twin was missed, so
						// the SAME query 42702'd inside a subquery while the
						// top level answered): a BARE key binding exactly ONE
						// output alias wins over FROM-scope ambiguity.
						if bare, n := orderByOutputAliasBinding(ob.rawExpr, ob.colName, sq); bare && n == 1 {
							continue
						}
						// Java's exact text, from the reference as written (M5).
						return nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
							"Ambiguous reference %s", ambigErr.Reference())
					}
					var notFoundErr *semantic.ColumnNotFoundError
					if errors.As(walkErr, &notFoundErr) {
						// Check if the ORDER BY name is a SELECT alias.
						if projAliasSet[strings.ToUpper(ob.colName)] {
							continue
						}
						// The ORDER BY rawExpr may reference a GROUP BY
						// alias (`ORDER BY z` where `GROUP BY x.col1 AS
						// z`). classifySelectElements rewrites ob.colName
						// to the underlying column, so colName now differs
						// from the rawExpr text. Try resolving the
						// rewritten colName through the scope; if it
						// resolves, the reference is valid.
						if ob.bare != "" {
							if resolveColumnRefStructural(resolver, ob.bare, ob.qualifier, ob.qualified, ob.segs) == nil {
								continue
							}
						} else if ob.colName != "" && resolveColumnName(resolver, ob.colName) == nil {
							continue
						}
						return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
							"column %q does not exist", ob.colName)
					}
				}
			}
		}
	}

	if resolver != nil {
		for _, gb := range sq.groupBy {
			if gb.expr != nil {
				continue
			}
			if gb.bare != "" {
				if err := resolveColumnRefStructural(resolver, gb.bare, gb.qualifier, gb.qualified, gb.segs); err != nil {
					return nil, err
				}
			} else if err := resolveColumnName(resolver, gb.display); err != nil {
				return nil, err
			}
		}
	}

	if resolver != nil {
		for _, ac := range sq.aggCols {
			if ac.aggArg != "" && ac.aggExpr == nil {
				if ac.aggArgBare != "" {
					if err := resolveColumnRefStructural(resolver, ac.aggArgBare, ac.aggArgQualifier, ac.aggArgQualified, ac.aggArgSegs); err != nil {
						return nil, err
					}
				} else if err := resolveColumnName(resolver, ac.aggArg); err != nil {
					return nil, err
				}
			}
		}
	}

	// (5b) Validate SELECT-list group-column re-reads through the scope: a
	// BARE re-read that is ambiguous across sources (GROUP BY po.id, pi.id
	// re-read as `id`) is 42702 (Java AMBIGUOUS_COLUMN) — the aggregate
	// output-name table matches keys qualifier-stripped, so an unvalidated
	// bare re-read would silently bind ONE leg's key last-wins.
	// Expression-redirected entries (groupCol = the GROUP BY expression's
	// display) carry no column reference and are skipped.
	if resolver != nil {
		exprKeyDisplays := map[string]bool{}
		for _, gn := range sq.groupBy {
			if gn.expr != nil {
				exprKeyDisplays[gn.display] = true
			}
		}
		for _, ac := range sq.aggCols {
			if ac.groupCol == "" || ac.groupColBare == "" || exprKeyDisplays[ac.groupCol] {
				continue
			}
			if err := resolveColumnRefStructural(resolver, ac.groupColBare, ac.groupColQualifier, ac.groupColQualified, ac.groupColSegs); err != nil {
				return nil, err
			}
		}
	}

	if len(sq.groupBy) > 0 && !sq.countStar {
		if err := validateGroupByProjection(sq, md); err != nil {
			return nil, err
		}
	}

	// Detect overflow numeric literals and correlated-subquery rejections
	// in projection expressions.
	if resolver != nil && len(sq.projExprs) > 0 {
		for _, e := range sq.projExprs {
			if e == nil {
				continue
			}
			if _, walkErr := resolver.WalkExpressionForProjection(e); walkErr != nil {
				var overflow *expr.NumericOverflowLiteralError
				if errors.As(walkErr, &overflow) {
					return nil, api.NewError(api.ErrCodeNumericValueOutOfRange, overflow.Error())
				}
				var binErr *expr.InvalidBinaryLiteralError
				if errors.As(walkErr, &binErr) {
					return nil, api.NewError(api.ErrCodeInvalidBinaryRepresentation, binErr.Error())
				}
				var corrErr *CorrelatedExistsError
				if errors.As(walkErr, &corrErr) {
					// Consistent SQLSTATE with the WHERE-EXISTS path (genuine
					// resolution failure → 42703/42702; Unsupported decline → 0A000).
					if mapped := mapPredicateWalkError(walkErr); mapped != nil {
						return nil, mapped
					}
					return nil, api.NewError(api.ErrCodeUnsupportedOperation, corrErr.Error())
				}
			}
		}
	}

	// Derived-table/CTE references resolve to the OUTPUT column name verbatim
	// (Java semantics); the projection already emits under that name, so there
	// is nothing to rewrite back to a source column.

	if len(sq.joins) > 0 {
		if err := upgradeJoinOnPredicates(op, sq, md, schemaName, queryCTEScopes, cteOnScopes); err != nil {
			return nil, err
		}
	}

	if len(sq.aggCols) > 0 {
		if uerr := upgradeAggregateOperands(op, sq, md, schemaName, queryCTEScopes); uerr != nil {
			return nil, uerr
		}
	}

	// Create a unified SubqueryPlanner early so both projection and
	// WHERE walks can build inner plans for EXISTS and scalar subqueries.
	var existsPlanner *existsSubqueryPlanner
	if md != nil {
		var bodies map[string]logical.LogicalOperator
		if len(cteBodies) > 0 {
			bodies = cteBodies[0]
		}
		existsPlanner = &existsSubqueryPlanner{
			md:          md,
			schemaName:  schemaName,
			outerScope:  resolverScope(resolver),
			outerScopes: buildOuterScopeSources(sq, md, schemaName, queryCTEScopes),
			cteScopes:   cteScopes,
			cteOnScopes: cteOnScopes,
			cteBodies:   bodies,
		}
	}

	if len(sq.projExprs) > 0 || len(sq.postAggExprs) > 0 {
		if err := upgradeProjectionValues(op, sq, md, schemaName, queryCTEScopes, existsPlanner); err != nil {
			return nil, err
		}
	}

	// Attach scalar subqueries from projections to the LogicalProject.
	if existsPlanner != nil && len(existsPlanner.scalarSubqueries) > 0 {
		if proj := findProjection(op); proj != nil {
			proj.ScalarSubqueries = existsPlanner.scalarSubqueries
		}
		existsPlanner.scalarSubqueries = nil
	}
	if existsPlanner != nil && len(existsPlanner.correlatedScalarSubqueries) > 0 {
		if proj := findProjection(op); proj != nil {
			proj.CorrelatedScalarSubqueries = existsPlanner.correlatedScalarSubqueries
		}
		existsPlanner.correlatedScalarSubqueries = nil
	}

	if sq.havingExpr != nil {
		if herr := upgradeHavingPredicate(op, sq, md, schemaName, queryCTEScopes, existsPlanner); herr != nil {
			return nil, herr
		}
		// HAVING has no per-group correlated-scalar quantifier lowering yet.
		// Never let the freshly minted alias escape unattached into runtime
		// evaluation (an UnboundScalarSubqueryError on valid SQL).
		if len(existsPlanner.correlatedScalarSubqueries) > 0 {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"correlated scalar subquery in a HAVING predicate is not supported")
		}
	}

	if err := upgradeSortKeyValues(op, sq, md, schemaName, queryCTEScopes); err != nil {
		return nil, err
	}

	// A BARE ORDER BY sort key that binds to a lateral-unnest SHADOWING source
	// (`FROM t, t.arr AS v, …`) must sort by the key QUALIFIED to the unnest
	// correlation (`v.v`), exactly as the bare PROJECTION column above. A LATER FROM
	// item with its own `v` overwrites the bare sort key last-leg-wins in mergeRows;
	// the qualified `v.v` survives. Without this the SORT reads the clobbered bare
	// key → rows in the WRONG ORDER (silent-wrong). This is the SUBQUERY / DML /
	// derived-table SELECT-build twin of the PlanVisitor's step (15a), reusing the
	// SAME qualifyShadowedSortKeys / ResolveColumnShadowingQualified helpers so the
	// catalog and top-level paths shadow ORDER BY identically. RFC-142.
	if resolver != nil {
		if qerr := qualifyShadowedSortKeys(op, resolver); qerr != nil {
			return nil, qerr
		}
	}

	// RFC-141 Phase 2 (projected EXISTS, the hidden-blocker step): a projected
	// ExistsValue carries an existential alias but, unlike a WHERE-EXISTS, it is
	// not collected into the existential-subquery list the translator reads to
	// attach the NamedExistentialQuantifier. upgradeProjectionValues already ran
	// BuildExists for projected EXISTS (populating existsPlanner.subqueries via
	// the walk's walkExistsValue). When there is no WHERE clause to carry them,
	// synthesize a LogicalFilter (nil predicate) above the scan to hold the
	// projected-EXISTS subqueries so the translator attaches the existential
	// quantifier and builds the FlatMap; the existential boolean is then computed
	// by the projection's ExistsValue inside the SelectExpression's result value.
	if sq.whereExpr == nil && existsPlanner != nil && len(existsPlanner.subqueries) > 0 &&
		projectionHasExistsValue(op) {
		op = attachOrSynthesizeExistsFilter(op, existsPlanner.subqueries)
		existsPlanner.subqueries = nil
		// Fall through to QUALIFY handling below (the synthesized filter and a
		// QUALIFY predicate are independent).
	}

	if sq.whereExpr == nil {
		// No WHERE, but a QUALIFY filter (the vector K-NN ROW_NUMBER() <= K
		// predicate) must still be attached — synthesize a LogicalFilter above
		// the scan rather than dropping it (an unpartitioned KNN query has no
		// WHERE, so no filter was built upstream).
		qualPred, qErr := buildQualifyPredicate(md, schemaName, sq, queryCTEScopes)
		if qErr != nil {
			return nil, qErr
		}
		if qualPred != nil {
			op = attachOrSynthesizeFilter(op, qualPred)
			op = wrapGlobalRankVectorLimit(op, qualPred)
		}
		return op, nil
	}

	// RFC-141 R4: this select-build path (used for SUBQUERIES — scalar /
	// EXISTS / derived-table inner plans built via buildLogicalPlanForQueryWith*)
	// is a SECOND WHERE-build path, distinct from the PlanVisitor's
	// visitSelectQuery (which carries the same guard). An EXISTS buried in a SCALAR
	// expression in this subquery's WHERE (`(SELECT MAX(id) FROM t2 WHERE CASE WHEN
	// EXISTS(...) THEN 1 ELSE 0 END = 1)`) lowers to a constant-false Value with no
	// existential quantifier driving it — a silent wrong result for the subquery.
	// Detect it structurally and reject cleanly here too, so a nested subquery
	// behaves identically whether it runs standalone or embedded in an outer query
	// (the boundary stop makes the OUTER detector NOT pre-empt this — the
	// subquery owns its own clause; this guard is where that ownership is enforced).
	if sq.whereExpr.Expression() != nil && expr.WhereExistsInScalarPosition(sq.whereExpr.Expression()) {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"EXISTS nested in a scalar expression is not yet supported")
	}

	// Install the SubqueryPlanner on the resolver so EXISTS and scalar
	// subqueries in WHERE clauses can be planned.
	if resolver != nil && existsPlanner != nil {
		resolver.SetSubqueryPlanner(existsPlanner)
	}

	// Walk WHERE expression through the resolver to catch ambiguous/
	// undefined column references before the predicate builder. The
	// predicate builder swallows errors into text fallback — this
	// check ensures semantic errors surface with correct SQLSTATE.
	//
	// When the walk succeeds AND the SubqueryPlanner collected EXISTS
	// subqueries, use the pre-walk predicate directly — the
	// buildWherePredicate functions don't have a SubqueryPlanner and
	// would decline the EXISTS shape, falling back to text.
	var preWalkPred predicates.QueryPredicate
	if resolver != nil && sq.whereExpr.Expression() != nil {
		walked, walkErr := resolver.WalkPredicate(sq.whereExpr.Expression())
		if walkErr != nil {
			// Classify the failure with its correct SQLSTATE (shared with the
			// JOIN-ON path). A bare ColumnNotFoundError maps to
			// ErrCodeUndefinedColumn so a correlated subquery's BuildExists falls
			// back to buildCorrelatedExists with its richer outer scope (RFC-142
			// P2c); a structured *api.Error from a nested subquery build (e.g.
			// RFC-141 R4's buried-scalar-EXISTS rejection) surfaces VERBATIM rather
			// than degrading to the text-fallback builder below (which would mask the
			// precise reason). An UNrecognized error falls through to that text
			// fallback, preserving the historical WHERE behavior.
			if apiErr := mapPredicateWalkError(walkErr); apiErr != nil {
				return nil, apiErr
			}
		} else {
			preWalkPred = walked
		}
	}

	// When the pre-walk produced a subquery-bearing predicate (EXISTS
	// or scalar), use it directly. The buildWherePredicate functions
	// build their own resolvers without a SubqueryPlanner — they'd
	// decline these shapes and fall back to text, losing the plans.
	hasSubqueries := existsPlanner != nil &&
		(len(existsPlanner.subqueries) > 0 ||
			len(existsPlanner.scalarSubqueries) > 0 ||
			len(existsPlanner.correlatedScalarSubqueries) > 0)
	if hasSubqueries && preWalkPred != nil {
		pred := predicates.SimplifyPredicateValues(preWalkPred)

		// EXISTS is lowered to a conjunctive semi-join; under an OR that loses
		// the disjunction and silently returns empty. Reject rather than
		// return wrong rows (RFC-082; inline-EXISTS-under-OR is future work).
		if existsUnderDisjunction(pred) {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation,
				"EXISTS within an OR (disjunction) is not supported")
		}

		if installErr := installFirstWherePredicate(op, pred); installErr != nil {
			return nil, installErr
		}
		if len(existsPlanner.subqueries) > 0 {
			if !upgradeFirstFilterExistsSubqueries(op, existsPlanner.subqueries) {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery,
					"WHERE subqueries could not be installed on the logical plan")
			}
		}
		if len(existsPlanner.scalarSubqueries) > 0 {
			if !upgradeFirstFilterScalarSubqueries(op, existsPlanner.scalarSubqueries) {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery,
					"WHERE scalar subqueries could not be installed on the logical plan")
			}
		}
		if len(existsPlanner.correlatedScalarSubqueries) > 0 {
			if !upgradeFirstFilterCorrelatedScalarSubqueries(op, existsPlanner.correlatedScalarSubqueries) {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery,
					"WHERE correlated scalar subqueries could not be installed on the logical plan")
			}
		}
		return op, nil
	}

	var pred predicates.QueryPredicate
	var ok bool
	if queryCTEScopes != nil && len(sq.joins) == 0 {
		if src, found := queryCTEScopes[strings.ToUpper(sq.tableName)]; found && src.Table != nil {
			// A TOMBSTONE entry (nil Table: declared CTE, schema underivable)
			// must not reach scope construction — ResolveColumn nil-derefs.
			pred, ok = buildWherePredicateFromCTEScope(src, sq.tableAlias, sq.whereExpr, md)
		}
	}
	if !ok && queryCTEScopes != nil && len(sq.joins) > 0 {
		pred, ok = buildWherePredicateForJoinsWithCTEScopes(md, schemaName, sq, sq.whereExpr, queryCTEScopes)
	}
	if !ok {
		pred, ok = buildWherePredicate(md, schemaName, sq, sq.whereExpr)
	}
	// QUALIFY (vector K-NN ROW_NUMBER() filter) is AND-combined with the WHERE
	// predicate onto the same LogicalFilter — upgradeFirstFilter replaces, so
	// both must be attached together.
	qualPred, qErr := buildQualifyPredicate(md, schemaName, sq, queryCTEScopes)
	if qErr != nil {
		return nil, qErr
	}
	if qualPred != nil {
		if ok {
			pred = predicates.NewAnd(pred, qualPred)
		} else {
			pred, ok = qualPred, true
		}
	}
	if !ok {
		return op, nil
	}
	if installErr := installFirstWherePredicate(op, pred); installErr != nil {
		return nil, installErr
	}
	op = wrapGlobalRankVectorLimit(op, pred)
	return op, nil
}

// singleSourceQueryBlockCTEScopes admits a complete ON-only CTE schema to the
// semantic resolver for exactly one query block that reads that CTE as its sole
// FROM source. Join/unnest-bodied CTEs deliberately stay out of the global
// cteScopes map: making their lossy or incomplete schemas globally visible to
// comma joins once enabled the flatten-evasion wrong-row class. The ON-only
// registry is now complete-or-decline, however, and a non-marker entry is the
// exact output schema that execution exposes at the CTE boundary.
//
// A sole-source query has no sibling whose columns can be rebound by admitting
// that schema, so it is safe to use for WHERE, projection, GROUP/HAVING, and
// ORDER BY resolution in this block. The copy is intentionally block-local:
// callers must not mutate either registry, and a multi-leg or derived-source
// block receives the original global map unchanged. This is load-bearing for a
// nested CTE that shadows a same-named base table: falling through to the
// catalog types T.ID against the base T row instead of the CTE's projected row.
func singleSourceQueryBlockCTEScopes(
	sq *selectQuery,
	cteScopes map[string]semantic.ScopeSource,
	cteOnScopes map[string]semantic.ScopeSource,
) map[string]semantic.ScopeSource {
	if sq == nil || sq.tableName == "" || sq.derivedQuery != nil || len(sq.joins) != 0 {
		return cteScopes
	}
	key := strings.ToUpper(sq.tableName)
	if _, globallyVisible := cteScopes[key]; globallyVisible {
		return cteScopes
	}
	source, ok := cteOnScopes[key]
	if !ok || source.Table == nil {
		// A nil table is the explicit underivable marker. It must keep the
		// resolver closed and, in particular, must not fall through to a
		// same-named catalog table.
		return cteScopes
	}
	local := make(map[string]semantic.ScopeSource, len(cteScopes)+1)
	for name, visible := range cteScopes {
		local[name] = visible
	}
	local[key] = source
	return local
}

// buildSelectScope builds a semantic scope + resolver from the FROM
// clause of a selectQuery. This is the single point of scope
// construction — all identifier resolution (projection, ORDER BY,
// GROUP BY, WHERE, ON) goes through the returned resolver.
//
// Returns nil resolver when the scope can't be built (missing metadata,
// CTE-only sources without schema, etc.). Callers fall back to text.
func buildSelectScope(
	sq *selectQuery,
	md *recordlayer.RecordMetaData,
	schemaName string,
	cteScopes map[string]semantic.ScopeSource,
) *expr.Resolver {
	resolver, _ := buildSelectScopeChecked(sq, md, schemaName, cteScopes)
	return resolver
}

// buildSelectScopeChecked is the same scope construction with source errors
// preserved for binding a FROM collection before its virtual source is added.
func buildSelectScopeChecked(
	sq *selectQuery,
	md *recordlayer.RecordMetaData,
	schemaName string,
	cteScopes map[string]semantic.ScopeSource,
) (*expr.Resolver, error) {
	if sq == nil || md == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "SELECT source has no exact semantic scope")
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	scope := semantic.NewScope(sq.enclosingScope)
	if sq.tableName == "" {
		return expr.New(analyzer, scope), nil
	}
	schemaStrip := newUnnestTableResolver(md, schemaName)
	additionalTableQualifiers := func(tableName, alias string) []semantic.Identifier {
		if sq.tableQualifierAliases == nil || !sq.tableQualifierAliases[strings.ToUpper(alias)] ||
			tableName == "" || strings.EqualFold(tableName, alias) {
			return nil
		}
		return []semantic.Identifier{semantic.FromNormalized(tableName)}
	}

	// One entry per FROM position (0 = the primary source): does an outer join
	// pad this leg with NULLs? A padded leg's columns are nullable in this query
	// block's row, so every reference resolved through this scope carries the type
	// the join actually produces. Same derivation the derived-table body uses,
	// from the same helper, because a query block and that block read as a derived
	// table must agree on their row.
	padded := nullSupplyingFromLegs(sq.joins)
	addSource := func(tableName, alias, bindingID string, hidden []string, position int) error {
		// ACTIVE-SCHEMA-QUALIFIED source (`"s"."LA"`): on the visitor path
		// sq keeps the dotted spelling (normalizeSchemaQualifiedSelectSources
		// runs only on the catalog sub-build path), and a raw ResolveTable
		// miss here NIL'd the whole resolver — killing the 42702/42703 gates
		// (an ambiguous bare ref over `"s"."LA", LB` executed silently,
		// review-caught) and every WHERE/ORDER BY resolution over
		// schema-qualified explicit joins. Strip the schema segment with a
		// defaulted alias in lockstep, mirroring the ON-upgrade scope build
		// and the normalizer.
		if segs := strings.Split(tableName, "."); len(segs) == 2 && schemaStrip(segs) {
			if alias == tableName {
				alias = segs[1]
			}
			tableName = segs[1]
		}
		// CTE-FIRST: a declared CTE shadows a same-named catalog table
		// (execution's translateScan contract; the same ordering cteLegKind
		// applies). The prior catalog-first order analyzed the TABLE's
		// schema for reads that execute against the CTE — 42703 on the
		// CTE's own columns (review-caught; the plain-body variant of the
		// shape was broken this way all along, masked only for
		// schema-qualified bodies by the pre-round-9 nil resolver).
		if cteScopes != nil {
			if src, found := cteScopes[strings.ToUpper(tableName)]; found {
				// TOMBSTONE (nil Table): a DECLARED CTE whose schema is not
				// derivable in this context (underivable nested shadow). It
				// must NOT fall through to the catalog — a same-named base
				// table would bind ITS ordinals onto the CTE's rows (silent
				// wrong slots). Declining the scope add keeps resolution
				// loud downstream.
				if src.Table == nil {
					return api.NewErrorf(api.ErrCodeUnsupportedQuery, "CTE %q has no exact semantic schema", tableName)
				}
				aliasID := semantic.FromNormalized(alias)
				if alias == "" {
					aliasID = semantic.FromNormalized(tableName)
				}
				cteSrc := cteSourceAs(src, aliasID, bindingOrAlias(bindingID, aliasID))
				cteSrc.AdditionalQualifiers = additionalTableQualifiers(tableName, alias)
				cteSrc.HiddenColumns = hiddenColumnSet(hidden)
				return scope.AddSource(nullSupplyingSource(cteSrc, paddedAt(padded, position)))
			}
		}
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err != nil {
			if mapped := mapPredicateWalkError(err); mapped != nil {
				return mapped
			}
			return err
		}
		aliasID := semantic.FromNormalized(alias)
		if alias == "" {
			aliasID = semantic.FromNormalized(tableName)
		}
		return scope.AddSource(nullSupplyingSource(semantic.ScopeSource{
			Table:                tbl,
			Alias:                aliasID,
			CorrelationName:      bindingOrAlias(bindingID, aliasID),
			AdditionalQualifiers: additionalTableQualifiers(tableName, alias),
			HiddenColumns:        hiddenColumnSet(hidden),
		}, paddedAt(padded, position)))
	}

	if sq.inlineValues != nil {
		src, ok := parsedInlineValuesScopeSource(sq.inlineValues, sq.tableAlias, "", md)
		if !ok {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery, "inline VALUES has no exact semantic schema")
		}
		if err := scope.AddSource(nullSupplyingSource(src, paddedAt(padded, 0))); err != nil {
			return nil, err
		}
	} else if sq.derivedQuery != nil {
		src, err := boundDerivedSource(md, sq.tableName, sq.bindingID, sq.derivedQuery, sq.catalogAwareInnerPlan, sq.enclosingScope, schemaName, cteScopes)
		if err != nil {
			return nil, err
		}
		if err := scope.AddSource(nullSupplyingSource(src, paddedAt(padded, 0))); err != nil {
			return nil, err
		}
	} else if err := addSource(sq.tableName, sq.tableAlias, "", nil, 0); err != nil {
		return nil, err
	}
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	for i, j := range sq.joins {
		if j.inlineValues != nil {
			src, ok := parsedInlineValuesScopeSource(j.inlineValues, j.alias, j.bindingID, md)
			if !ok {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery, "inline VALUES has no exact semantic schema")
			}
			src.HiddenColumns = hiddenColumnSet(j.usingHiddenCols)
			if err := scope.AddSource(nullSupplyingSource(src, paddedAt(padded, i+1))); err != nil {
				return nil, err
			}
			continue
		}
		if j.derivedQuery != nil {
			src, err := boundDerivedSource(md, j.alias, j.bindingID, j.derivedQuery, j.catalogAwareInnerPlan, sq.enclosingScope, schemaName, cteScopes)
			if err != nil {
				return nil, err
			}
			if j.bindingID != "" {
				src.CorrelationName = j.bindingID
			}
			src.HiddenColumns = hiddenColumnSet(j.usingHiddenCols)
			if err := scope.AddSource(nullSupplyingSource(src, paddedAt(padded, i+1))); err != nil {
				return nil, err
			}
			continue
		}
		visible := visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i], resolvesToTable)
		if isLateralUnnestJoin(j, visible, resolvesToTable) {
			element, typed := unnestElementColumn(scope, j)
			var elementPtr *semantic.Column
			if typed {
				elementPtr = &element
			}
			src, ok := unnestVirtualScopeSourceWithElement(j, elementPtr)
			if !ok {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery, "unnest source requires an element or ordinal alias")
			}
			if err := scope.AddSource(src); err != nil {
				return nil, err
			}
			continue
		}
		if err := addSource(j.tableName, j.alias, j.bindingID, j.usingHiddenCols, i+1); err != nil {
			return nil, err
		}
	}
	return expr.New(analyzer, scope), nil
}

// paddedAt reads the null-supplying verdict for one FROM position. Out of
// range is FALSE: a position the join-algebra vector does not describe is not
// padded by any join it knows about.
func paddedAt(padded []bool, position int) bool {
	return position >= 0 && position < len(padded) && padded[position]
}

// nullSupplyingSource returns src with its columns nullable when an outer join
// pads that leg. Copy-on-wrap: the catalog's own Column values are never
// mutated, and a source with no table is returned unchanged.
func nullSupplyingSource(src semantic.ScopeSource, nullSupplying bool) semantic.ScopeSource {
	if !nullSupplying || src.Table == nil {
		return src
	}
	src.Table = nullSupplyingTable{Table: src.Table}
	return src
}

// cteSourceAs re-aliases a registered CTE source for the query block that
// reads it, carrying the source WHOLE — its flowed layout, hidden columns and
// shadowing state included — under the block's alias and runtime binding.
// Rebuilding the source from its Table alone dropped the flowed layout, so the
// quantified object a read of the CTE minted stated the SQL labels rather than
// the row the body flows, and the read was refused at execution as `edge
// lookup U: read as RECORD(G,G,W), declared RECORD(GA.G,G,W)` for every body
// whose runtime names differ from its SQL names — a join projection that
// repeats a bare leaf, a projection that repeats an alias. Every site that
// installs a CTE source into a reading scope goes through here.
func cteSourceAs(src semantic.ScopeSource, alias semantic.Identifier, binding string) semantic.ScopeSource {
	src.Alias = alias
	src.CorrelationName = binding
	return src
}

// hiddenColumnSet folds a USING-hidden column list into the ScopeSource
// set shape (UPPER keys). Nil in, nil out — most sources hide nothing.
func hiddenColumnSet(hidden []string) map[string]struct{} {
	if len(hidden) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(hidden))
	for _, h := range hidden {
		out[strings.ToUpper(h)] = struct{}{}
	}
	return out
}

// orderByOutputAliasBinding classifies an ORDER BY key for the
// output-alias-precedence rule: bareIdent is true only when the RAW key is
// exactly a single-segment column reference (typed walk — columnNameFromExpr
// canonicalizes aggregate calls too, so `ORDER BY SUM(K)` must NOT match a
// quoted "SUM(K)" alias and thereby suppress a genuine ambiguity inside the
// aggregate's argument, review-caught); matches counts how many projection
// output columns bind the name (a presence-only check let duplicate aliases
// K, K bypass 42702 and silently sort by whichever the alias map kept last,
// review-caught). The precedence applies only when bareIdent && matches==1.
func orderByOutputAliasBinding(rawExpr antlrgen.IExpressionContext, colName string, sq *selectQuery) (bareIdent bool, matches int) {
	if colName == "" || !isBareIdentifierExpr(rawExpr) {
		return false, 0
	}
	upper := strings.ToUpper(colName)
	for _, a := range sq.projAliases {
		if a != "" && strings.ToUpper(a) == upper {
			matches++
		}
	}
	for _, ac := range sq.aggCols {
		if ac.outName != "" && strings.ToUpper(ac.outName) == upper {
			matches++
		}
	}
	return true, matches
}

// isBareIdentifierExpr reports whether the expression is exactly a
// single-segment column reference — descending only single-child wrapper
// nodes to the atom (typed nodes, never text).
func isBareIdentifierExpr(e antlrgen.IExpressionContext) bool {
	var n antlr.Tree = e
	for n != nil {
		if c, ok := n.(*antlrgen.FullColumnNameExpressionAtomContext); ok {
			return len(c.FullColumnName().FullId().AllUid()) == 1
		}
		if n.GetChildCount() != 1 {
			return false
		}
		n = n.GetChild(0)
	}
	return false
}

// resolveColumnRefStructural resolves a column reference from its
// parse-tree SEGMENTS — never a dotted re-split of a rendered string,
// so a derived column or alias whose NAME contains a dot ("A.ID")
// resolves as itself instead of being torn at the last dot into a
// phantom qualifier (WS-N Phase A slice 1; the segments arrive
// quote-stripped with quoted case preserved, so identifiers are built
// case-sensitively — no re-fold).
// colRefIdentifiers renders a captured column reference as the ordered
// Identifier list resolution consumes. The leading segments name SOURCES and
// STRUCT COLUMNS, which are registered folded; the LEAF keeps the verbatim
// spelling, matching what the two-part path has always passed.
//
// segs is authoritative when present. The (qualifier, bare) fallback exists
// for carriers that capture no segments, and it can only ever express two —
// which is exactly the flattening that made a three-segment reference look up
// a source literally named "A.N".
func colRefIdentifiers(bare, qualifier string, qualified bool, segs []string) []semantic.Identifier {
	if len(segs) > 0 {
		ids := make([]semantic.Identifier, len(segs))
		for i, s := range segs {
			if i == len(segs)-1 {
				ids[i] = semantic.New(s, true)
			} else {
				ids[i] = semantic.FromNormalized(s)
			}
		}
		return ids
	}
	if qualified && qualifier != "" {
		return []semantic.Identifier{semantic.FromNormalized(qualifier), semantic.FromNormalized(bare)}
	}
	return []semantic.Identifier{semantic.FromNormalized(bare)}
}

func resolveColumnRefStructural(resolver *expr.Resolver, bare, qualifier string, qualified bool, segs []string) error {
	if resolver == nil || bare == "" {
		return nil
	}
	// A reference of THREE OR MORE segments (`a.n.sk`) can only be resolved
	// from its segment list: the two-part (qualifier, name) form joins the
	// leading segments into "A.N", which names neither a source nor a column,
	// so the reference died as UNDEFINED_COLUMN while Java answered it.
	// Java's resolver has no arity cap on this path — Identifier carries the
	// segments as a list and lookupNestedField consumes a matched prefix and
	// walks whatever remains (SemanticAnalyzer.java:559-601).
	if len(segs) > 2 {
		ids := colRefIdentifiers(bare, qualifier, qualified, segs)
		_, err := resolver.ResolveIdentifierPath(ids)
		if err != nil {
			var notFound *semantic.ColumnNotFoundError
			if errors.As(err, &notFound) {
				// The same folded retry the two-segment arm performs below, for
				// the same reason: a derived or virtual schema registers its
				// columns folded, so a verbatim miss re-tries the folded leaf.
				ids[len(ids)-1] = semantic.NewUnquoted(segs[len(segs)-1])
				if _, retryErr := resolver.ResolveIdentifierPath(ids); retryErr == nil {
					return nil
				}
			}
		}
		// The reference renders AS WRITTEN — all of its segments. Rebuilding it
		// from (qualifier, bare) would report a two-part name the user did not
		// type, which is the same flattening that caused the refusal.
		return mapColumnResolveError(err, strings.Join(segs, "."))
	}
	var qual semantic.Identifier
	display := bare
	if qualified {
		// The qualifier FOLDS: source aliases are registered through the
		// folded namespace (a quoted lowercase alias "q" registers as
		// "Q"), so the lookup must fold identically. Alias-namespace
		// case fidelity is Phase B's scope; the COLUMN side below stays
		// verbatim — that is where dotted/quoted-case names live.
		qual = semantic.FromNormalized(qualifier)
		display = qualifier + "." + bare
	}
	_, err := resolver.ResolveIdentifier(qual, semantic.New(bare, true))
	if err != nil {
		var notFound *semantic.ColumnNotFoundError
		if errors.As(err, &notFound) {
			// Folded retry: derived/virtual schemas still REGISTER their
			// columns folded (an alias "id" registers as "ID"), so a
			// verbatim miss re-tries the folded spelling. This retry MUST
			// fold — NewUnquoted on purpose, the one deliberate
			// re-normalization: FromNormalized would re-issue the
			// identical verbatim spelling and the exact-case lookup would
			// miss again (a quoted-lowercase reference over a derived
			// table's folded registration → spurious 42703).
			// Verbatim-first keeps case-significant names ("A.ID", quoted
			// lowercase stored columns) winning; Phase D makes
			// registrations case-faithful and retires this retry.
			if _, retryErr := resolver.ResolveIdentifier(qual, semantic.NewUnquoted(bare)); retryErr == nil {
				return nil
			}
		}
	}
	return mapColumnResolveError(err, display)
}

// resolveColumnName is the RENDERED-STRING arm for call sites whose
// carrier predates structural segment capture: it re-splits at the last
// dot (parseColRef), which mis-tears dotted display names — every
// caller with parse-tree segments must use resolveColumnRefStructural.
func resolveColumnName(resolver *expr.Resolver, col string) error {
	if resolver == nil || col == "" {
		return nil
	}
	var qualifier semantic.Identifier
	ref := parseColRef(col)
	id := semantic.FromNormalized(ref.bare())
	if ref.isQualified() {
		qualifier = semantic.FromNormalized(ref.table)
	}
	_, err := resolver.ResolveIdentifier(qualifier, id)
	return mapColumnResolveError(err, col)
}

// mapColumnResolveError classifies a ResolveIdentifier failure into its
// SQLSTATE (42702 ambiguous / 42703 undefined), shared by the
// structural and rendered-string arms.
func mapColumnResolveError(err error, display string) error {
	if err != nil {
		var ambigErr *semantic.AmbiguousColumnError
		if errors.As(err, &ambigErr) {
			// Java's exact SemanticAnalyzer text, from the reference as
			// written — verified against both duplicate and distinct
			// aliases, bare and qualified.
			return api.NewErrorf(api.ErrCodeAmbiguousColumn,
				"Ambiguous reference %s", ambigErr.Reference())
		}
		var notFoundErr *semantic.ColumnNotFoundError
		if errors.As(err, &notFoundErr) {
			return api.NewErrorf(api.ErrCodeUndefinedColumn,
				"column %q does not exist", display)
		}
		var srcNotFound *semantic.SourceNotFoundError
		if errors.As(err, &srcNotFound) {
			return api.NewErrorf(api.ErrCodeUndefinedColumn,
				"column reference with qualifier %q cannot be resolved", srcNotFound.Alias.Name())
		}
	}
	return nil
}

// splitNonExistsPredicatesFromWalked returns only the non-EXISTS parts
// of a walked predicate tree. EXISTS and NOT(EXISTS) nodes are dropped.
// Returns nil if there are no non-EXISTS predicates.
func splitNonExistsPredicatesFromWalked(p predicates.QueryPredicate) predicates.QueryPredicate {
	if p == nil {
		return nil
	}
	if _, ok := predicates.IsExistentialPredicate(p); ok {
		return nil
	}
	if _, ok := predicates.IsNotExistentialPredicate(p); ok {
		return nil
	}
	if and, ok := p.(*predicates.AndPredicate); ok {
		var nonExists []predicates.QueryPredicate
		for _, sub := range and.SubPredicates {
			if ne := splitNonExistsPredicatesFromWalked(sub); ne != nil {
				nonExists = append(nonExists, ne)
			}
		}
		if len(nonExists) == 1 {
			return nonExists[0]
		}
		if len(nonExists) > 1 {
			return predicates.NewAnd(nonExists...)
		}
		return nil
	}
	return p
}

// stripNonExistsPredicates removes non-EXISTS predicates from an AND
// tree, returning only the EXISTS (or NOT EXISTS) predicate. Returns
// nil if no EXISTS predicate is found.
func stripNonExistsPredicates(p predicates.QueryPredicate) predicates.QueryPredicate {
	if p == nil {
		return nil
	}
	if _, ok := predicates.IsExistentialPredicate(p); ok {
		return p
	}
	if _, ok := predicates.IsNotExistentialPredicate(p); ok {
		return p
	}
	if and, ok := p.(*predicates.AndPredicate); ok {
		var existsPreds []predicates.QueryPredicate
		for _, sub := range and.SubPredicates {
			if ep := stripNonExistsPredicates(sub); ep != nil {
				existsPreds = append(existsPreds, ep)
			}
		}
		if len(existsPreds) == 1 {
			return existsPreds[0]
		}
		if len(existsPreds) > 1 {
			return predicates.NewAnd(existsPreds...)
		}
	}
	return nil
}

// existsUnderDisjunction reports whether an EXISTS / NOT EXISTS predicate is
// reachable through an OR in the predicate tree. Go lowers EXISTS predicates to
// conjunctive semi-joins (FlatMap), which is only correct under AND. Under an
// OR the EXISTS must instead be evaluated as an inline boolean (P OR EXISTS(Q)
// is true when P is true OR Q matches) — not yet supported. Callers reject with
// a clear error rather than returning wrong rows: the split helpers
// (stripNonExistsPredicates / splitNonExistsPredicatesFromWalked) only recurse
// through AND, so an EXISTS under OR is silently mis-extracted into an
// unconditional semi-join and the disjunction is lost (returns empty).
func existsUnderDisjunction(p predicates.QueryPredicate) bool {
	return existsReachableUnderOr(p, false)
}

func existsReachableUnderOr(p predicates.QueryPredicate, underOr bool) bool {
	if p == nil {
		return false
	}
	if _, ok := predicates.IsExistentialPredicate(p); ok {
		return underOr
	}
	if _, ok := p.(*predicates.OrPredicate); ok {
		underOr = true
	}
	for _, ch := range p.Children() {
		if existsReachableUnderOr(ch, underOr) {
			return true
		}
	}
	return false
}

// upgradeFirstFilterExistsSubqueries walks the single-child chain from op and,
// at the first LogicalFilter, attaches the EXISTS subquery plans. Returns true
// when a Filter was found.
func upgradeFirstFilterExistsSubqueries(op logical.LogicalOperator, subqueries []logical.ExistsSubquery) bool {
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			f.ExistsSubqueries = subqueries
			return true
		}
		ch := cur.Children()
		if len(ch) != 1 {
			return false
		}
		cur = ch[0]
	}
	return false
}

// projectionHasExistsValue reports whether the LogicalProject on op's unary
// spine carries a projected ExistsValue (RFC-141 Phase 2). Walks the projected
// Value trees TYPED — no GetText / text matching — so `NOT EXISTS` (NotValue
// over ExistsValue), CASE branches, etc. are all detected structurally.
func projectionHasExistsValue(op logical.LogicalOperator) bool {
	proj := findProjection(op)
	if proj == nil {
		return false
	}
	for _, v := range proj.ProjectedValues {
		if v == nil {
			continue
		}
		found := false
		values.WalkValue(v, func(node values.Value) bool {
			if _, ok := node.(*values.ExistsValue); ok {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// attachOrSynthesizeExistsFilter attaches the projected-EXISTS subqueries to
// the first LogicalFilter on op's unary spine; if there is none (a projected
// EXISTS with no WHERE builds no filter), it synthesizes a LogicalFilter (nil
// predicate) directly above the base LogicalScan to hold them — the same
// position a WHERE filter occupies — so the translator attaches the existential
// quantifier and builds the FlatMap. Returns the (possibly new) plan root.
func attachOrSynthesizeExistsFilter(op logical.LogicalOperator, subqueries []logical.ExistsSubquery) logical.LogicalOperator {
	if upgradeFirstFilterExistsSubqueries(op, subqueries) {
		return op
	}
	if _, isScan := op.(*logical.LogicalScan); isScan {
		f := logical.NewFilterWithPredicate(op, nil, "")
		f.ExistsSubqueries = subqueries
		return f
	}
	// Walk the unary spine to the deepest unary operator and synthesize the
	// existential filter as its child — directly above the scan OR the join.
	// The filter MUST land UNDER the projection (between the last unary op and
	// the leaf/join), never above it: a filter above the projection runs the
	// projection — and its projected ExistsValue — BEFORE the FlatMap, where the
	// existential binding is dead (the join-from leak). With the filter
	// below the projection, translateProject's findExistsFilterUnderUnaryChain
	// reaches the existential filter and folds the projection into the
	// existential SelectExpression's result value (RFC-141), handling a JOIN
	// input via buildExistentialSelect's join flatten.
	for cur := op; cur != nil; {
		child, ok := unaryInput(cur)
		if !ok {
			// op itself is non-unary (e.g. a bare LogicalJoin with no project).
			// Wrap it directly; there is no projection above to displace.
			f := logical.NewFilterWithPredicate(op, nil, "")
			f.ExistsSubqueries = subqueries
			return f
		}
		// child is the deepest unary's input when it is a scan OR a join (any
		// non-unary). Either way the synthesized filter must sit on top of that
		// child, below cur, so the spine becomes ...cur -> Filter(child).
		if _, childUnary := unaryInput(child); !childUnary {
			f := logical.NewFilterWithPredicate(child, nil, "")
			f.ExistsSubqueries = subqueries
			setUnaryInput(cur, f)
			return op
		}
		cur = child
	}
	return op
}

// upgradeFirstFilterScalarSubqueries walks the single-child chain
// from op and, at the first LogicalFilter, attaches the scalar
// subquery plans. Returns true when a Filter was found.
func upgradeFirstFilterScalarSubqueries(op logical.LogicalOperator, subqueries []logical.ScalarSubquery) bool {
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			f.ScalarSubqueries = subqueries
			return true
		}
		ch := cur.Children()
		if len(ch) != 1 {
			return false
		}
		cur = ch[0]
	}
	return false
}

// upgradeFirstFilterCorrelatedScalarSubqueries attaches correlated scalar
// plans to the WHERE filter that owns their ScalarSubqueryValue references.
// Unlike uncorrelated scalars these plans are not pre-evaluated: the Cascades
// translator materializes each scalar per outer row through the strict
// LEFT-scalar join lowering.
func upgradeFirstFilterCorrelatedScalarSubqueries(op logical.LogicalOperator, subqueries []logical.CorrelatedScalarSubquery) bool {
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			f.CorrelatedScalarSubqueries = subqueries
			return true
		}
		ch := cur.Children()
		if len(ch) != 1 {
			return false
		}
		cur = ch[0]
	}
	return false
}

// upgradeFirstFilter walks the single-child chain from op and, at
// the first LogicalFilter, sets Predicate. Stops at the first
// non-unary node. Returns true when a Filter was found and upgraded.
func upgradeFirstFilter(op logical.LogicalOperator, pred predicates.QueryPredicate) bool {
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			f.Predicate = pred
			return true
		}
		ch := cur.Children()
		if len(ch) != 1 {
			return false
		}
		cur = ch[0]
	}
	return false
}

// installFirstWherePredicate is the checked form of upgradeFirstFilter for
// WHERE-bearing production builders. Those builders create a LogicalFilter
// before adding unary SELECT shells; if that structural invariant ever drifts,
// fail closed with a typed SQL error instead of returning a logical tree whose
// text-only filter cannot be translated.
func installFirstWherePredicate(op logical.LogicalOperator, pred predicates.QueryPredicate) error {
	if pred == nil {
		return api.NewError(api.ErrCodeUnsupportedQuery,
			"WHERE predicate could not be constructed")
	}
	if !upgradeFirstFilter(op, pred) {
		return api.NewError(api.ErrCodeUnsupportedQuery,
			"WHERE predicate could not be installed on the logical plan")
	}
	return nil
}

// upgradeProjectionValues walks the unary spine from op to find the
// LogicalProject node, then attempts to resolve each projExpr through
// the expr.Resolver to produce a values.Value tree. Successful slots
// are stored in LogicalProject.ProjectedValues; failed slots remain nil
// (the Cascades translator treats nil as "plain column reference" when
// the text isn't a computed expression, or "cannot translate" otherwise).
func upgradeProjectionValues(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource, subqPlanner *existsSubqueryPlanner) error {
	if err := resolveProjectionValues(op, sq, md, schemaName, cteScopes, subqPlanner); err != nil {
		return err
	}
	if err := validateSelectOutputNames(sq); err != nil {
		return err
	}
	publishInheritedProjectionNames(findProjection(op), sq)
	return nil
}

// publishInheritedProjectionNames preserves Expression.fromColumn's inherited
// SQL name after resolution. Output aliases are names, not evidence of SELECT
// AS syntax; that fact stays in sq.projAliases and aggregate outputAliased.
func publishInheritedProjectionNames(proj *logical.LogicalProject, sq *selectQuery) {
	if proj == nil {
		return
	}
	var names []string
	for i, col := range sq.projCols {
		if i >= len(proj.Projections) || i >= len(proj.ProjectedValues) || proj.ProjectedValues[i] == nil {
			continue
		}
		name := col.bare
		_, isField := values.AsFieldValue(proj.ProjectedValues[i])
		if !col.qualified && (col.bound != nil || isField) {
			// Expression.fromColumn inherits the selected ATTRIBUTE'S name, not
			// the one-segment reference spelling that reached it. This distinction
			// is live for Go's case-insensitive lookup extension: `"keepcase"` may
			// bind declared `KeepCase`, but the output remains `KeepCase`. A
			// qualified reference already carries its structural bare leaf in
			// col.bare; its pre-bake Value rendering can still be X.K and must not
			// become the output name. A scalar QOV reference keeps col.bare:
			// its correlation names a runtime binding, not a SQL attribute.
			name = values.DisplayColumnName(proj.ProjectedValues[i], "")
		}
		if name == "" {
			if col.bound == nil {
				continue
			}
			// Star can carry an unnamed expression from a derived/CTE source.
			// Its physical label belongs to this output position, not the input
			// field's generated label; neither becomes a semantic SQL name.
			if proj.SQLNames == nil {
				proj.SQLNames = projectionOutputNames(sq)
			}
			name = values.OrdinalFieldName(i)
		}
		if i < len(sq.projExprs) && sq.projExprs[i] != nil {
			continue
		}
		if i < len(proj.Aliases) && proj.Aliases[i] != "" || i < len(proj.AliasMinted) && proj.AliasMinted[i] {
			continue
		}
		if names == nil {
			names = make([]string, len(proj.Projections))
			copy(names, proj.Aliases)
		}
		names[i] = name
	}
	if names != nil {
		proj.Aliases = names
	}
}

// validateSelectOutputNames mirrors Type.Record.Field's storage-name construction
// after binding. Executor keys and ignored trailing SELECT identifiers are not
// authored names; only the captured AS names cross this materialization boundary.
func validateSelectOutputNames(sq *selectQuery) error {
	validate := func(name string) error {
		if name == "" {
			return nil
		}
		if _, err := protoname.ToProtoBufCompliantName(name); err != nil {
			return api.WrapError(api.ErrCodeInvalidName, err.Error(), err)
		}
		return nil
	}
	for _, name := range sq.projAliases {
		if err := validate(name); err != nil {
			return err
		}
	}
	if err := validate(sq.countStarAlias); err != nil {
		return err
	}
	for _, ac := range sq.aggCols {
		if ac.outputAliased {
			if err := validate(ac.outName); err != nil {
				return err
			}
		}
	}
	return nil
}

func resolveProjectionValues(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource, subqPlanner *existsSubqueryPlanner) error {
	proj := findProjection(op)
	if proj == nil {
		return nil
	}
	// Post-aggregation projections: walk through the Resolver using base
	// table scope, then rewrite AggregateValues to FieldValue references.
	if len(sq.postAggExprs) > 0 {
		resolver := buildProjectionResolverWithCTEScopes(sq, md, schemaName, cteScopes)
		if resolver == nil {
			resolver = buildSelectScope(sq, md, schemaName, cteScopes)
		}
		if resolver == nil {
			return nil
		}
		if subqPlanner != nil {
			resolver.SetSubqueryPlanner(subqPlanner)
		}
		vals := make([]values.Value, len(proj.Projections))
		copy(vals, proj.ProjectedValues)
		agg := findAggregate(op)
		if agg == nil || len(proj.AggregateOutputOrdinals) != len(proj.Projections) {
			return api.NewError(api.ErrCodeUnsupportedQuery,
				"post-aggregate projection has no complete native output-slot layout")
		}
		aggSlots := make([]bool, len(proj.Projections))
		for i, ordinal := range proj.AggregateOutputOrdinals {
			aggSlots[i] = ordinal >= len(agg.GroupKeys)
			if ordinal >= 0 {
				// A native output slot is metadata until translateProject has
				// constructed the quantifier that actually owns the aggregate row.
				// Publishing a childless/current-root FieldValue here would erase
				// precisely that ownership. The translator resolves this ordinal
				// against its real input quantifier.
				vals[i] = nil
			}
		}
		for i, e := range sq.postAggExprs {
			if i >= len(vals) || e == nil {
				continue
			}
			v, err := resolver.WalkExpression(e)
			if err != nil {
				// Propagate real semantic errors (e.g. 42703 undefined
				// column from a correlated scalar subquery). Only
				// UnsupportedExpressionShapeError should be swallowed.
				var apiErr *api.Error
				if errors.As(err, &apiErr) {
					return err
				}
				continue
			}
			aggSlots[i] = containsAggregate(v) // pre-rewrite: aggregate nodes still present
			if err = validatePostAggregateValueDraft(v, agg); err != nil {
				return err
			}
			vals[i] = v
		}
		proj.ProjectedValues = vals
		proj.AggregateSlots = aggSlots
		// ORDER BY and aggregate-index DDL need the SELECT output instances,
		// including direct group/aggregate slots. They cannot be published as
		// owner-bound FieldValues yet, but the exact logical draft Values are
		// stable identity tokens for this pre-translation contract. The
		// translator still binds the parallel AggregateOutputOrdinals onto its
		// real quantifier; these pointers never escape as runtime reads.
		for i, ordinal := range proj.AggregateOutputOrdinals {
			if i >= len(proj.ProjectedValues) || proj.ProjectedValues[i] != nil || ordinal < 0 {
				continue
			}
			if ordinal < len(agg.GroupKeys) {
				proj.ProjectedValues[i] = agg.GroupKeys[ordinal].Value
				continue
			}
			call := ordinal - len(agg.GroupKeys)
			if call >= 0 && call < len(agg.Calls) {
				var operand values.Value
				if call < len(agg.AggregateOperands) {
					operand = agg.AggregateOperands[call]
				}
				proj.ProjectedValues[i] = aggregateCallDraftValue(agg.Calls[call], operand)
			}
		}
		return nil
	}

	// Regular projections.
	exprs := sq.projExprs
	if len(exprs) == 0 {
		return nil
	}
	resolver := buildProjectionResolverWithCTEScopes(sq, md, schemaName, cteScopes)
	if resolver == nil {
		resolver = buildSelectScope(sq, md, schemaName, cteScopes)
	}
	if resolver == nil {
		return nil
	}
	if subqPlanner != nil {
		resolver.SetSubqueryPlanner(subqPlanner)
	}
	vals := make([]values.Value, len(proj.Projections))
	copy(vals, proj.ProjectedValues)
	aggSlots := make([]bool, len(proj.Projections))
	for i, e := range exprs {
		if i >= len(vals) {
			break
		}
		if e == nil {
			continue
		}
		v, err := resolver.WalkExpressionForProjection(e)
		if err != nil {
			var apiErr *api.Error
			if errors.As(err, &apiErr) {
				return err
			}
			var corrErr *CorrelatedExistsError
			if errors.As(err, &corrErr) {
				// Route through the SAME classifier the WHERE-EXISTS path uses so a
				// GENUINE resolution failure in the ON (missing column/source) reports
				// 42703/42702 and only a DELIBERATE Unsupported decline reports 0A000 —
				// the projected and WHERE forms then agree on the SQLSTATE.
				if mapped := mapPredicateWalkError(err); mapped != nil {
					return mapped
				}
				return api.NewError(api.ErrCodeUnsupportedOperation, corrErr.Error())
			}
			// RFC-141 R4 (P1b): a SELECT item with a NESTED EXISTS is not
			// foldable; reject cleanly rather than fall through to the text path
			// (which would evaluate the ExistsValue with a dead binding → wrong).
			var nestedExists *expr.NestedExistsProjectionError
			if errors.As(err, &nestedExists) {
				return api.NewError(api.ErrCodeUnsupportedQuery, nestedExists.Error())
			}
			// A resolved expression's semantic failure is not an unsupported
			// syntax decline. Preserve it instead of leaving an empty value slot.
			if mapped := mapPredicateWalkError(err); mapped != nil {
				return mapped
			}
			continue
		}
		if fn := unsafeScalarFunctionName(v); fn != "" {
			// Java rejects the call by NAME, during encapsulation:
			// "Unsupported operator IF". Report it here, where the name is in
			// hand. Dropping the slot instead leaves the projection with no
			// resolved Value, and the translator's later refusal can only say
			// `projection slot 0 has no resolved Value` — which names neither
			// the function nor the query's actual problem, and is the same
			// sentence an unrelated resolution gap produces.
			return api.NewError(api.ErrCodeUnsupportedQuery, "Unsupported operator "+fn)
		}
		aggSlots[i] = containsAggregate(v) // pre-rewrite: aggregate nodes still present
		if aggSlots[i] {
			return api.NewError(api.ErrCodeUnsupportedQuery,
				"aggregate expression reached a projection without an aggregate output owner")
		}
		vals[i] = v
	}
	proj.ProjectedValues = vals
	proj.AggregateSlots = aggSlots
	return nil
}

func aggregateCallDraftValue(call logical.AggregateCall, operand values.Value) values.Value {
	var op values.AggregateOp
	switch strings.ToUpper(call.Func) {
	case "COUNT":
		if call.Star {
			op = values.AggCountStar
		} else {
			op = values.AggCount
		}
	case "SUM":
		op = values.AggSum
	case "MIN":
		op = values.AggMin
	case "MAX":
		op = values.AggMax
	case "AVG":
		op = values.AggAvg
	default:
		return nil
	}
	return &values.AggregateValue{Op: op, Operand: operand}
}

// aggregateNativeOutputName is the name at one ordinal of the aggregate's
// native output row: a group key first (by its resolved Value, else its Bare,
// else its Display), then a call by its CanonicalName.
//
// VERBATIM on every arm. Those are the same three inputs — key.Bare,
// key.Display, CanonicalName() — that the translator's aggregateOutputColumns
// publishes, and the two must spell one row the same way. The output reaches
// SortKey.Expr and the explain name, so a fold here renames a column the
// authority named otherwise.
func aggregateNativeOutputName(agg *logical.LogicalAggregate, ordinal int) string {
	if agg == nil || ordinal < 0 {
		return ""
	}
	if ordinal < len(agg.GroupKeys) {
		key := agg.GroupKeys[ordinal]
		if key.Value != nil {
			return aggregateGroupKeyOutputName(key.Value)
		}
		if key.Bare != "" {
			return key.Bare
		}
		return key.Display
	}
	callIdx := ordinal - len(agg.GroupKeys)
	if callIdx < 0 || callIdx >= len(agg.Calls) {
		return ""
	}
	return agg.Calls[callIdx].CanonicalName()
}

// unsafeScalarFunctionName returns the name of the first scalar function in v
// that Java's Cascades planner has no catalogue entry for, or "" when the whole
// tree is supported.
//
// It reports the NAME rather than a bool because that name is the whole
// rejection Java gives ("Unsupported operator IF"), and a caller that only
// learns "unsupported" has to drop the slot and let something downstream
// report a projection with no resolved Value — which names neither the
// function nor anything the user can act on.
func unsafeScalarFunctionName(v values.Value) string {
	var unsupported string
	values.WalkValue(v, func(node values.Value) bool {
		if sf, ok := node.(*values.ScalarFunctionValue); ok {
			if !cascadesSafeScalarFunction(sf.FuncName) {
				unsupported = sf.FuncName
				return false
			}
		}
		return true
	})
	return unsupported
}

func cascadesSafeScalarFunction(name string) bool {
	return values.IsCascadesSafeScalarFunction(name)
}

// aggColOperandText recomputes one parsed aggregate column's operand text, by
// the same derivation the producer uses before it applies its qualifier strip.
// It is the value `AggCallProvenance.Operand` was recorded from, so the two
// agree exactly whenever the column has not moved.
//
// ONE SPELLING, NOT TWO, and the second was not merely redundant — it was a
// hole. An earlier draft also offered the argument with its LEADING segment
// removed, carried over from the pre-RFC-241 matcher where it accommodated the
// producer's strip. Once the comparison moved to the PRE-strip text that
// accommodation has nothing to do: the recorded value is derived by these exact
// lines, so it matches this spelling by construction, and a second spelling can
// therefore only ever match when the first does NOT — i.e. precisely when the
// column HAS changed, which is the event being watched. Concretely, a call
// recorded as `X` validated clean against a column now rendering `A.X`. Widening
// a checksum with an alternative that only fires on corruption inverts it.
func aggColOperandText(ac aggSelectCol) string {
	arg := ac.aggArg
	if arg == "" && ac.aggExpr != nil {
		arg = aggOperandCanonicalText(ac.aggExpr)
	}
	if arg == "" {
		arg = "*"
	}
	return arg
}

// validateAggCallProvenance refuses a recorded call-to-column correspondence
// that no longer describes the columns in hand.
//
// The correspondence is recorded by the producer and consumed here, and the two
// are separated by the whole of logical building — so the table is only as good
// as the assumption that `aggCols` did not move underneath it. Rather than
// forbid from the outside every code shape that could move it (a list keyed on
// an identifier cannot see a write that never names it — a whole-struct
// assignment replaces the slice header while mentioning nothing), this asserts
// the correspondence still holds where it is about to be USED.
//
// The operand comparison is EXACT and must stay exact. A case-insensitive one
// is the RFC-241 defect itself, and putting it back here would reintroduce that
// defect inside the guard against it.
//
// It is a checksum on a structural bind, not a selector: the recorded index
// decides which call a column feeds, and this only refuses. Both sides derive
// the operand text through the same renderer, which is what makes the check
// sound rather than vacuous: a change to that renderer moves both sides and the
// check keeps passing — correctly, since re-rendering does not invalidate the
// correspondence — while a change to `aggCols` moves one side only, which is
// precisely the event being watched.
//
// Blind in exactly one case, by subsumption rather than by hope: two columns
// sharing function, DISTINCT-ness and canonical operand text. The bind is then
// interchangeable, because the resolved Value is identical either way.
func validateAggCallProvenance(agg *logical.LogicalAggregate, aggCols []aggSelectCol) error {
	if !agg.HasCallProvenance {
		// Built by a producer that resolves its own operands (the correlated
		// scalar-subquery path) and never needs this pass. Nothing to check.
		//
		// Tested on the FLAG, not on the slice being nil: this pass nils the
		// slice when it is done with it, so nil alone would mean both "never
		// recorded" and "already consumed" — and the second would resolve
		// nothing, silently.
		return nil
	}
	if agg.CallProvenance == nil {
		return api.NewErrorf(api.ErrCodeInternalError,
			"aggregate call provenance was recorded and has already been consumed; "+
				"operand resolution ran twice on one aggregate")
	}
	if len(agg.CallProvenance) != len(agg.Calls) {
		return api.NewErrorf(api.ErrCodeInternalError,
			"aggregate call provenance is %d entries for %d calls",
			len(agg.CallProvenance), len(agg.Calls))
	}
	if agg.CallProvenanceCols != len(aggCols) {
		return api.NewErrorf(api.ErrCodeInternalError,
			"aggregate columns moved between lowering and operand resolution: "+
				"recorded %d, have %d", agg.CallProvenanceCols, len(aggCols))
	}
	for i, prov := range agg.CallProvenance {
		if prov.AggColIdx < 0 {
			continue // synthesized COUNT(*): no parsed column, no operand
		}
		if prov.AggColIdx >= len(aggCols) {
			return api.NewErrorf(api.ErrCodeInternalError,
				"aggregate call %d names parsed column %d of %d",
				i, prov.AggColIdx, len(aggCols))
		}
		ac, call := aggCols[prov.AggColIdx], agg.Calls[i]
		if call.Func != strings.ToUpper(ac.aggFunc) || call.Distinct != ac.aggDistinct {
			return api.NewErrorf(api.ErrCodeInternalError,
				"aggregate call %d (%s, distinct=%v) no longer matches parsed column %d (%s, distinct=%v)",
				i, call.Func, call.Distinct, prov.AggColIdx, strings.ToUpper(ac.aggFunc), ac.aggDistinct)
		}
		// Compared on the PRE-STRIP text recorded by the producer, never on
		// call.Operand: the producer applies a qualifier strip this side cannot
		// reproduce, so comparing the stripped form against a recomputed
		// unstripped one rejects valid queries — `SUM(d.a + d.b)` over a derived
		// table renders `D.A+D.B`, stores `A+D.B`, and matches no spelling.
		if want := aggColOperandText(ac); prov.Operand != want {
			return api.NewErrorf(api.ErrCodeInternalError,
				"aggregate call %d recorded operand %q but parsed column %d now renders %q",
				i, prov.Operand, prov.AggColIdx, want)
		}
	}
	return nil
}

func upgradeAggregateOperands(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) error {
	agg := findAggregate(op)
	if agg == nil {
		return nil
	}
	// The side table is parse-time provenance and this is its only consumer, so
	// it stops existing when this function returns — however it returns. The
	// resolver-nil path below is the one that matters: it leaves a POPULATED
	// table on a live aggregate that nothing validated, which is exactly the
	// state a later reader would most reasonably trust.
	defer func() {
		agg.CallProvenance = nil
		agg.CallProvenanceCols = 0
	}()
	if err := validateAggCallProvenance(agg, sq.aggCols); err != nil {
		return err
	}
	resolver := buildProjectionResolverWithCTEScopes(sq, md, schemaName, cteScopes)
	if resolver == nil {
		resolver = buildSelectScope(sq, md, schemaName, cteScopes)
	}
	if resolver == nil {
		return nil
	}
	operands := make([]values.Value, len(agg.Calls))
	for aggColIdx, ac := range sq.aggCols {
		// A PLAIN-column aggregate arg (`MIN(pid)`, `MIN(c2.pid)`) carries
		// aggArg only — the parser's resolveArg captures no aggExpr for a bare
		// FullColumnName. It must STILL resolve here: the translator's
		// bare-column lazy read keeps a qualified arg as ONE opaque dotted
		// FieldValue{"C2.PID"}, which key-misses the scan row's bare "PID" at
		// accumulation and silently aggregates NULL (a sub-planned scalar
		// subquery's rows carry bare keys only). COUNT(*) has neither and skips.
		if ac.aggFunc == "" || (ac.aggExpr == nil && ac.aggArg == "") {
			continue
		}
		// A HAVING that repeats a SELECT-list aggregate
		// (`SELECT SUM(x) … HAVING SUM(x) > k`) can create a SECOND slot with
		// the same call shape, so a column may feed several calls; leaving any
		// of them unresolved makes the translator fall back to the lazy
		// bare-column read, whose flat dotted operand refs the ordinal frontier
		// cannot resolve.
		//
		// Which calls this column produced is a fact the producer RECORDED, not
		// one to be reconstructed from a rendering. Reconstructing it by folding
		// the operand text is the RFC-241 defect: that text carries identifiers
		// and string literals alike, and a fold matched `…'us'…` against
		// `…'US'…`, so both columns wrote both slots and the second clobbered
		// the first. `spellings` survives only to VALIDATE the recorded index
		// (see validateAggCallProvenance), never to choose one.
		idxs := agg.CallsFromAggCol(aggColIdx)
		if len(idxs) == 0 {
			continue
		}
		var v values.Value
		if ac.aggExpr != nil {
			walked, err := resolver.WalkExpression(ac.aggExpr)
			if err != nil {
				if mapped := mapPredicateWalkError(err); mapped != nil {
					return mapped
				}
				continue
			}
			v = walked
		} else {
			// Plain-column arg: resolve through the semantic scope
			// (ResolveIdentifier — the same resolution a WHERE reference gets),
			// so a qualified `c2.pid` binds against its FROM source instead of
			// surviving as opaque dotted text. Unresolvable → fall through to
			// the text path (fail-soft).
			bareArg := ac.aggArgBare
			if bareArg == "" {
				bareArg = ac.aggArg
			}
			qv, rerr := resolver.ResolveIdentifierPath(
				colRefIdentifiers(bareArg, ac.aggArgQualifier, ac.aggArgQualified, ac.aggArgSegs))
			if rerr != nil || qv == nil {
				var unresArg *expr.UnresolvableOrdinalError
				if errors.As(rerr, &unresArg) {
					return unresArg
				}
				continue
			}
			v = qv
		}
		for _, idx := range idxs {
			operands[idx] = v
		}
	}
	agg.AggregateOperands = operands

	// Resolve GROUP BY key Values. Two sources:
	//   - COMPUTED keys (`GROUP BY x.col1 + x.col2`) carry a parse expr in
	//     sq.groupByExprs[i] → resolve via WalkExpressionForProjection.
	//   - SIMPLE column keys (`GROUP BY V`) do NOT populate groupByExprs, so they
	//     fall through to the translator's bare FieldValue{V} fallback. For a bare
	//     key that binds to a lateral-unnest SHADOWING source (`FROM t, t.arr AS V,
	//     u` where a LATER FROM item u also has a column V), that bare FieldValue
	//     reads the merged row's bare `V` key — which mergeRows overwrites
	//     last-leg-wins with u.V — so grouping happens on the LATER table's column,
	//     not the shadowing unnest element (P2a, silent-wrong
	//     grouping). Route the simple key through ResolveColumnShadowingQualified —
	//     the SAME helper the projection (buildSelectShell) and ORDER-BY
	//     (qualifyShadowedSortKeys) paths use — so a key that binds to the unnest
	//     resolves to the QUALIFIED `V.V` (which mergeRows preserves verbatim),
	//     grouping on the unnest element. An explicitly-qualified `u.V` key binds to
	//     u's real source (not Shadowing) → ok=false → left for the bare fallback,
	//     so the control (group by the later column) is unaffected. RFC-142.
	keyValues := make([]values.Value, len(agg.GroupKeys))
	filled := false
	for i := range agg.GroupKeys {
		if i < len(sq.groupBy) && sq.groupBy[i].expr != nil {
			v, err := resolver.WalkExpressionForProjection(sq.groupBy[i].expr)
			if err != nil {
				if mapped := mapPredicateWalkError(err); mapped != nil {
					return mapped
				}
				return err
			}
			keyValues[i] = v
			filled = true
			continue
		}
		gk := agg.GroupKeys[i]
		// Structured segments, never a re-parse of the display text: a
		// delimited identifier containing a dot stays one bare segment.
		ref := colRef{table: gk.Qualifier, col: gk.Bare}
		if gk.Bare == "" {
			ref = colRef{col: gk.Display}
		}
		// THE STRUCT-DESCENT ARM. A grouping key whose segments descend INTO a
		// struct column (`GROUP BY r.v.z`) is resolved by the SAME identifier
		// resolver every other column reference uses, producing ONE fused
		// multi-accessor FieldValue — Java's ofFieldsAndFuseIfPossible. The
		// arms below all read a qualified key as `source . column`, which for
		// `r.v.z` asks the scope for a source named "R.V"; nothing answers, and
		// the key degraded to a flat dotted FieldValue{"R.V.Z"} that no runtime
		// row can address (`ordinal -1 … malformed plan`). That degraded mint —
		// not a missing capability in the aggregate pipeline — is what made a
		// nested grouping key unsupported: the executor already evaluates the
		// key Value against the row (Java's StreamGrouping.evalGroupingKey), so
		// a correctly-minted nested key needs nothing downstream.
		//
		// It is a CO-EQUAL candidate, not a fallback tried after the others
		// fail. The one-population ambiguity rule lives inside the shared
		// resolver (ResolveColumnRefPath counts every prefix split once and
		// spends 42702 when two resolve — SemanticAnalyzer.java:430,:436), so
		// asking it FIRST is what keeps order of attempt from becoming a
		// semantics. Resolving the collision by which arm ran first is exactly
		// what TestScope_NestedDescent_CollidesWithSourceAliasAsAmbiguity
		// forbids.
		//
		// The predicate is the one the retired refusal used, so the arm covers
		// exactly the set that refusal turned away and no other key changes
		// route.
		if gk.Qualified && gk.Bare != "" {
			descentIDs := colRefIdentifiers(ref.bare(), gk.Qualifier, gk.Qualified, gk.Segs)
			if nested, derr := resolver.DescendsIntoStructPath(descentIDs); derr == nil && nested {
				var dv values.Value
				if len(sq.joins) > 0 {
					// Over a join the descent's ROOT must be the
					// quantifier-addressed source-relative bake, exactly as the
					// qualified projection binds it: a childless bake carries an
					// ordinal relative to its own source row and would misread
					// another leg's slot over the merged row.
					dv = resolveQualifiedBakedPath(resolver, descentIDs)
				} else if v, derr2 := resolver.ResolveIdentifierPath(descentIDs); derr2 == nil {
					dv = v
				} else {
					var unresDescent *expr.UnresolvableOrdinalError
					if errors.As(derr2, &unresDescent) {
						return unresDescent
					}
				}
				if dv != nil {
					keyValues[i] = dv
					filled = true
					continue
				}
			}
		}
		var qualID semantic.Identifier
		if gk.Qualified {
			qualID = semantic.FromNormalized(gk.Qualifier)
			// The dup-alias twin: a qualified key
			// binding a LATER duplicate-alias leg must group by the BINDING
			// correlation (`Q$DUP1.QID`) — the join row's actual namespace —
			// never the display alias, whose bare FieldValue fallback misses
			// and silently groups every row under NULL. Same helper as the
			// projection and ORDER-BY paths (qualifyShadowedSortKeys), so the
			// three cannot diverge; nil for every non-duplicate reference.
			// An AmbiguousColumnError here is DISCARDED on purpose: the
			// upstream group-key reference validation already terminated an
			// ambiguous key with 42702 before this pass runs (the ladder's
			// >=2 arm is owned there, not here).
			qv, qerr := resolver.ResolveQualifiedProjection(qualID, semantic.FromNormalized(ref.bare()))
			if qerr == nil && qv != nil {
				keyValues[i] = qv
				filled = true
				continue
			}
			var unres *expr.UnresolvableOrdinalError
			if errors.As(qerr, &unres) {
				// Born-baked (slice 2): never fall to the name channel.
				return unres
			}
			// Every other QUALIFIED group key resolves through
			// the scope to the quantifier-addressed source-relative baked
			// reference (QOV(leg).col with the construction-bound ordinal) —
			// the executor binds the leg's window off the merged row's own leg
			// boundaries (rowLegsBinder), so the key resolves positionally
			// instead of through a flat dotted "ALIAS.COL" name
			// read. The group-key OUTPUT column is labeled by the BARE field
			// (AggregateKeyColumnName) — the unified Java label rule.
			if bv := resolveQualifiedBaked(resolver, ref); bv != nil {
				keyValues[i] = bv
				filled = true
				continue
			}
			// A qualified reference over ONE source resolves to the same
			// childless source-relative bake as its unqualified twin: the
			// qualifier is redundant once the scope proves there is no leg
			// choice. resolveQualifiedBaked intentionally accepts only
			// QOV-addressed multi-source references, so retain this exact
			// single-source result explicitly. On a join, childless would lose
			// the defining leg and remains forbidden.
			if len(sq.joins) == 0 {
				rv, rerr := resolver.ResolveIdentifier(qualID, semantic.FromNormalized(ref.bare()))
				if rerr == nil {
					if fv := resolveBaked(rv, true); fv != nil {
						keyValues[i] = fv
						filled = true
						continue
					}
				}
				var unresKey *expr.UnresolvableOrdinalError
				if errors.As(rerr, &unresKey) {
					return unresKey
				}
			}
		}
		qv, ok, err := resolver.ResolveColumnShadowingQualified(qualID, semantic.FromNormalized(ref.bare()))
		if err == nil && ok {
			keyValues[i] = qv
			filled = true
		}
		var unresShadow *expr.UnresolvableOrdinalError
		if errors.As(err, &unresShadow) {
			return unresShadow
		}
		// A BARE non-shadowed group key resolves through the scope so it
		// carries the construction-bound ordinal, by the SAME shape rule as
		// every other binding site (resolveBaked). Field stays the bare
		// column, so the aggregate's OUTPUT column name
		// (AggregateKeyColumnName = Field) and every downstream name-keyed
		// consumer are unchanged. Qualified keys and unresolvable names keep
		// the translator's name emission.
		//
		// MULTI-SOURCE resolutions no longer fall through to the name, which
		// is what this comment used to say: resolveBaked's child-bearing arm
		// admits them. Of the two folded sites here this is the busier (7 firings
		// to the bare-projection twin's 1), though the PlanVisitor site outside
		// this file takes the same arm 131 times (RFC-223).
		if keyValues[i] == nil && !ref.isQualified() {
			rv, rerr := resolver.ResolveIdentifier(semantic.Identifier{}, semantic.FromNormalized(ref.bare()))
			if rerr == nil {
				if fv := resolveBaked(rv, true); fv != nil {
					keyValues[i] = fv
					filled = true
				}
			}
			var unresKey *expr.UnresolvableOrdinalError
			if errors.As(rerr, &unresKey) {
				return unresKey
			}
		}
	}
	if filled {
		for i := range agg.GroupKeys {
			if keyValues[i] != nil {
				agg.GroupKeys[i].Value = keyValues[i]
			}
		}
	}
	return groupByOutputConstructionPullUp(agg)
}

// groupByOutputConstructionPullUp is Java's group-by OUTPUT construction guard,
// and it is the one that decides duplicate grouping keys for every shape.
//
// LogicalOperator.java:454 builds the operator's output by pulling the grouping
// expressions up against the GroupByExpression's own result value:
//
//	groupByExpressions.concat(aggregates).pullUp(groupByExpression.getResultValue(), …)
//
// That routes through the ASSERTING Expressions.pullUp (Expressions.java:112),
// whose multimap holds one entry per matched result column. The sub-expressions
// being pulled up ARE the grouping keys, so two semantically equal keys produce
// two entries for one key and the `size() == 1` assert raises AMBIGUOUS_COLUMN.
//
// THREE CONSEQUENCES, each of which was got wrong here before:
//
//  1. It fires at OPERATOR CONSTRUCTION, before the SELECT-list
//     (QueryVisitor.java:301) or HAVING (:303) pull-up is reached, and
//     INDEPENDENTLY of whether any post-aggregate reference exists. A bare
//     `SELECT COUNT(*) … GROUP BY a.amount, amount` has nothing to pull up
//     later and Java still refuses it — measured, `join_qualified_vs_bare` in
//     conformance/duplicate_groupby_java_probe_test.go.
//  2. It therefore covers the PROJECTED half, which reaches none of the
//     post-aggregate guards: a projected key is bound by
//     buildAggregateOutputSlots, whose match is name-based. Instrumented, that
//     site binds `A.R.V.Z`→key 0 and `R.V.Z`→key 1 while the post-aggregate walk
//     reports two matches for the same query. One shape, two verdicts, decided by
//     two different predicates — an incoherence with no Java counterpart, which a
//     construction-time guard removes rather than documents.
//  3. It is NOT the name-based duplicate gate (groupKeysEquivalent) and does not
//     replace it. The gate keeps its own predicate; this adds the SEMANTIC
//     question Java asks, at the point Java asks it.
//
// The relation is the pull-up's own matcher, so the construction guard and the
// post-aggregate guards cannot disagree about what "the same key" means.
func groupByOutputConstructionPullUp(agg *logical.LogicalAggregate) error {
	if agg == nil || len(agg.GroupKeys) < 2 {
		return nil
	}
	for i := range agg.GroupKeys {
		ki := agg.GroupKeys[i].Value
		if ki == nil {
			continue
		}
		for j := range agg.GroupKeys {
			if j == i || agg.GroupKeys[j].Value == nil {
				continue
			}
			if !groupKeysPullUpEqual(ki, agg.GroupKeys[j].Value) {
				continue
			}
			// Java's Assert throws on the FIRST ambiguous sub-expression, so the
			// name reported is the first key that answers to more than one
			// column — not whichever the scan happened to end on.
			return api.NewErrorf(api.ErrCodeAmbiguousColumn, "Ambiguous columns for %s",
				aggregateGroupKeyOutputName(ki))
		}
	}
	return nil
}

// groupKeysPullUpEqual is the identity the pull-up uses, applied key-to-key. A
// QUALIFIED key carries its correlation, so two reads of one column through two
// DIFFERENT quantifiers (`GROUP BY o.k, i.k`) are structurally unequal and stay
// two keys; two spellings of the SAME column (`a.amount` and bare `amount` over
// one source) are equal and are one.
func groupKeysPullUpEqual(a, b values.Value) bool {
	return values.ValuesStructurallyEqual(a, b) ||
		values.SemanticEqualsUnderAliasMap(a, b, values.EmptyAliasMap())
}

func upgradeHavingPredicate(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource, subqPlanner *existsSubqueryPlanner) error {
	agg := findAggregate(op)
	if agg == nil || sq.havingExpr == nil {
		return nil
	}
	resolver := buildProjectionResolverWithCTEScopes(sq, md, schemaName, cteScopes)
	if resolver == nil {
		resolver = buildSelectScope(sq, md, schemaName, cteScopes)
	}
	if resolver == nil {
		return nil
	}
	// Install the SubqueryPlanner so EXISTS subqueries in HAVING can be planned.
	if subqPlanner != nil {
		// Reset subqueries so the HAVING walk starts fresh.
		subqPlanner.subqueries = nil
		subqPlanner.scalarSubqueries = nil
		resolver.SetSubqueryPlanner(subqPlanner)
	}
	pred, err := resolver.WalkPredicate(sq.havingExpr)
	if err != nil {
		// SEMANTIC errors surface with Java's codes: a bare HAVING re-read
		// of an ambiguous grouped column is 42702 (Java AMBIGUOUS_COLUMN),
		// exactly like the ORDER-BY twin — not a planner decline. An
		// unbindable ordinal is loud per born-baked (slice 2). Every OTHER
		// walk failure keeps the HasHaving decline sentinel: the translator
		// rejects a set-but-unresolved HAVING, so nothing is dropped.
		var ambig *semantic.AmbiguousColumnError
		if errors.As(err, &ambig) {
			return api.NewErrorf(api.ErrCodeAmbiguousColumn,
				"Ambiguous reference %s", ambig.Reference())
		}
		var unres *expr.UnresolvableOrdinalError
		if errors.As(err, &unres) {
			return unres
		}
		return nil
	}
	// Keep HAVING as a structural draft. Its aggregate/group-key references
	// cannot become FieldValues until translateAggregate owns the physical
	// GroupBy quantifier. Validate that every source reference is pullable now,
	// while aggregate identity is still available, then let the translator bind
	// the same tree against that real quantifier.
	if err := validatePostAggregatePredicateDraft(pred, agg); err != nil {
		return err
	}
	agg.HavingPredicate = pred
	if subqPlanner != nil && len(subqPlanner.subqueries) > 0 {
		agg.HavingExistsSubqueries = subqPlanner.subqueries
		subqPlanner.subqueries = nil
	}
	if subqPlanner != nil && len(subqPlanner.scalarSubqueries) > 0 {
		agg.HavingScalarSubqueries = subqPlanner.scalarSubqueries
		subqPlanner.scalarSubqueries = nil
	}
	return nil
}

func rewriteAggregateRefsInPredicate(pred predicates.QueryPredicate, agg *logical.LogicalAggregate) predicates.QueryPredicate {
	switch p := pred.(type) {
	case *predicates.ComparisonPredicate:
		lhs := rewriteAggregateValuesInTree(p.Operand, agg)
		// Copy the whole Comparison and replace ONLY the rewritten RHS operand,
		// preserving Escape and every other Comparison field. A fresh
		// {Type, Operand} would drop the LIKE escape rune (and the parameter /
		// text / distance-rank metadata) and change comparison semantics. RFC-142.
		cmp := p.Comparison
		cmp.Operand = rewriteAggregateValuesInTree(p.Comparison.Operand, agg)
		return predicates.NewComparisonPredicate(lhs, cmp)
	case *predicates.AndPredicate:
		rewritten := make([]predicates.QueryPredicate, len(p.SubPredicates))
		for i, sub := range p.SubPredicates {
			rewritten[i] = rewriteAggregateRefsInPredicate(sub, agg)
		}
		return predicates.NewAnd(rewritten...)
	case *predicates.OrPredicate:
		rewritten := make([]predicates.QueryPredicate, len(p.SubPredicates))
		for i, sub := range p.SubPredicates {
			rewritten[i] = rewriteAggregateRefsInPredicate(sub, agg)
		}
		return predicates.NewOr(rewritten...)
	case *predicates.NotPredicate:
		return predicates.NewNot(rewriteAggregateRefsInPredicate(p.Child, agg))
	}
	return pred
}

// containsAggregate reports whether v's value tree contains any
// *values.AggregateValue. Called PRE-rewrite (before
// rewriteAggregateValuesInTree replaces aggregates with typed FieldValue
// references) so the INSERT…SELECT promotion guard can mark which projection
// slots are aggregate-derived. Tree-walk, not a top-level type assert:
// `AVG(x)+1` is a top-level ArithmeticValue that still resolves to DOUBLE and
// must be guarded.
func containsAggregate(v values.Value) bool {
	found := false
	values.WalkValue(v, func(n values.Value) bool {
		if _, ok := n.(*values.AggregateValue); ok {
			found = true
			return false // stop descending
		}
		return !found
	})
	return found
}

func rewriteAggregateValuesInTree(v values.Value, agg *logical.LogicalAggregate) values.Value {
	if v == nil {
		return nil
	}
	if _, ok := v.(*values.AggregateValue); ok {
		return rewriteAggregateValue(v, agg)
	}
	if av, ok := v.(*values.ArithmeticValue); ok {
		return &values.ArithmeticValue{
			Op:    av.Op,
			Left:  rewriteAggregateValuesInTree(av.Left, agg),
			Right: rewriteAggregateValuesInTree(av.Right, agg),
		}
	}
	if sf, ok := v.(*values.ScalarFunctionValue); ok {
		args := make([]values.Value, len(sf.Args))
		for i, a := range sf.Args {
			args[i] = rewriteAggregateValuesInTree(a, agg)
		}
		return values.NewScalarFunctionValue(sf.FuncName, sf.Typ, args...)
	}
	if cv, ok := v.(*values.CastValue); ok {
		return values.NewCastValue(rewriteAggregateValuesInTree(cv.Child, agg), cv.Target)
	}
	// A machine-inserted PromoteValue (expr.promoteColumnColumnNumeric,
	// RelOpValue's Java analogue: `HAVING SUM(int_col) > 5.5` rewrites to
	// PromoteValue(SUM(int_col), DOUBLE) > 5.5) can wrap an AggregateValue
	// exactly like CastValue can — without this case the aggregate stayed
	// buried one level down, unrewritten, and reached AggregateValue.Evaluate
	// at row time via the residual filter path (which always errors — an
	// aggregate has no per-row scalar semantics), turning a legal
	// `HAVING aggregate > scalar-subquery` of mismatched numeric types into
	// "42803: aggregate function is not allowed here".
	if pv, ok := v.(*values.PromoteValue); ok {
		return values.NewPromoteValue(rewriteAggregateValuesInTree(pv.Child, agg), pv.Target)
	}
	if pv, ok := v.(*values.PickValue); ok {
		alts := make([]values.Value, len(pv.Alternatives))
		for i, a := range pv.Alternatives {
			alts[i] = rewriteAggregateValuesInTree(a, agg)
		}
		return values.NewPickValue(rewriteAggregateValuesInTree(pv.Selector, agg), alts, pv.Typ)
	}
	if cs, ok := v.(*values.ConditionSelectorValue); ok {
		impl := make([]values.Value, len(cs.Implications))
		for i, c := range cs.Implications {
			impl[i] = rewriteAggregateValuesInTree(c, agg)
		}
		return values.NewConditionSelectorValue(impl)
	}
	if ph, ok := v.(expr.PredicateValueHolder); ok {
		rewritten := rewriteAggregateRefsInPredicate(ph.GetPredicate(), agg)
		ph.SetPredicate(rewritten)
		return ph
	}
	return v
}

// aggregateCallOutputSlot is THE structural matcher from an AggregateValue to
// the slot that aggregate occupies in the aggregate's output row, and the sole
// place a post-aggregate reference to an aggregate is born. It answers with a
// FieldValue whose ordinal is RECORDED — the call's index in agg.Calls, offset
// by the group keys, which is precisely the [keys..., calls...] order
// GroupByOutputColumnNames and the executor's aggregateCursor emit.
//
// The match is on the aggregate's IDENTITY (function plus semantic operand
// equality), never on a rendered name. That matters because the two renderings
// that would otherwise have to agree are produced by different code from
// different inputs — canonicalAggName walks the RESOLVED operand Value while
// AggregateResultColumnName renders the PARSE TEXT the builder captured — so a
// name channel between them is a coincidence the compiler cannot check.
//
// The canonical-name fallback survives for the one case identity cannot serve:
// a catalog-free or constant call that carries no resolved AggregateOperands
// entry, where the call's own canonical rendering is the only identity there
// is. It stays a fallback, not a first choice.
//
// The result is PINNED: the ordinal is final against the executor's assembled
// output row, not relative to any source's declared column order. That pin is
// what stops groupByOutputBaker from discarding a slot decided here and
// recovering one from a last-wins map keyed by the rendered output name.
func aggregateCallOutputSlot(av *values.AggregateValue, agg *logical.LogicalAggregate) (int, bool) {
	if av == nil || agg == nil {
		return -1, false
	}
	var matches []int
	for i, call := range agg.Calls {
		if av.Op == values.AggCountStar {
			if call.Star {
				matches = append(matches, i)
			}
			continue
		}
		if call.Star || !strings.EqualFold(call.Func, av.Op.Symbol()) {
			continue
		}
		if i < len(agg.AggregateOperands) && agg.AggregateOperands[i] != nil &&
			values.SemanticEqualsUnderAliasMap(av.Operand, agg.AggregateOperands[i], values.EmptyAliasMap()) {
			matches = append(matches, i)
		}
	}
	if len(matches) == 0 {
		want := normalizeAggregateBindingName(canonicalAggName(av.Op.Symbol(), av.Operand))
		for i, call := range agg.Calls {
			if normalizeAggregateBindingName(call.CanonicalName()) == want {
				matches = append(matches, i)
			}
		}
	}
	if len(matches) == 0 {
		return -1, false
	}
	// Repeated identical aggregate calls are value-equivalent; the first
	// native slot is a deterministic, semantics-preserving bind.
	return len(agg.GroupKeys) + matches[0], true
}

// validatePostAggregateValueDraft proves that every aggregate/source field in
// a post-aggregate expression has exactly one native output slot. It
// deliberately does not publish a replacement Value: the logical builder has
// no quantifier that owns the aggregate row. translateProject/translateSort/
// translateAggregate repeat this structural match while resolving the ordinal
// against their real physical input quantifier.
func validatePostAggregateValueDraft(v values.Value, agg *logical.LogicalAggregate) error {
	if v == nil || agg == nil {
		return api.NewError(api.ErrCodeUnsupportedQuery,
			"post-aggregate expression has no aggregate output layout")
	}

	var bindErr error
	values.WalkValue(v, func(node values.Value) bool {
		if bindErr != nil {
			return false
		}
		if av, ok := node.(*values.AggregateValue); ok {
			if _, bindOK := aggregateCallOutputSlot(av, agg); !bindOK {
				bindErr = api.NewError(api.ErrCodeUnsupportedQuery,
					"post-aggregate expression could not bind an aggregate call to the native output row")
			}
			// The aggregate call denotes one native output slot as a whole. Its
			// source operand belongs below the aggregate and must not be
			// reinterpreted as a reference on the output row.
			return false
		}

		// Pre-order matching binds a whole computed GROUP BY expression before
		// considering any of its leaves.
		//
		// IT COLLECTS RATHER THAN FIRST-MATCHING, because Java's pull-up asserts
		// `pulledUpExpressionMap.get(subExpression).size() == 1` with
		// AMBIGUOUS_COLUMN (Expressions.java:112) before taking the element, and
		// this binder is a pull-up.
		//
		// IT IS NOT WHAT CLOSES THE DUPLICATE-KEY DIVERGENCE, and an earlier
		// revision of this comment claimed it was — asserting that a projected
		// reference under `... JOIN ... GROUP BY a.r.v.z, r.v.z` reaches THIS
		// loop. Instrumented, it does not: that reference is bound by
		// buildAggregateOutputSlots, and this loop is consulted zero times for
		// the shape. The claim was refuted by the file's own test, which had the
		// projected half planning — if the reference arrived here, the matcher
		// below is semantic and the two equal keys would have raised.
		//
		// Duplicates are refused upstream now, at output construction
		// (groupByOutputConstructionPullUp), which is where Java refuses them
		// (LogicalOperator.java:454) and which needs no reference at all. This
		// guard is therefore UNREACHABLE from SQL — measured over the whole
		// //pkg/relational/sqldriver target at 6158 subtests, this site is
		// consulted 414 times and never sees more than one match. It is kept
		// because Java keeps the assert at every pull-up site, and it is driven
		// directly by TestGroupKeyPullUpGuard_ExactBoundaryBinderRefusesAMultiMatch
		// so that unreachable does not become untested.
		keyMatch, keyMatches := -1, 0
		for i, key := range agg.GroupKeys {
			if key.Value != nil &&
				(values.SemanticEqualsUnderAliasMap(node, key.Value, values.EmptyAliasMap()) ||
					fieldValueMatchesAggregateGroupKey(node, key.Value, agg)) {
				if keyMatches == 0 {
					keyMatch = i
				}
				keyMatches++
			}
		}
		if keyMatches > 1 {
			bindErr = api.NewErrorf(api.ErrCodeAmbiguousColumn, "Ambiguous columns for %s",
				aggregateGroupKeyOutputName(agg.GroupKeys[keyMatch].Value))
			return false
		}
		if keyMatch >= 0 {
			// A complete grouping-key expression likewise owns one output slot;
			// its leaves are source-row implementation detail.
			return false
		}
		if _, isField := values.AsFieldValue(node); isField {
			// This is an ORIGINAL resolver reference: replacement roots are
			// not revisited by values.Replace. If it was neither consumed as
			// an aggregate operand nor matched as a complete grouping-key
			// subtree, its source-relative ordinal has no meaning on the
			// private aggregate row. Even an ordinal that happens to fall
			// within nativeWidth would be a coincidental, silently wrong
			// reinterpretation.
			bindErr = api.NewError(api.ErrCodeUnsupportedQuery,
				"post-aggregate expression references a field outside the aggregate output contract")
			return false
		}
		return true
	})
	if bindErr != nil {
		return bindErr
	}
	return nil
}

func validatePostAggregatePredicateDraft(pred predicates.QueryPredicate, agg *logical.LogicalAggregate) error {
	_, err := predicates.TransformEmbeddedValuesChecked(pred, func(v values.Value) (values.Value, error) {
		if err := validatePostAggregateValueDraft(v, agg); err != nil {
			return nil, err
		}
		return v, nil
	})
	return err
}

// fieldValueMatchesAggregateGroupKey recognizes the one safe representation
// difference semantic equality preserves: a qualified read over a single
// source carries QOV(source), while the same source's GROUP BY key may already
// have had that redundant qualifier stripped. Resolved path and defining source
// still have to prove identical; a multi-source aggregate never gets this
// childless/qualified relaxation.
//
// The ordinal path is the identity (values.SameColumnPath — the domain-checked,
// non-negative form of Java's ordinal-only FieldPath.equals). The per-accessor
// DISPLAY-name check this used to AND on is gone: a group-key reference whose
// display name was aliased away still denotes the column its ordinal names, and
// refusing it was the name-as-identity conflation in its refusing direction. The
// name was load-bearing against Ordinal -1 name-only accessors, where ordinal
// equality is vacuous; SameColumnPath declines those outright, which covers the
// same hazard without consulting a display name.
func fieldValueMatchesAggregateGroupKey(candidate, key values.Value, agg *logical.LogicalAggregate) bool {
	_ = agg
	cf, cok := values.AsFieldValue(candidate)
	kf, kok := values.AsFieldValue(key)
	if !cok || !kok {
		return false
	}
	left, right := cf.Path().Ordinals(), kf.Path().Ordinals()
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	cq, cHasQOV := values.AsQuantifiedObjectValue(cf.ChildValue())
	kq, kHasQOV := values.AsQuantifiedObjectValue(kf.ChildValue())
	return cHasQOV && kHasQOV && cq.Correlation() == kq.Correlation() &&
		values.FlowedTypesEqual(cq, kq)
}

func normalizeAggregateBindingName(s string) string {
	return strings.ReplaceAll(s, " ", "")
}

// canonicalAggName is the single canonicaliser for an aggregate's result-row
// column name. Both the HAVING-predicate rewrite (rewriteAggregateValue) and the
// correlated-scalar-subquery aggregate builder name aggregates through it, so a
// HAVING reference always resolves against the materialised slot — they cannot
// drift. funcSymbol is the aggregate function symbol (e.g. "SUM", "COUNT", or
// the count-star op's "COUNT(*)"); operand is the (already-resolved) argument
// Value, or nil for a no-operand aggregate. The form mirrors what the executor's
// aggResultName produces: FN(<ExplainValue verbatim, spaces stripped, one
// outer-paren pair stripped>), with COUNT(*)/no-operand => "FN(*)".
func canonicalAggName(funcSymbol string, operand values.Value) string {
	fn := strings.ToUpper(funcSymbol)
	if fn == "COUNT(*)" {
		return "COUNT(*)"
	}
	inner := "*"
	if operand != nil {
		// VERBATIM. ColumnNameValue already renders each field by the name it
		// declares, so an upper-fold here only destroys one: a correlated
		// scalar subquery `(SELECT SUM(i."Amount") …)` labelled its column
		// SUM(I.AMOUNT) while the very same aggregate reached through GROUP
		// BY/HAVING labelled SUM(Amount). Two routes to one name, disagreeing.
		//
		// The whitespace strip STAYS and is not the same kind of edit: it
		// normalizes the RENDERING's spacing (`(A * B)` -> `(A*B)`), which both
		// sides of every comparison here derive from ColumnNameValue, so it is
		// symmetric and touches no identifier.
		inner = values.ColumnNameValue(operand)
		inner = strings.ReplaceAll(inner, " ", "")
		if len(inner) > 2 && inner[0] == '(' && inner[len(inner)-1] == ')' {
			inner = inner[1 : len(inner)-1]
		}
	}
	return fn + "(" + inner + ")"
}

func rewriteAggregateValue(v values.Value, agg *logical.LogicalAggregate) values.Value {
	// Kept as a compatibility-shaped helper for callers that only need a tree
	// walk. A logical builder cannot turn an AggregateValue into a FieldValue:
	// no aggregate-output quantifier exists here. The structural draft remains
	// intact until the query translator owns that quantifier.
	_ = agg
	return v
}

func findAggregate(op logical.LogicalOperator) *logical.LogicalAggregate {
	for cur := op; cur != nil; {
		if a, ok := cur.(*logical.LogicalAggregate); ok {
			return a
		}
		ch := cur.Children()
		if len(ch) != 1 {
			return nil
		}
		cur = ch[0]
	}
	return nil
}

func findProjection(op logical.LogicalOperator) *logical.LogicalProject {
	for cur := op; cur != nil; {
		if p, ok := cur.(*logical.LogicalProject); ok {
			return p
		}
		ch := cur.Children()
		if len(ch) != 1 {
			return nil
		}
		cur = ch[0]
	}
	return nil
}

func validateGroupByProjection(sq *selectQuery, md *recordlayer.RecordMetaData) error {
	// RFC-141 §8 safety guard: GROUP BY on an EXISTS expression (e.g. `GROUP BY
	// id, EXISTS(...)` where the EXISTS column is the grouping key) cannot be
	// folded — the aggregate path has no SubqueryPlanner, so the existential
	// never resolves to a Value and the group key silently evaluates to a
	// constant. Reject cleanly rather than ship a constant-false grouped column.
	// Structural detection (typed ANTLR node), no text matching.
	for _, gb := range sq.groupBy {
		if gb.expr != nil && expr.ContainsExistsAtom(gb.expr) {
			return api.NewError(api.ErrCodeUnsupportedQuery,
				"projected EXISTS in this query shape is not yet supported")
		}
	}

	groupBySet := make(map[string]bool, len(sq.groupBy))
	for _, gb := range sq.groupBy {
		groupBySet[strings.ToUpper(gb.display)] = true
		if gb.qualified {
			groupBySet[strings.ToUpper(gb.bare)] = true
		}
	}

	// Collect the field set from EVERY base-table source — the primary
	// table AND each join source — so a GROUP BY / projection key from a
	// joined table (`SELECT d.dname ... FROM emp e JOIN dept d ... GROUP BY
	// d.dname`) passes the existence check instead of falsely 42703-ing
	// because it isn't a column of the first table. If ANY source is a
	// derived table / CTE (no record type), its columns are unknowable, so
	// skip the existence check entirely (tableFields = nil) — conservative,
	// matching the pre-join behaviour for an unresolvable primary source.
	var tableFields map[string]bool
	allResolved := true
	if md != nil {
		collect := func(tableName string) {
			if tableName == "" {
				allResolved = false
				return
			}
			rt := md.GetRecordType(tableName)
			if rt == nil || rt.Descriptor == nil {
				allResolved = false
				return
			}
			if tableFields == nil {
				tableFields = make(map[string]bool)
			}
			fields := rt.Descriptor.Fields()
			for i := 0; i < fields.Len(); i++ {
				tableFields[strings.ToUpper(string(fields.Get(i).Name()))] = true
			}
		}
		collect(sq.tableName)
		for _, j := range sq.joins {
			collect(j.tableName)
		}
	}
	if !allResolved {
		tableFields = nil
	}

	// INVARIANT (load-bearing): this existence test compares only the BARE
	// name against the UNION of all source fields, so it is deliberately
	// qualifier-blind — `e.dname` (dname on the joined dept, not emp) would
	// pass here because DNAME is in the union. That coarse check is SAFE only
	// because EVERY call site is bracketed by a precise semantic resolver gate
	// that has the final say on a wrong-qualifier / genuinely-undefined key —
	// the union check never decides alone. The gate runs on DIFFERENT sides at
	// the two sites:
	//   - top-level GROUP BY: resolveColumnName(resolver, gb) (this file, ~L1002)
	//     runs BEFORE validateGroupByProjection (~L1019), so a wrong qualifier is
	//     rejected before it ever reaches this union check.
	//   - correlated scalar subquery: validateGroupByProjection runs first,
	//     then the shared upgradeAggregateOperands semantic GROUP BY walk
	//     rejects a wrong qualifier before translation.
	// Both orderings are pinned by TestFDB_GroupByWrongQualifierRejected. The real
	// hazard is a NEW call site with NO resolver gate on either side; converging
	// the existence check onto resolver.ResolveIdentifier removes the coupling
	// entirely (TODO.md, RFC-088 follow-up).
	// tableFields holds TOP-LEVEL field names, so the leaf is the only segment
	// it can answer for when the leading segments name a SOURCE (`e.dname` ->
	// DNAME). When they name a STRUCT COLUMN instead (`r.v.z`), the leaf is a
	// struct MEMBER — asking a top-level field set about it is a category
	// error, and it answered "no such column R.V.Z" for a path that resolves.
	// The same category error in the other direction is what let `GROUP BY
	// n.sk` through on a table that happened to declare an unrelated flat `sk`.
	// A dotted reference is therefore admitted when EITHER end names a real
	// field: the leaf for the source-qualified spelling, the ROOT for a struct
	// descent. This union check never decides alone (see the invariant above) —
	// the precise resolver gate that brackets every call site is what rejects a
	// path whose root is a real column but whose descent does not resolve.
	sourceNames := make(map[string]bool)
	for _, n := range []string{sq.tableName, sq.tableAlias} {
		if n != "" {
			sourceNames[strings.ToUpper(n)] = true
		}
	}
	for _, j := range sq.joins {
		for _, n := range []string{j.tableName, j.alias} {
			if n != "" {
				sourceNames[strings.ToUpper(n)] = true
			}
		}
	}
	existsAsField := func(upper string) bool {
		if tableFields == nil {
			return true
		}
		if tableFields[parseColRef(upper).bare()] {
			return true
		}
		rest := upper
		// A source alias is not a field, so peel one leading segment that names
		// a FROM source before asking about the root: `a.r.v.z` descends into
		// column R of source A, and only R is a name tableFields can answer.
		if head, tail, dotted := strings.Cut(rest, "."); dotted && sourceNames[head] {
			rest = tail
		}
		root, _, dotted := strings.Cut(rest, ".")
		return dotted && tableFields[root]
	}
	checkColumn := func(col string) error {
		upper := strings.ToUpper(col)
		bare := parseColRef(upper).bare()
		if !existsAsField(upper) {
			return api.NewErrorf(api.ErrCodeUndefinedColumn,
				"column %q does not exist", col)
		}
		if !groupBySet[bare] && !groupBySet[upper] {
			return api.NewErrorf(api.ErrCodeGroupingError,
				"column %q must appear in the GROUP BY clause or be used in an aggregate function", col)
		}
		return nil
	}

	groupByExprSet := make(map[string]bool)
	for _, gb := range sq.groupBy {
		if gb.expr != nil {
			groupByExprSet[strings.ToUpper(gb.display)] = true
		}
	}

	// HAVING obeys the SAME grouping rule as the SELECT list: a base column
	// referenced OUTSIDE an aggregate must be covered by GROUP BY, else it is a
	// 42803 grouping error. The OutsideSubqueries walk skips aggregate-call
	// operands (SUM(v)'s v) AND does not descend into nested subqueries — a
	// column syntactically inside a HAVING subquery (`… HAVING EXISTS(SELECT 1
	// FROM u WHERE u.v = k)`) binds to THAT query block, so it must not be
	// group-checked against the outer sources (else a subquery-local column
	// whose bare name collides with an ungrouped outer column would wrongly
	// 42803). "Covered" = the column is a GROUP BY key OR appears inside a
	// GROUP BY EXPRESSION key, so `GROUP BY k+1 HAVING k+1 > 5` stays valid.
	// Only genuine base columns of a KNOWN source are flagged (the tableFields
	// guard) — an aggregate OUTPUT ALIAS or a derived/CTE source (tableFields
	// == nil) is left to downstream resolution. Without this, `HAVING id > 2`
	// with id neither grouped nor aggregated silently read the group key at
	// id's colliding ordinal (wrong rows), or leaked an internal "ordinal
	// resolution ... malformed plan" when id's base ordinal exceeded the
	// aggregated row width. Mirrors the SELECT-list/aggCols validation above.
	if sq.havingExpr != nil {
		groupByColumns := make(map[string]bool)
		for _, gb := range sq.groupBy {
			if gb.expr != nil {
				for _, c := range harvestBareColumnRefsOutsideSubqueries(gb.expr) {
					groupByColumns[strings.ToUpper(c)] = true
				}
				continue
			}
			groupByColumns[parseColRef(strings.ToUpper(gb.display)).bare()] = true
			if gb.qualified {
				groupByColumns[strings.ToUpper(gb.bare)] = true
			}
		}
		for _, ref := range harvestBareColumnRefsOutsideSubqueries(sq.havingExpr) {
			bare := strings.ToUpper(ref)
			if groupByColumns[bare] {
				continue
			}
			if tableFields != nil && tableFields[bare] {
				return api.NewErrorf(api.ErrCodeGroupingError,
					"column %q must appear in the GROUP BY clause or be used in an aggregate function", ref)
			}
		}
	}

	// ORDER BY over a grouped row obeys the same coverage rule as HAVING.
	// Validate it before exact native-slot binding so an ungrouped source
	// reference remains the user-facing 42803 error, not an internal 0AF00
	// contract failure. Aggregate operands are skipped by this walk, while a
	// unique bare output alias wins before source-column interpretation.
	if len(sq.orderBy) > 0 {
		groupByColumns := make(map[string]bool)
		for _, gb := range sq.groupBy {
			if gb.expr != nil {
				for _, c := range harvestBareColumnRefsOutsideSubqueries(gb.expr) {
					groupByColumns[strings.ToUpper(c)] = true
				}
				continue
			}
			groupByColumns[parseColRef(strings.ToUpper(gb.display)).bare()] = true
			if gb.qualified {
				groupByColumns[strings.ToUpper(gb.bare)] = true
			}
		}
		for _, ob := range sq.orderBy {
			if ob.pos > 0 || ob.rawExpr == nil {
				continue
			}
			if bare, n := orderByOutputAliasBinding(ob.rawExpr, ob.colName, sq); bare && n == 1 {
				continue
			}
			for _, ref := range harvestBareColumnRefsOutsideSubqueries(ob.rawExpr) {
				bare := strings.ToUpper(ref)
				if groupByColumns[bare] {
					continue
				}
				if tableFields != nil && tableFields[bare] {
					diagnosticRef := ref
					// The structural harvester deliberately returns bare
					// segments so delimited identifiers containing dots are
					// never re-split. For a direct plain ORDER BY reference we
					// already carry its parse-derived qualifier separately;
					// restore that source spelling for the user-facing 42803
					// diagnostic without using it as binding identity.
					if ob.qualified && strings.EqualFold(ob.bare, ref) {
						diagnosticRef = ob.qualifier + "." + ob.bare
					}
					return api.NewErrorf(api.ErrCodeGroupingError,
						"column %q must appear in the GROUP BY clause or be used in an aggregate function", diagnosticRef)
				}
			}
		}
	}

	if len(sq.aggCols) > 0 {
		for _, ac := range sq.aggCols {
			if ac.aggFunc != "" || !ac.visible {
				continue
			}
			if ac.outExpr != nil {
				// Expression entry (e.g. `x.col1 + x.col2`). Walk the
				// expression tree for column references outside of
				// aggregate calls and verify each is in GROUP BY.
				// Expressions that are purely constant or only reference
				// aggregate results are fine.
				refs := harvestColumnRefs(ac.outExpr)
				for _, ref := range refs {
					if err := checkColumn(ref); err != nil {
						return err
					}
				}
				continue
			}
			col := ac.groupCol
			if col == "" {
				col = ac.outName
			}
			if groupByExprSet[strings.ToUpper(col)] {
				continue
			}
			if err := checkColumn(col); err != nil {
				return err
			}
		}
		return nil
	}

	for i, col := range sq.projCols {
		if i < len(sq.projExprs) && sq.projExprs[i] != nil {
			continue
		}
		if err := checkColumn(col.name); err != nil {
			return err
		}
	}
	return nil
}

func buildProjectionResolverWithCTEScopes(sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) *expr.Resolver {
	if sq.tableName == "" && len(cteScopes) == 0 && sq.enclosingScope == nil {
		return nil
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	scope := semantic.NewScope(sq.enclosingScope)
	// hidden carries this leg's USING-hidden column names, exactly as
	// buildSelectScope threads them. Without it this scope answers an
	// unqualified reference to a USING column AMBIGUOUS while the other scope
	// builder resolves it to the left copy — and the two builders serve the same
	// query: buildSelectScope validates it, this one resolves its ORDER BY keys.
	// `SELECT b2 FROM ja JOIN jb USING (c1) ORDER BY c1` therefore parsed,
	// validated and planned, then failed at the sort with "ORDER BY key C1 has
	// no resolved Value".
	// One entry per FROM position (0 = the primary source): does an outer join
	// pad this leg with NULLs? A padded leg's columns are nullable in this query
	// block's row, so every reference resolved through this scope carries the type
	// the join actually produces. Same derivation the derived-table body uses,
	// from the same helper, because a query block and that block read as a derived
	// table must agree on their row.
	padded := nullSupplyingFromLegs(sq.joins)
	addSource := func(tableName, alias, bindingID string, hidden []string, position int) bool {
		aliasID := semantic.FromNormalized(alias)
		if alias == "" {
			aliasID = semantic.FromNormalized(tableName)
		}
		// The binding correlation: the parser-minted duplicate-leg id when
		// present, else the alias.
		binding := bindingOrAlias(bindingID, aliasID)
		if src, ok := cteScopes[strings.ToUpper(tableName)]; ok {
			cteSrc := cteSourceAs(src, aliasID, binding)
			cteSrc.HiddenColumns = hiddenColumnSet(hidden)
			return scope.AddSource(nullSupplyingSource(cteSrc, paddedAt(padded, position))) == nil
		}
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err != nil {
			return false
		}
		return scope.AddSource(nullSupplyingSource(semantic.ScopeSource{
			Table:           tbl,
			Alias:           aliasID,
			CorrelationName: binding,
			HiddenColumns:   hiddenColumnSet(hidden),
		}, paddedAt(padded, position))) == nil
	}
	addDerived := func(alias string, derivedQuery antlrgen.IQueryContext, body logical.LogicalOperator, bindingID string, hidden []string, position int) bool {
		if src, err := boundDerivedSource(md, alias, bindingID, derivedQuery, body, sq.enclosingScope, schemaName, cteScopes); err == nil {
			src.HiddenColumns = hiddenColumnSet(hidden)
			return scope.AddSource(nullSupplyingSource(src, paddedAt(padded, position))) == nil
		}
		return false
	}
	// A lateral array unnest leg is not a real table — register its virtual
	// element/ordinal source via the SAME shared helper buildSelectScope uses, so
	// a projection / GROUP BY / HAVING / ORDER BY over an unnest column resolves
	// here directly (the callers' buildSelectScope fallback becomes belt-and-
	// suspenders, no longer load-bearing). RFC-142.
	addUnnestSource := unnestScopeSourceAdder(scope)
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	if sq.tableName != "" {
		if sq.inlineValues != nil {
			src, ok := parsedInlineValuesScopeSource(sq.inlineValues, sq.tableAlias, "", md)
			if !ok || scope.AddSource(nullSupplyingSource(src, paddedAt(padded, 0))) != nil {
				return nil
			}
		} else if sq.derivedQuery != nil {
			if !addDerived(sq.tableName, sq.derivedQuery, sq.catalogAwareInnerPlan, sq.bindingID, nil, 0) {
				return nil
			}
		} else if !addSource(sq.tableName, sq.tableAlias, "", nil, 0) {
			return nil
		}
	}
	for i, j := range sq.joins {
		if j.inlineValues != nil {
			src, ok := parsedInlineValuesScopeSource(j.inlineValues, j.alias, j.bindingID, md)
			if !ok {
				return nil
			}
			src.HiddenColumns = hiddenColumnSet(j.usingHiddenCols)
			if scope.AddSource(nullSupplyingSource(src, paddedAt(padded, i+1))) != nil {
				return nil
			}
			continue
		}
		if j.derivedQuery != nil {
			if !addDerived(j.alias, j.derivedQuery, j.catalogAwareInnerPlan, j.bindingID, j.usingHiddenCols, i+1) {
				return nil
			}
			continue
		}
		visible := visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i], resolvesToTable)
		if isLateralUnnestJoin(j, visible, resolvesToTable) {
			if !addUnnestSource(j) {
				return nil
			}
			continue
		}
		if !addSource(j.tableName, j.alias, j.bindingID, j.usingHiddenCols, i+1) {
			return nil
		}
	}
	return expr.New(analyzer, scope)
}

// buildLogicalPlanForDeleteWithCatalog is the catalog-aware variant
// of buildLogicalPlanForDelete. If the WHERE walks cleanly through
// the expr resolver, the emitted LogicalFilter carries a
// QueryPredicate tree; otherwise the plan is identical to the
// text-only builder.
func buildLogicalPlanForDeleteWithCatalog(
	del antlrgen.IDeleteStatementContext,
	md *recordlayer.RecordMetaData,
	schemaName string,
) (logical.LogicalOperator, error) {
	// DELETE … LIMIT is rejected — Java rejects it too (QueryVisitor.visitDeleteStatement:
	// Assert ctx.limitClause()==null, "limit is not supported"), so this is conformant
	// in REJECTING with the same message. The shared grammar accepts a limitClause on a
	// DELETE, but honoring it is unimplemented — and the builder otherwise IGNORES it,
	// which silently DELETES ALL rows matching the WHERE instead of the requested subset
	// (data loss: `DELETE … WHERE p LIMIT 1` deleted every matching row). Fail closed.
	//
	// SQLSTATE: Java's 2-arg Assert.thatUnchecked(bool,String) defaults to
	// INTERNAL_ERROR (XX000) — a leaky internal code. We deliberately use the cleaner
	// 0AF00 (UNSUPPORTED_QUERY) instead, consistent with this PR's other XX000→clean-code
	// fixes; only the code differs from Java, not the reject-with-this-message behavior.
	if del != nil && del.LimitClause() != nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "limit is not supported")
	}
	op := buildLogicalPlanForDelete(del)
	if op == nil || md == nil || del == nil {
		return op, nil
	}
	tableName := ""
	if tn := del.TableName(); tn != nil && tn.FullId() != nil {
		tableName = functions.FullIdToName(tn.FullId())
	}
	// Validate the schema qualifier (if any) BEFORE classifying WHERE-column errors: a bad
	// qualifier's 42F00 (Unknown database) must take precedence over a WHERE-column 42703
	// when the bare table happens to exist in the active schema. For a valid/absent
	// qualifier this strips to the bare name used below. (Missing-but-valid-qualifier target
	// tables are caught with 42F01 in planDML after resolveQualifiedTableNames.)
	if tableName != "" {
		resolved, qErr := functions.ResolveQualifiedTableName(tableName, schemaName)
		if qErr != nil {
			return nil, qErr
		}
		tableName = resolved
	}
	w := del.WhereExpr()
	if w == nil || tableName == "" {
		return op, nil
	}
	// A DML statement runs inside a single schema's store, so the record
	// type is the bare name; strip any schema qualifier before resolving
	// and aliasing the predicate, so refs bind to the resolved scan
	// (which resolveQualifiedTableNames also reduces to the bare name).
	bare := bareTableName(tableName)
	// Prefer the subquery-aware path so DELETE … WHERE EXISTS(…) plans
	// through Cascades; fall back to the plain predicate builder. A carried
	// SQLSTATE from a WHERE-EXISTS subquery plan failure (RFC-142: AT-on-a-table
	// → WRONG_OBJECT_TYPE) is surfaced rather than masked by the text fallback.
	if ok, carried := upgradeDMLWhereWithCatalog(op, md, bare, w, schemaName); ok {
		return op, nil
	} else if carried != nil {
		return nil, carried
	}
	pred, ok, werr := buildWherePredicateForTableE(md, bare, bare, w)
	if werr != nil {
		// e.g. 42804 from a bare non-boolean DELETE WHERE — surface it, don't
		// mask it as a generic DML translation error (RFC-146).
		return nil, werr
	}
	if !ok {
		return op, nil
	}
	if installErr := installFirstWherePredicate(op, pred); installErr != nil {
		return nil, installErr
	}
	return op, nil
}

// recordTypeCI resolves a record type by name CASE-INSENSITIVELY. SQL table names arrive
// upper-cased (functions.FullIdToName), but a proto-derived record type keeps its proto
// message-name case (e.g. "Order"), and the catalog/analyzer the WHERE/SELECT path uses
// resolves case-insensitively — so a raw, case-sensitive md.GetRecordType would wrongly
// miss a real table (e.g. SQL "ORDER" vs record type "Order"). The fast path tries the
// exact key first (the common CREATE TABLE case, where the type is already upper-cased).
func recordTypeCI(md *recordlayer.RecordMetaData, name string) *recordlayer.RecordType {
	if md == nil {
		return nil
	}
	if rt := md.GetRecordType(name); rt != nil {
		return rt
	}
	for n, rt := range md.RecordTypes() {
		if strings.EqualFold(n, name) {
			return rt
		}
	}
	return nil
}

// bareTableName strips a leading schema qualifier ("s1.T" → "T"). Used so
// DML predicate resolution and correlation aliases match the resolved
// (bare) scan name.
func bareTableName(name string) string {
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		return name[dot+1:]
	}
	return name
}

// upgradeDMLWhereWithCatalog upgrades the WHERE filter of a single-table
// DML plan (DELETE / UPDATE) to a real predicate with full EXISTS / scalar
// subquery support — the same machinery the SELECT path uses (an
// existsSubqueryPlanner installed on the resolver). This is what lets
// `DELETE … WHERE EXISTS(…)` plan through Cascades like SELECT; the plain
// buildWherePredicateForTable has no SubqueryPlanner and declines the
// EXISTS shape. Returns ok=false when the WHERE can't be resolved (caller
// falls back to the plain predicate builder).
//
// A WHERE-EXISTS subquery that fails to PLAN with a carried, specific SQLSTATE
// (e.g. RFC-142's AT-ordinality-on-a-table → WRONG_OBJECT_TYPE) must NOT be
// swallowed into a silent text fallback: the fallback can't plan the EXISTS
// either and the user sees a generic "DML Cascades translation failed" (0AF00)
// instead of the faithful diagnostic. Return that carried error so the DML
// builder surfaces it — the same precedence the SELECT path gives a
// translation error code. A non-specific resolver error (no api.Error in the
// chain) is the ordinary "WHERE not resolvable here" signal and still falls back.
func upgradeDMLWhereWithCatalog(
	op logical.LogicalOperator,
	md *recordlayer.RecordMetaData,
	tableName string,
	whereExpr antlrgen.IWhereExprContext,
	schemaName string,
) (ok bool, carried error) {
	if op == nil || md == nil || whereExpr == nil || whereExpr.Expression() == nil {
		return false, nil
	}
	if schemaName == "" {
		schemaName = defaultEmbeddedSchema
	}
	sq := &selectQuery{tableName: tableName, tableAlias: tableName, limit: -1}
	resolver := buildSelectScope(sq, md, schemaName, nil)
	if resolver == nil {
		return false, nil
	}
	// schemaName threads onto the EXISTS-subquery planner so a schema-qualified
	// comma source inside a `DELETE/UPDATE … WHERE EXISTS (SELECT 1 FROM PA AS
	// main, main.PB AS B)` classifies main.PB as a schema-qualified TABLE against
	// the ACTIVE schema, not the hardcoded default. RFC-142.
	existsPlanner := &existsSubqueryPlanner{
		md:         md,
		schemaName: schemaName,
		// nil CTE registry: this DML entry point builds its selectQuery from a bare
		// table name, so there is no enclosing WITH clause whose legs could appear
		// in the FROM it describes.
		outerScopes: buildOuterScopeSources(sq, md, schemaName, nil),
	}
	resolver.SetSubqueryPlanner(existsPlanner)
	walked, err := resolver.WalkPredicate(whereExpr.Expression())
	if err != nil || walked == nil {
		// Surface an AUTHORITATIVE semantic classification from this subquery-aware walk —
		// an undefined / ambiguous column or bad source. This walk (with a SubqueryPlanner),
		// unlike the plain text fallback, can see PAST an EXISTS atom to a LATER bad column,
		// so dropping its ColumnNotFoundError would leave `… WHERE EXISTS(…) AND nope = 1`
		// falling through to a generic 0AF00. The column genuinely doesn't exist for
		// the text fallback either, so surfacing it here never masks a fallback-resolvable
		// WHERE. mapPredicateWalkError maps these to the same 42703/42702 the SELECT path gives.
		var colNF *semantic.ColumnNotFoundError
		var ambig *semantic.AmbiguousColumnError
		var srcNF *semantic.SourceNotFoundError
		if errors.As(err, &colNF) || errors.As(err, &ambig) || errors.As(err, &srcNF) {
			return false, mapPredicateWalkError(err)
		}
		// A specific carried SQLSTATE from a subquery PLAN failure (an EXISTS inner
		// build, or a scalar build rejecting LIMIT > 1 / DISTINCT / a window)
		// takes precedence over the text fallback. Gate on the WHERE actually
		// containing either subquery atom: those are precisely the shapes the
		// plain text builder cannot plan, so the catalog path's error is
		// authoritative. For a plain comparison WHERE the text fallback may still
		// succeed, and swallowing a generic api error here preserves that.
		var apiErr *api.Error
		hasSubqueryAtom := expr.ContainsExistsAtom(whereExpr.Expression()) ||
			expr.ContainsSubqueryAtom(whereExpr.Expression())
		if errors.As(err, &apiErr) && hasSubqueryAtom {
			return false, apiErr
		}
		return false, nil
	}
	// The SELECT lowering hides its materialized scalar slot with an outer-only
	// projection. DML consumes record identity directly, so that projection is
	// not yet proven transparent to UPDATE/DELETE record+primary-key plumbing.
	// Decline before attaching the carrier; never let DML fall into an unbound
	// alias or a reshaped-record write.
	if len(existsPlanner.correlatedScalarSubqueries) > 0 {
		return false, api.NewError(api.ErrCodeUnsupportedQuery,
			"correlated scalar subquery in a DML WHERE predicate is not supported")
	}
	if installErr := installFirstWherePredicate(op, predicates.SimplifyPredicateValues(walked)); installErr != nil {
		return false, installErr
	}
	if len(existsPlanner.subqueries) > 0 {
		if !upgradeFirstFilterExistsSubqueries(op, existsPlanner.subqueries) {
			return false, api.NewError(api.ErrCodeUnsupportedQuery,
				"WHERE subqueries could not be installed on the logical plan")
		}
	}
	if len(existsPlanner.scalarSubqueries) > 0 {
		if !upgradeFirstFilterScalarSubqueries(op, existsPlanner.scalarSubqueries) {
			return false, api.NewError(api.ErrCodeUnsupportedQuery,
				"WHERE scalar subqueries could not be installed on the logical plan")
		}
	}
	return true, nil
}

// buildLogicalPlanForUpdateWithCatalog is the catalog-aware variant
// of buildLogicalPlanForUpdate. Same shape as the Delete variant —
// walker failure falls back to text form on LogicalFilter.
func buildLogicalPlanForUpdateWithCatalog(
	upd antlrgen.IUpdateStatementContext,
	md *recordlayer.RecordMetaData,
	schemaName string,
) (logical.LogicalOperator, error) {
	op := buildLogicalPlanForUpdate(upd)
	if op == nil || md == nil || upd == nil {
		return op, nil
	}
	updOp, ok := op.(*logical.LogicalUpdate)
	if !ok {
		return op, nil
	}
	tableName := ""
	if tn := upd.TableName(); tn != nil && tn.FullId() != nil {
		tableName = functions.FullIdToName(tn.FullId())
	}
	if tableName == "" {
		return op, nil
	}
	// Validate the schema qualifier (if any) BEFORE the SET-column / WHERE classification: a
	// bad qualifier's 42F00 must take precedence over a 42703/42F01 when the bare table
	// exists in the active schema. Valid/absent qualifier → strips to the bare name.
	resolved, qErr := functions.ResolveQualifiedTableName(tableName, schemaName)
	if qErr != nil {
		return nil, qErr
	}
	tableName = resolved
	bare := bareTableName(tableName)

	// Resolve the target type STRICTLY, the same way planDML's 42F01 guard does.
	// (Missing target tables are rejected with 42F01 in planDML after
	// resolveQualifiedTableNames; rt stays nil here for a missing/qualified target
	// and the SET check below is then skipped.)
	//
	// It folded case once, and the ORDER of the two checks made that visible as a
	// wrong error rather than a lax one: this runs during logical construction,
	// planDML's guard runs afterwards on the built op, so `UPDATE customer SET
	// nosuchcol = 'z'` against a table declared `"Customer"` reported
	// `42703 column "NOSUCHCOL" not found in table "CUSTOMER"` -- diagnosing a
	// column of a table that does not exist -- instead of 42F01. Strict here means
	// rt is nil for that target and the SET check declines, leaving the 42F01 to
	// be the answer.
	rt := md.GetRecordType(bare)

	// Validate each SET target column exists in the table, mirroring INSERT's
	// build-time check (insert_cascades.go). Without this, an UPDATE that assigns a
	// nonexistent column reaches the executor and surfaces a LEAKY raw error
	// ("executor: update field %q not found in descriptor", no SQLSTATE) instead of
	// a clean 42703 — the same condition INSERT and SELECT already report as 42703.
	// The check is case-insensitive (EqualFold over the descriptor field names) so it
	// is exactly as permissive as the executor's lookup (ByName(lower) →
	// fieldByNameFold) and never rejects a column the executor would accept. rt may be
	// nil for a schema-qualified target (validated downstream); skip the check then.
	if rt != nil && rt.Descriptor != nil {
		fields := rt.Descriptor.Fields()
		for _, set := range updOp.Sets {
			found := false
			for i := 0; i < fields.Len(); i++ {
				if strings.EqualFold(string(fields.Get(i).Name()), set.Column) {
					found = true
					break
				}
			}
			if !found {
				return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
					"column %q not found in table %q", set.Column, bare)
			}
		}
	}

	// Resolve each SET RHS expression to a real Value against the target
	// table (e.g. `price / 2` → Divide(FieldValue(PRICE), 2)) so the
	// executor evaluates it per row instead of choking on raw text. The
	// iteration mirrors buildLogicalPlanForUpdate's append order/skip.
	if resolver := buildSelectScope(&selectQuery{tableName: bare, tableAlias: bare, limit: -1}, md, schemaName, nil); resolver != nil {
		idx := 0
		for _, el := range upd.AllUpdatedElement() {
			if el == nil || el.FullColumnName() == nil || el.Expression() == nil {
				continue
			}
			if idx < len(updOp.Sets) {
				// A STRUCT target takes the same target-type push-down the
				// INSERT path uses, so `SET s = (100, 100)` acquires the
				// column's field names and types instead of dying as an
				// untyped multi-element record constructor. Java reaches the
				// same typed result from the other side — it builds an
				// ANONYMOUS record here (visitUpdatedElement,
				// ExpressionVisitor.java:1085-1090, with no target type in
				// state) and coerces it into the target descriptor when the
				// transform is applied (MessageHelpers.deepCopyMessageIfNeeded,
				// keyed by field number = position). Pushing the type down is
				// the same information applied earlier, and it is what lets a
				// mistyped literal fail at plan time rather than at write.
				var v values.Value
				var err error
				if fd := structSetTargetField(rt, updOp.Sets[idx].Column); fd != nil {
					v, err = parseUpdateSetValue(fd, el.Expression(), resolver)
					if err != nil {
						return nil, err
					}
				} else {
					v, err = resolver.WalkExpression(el.Expression())
				}
				if err == nil && v != nil {
					updOp.Sets[idx].Value = v
				}
			}
			idx++
		}
	}

	// Upgrade the WHERE filter with EXISTS/scalar subquery support; fall
	// back to the plain predicate builder. No WHERE → UPDATE all rows. A carried
	// SQLSTATE from a WHERE-EXISTS subquery plan failure (RFC-142: AT-on-a-table
	// → WRONG_OBJECT_TYPE) is surfaced rather than masked by the text fallback.
	if w := upd.WhereExpr(); w != nil {
		if ok, carried := upgradeDMLWhereWithCatalog(op, md, bare, w, schemaName); !ok {
			if carried != nil {
				return nil, carried
			}
			pred, ok, werr := buildWherePredicateForTableE(md, bare, bare, w)
			if werr != nil {
				// e.g. 42804 from a bare non-boolean UPDATE WHERE — surface it.
				return nil, werr
			}
			if ok {
				if installErr := installFirstWherePredicate(op, pred); installErr != nil {
					return nil, installErr
				}
			}
		}
	}
	return op, nil
}

// buildLogicalPlanForInsertWithCatalog is the catalog-aware variant
// of buildLogicalPlanForInsert. INSERT VALUES has no nested query so
// it short-circuits to the text builder; INSERT … SELECT routes the
// inner SELECT through the catalog-aware Select path so its WHERE
// becomes a predicate tree when md is non-nil.
func buildLogicalPlanForInsertWithCatalog(
	ins antlrgen.IInsertStatementContext,
	md *recordlayer.RecordMetaData,
	schemaName string,
) (logical.LogicalOperator, error) {
	if ins == nil {
		return nil, nil
	}
	if md == nil {
		return buildLogicalPlanForInsert(ins), nil
	}
	if schemaName == "" {
		schemaName = defaultEmbeddedSchema
	}
	op := buildLogicalPlanForInsert(ins)
	if op == nil {
		return op, nil
	}
	insertOp, ok := op.(*logical.LogicalInsert)
	if !ok || insertOp.Source == nil {
		// VALUES form (no Source) — nothing to upgrade.
		return op, nil
	}
	// Re-run the inner SELECT through the catalog-aware path. We
	// can't directly mutate the existing Source's filter without
	// re-walking the SELECT, so just rebuild Source.
	selCtx, ok := ins.InsertStatementValue().(*antlrgen.InsertStatementValueSelectContext)
	if !ok {
		return op, nil
	}
	body := selCtx.QueryExpressionBody()
	if body == nil {
		return op, nil
	}
	termDefault, ok := body.(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return op, nil
	}
	simpleTable, ok := termDefault.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		return op, nil
	}
	sq, err := extractFromSimpleTable(simpleTable)
	if err != nil {
		return op, nil
	}
	// Defensive: only swap Source when the catalog-aware build
	// produced a non-nil tree. Today buildLogicalPlanForSelectWithCatalog
	// can't return nil while buildLogicalPlanForSelect returned non-nil
	// (same ANTLR node, same extractFromSimpleTable contract), but
	// pinning the invariant in code instead of in the comment guards
	// against future divergence between the text and catalog paths.
	//
	// schemaName is the ACTIVE session schema, threaded so a schema-qualified
	// comma source in the INSERT … SELECT body (`INSERT INTO dst SELECT … FROM
	// PA AS main, main.PB AS B` in a session whose schema is `main`) classifies
	// main.PB as the schema-qualified TABLE against the active schema — the same
	// classification the top-level SELECT path performs. Hardcoding the default
	// would check it against `s`, leaving a LogicalUnnest the DML path's
	// resolveQualifiedTableNames cannot repair. RFC-142.
	//
	// A carried SQLSTATE from the SELECT-body build (RFC-142: an AT-on-a-table
	// comma source in the INSERT … SELECT FROM list → WRONG_OBJECT_TYPE) is
	// surfaced — not swallowed into the original (mis-classified unnest) source
	// the text path produced, which would later fail translation with a generic
	// "DML Cascades translation failed" instead of the faithful 42809.
	upgraded, selErr := buildLogicalPlanForSelectWithCatalog(sq, md, schemaName)
	if selErr != nil {
		return nil, selErr
	}
	if upgraded != nil {
		insertOp.Source = upgraded
	}
	alignInsertSelectColumns(insertOp, md)
	return insertOp, nil
}

// alignInsertSelectColumns sets the SELECT projection's output aliases to
// the INSERT target columns positionally. INSERT … SELECT is positional —
// the SELECT's i-th output feeds the target's i-th column regardless of
// the SELECT output's own name (e.g. `INSERT INTO t(id,total) SELECT id,
// price*qty`) — so the projected row datum ends up keyed by the target
// column names and executeInsert can build the target record by name.
func alignInsertSelectColumns(insertOp *logical.LogicalInsert, md *recordlayer.RecordMetaData) {
	proj := findProjection(insertOp.Source)
	if proj == nil || len(proj.Projections) == 0 {
		return
	}
	targetCols := insertOp.Columns
	if len(targetCols) == 0 {
		rt := md.GetRecordType(bareTableName(insertOp.Table))
		if rt == nil {
			return
		}
		fields := rt.Descriptor.Fields()
		targetCols = make([]string, fields.Len())
		for i := 0; i < fields.Len(); i++ {
			targetCols[i] = string(fields.Get(i).Name())
		}
	}
	if proj.Aliases == nil {
		proj.Aliases = make([]string, len(proj.Projections))
	}
	for i := 0; i < len(proj.Projections) && i < len(targetCols); i++ {
		proj.Aliases[i] = targetCols[i]
		// The target column list is the USER's, so the slot's provenance is
		// reset with its name. Overwriting the name while leaving a mint marker
		// standing would leave the marker describing an alias that no longer
		// exists — the desync a per-slot marker is most exposed to.
		if i < len(proj.AliasMinted) {
			proj.AliasMinted[i] = false
		}
		if i < len(proj.AliasSources) {
			proj.AliasSources[i] = values.ProjectionAliasSource{}
		}
	}
}

// protoKindToValueType maps a proto field kind to the cascades values.Type used
// for INSERT promotion checks. Nullability is irrelevant to IsPromotable, so the
// nullable singletons are returned. Returns nil for kinds outside the numeric
// promotion core; the caller skips those (the runtime converter handles them).
func protoKindToValueType(k protoreflect.Kind) values.Type {
	switch k {
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return values.NullableInt
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return values.NullableLong
	case protoreflect.FloatKind:
		return values.NullableFloat
	case protoreflect.DoubleKind:
		return values.NullableDouble
	}
	return nil
}

// checkInsertSelectPromotable rejects an INSERT … SELECT whose projected
// AGGREGATE-result column cannot be promoted to its target column type — the
// plan-time, lattice-driven analogue of Java's PromoteValue assignability check.
// AVG(BIGINT) types DOUBLE (AggregateValue.Type()); DOUBLE→BIGINT has no edge in
// the promotion lattice, so the INSERT is rejected with SQLSTATE 22000 exactly
// like Java — and, because the verdict is purely IsPromotable over the
// structurally-derived type, independent of whether the source produces any rows
// (the empty-source axis).
//
// Every SQL aggregate source now has one final LogicalProject in SELECT-list
// order. An aggregate-derived slot appears in one of two shapes:
//   - a COMPUTED expression that CONTAINS an aggregate (e.g. AVG(v)+1) —
//     flagged by LogicalProject.AggregateSlots (provenance, captured pre-rewrite)
//     and reliably typed via the value's Type() (the aggregate reference carries
//     its result type, B′; ArithmeticValue propagates it). Provenance, NOT
//     type-presence: plain columns are concrete-typed too (ResolveIdentifier).
//   - a DIRECT aggregate (e.g. SELECT AVG(v)) — its exact projected Value carries
//     the native aggregate result type. The canonical-name lookup below remains
//     a defensive fallback for hand-built/legacy logical trees.
//
// Plain-column narrowing (LONG→INT, DOUBLE-col→INT) is NOT checked here — it
// stays deferred to the runtime converter, pending the Java end-state
// (PromoteValue projection nodes) that dissolves this guard. INSERT … SELECT
// with an explicit column list is rejected upstream, so the projection maps
// positionally onto the target record's fields.
//
// Scope: covers every SQL-built aggregate and ordinary projected source. A
// direct hand-built LogicalAggregate has no public SELECT contract and is
// conservatively outside this SQL-layer check.
func checkInsertSelectPromotable(insertOp *logical.LogicalInsert, md *recordlayer.RecordMetaData) error {
	proj := findProjection(insertOp.Source)
	if proj == nil {
		return nil
	}
	rt := md.GetRecordType(bareTableName(insertOp.Table))
	if rt == nil {
		return nil
	}
	// Canonical aggregate output name → reliable result type for a legacy or
	// hand-built Project whose direct aggregate Value was not populated.
	aggTypes := map[string]values.Type{}
	if agg := findAggregate(insertOp.Source); agg != nil {
		for j, call := range agg.Calls {
			var operand values.Value
			if j < len(agg.AggregateOperands) {
				operand = agg.AggregateOperands[j]
			}
			if t := aggResultTypeFromFunc(call.Func, operand); t != nil {
				aggTypes[strings.ToUpper(call.CanonicalName())] = t
			}
		}
	}
	fields := rt.Descriptor.Fields()
	for i := 0; i < len(proj.Projections) && i < fields.Len(); i++ {
		var srcType values.Type
		if i < len(proj.AggregateSlots) && proj.AggregateSlots[i] &&
			i < len(proj.ProjectedValues) && proj.ProjectedValues[i] != nil {
			srcType = proj.ProjectedValues[i].Type()
		} else if t, ok := aggTypes[strings.ToUpper(proj.Projections[i])]; ok {
			srcType = t
		}
		if srcType == nil || srcType.Code() == values.TypeCodeUnknown {
			continue
		}
		targetType := protoKindToValueType(fields.Get(i).Kind())
		if targetType == nil {
			continue
		}
		if !values.IsPromotable(srcType, targetType) {
			return api.NewErrorf(api.ErrCodeCannotConvertType,
				"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.")
		}
	}
	return nil
}

// aggResultTypeFromName derives an aggregate's result type from its canonical
// output function and its resolved operand. It is the defensive fallback for a
// legacy/hand-built aggregate projection without a ProjectedValue. AVG→DOUBLE
// and COUNT→LONG are function-determined; SUM/MIN/MAX
// inherit the operand type. The function prefix is read off the *internal*
// canonical name (the contract the executor's aggResultName also relies on), not
// user SQL text. Mirrors AggregateValue.Type() / Java's per-operator resultTypeCode
// — keep the two in sync until the PromoteValue follow-up (RFC-083) dissolves this
// function.
// Divergences from the shared javaAggregateResultCode table, deliberate
// for METADATA (this function feeds ResultSet column types, not the
// plan-time gates): COUNT reports NOT NULL (the metadata contract), and
// an unknown-operand SUM/MIN/MAX falls back to NullableLong rather than
// Unknown so the column still carries a displayable type.
func aggResultTypeFromFunc(fn string, operand values.Value) values.Type {
	switch fn {
	case "AVG":
		return values.NullableDouble
	case "COUNT":
		return values.NotNullLong
	case "SUM", "MIN", "MAX":
		if operand != nil {
			if t := operand.Type(); t != nil && t.Code() != values.TypeCodeUnknown {
				return t
			}
		}
		return values.NullableLong
	}
	return nil
}

// buildLogicalPlanForQueryWithCTECatalog is like
// buildLogicalPlanForQueryWithCatalog but accepts external CTE scopes
// from an enclosing WITH clause. Used by scalar subquery planning where
// the inner query (e.g. `SELECT MIN(v) FROM high`) references a CTE
// defined in the outer query's WITH clause. The outer scopes are merged
// with any CTEs the inner query itself defines (inner shadows outer on
// name collision, matching SQL scoping rules).
func buildLogicalPlanForQueryWithCTECatalog(
	q antlrgen.IQueryContext,
	md *recordlayer.RecordMetaData,
	schemaName string,
	outerCTEScopes map[string]semantic.ScopeSource,
	outerCTEOnScopes map[string]semantic.ScopeSource,
) (logical.LogicalOperator, error) {
	if schemaName == "" {
		schemaName = defaultEmbeddedSchema
	}
	// Only short-circuit to the schema-less WithCatalog variant when the ACTIVE
	// schema IS the default — that variant hardcodes defaultEmbeddedSchema for the
	// schema-qualified-table demotion. For a NON-default session schema (e.g.
	// `main`), stay on this path so the threaded schemaName reaches
	// buildLogicalPlanForSelectWithCTECatalog's demotion/normalization (a
	// `main.PB`-in-a-subquery source resolves against the active schema, not `s`).
	// The own-CTE pre-scan below runs identically with an empty outer-scope map.
	// BOTH outer maps must be empty to short-circuit: a join/unnest-bodied
	// outer CTE lives ONLY in outerCTEOnScopes (never cteScopes), and dropping
	// it here sent a subquery's `... FROM c JOIN t ON ...` into the scope-less
	// variant where the ON silently dropped (cross-product rows — the
	// review-proven scalar-subquery path hole). RFC-142 (P2b).
	if len(outerCTEScopes) == 0 && len(outerCTEOnScopes) == 0 && schemaName == defaultEmbeddedSchema {
		return buildLogicalPlanForQueryWithCatalog(q, md)
	}
	if q == nil {
		return nil, nil
	}
	if md == nil {
		return buildLogicalPlanForQuery(q), nil
	}

	ctesCtx := q.Ctes()
	preState := map[string]cteScopePreState{}

	// Start with outer CTE scopes, then overlay any inner CTE defs
	// (inner shadows outer on name collision). cteOnScopes mirrors the
	// overlay for the ON-resolution-only sources (see buildCTEOnOnlySource).
	cteScopes := make(map[string]semantic.ScopeSource, len(outerCTEScopes))
	for k, v := range outerCTEScopes {
		cteScopes[k] = v
	}
	cteOnScopes := make(map[string]semantic.ScopeSource, len(outerCTEOnScopes))
	for k, v := range outerCTEOnScopes {
		cteOnScopes[k] = v
	}
	if ctesCtx != nil {
		// Track inner CTE names to detect sibling duplicates.
		innerCTEs := make(map[string]bool)
		for _, nq := range ctesCtx.AllNamedQuery() {
			name := functions.FullIdToName(nq.GetName())
			upper := strings.ToUpper(name)
			if innerCTEs[upper] {
				return nil, api.NewErrorf(api.ErrCodeDuplicateAlias,
					"found '%s' more than once", name)
			}
			innerCTEs[upper] = true
			// Snapshot the PRE-REGISTRATION state (outer binding or absent)
			// before any write — the body build swaps back to it
			// (buildCTEBodySelfHidden: self-invisible, outer visible).
			if _, seen := preState[upper]; !seen {
				sv, sh := cteScopes[upper]
				ov, oh := cteOnScopes[upper]
				preState[upper] = cteScopePreState{scopeVal: sv, scopeHad: sh, onVal: ov, onHad: oh}
			}
			// Inner CTE shadowing an outer CTE is fine (SQL scoping).
			if src, ok, cteBodyErr := buildCTEColumnSource(md, name, nq.Query(), cteScopes); cteBodyErr != nil {
				return nil, cteBodyErr
			} else if ok {
				if colAliases := nq.GetColumnAliases(); colAliases != nil {
					if aliasList, ok := colAliases.(*antlrgen.FullIdListContext); ok && aliasList != nil {
						aliases := aliasList.AllFullId()
						if nAliases := len(aliases); nAliases > 0 && src.Table != nil {
							nCols := len(src.Table.Columns())
							if nAliases != nCols {
								return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference,
									"cte query has %d column(s), however %d aliases defined",
									nCols, nAliases)
							}
						}
					}
					src = applyCTEColumnAliases(src, colAliases)
				}
				cteScopes[upper] = src
				delete(cteOnScopes, upper) // inner derivable shadows an outer ON-only entry
			} else {
				// Declared but not globally derivable (join/unnest body): the
				// ON-only registration keeps an enclosing explicit join's ON
				// resolvable and supplies a complete sole-source block locally;
				// marker entries remain unpromoted and loud.
				// The registration-time derivation runs BEFORE the shadow
				// delete below: a body leg naming the outer same-name
				// correctly classifies against the OUTER binding (which is
				// what the body's reference means, pre-state scoping).
				if regErr := registerCTEOnOnlyScope(cteOnScopes, upper, nq.Query(), nq.GetColumnAliases(), md, schemaName, cteScopes); regErr != nil {
					return nil, regErr
				}
				// The mirror of the derivable arm's shadow delete: an inner
				// ON-ONLY registration must EVICT a same-named OUTER
				// derivable entry, or this level's MAIN query resolves the
				// inner CTE's reads against the STALE OUTER schema
				// (review-caught: MAX over a stale column returned the wrong
				// generation; the pre-registration snapshot keeps the outer
				// visible for the BODY build only). Post-evict the inner is
				// ON-only (in cteOnScopes, not cteScopes). A complete source
				// is admitted only by singleSourceQueryBlockCTEScopes for a
				// sole-source query block; that local copy gives WHERE,
				// projection, and ORDER BY the exact CTE boundary row without
				// advertising the schema to sibling legs. NO-shadow still adds
				// nothing to cteScopes, so comma/join flatten-evasion shapes
				// keep their clean decline. Marker/underivable entries remain
				// unpromoted.
				delete(cteScopes, upper)
			}
		}
	}

	main, err := buildLogicalPlanForQueryBodyWithCTECatalog(q.QueryExpressionBody(), md, schemaName, cteScopes, cteOnScopes)
	if err != nil {
		return nil, err
	}
	if main == nil {
		return nil, nil
	}
	if ctesCtx == nil {
		return main, nil
	}
	recursive := ctesCtx.RECURSIVE() != nil
	// No clause = ANY (the planner picks); an explicit level_order pins
	// the level union (the clause's only remaining alternative).
	traversalOrder := logical.TraversalAnyOrder
	if toc := ctesCtx.TraversalOrderClause(); toc != nil {
		traversalOrder = logical.TraversalLevelOrder
		if toc.PRE_ORDER() != nil {
			traversalOrder = logical.TraversalPreOrder
		} else if toc.POST_ORDER() != nil {
			traversalOrder = logical.TraversalPostOrder
		}
	}
	ctes := ctesCtx.AllNamedQuery()
	for i := len(ctes) - 1; i >= 0; i-- {
		nq := ctes[i]
		name := functions.FullIdToName(nq.GetName())
		var body logical.LogicalOperator
		if inner := nq.Query(); inner != nil {
			if recursive {
				qeb := inner.QueryExpressionBody()
				if _, isSet := qeb.(*antlrgen.SetQueryContext); !isSet {
					return nil, api.NewError(api.ErrCodeUnsupportedOperation,
						"recursive CTE requires UNION ALL body")
				}
			}
			// Self-invisible body build (buildCTEBodySelfHidden): the
			// registration loop completed BEFORE this build, so the maps
			// carry the CTE's own entry — CTE-first resolution would
			// resolve the body against its own output schema.
			body, err = buildCTEBodySelfHidden(cteScopes, cteOnScopes, strings.ToUpper(name), preState, recursive, func() (logical.LogicalOperator, error) {
				return buildLogicalPlanForQueryBodyWithCTECatalog(inner.QueryExpressionBody(), md, schemaName, cteScopes, cteOnScopes)
			})
			if err != nil {
				return nil, err
			}
		}
		if body == nil {
			return nil, nil
		}
		cte := logical.NewCTE(name, body, main, recursive)
		cte.TraversalOrder = traversalOrder
		if colAliases := nq.GetColumnAliases(); colAliases != nil {
			if aliasList, ok := colAliases.(*antlrgen.FullIdListContext); ok && aliasList != nil {
				aliases := aliasList.AllFullId()
				names := make([]string, len(aliases))
				for j, fid := range aliases {
					// NormalizeIdentifier ALREADY applied SQL identifier
					// semantics — an unquoted alias came back folded UPPER and a
					// quoted one verbatim — so a second fold here can only
					// destroy `WITH c("x")`. This is the CAPTURE, which is why
					// it is fixed here rather than at the three sites that
					// APPLY the list: they can only publish what this stored.
					names[j] = functions.FullIdToName(fid)
				}
				cte.ColumnAliases = names
			}
		}
		main = cte
	}
	if err := bindExactCTEOutputMetadata(main, md); err != nil {
		return nil, err
	}
	return main, nil
}

// buildLogicalPlanForQueryWithCatalog is the catalog-aware variant
// of buildLogicalPlanForQuery. Recurses into CTE bodies and the
// query body so WHERE clauses anywhere in the tree pick up the
// metadata when available. md=nil collapses to the text builder.
func buildLogicalPlanForQueryWithCatalog(
	q antlrgen.IQueryContext,
	md *recordlayer.RecordMetaData,
) (logical.LogicalOperator, error) {
	if q == nil {
		return nil, nil
	}
	if md == nil {
		return buildLogicalPlanForQuery(q), nil
	}

	ctesCtx := q.Ctes()
	preState := map[string]cteScopePreState{}

	// Pre-scan CTE definitions to extract column schemas. Process in
	// declaration order so CTE B can reference CTE A's derived schema.
	// This is the TOP-LEVEL (no external scope) variant — reached only from the
	// EXPLAIN-only generators and the WithCTECatalog default-schema short-circuit,
	// so it uses the default schema for the schema-qualified-table demotion. A
	// non-default session schema flows through the WithCTECatalog path instead.
	schemaName := defaultEmbeddedSchema
	var cteScopes map[string]semantic.ScopeSource
	var cteOnScopes map[string]semantic.ScopeSource
	if ctesCtx != nil {
		cteScopes = make(map[string]semantic.ScopeSource)
		cteOnScopes = make(map[string]semantic.ScopeSource)
		for _, nq := range ctesCtx.AllNamedQuery() {
			name := functions.FullIdToName(nq.GetName())
			upper := strings.ToUpper(name)
			if _, exists := cteScopes[upper]; exists {
				return nil, api.NewErrorf(api.ErrCodeDuplicateAlias,
					"found '%s' more than once", name)
			}
			// An ON-only registration is a DECLARED name too — without this
			// arm a join-bodied duplicate (never in cteScopes) silently
			// last-wins here while the visitor and the WithCTECatalog loop
			// both error (the review-caught third-loop copy of the same hole;
			// reachable live via a subquery-nested WITH through the
			// empty-scope short-circuit).
			if _, exists := cteOnScopes[upper]; exists {
				return nil, api.NewErrorf(api.ErrCodeDuplicateAlias,
					"found '%s' more than once", name)
			}
			// Pre-registration snapshot (always ABSENT on this route — the
			// maps are fresh — kept uniform with the WithCTECatalog loop so
			// the shared wrap-loop block reads identically).
			if _, seen := preState[upper]; !seen {
				sv, sh := cteScopes[upper]
				ov, oh := cteOnScopes[upper]
				preState[upper] = cteScopePreState{scopeVal: sv, scopeHad: sh, onVal: ov, onHad: oh}
			}
			if src, ok, cteBodyErr := buildCTEColumnSource(md, name, nq.Query(), cteScopes); cteBodyErr != nil {
				return nil, cteBodyErr
			} else if ok {
				// Apply CTE column aliases: WITH c1(x, y) AS (...)
				// Java's SemanticAnalyzer.validateCteColumnAliases checks
				// that the alias count matches the CTE body column count.
				if colAliases := nq.GetColumnAliases(); colAliases != nil {
					if aliasList, ok := colAliases.(*antlrgen.FullIdListContext); ok && aliasList != nil {
						aliases := aliasList.AllFullId()
						if nAliases := len(aliases); nAliases > 0 && src.Table != nil {
							nCols := len(src.Table.Columns())
							if nAliases != nCols {
								return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference,
									"cte query has %d column(s), however %d aliases defined",
									nCols, nAliases)
							}
						}
					}
					src = applyCTEColumnAliases(src, colAliases)
				}
				cteScopes[upper] = src
			} else {
				// Declared but not globally derivable (join/unnest body): the
				// ON-only registration keeps an enclosing explicit join's ON
				// resolvable and supplies a complete sole-source block locally;
				// marker entries remain unpromoted and loud.
				// The registration-time derivation runs BEFORE the shadow
				// delete below: a body leg naming the outer same-name
				// correctly classifies against the OUTER binding (which is
				// what the body's reference means, pre-state scoping).
				if regErr := registerCTEOnOnlyScope(cteOnScopes, upper, nq.Query(), nq.GetColumnAliases(), md, schemaName, cteScopes); regErr != nil {
					return nil, regErr
				}
				// The mirror of the derivable arm's shadow delete: an inner
				// ON-ONLY registration must EVICT a same-named OUTER
				// derivable entry, or this level's MAIN query resolves the
				// inner CTE's reads against the STALE OUTER schema
				// (review-caught: MAX over a stale column returned the wrong
				// generation; the pre-registration snapshot keeps the outer
				// visible for the BODY build only). Post-evict the inner is
				// ON-only (in cteOnScopes, not cteScopes). A complete source
				// is admitted only by singleSourceQueryBlockCTEScopes for a
				// sole-source query block; that local copy gives WHERE,
				// projection, and ORDER BY the exact CTE boundary row without
				// advertising the schema to sibling legs. NO-shadow still adds
				// nothing to cteScopes, so comma/join flatten-evasion shapes
				// keep their clean decline. Marker/underivable entries remain
				// unpromoted.
				delete(cteScopes, upper)
			}
		}
	}

	main, err := buildLogicalPlanForQueryBodyWithCTECatalog(q.QueryExpressionBody(), md, schemaName, cteScopes, cteOnScopes)
	if err != nil {
		return nil, err
	}
	if main == nil {
		return nil, nil
	}
	if ctesCtx == nil {
		return main, nil
	}
	recursive := ctesCtx.RECURSIVE() != nil
	// No clause = ANY (the planner picks); an explicit level_order pins
	// the level union (the clause's only remaining alternative).
	traversalOrder := logical.TraversalAnyOrder
	if toc := ctesCtx.TraversalOrderClause(); toc != nil {
		traversalOrder = logical.TraversalLevelOrder
		if toc.PRE_ORDER() != nil {
			traversalOrder = logical.TraversalPreOrder
		} else if toc.POST_ORDER() != nil {
			traversalOrder = logical.TraversalPostOrder
		}
	}
	ctes := ctesCtx.AllNamedQuery()
	for i := len(ctes) - 1; i >= 0; i-- {
		nq := ctes[i]
		name := functions.FullIdToName(nq.GetName())
		var body logical.LogicalOperator
		if inner := nq.Query(); inner != nil {
			if recursive {
				qeb := inner.QueryExpressionBody()
				if _, isSet := qeb.(*antlrgen.SetQueryContext); !isSet {
					return nil, api.NewError(api.ErrCodeUnsupportedOperation,
						"recursive CTE requires UNION ALL body")
				}
			}
			// Self-invisible body build (buildCTEBodySelfHidden): the
			// registration loop completed BEFORE this build, so the maps
			// carry the CTE's own entry — CTE-first resolution would
			// resolve the body against its own output schema.
			body, err = buildCTEBodySelfHidden(cteScopes, cteOnScopes, strings.ToUpper(name), preState, recursive, func() (logical.LogicalOperator, error) {
				return buildLogicalPlanForQueryBodyWithCTECatalog(inner.QueryExpressionBody(), md, schemaName, cteScopes, cteOnScopes)
			})
			if err != nil {
				return nil, err
			}
		}
		if body == nil {
			return nil, nil
		}
		cte := logical.NewCTE(name, body, main, recursive)
		cte.TraversalOrder = traversalOrder
		if colAliases := nq.GetColumnAliases(); colAliases != nil {
			if aliasList, ok := colAliases.(*antlrgen.FullIdListContext); ok && aliasList != nil {
				aliases := aliasList.AllFullId()
				names := make([]string, len(aliases))
				for j, fid := range aliases {
					// NormalizeIdentifier ALREADY applied SQL identifier
					// semantics — an unquoted alias came back folded UPPER and a
					// quoted one verbatim — so a second fold here can only
					// destroy `WITH c("x")`. This is the CAPTURE, which is why
					// it is fixed here rather than at the three sites that
					// APPLY the list: they can only publish what this stored.
					names[j] = functions.FullIdToName(fid)
				}
				cte.ColumnAliases = names
			}
		}
		main = cte
	}
	if err := bindExactCTEOutputMetadata(main, md); err != nil {
		return nil, err
	}
	return main, nil
}

// buildLogicalPlanForQueryBodyWithCatalog dispatches simple SELECT
// vs UNION, threading md through both arms. Mirrors the text
// builder's QueryTermDefault / SetQuery split.
func buildLogicalPlanForQueryBodyWithCatalog(
	body antlrgen.IQueryExpressionBodyContext,
	md *recordlayer.RecordMetaData,
) (logical.LogicalOperator, error) {
	if body == nil {
		return nil, nil
	}
	switch b := body.(type) {
	case *antlrgen.QueryTermDefaultContext:
		// A parenthesized query operand — `(SELECT … LIMIT n)` as a UNION
		// branch — surfaces as a ParenthesisQueryContext. Recurse into the
		// inner query body so the branch's own clauses (notably LIMIT) are
		// built and not silently dropped (RFC-128 §4.7). Without this a
		// parenthesized branch fell through to nil here.
		if paren, ok := b.QueryTerm().(*antlrgen.ParenthesisQueryContext); ok {
			if inner := paren.Query(); inner != nil {
				return buildLogicalPlanForQueryBodyWithCatalog(inner.QueryExpressionBody(), md)
			}
			return nil, nil
		}
		simpleTable, ok := b.QueryTerm().(*antlrgen.SimpleTableContext)
		if !ok {
			return nil, nil
		}
		sq, err := extractFromSimpleTable(simpleTable)
		if err != nil {
			return nil, err
		}
		if fn := findUnsupportedFunctionInSelectQuery(sq); fn != "" {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"Unsupported operator "+fn)
		}
		return buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	case *antlrgen.SetQueryContext:
		return buildLogicalPlanForUnionWithCatalog(b, md)
	}
	return nil, nil
}

// buildLogicalPlanForQueryBodyWithCTECatalog is like
// buildLogicalPlanForQueryBodyWithCatalog but passes CTE-derived
// column schemas to the predicate builder so WHERE clauses on CTE
// references can produce real QueryPredicates.
func buildLogicalPlanForQueryBodyWithCTECatalog(
	body antlrgen.IQueryExpressionBodyContext,
	md *recordlayer.RecordMetaData,
	schemaName string,
	cteScopes map[string]semantic.ScopeSource,
	cteOnScopes map[string]semantic.ScopeSource,
) (logical.LogicalOperator, error) {
	if body == nil {
		return nil, nil
	}
	if schemaName == "" {
		schemaName = defaultEmbeddedSchema
	}
	// As in buildLogicalPlanForQueryWithCTECatalog: only short-circuit to the
	// schema-less variant when the active schema IS the default; a non-default
	// session schema must keep threading so the demotion uses the active schema.
	// BOTH maps must be empty — a join/unnest-bodied outer CTE lives ONLY in
	// cteOnScopes, and dropping it here silently dropped the enclosing join's
	// ON on the subquery build path (cross-product rows).
	if len(cteScopes) == 0 && len(cteOnScopes) == 0 && schemaName == defaultEmbeddedSchema {
		return buildLogicalPlanForQueryBodyWithCatalog(body, md)
	}
	switch b := body.(type) {
	case *antlrgen.QueryTermDefaultContext:
		// Parenthesized UNION branch — recurse into the inner query body so
		// the branch's LIMIT/clauses survive (RFC-128 §4.7); see the
		// non-CTE variant above.
		if paren, ok := b.QueryTerm().(*antlrgen.ParenthesisQueryContext); ok {
			if inner := paren.Query(); inner != nil {
				return buildLogicalPlanForQueryBodyWithCTECatalog(inner.QueryExpressionBody(), md, schemaName, cteScopes, cteOnScopes)
			}
			return nil, nil
		}
		simpleTable, ok := b.QueryTerm().(*antlrgen.SimpleTableContext)
		if !ok {
			return nil, nil
		}
		sq, err := extractFromSimpleTable(simpleTable)
		if err != nil {
			return nil, err
		}
		if fn := findUnsupportedFunctionInSelectQuery(sq); fn != "" {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"Unsupported operator "+fn)
		}
		return buildLogicalPlanForSelectWithCTECatalog(sq, md, schemaName, cteScopes, cteOnScopes)
	case *antlrgen.SetQueryContext:
		return buildLogicalPlanForUnionWithCTECatalog(b, md, schemaName, cteScopes, cteOnScopes, false)
	}
	return nil, nil
}

func buildLogicalPlanForUnionWithCTECatalog(
	setQ *antlrgen.SetQueryContext,
	md *recordlayer.RecordMetaData,
	schemaName string,
	cteScopes map[string]semantic.ScopeSource,
	cteOnScopes map[string]semantic.ScopeSource,
	allowDistinct bool,
) (logical.LogicalOperator, error) {
	if setQ == nil {
		return nil, nil
	}
	if schemaName == "" {
		schemaName = defaultEmbeddedSchema
	}
	distinct := false
	if setQ.ALL() == nil {
		if !allowDistinct {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery, "only UNION ALL is supported")
		}
		distinct = true
	}
	left, err := buildLogicalPlanForQueryBodyWithCTECatalog(setQ.GetLeft(), md, schemaName, cteScopes, cteOnScopes)
	if err != nil {
		return nil, err
	}

	// The grammar attaches a trailing ORDER BY / LIMIT / OFFSET to
	// the rightmost simpleTable. For a UNION, those clauses apply to
	// the combined result (SQL standard), NOT to the right branch
	// alone. Strip them from the right branch before building (so
	// column validation doesn't reject LEFT-branch column names
	// against the right table) and lift them to wrap the whole UNION.
	var lifted unionLiftedClauses
	var right logical.LogicalOperator
	right, lifted, err = buildUnionRightBranchStrippingOrderBy(setQ.GetRight(), md, schemaName, cteScopes, cteOnScopes)
	if err != nil {
		return nil, err
	}
	if left == nil || right == nil {
		return nil, nil
	}

	// Legacy fallback: if the right branch's sort wasn't stripped at
	// the selectQuery level (e.g. nested UNION), peel it off the
	// logical plan tree.
	if len(lifted.sortKeys) == 0 {
		if s, ok := right.(*logical.LogicalSort); ok {
			lifted.sortKeys = s.Keys
			right = s.Input
		} else if p, ok := right.(*logical.LogicalProject); ok {
			if s, ok := p.Input.(*logical.LogicalSort); ok {
				lifted.sortKeys = s.Keys
				p.Input = s.Input
			}
		}
	}

	inputs := []logical.LogicalOperator{left, right}
	if innerUnion, ok := left.(*logical.LogicalUnion); ok && !innerUnion.Distinct {
		inputs = append(append([]logical.LogicalOperator(nil), innerUnion.Inputs...), right)
	}
	if err := validateUnionColumnCounts(inputs); err != nil {
		return nil, err
	}
	if err := validateUnionColumnTypes(inputs, md); err != nil {
		return nil, err
	}
	if len(lifted.sortKeys) > 0 {
		liftedSort := &logical.LogicalSort{Keys: lifted.sortKeys}
		if err := validateUnionOrderByColumns(liftedSort, inputs[0]); err != nil {
			return nil, err
		}
	}
	var result logical.LogicalOperator = logical.NewUnion(inputs, distinct)
	if len(lifted.sortKeys) > 0 {
		result = logical.NewSort(result, lifted.sortKeys)
	}
	if lifted.limit >= 0 || lifted.offset > 0 {
		result = logical.NewLimit(result, lifted.limit, lifted.offset)
	}
	return result, nil
}

// unionLiftedClauses holds ORDER BY / LIMIT / OFFSET stripped from a
// UNION's right branch so the caller can re-attach them to the
// combined result.
type unionLiftedClauses struct {
	sortKeys []logical.SortKey
	limit    int64 // <0 means no limit
	offset   int64
}

// buildUnionRightBranchStrippingOrderBy builds the right branch of a
// UNION, stripping any trailing ORDER BY and LIMIT/OFFSET from the
// simpleTable before building the logical plan. Returns the built
// plan and the stripped clauses (empty if none). For non-simpleTable
// right branches (e.g. nested UNION), falls through to the normal
// builder and returns empty clauses.
func buildUnionRightBranchStrippingOrderBy(
	body antlrgen.IQueryExpressionBodyContext,
	md *recordlayer.RecordMetaData,
	schemaName string,
	cteScopes map[string]semantic.ScopeSource,
	cteOnScopes map[string]semantic.ScopeSource,
) (logical.LogicalOperator, unionLiftedClauses, error) {
	if schemaName == "" {
		schemaName = defaultEmbeddedSchema
	}
	qtd, ok := body.(*antlrgen.QueryTermDefaultContext)
	if !ok {
		op, err := buildLogicalPlanForQueryBodyWithCTECatalog(body, md, schemaName, cteScopes, cteOnScopes)
		return op, unionLiftedClauses{limit: -1}, err
	}
	simpleTable, ok := qtd.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		op, err := buildLogicalPlanForQueryBodyWithCTECatalog(body, md, schemaName, cteScopes, cteOnScopes)
		return op, unionLiftedClauses{limit: -1}, err
	}
	sq, err := extractFromSimpleTable(simpleTable)
	if err != nil {
		return nil, unionLiftedClauses{limit: -1}, err
	}

	var lifted unionLiftedClauses
	lifted.limit = -1

	// Save and strip ORDER BY.
	if len(sq.orderBy) > 0 {
		for _, ob := range sq.orderBy {
			e := ob.colName
			if e == "" && ob.rawExpr != nil {
				e = canonicalTextOf(ob.rawExpr)
			}
			dir := logical.SortAsc
			if !ob.ascending {
				dir = logical.SortDesc
			}
			nullsFirst := ob.ascending
			if ob.nullsFirst != nil {
				nullsFirst = *ob.nullsFirst
			}
			// Carry the SELECT-list position for a POSITIONAL key: the ordinal
			// binds to the union OUTPUT slot (a Go extension — live-probed Java
			// 4.12.11.0 has NO positional ORDER BY at all, and attaches a
			// trailing ORDER BY to the RIGHT LEG ONLY, not the combined union;
			// Go deliberately implements the SQL-standard combined-result
			// semantics, see union_columns.yaml). Without Pos the key's TEXT
			// resolves against the RIGHT leg's spelling and then fails the
			// LEFT-leg name validation when the legs spell the position
			// differently (`SELECT '2024', … UNION ALL SELECT '2025', …
			// ORDER BY 1`). RFC-180.
			lifted.sortKeys = append(lifted.sortKeys, logical.SortKey{
				Expr:       e,
				Pos:        ob.pos,
				Dir:        dir,
				NullsFirst: nullsFirst,
				BareRef:    ob.bareRef,
				Bare:       ob.bare,
				Qualifier:  ob.qualifier,
				Qualified:  ob.qualified,
				Segs:       append([]string(nil), ob.segs...),
			})
		}
		sq.orderBy = nil
	}

	// Save and strip LIMIT/OFFSET. This is the rightmost simpleTable of an
	// UNPARENTHESIZED union (e.g. `… UNION ALL SELECT … ORDER BY id LIMIT n`),
	// whose trailing ORDER BY/LIMIT applies to the COMBINED result, NOT the
	// right branch alone (SQL standard). extractFromSimpleTable now populates
	// sq.limit/sq.offset, so we lift those and RESET them on the branch to
	// avoid double-applying the clause to the right branch (RFC-128). A
	// parenthesized right branch never reaches here — it is a
	// ParenthesisQueryContext, handled by the !ok path above, which keeps the
	// branch's own LIMIT inside.
	if sq.limit >= 0 || sq.offset > 0 {
		lifted.limit = sq.limit
		lifted.offset = sq.offset
		sq.limit = -1
		sq.offset = 0
	}

	if fn := findUnsupportedFunctionInSelectQuery(sq); fn != "" {
		return nil, lifted, api.NewError(api.ErrCodeUnsupportedQuery, "Unsupported operator "+fn)
	}
	op, err := buildLogicalPlanForSelectWithCTECatalog(sq, md, schemaName, cteScopes, cteOnScopes)
	if err != nil {
		return nil, lifted, err
	}
	return op, lifted, nil
}

// upgradeSortKeyValues walks the logical plan's LogicalSort and resolves
// sort key expressions through the expression walker. When an ORDER BY
// key is an aggregate expression (SUM(v)*2, COALESCE(SUM(v),0)), the
// walker produces a Value tree with AggregateValues rewritten to
// FieldValues referencing the aggregate output.
func upgradeSortKeyValues(op logical.LogicalOperator, sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) error {
	sort := findSort(op)
	if sort == nil || len(sort.Keys) == 0 {
		return nil
	}
	// OWNERSHIP, not proximity. findSort descends the WHOLE subtree, so a
	// select with no ORDER BY of its own still reaches the sort a DERIVED
	// TABLE built for its own ORDER BY — and rebinds its keys against THIS
	// select's scope, where the derived table is registered under its outer
	// alias. On
	//
	//   SELECT a.id FROM (SELECT id FROM t ORDER BY g DESC, id ASC LIMIT 4) a
	//
	// that turned the inner key `id` from T.ID#0 into A.ID#0 — a read of the
	// derived table's OUTPUT row by a sort that runs BELOW the projection
	// producing it, so nothing binds A at runtime and the query fails with
	// `exact QOV "A" ... has no declared runtime binding`. The asymmetry is
	// what makes it easy to miss: `g` is not an output column of a, so it
	// stayed correctly on T, and only the key that COLLIDES with an output
	// name was captured.
	//
	// Every binding decision below reads sq.orderBy (directly, or through
	// findOrderByForKey), so an empty one means this select contributed no
	// ORDER BY and any sort in the tree belongs to a nested query that has
	// already bound its own keys. The union lift clears sq.orderBy after
	// moving the keys onto the enclosing lifted sort, which this pass does
	// not own either.
	if len(sq.orderBy) == 0 {
		return nil
	}

	// Build alias→column mapping from projections.
	aliasToCol := make(map[string]string)
	aliasToIdx := make(map[string]int)
	if sq.projAliases != nil && sq.projCols != nil {
		for i, a := range sq.projAliases {
			if a != "" && i < len(sq.projCols) {
				aliasToCol[strings.ToUpper(a)] = sq.projCols[i].name
				aliasToIdx[strings.ToUpper(a)] = i
			}
		}
	}
	for _, ac := range sq.aggCols {
		if ac.outName != "" && ac.groupCol != "" {
			aliasToCol[strings.ToUpper(ac.outName)] = ac.groupCol
		}
	}

	// Resolve ORDER BY alias → underlying column or Value.
	// SQL standard (and Java): ORDER BY resolves to SELECT-list output
	// column names first, then table columns. Aliases take precedence.
	proj := findProjection(op)
	agg := findAggregate(op)
	// groupKeyOrdinalByDisplay carries the group key's OUTPUT ORDINAL — its index
	// in agg.GroupKeys, which IS its slot in the aggregate's [keys..., calls...]
	// output row. It used to carry the rendered output NAME, which is what the
	// consumer needs to spell the key, but the name is not what identifies the
	// key: aggregateGroupKeyOutputName renders a FieldValue as its bare leaf, so
	// two keys of `GROUP BY o.k, i.k` both render "K" and a map holding that name
	// cannot say which key it came from. The ordinal can, and the name is derived
	// back from it at the one place that needs to spell it.
	var groupKeyOrdinalByDisplay map[string]int
	if agg != nil && len(agg.GroupKeys) > 0 {
		groupKeyOrdinalByDisplay = make(map[string]int)
		for gkOrdinal, gk := range agg.GroupKeys {
			gkv := gk.Value
			if gkv == nil {
				continue
			}
			// The sort sits ABOVE the aggregate, so the group-key sort key must read
			// the AGGREGATE OUTPUT column name — what the executor (aggKeyName /
			// aggregateCursor.finalizeGroup) keys the group-key column by: a FieldValue
			// group key flows under its bare Field NAME (`V`), NOT its qualified explain
			// (`V.V`). A qualified group key value arises from a lateral-unnest SHADOWING
			// group key (`FROM t, t.arr AS V, u GROUP BY V`, resolved to FieldValue(QOV(V),
			// V) by upgradeAggregateOperands) — using the raw explain `V.V` here would key
			// the sort by a column the aggregate output does not carry → a no-op sort
			// (ORDER BY DESC silently ignored, P2b). Mirror aggKeyName:
			// the field name for a FieldValue, the explain for a computed key (whose
			// output column IS its explain). RFC-142. That naming rule still
			// applies; it is applied at the READ below, to the key this
			// ordinal names.
			groupKeyOrdinalByDisplay[strings.ToUpper(gk.Display)] = gkOrdinal
		}
	}
	// colToIdx maps a NON-aliased select item's canonical text to its select-list
	// position — the correspondence `ORDER BY <n>` (positional, whose key Expr is
	// the item's rendered name) and a text-form computed key (`ORDER BY col1 +
	// 10`) resolve through. Copying the EXACT projected Value (pointer) lets the
	// translator's pull-up bake the key to its OUTPUT ordinal: the key
	// must carry a plan-time ordinal, since a runtime name read silently
	// no-op-sorts when the rendered text and the output column spelling
	// diverge, e.g. a computed column named `(COL1 + 10)` vs the source text
	// `col1 + 10`. First-match on duplicate renderings — the duplicates are the
	// same expression, so the sort order is identical either way.
	colToIdx := make(map[string]int, len(sq.projCols))
	for i, c := range sq.projCols {
		key := strings.ToUpper(c.name)
		if _, dup := colToIdx[key]; !dup {
			colToIdx[key] = i
		}
	}
	// GROUPED-select correspondence (Java LogicalOperator.generateSelect): for
	// an aggregate query the SELECT list lives in aggCols, not projCols, so the
	// maps above are empty — but the reshaping POST-AGGREGATE projection carries
	// the items' rendered texts, aliases, and resolved output Values. Map them
	// to their output slots so a computed ORDER BY key (`ORDER BY a + b` over
	// `SELECT a + b, MAX(c) … GROUP BY a, b`) copies the EXACT projected Value
	// pointer and the translator's pull-up (pullUpToOutputField) bakes the key
	// to the projection OUTPUT ordinal. Without this the key resolves against
	// the FROM scope (base-row ordinals) and the enforcer sort ABOVE the
	// projection reads a foreign slot — silent mis-sort when the ordinal lands
	// in range, an ordinal-model malformed-plan error when it doesn't.
	// First-match semantics mirror colToIdx; existing entries win.
	if proj != nil {
		for i, ptext := range proj.Projections {
			key := strings.ToUpper(ptext)
			if _, dup := colToIdx[key]; !dup {
				colToIdx[key] = i
			}
		}
		for i, alias := range proj.Aliases {
			if alias == "" {
				continue
			}
			key := strings.ToUpper(alias)
			if _, dup := aliasToIdx[key]; !dup {
				aliasToIdx[key] = i
			}
		}
	}
	// POSITIONAL keys first, by ORDINAL — never by text. A positional key
	// is an ordinal into THIS select's output list; when the select's own
	// projection sits ABOVE the sort (the plain-select shape), the ordinal
	// resolves to that projection's item: the resolved item Value when the
	// catalog pass populated it (typed — immune to items whose rendered
	// texts or aliases collide), the item's underlying text otherwise. Pos
	// is CLEARED here so the translator can never bake the ordinal into
	// whatever projection roots the sort's INPUT (a derived source's
	// layout). When the projection is NOT an ancestor of the sort (the
	// aggregate reshaping strip below the sort, or a union), Pos survives
	// untouched — those inputs ARE select-list carriers and the
	// translator's Pos bake against them is the correct binding.
	positionalKey := make([]bool, len(sort.Keys))
	positionalBound := make([]bool, len(sort.Keys))
	for i := range sort.Keys {
		positionalKey[i] = sort.Keys[i].Pos > 0
	}
	if proj != nil && sortOwnedBySelect(proj, sort) {
		for i := range sort.Keys {
			pos := sort.Keys[i].Pos
			if pos < 1 || pos > len(proj.Projections) {
				continue
			}
			if proj.ProjectedValues != nil && pos-1 < len(proj.ProjectedValues) && proj.ProjectedValues[pos-1] != nil {
				sort.Keys[i].Value = proj.ProjectedValues[pos-1]
				positionalBound[i] = true
				if len(proj.AggregateOutputOrdinals) == len(proj.Projections) {
					sort.Keys[i].AggregateOutputValueExact = true
				}
			} else {
				sort.Keys[i].Expr = proj.Projections[pos-1]
			}
			sort.Keys[i].Pos = 0
		}
	}

	for i := range sort.Keys {
		// A successfully resolved ORDER BY ordinal is the SQL output-slot
		// authority. Its Expr is retained only for diagnostics; feeding that text
		// through the alias/column maps below can select another slot when a
		// computed SELECT item is omitted from sq.projCols (for example SELECT
		// score+0 AS id, id AS y ... ORDER BY 2). Never let a later text match
		// overwrite the exact projected Value copied by ordinal above.
		if positionalBound[i] {
			continue
		}
		upper := strings.ToUpper(sort.Keys[i].Expr)
		// Output aliases bind BARE one-segment identifiers only
		// (SortKey.BareRef): a qualified key's Expr is already
		// qualifier-stripped and an aggregate key's Expr is its canonical
		// rendering, so without the flag `ORDER BY d.x` / `ORDER BY
		// SUM(s.score)` would bind a same-spelled SELECT alias and
		// silently mis-sort.
		if real, ok := aliasToCol[upper]; ok && sort.Keys[i].BareRef {
			sort.Keys[i].Expr = real
		}
		if idx, ok := aliasToIdx[upper]; ok && proj != nil && sort.Keys[i].BareRef {
			if idx < len(proj.ProjectedValues) && proj.ProjectedValues[idx] != nil {
				sort.Keys[i].Value = proj.ProjectedValues[idx]
				if len(proj.AggregateOutputOrdinals) == len(proj.Projections) {
					sort.Keys[i].AggregateOutputValueExact = true
				}
			}
		} else if idx, ok := colToIdx[upper]; ok && proj != nil {
			if idx < len(proj.ProjectedValues) && proj.ProjectedValues[idx] != nil {
				// ORDER BY resolves in the SELECT output namespace before the
				// input namespace. Keep the projection's exact Value INSTANCE as
				// that authority even when an earlier catalog pass already filled
				// an independently constructed, type-correct input Value. Aggregate
				// index DDL translates this pointer through OutputSlots; retaining
				// the input instance makes every ordered aggregate appear absent
				// from its own projection list.
				sort.Keys[i].Value = proj.ProjectedValues[idx]
				if len(proj.AggregateOutputOrdinals) == len(proj.Projections) {
					sort.Keys[i].AggregateOutputValueExact = true
				}
			}
		}
		if groupKeyOrdinalByDisplay != nil && !sort.Keys[i].AggregateOutputValueExact {
			if gkOrdinal, ok := groupKeyOrdinalByDisplay[strings.ToUpper(sort.Keys[i].Expr)]; ok {
				// The name is spelled FROM the ordinal — the same authority
				// (aggregateGroupKeyOutputName over that slot's own key Value)
				// the map used to transport, now read off the key the ordinal
				// names instead of off whichever key wrote the map entry last.
				explain := aggregateNativeOutputName(agg, gkOrdinal)
				// The sort reads the row ABOVE the outermost operator. Directly
				// over the aggregate that is the AGGREGATE output name; with a
				// visible PROJECTION in between (mixed aliased/uneliased select
				// list) the sort key must read the PROJECTION's OUTPUT column
				// for the same underlying group key — the aggregate-output bare
				// name is not a column of the projected row (a lazy key naming
				// it is loud at runtime under the ordinal model; the retired
				// name read no-op-sorted silently).
				//
				// DEFERRED-strip inversion: when the reshaping projection sits
				// ABOVE the sort (a group key read only by ORDER BY defers the
				// strip), the sort reads the AGGREGATE row — redirecting the
				// key to the projection ALIAS would read a column that exists
				// only above (loud failure), or silently bind a same-named
				// hidden key. Redirect only when the sort is above the
				// projection.
				if proj != nil && !operatorContains(proj, sort) {
					for pi, ptext := range proj.Projections {
						if !strings.EqualFold(ptext, sort.Keys[i].Expr) {
							continue
						}
						if pi < len(proj.Aliases) && proj.Aliases[pi] != "" {
							explain = strings.ToUpper(proj.Aliases[pi])
						} else {
							explain = strings.ToUpper(ptext)
						}
						break
					}
				}
				sort.Keys[i].Expr = explain
				sort.Keys[i].AggregateOutputOrdinal = gkOrdinal
				sort.Keys[i].HasAggregateOutputOrdinal = true
				sort.Keys[i].Value = nil
			}
		}
	}

	resolver := buildProjectionResolverWithCTEScopes(sq, md, schemaName, cteScopes)
	if resolver == nil {
		// A lateral array unnest (`FROM t, t.arr AS v`) makes
		// buildProjectionResolverWithCTEScopes return nil: it tries to resolve the
		// dotted unnest source (`t.arr`) as a TABLE and fails, never registering the
		// unnest's AS/AT virtual columns. buildSelectScope is the single scope
		// builder that knows the unnest virtual source (unnestScopeSourceAdder), so
		// a COMPUTED ORDER BY over an unnest column (`ORDER BY v + 0 DESC`) can only
		// resolve there. Fall back to it; without this the sort key stays raw text
		// and the executor compares a non-existent field → a silent no-op sort.
		// RFC-142 (P2a).
		resolver = buildSelectScope(sq, md, schemaName, cteScopes)
		if resolver == nil {
			return nil
		}
	}
	exactAggregateBoundary := agg != nil && proj != nil &&
		len(proj.AggregateOutputOrdinals) == len(proj.Projections)
	for i := range sort.Keys {
		// Positional keys were already bound by ordinal above (or retain Pos
		// for the translator). Never walk their raw parse node: it is the
		// numeric literal itself and would overwrite ORDER BY 2 with constant
		// 2. Likewise, a prior alias/project/group-key mapping is already the
		// authoritative SQL output binding.
		if positionalKey[i] || sort.Keys[i].HasAggregateOutputOrdinal ||
			sort.Keys[i].AggregateOutputValueExact ||
			(!exactAggregateBoundary && sort.Keys[i].Value != nil) {
			continue
		}
		ob := findOrderByForKey(sq, sort.Keys[i].Expr)
		// rawExpr is authoritative even when the parser could also render a
		// colName. Aggregate calls such as MAX(x.v) are name-classified, but
		// their qualified operand still has to resolve structurally and bind to
		// the producer-native aggregate slot. Skipping name-classified items
		// leaves a qualified spelling (MAX(X.V)) that cannot match the private
		// aggregate label (MAX(V)), causing either a malformed plan or a
		// name-based misbind. Plain column references are safe here too: in an
		// aggregate query the structural binder maps a group key to its native
		// key slot; outside one the normal resolver Value is retained.
		var (
			v   values.Value
			err error
		)
		switch {
		case ob != nil && ob.rawExpr != nil:
			v, err = resolver.WalkExpression(ob.rawExpr)
		case sort.Keys[i].Bare != "":
			// A lifted/rebuilt key can legitimately outlive the raw ORDER BY
			// parse node. Its structured segments are still resolution-grade:
			// resolve those directly through the same semantic scope. Expr is a
			// diagnostic rendering only and is never parsed or split here.
			v, err = resolver.ResolveIdentifierPath(colRefIdentifiers(
				sort.Keys[i].Bare,
				sort.Keys[i].Qualifier,
				sort.Keys[i].Qualified,
				sort.Keys[i].Segs,
			))
		default:
			continue
		}
		if err != nil {
			if exactAggregateBoundary {
				if mapped := mapPredicateWalkError(err); mapped != nil {
					return mapped
				}
				return api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"ORDER BY expression could not be resolved against the exact aggregate output: %v", err)
			}
			continue
		}
		if exactAggregateBoundary {
			if err = validatePostAggregateValueDraft(v, agg); err != nil {
				return err
			}
			sort.Keys[i].AggregateOutputValueExact = true
		} else if containsAggregate(v) {
			return api.NewError(api.ErrCodeUnsupportedQuery,
				"ORDER BY aggregate expression has no aggregate output owner")
		}
		// A bare unnest sort key (`ORDER BY v`) resolves through the unnest's
		// Shadowing scope source to a qualified FieldValue over the unnest
		// correlation, which the P2a path already qualifies via
		// qualifyShadowedSortKeys. A COMPUTED key (`v + 0`) wraps that FieldValue in
		// an arithmetic Value; the qualification is intrinsic to the resolved tree
		// (the FieldValue carries its Child correlation), so the executor's ValueExpr
		// evaluates the qualified reference per row and the sort sorts for real.
		sort.Keys[i].Value = v
	}
	return nil
}

// bindExactCTEOutputMetadata is the post-CTE counterpart of the semantic
// projection and sort-key resolvers. An ON-only/otherwise statically
// underivable CTE is
// deliberately absent from the semantic scope: promoting its lossy schema
// would let ordinary reads bind to the wrong slot. Once the CTE body has been
// built, however, query.ExactLogicalResultType is an exact output contract.
// Use that contract only for plain structured projection and ORDER BY
// references over a single CTE input, and mint each value from a checked QOV +
// checked field path.
//
// Computed keys, ambiguous duplicate output labels, joins, and bodies whose
// exact result type cannot be derived remain unset. translateSort then rejects
// them loudly; no spelling is parsed or used as runtime identity here.
func bindExactCTEOutputMetadata(op logical.LogicalOperator, md *recordlayer.RecordMetaData) error {
	return bindExactCTEOutputMetadataInScope(op, md, make(map[string]*values.RecordType))
}

func bindExactCTEOutputMetadataInScope(
	op logical.LogicalOperator,
	md *recordlayer.RecordMetaData,
	cteTypes map[string]*values.RecordType,
) error {
	if op == nil {
		return nil
	}
	switch typed := op.(type) {
	case *logical.LogicalCTE:
		// The defining body sees the prior lexical binding, never itself.
		if err := bindExactCTEOutputMetadataInScope(typed.Body, md, cteTypes); err != nil {
			return err
		}
		name := strings.ToUpper(typed.Name)
		previous, hadPrevious := cteTypes[name]
		if result, ok := exactCTEDefinitionRecordType(typed, md); ok {
			cteTypes[name] = result
		} else {
			delete(cteTypes, name)
		}
		err := bindExactCTEOutputMetadataInScope(typed.Main, md, cteTypes)
		if hadPrevious {
			cteTypes[name] = previous
		} else {
			delete(cteTypes, name)
		}
		return err
	case *logical.LogicalProject:
		if err := bindExactCTEProjection(typed, cteTypes); err != nil {
			return err
		}
	case *logical.LogicalSort:
		if err := bindExactCTESortKeysOnSort(typed, cteTypes); err != nil {
			return err
		}
	}
	for _, child := range op.Children() {
		if err := bindExactCTEOutputMetadataInScope(child, md, cteTypes); err != nil {
			return err
		}
	}
	return nil
}

func exactCTEDefinitionRecordType(
	cte *logical.LogicalCTE,
	md *recordlayer.RecordMetaData,
) (*values.RecordType, bool) {
	if cte == nil || cte.Recursive {
		return nil, false
	}
	typ, err := query.ExactLogicalResultType(cte.Body, md)
	if err != nil {
		return nil, false
	}
	record, ok := typ.(*values.RecordType)
	if !ok || record == nil {
		return nil, false
	}
	fields := append([]values.Field(nil), record.Fields...)
	switch {
	case len(cte.ColumnAliases) > 0:
		if len(cte.ColumnAliases) != len(fields) {
			return nil, false
		}
		for i, alias := range cte.ColumnAliases {
			// VERBATIM — the THIRD site applying this same alias list, beside
			// cteBoundRowType and cascades_translator's derivedOutputColumns.
			// A CTE column alias arrives already normalized by the parse
			// capture, so the fold could only destroy `WITH c("x")`, and it
			// did: the other two published `x` while this one published `X`,
			// which the executor reported as
			// `edge lookup C: read as RECORD(x), declared RECORD(X)` on
			// `WITH c("x") AS (…) SELECT * FROM c WHERE c."x" > 5`.
			//
			// cteBoundRowType's own comment says the three agree by
			// construction. That sentence is only true while all three spell
			// the alias the same way.
			//
			// This site being verbatim was necessary and not sufficient: the
			// alias was arriving here ALREADY folded, from a DOUBLE STRIP at
			// the parse capture (`NormalizeIdentifier(FullIdToName(fid))`,
			// where FullIdToName already strips, so the outer call saw an
			// unquoted `x` and upper-cased it). Four sites APPLY this list and
			// one CAPTURES it; every application was correct and every one was
			// handed X.
			fields[i].Name = alias
			fields[i].Ordinal = i
		}
	default:
		// Without a column list the CTE's columns are named by its BODY's SQL
		// output labels. That is not the same as the exact row's field names: a
		// join row qualifies every leg column with its source alias so the
		// executor's row map can keep A.K and B.K apart, and those datum keys
		// are what this map published. Every consumer of it — the projection
		// binder just below, the sort-key binder, the qualified-join lookup —
		// then matched a user's `D.AID` against a field called `A.AID`, found
		// nothing, and left the slot with no Value; the query died later as
		// `projection slot 0 has no resolved Value`, naming neither the column
		// nor the CTE.
		//
		// A body whose labels cannot be derived keeps the exact row's names
		// rather than declining: that is the pre-existing behaviour for every
		// shape whose labels and field names already agree, which is all of
		// them except a multi-leg star.
		if labels, labelErr := query.ExactLogicalOutputLabels(cte.Body, md, nil); labelErr == nil &&
			len(labels) == len(fields) {
			for i := range fields {
				fields[i].Name = labels[i]
				fields[i].Ordinal = i
			}
		}
	}
	return &values.RecordType{
		RecordName: record.RecordName,
		Nullable:   record.Nullable,
		Fields:     fields,
		Legs:       append([]values.RecordTypeLeg(nil), record.Legs...),
	}, true
}

func bindExactCTESortKeysOnSort(
	sort *logical.LogicalSort,
	cteTypes map[string]*values.RecordType,
) error {
	if sort == nil {
		return nil
	}
	scan := singleCTEInput(sort.Input)
	if scan == nil {
		return nil
	}
	record := cteTypes[strings.ToUpper(scan.Table)]
	if record == nil {
		return nil
	}
	correlation := cteScanCorrelation(scan)
	if correlation == "" {
		return nil
	}

	for i := range sort.Keys {
		key := &sort.Keys[i]
		if key.Value != nil || key.Pos > 0 || key.HasAggregateOutputOrdinal ||
			key.AggregateOutputValueExact || key.Bare == "" || len(key.Segs) == 0 {
			continue
		}
		segments := append([]string(nil), key.Segs...)
		if len(segments) > 1 && cteSortQualifierMatchesScan(segments[0], scan) {
			segments = segments[1:]
		}
		if len(segments) == 0 {
			continue
		}
		rootOrdinal := -1
		for ordinal, field := range record.Fields {
			if strings.EqualFold(field.Name, segments[0]) {
				if rootOrdinal >= 0 {
					// Duplicate output labels carry no unique identity.
					rootOrdinal = -1
					break
				}
				rootOrdinal = ordinal
			}
		}
		if rootOrdinal < 0 {
			continue
		}

		owner, err := values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier(strings.ToUpper(correlation)), record)
		if err != nil {
			return err
		}
		requests := make([]values.FieldRequest, 0, len(segments))
		root, err := values.FieldByNameAndOrdinal(record.Fields[rootOrdinal].Name, rootOrdinal)
		if err != nil {
			return err
		}
		requests = append(requests, root)
		for _, segment := range segments[1:] {
			request, requestErr := values.FieldByName(segment)
			if requestErr != nil {
				return requestErr
			}
			requests = append(requests, request)
		}
		resolved, err := values.ResolveFieldAccess(owner, requests)
		if err != nil {
			return err
		}
		key.Value = resolved
	}
	return nil
}

// bindExactCTEProjection resolves only parsed, plain column projection slots
// over one CTE scan. It also admits a qualified reference to one uniquely
// addressed direct CTE scan in an all-inner direct-scan join tree. The CTE's
// built result record is the ordinal and type authority; rendered projection
// text is never inspected. Computed slots, duplicate output labels, and
// qualifiers that do not name the scan remain nil so translation rejects them
// loudly.
func bindExactCTEProjection(
	project *logical.LogicalProject,
	cteTypes map[string]*values.RecordType,
) error {
	if project == nil {
		return nil
	}
	singleScan := singleCTEInput(project.Input)

	projected := make([]values.Value, len(project.Projections))
	copy(projected, project.ProjectedValues)
	changed := false
	for i := range project.Projections {
		if projected[i] != nil || (i < len(project.IsComputed) && project.IsComputed[i]) ||
			i >= len(project.ProjectionRefs) {
			continue
		}
		ref := project.ProjectionRefs[i]
		if !ref.Present || ref.Bare == "" {
			continue
		}
		scan := singleScan
		var record *values.RecordType
		if scan == nil {
			if !ref.Qualified {
				continue
			}
			scan, record = uniqueQualifiedDirectCTEJoinInput(
				project.Input, ref.Qualifier, cteTypes)
			if scan == nil {
				continue
			}
		} else {
			record = cteTypes[strings.ToUpper(scan.Table)]
		}
		if record == nil || (ref.Qualified && !cteSortQualifierMatchesScan(ref.Qualifier, scan)) {
			continue
		}
		correlation := cteScanCorrelation(scan)
		if correlation == "" {
			continue
		}

		ordinal := -1
		for candidate, field := range record.Fields {
			if !strings.EqualFold(field.Name, ref.Bare) {
				continue
			}
			if ordinal >= 0 {
				// A duplicate output label has no unique column identity.
				ordinal = -1
				break
			}
			ordinal = candidate
		}
		if ordinal < 0 {
			continue
		}

		owner, err := values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier(strings.ToUpper(correlation)), record)
		if err != nil {
			return err
		}
		request, err := values.FieldByNameAndOrdinal(record.Fields[ordinal].Name, ordinal)
		if err != nil {
			return err
		}
		resolved, err := values.ResolveFieldAccess(owner, []values.FieldRequest{request})
		if err != nil {
			return err
		}
		projected[i] = resolved
		changed = true
	}
	if changed {
		project.ProjectedValues = projected
	}
	return nil
}

// uniqueQualifiedDirectCTEJoinInput recognizes only the topology in which a
// qualified source-relative Value remains exact without flattening the CTE's
// schema into the enclosing query scope: an all-inner join tree whose leaves
// are direct scans, with exactly one scan addressed by the qualifier, and that
// scan naming a CTE whose fully built result type is available. A wrapper,
// derived source, outer join, or colliding qualifier keeps the projection
// unresolved and therefore loud.
func uniqueQualifiedDirectCTEJoinInput(
	op logical.LogicalOperator,
	qualifier string,
	cteTypes map[string]*values.RecordType,
) (*logical.LogicalScan, *values.RecordType) {
	if qualifier == "" || len(cteTypes) == 0 {
		return nil, nil
	}
	if _, ok := op.(*logical.LogicalJoin); !ok {
		return nil, nil
	}
	valid := true
	matches := 0
	var matchedScan *logical.LogicalScan
	var matchedRecord *values.RecordType
	var walk func(logical.LogicalOperator)
	walk = func(candidate logical.LogicalOperator) {
		if !valid || candidate == nil {
			valid = false
			return
		}
		switch typed := candidate.(type) {
		case *logical.LogicalJoin:
			if typed.Kind != logical.JoinInner {
				valid = false
				return
			}
			walk(typed.Left)
			walk(typed.Right)
		case *logical.LogicalScan:
			if !directCTEJoinQualifierMatchesScan(qualifier, typed) {
				return
			}
			matches++
			if record := cteTypes[strings.ToUpper(typed.Table)]; record != nil {
				matchedScan = typed
				matchedRecord = record
			}
		default:
			valid = false
		}
	}
	walk(op)
	if !valid || matches != 1 || matchedScan == nil || matchedRecord == nil {
		return nil, nil
	}
	return matchedScan, matchedRecord
}

// directCTEJoinQualifierMatchesScan applies SQL's authored qualifier rule at
// the narrow joined-CTE recovery boundary. An explicit alias hides the table
// name; Binding is an internal correlation identity and is never a SQL-visible
// qualifier. The broader sort helper below also recognizes those internal
// identities because it consumes already-normalized metadata, so it is not the
// authority for this parsed ProjectionRef.
func directCTEJoinQualifierMatchesScan(qualifier string, scan *logical.LogicalScan) bool {
	if qualifier == "" || scan == nil {
		return false
	}
	if scan.Alias != "" {
		return strings.EqualFold(qualifier, scan.Alias)
	}
	return strings.EqualFold(qualifier, scan.Table)
}

// singleCTEInput returns only the one-source, row-shape-preserving input class
// for which a CTE definition's output ordinals are still the consumer's input
// ordinals. A projection, join, aggregate, or union changes that contract and
// must stay loud until it carries its own exact metadata.
func singleCTEInput(op logical.LogicalOperator) *logical.LogicalScan {
	for op != nil {
		switch typed := op.(type) {
		case *logical.LogicalScan:
			return typed
		case *logical.LogicalFilter:
			op = typed.Input
		case *logical.LogicalSort:
			op = typed.Input
		case *logical.LogicalLimit:
			op = typed.Input
		case *logical.LogicalDistinct:
			op = typed.Input
		default:
			return nil
		}
	}
	return nil
}

func cteScanCorrelation(scan *logical.LogicalScan) string {
	if scan == nil {
		return ""
	}
	if scan.Binding != "" {
		return scan.Binding
	}
	if scan.Alias != "" {
		return scan.Alias
	}
	return scan.Table
}

func cteSortQualifierMatchesScan(qualifier string, scan *logical.LogicalScan) bool {
	if qualifier == "" || scan == nil {
		return false
	}
	return (scan.Binding != "" && strings.EqualFold(qualifier, scan.Binding)) ||
		(scan.Alias != "" && strings.EqualFold(qualifier, scan.Alias)) ||
		strings.EqualFold(qualifier, scan.Table)
}

// aggregateGroupKeyOutputName returns the OUTPUT column name a group-key Value is
// keyed by in the aggregate's result row — the exact mirror of the executor's
// aggKeyName (executor.go): a FieldValue group key flows under its bare Field
// name (`V`), every other (computed) group key under its ExplainValue. The
// name is carried VERBATIM, which is what makes "mirror" true: aggKeyName
// delegates to expressions.AggregateKeyColumnName, and that authority stopped
// folding under RFC-237. Two of the three arms here kept folding while the
// nested arm did not, so this function disagreed with its declared authority on
// two shapes out of three — under a doc sentence asserting they agree, which is
// the failure class rather than a typo.
// Load-bearing for a lateral-unnest SHADOWING group key, whose resolved Value is
// a QUALIFIED FieldValue(QOV(V), V): its bare field name `V` (not the explain
// `V.V`) is the aggregate output column. RFC-142.
//
// A NESTED key takes its resolved PATH for the same reason
// expressions.AggregateKeyColumnName does, and through the same predicate: this
// function is that authority's mirror, and a mirror that disagrees on one shape
// is how a reference comes to read a slot the executor keyed differently.
func aggregateGroupKeyOutputName(gkv values.Value) string {
	if path, nested := values.NestedResolvedPath(gkv); nested {
		return path
	}
	if fv, ok := values.AsFieldValue(gkv); ok {
		return fv.DisplayName()
	}
	return values.ColumnNameValue(gkv)
}

func findSort(op logical.LogicalOperator) *logical.LogicalSort {
	if op == nil {
		return nil
	}
	if s, ok := op.(*logical.LogicalSort); ok {
		return s
	}
	for _, ch := range op.Children() {
		if s := findSort(ch); s != nil {
			return s
		}
	}
	return nil
}

func findOrderByForKey(sq *selectQuery, keyExpr string) *orderByClause {
	if sq == nil {
		return nil
	}
	for i := range sq.orderBy {
		ob := &sq.orderBy[i]
		name := ob.colName
		if name == "" && ob.rawExpr != nil {
			name = canonicalTextOf(ob.rawExpr)
		}
		if strings.EqualFold(name, keyExpr) {
			return ob
		}
	}
	// A SECOND (and later) ORDER BY AGGREGATE EXPRESSION carries a SYNTHESISED
	// colName. The aggregate harvester gives each such clause its own
	// non-visible aggCols entry, and from the second one on it names that entry
	// `__ob_agg_N__` so the entries cannot collide on the empty name — then
	// writes that synthetic name back over the CLAUSE's colName. The clause
	// therefore no longer answers to its own rendering, while the sort key built
	// from the same clause still spells the expression (`MIN(v) + 0`), so the
	// pass above cannot pair them and the key reached the translator with no
	// resolved Value at all.
	//
	// The clause's rawExpr is untouched by that rewrite, so it is the identity
	// that survives. Second pass rather than first: a clause whose colName was
	// deliberately REBASED (a positional key rebased to the projection's
	// underlying text, an output alias rebased to the stripped projection) must
	// still be found under the rebased name, and that name is not its raw text.
	for i := range sq.orderBy {
		ob := &sq.orderBy[i]
		if ob.colName == "" || ob.rawExpr == nil {
			continue
		}
		if strings.EqualFold(canonicalTextOf(ob.rawExpr), keyExpr) {
			return ob
		}
	}
	return nil
}

// buildOuterPlanOnDerived builds the Aggregate/Sort/Limit/Project/Distinct
// shell from a selectQuery on top of an already-built inner plan (derived
// table). Delegates to buildSelectShell with the derived table qualifier
// as the strip prefix.
func buildOuterPlanOnDerived(sq *selectQuery, innerOp logical.LogicalOperator) logical.LogicalOperator {
	// Keep the derived alias on the logical tree — the same LogicalCTE
	// wrapper the visitor path uses (the tree's one alias carrier for
	// derived tables). A bare innerOp loses the alias: sourceAlias() walks
	// to the BASE table and a correlated EXISTS on the derived alias binds
	// the outer row under the wrong name (`SELECT e.*` routes here via the
	// qualified-star rebuild — the visitor-path fix's rebuild-path twin).
	var op logical.LogicalOperator = derivedSourceCarrier(sq.tableName, sq.bindingID, innerOp)
	if sq.whereExpr != nil {
		op = logical.NewFilter(op, canonicalTextOf(sq.whereExpr))
	}
	return buildSelectShell(op, sq, strings.ToUpper(sq.tableName)+".")
}

func hasAnyQualifiedStar(sq *selectQuery) bool {
	if sq == nil || sq.projStarQualifiers == nil {
		return false
	}
	for _, q := range sq.projStarQualifiers {
		if q != "" {
			return true
		}
	}
	return false
}

// expandQualifiedStars replaces qualified-star projection slots (a.*)
// with explicit column names from the matching source table. Modifies
// sq.projCols, sq.projAliases, sq.projExprs, sq.projStarQualifiers in place.
//
// A `<qualifier>.*` over a lateral array unnest alias (`SELECT V.* FROM t,
// t.arr AS V`) is expanded to the unnest's element column(s) (and the ordinal
// under WITH ORDINALITY) via the SHARED unnest virtual source — NOT only real
// record types. Without this the star qualifier resolves to nothing and the
// query degrades to an UNQUALIFIED star → returns the ENTIRE FlatMap row (outer
// columns included) instead of just the unnest source's columns (silent-wrong).
// RFC-142.
func expandQualifiedStars(sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) error {
	if sq == nil || !hasAnyQualifiedStar(sq) {
		return nil
	}
	resolver, err := buildSelectScopeChecked(sq, md, schemaName, cteScopes)
	if err != nil {
		return err
	}
	var newCols []projCol
	var newAliases, newQuals []string
	var newExprs []antlrgen.IExpressionContext
	for i, col := range sq.projCols {
		qual := ""
		if i < len(sq.projStarQualifiers) {
			qual = sq.projStarQualifiers[i]
		}
		if qual == "" {
			newCols = append(newCols, col)
			alias := ""
			if i < len(sq.projAliases) {
				alias = sq.projAliases[i]
			}
			newAliases = append(newAliases, alias)
			var expression antlrgen.IExpressionContext
			if i < len(sq.projExprs) {
				expression = sq.projExprs[i]
			}
			newExprs = append(newExprs, expression)
			newQuals = append(newQuals, "")
			continue
		}
		columns, err := starColumnsFromScopeChecked(resolver, qual)
		if err != nil {
			return err
		}
		for _, column := range columns {
			newCols = append(newCols, column)
			newAliases = append(newAliases, column.bare)
			newExprs = append(newExprs, nil)
			newQuals = append(newQuals, "")
		}
	}
	sq.projCols, sq.projAliases, sq.projExprs, sq.projStarQualifiers = newCols, newAliases, newExprs, newQuals
	return nil
}

// expandBareStarOverUsingJoins expands a bare `SELECT *` into explicit
// projCols when a JOIN … USING is present, so the RIGHT side's copy of
// each USING column drops out of the star — Java's star expansion filters
// hidden expressions (SemanticAnalyzer.expandStar → nonEphemeralVisible;
// the right copy was hidden by resolveJoinUsingClause). Without this the
// nil-projCols path projects every leg column and the row is wider than
// Java's (`SELECT * FROM ja JOIN jb USING (c1)` must be C1, A2, B2).
//
// Returns true when it expanded (caller rebuilds the plan). A DERIVED
// leg enumerates from its own select list (buildDerivedTableSource — the
// same deriver the semantic scope uses), so `JA JOIN (SELECT c1, b2 FROM
// JB) AS X USING (c1)` hides X's c1 exactly like a base-table leg
// (measured live: Java answers [C1 A2 B2]). Declines — keeping the
// legacy full-width star — only when a leg genuinely cannot be
// enumerated (an UNDERIVABLE derived body such as a join-bodied one,
// a lateral unnest, a catalog-aware sub-plan): a partial expansion would
// silently drop that leg's columns. Measured today that decline is
// UNREACHABLE for USING legs — the one underivable derived shape
// (join-bodied) fail-closes 0AF00 at the ON-scope drop-risk gate before
// any star expansion; TestFDB_JoinUsingStarHidesRightColumns's
// underivable-leg subtest pins that unreachability and re-arms the
// hidden-star expectation if the shape ever plans.
func expandBareStarOverUsingJoins(sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) bool {
	if sq == nil || md == nil || sq.projCols != nil || sq.projQualifier != "" ||
		sq.countStar || len(sq.aggCols) > 0 {
		return false
	}
	anyHidden := false
	for _, j := range sq.joins {
		if len(j.usingHiddenCols) > 0 {
			anyHidden = true
			break
		}
	}
	if !anyHidden {
		return false
	}
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	columnsFor := func(tableName string) ([]string, bool) {
		if src, found := cteScopes[strings.ToUpper(tableName)]; found {
			if src.Table == nil {
				return nil, false
			}
			cteCols := src.Table.Columns()
			cols := make([]string, len(cteCols))
			for i, c := range cteCols {
				cols[i] = strings.ToUpper(c.Id.Name())
			}
			return cols, true
		}
		rt := md.GetRecordType(tableName)
		if rt == nil || rt.Descriptor == nil {
			return nil, false
		}
		fields := rt.Descriptor.Fields()
		cols := make([]string, fields.Len())
		for i := 0; i < fields.Len(); i++ {
			cols[i] = strings.ToUpper(string(fields.Get(i).Name()))
		}
		return cols, true
	}
	type leg struct {
		qual   string
		cols   []string
		hidden map[string]struct{}
	}
	derivedColumns := func(alias string, inner antlrgen.IQueryContext, body logical.LogicalOperator) ([]string, bool) {
		src, err := boundDerivedSource(md, alias, "", inner, body, sq.enclosingScope, schemaName, cteScopes)
		if err != nil || src.Table == nil {
			return nil, false
		}
		srcCols := src.Table.Columns()
		cols := make([]string, len(srcCols))
		for i, c := range srcCols {
			cols[i] = strings.ToUpper(c.Id.Name())
		}
		return cols, true
	}
	primaryAlias := sq.tableAlias
	if primaryAlias == "" {
		primaryAlias = sq.tableName
	}
	var primaryCols []string
	var ok bool
	if sq.inlineValues != nil {
		if src, found := parsedInlineValuesScopeSource(sq.inlineValues, sq.tableAlias, "", md); found && src.Table != nil {
			srcCols := src.Table.Columns()
			primaryCols = make([]string, len(srcCols))
			for i, c := range srcCols {
				primaryCols[i] = strings.ToUpper(c.Id.Name())
			}
			ok = true
		}
	} else if sq.derivedQuery != nil {
		primaryCols, ok = derivedColumns(sq.tableName, sq.derivedQuery, sq.catalogAwareInnerPlan)
	} else {
		primaryCols, ok = columnsFor(sq.tableName)
	}
	if !ok {
		return false
	}
	legs := []leg{{qual: primaryAlias, cols: primaryCols}}
	for i, j := range sq.joins {
		alias := j.alias
		if alias == "" {
			alias = j.tableName
		}
		var cols []string
		switch {
		case j.inlineValues != nil:
			src, found := parsedInlineValuesScopeSource(j.inlineValues, j.alias, j.bindingID, md)
			if !found || src.Table == nil {
				return false
			}
			srcCols := src.Table.Columns()
			cols = make([]string, len(srcCols))
			for k, c := range srcCols {
				cols[k] = strings.ToUpper(c.Id.Name())
			}
			ok = true
		case j.derivedQuery != nil:
			// A catalog-aware inner plan may coexist with the parsed
			// derived body; the SELECT LIST is still the column
			// authority, so enumerate from it.
			cols, ok = derivedColumns(alias, j.derivedQuery, j.catalogAwareInnerPlan)
		case j.catalogAwareInnerPlan != nil:
			return false
		default:
			visible := visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i], resolvesToTable)
			if isLateralUnnestJoin(j, visible, resolvesToTable) {
				return false
			}
			cols, ok = columnsFor(j.tableName)
		}
		if !ok {
			return false
		}
		legs = append(legs, leg{qual: alias, cols: cols, hidden: hiddenColumnSet(j.usingHiddenCols)})
	}
	var projCols []projCol
	for _, l := range legs {
		for _, c := range l.cols {
			if _, hide := l.hidden[c]; hide {
				continue
			}
			projCols = append(projCols, projCol{name: l.qual + "." + c, bare: c, qualifier: l.qual, qualified: true})
		}
	}
	sq.projCols = projCols
	sq.projAliases = make([]string, len(projCols))
	sq.projExprs = make([]antlrgen.IExpressionContext, len(projCols))
	sq.projStarQualifiers = make([]string, len(projCols))
	return true
}

// expandProjQualifier handles `SELECT <qualifier>.*` when it is the
// only SELECT element (projQualifier set, projCols nil). Expands the
// qualifier into explicit projCols with qualified column names
// (`qualifier.COL`) so buildLogicalPlanForSelect emits a LogicalProject
// that restricts the output to that source's columns. Without this,
// JOIN queries with a lone qualified star would project all columns
// from all sources (the nil-projCols path in buildLogicalPlanForSelect
// skips the projection node entirely).
//
// For single-table queries `a.*` is equivalent to `*`, so the expansion
// is technically unnecessary but harmless — the resulting projection
// lists the same columns the scan produces.
// A lone `V.*` over a lateral array unnest alias (`SELECT V.* FROM t, t.arr AS
// V`) expands to the unnest's element column(s) (and the ordinal under WITH
// ORDINALITY) via the SHARED unnest virtual source — NOT only real record types.
// Without this the qualifier resolves to nothing and the query falls through to
// the nil-projCols path → returns the ENTIRE FlatMap row instead of just the
// unnest source's columns (silent-wrong). RFC-142.
// expandBareStarFromScope publishes the visible SQL attributes when they differ
// from the physical row. Projection elision requires the same names and ordinal
// mapping, not merely a SELECT *: a CTE can expose K,K over physical K,K_2.
func expandBareStarFromScope(sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) (bool, error) {
	if sq == nil || md == nil || sq.projCols != nil || sq.projQualifier != "" || sq.countStar || len(sq.aggCols) > 0 {
		return false, nil
	}
	resolver, err := buildSelectScopeChecked(sq, md, schemaName, cteScopes)
	if err != nil {
		return false, err
	}
	needsProjection := false
	for _, source := range resolver.Scope().Sources() {
		needed, err := starSourceNeedsProjection(source)
		if err != nil {
			return false, err
		}
		needsProjection = needsProjection || needed
	}
	if !needsProjection {
		return false, nil
	}
	columns, err := starColumnsFromScopeChecked(resolver, "")
	if err != nil {
		return false, err
	}
	sq.projCols = columns
	sq.projAliases = make([]string, len(columns))
	for i, column := range columns {
		sq.projAliases[i] = column.bare
	}
	sq.projExprs = make([]antlrgen.IExpressionContext, len(columns))
	sq.projStarQualifiers = make([]string, len(columns))
	return true, nil
}

// starSourceNeedsProjection is the source-level identity check for bare-star
// publication. Hidden attributes do not occupy output positions; SQL labels and
// declared attribute ordinals must match the complete flowed row to elide it.
func starSourceNeedsProjection(source semantic.ScopeSource) (bool, error) {
	if source.Table == nil {
		return false, api.NewError(api.ErrCodeUnsupportedQuery, "star source has no declared attributes")
	}
	columns := source.Table.Columns()
	if source.ColumnOrdinals != nil && len(source.ColumnOrdinals) != len(columns) {
		return false, api.NewError(api.ErrCodeUnsupportedQuery, "star source has an incomplete attribute layout")
	}
	if source.FlowedObject != nil || source.Shadowing {
		return true, nil
	}
	row := expr.SourceRowType(source)
	if row == nil {
		return false, api.NewError(api.ErrCodeUnsupportedQuery, "star source has no exact flowed row")
	}
	needed, visible := false, 0
	for position, column := range columns {
		_, hidden := source.HiddenColumns[strings.ToUpper(column.Id.Name())]
		if column.Ephemeral || hidden {
			needed = true
			continue
		}
		ordinal := position
		if source.ColumnOrdinals != nil {
			ordinal = source.ColumnOrdinals[position]
		}
		if ordinal < 0 || ordinal >= len(row.Fields) {
			return false, api.NewError(api.ErrCodeUnsupportedQuery, "star attribute is outside its flowed row")
		}
		needed = needed || ordinal != visible || column.Id.Name() != row.Fields[ordinal].Name
		visible++
	}
	return needed || visible != len(row.Fields), nil
}

// starColumnsFromScope is Java's expandStar, Case 1 and Case 2
// (SemanticAnalyzer.java:321-368): the star expands to the FROM-side operators'
// output columns, in FROM order, filtered by nonEphemeralVisible
// (Expressions.java:163-166). Case 2 restricts that output to one qualifier.
//
// The semantic scope IS that output. Deriving the list from the scope rather
// than re-walking the FROM parse tree is the point: the scope already carries
// derived-table bodies, CTEs and lateral unnest bindings under a single rule, so
// the star cannot name a source differently from the resolver that binds a
// reference to it. HiddenColumns is Go's carrier for Java's isVisible() — the
// right-hand copy of a JOIN … USING column.
//
// A GROUP BY alias is deliberately NOT a candidate here. Java files an aliased
// grouping item as an EphemeralExpression (ExpressionVisitor.java:252-256) and
// splices it into the operator output for NAME RESOLUTION only
// (QueryVisitor.java:281-286); expandStar's nonEphemeralVisible filter drops it
// again. That is why `select * from (select col1 from T1) as X group by col1 AS Y`
// outputs COL1 and not Y.
//
// ok=false means the sources' columns are not knowable, which every caller must
// treat as "cannot expand" — never as "expands to nothing". An empty expansion
// and an unresolvable one are the same value in a naive encoding, and they have
// opposite correct handling.
func starColumnsFromScope(resolver *expr.Resolver, qualifier string) ([]projCol, bool) {
	columns, err := starColumnsFromScopeChecked(resolver, qualifier)
	return columns, err == nil
}

func starColumnsFromScopeChecked(resolver *expr.Resolver, qualifier string) ([]projCol, error) {
	if resolver == nil || resolver.Scope() == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "star has no exact semantic scope")
	}
	sources := resolver.Scope().Sources()
	if qualifier != "" {
		var selected []semantic.ScopeSource
		for _, source := range sources {
			if source.Alias.EqualsIgnoreQuoting(semantic.FromNormalized(qualifier)) {
				// Java's qualified star selects the first operator, unlike named
				// lookup, which counts all matching attributes.
				if expr.SourceRowType(source) == nil {
					return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference, "attempt to expand non-struct column %s", qualifier)
				}
				selected = []semantic.ScopeSource{source}
				break
			}
		}
		if len(selected) == 0 {
			// No operator alias matched. The grammar supplies one identifier;
			// a dot inside a quoted identifier remains part of that identifier.
			value, err := resolver.ResolveIdentifierPath([]semantic.Identifier{semantic.FromNormalized(qualifier)})
			if err != nil {
				if mapped := mapColumnResolveError(err, qualifier); mapped != nil {
					return nil, mapped
				}
				return nil, err
			}
			record, ok := value.Type().(*values.RecordType)
			if !ok {
				return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference, "attempt to expand non-struct column %s", qualifier)
			}
			columns := make([]projCol, 0, len(record.Fields))
			for i, field := range record.Fields {
				request, err := values.FieldByNameAndOrdinal(field.Name, i)
				if err != nil {
					return nil, err
				}
				bound, err := values.ResolveFieldAccess(value, []values.FieldRequest{request})
				if err != nil {
					return nil, err
				}
				columns = append(columns, projCol{bound: bound, name: qualifier + "." + field.Name, bare: field.Name, qualifier: qualifier, qualified: true})
			}
			return columns, nil
		}
		sources = selected
	}
	var columns []projCol
	for _, source := range sources {
		if source.Table == nil {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery, "star source has no declared attributes")
		}
		for position, column := range source.Table.Columns() {
			name := column.Id.Name()
			if column.Ephemeral {
				continue
			}
			if _, hidden := source.HiddenColumns[strings.ToUpper(name)]; hidden {
				continue
			}
			bound, err := expr.SourceColumnValue(source, position)
			if err != nil {
				return nil, err
			}
			columns = append(columns, projCol{bound: bound, name: source.Alias.Name() + "." + name, bare: name, qualifier: source.Alias.Name(), qualified: true, segs: []string{source.Alias.Name(), name}})
		}
	}
	if len(columns) == 0 {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "star source publishes no visible attributes")
	}
	return columns, nil
}

// starExpanderFor adapts a FROM clause into the expander
// classifySelectElements consumes, DEFERRING the scope construction to the
// first expansion that actually asks for it.
//
// The laziness is the point. visitSimpleTableBody runs for every SELECT, while
// the expander is consulted only by the star-under-GROUP-BY branch of the
// classifier — a small minority of queries. Building the FROM-only scope
// eagerly put a second full scope construction on the hot path of every query
// to pay for a branch most never reach.
//
// NIL-NESS IS STILL DECIDED EAGERLY, and must be: a nil expander is the
// classifier's "no expansion available" signal, which keeps the asserted 42803
// refusal rather than silently dropping the GROUP BY. That decision cannot
// depend on whether anything happened to ask.
//
// The result is memoised INCLUDING a nil resolver — a scope that could not be
// built is a determination, not a retry. starColumnsFromScope answers
// (nil, false) for it, which is the same "cannot expand" the classifier needs.
// This closure is called from one goroutine per plan build; it is not shared.
func starExpanderFor(fs *fromSource, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) starExpander {
	if fs == nil || md == nil {
		return nil
	}
	var (
		resolver *expr.Resolver
		built    bool
	)
	return func(qualifier string) ([]projCol, bool) {
		if !built {
			built = true
			resolver = buildFromOnlySelectScope(fs, md, schemaName, cteScopes)
		}
		return starColumnsFromScope(resolver, qualifier)
	}
}

// buildFromOnlySelectScope builds the semantic scope from the FROM clause
// ALONE, before the SELECT list is classified.
//
// Star expansion needs the sources' columns, and classification needs the star
// expansion, so the scope has to exist first. buildSelectScope reads only the
// FROM-derived fields of its selectQuery (tableName, tableAlias, derivedQuery,
// joins), so a classification-free shell is a complete input — this is not a
// partial build that later grows. Java has the same ordering: the select-where
// operator is generated (QueryVisitor.java:275) before visitSelectElements
// expands the star against it (QueryVisitor.java:286).
func buildFromOnlySelectScope(fs *fromSource, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) *expr.Resolver {
	if fs == nil || md == nil {
		return nil
	}
	return buildSelectScope(selectQueryFromClassification(&selectClassification{}, fs), md, schemaName, cteScopes)
}

// normalizeSoleQualifiedStar routes SELECT alias.* through the same expansion
// as a mixed projection. Both forms must use the visible source's carried row,
// including derived/CTE outputs, rather than interpreting its alias as a table.
func normalizeSoleQualifiedStar(sq *selectQuery) {
	sq.projCols = []projCol{{name: "*", bare: "*"}}
	sq.projAliases = []string{""}
	sq.projExprs = []antlrgen.IExpressionContext{nil}
	sq.projStarQualifiers = []string{sq.projQualifier}
	sq.projQualifier = ""
}

// validateUnionColumnCounts checks that all UNION branches project the
// same number of columns. Matches Java's SemanticAnalyzer.validateUnionTypes
// column-count check (ErrorCode.UNION_INCORRECT_COLUMN_COUNT / 42F64).
func validateUnionColumnCounts(inputs []logical.LogicalOperator) error {
	if len(inputs) < 2 {
		return nil
	}
	firstCount := countProjectionColumns(inputs[0])
	if firstCount < 0 {
		return nil
	}
	for i := 1; i < len(inputs); i++ {
		c := countProjectionColumns(inputs[i])
		if c < 0 {
			continue
		}
		if c != firstCount {
			return api.NewErrorf(api.ErrCodeUnionIncorrectColumnCount,
				"UNION legs do not have the same number of columns")
		}
	}
	return nil
}

func countProjectionColumns(op logical.LogicalOperator) int {
	if op == nil {
		return -1
	}
	if proj, ok := op.(*logical.LogicalProject); ok {
		return len(proj.Projections)
	}
	for _, ch := range op.Children() {
		if n := countProjectionColumns(ch); n >= 0 {
			return n
		}
	}
	if scan, ok := op.(*logical.LogicalScan); ok {
		_ = scan
		return -1
	}
	return -1
}

func validateUnionOrderByColumns(sort *logical.LogicalSort, leftBranch logical.LogicalOperator) error {
	leftProj := findProjection(leftBranch)
	if leftProj == nil {
		return nil
	}
	// UNION publishes the LEFT branch's output row. Resolve every accepted
	// output spelling to that row's exact ordinal while the projection still
	// carries the SQL output contract; the Cascades translator must never turn
	// the spelling back into a field identity. First occurrence preserves the
	// prior first-match behaviour for duplicate output labels.
	leftOrdinals := make(map[string]int, len(leftProj.Projections)*2)
	register := func(name string, ordinal int) {
		if name == "" {
			return
		}
		key := strings.ToUpper(name)
		if _, exists := leftOrdinals[key]; !exists {
			leftOrdinals[key] = ordinal
		}
	}
	for i, col := range leftProj.Projections {
		register(col, i)
		// The bare form comes from the RESOLVED channel: a childless
		// FieldValue's Field IS the bare column (the same structural truth
		// the upgrade passes bind), never a last-dot split of the rendering
		// — a delimited identifier containing a literal dot is one name.
		if i < len(leftProj.ProjectedValues) {
			if fv, ok := values.AsFieldValue(leftProj.ProjectedValues[i]); ok {
				register(fv.DisplayName(), i)
			}
		}
		if i < len(leftProj.Aliases) && leftProj.Aliases[i] != "" {
			register(leftProj.Aliases[i], i)
		}
		if i < len(leftProj.ProjectionRefs) && leftProj.ProjectionRefs[i].Present {
			register(leftProj.ProjectionRefs[i].Bare, i)
		}
	}
	for i := range sort.Keys {
		k := &sort.Keys[i]
		if k.Expr == "" {
			continue
		}
		// A POSITIONAL key binds to the union OUTPUT slot by ordinal — its
		// Expr carries the RIGHT leg's rendering of that slot (where the
		// parser resolved it), which legitimately differs from the left
		// leg's spelling. In-range is guaranteed upstream
		// (resolveSelectListPosition errors out-of-range) plus the union's
		// equal-column-count validation. RFC-180.
		if k.Pos > 0 {
			continue
		}
		upper := strings.ToUpper(k.Expr)
		bareName := upper
		if k.Bare != "" {
			// Structured bare segment — never a last-dot split of the
			// rendering (a delimited identifier may contain a literal dot).
			bareName = strings.ToUpper(k.Bare)
		}
		ordinal, found := leftOrdinals[upper]
		if !found {
			ordinal, found = leftOrdinals[bareName]
		}
		if !found {
			return api.NewErrorf(api.ErrCodeUndefinedColumn,
				"column %q not found in UNION result columns", k.Expr)
		}
		// Pos is the existing exact union-output ordinal carrier. The key is
		// no longer a name consumer after this validation point.
		k.Pos = ordinal + 1
	}
	return nil
}

func validateUnionColumnTypes(inputs []logical.LogicalOperator, md *recordlayer.RecordMetaData) error {
	if md == nil || len(inputs) < 2 {
		return nil
	}
	firstTypes := resolveProjectionTypes(inputs[0], md)
	if firstTypes == nil {
		return nil
	}
	for i := 1; i < len(inputs); i++ {
		otherTypes := resolveProjectionTypes(inputs[i], md)
		if otherTypes == nil {
			continue
		}
		n := len(firstTypes)
		if len(otherTypes) < n {
			n = len(otherTypes)
		}
		for j := 0; j < n; j++ {
			if firstTypes[j] == 0 || otherTypes[j] == 0 {
				continue
			}
			lCat := unionTypeCategory(firstTypes[j])
			rCat := unionTypeCategory(otherTypes[j])
			if lCat == 0 || rCat == 0 {
				continue
			}
			if lCat != rCat {
				return api.NewErrorf(api.ErrCodeUnionIncompatibleColumns,
					"Incompatible column types in UNION legs")
			}
		}
	}
	return nil
}

func unionTypeCategory(k protoreflect.Kind) int {
	switch k {
	case protoreflect.BoolKind:
		return 1
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind,
		protoreflect.FloatKind, protoreflect.DoubleKind:
		return 2
	case protoreflect.StringKind:
		return 3
	case protoreflect.BytesKind:
		return 4
	case protoreflect.EnumKind:
		return 5
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return 6
	}
	return 0
}

func findScanTable(op logical.LogicalOperator) string {
	for cur := op; cur != nil; {
		if scan, ok := cur.(*logical.LogicalScan); ok {
			return scan.Table
		}
		ch := cur.Children()
		if len(ch) != 1 {
			return ""
		}
		cur = ch[0]
	}
	return ""
}

func resolveProjectionTypes(op logical.LogicalOperator, md *recordlayer.RecordMetaData) []protoreflect.Kind {
	proj := findProjection(op)
	if proj == nil {
		return nil
	}
	tableName := findScanTable(op)
	if tableName == "" {
		return nil
	}
	rt := md.GetRecordType(tableName)
	if rt == nil || rt.Descriptor == nil {
		return nil
	}
	desc := rt.Descriptor
	kinds := make([]protoreflect.Kind, len(proj.Projections))
	for i, col := range proj.Projections {
		if i < len(proj.IsComputed) && proj.IsComputed[i] {
			continue
		}
		bare := parseColRef(col).bare()
		fd := desc.Fields().ByName(protoreflect.Name(strings.ToLower(bare)))
		if fd == nil {
			fd = desc.Fields().ByName(protoreflect.Name(bare))
		}
		if fd != nil {
			kinds[i] = fd.Kind()
		}
	}
	return kinds
}

// buildLogicalPlanForUnionWithCatalog mirrors buildLogicalPlanForUnion
// — same flattening logic, threads md to each branch.
//
// Trailing ORDER BY: the ANTLR grammar greedily attaches a trailing
// ORDER BY to the rightmost SimpleTable, but SQL standard says it
// applies to the whole UNION result. Mirror the lift in execUnion
// (union.go): strip ORDER BY from the right branch's selectQuery
// before building it, then wrap the final LogicalUnion in a
// LogicalSort using the lifted keys.
func buildLogicalPlanForUnionWithCatalog(
	setQ *antlrgen.SetQueryContext,
	md *recordlayer.RecordMetaData,
) (logical.LogicalOperator, error) {
	if setQ == nil {
		return nil, nil
	}
	if setQ.ALL() == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "only UNION ALL is supported")
	}
	left, err := buildLogicalPlanForQueryBodyWithCatalog(setQ.GetLeft(), md)
	if err != nil {
		return nil, err
	}

	// Same ORDER BY / LIMIT stripping as the CTE-catalog variant.
	var lifted unionLiftedClauses
	var right logical.LogicalOperator
	right, lifted, err = buildUnionRightBranchStrippingOrderBy(setQ.GetRight(), md, defaultEmbeddedSchema, nil, nil)
	if err != nil {
		return nil, err
	}
	if left == nil || right == nil {
		return nil, nil
	}

	if len(lifted.sortKeys) == 0 {
		if s, ok := right.(*logical.LogicalSort); ok {
			lifted.sortKeys = s.Keys
			right = s.Input
		} else if p, ok := right.(*logical.LogicalProject); ok {
			if s, ok := p.Input.(*logical.LogicalSort); ok {
				lifted.sortKeys = s.Keys
				p.Input = s.Input
			}
		}
	}

	inputs := []logical.LogicalOperator{left, right}
	if innerUnion, ok := left.(*logical.LogicalUnion); ok && !innerUnion.Distinct {
		inputs = append(append([]logical.LogicalOperator(nil), innerUnion.Inputs...), right)
	}
	if err := validateUnionColumnCounts(inputs); err != nil {
		return nil, err
	}
	if err := validateUnionColumnTypes(inputs, md); err != nil {
		return nil, err
	}
	if len(lifted.sortKeys) > 0 {
		liftedSort := &logical.LogicalSort{Keys: lifted.sortKeys}
		if err := validateUnionOrderByColumns(liftedSort, inputs[0]); err != nil {
			return nil, err
		}
	}
	var result logical.LogicalOperator = logical.NewUnion(inputs, false)
	if len(lifted.sortKeys) > 0 {
		result = logical.NewSort(result, lifted.sortKeys)
	}
	if lifted.limit >= 0 || lifted.offset > 0 {
		result = logical.NewLimit(result, lifted.limit, lifted.offset)
	}
	return result, nil
}

// existsSubqueryPlanner implements expr.SubqueryPlanner. It builds
// logical plans for EXISTS and scalar subqueries and collects the
// (alias, plan) pairs that the LogicalFilter/LogicalProject need to
// carry to the Cascades translator.
func buildOuterScopeSources(sq *selectQuery, md *recordlayer.RecordMetaData, schemaName string, cteScopes map[string]semantic.ScopeSource) []semantic.ScopeSource {
	// A DUPLICATE-PRESERVING slice in FROM order, never an alias-keyed map:
	// duplicate outer aliases are legal, and a
	// map collapsed them last-wins — an inner correlated reference then saw
	// only ONE leg (false 42703 for the lost leg's columns; a missed terminal
	// ambiguity for shared ones) and bound the survivor under the DISPLAY
	// alias, mis-correlating a later duplicate leg whose row namespace is its
	// minted BINDING. Every source carries bindingOrAlias — the same
	// convention as the SELECT/WHERE scope builders — so per-attribute
	// resolution and QOV emission work across scope depth exactly as at the
	// top level (the ladder: 1→bind, 0→fallthrough, ≥2→terminal 42702).
	if sq == nil || md == nil || sq.tableName == "" {
		return nil
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	var sources []semantic.ScopeSource
	addSrc := func(tableName, alias, bindingID string) {
		a := semantic.FromNormalized(alias)
		if alias == "" {
			a = semantic.FromNormalized(tableName)
		}
		// CTE-FIRST, in lockstep with buildSelectScope's addSource, because a
		// subquery's outer scope must see the SAME FROM clause as the query it is
		// nested in. A WITH leg is not a catalog table, so a catalog-only lookup
		// returned silently here: the outer scope held every REAL leg and no CTE
		// leg, and a correlated reference to the CTE alias died 42703 ("no FROM
		// source aliased as C") while the identical correlation to a base-table
		// alias resolved. The DERIVED-table leg below was registered for exactly
		// this reason; the CTE leg was the residual gap left beside it.
		//
		// The ORDER is not incidental. A declared CTE SHADOWS a same-named catalog
		// table, and resolving the table instead would analyze the TABLE's schema
		// for reads that execute against the CTE. A TOMBSTONE entry (nil Table — a
		// declared CTE whose schema is not derivable here) must therefore DECLINE
		// rather than fall through, or a same-named base table would bind its
		// ordinals onto the CTE's rows.
		//
		// The registry supplies the COLUMN SCHEMA only: alias and correlation come
		// from THIS reference, so `FROM c AS x` binds under X and a duplicated CTE
		// leg keeps its own binding id, exactly as a duplicated real table does.
		if src, found := cteScopes[strings.ToUpper(tableName)]; found {
			if src.Table == nil {
				return
			}
			sources = append(sources, cteSourceAs(src, a, bindingOrAlias(bindingID, a)))
			return
		}
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err != nil {
			return
		}
		sources = append(sources, semantic.ScopeSource{
			Table: tbl, Alias: a, CorrelationName: bindingOrAlias(bindingID, a),
		})
	}
	// A DERIVED-TABLE source (`FROM (SELECT ...) e`) is NOT a real table
	// either — register its VIRTUAL column schema (the SAME
	// buildDerivedTableSource the SELECT scope uses) so a CORRELATED
	// subquery referencing the derived alias resolves it. Without this,
	// addSrc's ResolveTable fails silently and the correlated reference
	// dies 42703 ("no FROM source aliased as E" single-source, `qualifier
	// "E" cannot be resolved` join form) — while the identical correlation
	// to a REAL table alias works. Mirrors the lateral-unnest leg
	// registration below.
	addDerived := func(alias, bindingID string, inner antlrgen.IQueryContext, body logical.LogicalOperator) {
		if src, err := boundDerivedSource(md, alias, bindingID, inner, body, sq.enclosingScope, schemaName, cteScopes); err == nil {
			sources = append(sources, src)
		}
	}
	if sq.inlineValues != nil {
		if src, ok := parsedInlineValuesScopeSource(sq.inlineValues, sq.tableAlias, "", md); ok {
			sources = append(sources, src)
		}
	} else if sq.derivedQuery != nil {
		// The primary derived source: the parser carries the alias in
		// tableAlias when present, else in tableName (the same convention
		// buildWherePredicateForDerived resolves against). Its runtime binding
		// may differ when this query block has an enclosing row scope.
		alias := sq.tableAlias
		if alias == "" {
			alias = sq.tableName
		}
		addDerived(alias, sq.bindingID, sq.derivedQuery, sq.catalogAwareInnerPlan)
	} else {
		addSrc(sq.tableName, sq.tableAlias, "")
	}
	resolvesToTable := newUnnestTableResolver(md, schemaName)
	for i, j := range sq.joins {
		if j.inlineValues != nil {
			if src, ok := parsedInlineValuesScopeSource(j.inlineValues, j.alias, j.bindingID, md); ok {
				sources = append(sources, src)
			}
			continue
		}
		// A lateral array unnest leg (`FROM t, t.arr AS x [AT ord]`) is NOT a real
		// table; register its VIRTUAL Shadowing source (the SAME one the SELECT scope
		// uses, via unnestVirtualScopeSourceWithElement) so a CORRELATED subquery referencing the
		// unnested element/ordinal resolves it. Without this the inner EXISTS / scalar
		// subquery's outer scope sees only the REAL tables and the correlated
		// reference (`WHERE U.V = VAL`) fails → a generic Cascades translation failure
		// (P2c). The existing EXISTS-over-unnest lowering binds it at
		// execution. RFC-142.
		visible := visibleFromAliases(sq.tableName, sq.tableAlias, sq.joins[:i], resolvesToTable)
		if isLateralUnnestJoin(j, visible, resolvesToTable) {
			// The element's DECLARED FIELDS travel with the binding, exactly as
			// they do on the SELECT scope (unnestScopeSourceAdder). Without them
			// the outer scope exposes a fieldless element column and a correlated
			// reference to a STRUCT member (`… WHERE m.id = x.ek`) died 42703
			// while the identical reference resolved outside the subquery — one
			// binding described two ways.
			element, typed := unnestElementColumnFromSources(sources, j)
			var elementPtr *semantic.Column
			if typed {
				elementPtr = &element
			}
			if src, ok := unnestVirtualScopeSourceWithElement(j, elementPtr); ok {
				sources = append(sources, src)
			}
			continue
		}
		if j.derivedQuery != nil {
			addDerived(j.alias, j.bindingID, j.derivedQuery, j.catalogAwareInnerPlan)
			continue
		}
		addSrc(j.tableName, j.alias, j.bindingID)
	}
	return sources
}

type existsSubqueryPlanner struct {
	md *recordlayer.RecordMetaData
	// schemaName is the ACTIVE session schema. EXISTS / scalar subquery plans are
	// built through buildLogicalPlanForQueryWithCTECatalog, which threads this into
	// the schema-qualified-table demotion (demoteSchemaQualifiedUnnest /
	// normalizeSchemaQualifiedSelectSources): a `… EXISTS (SELECT 1 FROM PA AS main,
	// main.PB AS B)` in a session whose schema is `main` resolves `main.PB` as the
	// schema-qualified TABLE against the ACTIVE schema, not the hardcoded default
	// `s`. Empty falls back to defaultEmbeddedSchema. RFC-142 (P2b).
	schemaName string
	// outerScopes is a DUPLICATE-PRESERVING slice in FROM order — see
	// buildOuterScopeSources.
	outerScope                 *semantic.Scope
	outerScopes                []semantic.ScopeSource
	cteScopes                  map[string]semantic.ScopeSource
	cteOnScopes                map[string]semantic.ScopeSource    // ON-resolution-only CTE sources (join/unnest bodies; see buildCTEOnOnlySource)
	cteBodies                  map[string]logical.LogicalOperator // CTE name → body plan, for wrapping scalar subquery plans
	subqueries                 []logical.ExistsSubquery
	scalarSubqueries           []logical.ScalarSubquery
	correlatedScalarSubqueries []logical.CorrelatedScalarSubquery
	lastJoinPredicate          predicates.QueryPredicate
	// lastJoinPredicateOuterOnly mirrors lastJoinPredicate: the Case-1
	// nested-EXISTS middle routes OUTER-ONLY conjuncts through the join
	// predicate (the inside placement does not plan); the flag travels
	// onto the ExistsSubquery so the anti-join consumer can decline.
	lastJoinPredicateOuterOnly bool
}

// visibleScopeNames is the upper-cased set of every user-visible SQL name a
// subquery-level mint must avoid: each outer scope's Alias AND
// CorrelationName (the latter carries enclosing mints and dup-alias binding
// ids) plus the CTE registry's names. The collision invariant ("no
// generated identity equals a user-visible name") is enforced by TWO
// mechanisms with disjoint jobs: this SET covers names that can co-occur in
// the binding's resolution/registration context — the scope CHAIN (parent
// scopes carry an enclosing mint via nestedOuterScopes' CorrelationName)
// plus the CTE registry — while counter MONOTONICITY covers generated-vs-
// generated (two mints from one strictly-increasing counter can never
// collide, skip loop included, since skipping only advances). A sibling
// subquery at another level is deliberately EXCLUDED from the set: its
// binding registers at its own parent select, which is not in this
// subquery's chain — no co-occurrence, no hazard; do not "fix" the set by
// stuffing sibling names in. An ALIASED outer CTE leg is dropped from
// p.outerScopes by addSrc's silent resolve-failure arm and so escapes this
// set — booked with the outer-CTE-leg scope-registration gap (loud today:
// correlated refs to such legs die 42703).
func (p *existsSubqueryPlanner) visibleScopeNames() map[string]struct{} {
	visible := map[string]struct{}{}
	for _, src := range p.outerScopes {
		if n := src.Alias.Name(); n != "" {
			visible[strings.ToUpper(n)] = struct{}{}
		}
		if src.CorrelationName != "" {
			visible[strings.ToUpper(src.CorrelationName)] = struct{}{}
		}
	}
	for name := range p.cteScopes {
		visible[strings.ToUpper(name)] = struct{}{}
	}
	return visible
}

// mintSubqueryAlias mints the subquery binding identity (the esq / scalar /
// correlated-scalar Alias), skipping candidates a user-visible SQL name
// already spells. Without the skip, a quoted alias like `"Q$3"` regresses a
// VALID query to a loud planner failure whenever the process-global counter
// lands on it — and any counter-consuming change (e.g. the inner-correlation
// mint's own skip) SHIFTS which queries hit the alignment, so the failure
// set depends on planning history.
func (p *existsSubqueryPlanner) mintSubqueryAlias() values.CorrelationIdentifier {
	return mintDistinctIdentifier(p.visibleScopeNames(), values.UniqueCorrelationIdentifier)
}

// tryBuildCorrelatedPrimaryUnnest recognizes Java's correlated-array primary
// source inside EXISTS:
//
//	EXISTS (SELECT E FROM R.TAGS AS E WHERE E = 9)
//
// The generic SELECT builder cannot classify this source because a primary
// FROM item normally has no source to its left. Here, however, R belongs to the
// enclosing query. Resolve the collection through the outer semantic scope and
// build a standalone LogicalUnnest carrying that resolved Value. The path is
// deliberately narrow: one direct array field, one explicit element alias,
// and no query-shaping clauses. Wider recognized shapes fail loudly rather
// than being rebuilt as a phantom table scan.
func (p *existsSubqueryPlanner) tryBuildCorrelatedPrimaryUnnest(
	q antlrgen.IQueryContext,
) (logical.LogicalOperator, bool, error) {
	if q == nil || len(p.outerScopes) == 0 || p.md == nil {
		return nil, false, nil
	}
	body, ok := q.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return nil, false, nil
	}
	sq, err := extractFromQueryTerm(body)
	if err != nil || sq == nil || sq.derivedQuery != nil || len(sq.sourceSegments) < 2 {
		return nil, false, nil
	}
	// Java resolves a real table before falling through to correlated-field
	// access. Preserve that precedence for an active-schema-qualified table.
	if newUnnestTableResolver(p.md, p.effectiveSchemaName())(sq.sourceSegments) {
		return nil, false, nil
	}

	ownerID := semantic.FromNormalized(sq.sourceSegments[0])
	fieldID := semantic.FromNormalized(sq.sourceSegments[1])
	var owner *semantic.ScopeSource
	var col semantic.Column
	aliasSeen := false
	// STRICT over every alias-matching source, then RELAXED over every one —
	// never a mix. This loop COUNTS owners across sources and raises 42702 on
	// two, so relaxing per source would let a case-insensitive match at one
	// compete with an exact match at another and make a reference with exactly
	// one right answer ambiguous. Same rule, same reason, as
	// Scope.ResolveColumn and usingOwnerOf.
	for _, relaxed := range [...]bool{false, true} {
		for i := range p.outerScopes {
			src := &p.outerScopes[i]
			if !src.Alias.EqualsIgnoreQuoting(ownerID) {
				continue
			}
			aliasSeen = true
			if src.Table == nil {
				continue
			}
			var candidateColumn semantic.Column
			var found bool
			if relaxed {
				candidateColumn, found = semantic.LookupColumnRelaxed(src.Table, fieldID)
			} else {
				candidateColumn, found = src.Table.LookupColumn(fieldID)
			}
			if !found {
				continue
			}
			if owner != nil {
				return nil, true, api.NewErrorf(api.ErrCodeAmbiguousColumn,
					"correlated array source %q is ambiguous", strings.Join(sq.sourceSegments, "."))
			}
			owner = src
			col = candidateColumn
		}
		if owner != nil {
			break
		}
	}
	if owner == nil {
		if aliasSeen {
			return nil, true, api.NewErrorf(api.ErrCodeUndefinedColumn,
				"column %q does not exist on source %q", sq.sourceSegments[1], sq.sourceSegments[0])
		}
		return nil, false, nil
	}
	if len(sq.sourceSegments) != 2 {
		return nil, true, api.NewError(api.ErrCodeUnsupportedQuery,
			"nested correlated array sources in an EXISTS primary FROM clause are not yet supported")
	}
	if owner.Table == nil {
		return nil, true, api.NewError(api.ErrCodeUnsupportedQuery,
			"a correlated array primary source requires a resolved outer table")
	}

	if !col.IsArray {
		return nil, true, api.NewError(api.ErrCodeInvalidColumnReference,
			"join correlation can occur only on a column of repeated (array) type")
	}

	innerAlias := sq.tableAlias
	if innerAlias == "" || strings.EqualFold(innerAlias, sq.tableName) {
		return nil, true, api.NewError(api.ErrCodeUnsupportedQuery,
			"a correlated array primary source in EXISTS requires an explicit element alias")
	}
	for _, src := range p.outerScopes {
		if src.Alias.EqualsIgnoreQuoting(semantic.FromNormalized(innerAlias)) ||
			(src.CorrelationName != "" && strings.EqualFold(src.CorrelationName, innerAlias)) {
			return nil, true, api.NewError(api.ErrCodeDuplicateAlias,
				"correlated array element alias collides with an outer source alias")
		}
	}
	if len(sq.joins) != 0 || sq.distinct || len(sq.orderBy) != 0 ||
		len(sq.groupBy) != 0 || len(sq.aggCols) != 0 || sq.countStar ||
		sq.havingExpr != nil || sq.qualifyExpr != nil || sq.limit >= 0 || sq.offset != 0 {
		return nil, true, api.NewError(api.ErrCodeUnsupportedQuery,
			"query-shaping clauses over a correlated array primary source in EXISTS are not yet supported")
	}
	// Validate the ignored EXISTS projection rather than silently accepting an
	// invalid SELECT list. This first production slice admits exactly the
	// element binding (`SELECT E`); EXISTS does not otherwise consume it.
	if len(sq.projCols) != 1 || len(sq.projExprs) != 1 || sq.projExprs[0] != nil ||
		sq.projCols[0].qualified {
		return nil, true, api.NewError(api.ErrCodeUnsupportedQuery,
			"correlated array EXISTS currently requires projecting its element alias")
	}
	if sq.projCols[0].bare != innerAlias {
		return nil, true, api.NewErrorf(api.ErrCodeUndefinedColumn,
			"column %q does not exist", sq.projCols[0].bare)
	}

	cat := rlcatalog.Wrap(p.md)
	analyzer := semantic.NewAnalyzer(cat, false)
	outerScope := semantic.NewScope(nil)
	for _, src := range p.outerScopes {
		if addErr := outerScope.AddSource(src); addErr != nil {
			return nil, true, addErr
		}
	}
	aliasID := semantic.FromNormalized(innerAlias)
	innerScope := semantic.NewScope(outerScope)
	// The element column carries the array element's DECLARED FIELDS when that
	// element is a struct, so `x.ek` / `x.d.dk` inside the EXISTS body descend
	// through the ordinary struct-descent (Column.LookupStructField), exactly as
	// they do on the SELECT-scope binding. col.Type already names the ELEMENT
	// kind ("RECORD" for a struct array, the scalar kind otherwise) and
	// col.StructFields already IS the element's field list — dropping it here
	// left a "RECORD" column with no fields, which no descent can enter, so the
	// member reference died 42703 while resolving fine outside EXISTS. Java has
	// no such split: the unnest quantifier's flowed type IS the element type, so
	// the element's fields are the quantifier's own attributes
	// (LogicalOperator.generateCorrelatedFieldAccess emits one output per struct
	// field). This mint is the correlated-primary sibling of
	// unnestVirtualScopeSourceWithElement; the two must agree on what an element
	// binding exposes.
	virtual := &semantic.StaticTable{
		TableName: semantic.FromSegments([]string{innerAlias}, false),
		TableColumns: []semantic.Column{{
			Id:           aliasID,
			Type:         col.Type,
			Nullable:     true,
			StructFields: col.StructFields,
		}},
	}
	if addErr := innerScope.AddSource(semantic.ScopeSource{
		Table:           virtual,
		Alias:           aliasID,
		CorrelationName: aliasID.Name(),
		Shadowing:       true,
	}); addErr != nil {
		return nil, true, addErr
	}
	// Resolve the collection FROM THE INNER SCOPE so R is a parent-scope hit.
	// ResolveIdentifier then emits FieldValue(QOV(R), TAGS), preserving the
	// external correlation. Resolving against outerScope directly would treat
	// R as local and produce a childless baked FieldValue: executable only by
	// ambient-row accident and impossible to match to the candidate Explode.
	resolver := expr.New(analyzer, innerScope)
	collection, resolveErr := resolver.ResolveIdentifier(ownerID, fieldID)
	if resolveErr != nil {
		if mapped := mapPredicateWalkError(resolveErr); mapped != nil {
			return nil, true, mapped
		}
		return nil, true, resolveErr
	}

	unnest := &logical.LogicalUnnest{
		Segments:             append([]string(nil), sq.sourceSegments...),
		Alias:                innerAlias,
		CorrelatedCollection: collection,
	}
	if sq.whereExpr == nil || sq.whereExpr.Expression() == nil {
		return unnest, true, nil
	}
	if expr.ContainsSubqueryAtom(sq.whereExpr.Expression()) ||
		expr.ContainsExistsAtom(sq.whereExpr.Expression()) {
		return nil, true, api.NewError(api.ErrCodeUnsupportedQuery,
			"nested subqueries inside a correlated array EXISTS predicate are not yet supported")
	}
	pred, walkErr := resolver.WalkPredicate(sq.whereExpr.Expression())
	if walkErr != nil {
		if mapped := mapPredicateWalkError(walkErr); mapped != nil {
			return nil, true, mapped
		}
		return nil, true, walkErr
	}
	return &logical.LogicalFilter{
		Input:     unnest,
		Predicate: predicates.SimplifyPredicateValues(pred),
	}, true, nil
}

func (p *existsSubqueryPlanner) BuildExists(q antlrgen.IQueryContext) (values.CorrelationIdentifier, values.Type, error) {
	subqueryCount := len(p.subqueries)
	scalarCount := len(p.scalarSubqueries)
	correlatedScalarCount := len(p.correlatedScalarSubqueries)
	alias, err := p.buildExists(q)
	if err != nil {
		return values.CorrelationIdentifier{}, nil, err
	}
	if len(p.subqueries) != subqueryCount+1 {
		return values.CorrelationIdentifier{}, nil, fmt.Errorf("EXISTS: planner did not register exactly one subquery")
	}
	flowed, err := query.ExactLogicalResultType(p.subqueries[subqueryCount].Plan, p.md)
	if err != nil {
		// Exact typing is part of construction. Do not leave any plan registered
		// for an ExistsValue that was never admitted.
		p.subqueries = p.subqueries[:subqueryCount]
		p.scalarSubqueries = p.scalarSubqueries[:scalarCount]
		p.correlatedScalarSubqueries = p.correlatedScalarSubqueries[:correlatedScalarCount]
		return values.CorrelationIdentifier{}, nil, fmt.Errorf("EXISTS: derive exact inner result type: %w", err)
	}
	p.subqueries[subqueryCount].FlowedType = flowed
	return alias, flowed, nil
}

func (p *existsSubqueryPlanner) buildExists(q antlrgen.IQueryContext) (values.CorrelationIdentifier, error) {
	if q == nil {
		return values.CorrelationIdentifier{}, fmt.Errorf("EXISTS: nil query context")
	}
	innerOp, correlatedPrimaryUnnest, err := p.tryBuildCorrelatedPrimaryUnnest(q)
	if err != nil {
		return values.CorrelationIdentifier{}, err
	}
	if !correlatedPrimaryUnnest {
		innerOp, err = buildLogicalPlanForQueryWithCTECatalog(q, p.md, p.schemaName, p.cteScopes, p.cteOnScopes)
	}
	isUndefinedCol := false
	if err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.Code == api.ErrCodeUndefinedColumn {
			isUndefinedCol = true
		}
	}
	if err != nil && (!isUndefinedCol || len(p.outerScopes) == 0) {
		return values.CorrelationIdentifier{}, err
	}
	if isUndefinedCol && !correlatedPrimaryUnnest {
		p.lastJoinPredicate = nil
		p.lastJoinPredicateOuterOnly = false
		innerOp, err = p.buildCorrelatedExists(q)
		if err != nil {
			return values.CorrelationIdentifier{}, err
		}
	}
	if innerOp == nil {
		return values.CorrelationIdentifier{}, fmt.Errorf("EXISTS: inner query could not be planned")
	}
	// Keep an EXISTS plan self-contained when its inner FROM reads a CTE from
	// the enclosing WITH clause. The inner plan is translated through its own
	// existential Reference; it cannot rely on the outer LogicalCTE wrapper's
	// transient translator scope surviving that boundary. Scalar subqueries
	// already take this exact path in BuildScalar below. The wrapper helper is
	// selective (only referenced CTE names are added), so ordinary table-backed
	// EXISTS plans and correlated fallbacks retain their existing shape.
	innerOp = p.wrapWithOuterCTEs(innerOp)
	// The correlated fallback deliberately ignores the SELECT values (EXISTS
	// observes only cardinality), but that is not enough when an aggregate or
	// pagination changes cardinality. Classify those operators in SQL order:
	// first establish the non-grouped aggregate's exact one-row output, then
	// apply LIMIT/OFFSET. A known result is folded by the translator, avoiding
	// the semi-join entirely while preserving correlation semantics. A
	// data-dependent OFFSET or a pagination atom still unresolved at planning
	// time cannot ride the fallback
	// safely and is rejected typed-loud rather than reverting to raw row
	// existence. The uncorrelated path keeps its real Aggregate/Limit operators.
	var knownTruth predicates.TriBool
	if isUndefinedCol && !correlatedPrimaryUnnest {
		knownTruth, err = correlatedExistsTruthAfterPagination(q)
		if err != nil {
			return values.CorrelationIdentifier{}, err
		}
	}
	alias := p.mintSubqueryAlias()
	p.subqueries = append(p.subqueries, logical.ExistsSubquery{
		Alias:                  alias,
		Plan:                   innerOp,
		JoinPredicate:          p.lastJoinPredicate,
		OuterOnlyJoinConjuncts: p.lastJoinPredicateOuterOnly,
		KnownTruth:             knownTruth,
	})
	p.lastJoinPredicate = nil
	p.lastJoinPredicateOuterOnly = false
	return alias, nil
}

// correlatedExistsTruthAfterPagination classifies the cardinality effects that
// buildCorrelatedExists otherwise drops with the ignored SELECT list.
//
// A non-grouped, non-windowed aggregate produces exactly one row before
// pagination. Applying a literal LIMIT/OFFSET to that one row therefore yields
// a compile-time EXISTS truth value. For every other supported inner shape,
// LIMIT n>=1 OFFSET 0 preserves row existence and LIMIT 0 is always empty, so
// those cases are also safe. A positive OFFSET is data-dependent (notably after
// GROUP BY), and a pagination atom still unresolved at planning time is unsafe;
// both are rejected typed-loud instead of falling through to the raw-row
// semi-join. Public SQL-driver arguments are substituted before parsing and
// therefore reach this classifier as ordinary literal values.
//
// A nil truth with nil error means the fallback may proceed because the dropped
// shaping operators provably preserve existence.
func correlatedExistsTruthAfterPagination(q antlrgen.IQueryContext) (predicates.TriBool, error) {
	if q == nil {
		return nil, nil
	}
	body, ok := q.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return nil, nil
	}
	simpleTable, ok := body.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		return nil, nil
	}

	exactlyOneBeforePagination := queryInnerIsExactlyOneRowBeforePagination(q)
	limitClause := simpleTable.LimitClause()
	if limitClause == nil {
		if exactlyOneBeforePagination {
			return predicates.TriTrue, nil
		}
		return nil, nil
	}

	// parseLimitClause intentionally leaves a sentinel for an atom that is still
	// unresolved in this planner invocation. Here that sentinel is unsafe:
	// treating `LIMIT ?` as absent can change EXISTS. (The public driver
	// substitutes bound arguments before parsing, so those arrive as literals.)
	for _, atom := range limitClause.AllLimitClauseAtom() {
		if _, resolved, atomErr := resolveLimitAtom(atom); atomErr != nil {
			return nil, atomErr
		} else if !resolved {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"a correlated EXISTS with a planning-time unresolved LIMIT/OFFSET is not supported")
		}
	}
	limit, offset, limitErr := parseLimitClause(simpleTable)
	if limitErr != nil {
		return nil, limitErr
	}

	if exactlyOneBeforePagination {
		if limit == 0 || offset > 0 {
			return predicates.TriFalse, nil
		}
		return predicates.TriTrue, nil
	}
	if limit == 0 {
		return predicates.TriFalse, nil
	}
	if offset > 0 {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"a correlated EXISTS with data-dependent OFFSET is not supported")
	}
	// LIMIT n>=1 with no OFFSET preserves whether a non-aggregate/grouped inner
	// is empty, so dropping that cap from an EXISTS plan is semantics-neutral.
	return nil, nil
}

// correlatedSubqueryJoinRight builds the right child for a comma/JOIN FROM leg of
// a correlated EXISTS / scalar subquery whose inner FROM clause is rebuilt here
// (the fallback paths buildCorrelatedExists / buildCorrelatedScalar). It reuses
// the EXACT lateral-unnest classification the main FROM path uses
// (lateralUnnestCandidate over visibleFromAliases + newUnnestTableResolver): a
// `t.arr AS x [AT ord]` comma source resolves to a LogicalUnnest so the Cascades
// translator lowers it to FlatMap(Scan, Explode), instead of mis-scanning
// `t.arr` as a table name. A DERIVED-TABLE leg (`… , (SELECT …) AS d`) builds its
// body via the LogicalCTE(alias) carrier (buildDerivedInnerCarrier) — the leg twin
// of the primary-source treatment — so it is never mis-scanned as a table `d`.
// Anything else stays a plain table scan. RFC-142.
func (p *existsSubqueryPlanner) correlatedSubqueryJoinRight(j joinClause, primaryTable, primaryAlias string, priorJoins []joinClause) (logical.LogicalOperator, error) {
	resolvesToTable := newUnnestTableResolver(p.md, p.effectiveSchemaName())
	visible := visibleFromAliases(primaryTable, primaryAlias, priorJoins, resolvesToTable)
	if u := lateralUnnestCandidate(j, visible, resolvesToTable); u != nil {
		return u, nil
	}
	// A DERIVED-TABLE comma/JOIN leg is NOT a catalog table: rebuilding it as
	// NewScan(j.tableName) scans a non-existent table `d` (the executor treats it
	// as EMPTY), so a cross-product leg silently collapses to ∅ and EXISTS answers
	// wrong rows — the leg twin of the primary bug correlatedInnerPrimarySource
	// fixes. Build the derived BODY and wrap it in the same CTE carrier.
	if j.derivedQuery != nil {
		return p.buildDerivedInnerCarrier(j.derivedQuery, j.tableName)
	}
	return logical.NewScan(j.tableName, j.alias, j.segments...), nil
}

// buildDerivedInnerCarrier builds a DERIVED-TABLE inner source
// (`(SELECT …) AS alias`) for a correlated subquery whose inner FROM this fallback
// rebuilds. A derived source is NOT a catalog table: rebuilding it as
// `NewScan(alias)` scans a non-existent table `alias`, which the executor treats
// as EMPTY — so the source silently reads the wrong (empty) relation and the query
// answers wrong rows. Plan the derived BODY through the SAME catalog-aware path the
// normal SELECT uses (buildLogicalPlanForQueryBodyWithCTECatalog) and wrap it in the
// LogicalCTE(alias) carrier buildOuterPlanOnDerived installs, so the inner FROM
// carries the derived subplan and sourceAlias resolves to the derived alias. A body
// the inner builder cannot plan declines LOUDLY (correct-or-conservative) rather
// than degrading to the empty bare scan. Mirrors the cteScopes resolution the
// WHERE/ON path uses for a CTE inner (a derived source is resolved via its body, not
// a WITH registry). Shared by the PRIMARY source (correlatedInnerPrimarySource) and
// each comma/JOIN LEG (correlatedSubqueryJoinRight). Its result is USED by the EXISTS
// fast path and the WHERE/ON/scalar consumers; their matching semantic scopes
// are resolved from the same checked derived declaration, never the catalog.
func (p *existsSubqueryPlanner) buildDerivedInnerCarrier(derivedQuery antlrgen.IQueryContext, alias string) (logical.LogicalOperator, error) {
	innerOp, innerErr := buildLogicalPlanForQueryWithCTECatalog(
		derivedQuery, p.md, p.effectiveSchemaName(), p.cteScopes, p.cteOnScopes)
	if innerErr != nil {
		// A STRUCTURED planner/resolution failure of the derived BODY (e.g. an
		// undefined column → 42703) is a FAITHFUL diagnostic of the inner query, not
		// an unsupported-shape decline. Surface it VERBATIM so the SQLSTATE is the
		// SAME in every EXISTS position: mapPredicateWalkError matches
		// CorrelatedExistsError (→ 0A000) before the raw api.Error, so wrapping this
		// would rewrite the derived body's 42703 to 0A000 in a WHERE EXISTS while the
		// projected position keeps 42703. Returning the api.Error unwrapped keeps both
		// faithful without recategorizing a body diagnostic as a source lookup failure.
		var apiErr *api.Error
		if errors.As(innerErr, &apiErr) {
			return nil, apiErr
		}
		return nil, &CorrelatedExistsError{
			Message: fmt.Sprintf("correlated subquery: build derived inner %q: %v", alias, innerErr),
			Cause:   innerErr,
		}
	}
	if innerOp == nil {
		return nil, &CorrelatedExistsError{Message: fmt.Sprintf("correlated subquery: derived inner %q is out of scope", alias)}
	}
	carrier := logical.NewCTE(alias, innerOp, logical.NewScan(alias, "", alias), false)
	carrier.Binding = alias
	return carrier, nil
}

// correlatedInnerPrimarySource builds the primary FROM-source operator for a
// correlated EXISTS / scalar subquery whose inner FROM this fallback rebuilds. A
// plain table source is a bare scan; a DERIVED-TABLE source (`(SELECT …) AS d`)
// routes through buildDerivedInnerCarrier (which builds the body, never mis-scans
// `d` as a table).
func (p *existsSubqueryPlanner) correlatedInnerPrimarySource(sq *selectQuery, innerAlias string) (logical.LogicalOperator, error) {
	if sq.derivedQuery == nil {
		return logical.NewScan(sq.tableName, innerAlias, sq.sourceSegments...), nil
	}
	return p.buildDerivedInnerCarrier(sq.derivedQuery, innerAlias)
}

// correlatedNamedScopeSource resolves a FROM source through its declaration:
// derived bodies and WITH bindings carry their exact semantic publication;
// only a catalog source goes through ResolveTable. The boolean identifies that
// catalog case for the existing single-table correlation mint.
func (p *existsSubqueryPlanner) correlatedNamedScopeSource(name, alias string, derived antlrgen.IQueryContext, analyzer *semantic.Analyzer) (semantic.ScopeSource, bool, error) {
	if alias == "" {
		alias = name
	}
	if derived != nil {
		source, err := buildDerivedTableSourceWithCTEsChecked(p.md, alias, derived, p.effectiveSchemaName(), p.cteScopes)
		return source, false, err
	}
	aliasID := semantic.FromNormalized(alias)
	if source, found := p.cteScopes[strings.ToUpper(name)]; found && source.Table != nil {
		return cteSourceAs(source, aliasID, aliasID.Name()), false, nil
	}
	table, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(name, "."), false))
	if err != nil {
		return semantic.ScopeSource{}, true, err
	}
	return semantic.ScopeSource{Table: table, Alias: aliasID, CorrelationName: aliasID.Name()}, true, nil
}

// addCorrelatedJoinScopeSource registers the inner-scope source for a comma/JOIN
// FROM leg of a correlated subquery so the inner WHERE / ON resolves its columns.
// A lateral-unnest leg registers the SAME virtual Shadowing source the main path
// uses (unnestVirtualScopeSourceWithElement) — exposing the element/ordinal binding — rather
// than resolving `t.arr` as a table. A plain table leg resolves the table from
// metadata as before. Mirrors the main path's scope binding
// (unnestScopeSourceAdder / isLateralUnnestJoin). RFC-142.
func (p *existsSubqueryPlanner) addCorrelatedJoinScopeSource(innerScope *semantic.Scope, analyzer *semantic.Analyzer, j joinClause, primaryTable, primaryAlias string, priorJoins []joinClause) error {
	resolvesToTable := newUnnestTableResolver(p.md, p.effectiveSchemaName())
	visible := visibleFromAliases(primaryTable, primaryAlias, priorJoins, resolvesToTable)
	if isLateralUnnestJoin(j, visible, resolvesToTable) {
		// innerScope already holds the primary source and every prior leg, which
		// is where the unnested array column lives — so the element's declared
		// fields are typeable here and travel with the binding, as they do on the
		// SELECT scope. A fieldless element column would make `x.ek` in this
		// leg's ON / the inner WHERE decline 42703.
		element, typed := unnestElementColumn(innerScope, j)
		var elementPtr *semantic.Column
		if typed {
			elementPtr = &element
		}
		if src, ok := unnestVirtualScopeSourceWithElement(j, elementPtr); ok {
			_ = innerScope.AddSource(src)
		}
		return nil
	}
	source, _, err := p.correlatedNamedScopeSource(j.tableName, j.alias, j.derivedQuery, analyzer)
	if err != nil {
		return err
	}
	return innerScope.AddSource(source)
}

// effectiveSchemaName is the planner's active session schema, falling back to
// defaultEmbeddedSchema when unset — matching how buildLogicalPlanForQueryWithCTECatalog
// resolves p.schemaName so the unnest table resolver classifies a
// schema-qualified table source against the same schema. RFC-142.
func (p *existsSubqueryPlanner) effectiveSchemaName() string {
	if p.schemaName == "" {
		return defaultEmbeddedSchema
	}
	return p.schemaName
}

func (p *existsSubqueryPlanner) buildCorrelatedExists(q antlrgen.IQueryContext) (logical.LogicalOperator, error) {
	if q == nil {
		return nil, &CorrelatedExistsError{Message: "correlated EXISTS: nil query"}
	}
	body, ok := q.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return nil, &CorrelatedExistsError{Message: fmt.Sprintf("correlated EXISTS: unsupported query body shape %T", q.QueryExpressionBody())}
	}
	sq, err := extractFromQueryTerm(body)
	if err != nil || sq == nil {
		return nil, &CorrelatedExistsError{Message: fmt.Sprintf("correlated EXISTS: %v", err), Cause: err}
	}

	// An inner with HAVING / QUALIFY cannot ride this fallback: the rebuild
	// below carries only FROM + WHERE, so a group-eliminating filter would be
	// silently DROPPED and the semijoin would keep outer rows whose every
	// group fails HAVING — wrong rows (yamsql exists_with_aggregate:
	// `EXISTS(… GROUP BY o.customer_id HAVING SUM(o.amount) > 150)` kept a
	// customer whose group sums to 50). Java plans this shape (an existential
	// quantifier over a GroupByExpression); the port is the RFC-180 booked
	// follow-up — until then decline TYPED, never wrong rows.
	//
	// A HAVING-less GROUP BY is deliberately NOT declined: for EXISTS the
	// drop is semantics-preserving — grouping a non-empty row set yields ≥1
	// group and grouping an empty set yields none, so EXISTS(GROUP BY over S)
	// ⇔ EXISTS(S) when pagination preserves existence. BuildExists separately
	// rejects a data-dependent grouped OFFSET. A NON-grouped aggregate inner
	// continues because its exact pre-pagination cardinality is one row;
	// BuildExists applies LIMIT/OFFSET to that cardinality and the translator
	// folds the resulting TRUE/FALSE in either polarity.
	if sq.havingExpr != nil || sq.qualifyExpr != nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"correlated EXISTS over a GROUP BY / HAVING subquery is not supported")
	}

	// Strip the session-schema qualifier off a schema-qualified table source
	// (`s.PB` → `PB` when `s` is the active schema and PB exists) BEFORE building
	// the scan/join tree and resolving the join sources. The normal catalog-aware
	// SELECT path runs the same pass (buildLogicalPlanForSelectWithCTECatalog), but
	// this correlated fallback rebuilds the inner FROM clause itself and would hand
	// the raw `s.PB` straight to Analyzer.ResolveTable (which does NOT strip a
	// schema qualifier) → `table not found: S.PB`, rejecting a valid correlated
	// subquery. Java's generateAccess resolves the table first at every FROM-source
	// point; this matches it. A dotted reference whose qualifier is a prior FROM
	// alias (a genuine lateral unnest) is NOT a schema-qualified-table pair, so its
	// segments survive for the unnest classifier. RFC-142.
	normalizeSchemaQualifiedSelectSources(sq, p.effectiveSchemaName(), p.md)

	innerAlias := sq.tableAlias
	if innerAlias == "" {
		innerAlias = sq.tableName
	}

	// Resolve the leg operators + join kinds first — this needs no resolver
	// (correlatedSubqueryJoinRight classifies scan/unnest sources directly).
	rights := make([]logical.LogicalOperator, len(sq.joins))
	kinds := make([]logical.JoinKind, len(sq.joins))
	for i, j := range sq.joins {
		right, rErr := p.correlatedSubqueryJoinRight(j, sq.tableName, innerAlias, sq.joins[:i])
		if rErr != nil {
			return nil, rErr
		}
		rights[i] = right
		switch j.joinType {
		case joinTypeLeft:
			kinds[i] = logical.JoinLeft
		case joinTypeRight:
			kinds[i] = logical.JoinRight
		case joinTypeFull:
			kinds[i] = logical.JoinFull
		default:
			kinds[i] = logical.JoinInner
		}
	}

	// CTE-safe fast path: the scope+resolver below is needed ONLY to walk an ON or
	// a WHERE. A correlated fallback entered solely because the (ignored) SELECT
	// list references an outer column — no WHERE, no ON — must return the bare join
	// tree WITHOUT resolving the inner source as a catalog table: a CTE / derived
	// inner is not in the catalog, so reaching Analyzer.ResolveTable would reject a
	// valid inner ("table not found"). This restores the original pre-scope
	// position of the fast path.
	anyOn := false
	for _, j := range sq.joins {
		if j.onExpr != nil {
			anyOn = true
			break
		}
	}
	if (sq.whereExpr == nil || sq.whereExpr.Expression() == nil) && !anyOn {
		op, primErr := p.correlatedInnerPrimarySource(sq, innerAlias)
		if primErr != nil {
			return nil, primErr
		}
		for i := range sq.joins {
			op = logical.NewJoinWithPredicate(op, rights[i], kinds[i], nil)
		}
		if err := bindLateralCollections(op, sq, p.md, p.effectiveSchemaName(), p.cteScopes); err != nil {
			return nil, err
		}
		return op, nil
	}

	// There is a WHERE or an ON — build the inner scope + resolver so each explicit
	// `JOIN … ON` clause can be walked and placed correctly (the sibling
	// buildCorrelatedScalar has the same ordering). An INNER-join ON is
	// equivalent to a WHERE conjunct and is folded into the inner predicate
	// stream below; an OUTER-join ON is NOT (unmatched preserved-side rows must
	// survive), so it stays on the join node — see the join loop.
	cat := rlcatalog.Wrap(p.md)
	analyzer := semantic.NewAnalyzer(cat, false)

	outerScope := semantic.NewScope(nil)
	for _, src := range p.outerScopes {
		_ = outerScope.AddSource(src)
	}

	innerScope := semantic.NewScope(outerScope)
	innerSource, catalogSource, sourceErr := p.correlatedNamedScopeSource(sq.tableName, innerAlias, sq.derivedQuery, analyzer)
	if sourceErr != nil {
		return nil, sourceErr
	}
	aliasID := innerSource.Alias
	// Collision mint: a single catalog or derived inner is BORN under a
	// unique correlation identity, never its SQL source name. The SQL name
	// (aliasID) stays the scope-resolution qualifier — `MA.c` inside the
	// subquery still resolves against the inner source, SHADOWING a
	// same-named outer leg (Java SemanticAnalyzer.resolveAcrossFragments:
	// innermost fragment first) — but every reference the walk emits is
	// qualified under the MINTED identity, so the join predicate can never
	// carry the ambiguous SQL name. Without the mint, an inner-bound ref
	// qualified under the source name collided with a same-named outer leg
	// at the join level: the name-model rebase reinterpreted it as an
	// OUTER-leg read and the positive-polarity outer-routing pre-filtered
	// per outer row — wrong rows vs Java's inner-shadow semantics (live-
	// verified: `FROM MA, MA.arr AS X WHERE EXISTS (SELECT 1 FROM MA WHERE
	// MA.c < X)` answers ALL elements with X > min(MA.c) in Java). The
	// SIMPLE NOT-EXISTS twin was already correct — negation forbids the
	// hoist, the conjunct stayed under the ∃ and bound inner — the polarity
	// split that proved the ambiguity, not the runtime, was the defect; a
	// NESTED NOT-EXISTS composition was still wrong pre-mint and is fixed
	// by the same identity (pinned: notexists_around_nested_colliding).
	// Uppercase because
	// every consumer (sourceAlias, outerBoundAliases, splitOuterOnly-
	// conjuncts) upper-cases SQL aliases; existsInnerCorrelation's rename
	// then rebases the minted name onto esq.Alias exactly as it did the
	// source name. Multi-source and named WITH-CTE inners retain their
	// existing identities; this single-source mint does not rebind join legs.
	mintedInnerCorr := ""
	if len(sq.joins) == 0 && (catalogSource || sq.derivedQuery != nil) {
		// A QUOTED SQL alias can legally spell `"Q$N"`, so a raw mint could
		// equal a visible outer name when the global counter happens to
		// align — the outer's refs would then be captured by the inner
		// binding, with results depending on planning history. Mint until
		// distinct from every visible name (see mintDistinctUpper): the
		// inner SQL alias, every outer scope's Alias AND CorrelationName
		// (the latter covers enclosing mints and dup-alias binding ids),
		// and the CTE registry's names — an unaliased CTE leg (`FROM c`)
		// is absent from p.outerScopes (addSrc drops catalog-resolution
		// failures), so its name would otherwise escape the set. An
		// ALIASED CTE leg (`FROM c AS "Q$44"`) is dropped alias-and-all —
		// that alias is unreachable here and is the one residual gap,
		// booked with the outer-CTE-leg scope-registration fix (the same
		// family as the derived-table registration above). esq.Alias
		// values (existsInnerCorrelation's rename targets) are a distinct
		// generated namespace off the SAME strictly-increasing counter —
		// and mintSubqueryAlias skips user-visible names for them too — so
		// a mint can never equal one; no entry needed for them.
		visible := p.visibleScopeNames()
		visible[strings.ToUpper(innerAlias)] = struct{}{}
		mintedInnerCorr = mintDistinctUpper(visible, values.UniqueCorrelationIdentifier)
	}
	innerCorrName := aliasID.Name()
	if mintedInnerCorr != "" {
		innerCorrName = mintedInnerCorr
	}
	innerSource.CorrelationName = innerCorrName
	_ = innerScope.AddSource(innerSource)

	// Join sources are added to the inner scope INCREMENTALLY in the join loop
	// below — each leg registered right BEFORE its own ON is walked — so an ON at
	// join level i sees only {primary + legs[0..i]} (SQL left-to-current
	// visibility), never a LATER leg. Without this, a later leg that REUSES an
	// outer alias would capture an earlier ON's reference to that name (which must
	// bind the OUTER source), misclassifying a correlation as inner and misplacing
	// the predicate. The resolver holds innerScope by reference, so sources added
	// after construction are visible to subsequent walks; the WHERE walk (after the
	// loop) sees the FULL inner scope, which is correct — only ON visibility is
	// left-to-current.
	resolver := expr.New(analyzer, innerScope)

	// Install a SubqueryPlanner on the resolver so that nested EXISTS
	// subqueries in the inner WHERE can be planned. The nested planner's
	// outer scopes include both the current planner's outer scopes and
	// the inner table — this enables correlation across multiple levels
	// (e.g. innermost EXISTS referencing outermost emp.id).
	// The inner source SHADOWS a same-aliased outer for the next nesting
	// level (the semantics the alias-keyed map's overwrite used to encode):
	// drop same-aliased outers before appending, so a doubly-nested EXISTS
	// still resolves the nearer source first — never a same-level duplicate
	// of an outer leg with the inner table.
	nestedOuterScopes := make([]semantic.ScopeSource, 0, len(p.outerScopes)+1)
	for _, v := range p.outerScopes {
		if !v.Alias.EqualsIgnoreQuoting(aliasID) {
			nestedOuterScopes = append(nestedOuterScopes, v)
		}
	}
	// The nested scope carries the MINTED correlation (innerCorrName) so a
	// nested EXISTS's reference to THIS level's source emits the identity
	// the runtime actually binds — the minted scan alias — not the SQL name
	// (which may be an outer leg's).
	nestedOuterScopes = append(nestedOuterScopes, innerSource)
	nestedPlanner := &existsSubqueryPlanner{
		md:          p.md,
		schemaName:  p.schemaName,
		outerScope:  innerScope,
		outerScopes: nestedOuterScopes,
		cteScopes:   p.cteScopes,
		cteOnScopes: p.cteOnScopes,
	}
	resolver.SetSubqueryPlanner(nestedPlanner)

	// Build the join tree from the inner FROM clause (handles multi-table
	// EXISTS). A `t.arr AS x [AT ord]` comma source is a lateral array unnest,
	// not a table — classify it via the SAME helper the main FROM path uses so
	// the Cascades translator lowers it to FlatMap(Scan, Explode). RFC-142.
	//
	// Each explicit `JOIN … ON` is split against the inner-source universe:
	//   - INNER-INNER conjuncts (reference only inner sources, e.g. `f.fid=e.fid`)
	//     stay ON THAT JOIN'S NODE — applied at the correct join level in EVERY
	//     ordering. Folding them into one predicate below the whole inner join
	//     would misplace an INNER ON that precedes a later RIGHT/FULL join: a
	//     preserved outer-join row has NULL inner columns, so the folded ON goes
	//     NULL→false and drops a row that must keep EXISTS true.
	//   - CORRELATION conjuncts (reference the outer query, e.g. `e.eid=p.id`):
	//     an INNER join lifts them to the outer level (like a WHERE correlation);
	//     an OUTER (LEFT/RIGHT/FULL) join cannot — lifting a predicate out of an
	//     outer-join ON changes which rows are preserved — so decline cleanly.
	//
	// A conjunct is a liftable CORRELATION only if it references a REAL
	// OUTER-SCOPE source (a source in the enclosing query's scope), not merely a
	// name absent from the inner sources. This is the robustness boundary: a
	// nested subquery inside an ON binds a GENERATED alias that is neither an
	// inner source nor an outer-scope source — classifying it as "outer" would
	// lift it, and the downstream nested-EXISTS hoist would then drop the whole
	// join tree. Build the outer-scope name set here.
	outerAliases := map[string]struct{}{}
	for _, src := range p.outerScopes {
		if src.CorrelationName != "" {
			outerAliases[strings.ToUpper(src.CorrelationName)] = struct{}{}
		}
		if n := src.Alias.Name(); n != "" {
			outerAliases[strings.ToUpper(n)] = struct{}{}
		}
	}

	// The inner-source alias set is accumulated INCREMENTALLY (primary + legs seen
	// so far) so each ON's split reflects SQL left-to-current visibility: a name
	// that is only a LATER inner source is out of scope at an earlier ON — there it
	// binds an outer source with that name (a correlation) or is unresolved. A
	// conjunct is a liftable correlation only if it references an outer-scope name
	// that is NOT ALSO an (in-scope) inner source: when the outer query and the
	// inner FROM reuse the same alias, the inner source SHADOWS the outer, so a
	// reference to that name binds inner (the shadowing guard in
	// splitConjunctsByOuterRef).
	levelInnerAliases := innerSourceAliases(logical.NewScan(sq.tableName, innerAlias, sq.sourceSegments...))

	// The FULL inner-source alias set (all legs) is used to detect an outer/inner
	// alias COLLISION: an earlier ON that references an alias which is ALSO a LATER
	// inner leg. Per-join scope correctly binds that reference to the OUTER source
	// (the later leg isn't in scope yet), but the lifted correlation's QOV(name)
	// then collides with the inner leg of the same name at runtime — ambiguous,
	// silent-wrong. Such a correlation is declined below rather than mis-answered.
	fullInnerAliases := innerSourceAliases(logical.NewScan(sq.tableName, innerAlias, sq.sourceSegments...))
	for i := range sq.joins {
		for a := range innerSourceAliases(rights[i]) {
			fullInnerAliases[a] = struct{}{}
		}
	}

	// The scan itself is built under the minted identity (falling back to the
	// SQL alias for the unminted shapes) — the plan-side half of the mint:
	// sourceAlias(esq.Plan) and outerBoundAliases(esq.Plan) then report the
	// unique name, so a same-named outer leg can never alias-collide with
	// the inner at the join level.
	scanAlias := innerAlias
	if mintedInnerCorr != "" {
		scanAlias = mintedInnerCorr
	}
	op, primaryErr := p.correlatedInnerPrimarySource(sq, scanAlias)
	if primaryErr != nil {
		return nil, primaryErr
	}
	var liftedOnCorr []predicates.QueryPredicate
	for i, j := range sq.joins {
		// Register leg i's source in the inner scope BEFORE walking its ON (so the
		// ON sees {primary + legs[0..i]}), and accumulate its aliases into the
		// per-level inner set used by the split. A lateral-unnest leg registers the
		// same virtual Shadowing source the main path uses (exposing the
		// element/ordinal binding) instead of resolving `t.arr` as a table. RFC-142.
		if jErr := p.addCorrelatedJoinScopeSource(innerScope, analyzer, j, sq.tableName, innerAlias, sq.joins[:i]); jErr != nil {
			return nil, &CorrelatedExistsError{Message: fmt.Sprintf("correlated EXISTS: resolve join table %q: %v", j.tableName, jErr), Cause: jErr}
		}
		for a := range innerSourceAliases(rights[i]) {
			levelInnerAliases[a] = struct{}{}
		}
		var nodeOn predicates.QueryPredicate
		if j.onExpr != nil {
			subqBefore := len(nestedPlanner.subqueries) + len(nestedPlanner.scalarSubqueries) + len(nestedPlanner.correlatedScalarSubqueries)
			walkedOn, onErr := resolver.WalkPredicate(j.onExpr)
			if onErr != nil {
				// A nested subquery/EXISTS inside the ON is an unsupported shape
				// (declined below via onAddedSubquery). But the walk can FAIL first —
				// e.g. `ON EXISTS (SELECT 1 FROM h WHERE h.hid = f.fid)` where the
				// nested subquery references the CURRENT leg `f`, which the nested
				// planner's scope does not expose. Surface that as the deliberate
				// 0A000 decline (Unsupported) rather than a raw 42703 resolution
				// failure in the WHERE-EXISTS path.
				if expr.ContainsSubqueryAtom(j.onExpr) || expr.ContainsExistsAtom(j.onExpr) {
					return nil, &CorrelatedExistsError{Message: "correlated EXISTS: a nested subquery inside a JOIN ON clause is not supported", Unsupported: true}
				}
				return nil, wrapCorrelatedExistsWalkErr(fmt.Sprintf("correlated EXISTS: walk ON clause: %v", onErr), onErr)
			}
			// An ON that itself contains a nested EXISTS/scalar subquery cannot be
			// handled by this fallback: lifting it to the outer level misclassifies
			// the generated subquery alias as a correlation (and the downstream
			// nested-EXISTS hoist would drop the join tree), while keeping it on the
			// join node orphans the nested subquery's PLAN (the join node carries no
			// ExistsSubqueries slot, so the nested EXISTS evaluates as a dead
			// always-false predicate). Neither placement is correct, so decline
			// cleanly (correct-or-conservative) rather than answer wrong rows.
			onAddedSubquery := len(nestedPlanner.subqueries)+len(nestedPlanner.scalarSubqueries)+len(nestedPlanner.correlatedScalarSubqueries) > subqBefore
			if onAddedSubquery {
				return nil, &CorrelatedExistsError{Message: "correlated EXISTS: a nested subquery inside a JOIN ON clause is not supported", Unsupported: true}
			}
			// A subquery-free ON is split into a liftable correlation (references a
			// real outer-scope source that is not shadowed by an inner source) and
			// the inner-inner part (stays on the node).
			correlation, innerInner := splitConjunctsByOuterRef(walkedOn, outerAliases, levelInnerAliases)
			nodeOn = innerInner
			if correlation != nil {
				if kinds[i] != logical.JoinInner {
					return nil, &CorrelatedExistsError{Message: "correlated EXISTS: a correlation inside an OUTER (LEFT/RIGHT/FULL) JOIN ON clause is not supported", Unsupported: true}
				}
				// Outer/inner alias collision: the correlation references an outer
				// name that is ALSO a (later) inner leg — the same name bound in two
				// scopes. Per-join scope bound it to the outer here, but lifting the
				// correlation makes its QOV(name) collide with the inner leg at runtime
				// (ambiguous). Decline (correct-or-conservative) rather than silent-wrong.
				for name := range predicates.GetCorrelatedToOfPredicate(correlation) {
					n := strings.ToUpper(name.Name())
					_, isOuter := outerAliases[n]
					_, isFullInner := fullInnerAliases[n]
					if isOuter && isFullInner {
						return nil, &CorrelatedExistsError{Message: "correlated EXISTS: a JOIN ON references an alias reused as a later inner join source (outer/inner alias collision) is not supported", Unsupported: true}
					}
				}
				// Lifting a correlated INNER-join ON to the outer level applies it
				// AFTER the whole inner plan (like a WHERE correlation). That loses
				// the ON's join-level placement: if a LATER join is RIGHT or FULL, it
				// preserves g-side rows with NULL on this join's e/f columns, and the
				// lifted `e.eid=p.id` then evaluates NULL→false and rejects those
				// preserved rows — EXISTS wrongly false. (A later LEFT/INNER join does
				// not preserve NULL-e rows, so the lift is safe.) Reproducing the
				// correct join-level placement is not something this fallback can do,
				// so decline cleanly rather than answer wrong rows.
				laterOuterPreservesOtherSide := false
				for k := i + 1; k < len(kinds); k++ {
					if kinds[k] == logical.JoinRight || kinds[k] == logical.JoinFull {
						laterOuterPreservesOtherSide = true
						break
					}
				}
				if laterOuterPreservesOtherSide {
					return nil, &CorrelatedExistsError{Message: "correlated EXISTS: a correlation inside a JOIN ON clause before a later RIGHT/FULL JOIN is not supported", Unsupported: true}
				}
				liftedOnCorr = append(liftedOnCorr, correlation)
			}
		}
		op = logical.NewJoinWithPredicate(op, rights[i], kinds[i], nodeOn)
	}

	if err := bindLateralCollections(op, sq, p.md, p.effectiveSchemaName(), p.cteScopes); err != nil {
		return nil, err
	}

	// With inner-inner ON conjuncts on their join nodes, only an INNER-join ON's
	// correlation or the WHERE still needs a filter. A `SELECT 1 FROM e JOIN f ON
	// f.fid=e.fid AND e.eid=p.id` inner with NO WHERE must NOT early-return the
	// bare join: that would drop the lifted correlation and make EXISTS silently
	// true over an empty inner join.
	if (sq.whereExpr == nil || sq.whereExpr.Expression() == nil) && len(liftedOnCorr) == 0 {
		return op, nil
	}

	var pred predicates.QueryPredicate
	if sq.whereExpr != nil && sq.whereExpr.Expression() != nil {
		var walkErr error
		pred, walkErr = resolver.WalkPredicate(sq.whereExpr.Expression())
		if walkErr != nil {
			return nil, wrapCorrelatedExistsWalkErr(fmt.Sprintf("correlated EXISTS: walk predicate: %v", walkErr), walkErr)
		}
	}

	// Lift each INNER-join ON's correlation conjuncts to the outer level, routed
	// by the same qualify + splitOuterOnlyConjuncts machinery as the WHERE.
	for _, onCorr := range liftedOnCorr {
		if pred == nil {
			pred = onCorr
		} else {
			pred = predicates.NewAnd(pred, onCorr)
		}
	}
	if pred == nil {
		return op, nil
	}

	// MULTI-SOURCE scope-ambiguity decline (correct-or-loud): an UNMINTED
	// multi-source inner keeps its SQL leg names, so a predicate ref to a
	// leg that REUSES an outer bound name is ambiguous at the join level —
	// the walk bound it INNER (SQL shadowing), but the name-model runtime
	// routes such refs by name against the merged outer row (per-outer-row
	// reads; Java's inner-shadow semantics answer differently — live-
	// verified). Decline LOUDLY rather than answer wrong rows; mint-per-leg
	// (booked) closes the reach gap for real. Placement: BEFORE the
	// nested-EXISTS branches — Case 1 assigns this predicate's non-EXISTS
	// part as the join predicate and would otherwise carry the ambiguous
	// ref out unchecked (a nested constant-true EXISTS must not disable
	// the guard); Case 2's hoist is covered by the nested planner running
	// this same check recursively for its own scope. Checking the full
	// walked predicate here is a SAFE SUPERSET of the old tail check
	// (rest ⊂ pred): an outer-only conjunct cannot carry an intersection
	// name (an intersection name IS an inner alias, so the split keeps it
	// in rest), and EXISTS/scalar markers carry only generated aliases —
	// which cannot equal user names BECAUSE of the mint law (the 3a skip
	// makes every generated identity distinct from user-visible names;
	// that law is load-bearing for this argument). The check runs on a
	// SIMPLIFIED COPY: constant folding can eliminate a ref entirely
	// (`COALESCE(1, a.id) = 1` never reads a), and the join predicate
	// that actually rides out is the simplified form — declining on a
	// foldable ref would 0A000 valid queries the tail-era check accepted.
	// Parent-fallthrough refs cannot false-positive: in a multi-source
	// scope a shadowed parent hit already dies at plan time
	// (CorrelatedShadowError, 42703), so a surviving ref carrying an
	// intersection name is inner-bound by construction. Minted single-
	// table inners never enter (their inner refs are Q$N).
	if len(sq.joins) > 0 {
		if n := scopeAmbiguousName(predicates.SimplifyPredicateValues(pred), fullInnerAliases, p.outerScopes); n != "" {
			return nil, &CorrelatedExistsError{Message: fmt.Sprintf("correlated EXISTS: inner FROM source %q reuses an outer FROM name referenced by the subquery predicate (scope-ambiguous)", n), Unsupported: true}
		}
	}

	// Propagate SCALAR subquery plans the nested planner collected while walking
	// the inner WHERE (`… EXISTS (SELECT 1 FROM c WHERE p.id > (SELECT MIN(id)
	// FROM c2))`). The walked predicate references the scalar's ALIAS; without
	// the plan the executor never pre-evaluates it, the alias binding stays
	// unset, and the comparison is silently NULL → every outer row dropped.
	// Bubbling them into THIS planner routes them to the enclosing filter/
	// projection exactly like a top-level scalar subquery (an uncorrelated
	// scalar is a query-constant external binding — its evaluation point is
	// scope-free). Correlated scalars propagate the same way; their per-row
	// evaluation would need per-row re-execution (below).
	p.scalarSubqueries = append(p.scalarSubqueries, nestedPlanner.scalarSubqueries...)
	// A CORRELATED scalar inside an EXISTS WHERE has NO evaluation path: the
	// one-shot pre-eval cannot re-run it per row, and the WHERE channel has no
	// CorrelatedScalarSubquery consumer (only projections and HAVING do).
	// Dropping it silently NULLed the comparison and returned zero rows for
	// every outer row; decline LOUDLY instead (CORRECT-or-LOUD — the per-row
	// evaluation is tracked follow-on work with the EXISTS wrong-rows batch).
	if len(nestedPlanner.correlatedScalarSubqueries) > 0 {
		return nil, &CorrelatedExistsError{Message: "correlated EXISTS: a correlated scalar subquery inside an EXISTS WHERE clause is not supported"}
	}

	// If the nested planner collected EXISTS subqueries, check whether
	// the middle level has its own correlation predicate (non-EXISTS).
	if len(nestedPlanner.subqueries) > 0 {
		innerCorr := strings.ToUpper(innerCorrName)
		nonExistsPred := splitNonExistsPredicatesFromWalked(pred)

		if nonExistsPred != nil {
			// Case 1: middle has BOTH correlation + nested EXISTS.
			// Build a proper LogicalFilter preserving the middle level.
			existsPred := stripNonExistsPredicates(pred)
			qualifyBareFields(nonExistsPred, innerCorr)
			simplified := predicates.SimplifyPredicateValues(nonExistsPred)
			// A NON-INNER conjunct here — an outer-only correlation OR a
			// reference-free constant/parameter, i.e. anything the
			// existential rule routes to the OUTER input — CANNOT take the
			// tail path's inside placement: a nested-EXISTS-carrying filter
			// with an extra plain conjunct (or an inner filter layer) does
			// not plan for this composition (the booked multi-EXISTS
			// best-expression family; both placements were tried and die at
			// physical planning). It rides lastJoinPredicate, which the
			// semi-join outer-routes — VALID for positive polarity
			// (P ∧ ∃(Q) ≡ ∃(P∧Q)) and WRONG under NOT EXISTS (computes
			// P ∧ ¬∃(Q), silently dropping every ¬P outer row — the
			// pre-existing leak this branch shipped with). The esq is
			// FLAGGED so the anti-join consumer declines LOUDLY; positive
			// polarity keeps its valid outer-routing unchanged. The flag
			// test is deliberately BROADER than splitOuterOnlyConjuncts:
			// that split keeps reference-free conjuncts (`1 = 0`, a
			// parameter) in rest, yet they outer-route all the same and
			// carry the identical polarity hazard.
			p.lastJoinPredicate = simplified
			p.lastJoinPredicateOuterOnly = hasNonInnerConjunct(simplified, innerSourceAliases(op))
			filter := &logical.LogicalFilter{
				Input:            op,
				Predicate:        existsPred,
				ExistsSubqueries: nestedPlanner.subqueries,
			}
			return filter, nil
		}

		// Case 2: middle has ONLY EXISTS (no own correlation).
		// The inner correlation spans multiple levels (innermost →
		// outermost). Hoist the inner plan to this level so the
		// correlation binds against the outer row directly.
		innerESQ := nestedPlanner.subqueries[0]
		p.lastJoinPredicate = innerESQ.JoinPredicate
		p.lastJoinPredicateOuterOnly = innerESQ.OuterOnlyJoinConjuncts
		return innerESQ.Plan, nil
	}

	// The predicate will be evaluated in a merged NLJ context where both
	// inner and outer columns coexist keyed by UPPER-CASE qualified names
	// (e.g. SUB.V, A.V). The resolver produced bare field names for inner
	// columns (e.g. "V") because the inner scope has only one source.
	// Qualify them with the inner correlation name — the MINTED identity
	// when the mint applies — so that merged-row lookup finds the inner
	// column, not the outer's value leaking through when the inner row has
	// a NULL (absent-from-map) field.
	innerCorr := strings.ToUpper(innerCorrName)
	qualifyBareFields(pred, innerCorr)
	pred = predicates.SimplifyPredicateValues(pred)

	// OUTER-ONLY conjuncts (`… WHERE p.id = 1` — no inner-source reference) stay
	// INSIDE the subquery as a filter on the inner plan, so they evaluate UNDER
	// the ∃ in both polarities: ¬∃(P∧Q) ≡ ¬P ∨ ¬∃(Q). Threading them through the
	// join predicate instead hands them to the semi-join implementation's
	// inner/outer routing, which pre-filters the OUTER on outer-only conjuncts —
	// an equivalence that holds ONLY for the positive polarity (P ∧ ∃(Q) ≡
	// ∃(P∧Q)); under NOT EXISTS it computes P ∧ ¬∃(Q) and wrongly drops every
	// ¬P outer row. Placement, not polarity, is the invariant: subquery-origin
	// conjuncts never leave the subquery. That INCLUDES conjuncts referencing a
	// SCALAR-subquery alias: the pre-evaluated binding lives in the root
	// evaluation context and IS visible below the FirstOrDefault (the filter
	// contexts thread it) — the RFC-141 R4 outer-routing rationale concerns
	// SIBLING predicates outside the ∃ (which must not be skipped when the
	// inner is empty), never subquery-internal conjuncts. Routing a
	// scalar-referencing internal conjunct outward reproduced the pre-filter
	// polarity bug for exactly the NOT-EXISTS + scalar shape.
	// The inner-source universe comes from the BINDER-EXACT collector over the
	// built op tree (the same helper the correlated-scalar scope discriminator
	// uses, pinned by TestInnerSourceAliases_MirrorsUnnestBinder) — one
	// inner-source authority, not a second joins-walk.
	outerOnly, rest := splitOuterOnlyConjuncts(pred, innerSourceAliases(op))
	if outerOnly != nil {
		op = &logical.LogicalFilter{Input: op, Predicate: outerOnly}
	}
	// The multi-source scope-ambiguity decline already ran on the FULL
	// walked predicate above (before the nested-EXISTS branches) — rest is
	// a subset of it, so no second check is needed here.
	p.lastJoinPredicate = rest
	return op, nil
}

// splitOuterOnlyConjuncts partitions a subquery WHERE's top-level AND tree into
// (outerOnly, rest): a conjunct is OUTER-ONLY iff it references at least one
// correlation and none of them is an inner FROM source. A scalar-subquery
// alias counts as OUTER here — the pre-evaluated binding lives in the root
// evaluation context and is visible under the ∃, and the placement invariant
// (subquery conjuncts evaluate under the ∃, both polarities) applies to it
// like any other outer-only conjunct. Inner-only conjuncts, genuine
// correlation conjuncts, and reference-free constants stay in rest (the join
// predicate), preserving the existing routing. An OR tree is one conjunct,
// classified atomically by its whole correlation set.
func splitOuterOnlyConjuncts(pred predicates.QueryPredicate, innerAliases map[string]struct{}) (outerOnly, rest predicates.QueryPredicate) {
	if pred == nil {
		return nil, nil
	}
	var outer, keep []predicates.QueryPredicate
	var walk func(p predicates.QueryPredicate)
	walk = func(p predicates.QueryPredicate) {
		if and, ok := p.(*predicates.AndPredicate); ok {
			for _, sub := range and.SubPredicates {
				walk(sub)
			}
			return
		}
		corrs := predicates.GetCorrelatedToOfPredicate(p)
		if len(corrs) == 0 {
			keep = append(keep, p)
			return
		}
		for c := range corrs {
			name := strings.ToUpper(c.Name())
			if _, isInner := innerAliases[name]; isInner {
				keep = append(keep, p)
				return
			}
		}
		outer = append(outer, p)
	}
	walk(pred)
	andOf := func(ps []predicates.QueryPredicate) predicates.QueryPredicate {
		switch len(ps) {
		case 0:
			return nil
		case 1:
			return ps[0]
		default:
			return predicates.NewAnd(ps...)
		}
	}
	return andOf(outer), andOf(keep)
}

// splitConjunctsByOuterRef partitions a walked ON's top-level AND tree into
// (refsOuter, innerOnly): a conjunct is refsOuter iff it references at least one
// correlation that is a REAL OUTER-SCOPE source (name in outerAliases) AND is NOT
// ALSO an inner source (name absent from innerAliases). Every other conjunct —
// referencing only inner sources, a generated (non-outer-scope) alias, an
// outer-scope name SHADOWED by a same-named inner source, or no correlation at
// all — is innerOnly. Two robustness boundaries live in this test:
//   - Membership in the outer-scope set (not mere ABSENCE from inner sources)
//     keeps a generated nested-subquery alias — which is neither outer-scope nor
//     inner — off the lift path.
//   - The `!isInner` guard handles ALIAS SHADOWING: when the outer query and the
//     inner FROM reuse the same alias (`c`), an inner reference `c.col` binds to
//     the inner source (inner shadows outer in the inner scope), so it must not be
//     misclassified as an outer correlation and over-decline a valid inner-only ON.
//
// This isolates a genuine correlation conjunct like `e.eid = p.id` (inner e +
// unshadowed outer p) into refsOuter so an INNER join's ON correlation can be
// lifted to the outer level while its inner-inner conjuncts stay on the join node.
// Either return may be nil.
func splitConjunctsByOuterRef(pred predicates.QueryPredicate, outerAliases, innerAliases map[string]struct{}) (refsOuter, innerOnly predicates.QueryPredicate) {
	if pred == nil {
		return nil, nil
	}
	var outer, inner []predicates.QueryPredicate
	var walk func(p predicates.QueryPredicate)
	walk = func(p predicates.QueryPredicate) {
		if and, ok := p.(*predicates.AndPredicate); ok {
			for _, sub := range and.SubPredicates {
				walk(sub)
			}
			return
		}
		hasOuter := false
		for c := range predicates.GetCorrelatedToOfPredicate(p) {
			name := strings.ToUpper(c.Name())
			_, isOuter := outerAliases[name]
			_, isInner := innerAliases[name]
			if isOuter && !isInner {
				hasOuter = true
				break
			}
		}
		if hasOuter {
			outer = append(outer, p)
		} else {
			inner = append(inner, p)
		}
	}
	walk(pred)
	andOf := func(ps []predicates.QueryPredicate) predicates.QueryPredicate {
		switch len(ps) {
		case 0:
			return nil
		case 1:
			return ps[0]
		default:
			return predicates.NewAnd(ps...)
		}
	}
	return andOf(outer), andOf(inner)
}

// mintDistinctIdentifier mints a fresh CorrelationIdentifier whose
// UPPER-CASED name is DISTINCT from every name in visible. A quoted SQL
// alias can legally spell `"Q$N"`, so a raw `UniqueCorrelationIdentifier`
// could equal a user-visible name whenever the process-global counter
// happens to align — capturing that name's references (the inner-
// correlation mint) or colliding a subquery binding with a user alias at
// the translator (esq/scalar Alias — observed as a loud planner failure
// on a valid query). The retry loop makes the outcome history-
// INDEPENDENT: a colliding candidate is skipped (the counter advances),
// and any non-colliding candidate yields identical semantics regardless
// of its numeric suffix. Terminates because visible is finite and the
// counter is strictly increasing. next is injected for deterministic
// unit testing; production passes values.UniqueCorrelationIdentifier.
func mintDistinctIdentifier(visible map[string]struct{}, next func() values.CorrelationIdentifier) values.CorrelationIdentifier {
	for {
		candidate := next()
		if _, taken := visible[strings.ToUpper(candidate.Name())]; !taken {
			return candidate
		}
	}
}

// mintDistinctUpper is mintDistinctIdentifier's upper-cased-name form —
// the inner-correlation mint consumes the NAME (scope CorrelationName,
// scan alias, qualifyBareFields), which every consumer upper-cases.
func mintDistinctUpper(visible map[string]struct{}, next func() values.CorrelationIdentifier) string {
	return strings.ToUpper(mintDistinctIdentifier(visible, next).Name())
}

// hasNonInnerConjunct reports whether any top-level conjunct of pred fails
// to reference an inner FROM source — the class the existential rule routes
// to the OUTER input: correlated outer-only conjuncts AND reference-free
// ones (constants, parameters). Used for the Case-1 polarity flag;
// splitOuterOnlyConjuncts alone under-covers it because that split
// deliberately keeps reference-free conjuncts in rest.
func hasNonInnerConjunct(pred predicates.QueryPredicate, innerAliases map[string]struct{}) bool {
	if pred == nil {
		return false
	}
	if and, ok := pred.(*predicates.AndPredicate); ok {
		for _, sub := range and.SubPredicates {
			if hasNonInnerConjunct(sub, innerAliases) {
				return true
			}
		}
		return false
	}
	for c := range predicates.GetCorrelatedToOfPredicate(pred) {
		if _, isInner := innerAliases[strings.ToUpper(c.Name())]; isInner {
			return false
		}
	}
	// A non-inner leaf is hazardous only if it can actually FILTER: a
	// statically-TRUE conjunct (`1 = 1`) outer-routes as a no-op, so
	// flagging it would over-decline semantics-neutral tautologies that
	// planned correctly before the guard. A statically-FALSE or
	// non-static leaf stays flagged — a routed FALSE drops every outer
	// row, the exact hazard. Static means BOTH comparison sides are
	// row-context-independent (IsConstantValue), so Eval with a nil
	// context is safe and deterministic.
	if cp, ok := pred.(*predicates.ComparisonPredicate); ok &&
		cp.Operand != nil && values.IsConstantValue(cp.Operand) &&
		(cp.Comparison.Operand == nil || values.IsConstantValue(cp.Comparison.Operand)) {
		if tv, err := cp.Eval(nil); err == nil && tv == predicates.TriTrue {
			return false
		}
	}
	return true
}

// scopeAmbiguousName returns the first correlation name in pred that is BOTH
// an inner leg name AND an ACTUALLY-BOUND outer name, or "" when none — the
// multi-source scope-ambiguity test (see the decline site in
// buildCorrelatedExists). The outer set is the RUNTIME-BOUND name per source
// — CorrelationName when present, else Alias — deliberately NOT the display
// set the ON-split's outerAliases uses: a minted middle carries
// {Alias: MID, CorrelationName: Q$N} and only Q$N binds at runtime, so an
// innermost leg re-declaring MID cannot collide with it; testing display
// names 0A000'd valid queries. Do not "unify" this with outerAliases — the
// two sets answer different questions (walk-time reference matching vs
// runtime binding collision).
func scopeAmbiguousName(pred predicates.QueryPredicate, innerLegNames map[string]struct{}, outerScopes []semantic.ScopeSource) string {
	if pred == nil || len(innerLegNames) == 0 {
		return ""
	}
	outerBound := map[string]struct{}{}
	for _, src := range outerScopes {
		n := src.CorrelationName
		if n == "" {
			n = src.Alias.Name()
		}
		if n != "" {
			outerBound[strings.ToUpper(n)] = struct{}{}
		}
	}
	for c := range predicates.GetCorrelatedToOfPredicate(pred) {
		n := strings.ToUpper(c.Name())
		_, isInner := innerLegNames[n]
		_, isBoundOuter := outerBound[n]
		if isInner && isBoundOuter {
			return n
		}
	}
	return ""
}

// qualifyBareFields walks a predicate tree and prepends qualifier+"."
// to every FieldValue whose Field has no dot (i.e. was unqualified by
// the resolver because the inner scope had only one source). This is
// necessary for correlated EXISTS predicates that will be evaluated in
// a merged NLJ row where both outer and inner columns coexist.
func qualifyBareFields(p predicates.QueryPredicate, qualifier string) {
	if p == nil || qualifier == "" {
		return
	}
	predicates.WalkPredicate(p, func(qp predicates.QueryPredicate) bool {
		switch pred := qp.(type) {
		case *predicates.ComparisonPredicate:
			qualifyBareFieldValue(pred.Operand, qualifier)
			if pred.Comparison.Operand != nil {
				qualifyBareFieldValue(pred.Comparison.Operand, qualifier)
			}
		case *predicates.ValuePredicate:
			qualifyBareFieldValue(pred.Value, qualifier)
		}
		return true
	})
}

// qualifyBareFieldValue mutates FieldValue nodes in place, setting
// Child to a QOV. Safe because buildCorrelatedExists constructs a
// fresh predicate tree via resolver.WalkPredicate for each call —
// these FieldValues are never shared or memoized.
func qualifyBareFieldValue(v values.Value, qualifier string) {
	// Exact resolver FieldValues already carry the QOV that owns their source
	// row. There is no legal childless node to qualify after the fact, and the
	// immutable value graph must not be mutated. Retain the call boundary while
	// the surrounding correlated-EXISTS plumbing is simplified.
	_, _ = v, qualifier
}

// resolverScope preserves the actual lexical parent rather than flattening its
// sources into a new root; identical aliases at different levels stay distinct.
func resolverScope(resolver *expr.Resolver) *semantic.Scope {
	if resolver == nil {
		return nil
	}
	return resolver.Scope()
}

func (p *existsSubqueryPlanner) BuildScalar(q antlrgen.IQueryContext) (values.CorrelationIdentifier, values.Type, error) {
	if q == nil {
		return values.CorrelationIdentifier{}, values.UnknownType, fmt.Errorf("scalar subquery: nil query context")
	}
	parent := p.outerScope
	if parent == nil && len(p.outerScopes) != 0 {
		// Programmatic planners can supply one explicit outer frame. SQL
		// producers retain their actual Scope, including all ancestor frames.
		parent = semantic.NewScope(nil)
		for _, source := range p.outerScopes {
			if err := parent.AddSource(source); err != nil {
				return values.CorrelationIdentifier{}, values.UnknownType, err
			}
		}
	}
	visitor := NewPlanVisitorWithSchema(p.md, p.schemaName)
	visitor.enclosingScope = parent
	visitor.cteScopes = maps.Clone(p.cteScopes)
	visitor.cteOnScopes = maps.Clone(p.cteOnScopes)
	visitor.cteBodies = maps.Clone(p.cteBodies)
	innerOp, err := visitor.VisitQuery(q)
	if err != nil {
		return values.CorrelationIdentifier{}, values.UnknownType, err
	}
	var localInnerAliases map[string]struct{}
	if innerOp != nil {
		// Capture the scalar query's own FROM names before outer CTE definitions
		// are wrapped around it; names inside those definitions are not local
		// declarations of the scalar Main.
		localInnerAliases = innerSourceAliases(innerOp)
		// Resolve the scalar Main's FROM paths before wrapping outer CTE
		// definitions around it. Running the mutation passes over the wrapper
		// rewrites names inside those definitions as if they belonged to this
		// query block, which strands genuine outer correlations in nested CTEs.
		schemaName := p.effectiveSchemaName()
		if err := demoteSchemaQualifiedUnnest(innerOp, schemaName, p.md); err != nil {
			return values.CorrelationIdentifier{}, values.UnknownType, err
		}
		if err := rejectAtOrdinalityOnTable(innerOp, p.md); err != nil {
			return values.CorrelationIdentifier{}, values.UnknownType, err
		}
		if err := resolveQualifiedTableNames(innerOp, schemaName); err != nil {
			return values.CorrelationIdentifier{}, values.UnknownType, err
		}
		innerOp = p.wrapWithOuterCTEs(innerOp)
		// Validation runs after wrapping so a scan of an outer CTE is known to
		// be a CTE, while a genuinely missing scalar source still carries 42F01
		// back through DML instead of becoming a generic translation failure.
		if err := validateTablesAndColumns(innerOp, p.md); err != nil {
			return values.CorrelationIdentifier{}, values.UnknownType, err
		}
	}
	if innerOp == nil {
		return values.CorrelationIdentifier{}, values.UnknownType, api.NewError(api.ErrCodeUnsupportedQuery, "scalar query has no logical plan")
	}
	if err = validateScalarSubqueryOutputArity(innerOp, p.md); err != nil {
		return values.CorrelationIdentifier{}, values.UnknownType, err
	}
	_, err = scalarSubqueryOutputTypeChecked(innerOp)
	if err != nil {
		return values.CorrelationIdentifier{}, values.UnknownType, err
	}
	exact, err := query.ExactLogicalResultType(innerOp, p.md)
	if err != nil {
		return values.CorrelationIdentifier{}, values.UnknownType, err
	}
	row, ok := exact.(*values.RecordType)
	if !ok || len(row.Fields) != 1 {
		return values.CorrelationIdentifier{}, values.UnknownType, api.NewError(api.ErrCodeSyntaxError, "scalar subquery must return exactly one column")
	}
	// Empty input produces SQL NULL even for a selected NOT NULL field.
	outputType := values.WithNullability(row.Fields[0].FieldType, true)
	// Use the existing relational correlation property: a successful lookup
	// or an earlier undefined-column error is not a correlation certificate.
	ref, _, err := query.TranslateToCascadesWithError(innerOp, p.md)
	if err != nil {
		return values.CorrelationIdentifier{}, values.UnknownType, err
	}
	if ref == nil {
		return values.CorrelationIdentifier{}, values.UnknownType, api.NewError(api.ErrCodeUnsupportedQuery, "scalar query has no relational expression")
	}
	correlated := false
	innerAliases := localInnerAliases
	for frame := parent; frame != nil; frame = frame.Parent() {
		for _, source := range frame.Sources() {
			name := source.CorrelationName
			if name == "" {
				name = source.Alias.Name()
			}
			for correlation := range ref.GetCorrelatedTo() {
				correlationName := strings.ToUpper(correlation.Name())
				if _, declaredInside := innerAliases[correlationName]; declaredInside {
					// A CTE/source visible in the parent can also be a FROM source
					// of the scalar query itself. Its correlation is local and
					// shadows the parent; treating it as external routes an
					// uncorrelated scalar through the outer ordinal seed.
					continue
				}
				if strings.EqualFold(name, correlation.Name()) {
					correlated = true
				}
			}
		}
	}
	// A wrapped outer CTE contributes its definition's correlations to the
	// translated Reference even though those names are local to the definition,
	// not correlations of the scalar Main. Require a direct parse-tree reference
	// that the enclosing lexical scope can actually resolve before routing the
	// scalar through the per-outer-row ordinal seed.
	if correlated && !p.subqueryReferencesOuterColumn(q) {
		correlated = false
	}
	alias := p.mintSubqueryAlias()
	if correlated {
		maximum := properties.ProvenCardinalitiesOf(ref.Get()).GetMaxCardinality()
		p.correlatedScalarSubqueries = append(p.correlatedScalarSubqueries, logical.CorrelatedScalarSubquery{
			Alias: alias, InnerPlan: innerOp, InnerAlias: p.mintSubqueryAlias().Name(), ScalarCol: row.Fields[0].Name,
			// The complete query retains written pagination. The existing
			// scalar barrier checks the resulting stream, never its input.
			StrictSingle: maximum.IsUnknown() || maximum.Value() > 1,
		})
	} else {
		p.scalarSubqueries = append(p.scalarSubqueries, logical.ScalarSubquery{Alias: alias, Plan: innerOp})
	}
	return alias, outputType, nil
}

// validateScalarSubqueryOutputArity enforces the scalar query's SQL output
// contract before its value is admitted into an enclosing exact projection.
// In particular, a two-column inner projection must report the semantic 42601
// rather than first becoming an UNKNOWN ScalarSubqueryValue and failing the
// outer projection's exact-type constructor with a generic 0AF00.
func validateScalarSubqueryOutputArity(op logical.LogicalOperator, md *recordlayer.RecordMetaData) error {
	if exact, exactErr := query.ExactLogicalResultType(op, md); exactErr == nil {
		if record, ok := exact.(*values.RecordType); ok && len(record.Fields) != 1 {
			return api.NewErrorf(api.ErrCodeSyntaxError,
				"scalar subquery must return exactly one column, got %d", len(record.Fields))
		}
		return nil
	}
	// Exact type derivation may itself be unavailable for a deliberately
	// underivable value. The logical output projection still states its arity,
	// so retain the semantic check without treating a failed type derivation as
	// permission to manufacture a type.
	if width, ok := scalarSubqueryStructuralOutputArity(op); ok && width != 1 {
		return api.NewErrorf(api.ErrCodeSyntaxError,
			"scalar subquery must return exactly one column, got %d", width)
	}
	return nil
}

func scalarSubqueryStructuralOutputArity(op logical.LogicalOperator) (int, bool) {
	switch typed := op.(type) {
	case *logical.LogicalFilter:
		return scalarSubqueryStructuralOutputArity(typed.Input)
	case *logical.LogicalSort:
		return scalarSubqueryStructuralOutputArity(typed.Input)
	case *logical.LogicalLimit:
		return scalarSubqueryStructuralOutputArity(typed.Input)
	case *logical.LogicalDistinct:
		return scalarSubqueryStructuralOutputArity(typed.Input)
	case *logical.LogicalCTE:
		return scalarSubqueryStructuralOutputArity(typed.Main)
	case *logical.LogicalProject:
		return len(typed.Projections), true
	case *logical.LogicalAggregate:
		return len(typed.GroupKeys) + len(typed.Calls), true
	case *logical.LogicalUnion:
		if len(typed.Inputs) > 0 {
			return scalarSubqueryStructuralOutputArity(typed.Inputs[0])
		}
	}
	return 0, false
}

// scalarSubqueryOutputTypeChecked derives the same exact scalar type while
// preserving semantic validation precedence for a known-invalid aggregate
// operand. The aggregate translator has the identical numeric-only gate, but
// scalar plans are translated after their enclosing plan; without this early
// check, UNKNOWN reaches the enclosing exact projection first and masks the
// intended 0A000 with 0AF00.
func scalarSubqueryOutputTypeChecked(op logical.LogicalOperator) (values.Type, error) {
	switch o := op.(type) {
	case *logical.LogicalLimit:
		return scalarSubqueryOutputTypeChecked(o.Input)
	case *logical.LogicalSort:
		return scalarSubqueryOutputTypeChecked(o.Input)
	case *logical.LogicalFilter:
		return scalarSubqueryOutputTypeChecked(o.Input)
	case *logical.LogicalDistinct:
		return scalarSubqueryOutputTypeChecked(o.Input)
	case *logical.LogicalCTE:
		// wrapWithOuterCTEs places the scalar query in Main and carries the
		// referenced CTE definition in Body.  The scalar's output contract is
		// therefore the Main result, not the wrapper node itself.  Omitting
		// this transparent arm discarded the exact aggregate type for
		// `(WITH ... SELECT MIN(...) FROM cte)` and minted an UNKNOWN
		// ScalarSubqueryValue in the enclosing projection.
		return scalarSubqueryOutputTypeChecked(o.Main)
	case *logical.LogicalProject:
		if len(o.Projections) == 1 && len(o.AggregateOutputOrdinals) == 1 {
			if agg := findAggregate(o.Input); agg != nil {
				ordinal := o.AggregateOutputOrdinals[0]
				switch {
				case ordinal >= 0 && ordinal < len(agg.GroupKeys):
					if v := agg.GroupKeys[ordinal].Value; v != nil && v.Type() != nil {
						return v.Type(), nil
					}
				case ordinal >= len(agg.GroupKeys) && ordinal < len(agg.GroupKeys)+len(agg.Calls):
					callIdx := ordinal - len(agg.GroupKeys)
					var operand []values.Value
					if callIdx < len(agg.AggregateOperands) {
						operand = []values.Value{agg.AggregateOperands[callIdx]}
					}
					return aggregateCallOutputTypeChecked(agg.Calls[callIdx], operand)
				}
			}
		}
		if len(o.Projections) == 1 && len(o.ProjectedValues) == 1 && o.ProjectedValues[0] != nil {
			if t := o.ProjectedValues[0].Type(); t != nil {
				return t, nil
			}
		}
		return values.UnknownType, nil
	case *logical.LogicalAggregate:
		if len(o.GroupKeys) == 0 && len(o.Calls) == 1 {
			return aggregateCallOutputTypeChecked(o.Calls[0], o.AggregateOperands)
		}
		return values.UnknownType, nil
	}
	return values.UnknownType, nil
}

func aggregateCallOutputTypeChecked(call logical.AggregateCall, operands []values.Value) (values.Type, error) {
	typ := aggregateCallOutputType(call, operands)
	if len(operands) == 0 || operands[0] == nil || operands[0].Type() == nil {
		return typ, nil
	}
	operandCode := operands[0].Type().Code()
	if operandCode == values.TypeCodeUnknown || operandCode.IsNumeric() {
		return typ, nil
	}
	switch strings.ToUpper(call.Func) {
	case "SUM", "AVG", "MIN", "MAX":
		return values.UnknownType, api.NewError(api.ErrCodeUnsupportedOperation,
			"unable to encapsulate aggregate operation due to type mismatch(es)")
	default:
		return typ, nil
	}
}

// aggregateCallOutputType maps an aggregate call to the DECLARED Java
// result type (nullable — Type.primitiveType defaults nullable=true;
// a scalar subquery with zero rows is NULL anyway). The code-level
// table is javaAggregateResultCode; a combination with no Java row —
// including a STRUCTURED operand code, which has no NumericAggregation
// operator and must never reach NewPrimitiveType's structured-code
// panic — reports UnknownType (gate-exempt, no false claims).
func aggregateCallOutputType(call logical.AggregateCall, operands []values.Value) values.Type {
	opCode := values.TypeCodeUnknown
	if len(operands) >= 1 && operands[0] != nil {
		if t := operands[0].Type(); t != nil {
			opCode = t.Code()
		}
	}
	if code, ok := javaAggregateResultCode(call.Func, opCode); ok {
		return values.NewPrimitiveType(code, true)
	}
	return values.UnknownType
}

// javaAggregateResultCode is THE Java aggregate result-type table at the
// TypeCode level (NumericAggregationValue / CountValue, tag 4.12.11.0):
// COUNT and COUNT(*) return LONG regardless of operand; AVG returns
// DOUBLE for every numeric operand; SUM/MIN/MAX return the OPERAND's
// code — and Java defines those operators ONLY over INT/LONG/FLOAT/
// DOUBLE (SUM_I/L/F/D, MIN_*, MAX_*), so any other operand code has no
// row (ok=false). Both this table's consumers document their own
// nullability choice at the call site; aggResultTypeFromFunc layers its
// metadata-specific divergences over the same table.
func javaAggregateResultCode(fn string, operandCode values.TypeCode) (values.TypeCode, bool) {
	return values.JavaAggregateResultCode(fn, operandCode)
}

// colBareOrName: the structured bare segment, or the whole name as one
// opaque label for computed/rebased entries — never a dot split.
func colBareOrName(c projCol) string {
	if c.bare != "" {
		return c.bare
	}
	return c.name
}

// subqueryReferencesOuterColumn reports whether any column reference in body,
// including a local WITH body or nested query whose result contributes to it,
// is one an ENCLOSING scope can answer. It is the evidence that separates a
// subquery this builder merely cannot express from one that was never correlated
// to begin with.
//
// It errs toward TRUE: with no enclosing scope to consult, or a reference this
// walk cannot enumerate, the caller keeps the correlated diagnosis. Claiming
// "not correlated" wrongly would replace a correct decline with a misleading
// column error; the reverse only keeps today's message.
func (p *existsSubqueryPlanner) subqueryReferencesOuterColumn(body antlr.Tree) bool {
	if body == nil {
		return false
	}
	outerScope := p.outerScope
	if outerScope == nil {
		if len(p.outerScopes) == 0 {
			return false
		}
		outerScope = semantic.NewScope(nil)
		for _, src := range p.outerScopes {
			if err := outerScope.AddSource(src); err != nil {
				return true // cannot decide against an incomplete scope
			}
		}
	}
	found := false
	var visit func(n antlr.Tree)
	visit = func(n antlr.Tree) {
		if n == nil || found {
			return
		}
		// Descend through nested query blocks. A local WITH body or nested scalar
		// that reads the enclosing row makes the containing scalar dependent on
		// that row too; stopping at QueryContext misclassified exactly that shape
		// as uncorrelated and pre-evaluated it without its outer binding.
		if column, ok := n.(*antlrgen.FullColumnNameExpressionAtomContext); ok {
			uids := column.FullColumnName().FullId().AllUid()
			segments := make([]semantic.Identifier, 0, len(uids))
			for _, uid := range uids {
				segments = append(segments,
					semantic.FromNormalized(functions.NormalizeIdentifier(uid.GetText())))
			}
			if _, _, _, err := outerScope.ResolvePathNested(segments); err == nil {
				found = true
			}
			return
		}
		for i := 0; i < n.GetChildCount(); i++ {
			visit(n.GetChild(i))
		}
	}
	visit(body)
	return found
}

// requireResolvedLimitClause rejects unresolved pagination before a nested
// query can lose it to the no-limit sentinel. Driver arguments are substituted
// before this phase; a remaining parameter has no executable limit yet.
func requireResolvedLimitClause(simpleTable *antlrgen.SimpleTableContext) error {
	if clause := simpleTable.LimitClause(); clause != nil {
		for _, atom := range clause.AllLimitClauseAtom() {
			if _, resolved, err := resolveLimitAtom(atom); err != nil {
				return err
			} else if !resolved {
				return api.NewError(api.ErrCodeUnsupportedQuery, "a scalar subquery with a planning-time unresolved LIMIT/OFFSET is not supported")
			}
		}
	}
	return nil
}

// innerSourceAliases collects the UPPER source aliases a correlated scalar
// subquery's scan/join tree binds — the universe that discriminates an
// INNER-scope projected field from an OUTER-scope one (the latter is not an
// inner row key and must take the materialized path; see the review-finding
// comment at the caller).
//
// This collector must be BINDER-EXACT, not merely inclusive: an
// over-inclusion flips an outer field to inner-scoped and skips its
// materialization (a wrong-key read). Its query-package twin
// (outerSubtreeAliases) is deliberately MORE inclusive — there an extra entry
// only pushes toward a decline or a skipped classification, never wrong rows.
// The asymmetry is load-bearing; do not harmonize the two collectors.
func innerSourceAliases(op logical.LogicalOperator) map[string]struct{} {
	out := map[string]struct{}{}
	var walk func(logical.LogicalOperator)
	walk = func(op logical.LogicalOperator) {
		if op == nil {
			return
		}
		switch o := op.(type) {
		case *logical.LogicalScan:
			a := o.Binding
			if a == "" {
				a = o.Alias
			}
			if a == "" {
				a = o.Table
			}
			out[strings.ToUpper(a)] = struct{}{}
		case *logical.LogicalCTE:
			if o.Binding != "" {
				out[strings.ToUpper(o.Binding)] = struct{}{}
			} else if o.PreserveMainSource {
				walk(o.Main)
			} else {
				out[strings.ToUpper(o.Name)] = struct{}{}
			}
			return
		case *logical.LogicalUnnest:
			// Use the binder's carried identity before any display alias.
			// With `AS v AT c`, c is a column through the source row, not an
			// additional correlation that could hide a same-named outer source.
			if binding := logical.UnnestBindingName(o.Binding, o.Alias, o.AtAlias); binding != "" {
				out[binding] = struct{}{}
			}
		}
		for _, c := range op.Children() {
			walk(c)
		}
	}
	walk(op)
	return out
}

// wrapWithOuterCTEs wraps op with LogicalCTE nodes for every outer CTE
// whose name appears as a LogicalScan in the plan tree. This makes the
// plan self-contained so the Cascades translator can resolve CTE scan
// references without external scope.
func (p *existsSubqueryPlanner) wrapWithOuterCTEs(op logical.LogicalOperator) logical.LogicalOperator {
	if len(p.cteBodies) == 0 {
		return op
	}
	refs := collectScanTableNames(op)
	for name, body := range p.cteBodies {
		if refs[name] {
			wrapped := logical.NewCTE(name, body, op, false)
			// This is a lexical-scope envelope, not a derived-source alias
			// carrier. The EXISTS/Scalar query's Main owns the outward source
			// identity (e.g. BO in `FROM big_orders BO`); replacing it with the
			// definition name BIG_ORDERS makes the correlation predicate and
			// FlatMap binding disagree even though their rendered rows match.
			wrapped.PreserveMainSource = true
			op = wrapped
		}
	}
	return op
}

// collectScanTableNames returns the set of UPPER-CASE table names
// referenced by LogicalScan nodes in the plan tree.
func collectScanTableNames(op logical.LogicalOperator) map[string]bool {
	names := make(map[string]bool)
	collectScanTableNamesInner(op, names)
	return names
}

func collectScanTableNamesInner(op logical.LogicalOperator, names map[string]bool) {
	if op == nil {
		return
	}
	if scan, ok := op.(*logical.LogicalScan); ok {
		names[strings.ToUpper(scan.Table)] = true
	}
	for _, ch := range op.Children() {
		collectScanTableNamesInner(ch, names)
	}
}

// sortOwnedBySelect reports whether sort is THIS select shell's own sort:
// reachable from the select's projection through row-preserving single-child
// operators only. A Sort below another Project/Aggregate belongs to a NESTED
// select (derived table / CTE body); rewriting its ordinals against the
// OUTER projection would swap in an unrelated item — `SELECT total FROM
// (SELECT id AS x, SUM(score) AS total … ORDER BY 1 …) d` must keep the
// inner ordinal on inner item 1, never the outer's slot 1.
func sortOwnedBySelect(proj *logical.LogicalProject, sort *logical.LogicalSort) bool {
	cur := proj.Input
	for cur != nil {
		if cur == logical.LogicalOperator(sort) {
			return true
		}
		switch cur.(type) {
		case *logical.LogicalFilter, *logical.LogicalLimit, *logical.LogicalDistinct:
			ch := cur.Children()
			if len(ch) != 1 {
				return false
			}
			cur = ch[0]
		default:
			return false
		}
	}
	return false
}

// operatorContains reports whether target appears in root's subtree
// (including root itself). Used to determine relative operator placement
// when a pass's rewrite depends on which of two operators is above.
func operatorContains(root, target logical.LogicalOperator) bool {
	if root == nil {
		return false
	}
	if root == target {
		return true
	}
	for _, ch := range root.Children() {
		if operatorContains(ch, target) {
			return true
		}
	}
	return false
}

// exactStarRowCarriesAnEphemeral reports whether the exact row derived for a
// STAR body states the ephemeral __ROW_VERSION pseudo-column. A star hides it
// (Java's SemanticAnalyzer.expandStar → nonEphemeralVisible), so a row that
// carries it is not the row the star's reader sees: published, `WITH d AS
// (SELECT * FROM aa, bb) SELECT d.y FROM d ORDER BY d.y` over row-versioned
// tables minted a read over a six-column row with two hidden version slots
// that no runtime binding declares, and the derived spelling could not adopt
// its physical output names. A body that spells its projection names every
// column it emits and is never declined here.
//
// The pseudo-column is the one of VERSION type: a REAL column a table declares
// under that name (real-column-wins; `"__ROW_VERSION" STRING`) is star-visible
// and is not it, so the name alone does not decide.
func exactStarRowCarriesAnEphemeral(innerSQ *selectQuery, src semantic.ScopeSource) bool {
	if innerSQ == nil || projectionOutputNames(innerSQ) != nil || src.Table == nil {
		return false
	}
	isPseudo := func(c semantic.Column) bool {
		return c.Id.Name() == values.PseudoFieldRowVersion && c.Type == "VERSION"
	}
	for _, c := range src.Table.Columns() {
		if isPseudo(c) {
			return true
		}
	}
	for _, c := range src.FlowedColumns {
		if isPseudo(c) {
			return true
		}
	}
	return false
}

// nestedProjectedPath reports whether a projected column reference reaches
// INTO a column — `t.w.x`, or `w.x` — by its shape alone: with the body
// source's own qualifier stripped, two or more segments remain. A reference
// is not looked up by its leaf name to find this out; RFC-238's finding is
// that which dot is the qualifier is structure, and a leaf that happens to
// share a top-level column's name must not be mistaken for that column.
func nestedProjectedPath(col projCol, bodySourceName string) bool {
	segs := col.segs
	if len(segs) > 1 && bodySourceName != "" && strings.EqualFold(segs[0], bodySourceName) {
		segs = segs[1:]
	}
	return len(segs) >= 2
}

package embedded

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// jdbcColumnName transforms an internal projection name into the
// JDBC-style result-set metadata name that fdb-relational emits.
// Java uppercases unquoted identifiers, strips qualifiers from
// projected columns, and synthesises `_N` (zero-based position) for
// anonymous expression projections (CASE, COUNT(*), arithmetic, etc.).
//
// Rules:
//   - Bare simple identifier ("id", "name") → upper-cased ("ID").
//   - Qualified column ("t.id", "u.name") → strip qualifier, upper.
//   - Anything containing operators / parens / spaces → "_<pos>".
//
// Idempotent: applying twice yields the same result, so it's safe
// to chain through multiple staticRows wrappers (CTE → SELECT, UNION
// over already-transformed sources).
func jdbcColumnName(name string, position int) string {
	base := parseColRef(name).bare()
	if isSimpleIdentifier(base) {
		return strings.ToUpper(base)
	}
	return fmt.Sprintf("_%d", position)
}

// isSimpleIdentifier reports whether s matches `[A-Za-z_][A-Za-z0-9_]*`.
// Empty strings, leading digits, and any non-ASCII / punctuation
// characters fail the check.
func isSimpleIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// jdbcizeColumnNames returns a copy of cols with each entry transformed
// via jdbcColumnName. Used at the driver-output boundary so the
// internal column-name flow (used for ORDER BY resolution, alias
// remapping, etc.) stays unchanged.
func jdbcizeColumnNames(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = jdbcColumnName(c, i)
	}
	return out
}

// jdbcTypeMax implements the SQL type-promotion lattice over JDBC
// type-name strings. Mirrors cascades.MaximumType for primitives —
// strings are used here instead of the cascades Type enum to avoid
// a package dependency from the embedded engine into cascades/values.
//
// Numeric promotion: DOUBLE > FLOAT > BIGINT > INTEGER (lower
// promotes upward). Non-numeric types (STRING, BOOLEAN, BYTES,
// OTHER): identity — same type returns itself, mismatched returns
// empty. Mixing numeric with non-numeric returns empty.
//
// "" on either side propagates as "" (unknown).
func jdbcTypeMax(a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	if a == b {
		return a
	}
	const temporalBase = 100
	rank := func(t string) int {
		switch t {
		case "INTEGER":
			return 1
		case "BIGINT":
			return 2
		case "FLOAT":
			return 3
		case "DOUBLE":
			return 4
		case "DATE":
			return temporalBase
		case "TIMESTAMP":
			return temporalBase + 1
		}
		return 0
	}
	ra, rb := rank(a), rank(b)
	if ra == 0 || rb == 0 {
		return ""
	}
	// DATE promotes to TIMESTAMP; temporal types are incompatible with numeric.
	if (ra >= temporalBase) != (rb >= temporalBase) {
		return ""
	}
	if ra >= rb {
		return a
	}
	return b
}

// inferProjectionJDBCType walks an expression AST and returns the
// JDBC-style type name of the expression's result, or "" when the
// type can't be inferred (deep / unrecognised shapes fall through to
// the runner's value-based inference).
//
// Handles: bare column refs (looked up in msgDesc), math operators
// (+ - * /, lattice via jdbcTypeMax), bit operators (always
// integer), CAST(expr AS type), parenthesised single-element record
// constructors, and simple constant literals.
//
// Used by the SELECT-path projection-binding step in
// select_query_full.go to populate staticRows.colTypes for computed-
// expression projections that don't carry a FieldDescriptor.
func inferProjectionJDBCType(expr antlrgen.IExpressionContext, msgDesc protoreflect.MessageDescriptor) string {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		// Logical / boolean expressions (AND/OR/NOT, comparisons) —
		// not a value, treat as BOOLEAN.
		return "BOOLEAN"
	}
	if pred.Predicate() != nil {
		// IN, IS, LIKE, BETWEEN — boolean modifier.
		return "BOOLEAN"
	}
	return inferAtomJDBCType(pred.ExpressionAtom(), msgDesc)
}

// inferAtomJDBCType is the recursion entry point for ExpressionAtom
// nodes. Mirrors evalExprAtom's switch in eval_proto.go but returns
// types instead of values.
func inferAtomJDBCType(atom antlrgen.IExpressionAtomContext, msgDesc protoreflect.MessageDescriptor) string {
	switch a := atom.(type) {
	case *antlrgen.ConstantExpressionAtomContext:
		return inferConstantJDBCType(a.Constant())
	case *antlrgen.FullColumnNameExpressionAtomContext:
		colName := functions.FullIdToName(a.FullColumnName().FullId())
		// Strip qualifier for the field lookup, same as the SELECT-path
		// projection-binding does in select_query_full.go.
		bare := parseColRef(colName).bare()
		if msgDesc != nil {
			fd := msgDesc.Fields().ByName(protoreflect.Name(bare))
			if fd != nil {
				return jdbcTypeNameForFD(fd)
			}
		}
		return ""
	case *antlrgen.MathExpressionAtomContext:
		l := inferAtomJDBCType(a.GetLeft(), msgDesc)
		r := inferAtomJDBCType(a.GetRight(), msgDesc)
		return jdbcTypeMax(l, r)
	case *antlrgen.BitExpressionAtomContext:
		// Bit operators (& | ^ << >>) always produce integer; Java
		// promotes both operands to BIGINT before the op so the
		// result is BIGINT regardless of operand widths.
		return "BIGINT"
	case *antlrgen.RecordConstructorExpressionAtomContext:
		// Single-field parenthesised group `(expr)` — recurse inner.
		// Multi-field record constructors don't appear at the
		// projection-name boundary so we ignore them here.
		rc := a.RecordConstructor()
		if rc == nil {
			return ""
		}
		fields := rc.AllExpressionWithOptionalName()
		if len(fields) != 1 {
			return ""
		}
		return inferProjectionJDBCType(fields[0].Expression(), msgDesc)
	case *antlrgen.FunctionCallExpressionAtomContext:
		return inferFunctionCallJDBCType(a.FunctionCall(), msgDesc)
	}
	return ""
}

// inferConstantJDBCType maps a literal AST node to its JDBC type.
// Negative numbers (NegativeDecimalConstantContext) follow the same
// numeric-vs-decimal split as positive ones.
func inferConstantJDBCType(c antlrgen.IConstantContext) string {
	switch cv := c.(type) {
	case *antlrgen.DecimalConstantContext:
		text := cv.DecimalLiteral().GetText()
		if _, err := strconv.ParseInt(text, 10, 64); err == nil {
			return integerLiteralJDBCType(text)
		}
		return "DOUBLE"
	case *antlrgen.NegativeDecimalConstantContext:
		text := "-" + cv.DecimalLiteral().GetText()
		if _, err := strconv.ParseInt(text, 10, 64); err == nil {
			return integerLiteralJDBCType(text)
		}
		return "DOUBLE"
	case *antlrgen.StringConstantContext:
		return "STRING"
	case *antlrgen.BooleanConstantContext:
		return "BOOLEAN"
	case *antlrgen.BytesConstantContext:
		return "BINARY"
	case *antlrgen.NullConstantContext:
		return "" // NULL has no type until promoted by an outer op
	}
	return ""
}

func integerLiteralJDBCType(text string) string {
	if _, err := strconv.ParseInt(text, 10, 32); err == nil {
		return "INTEGER"
	}
	return "BIGINT"
}

// inferFunctionCallJDBCType handles the function-call subtree:
// scalar functions in fdb-relational's registry (COALESCE, GREATEST,
// LEAST, date-parts, ...) and CASE / CAST forms. Walks into the
// SpecificFunctionContext for CAST and CASE.
func inferFunctionCallJDBCType(fc antlrgen.IFunctionCallContext, msgDesc protoreflect.MessageDescriptor) string {
	switch fc := fc.(type) {
	case *antlrgen.SpecificFunctionCallContext:
		return inferSpecificFunctionJDBCType(fc.SpecificFunction(), msgDesc)
	case *antlrgen.ScalarFunctionCallContext:
		return inferScalarFunctionJDBCType(fc, msgDesc)
	}
	return ""
}

// inferScalarFunctionJDBCType handles type inference for the scalar
// functions still implemented Go-side. Functions whose name we don't
// recognise fall through to "" — caller's value-based inference can
// take over. The Go-only feature names (UPPER / LOWER / LENGTH /
// SUBSTRING / TRIM / CONCAT / REPLACE / ABS / SQRT / FLOOR / CEIL /
// ROUND / SIGN / EXP / LN / LOG / IFNULL / IF / IIF / NULLIF / MOD-
// function-form / NOW / CURDATE / etc.) are intentionally absent —
// those reject at evaluation time with "Unsupported operator <name>"
// (byte-equal Java's RelationalException for the same registry-miss).
func inferScalarFunctionJDBCType(fc *antlrgen.ScalarFunctionCallContext, msgDesc protoreflect.MessageDescriptor) string {
	name := strings.ToUpper(fc.ScalarFunctionName().GetText())
	switch name {
	case "COALESCE", "GREATEST", "LEAST":
		args := fc.FunctionArgs()
		if args == nil {
			return ""
		}
		var resultType string
		for _, a := range args.AllFunctionArg() {
			t := inferFunctionArgJDBCType(a, msgDesc)
			if resultType == "" {
				resultType = t
			} else if t != "" {
				resultType = jdbcTypeMax(resultType, t)
				if resultType == "" {
					return ""
				}
			}
		}
		return resultType
	case "YEAR", "MONTH", "DAY", "DAYOFMONTH",
		"HOUR", "MINUTE", "SECOND",
		"DAYOFWEEK", "DAYOFYEAR":
		return "BIGINT"
	}
	return ""
}

// firstFunctionArgType returns the inferred type of args[0] or "" if
// the args list is empty.
func firstFunctionArgType(args antlrgen.IFunctionArgsContext, msgDesc protoreflect.MessageDescriptor) string {
	if args == nil {
		return ""
	}
	all := args.AllFunctionArg()
	if len(all) == 0 {
		return ""
	}
	return inferFunctionArgJDBCType(all[0], msgDesc)
}

// inferSpecificFunctionJDBCType handles CAST and CASE shapes.
//   - CAST(expr AS T): result type is T, mapped from the parser's
//     ConvertedDataType text via convertedDataTypeToJDBC.
//   - CASE WHEN ... THEN x ... ELSE y END: result type is the
//     MaximumType of all THEN / ELSE branches.
func inferSpecificFunctionJDBCType(sf antlrgen.ISpecificFunctionContext, msgDesc protoreflect.MessageDescriptor) string {
	switch sf := sf.(type) {
	case *antlrgen.DataTypeFunctionCallContext:
		// CAST(expr AS dataType). Pull the target type's text and
		// canonicalise to a JDBC name.
		if cdt := sf.ConvertedDataType(); cdt != nil {
			return classifyConvertedDataTypeJDBC(cdt)
		}
	case *antlrgen.CaseFunctionCallContext:
		// Searched CASE: each WHEN-clause has THEN expr; optional ELSE expr.
		// Result type = MaximumType of all branches.
		return inferCaseBranchesJDBCType(sf.AllCaseFuncAlternative(), sf.GetElseArg(), msgDesc)
	case *antlrgen.CaseExpressionFunctionCallContext:
		// Simple CASE: CASE expr WHEN val THEN result ... [ELSE result] END
		return inferCaseBranchesJDBCType(sf.AllCaseFuncAlternative(), sf.GetElseArg(), msgDesc)
	case *antlrgen.SimpleFunctionCallContext:
		switch {
		case sf.CURRENT_DATE() != nil:
			return "DATE"
		case sf.CURRENT_TIMESTAMP() != nil, sf.LOCALTIME() != nil, sf.CURRENT_TIME() != nil:
			return "TIMESTAMP"
		}
	}
	return ""
}

func inferCaseBranchesJDBCType(alts []antlrgen.ICaseFuncAlternativeContext, elseArg antlrgen.IFunctionArgContext, msgDesc protoreflect.MessageDescriptor) string {
	var resultType string
	for _, alt := range alts {
		t := inferFunctionArgJDBCType(alt.GetConsequent(), msgDesc)
		if resultType == "" {
			resultType = t
		} else if t != "" {
			resultType = jdbcTypeMax(resultType, t)
			if resultType == "" {
				return ""
			}
		}
	}
	if elseArg != nil {
		t := inferFunctionArgJDBCType(elseArg, msgDesc)
		if resultType == "" {
			resultType = t
		} else if t != "" {
			resultType = jdbcTypeMax(resultType, t)
			if resultType == "" {
				return ""
			}
		}
	}
	return resultType
}

// inferFunctionArgJDBCType extracts the inner expression from a
// function-arg node and recurses. Used by CASE-branch traversal.
func inferFunctionArgJDBCType(arg antlrgen.IFunctionArgContext, msgDesc protoreflect.MessageDescriptor) string {
	if arg == nil {
		return ""
	}
	if e := arg.Expression(); e != nil {
		return inferProjectionJDBCType(e, msgDesc)
	}
	return ""
}

// classifyPrimitiveType returns the canonical upper-case SQL type name
// from a ConvertedDataType parse node using typed ANTLR terminals.
func classifyPrimitiveType(cdt antlrgen.IConvertedDataTypeContext) string {
	pt := cdt.PrimitiveType()
	if pt == nil {
		return ""
	}
	p, ok := pt.(*antlrgen.PrimitiveTypeContext)
	if !ok {
		return ""
	}
	switch {
	case p.INTEGER() != nil:
		return "INTEGER"
	case p.BIGINT() != nil:
		return "BIGINT"
	case p.FLOAT() != nil:
		return "FLOAT"
	case p.DOUBLE() != nil:
		return "DOUBLE"
	case p.STRING() != nil:
		return "STRING"
	case p.BOOLEAN() != nil:
		return "BOOLEAN"
	case p.BYTES() != nil:
		return "BYTES"
	case p.UUID() != nil:
		return "UUID"
	case p.DATE() != nil:
		return "DATE"
	case p.TIMESTAMP() != nil:
		return "TIMESTAMP"
	}
	return ""
}

// classifyConvertedDataTypeJDBC maps a ConvertedDataType parse node to
// the JDBC type name using typed ANTLR terminals (no GetText()).
func classifyConvertedDataTypeJDBC(cdt antlrgen.IConvertedDataTypeContext) string {
	pt := cdt.PrimitiveType()
	if pt == nil {
		return ""
	}
	p, ok := pt.(*antlrgen.PrimitiveTypeContext)
	if !ok {
		return ""
	}
	switch {
	case p.INTEGER() != nil:
		return "INTEGER"
	case p.BIGINT() != nil:
		return "BIGINT"
	case p.FLOAT() != nil:
		return "FLOAT"
	case p.DOUBLE() != nil:
		return "DOUBLE"
	case p.STRING() != nil:
		return "STRING"
	case p.BOOLEAN() != nil:
		return "BOOLEAN"
	case p.BYTES() != nil:
		return "BINARY"
	case p.UUID() != nil:
		return "OTHER"
	case p.DATE() != nil:
		return "DATE"
	case p.TIMESTAMP() != nil:
		return "TIMESTAMP"
	}
	return ""
}

// aggregateResultJDBCType returns the JDBC result-set type name for
// an aggregate-column slot. Handles GROUP-BY columns (look up the
// underlying field's type), pure aggregates (COUNT → BIGINT;
// SUM/MIN/MAX → operand type; AVG → DOUBLE), and outExpr post-
// aggregation slots (heuristic: BIGINT). Empty string for shapes
// where the type can't be determined statically — the runner-side
// fallback handles those (no slot today).
func aggregateResultJDBCType(ac aggSelectCol, msgDesc protoreflect.MessageDescriptor) string {
	// GROUP BY column: type comes from the underlying field.
	if ac.groupCol != "" {
		bare := parseColRef(ac.groupCol).bare()
		if msgDesc != nil {
			if fd := msgDesc.Fields().ByName(protoreflect.Name(bare)); fd != nil {
				return jdbcTypeNameForFD(fd)
			}
		}
		return ""
	}
	// outExpr post-aggregation: walking the expression isn't trivial
	// because operand columns reference aggregator slots, not table
	// fields. Default to BIGINT — covers SUM(a) + SUM(b) shapes.
	if ac.outExpr != nil {
		return "BIGINT"
	}
	// Pure aggregate.
	switch ac.aggFunc {
	case "COUNT":
		return "BIGINT"
	case "AVG":
		return "DOUBLE"
	case "SUM", "MIN", "MAX":
		// Result inherits the operand's type. Fast path: bare column
		// argument → look up the FieldDescriptor. Expression arg
		// (SUM(qty * price)) walks the expression AST.
		if ac.aggArg != "" {
			bare := parseColRef(ac.aggArg).bare()
			if msgDesc != nil {
				if fd := msgDesc.Fields().ByName(protoreflect.Name(bare)); fd != nil {
					return jdbcTypeNameForFD(fd)
				}
			}
		}
		if ac.aggExpr != nil {
			return inferProjectionJDBCType(ac.aggExpr, msgDesc)
		}
		return "BIGINT" // best-effort default
	}
	return ""
}

// jdbcTypeNameForFD maps a protoreflect FieldDescriptor's kind to the
// JDBC-style type name fdb-relational reports via
// ResultSetMetaData.getColumnTypeName(). Empty string for unmappable /
// nil descriptors — the staticRows.ColumnTypeDatabaseTypeName implementation
// treats "" as "type unknown".
func jdbcTypeNameForFD(fd protoreflect.FieldDescriptor) string {
	if fd == nil {
		return ""
	}
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return "BOOLEAN"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return "INTEGER"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "BIGINT"
	case protoreflect.FloatKind:
		return "FLOAT"
	case protoreflect.DoubleKind:
		return "DOUBLE"
	case protoreflect.StringKind:
		return "STRING"
	case protoreflect.BytesKind:
		// JDBC standard for variable-length byte arrays is BINARY; this
		// is what fdb-relational's ResultSetMetaData reports.
		return "BINARY"
	case protoreflect.MessageKind:
		// UUID columns are stored as the tuple_fields.UUID message
		// and reported as JDBC's catch-all "OTHER" type name (matches
		// Java's java.sql.Types.OTHER for UUIDs).
		if msg := fd.Message(); msg != nil && string(msg.FullName()) == functions.UUIDProtoMessageName {
			return "OTHER"
		}
	}
	return ""
}

// SELECT-path helpers shared by the single-table / JOIN / CTE
// executors in select_query_full.go / join.go / cte_scan.go.
//
//   cteRowsToMaps            CTE rows → qualified + unqualified
//                            map representation
//   scanTableToMaps          table rows (or CTE rows) → same
//                            map representation
//   resolveQualifierColumns  `<qualifier>.*` → ordered columns
//                            from the named source
//   resolveSelectListPosition  `ORDER BY 2` → output column name
//   expandStarSlots          rewrites `a.*` in mixed projections
//   collectLeftJoinKeys      row-map keys to NULL-pad for RIGHT JOIN
//   computeAmbiguousBareColumns / poisonAmbiguousBareCols
//                            ambiguous-unqualified-reference
//                            detection + poisoning
//
// Destined for plan/physical/helpers.go once the SELECT executors
// move out of this package.

// cteRowsToMaps converts materialized CTE data into the map format used by JOIN evaluation.
func cteRowsToMaps(cte *cteData, alias string) []map[string]driver.Value {
	result := make([]map[string]driver.Value, len(cte.rows))
	for i, row := range cte.rows {
		m := make(map[string]driver.Value, len(cte.cols)*2)
		for j, col := range cte.cols {
			m[col] = row[j]
			m[alias+"."+col] = row[j]
		}
		result[i] = m
	}
	return result
}

// scanTableToMaps scans all records of tableName into a slice of maps.
// Each map has two key styles:
//   - "alias.colName" (qualified, using alias or tableName)
//   - "colName" (unqualified, for convenience)
func (c *EmbeddedConnection) scanTableToMaps(
	ctx context.Context,
	store *recordlayer.FDBRecordStore,
	tableName, alias string,
) ([]map[string]driver.Value, error) {
	// If the table name resolves to a CTE, return materialized rows directly.
	if c.ctes != nil {
		if cte, ok := c.ctes[strings.ToUpper(tableName)]; ok {
			return cteRowsToMaps(cte, alias), nil
		}
	}

	cursor := store.ScanRecordsByType(tableName, nil, recordlayer.ForwardScan())
	defer cursor.Close() //nolint:errcheck

	var rows []map[string]driver.Value
	for {
		result, nextErr := cursor.OnNext(ctx)
		if nextErr != nil {
			return nil, nextErr
		}
		if !result.HasNext() {
			break
		}
		msg := result.GetValue().Record
		msgRef := msg.ProtoReflect()
		fields := msgRef.Descriptor().Fields()
		m := make(map[string]driver.Value, fields.Len()*2)
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			col := string(fd.Name())
			var v driver.Value
			if msgRef.Has(fd) {
				v = functions.ProtoValueToDriver(fd, msgRef.Get(fd))
			}
			m[col] = v
			m[alias+"."+col] = v
		}
		rows = append(rows, m)
	}
	return rows, nil
}

// resolveQualifierColumns resolves a `<qualifier>.*` SELECT list against
// the FROM-clause sources. Returns the ordered column list from the matching
// source and the effective alias (useful for row-key lookups of the form
// "alias.col"). Returns ErrCodeUndefinedTable when the qualifier does not
// match any source.
//
// Matching rules (first match wins):
//  1. tableAlias (or tableName when no explicit alias was given).
//  2. Any joins[i].alias (or joins[i].tableName when no alias).
//
// Columns come from: the CTE definition when the source names a CTE; the
// record type descriptor otherwise.
func (c *EmbeddedConnection) resolveQualifierColumns(md *recordlayer.RecordMetaData, sq *selectQuery, qualifier string) ([]string, string, error) {
	type source struct {
		tableName string
		alias     string // falls back to tableName when not explicitly aliased
	}
	sources := make([]source, 0, 1+len(sq.joins))
	leftAlias := sq.tableAlias
	if leftAlias == "" {
		leftAlias = sq.tableName
	}
	sources = append(sources, source{tableName: sq.tableName, alias: leftAlias})
	for _, jc := range sq.joins {
		a := jc.alias
		if a == "" {
			a = jc.tableName
		}
		sources = append(sources, source{tableName: jc.tableName, alias: a})
	}

	for _, s := range sources {
		if !strings.EqualFold(s.alias, qualifier) {
			continue
		}
		if c.ctes != nil {
			if cte, ok := c.ctes[strings.ToUpper(s.tableName)]; ok {
				cols := make([]string, len(cte.cols))
				copy(cols, cte.cols)
				return cols, s.alias, nil
			}
		}
		rt := md.GetRecordType(s.tableName)
		if rt == nil {
			return nil, "", api.NewErrorf(api.ErrCodeUndefinedTable,
				"qualifier %q resolves to table %q which has no record type", qualifier, s.tableName)
		}
		fields := rt.Descriptor.Fields()
		cols := make([]string, 0, fields.Len())
		for i := 0; i < fields.Len(); i++ {
			cols = append(cols, string(fields.Get(i).Name()))
		}
		return cols, s.alias, nil
	}

	return nil, "", api.NewErrorf(api.ErrCodeUndefinedTable,
		"SELECT %s.*: qualifier does not match any FROM-clause source", qualifier)
}

// resolveSelectListPosition maps a SQL-92 positional reference (e.g.
// `ORDER BY 2` or `GROUP BY 1`) to the matching output column name from
// the current SELECT list. `clause` is the SQL keyword used for the
// out-of-range error message ("ORDER BY" or "GROUP BY"). Accepts a
// positive integer literal (DecimalConstant wrapped in
// PredicatedExpression→ConstantExpressionAtom).
//
// Returns:
//   - (name, true, nil): positional reference resolved to an output column.
//   - ("", false, nil): the expression isn't a positional reference at all
//     (caller falls through to column / expression paths).
//   - ("", false, err): expression IS a positive integer literal but N is
//     out of range. Postgres / MySQL error on this instead of treating the
//     integer as a constant sort / group key, so we do the same.
func resolveSelectListPosition(clause string, expr antlrgen.IExpressionContext, projCols, projAliases []string, aggCols []aggSelectCol) (string, bool, error) {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return "", false, nil
	}
	atom, ok := pred.ExpressionAtom().(*antlrgen.ConstantExpressionAtomContext)
	if !ok {
		return "", false, nil
	}
	dec, ok := atom.Constant().(*antlrgen.DecimalConstantContext)
	if !ok {
		return "", false, nil
	}
	n, err := strconv.ParseInt(dec.DecimalLiteral().GetText(), 10, 64)
	if err != nil || n < 1 {
		return "", false, nil
	}
	listLen := len(projCols)
	if listLen == 0 {
		listLen = len(aggCols)
	}
	if int(n) > listLen {
		return "", false, api.NewErrorf(api.ErrCodeInvalidParameter,
			"%s position %d is out of range: SELECT list has %d entries", clause, n, listLen)
	}
	switch {
	case len(projCols) > 0:
		if int(n) <= len(projAliases) && projAliases[n-1] != "" {
			return projAliases[n-1], true, nil
		}
		return projCols[n-1], true, nil
	case len(aggCols) > 0:
		return aggCols[n-1].outName, true, nil
	}
	return "", false, nil
}

// expandStarSlots expands mixed SELECT lists of the form
// `SELECT a.*, b.label FROM a, b` by rewriting each `<qualifier>.*` slot
// (marked via sq.projStarQualifiers[i]) into its resolved per-source
// column list. After expansion projStarQualifiers is zeroed out so the
// downstream execution loop can treat every slot as a plain named column.
//
// For each expanded column `col` from source with alias `A`, projCols[k]
// becomes `A.col` (alias-qualified, matching the keys scanTableToMaps
// writes and the ORDER BY resolver which expects qualified names to
// appear in cols[]) and projAliases[k] stays empty — the downstream
// cols-from-projCols fallback uses projCols verbatim, which keeps
// ORDER BY a.id resolvable. The runner compares row values, not
// column names, so the qualified output name is fine. projExprs[k] = nil.
//
// No-op when projCols is nil (pure SELECT * / pure qualifier-star take
// the legacy projQualifier / nil-projCols paths) or when no slot is a
// star. Safe to call multiple times — subsequent calls see an empty
// star set and bail.
func (c *EmbeddedConnection) expandStarSlots(md *recordlayer.RecordMetaData, sq *selectQuery) error {
	if sq.projCols == nil {
		return nil
	}
	hasStar := false
	for _, q := range sq.projStarQualifiers {
		if q != "" {
			hasStar = true
			break
		}
	}
	if !hasStar {
		return nil
	}
	newCols := make([]string, 0, len(sq.projCols))
	newAliases := make([]string, 0, len(sq.projCols))
	newExprs := make([]antlrgen.IExpressionContext, 0, len(sq.projCols))
	newStars := make([]string, 0, len(sq.projCols))
	for i, col := range sq.projCols {
		qual := ""
		if i < len(sq.projStarQualifiers) {
			qual = sq.projStarQualifiers[i]
		}
		if qual == "" {
			newCols = append(newCols, col)
			newAliases = append(newAliases, sq.projAliases[i])
			newExprs = append(newExprs, sq.projExprs[i])
			newStars = append(newStars, "")
			continue
		}
		cols, qAlias, err := c.resolveQualifierColumns(md, sq, qual)
		if err != nil {
			return err
		}
		for _, cn := range cols {
			newCols = append(newCols, qAlias+"."+cn)
			newAliases = append(newAliases, "")
			newExprs = append(newExprs, nil)
			newStars = append(newStars, "")
		}
	}
	sq.projCols = newCols
	sq.projAliases = newAliases
	sq.projExprs = newExprs
	sq.projStarQualifiers = newStars
	return nil
}

// collectLeftJoinKeys returns the set of row-map keys that describe the
// left-hand side of a RIGHT JOIN — unqualified column names and
// alias-qualified variants for every source that has already been
// merged into the nested-loop `joined` accumulator. Used for NULL-
// padding unmatched right rows; deriving the keys from metadata
// (record type or CTE) instead of sampling a runtime row means the
// NULL-padding works even when the left side is entirely empty.
//
// `sources` must list the sources in the order they were merged in,
// with the same tableName / alias that scanTableToMaps was given (so
// the alias-qualified keys match the ones stored on real rows).
func (c *EmbeddedConnection) collectLeftJoinKeys(md *recordlayer.RecordMetaData, sources []struct{ tableName, alias string }) []string {
	seen := make(map[string]struct{})
	var keys []string
	addKey := func(k string) {
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}

	for _, s := range sources {
		alias := s.alias
		if alias == "" {
			alias = s.tableName
		}
		var cols []string
		if c.ctes != nil {
			if cte, ok := c.ctes[strings.ToUpper(s.tableName)]; ok {
				cols = cte.cols
			}
		}
		if cols == nil {
			rt := md.GetRecordType(s.tableName)
			if rt != nil {
				fields := rt.Descriptor.Fields()
				cols = make([]string, 0, fields.Len())
				for i := 0; i < fields.Len(); i++ {
					cols = append(cols, string(fields.Get(i).Name()))
				}
			}
		}
		for _, col := range cols {
			addKey(col)
			addKey(alias + "." + col)
		}
	}
	return keys
}

// computeAmbiguousBareColumns returns the set of bare column names that
// appear in the schema of more than one FROM-clause source (including
// comma-cross-joins and explicit JOINs). Unqualified references to such
// columns are ambiguous per SQL §6.4 and must error 42702 at lookup
// time. Column sources come from the CTE column list when the source
// names a CTE, and from the record type descriptor otherwise.
func (c *EmbeddedConnection) computeAmbiguousBareColumns(md *recordlayer.RecordMetaData, sq *selectQuery) map[string]bool {
	sources := make([]struct{ tableName string }, 0, 1+len(sq.joins))
	sources = append(sources, struct{ tableName string }{sq.tableName})
	for _, jc := range sq.joins {
		sources = append(sources, struct{ tableName string }{jc.tableName})
	}
	counts := make(map[string]int)
	for _, s := range sources {
		var cols []string
		if c.ctes != nil {
			if cte, ok := c.ctes[strings.ToUpper(s.tableName)]; ok {
				cols = cte.cols
			}
		}
		if cols == nil {
			rt := md.GetRecordType(s.tableName)
			if rt != nil {
				fields := rt.Descriptor.Fields()
				cols = make([]string, 0, fields.Len())
				for i := 0; i < fields.Len(); i++ {
					cols = append(cols, string(fields.Get(i).Name()))
				}
			}
		}
		// A single source listing the same column twice (descriptors
		// shouldn't, CTEs also shouldn't) must not self-bump the count.
		seen := make(map[string]bool, len(cols))
		for _, col := range cols {
			if !seen[col] {
				counts[col]++
				seen[col] = true
			}
		}
	}
	result := make(map[string]bool)
	for col, count := range counts {
		if count > 1 {
			result[col] = true
		}
	}
	return result
}

// poisonAmbiguousBareCols overwrites any bare key in row that matches an
// entry in ambiguous with the ambiguousColumnMarker sentinel. Qualified
// (alias.col) entries are left untouched, so callers that qualify their
// reference still resolve normally. Call after every row merge/build
// path in execSelectJoin so no emitted row exposes the last-write-wins
// bare value.
func poisonAmbiguousBareCols(row map[string]driver.Value, ambiguous map[string]bool) {
	for col := range ambiguous {
		if _, has := row[col]; has {
			row[col] = ambiguousColumnMarker{Col: col}
		}
	}
}

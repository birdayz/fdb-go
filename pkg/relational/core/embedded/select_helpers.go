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
	base := name
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		base = name[dot+1:]
	}
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
		return "BYTES"
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

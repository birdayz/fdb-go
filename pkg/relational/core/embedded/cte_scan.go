package embedded

import (
	"context"
	"database/sql/driver"
	"sort"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// execSelectFromCTE consumes a materialised CTE result set as if it
// were a base table — WHERE filter + projection + ORDER BY + LIMIT /
// OFFSET. Aggregates and JOINs against CTEs remain unsupported here;
// Phase 1c / Cascades subsumes this path into a general
// LogicalTableScan over an in-memory relation.

// execSelectFromCTE executes a SELECT against a materialized CTE result set.
// Supports WHERE, projected columns, ORDER BY, LIMIT, and OFFSET.
// Aggregate queries and JOINs against CTEs are not yet supported.
func (c *EmbeddedConnection) execSelectFromCTE(ctx context.Context, sq *selectQuery, cte *cteData) (driver.Rows, error) {
	// WHERE bare-paren rejection — fire before any row materialization
	// since the check is row-independent (purely structural over the
	// parse tree).
	if sq.whereExpr != nil {
		if err := rejectTopLevelParenthesizedWhere(sq.whereExpr.Expression()); err != nil {
			return nil, err
		}
	}
	alias := sq.tableAlias
	if alias == "" {
		alias = sq.tableName
	}

	// Qualified-star (SELECT <q>.*) must match this CTE's alias or the CTE
	// name itself. Any other qualifier is undefined in a single-source FROM.
	// Inline (rather than calling resolveQualifierColumns) because this
	// path has no RecordMetaData in scope — a CTE-backed query never
	// consults the schema. The rule is trivially the same either way.
	if sq.projQualifier != "" &&
		!strings.EqualFold(sq.projQualifier, alias) &&
		!strings.EqualFold(sq.projQualifier, sq.tableName) {
		return nil, api.NewErrorf(api.ErrCodeUndefinedTable,
			"SELECT %s.*: qualifier does not match FROM-clause source %q",
			sq.projQualifier, alias)
	}

	// Build map rows.
	mapRows := cteRowsToMaps(cte, alias)

	// Apply WHERE filter. (rejectTopLevelParenthesizedWhere fires
	// once at execSelectFromCTE entry — see the function preamble.)
	if sq.whereExpr != nil {
		filtered := mapRows[:0]
		for _, row := range mapRows {
			ok, err := evalPredicateOnMapExpr(ctx, c, row, sq.whereExpr.Expression())
			if err != nil {
				return nil, err
			}
			if ok {
				filtered = append(filtered, row)
			}
		}
		mapRows = filtered
	}

	// Determine output columns and build output rows.
	var colNames []string
	var colTypes []string
	var outRows [][]driver.Value

	// Build a CTE-column → JDBC-type lookup so projection paths can
	// propagate types from the materialised relation. Bare column
	// names match directly; qualified `alias.col` strips the qualifier.
	cteTypeByCol := make(map[string]string, len(cte.cols))
	for i, col := range cte.cols {
		t := ""
		if i < len(cte.colTypes) {
			t = cte.colTypes[i]
		}
		cteTypeByCol[col] = t
		if dot := strings.LastIndex(col, "."); dot >= 0 {
			cteTypeByCol[col[dot+1:]] = t
		}
	}

	if len(sq.aggCols) > 0 || sq.countStar {
		aggCols, aggColTypes, aggData, aggErr := c.aggregateMapRows(ctx, sq, mapRows)
		if aggErr != nil {
			return nil, aggErr
		}
		colNames = aggCols
		colTypes = aggColTypes
		outRows = aggData
	} else if sq.projCols == nil {
		// SELECT * — emit all CTE columns in definition order.
		colNames = cte.cols
		colTypes = append([]string{}, cte.colTypes...)
		for _, row := range mapRows {
			outRow := make([]driver.Value, len(cte.cols))
			for j, col := range cte.cols {
				outRow[j] = row[col]
			}
			outRows = append(outRows, outRow)
		}
	} else {
		colNames = make([]string, len(sq.projCols))
		colTypes = make([]string, len(sq.projCols))
		for j, col := range sq.projCols {
			if j < len(sq.projAliases) && sq.projAliases[j] != "" {
				colNames[j] = sq.projAliases[j]
			} else {
				colNames[j] = col
			}
			// Type comes from the CTE's column types when this slot
			// is a bare column ref; computed-expression slots
			// (projExprs[j] != nil) leave colTypes[j] = "" — the
			// expression evaluator can't yet infer types over CTE
			// rows.
			if j < len(sq.projExprs) && sq.projExprs[j] != nil {
				continue
			}
			bare := col
			if dot := strings.LastIndex(col, "."); dot >= 0 {
				bare = col[dot+1:]
			}
			colTypes[j] = cteTypeByCol[bare]
		}
		// Java alignment (cte.yamsql line 111,114): when a WITH rename
		// renames the CTE columns (e.g. `WITH c1(w, z) AS (SELECT id,
		// v FROM t)`), the original names (id, v) are no longer
		// visible from the CTE. Pre-swingshift-41 Go emitted NULL for
		// unknown bare columns since `row[col]` returns zero value on
		// miss. Validate each bare col against the CTE's cols; error
		// 42703 when missing. Qualified `alias.col` follows the same
		// rule on the bare suffix. Expression slots (projExprs[j] !=
		// nil) skip the check — evalExprOnMap raises 42703 itself.
		cteColSet := make(map[string]bool, len(cte.cols))
		for _, col := range cte.cols {
			cteColSet[col] = true
		}
		for j, col := range sq.projCols {
			if j < len(sq.projExprs) && sq.projExprs[j] != nil {
				continue
			}
			if col == "" {
				continue // qualifier-star sentinel; handled elsewhere
			}
			bare := col
			if dot := strings.LastIndex(col, "."); dot >= 0 {
				bare = col[dot+1:]
			}
			if !cteColSet[bare] {
				return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
					"column %q not found in CTE %q", col, sq.tableName)
			}
		}
		for _, row := range mapRows {
			outRow := make([]driver.Value, len(sq.projCols))
			for j, col := range sq.projCols {
				if j < len(sq.projExprs) && sq.projExprs[j] != nil {
					if j < len(sq.projConstFolded) && sq.projConstFolded[j].present {
						if v := sq.projConstFolded[j].value; v != nil {
							outRow[j] = v.(driver.Value) //nolint:forcetypeassert
						}
						continue
					}
					v, evalErr := evalExprOnMap(ctx, c, row, sq.projExprs[j])
					if evalErr != nil {
						return nil, evalErr
					}
					outRow[j] = v
				} else {
					outRow[j] = row[col]
				}
			}
			outRows = append(outRows, outRow)
		}
	}

	// SELECT DISTINCT against a CTE. Pre-fix the CTE path didn't
	// dedupe at all — `SELECT DISTINCT v FROM cte` returned every
	// duplicate row through. Same dedup-on-projected-cols semantic
	// the JOIN and proto paths use.
	if sq.distinct && !sq.countStar && len(sq.aggCols) == 0 {
		seen := make(map[string]struct{}, len(outRows))
		dedupedRows := outRows[:0]
		dedupedMaps := mapRows[:0]
		for i, row := range outRows {
			key := rowKey(row)
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				dedupedRows = append(dedupedRows, row)
				if i < len(mapRows) {
					dedupedMaps = append(dedupedMaps, mapRows[i])
				}
			}
		}
		outRows = dedupedRows
		mapRows = dedupedMaps
	}

	// ORDER BY. For aggregate CTE results, outRows was built via
	// aggregateMapRows and mapRows is no longer in lockstep — use colName
	// path only. For non-aggregate CTE, mapRows[i] matches outRows[i].
	if len(sq.orderBy) > 0 {
		colIdx := make(map[string]int, len(colNames))
		for i, cn := range colNames {
			colIdx[cn] = i
		}
		hasExpr := false
		for _, ob := range sq.orderBy {
			if ob.expr != nil {
				hasExpr = true
				break
			}
		}
		var keys [][]driver.Value
		if hasExpr {
			if len(mapRows) != len(outRows) {
				return nil, api.NewError(api.ErrCodeUnsupportedOperation,
					"ORDER BY on an arithmetic / function expression is not supported when the query also aggregates; use a column or a plain aggregate (e.g. ORDER BY SUM(col))")
			}
			keys = make([][]driver.Value, len(outRows))
			for i := range outRows {
				keys[i] = make([]driver.Value, len(sq.orderBy))
				for oi, ob := range sq.orderBy {
					if ob.expr != nil {
						v, evalErr := evalExprOnMap(ctx, c, mapRows[i], ob.expr)
						if evalErr != nil {
							return nil, evalErr
						}
						keys[i][oi] = v
					}
				}
			}
		}
		indexes := make([]int, len(outRows))
		for i := range indexes {
			indexes[i] = i
		}
		// Pre-validate ORDER BY column references against the CTE: any
		// name not in colIdx and not present in the materialised CTE row
		// keys is a typo and must error rather than silently no-op'ing
		// the sort. Round-10 reviewer note. Skips expression-keyed and
		// aggregate-path ORDER BY items (they're handled above).
		if len(mapRows) == len(outRows) && len(mapRows) > 0 {
			for _, ob := range sq.orderBy {
				if ob.expr != nil {
					continue
				}
				if _, ok := colIdx[ob.colName]; ok {
					continue
				}
				if _, present := mapRows[0][ob.colName]; !present {
					return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
						"ORDER BY column %q not found in CTE %q", ob.colName, sq.tableName)
				}
			}
		}
		sort.SliceStable(indexes, func(ii, jj int) bool {
			i, j := indexes[ii], indexes[jj]
			for oi, ob := range sq.orderBy {
				var a, b driver.Value
				if ob.expr != nil && keys != nil {
					a, b = keys[i][oi], keys[j][oi]
				} else if idx, ok := colIdx[ob.colName]; ok {
					a, b = outRows[i][idx], outRows[j][idx]
				} else if len(mapRows) == len(outRows) {
					// ORDER BY on a CTE column not in the projection
					// (`SELECT grp FROM s ORDER BY total`). Materialised
					// CTE rows still carry the column in their map; pull
					// the value directly. mapRows[i] is in lockstep with
					// outRows[i] only on the non-aggregate CTE path.
					a, b = mapRows[i][ob.colName], mapRows[j][ob.colName]
				} else {
					continue
				}
				less, equal := orderByLess(a, b, ob)
				if !equal {
					return less
				}
			}
			return false
		})
		sorted := make([][]driver.Value, len(outRows))
		for nn, oldIdx := range indexes {
			sorted[nn] = outRows[oldIdx]
		}
		outRows = sorted
	}

	// OFFSET then LIMIT.
	if sq.offset > 0 {
		if sq.offset >= int64(len(outRows)) {
			outRows = nil
		} else {
			outRows = outRows[sq.offset:]
		}
	}
	if sq.limit >= 0 && int64(len(outRows)) > sq.limit {
		outRows = outRows[:sq.limit]
	}
	if len(sq.aggCols) > 0 {
		colNames, outRows = stripAggregateSortOnly(sq, colNames, outRows)
	}

	return &staticRows{cols: colNames, colTypes: colTypes, rows: outRows}, nil
}

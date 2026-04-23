package embedded

import (
	"database/sql/driver"
	"io"
	"sort"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// driver.Rows shims for pre-materialised result sets.
//
// staticRows wraps a slice of rows the engine assembled in-memory
// (INFORMATION_SCHEMA handlers, SHOW … handlers, projected cursor
// output after an aggregate or sort, etc.). Implements just enough
// of driver.Rows to stream back to the caller — no FDB transaction
// carries over.
//
// emptyRows is the degenerate case — zero columns, zero data,
// returned by DDL statements that have no row output but still
// need a non-nil driver.Rows.
//
// projectSystemRows applies a SELECT's projCols + ORDER BY +
// LIMIT / OFFSET to the output of an INFORMATION_SCHEMA / SHOW
// handler. System-table handlers always emit every column with
// canonical names; projectSystemRows narrows them to what the
// user actually asked for.

type staticRows struct {
	cols    []string
	rows    [][]driver.Value
	current int
}

func (r *staticRows) Columns() []string { return r.cols }
func (r *staticRows) Close() error      { r.current = len(r.rows); return nil }
func (r *staticRows) Next(dest []driver.Value) error {
	if r.current >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.current])
	r.current++
	return nil
}

// emptyRows is a driver.Rows with no columns and no data.
type emptyRows struct{}

func (emptyRows) Columns() []string           { return []string{} }
func (emptyRows) Close() error                { return nil }
func (emptyRows) Next(_ []driver.Value) error { return io.EOF }

// projectSystemRows applies the SELECT-list projection, ORDER BY, and
// LIMIT/OFFSET of `sq` to the rows returned by an INFORMATION_SCHEMA
// handler. System-table handlers always emit every column; without a
// projection step `SELECT TABLE_NAME FROM "INFORMATION_SCHEMA"."TABLES"`
// returns all 10 TABLES columns. Column name matching is case-
// insensitive — CREATE TABLE preserves identifier case, but an
// INFORMATION_SCHEMA filter typically uses the canonical upper-cased
// column names regardless.
//
// Computed expressions (SELECT UPPER(TABLE_NAME) ...) are not
// supported — system-table SELECT lists are limited to plain column
// references and SELECT *. Projection aliases override the column
// name in the returned row set.
func projectSystemRows(in driver.Rows, sq *selectQuery) (driver.Rows, error) {
	sr, ok := in.(*staticRows)
	if !ok {
		// Handler returned a non-staticRows implementation; pass through.
		return in, nil
	}
	rows := sr
	if sq.projCols != nil {
		idxByCol := make(map[string]int, len(rows.cols))
		for i, c := range rows.cols {
			idxByCol[strings.ToUpper(c)] = i
		}
		projIdx := make([]int, len(sq.projCols))
		projNames := make([]string, len(sq.projCols))
		for i, col := range sq.projCols {
			if i < len(sq.projExprs) && sq.projExprs[i] != nil {
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
					"computed expressions in INFORMATION_SCHEMA SELECT are not supported (%s)", col)
			}
			idx, found := idxByCol[strings.ToUpper(col)]
			if !found {
				return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
					"column %q not found in INFORMATION_SCHEMA.%s", col, sq.tableName)
			}
			projIdx[i] = idx
			name := col
			if i < len(sq.projAliases) && sq.projAliases[i] != "" {
				name = sq.projAliases[i]
			}
			projNames[i] = name
		}
		projected := make([][]driver.Value, len(rows.rows))
		for i, row := range rows.rows {
			out := make([]driver.Value, len(projIdx))
			for j, idx := range projIdx {
				out[j] = row[idx]
			}
			projected[i] = out
		}
		rows = &staticRows{cols: projNames, rows: projected}
	}

	// ORDER BY — column-name based. Expression-based ORDER BY
	// (`ORDER BY LENGTH(TABLE_NAME)`) is silently ignored on system
	// tables — `ob.expr != nil` falls through the `continue` below.
	// Consistent with the "plain column references only" policy the
	// SELECT list also enforces; users can alias the expression in
	// a derived table if they need it. `ob.colName` is matched case-
	// insensitively against the projected column names so aliased
	// columns in the SELECT list sort under their alias.
	if len(sq.orderBy) > 0 {
		colIdx := make(map[string]int, len(rows.cols))
		for i, c := range rows.cols {
			colIdx[strings.ToUpper(c)] = i
		}
		sort.SliceStable(rows.rows, func(ii, jj int) bool {
			for _, ob := range sq.orderBy {
				if ob.expr != nil {
					continue // not supported here
				}
				idx, found := colIdx[strings.ToUpper(ob.colName)]
				if !found {
					continue
				}
				a, b := rows.rows[ii][idx], rows.rows[jj][idx]
				less, equal := orderByLess(a, b, ob)
				if !equal {
					return less
				}
			}
			return false
		})
	}

	// OFFSET then LIMIT.
	if sq.offset > 0 {
		if sq.offset >= int64(len(rows.rows)) {
			rows.rows = nil
		} else {
			rows.rows = rows.rows[sq.offset:]
		}
	}
	if sq.limit >= 0 && int64(len(rows.rows)) > sq.limit {
		rows.rows = rows.rows[:sq.limit]
	}

	return rows, nil
}

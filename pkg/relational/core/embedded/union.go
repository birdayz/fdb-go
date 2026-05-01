package embedded

import (
	"context"
	"database/sql/driver"
	"sort"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// UNION ALL executor.
//
// Trailing ORDER BY / LIMIT / OFFSET on the rightmost simpleTable
// is lifted to the combined result — SQL-standard semantics the
// ANTLR grammar hides by greedily attaching the clause to the last
// selectElements. Column count + column-type compatibility checks
// match Java's union.yamsql contract (UNION ALL mismatch → 42F64,
// type mismatch → 42F65).
//
// Plain `UNION` (implicit DISTINCT) is rejected at entry — Java
// alignment. fdb-relational's planner has no de-duplication
// operator wired into the union path, so DISTINCT is not supported.
//
// Destined for plan/physical/union.go per RFC 021 Phase 1c.

// execUnion executes a UNION ALL query. Plain UNION (implicit
// DISTINCT) is rejected at entry per the  alignment.
//
// Trailing ORDER BY / LIMIT / OFFSET on the rightmost simpleTable is lifted
// to the combined result. SQL-standard semantics (and Postgres, MySQL): a
// trailing `ORDER BY ... LIMIT N` on a UNION applies to the whole result,
// not just the last branch. The ANTLR grammar nests the clause inside the
// right-side simpleTable because the parser greedily attaches it to the
// last selectElements production, so we pull it back up here.
//
// For a three-way union `A UNION B UNION C ORDER BY col`, the grammar
// produces SetQuery(SetQuery(A, B), C). The outer execUnion lifts C's
// ORDER BY post-combined — correct. A three-way union with an ORDER BY
// bound to the middle SELECT (e.g. `A UNION B ORDER BY col UNION C`)
// would be parsed as SetQuery(SetQuery(A, B_ordered), C) and the inner
// execUnion would sort A∪B without the outer UNION re-sorting; that
// form is also a syntax error in Postgres (ORDER BY can only appear at
// the end without parentheses), so we do not expect valid SQL to hit
// the degenerate case.
func (c *EmbeddedConnection) execUnion(ctx context.Context, setQ *antlrgen.SetQueryContext) (driver.Rows, error) {
	// Java verbatim: "only UNION ALL is supported". fdb-relational's
	// planner has no de-duplication operator wired into the union path,
	// so plain UNION (implicit DISTINCT) is rejected outright. Per
	// project conformance principle, Go aligns at parse time. Aligned
	// .
	q := setQ.GetQuantifier()
	if q == nil || strings.ToUpper(q.GetText()) != "ALL" {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"only UNION ALL is supported")
	}
	leftCols, leftColTypes, leftRows, err := c.execQueryBodyRows(ctx, setQ.GetLeft())
	if err != nil {
		return nil, err
	}

	var unionOrder []orderByClause
	var unionLimit int64 = -1
	var unionOffset int64 = 0
	var rightCols []string
	var rightRows [][]driver.Value
	if rb, ok := setQ.GetRight().(*antlrgen.QueryTermDefaultContext); ok {
		// Run the right side with ORDER BY / LIMIT / OFFSET stripped so those
		// clauses apply post-union. Leaving LIMIT in place on the right side
		// would truncate before dedup/concat and produce wrong results for
		// queries like `... UNION ... LIMIT 5`.
		rsq, parseErr := extractFromQueryTerm(rb)
		if parseErr != nil {
			return nil, parseErr
		}
		unionOrder = rsq.orderBy
		rsq.orderBy = nil
		unionLimit = rsq.limit
		rsq.limit = -1
		unionOffset = rsq.offset
		rsq.offset = 0
		rows, rErr := c.execSelectQuery(ctx, rsq)
		if rErr != nil {
			return nil, rErr
		}
		sr := rows.(*staticRows)
		rightCols, rightRows = sr.cols, sr.rows
	} else {
		rightCols, _, rightRows, err = c.execQueryBodyRows(ctx, setQ.GetRight())
		if err != nil {
			return nil, err
		}
	}

	// SQL standard: UNION sides must have matching column counts; names
	// are positional (left's names become the result schema). Java's
	// union.yamsql errors 42F64 (UNION_INCORRECT_COLUMN_COUNT) on
	// arity mismatch. Only UNION ALL reaches here (DISTINCT was
	// rejected at entry).
	if len(leftCols) != len(rightCols) {
		return nil, api.NewErrorf(api.ErrCodeUnionIncorrectColumnCount,
			"UNION ALL column count mismatch: left has %d columns, right has %d",
			len(leftCols), len(rightCols))
	}

	// Java's union.yamsql errors 42F65 UNION_INCOMPATIBLE_COLUMNS when a
	// positional column pair has non-unifiable types. Best-effort runtime
	// check: sample the first non-NULL value from each side per column
	// and require them to be comparable (numeric pairs are fine, same
	// concrete type is fine; anything else errors). When one side has
	// all NULLs for a column we skip that column — can't infer a type
	// without schema-typed columns.
	for ci := 0; ci < len(leftCols); ci++ {
		var lSample, rSample driver.Value
		for _, row := range leftRows {
			if ci < len(row) && row[ci] != nil {
				lSample = row[ci]
				break
			}
		}
		for _, row := range rightRows {
			if ci < len(row) && row[ci] != nil {
				rSample = row[ci]
				break
			}
		}
		if lSample == nil || rSample == nil {
			continue
		}
		if !valuesComparable(lSample, rSample) {
			return nil, api.NewErrorf(api.ErrCodeUnionIncompatibleColumns,
				"UNION column %d has incompatible types: left is %T, right is %T",
				ci+1, lSample, rSample)
		}
	}

	combined := append(leftRows, rightRows...) //nolint:gocritic

	// Apply union-level ORDER BY against the result schema (leftCols by position).
	if len(unionOrder) > 0 {
		colIdx := make(map[string]int, len(leftCols))
		for i, name := range leftCols {
			// Case-insensitive lookup to match the standard SELECT-list /
			// ORDER BY semantics the single-source path uses.
			colIdx[strings.ToLower(name)] = i
		}
		// Resolve each ORDER BY entry to a column index. Expression-based
		// ORDER BY is not supported at the union level — the combined row
		// set has no backing map/message to evaluate against.
		indices := make([]int, len(unionOrder))
		for i, ob := range unionOrder {
			if ob.expr != nil {
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
					"ORDER BY expression not supported on UNION result; use a column name from the left SELECT list")
			}
			idx, ok := colIdx[strings.ToLower(ob.colName)]
			if !ok {
				return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
					"ORDER BY column %q not found in UNION result schema", ob.colName)
			}
			indices[i] = idx
		}
		sort.SliceStable(combined, func(a, b int) bool {
			for k, idx := range indices {
				less, equal := orderByLess(combined[a][idx], combined[b][idx], unionOrder[k])
				if equal {
					continue
				}
				return less
			}
			return false
		})
	}

	// Apply union-level OFFSET / LIMIT.
	if unionOffset > 0 {
		if unionOffset >= int64(len(combined)) {
			combined = combined[:0]
		} else {
			combined = combined[unionOffset:]
		}
	}
	if unionLimit >= 0 && int64(len(combined)) > unionLimit {
		combined = combined[:unionLimit]
	}

	return &staticRows{cols: leftCols, colTypes: leftColTypes, rows: combined}, nil
}

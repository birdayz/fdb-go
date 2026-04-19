// Package embedded implements the embedded (in-process) SQL execution engine
// for the FoundationDB relational layer.
//
// EmbeddedConnection is the Go equivalent of Java's EmbeddedRelationalConnection.
// It parses SQL, routes DDL statements through the MetadataOperationsFactory,
// and (eventually) routes DML through the query planner.
package embedded

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	apiddl "github.com/birdayz/fdb-record-layer-go/pkg/relational/api/ddl"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/catalog"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/keyspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// cteData holds the materialized result of a WITH clause named query.
type cteData struct {
	cols []string
	rows [][]driver.Value
}

// EmbeddedConnection is an in-process SQL connection backed by FDB.
//
// Implements driver.Conn and driver.ExecerContext so DDL statements can
// execute without a Prepare round-trip.
//
// Transaction model:
//
//	Auto-commit: every statement runs in its own FDB transaction via fdbDB.Run().
//	Explicit transaction: BeginTx opens an FDB transaction; all statements in
//	the transaction share it. Commit/Rollback close it.
type EmbeddedConnection struct {
	dbPath        string // current database URI (e.g. "/mydb")
	schema        string // current schema name (set via USE SCHEMA / SetSchema)
	defaultSchema string // schema set at connection creation time; restored on ResetSession
	fdbDB         *recordlayer.FDBDatabase
	cat           *catalog.RecordLayerStoreCatalog
	ks            *keyspace.RelationalKeyspace
	factory       apiddl.MetadataOperationsFactory
	closed        atomic.Bool

	// activeTx is non-nil when an explicit transaction is open (BeginTx called).
	// nil means auto-commit mode.
	activeTx *embeddedTx

	// schemaCache is a connection-level cache: (dbPath+"\x00"+schemaName) → api.Schema.
	// Safe because driver connections are single-goroutine. Invalidated by DDL.
	schemaCache map[string]api.Schema

	// ctes holds materialized CTE results for the current SELECT statement.
	// Non-nil only during execSelect; nil outside of that scope.
	ctes map[string]*cteData

	// catalogReady is set to true after the first successful catalog init.
	// Protected by catalogMu so transient failures can be retried.
	catalogMu    sync.Mutex
	catalogReady bool
}

// embeddedTx is the driver.Tx returned by BeginTx. It holds the open FDB
// record context for the duration of the explicit transaction.
type embeddedTx struct {
	conn *EmbeddedConnection
	rctx *recordlayer.FDBRecordContext
}

// Commit runs pre-commit hooks, flushes version mutations, commits the FDB
// transaction, runs post-commit hooks, and clears the connection's activeTx.
func (tx *embeddedTx) Commit() error {
	err := tx.rctx.CommitWithHooks()
	tx.conn.activeTx = nil
	return err
}

// Rollback cancels the FDB transaction and clears the connection's activeTx.
func (tx *embeddedTx) Rollback() error {
	tx.conn.activeTx = nil
	tx.rctx.Cancel()
	return nil
}

// runInTx executes fn either inside the open explicit transaction (if one
// exists) or inside a new auto-commit transaction via fdbDB.Run.
// In explicit-transaction mode, fn errors propagate without retry.
func (c *EmbeddedConnection) runInTx(ctx context.Context, fn func(*recordlayer.FDBRecordContext) (any, error)) (any, error) {
	if c.activeTx != nil {
		return fn(c.activeTx.rctx)
	}
	return c.fdbDB.Run(ctx, fn)
}

func (c *EmbeddedConnection) schemaCacheKey(dbPath, schemaName string) string {
	return dbPath + "\x00" + schemaName
}

// cachedLoadSchema returns the api.Schema for (dbPath, schemaName), using the
// connection-level cache to avoid repeated FDB reads within the same session.
// The cache is invalidated by any DDL that modifies schema definitions.
//
// When an explicit user transaction is active we read the catalog via a
// separate auto-commit transaction so that catalog reads do not add read
// conflict ranges to the user's write transaction, which would cause spurious
// not_committed (1020) conflicts when other tests run DDL concurrently.
func (c *EmbeddedConnection) cachedLoadSchema(txn api.Transaction, dbPath, schemaName string) (api.Schema, error) {
	key := c.schemaCacheKey(dbPath, schemaName)
	if s, ok := c.schemaCache[key]; ok {
		return s, nil
	}
	var s api.Schema
	var err error
	if c.activeTx != nil {
		// Read catalog outside the user transaction to avoid adding catalog
		// read-conflict ranges that conflict with concurrent DDL.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = c.fdbDB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
			readTxn := catalog.NewFDBTransaction(rctx)
			s, err = c.cat.LoadSchema(readTxn, dbPath, schemaName)
			return nil, err
		})
	} else {
		s, err = c.cat.LoadSchema(txn, dbPath, schemaName)
	}
	if err != nil {
		return nil, err
	}
	c.schemaCache[key] = s
	return s, nil
}

func (c *EmbeddedConnection) invalidateSchemaCache(dbPath, schemaName string) {
	delete(c.schemaCache, c.schemaCacheKey(dbPath, schemaName))
}

// New returns a ready-to-use embedded connection.
func New(
	dbPath string,
	fdbDB *recordlayer.FDBDatabase,
	cat *catalog.RecordLayerStoreCatalog,
	factory apiddl.MetadataOperationsFactory,
	ks *keyspace.RelationalKeyspace,
) *EmbeddedConnection {
	return &EmbeddedConnection{
		dbPath:      dbPath,
		fdbDB:       fdbDB,
		cat:         cat,
		factory:     factory,
		ks:          ks,
		schemaCache: make(map[string]api.Schema),
	}
}

// ExecContext executes SQL (DDL only in phase 1) and returns the result.
// Implements driver.ExecerContext so database/sql skips the Prepare round-trip.
func (c *EmbeddedConnection) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.closed.Load() {
		return nil, driver.ErrBadConn
	}
	var err error
	if query, err = substituteParams(query, args); err != nil {
		return nil, err
	}
	root, err := parser.Parse(query)
	if err != nil {
		return nil, err
	}
	stmts := root.Statements()
	if stmts == nil || len(stmts.AllStatement()) == 0 {
		return driver.RowsAffected(0), nil
	}
	var totalRows int64
	for _, stmt := range stmts.AllStatement() {
		rows, execErr := c.execStatement(ctx, stmt)
		if execErr != nil {
			return nil, execErr
		}
		totalRows += rows
	}
	return driver.RowsAffected(totalRows), nil
}

// QueryContext handles read-only queries. Supports SHOW statements and
// SELECT * FROM <table>; all other queries return ErrCodeUnsupportedOperation.
func (c *EmbeddedConnection) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.closed.Load() {
		return nil, driver.ErrBadConn
	}
	var subErr error
	if query, subErr = substituteParams(query, args); subErr != nil {
		return nil, subErr
	}
	if err := c.ensureCatalogInit(ctx); err != nil {
		return nil, err
	}
	root, err := parser.Parse(query)
	if err != nil {
		return nil, err
	}
	stmts := root.Statements()
	if stmts == nil || len(stmts.AllStatement()) == 0 {
		return emptyRows{}, nil
	}
	if len(stmts.AllStatement()) > 1 {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "multi-statement queries are not supported")
	}
	stmt := stmts.AllStatement()[0]

	// Route SELECT statements.
	if sel := stmt.SelectStatement(); sel != nil {
		return c.execSelect(ctx, sel)
	}

	admin := stmt.AdministrationStatement()
	if admin == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "only SHOW and SELECT statements are supported")
	}
	show := admin.ShowStatement()
	if show == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "only SHOW statements are supported")
	}
	return c.execShowStatement(ctx, show)
}

// execQueryBodyRows executes a queryExpressionBody and returns (colNames, rows).
// Handles both simple queries (QueryTermDefaultContext) and UNION (SetQueryContext).
func (c *EmbeddedConnection) execQueryBodyRows(ctx context.Context, body antlrgen.IQueryExpressionBodyContext) ([]string, [][]driver.Value, error) {
	switch b := body.(type) {
	case *antlrgen.QueryTermDefaultContext:
		sq, err := extractFromQueryTerm(b)
		if err != nil {
			return nil, nil, err
		}
		rows, err := c.execSelectQuery(ctx, sq)
		if err != nil {
			return nil, nil, err
		}
		sr := rows.(*staticRows)
		return sr.cols, sr.rows, nil
	case *antlrgen.SetQueryContext:
		r, err := c.execUnion(ctx, b)
		if err != nil {
			return nil, nil, err
		}
		sr := r.(*staticRows)
		return sr.cols, sr.rows, nil
	default:
		return nil, nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported query expression type %T", body)
	}
}

// execUnion executes a UNION ALL / UNION DISTINCT query.
func (c *EmbeddedConnection) execUnion(ctx context.Context, setQ *antlrgen.SetQueryContext) (driver.Rows, error) {
	leftCols, leftRows, err := c.execQueryBodyRows(ctx, setQ.GetLeft())
	if err != nil {
		return nil, err
	}
	_, rightRows, err := c.execQueryBodyRows(ctx, setQ.GetRight())
	if err != nil {
		return nil, err
	}

	combined := append(leftRows, rightRows...) //nolint:gocritic

	quantifier := ""
	if q := setQ.GetQuantifier(); q != nil {
		quantifier = strings.ToUpper(q.GetText())
	}
	if quantifier != "ALL" {
		// UNION (implicit DISTINCT) — deduplicate.
		seen := make(map[string]struct{}, len(combined))
		deduped := combined[:0]
		for _, row := range combined {
			key := rowKey(row)
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				deduped = append(deduped, row)
			}
		}
		combined = deduped
	}

	return &staticRows{cols: leftCols, rows: combined}, nil
}

// execSelectQuery executes a parsed selectQuery and returns a driver.Rows.
// Extracted so execQueryBodyRows can call it without an ISelectStatementContext.
func (c *EmbeddedConnection) execSelectQuery(ctx context.Context, sq *selectQuery) (driver.Rows, error) {
	// SELECT without FROM: evaluate projExprs as constants and return one row.
	if sq.tableName == "" {
		cols := make([]string, len(sq.projCols))
		row := make([]driver.Value, len(sq.projCols))
		for i, col := range sq.projCols {
			name := sq.projAliases[i]
			if name == "" {
				name = col
			}
			cols[i] = name
			if sq.projExprs[i] != nil {
				v, err := evalExpr(ctx, c, nil, sq.projExprs[i])
				if err != nil {
					return nil, err
				}
				row[i] = v
			}
		}
		return &staticRows{cols: cols, rows: [][]driver.Value{row}}, nil
	}

	// Execute derived table query and register it as a temporary CTE.
	if sq.derivedQuery != nil {
		cols, rows, err := c.execQueryBodyRows(ctx, sq.derivedQuery.QueryExpressionBody())
		if err != nil {
			return nil, fmt.Errorf("derived table %q: %w", sq.tableName, err)
		}
		if c.ctes == nil {
			c.ctes = make(map[string]*cteData)
			defer func() { c.ctes = nil }()
		}
		c.ctes[strings.ToUpper(sq.tableName)] = &cteData{cols: cols, rows: rows}
	}

	// Check if the table name resolves to a CTE.
	if c.ctes != nil {
		if cte, ok := c.ctes[strings.ToUpper(sq.tableName)]; ok {
			return c.execSelectFromCTE(ctx, sq, cte)
		}
	}

	// Route INFORMATION_SCHEMA.* queries to system table handlers.
	upper := strings.ToUpper(sq.tableName)
	if strings.HasPrefix(upper, "INFORMATION_SCHEMA.") {
		sysTable := upper[len("INFORMATION_SCHEMA."):]
		return c.execSystemTable(ctx, sysTable, sq.whereExpr)
	}

	if c.schema == "" {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.dbPath == "" {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}
	// Delegate to the existing full implementation.
	return c.execSelectQueryFull(ctx, sq)
}

// execSelect executes a SELECT statement. Supports single-table and multi-table
// (INNER/LEFT JOIN) queries, WHERE, ORDER BY, GROUP BY, HAVING, LIMIT/OFFSET,
// aggregate functions, and INFORMATION_SCHEMA system tables.
func (c *EmbeddedConnection) execSelect(ctx context.Context, sel antlrgen.ISelectStatementContext) (driver.Rows, error) {
	query := sel.Query()
	if query == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "malformed SELECT statement")
	}

	// Materialize CTEs before routing the main query.
	if ctesCtx := query.Ctes(); ctesCtx != nil {
		c.ctes = make(map[string]*cteData)
		defer func() { c.ctes = nil }()
		for _, nq := range ctesCtx.AllNamedQuery() {
			cteName := strings.ToUpper(fullIdToName(nq.GetName()))
			cteCols, cteRows, cteErr := c.execQueryBodyRows(ctx, nq.Query().QueryExpressionBody())
			if cteErr != nil {
				return nil, fmt.Errorf("CTE %q: %w", cteName, cteErr)
			}
			c.ctes[cteName] = &cteData{cols: cteCols, rows: cteRows}
		}
	}

	if setQ, ok := query.QueryExpressionBody().(*antlrgen.SetQueryContext); ok {
		return c.execUnion(ctx, setQ)
	}
	sq, err := extractSelectParts(sel)
	if err != nil {
		return nil, err
	}
	return c.execSelectQuery(ctx, sq)
}

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
				v = protoValueToDriver(fd, msgRef.Get(fd))
			}
			m[col] = v
			m[alias+"."+col] = v
		}
		rows = append(rows, m)
	}
	return rows, nil
}

// execSelectJoin executes a SELECT with one or more JOIN clauses.
// Supports INNER JOIN and LEFT OUTER JOIN using nested-loop join.
// aggregateMapRows applies GROUP BY + aggregate computation to a slice of map rows
// (as produced by JOIN evaluation or CTE materialization). Returns the resulting
// output column names and tuple rows.
//
// Behavior:
//   - COUNT(*) (sq.countStar, no aggCols): returns [["COUNT(*)"]] with a single row
//     holding int64(len(filtered)). NOTE: parser sets countStar only when the
//     entire SELECT list is a bare COUNT(*); with GROUP BY, COUNT(*) flows through
//     sq.aggCols instead.
//   - Otherwise: computes per-group aggregates (COUNT, COUNT(DISTINCT col), SUM,
//     MIN, MAX, AVG), emits one row per group (or a single synthetic group when
//     sq.groupBy is empty), optionally filtered by sq.havingExpr.
func (c *EmbeddedConnection) aggregateMapRows(ctx context.Context, sq *selectQuery, filtered []map[string]driver.Value) (cols []string, data [][]driver.Value, err error) {
	if sq.countStar {
		return []string{"COUNT(*)"}, [][]driver.Value{{int64(len(filtered))}}, nil
	}

	type mapGroupState struct {
		groupVals    []driver.Value
		counts       []int64
		sums         []float64
		mins         []driver.Value
		maxes        []driver.Value
		avgs         []float64
		avgsN        []int64
		distinctSets []map[string]struct{}
	}
	groupOrder := []string{}
	groups := map[string]*mapGroupState{}
	hasGroups := len(sq.groupBy) > 0
	for _, row := range filtered {
		gVals := make([]driver.Value, len(sq.groupBy))
		for gi, gcol := range sq.groupBy {
			if v, ok := row[gcol]; ok {
				gVals[gi] = v
			} else if dot := strings.LastIndex(gcol, "."); dot >= 0 {
				gVals[gi] = row[gcol[dot+1:]]
			}
		}
		key := groupByKey(gVals)
		if !hasGroups {
			key = ""
		}
		gs, exists := groups[key]
		if !exists {
			dsets := make([]map[string]struct{}, len(sq.aggCols))
			for di, ac := range sq.aggCols {
				if ac.aggDistinct {
					dsets[di] = make(map[string]struct{})
				}
			}
			gs = &mapGroupState{
				groupVals:    gVals,
				counts:       make([]int64, len(sq.aggCols)),
				sums:         make([]float64, len(sq.aggCols)),
				mins:         make([]driver.Value, len(sq.aggCols)),
				maxes:        make([]driver.Value, len(sq.aggCols)),
				avgs:         make([]float64, len(sq.aggCols)),
				avgsN:        make([]int64, len(sq.aggCols)),
				distinctSets: dsets,
			}
			groups[key] = gs
			groupOrder = append(groupOrder, key)
		}
		for i, ac := range sq.aggCols {
			if ac.groupCol != "" {
				continue
			}
			colVal := row[ac.aggArg]
			if colVal == nil && ac.aggArg != "" {
				if dot := strings.LastIndex(ac.aggArg, "."); dot >= 0 {
					colVal = row[ac.aggArg[dot+1:]]
				}
			}
			if ac.aggDistinct && ac.aggArg != "" {
				if colVal != nil {
					dk := fmt.Sprintf("%v", colVal)
					if _, seen := gs.distinctSets[i][dk]; !seen {
						gs.distinctSets[i][dk] = struct{}{}
						gs.counts[i]++
					}
				}
				continue
			}
			gs.counts[i]++
			if colVal != nil {
				fv, _ := toFloat64(colVal)
				switch ac.aggFunc {
				case "SUM":
					gs.sums[i] += fv
				case "MIN":
					if gs.mins[i] == nil || compareValues(colVal, gs.mins[i]) < 0 {
						gs.mins[i] = colVal
					}
				case "MAX":
					if gs.maxes[i] == nil || compareValues(colVal, gs.maxes[i]) > 0 {
						gs.maxes[i] = colVal
					}
				case "AVG":
					gs.avgs[i] += fv
					gs.avgsN[i]++
				}
			}
		}
	}
	groupColIdx := map[string]int{}
	for i, col := range sq.groupBy {
		groupColIdx[col] = i
	}
	cols = make([]string, len(sq.aggCols))
	for i, ac := range sq.aggCols {
		cols[i] = ac.outName
	}
	for _, key := range groupOrder {
		gs := groups[key]
		rowVals := make([]driver.Value, len(sq.aggCols))
		rowMap := make(map[string]driver.Value, len(sq.aggCols))
		for i, ac := range sq.aggCols {
			if ac.groupCol != "" {
				idx, ok := groupColIdx[ac.groupCol]
				if ok {
					rowVals[i] = gs.groupVals[idx]
				}
			} else {
				switch ac.aggFunc {
				case "COUNT":
					rowVals[i] = gs.counts[i]
				case "SUM":
					rowVals[i] = gs.sums[i]
				case "MIN":
					rowVals[i] = gs.mins[i]
				case "MAX":
					rowVals[i] = gs.maxes[i]
				case "AVG":
					if gs.avgsN[i] > 0 {
						rowVals[i] = gs.avgs[i] / float64(gs.avgsN[i])
					}
				}
			}
			rowMap[ac.outName] = rowVals[i]
		}
		if sq.havingExpr != nil {
			ok, hErr := evalHaving(ctx, c, rowMap, sq.havingExpr)
			if hErr != nil {
				return nil, nil, hErr
			}
			if !ok {
				continue
			}
		}
		data = append(data, rowVals)
	}
	return cols, data, nil
}

func (c *EmbeddedConnection) execSelectJoin(ctx context.Context, sq *selectQuery) (driver.Rows, error) {
	var cols []string
	var data [][]driver.Value

	_, runErr := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil
		cols = nil
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.dbPath, c.schema)
		if loadErr != nil {
			return nil, loadErr
		}
		rlTmpl, tmplOk := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
		if !tmplOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "schema template is not a RecordLayerSchemaTemplate")
		}
		md := rlTmpl.Underlying()
		ss, ssErr := c.ks.SchemaSubspace(c.dbPath, c.schema)
		if ssErr != nil {
			return nil, ssErr
		}
		store, storeErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(md).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		// Scan left table.
		leftRows, leftErr := c.scanTableToMaps(ctx, store, sq.tableName, sq.tableAlias)
		if leftErr != nil {
			return nil, leftErr
		}

		// Scan each joined table; apply nested-loop join.
		joined := leftRows
		for _, jc := range sq.joins {
			rightRows, rightErr := c.scanTableToMaps(ctx, store, jc.tableName, jc.alias)
			if rightErr != nil {
				return nil, rightErr
			}
			var next []map[string]driver.Value
			for _, left := range joined {
				matched := false
				for _, right := range rightRows {
					// Merge rows into combined map.
					combined := make(map[string]driver.Value, len(left)+len(right))
					for k, v := range left {
						combined[k] = v
					}
					for k, v := range right {
						combined[k] = v
					}
					// Evaluate ON condition.
					if jc.onExpr != nil {
						ok, onErr := evalPredicateOnMapExpr(ctx, c, combined, jc.onExpr)
						if onErr != nil {
							return nil, onErr
						}
						if !ok {
							continue
						}
					}
					matched = true
					next = append(next, combined)
				}
				// LEFT JOIN: emit left row with NULLs if no right match.
				if jc.joinType == "LEFT" && !matched {
					// Build null right side.
					nullRight := make(map[string]driver.Value, len(left)+10)
					for k, v := range left {
						nullRight[k] = v
					}
					next = append(next, nullRight)
				}
			}
			// RIGHT JOIN: also emit right rows that had no left match (null left side).
			if jc.joinType == "RIGHT" {
				matchedRight := make([]bool, len(rightRows))
				for _, left := range joined {
					for ri, right := range rightRows {
						combined := make(map[string]driver.Value, len(left)+len(right))
						for k, v := range left {
							combined[k] = v
						}
						for k, v := range right {
							combined[k] = v
						}
						if jc.onExpr != nil {
							ok, onErr := evalPredicateOnMapExpr(ctx, c, combined, jc.onExpr)
							if onErr != nil {
								return nil, onErr
							}
							if ok {
								matchedRight[ri] = true
							}
						} else {
							matchedRight[ri] = true
						}
					}
				}
				for ri, right := range rightRows {
					if !matchedRight[ri] {
						combined := make(map[string]driver.Value, len(right)+10)
						for k, v := range right {
							combined[k] = v
						}
						next = append(next, combined)
					}
				}
			}
			joined = next
		}

		// Apply WHERE filter using map-based evaluation.
		var filtered []map[string]driver.Value
		for _, row := range joined {
			if sq.whereExpr == nil {
				filtered = append(filtered, row)
				continue
			}
			ok, wErr := evalPredicateOnMapExpr(ctx, c, row, sq.whereExpr.Expression())
			if wErr != nil {
				return nil, wErr
			}
			if ok {
				filtered = append(filtered, row)
			}
		}

		// GROUP BY + aggregate on map rows (for JOIN queries).
		// Aggregated results fall through to ORDER BY/LIMIT/OFFSET below;
		// the normal column-selection and row-building blocks are skipped.
		isAggregate := sq.countStar || len(sq.aggCols) > 0
		if isAggregate {
			aggCols, aggData, aggErr := c.aggregateMapRows(ctx, sq, filtered)
			if aggErr != nil {
				return nil, aggErr
			}
			cols = aggCols
			data = aggData
		} else {
			// Determine output columns.
			// For SELECT *, collect all unique unqualified column names in order.
			if sq.projCols == nil {
				seen := make(map[string]bool)
				// Order: left table columns first, then join table columns.
				leftRt := md.GetRecordType(sq.tableName)
				if leftRt != nil {
					for i := 0; i < leftRt.Descriptor.Fields().Len(); i++ {
						name := string(leftRt.Descriptor.Fields().Get(i).Name())
						if !seen[name] {
							cols = append(cols, name)
							seen[name] = true
						}
					}
				}
				for _, jc := range sq.joins {
					jRt := md.GetRecordType(jc.tableName)
					if jRt != nil {
						for i := 0; i < jRt.Descriptor.Fields().Len(); i++ {
							name := string(jRt.Descriptor.Fields().Get(i).Name())
							if !seen[name] {
								cols = append(cols, name)
								seen[name] = true
							}
						}
					}
				}
			} else {
				cols = make([]string, len(sq.projCols))
				for i, c := range sq.projCols {
					out := c
					if i < len(sq.projAliases) && sq.projAliases[i] != "" {
						out = sq.projAliases[i]
					}
					cols[i] = out
				}
			}

			// Build output rows.
			for _, row := range filtered {
				var vals []driver.Value
				if sq.projCols == nil {
					// SELECT * — use cols order.
					vals = make([]driver.Value, len(cols))
					for i, col := range cols {
						vals[i] = row[col]
					}
				} else {
					vals = make([]driver.Value, len(sq.projCols))
					for i, col := range sq.projCols {
						// Try qualified first, then unqualified.
						if v, ok := row[col]; ok {
							vals[i] = v
						} else {
							// Strip table prefix: "a.id" → try "id" in map.
							if dot := strings.LastIndex(col, "."); dot >= 0 {
								vals[i] = row[col[dot+1:]]
							}
						}
					}
				}
				data = append(data, vals)
			}
		}

		// ORDER BY. For aggregate results, `filtered` and `data` diverge — the
		// colName path handles that. For non-aggregate rows, data[i] matches
		// filtered[i] in lockstep, so `ob.expr` can be evaluated against
		// filtered[i] for arbitrary-expression sort keys.
		if len(sq.orderBy) > 0 {
			colIdx := make(map[string]int, len(cols))
			for i, c := range cols {
				colIdx[c] = i
			}
			// Pre-compute sort keys for expression order-by to avoid redundant
			// evaluation inside the comparator.
			hasExpr := false
			for _, ob := range sq.orderBy {
				if ob.expr != nil {
					hasExpr = true
					break
				}
			}
			var keys [][]driver.Value
			if hasExpr && len(filtered) == len(data) {
				keys = make([][]driver.Value, len(data))
				for i := range data {
					keys[i] = make([]driver.Value, len(sq.orderBy))
					for oi, ob := range sq.orderBy {
						if ob.expr != nil {
							v, evalErr := evalExprOnMap(ctx, c, filtered[i], ob.expr)
							if evalErr != nil {
								return nil, evalErr
							}
							keys[i][oi] = v
						}
					}
				}
			}
			indexes := make([]int, len(data))
			for i := range indexes {
				indexes[i] = i
			}
			sort.SliceStable(indexes, func(ii, jj int) bool {
				i, j := indexes[ii], indexes[jj]
				for oi, ob := range sq.orderBy {
					var cmp int
					if ob.expr != nil && keys != nil {
						cmp = compareValues(keys[i][oi], keys[j][oi])
					} else {
						idx, ok := colIdx[ob.colName]
						if !ok {
							continue
						}
						cmp = compareValues(data[i][idx], data[j][idx])
					}
					if cmp != 0 {
						if ob.ascending {
							return cmp < 0
						}
						return cmp > 0
					}
				}
				return false
			})
			sorted := make([][]driver.Value, len(data))
			for nn, oldIdx := range indexes {
				sorted[nn] = data[oldIdx]
			}
			data = sorted
		}

		// LIMIT / OFFSET.
		if sq.offset > 0 && int(sq.offset) < len(data) {
			data = data[sq.offset:]
		} else if sq.offset > 0 {
			data = nil
		}
		if sq.limit >= 0 && int(sq.limit) < len(data) {
			data = data[:sq.limit]
		}

		return nil, nil
	})
	if runErr != nil {
		return nil, runErr
	}
	return &staticRows{cols: cols, rows: data}, nil
}

// execSelectFromCTE executes a SELECT against a materialized CTE result set.
// Supports WHERE, projected columns, ORDER BY, LIMIT, and OFFSET.
// Aggregate queries and JOINs against CTEs are not yet supported.
func (c *EmbeddedConnection) execSelectFromCTE(ctx context.Context, sq *selectQuery, cte *cteData) (driver.Rows, error) {
	alias := sq.tableAlias
	if alias == "" {
		alias = sq.tableName
	}

	// Build map rows.
	mapRows := cteRowsToMaps(cte, alias)

	// Apply WHERE filter.
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
	var outRows [][]driver.Value

	if len(sq.aggCols) > 0 || sq.countStar {
		aggCols, aggData, aggErr := c.aggregateMapRows(ctx, sq, mapRows)
		if aggErr != nil {
			return nil, aggErr
		}
		colNames = aggCols
		outRows = aggData
	} else if sq.projCols == nil {
		// SELECT * — emit all CTE columns in definition order.
		colNames = cte.cols
		for _, row := range mapRows {
			outRow := make([]driver.Value, len(cte.cols))
			for j, col := range cte.cols {
				outRow[j] = row[col]
			}
			outRows = append(outRows, outRow)
		}
	} else if sq.projCols != nil {
		colNames = make([]string, len(sq.projCols))
		for j, col := range sq.projCols {
			if j < len(sq.projAliases) && sq.projAliases[j] != "" {
				colNames[j] = sq.projAliases[j]
			} else {
				colNames[j] = col
			}
		}
		for _, row := range mapRows {
			outRow := make([]driver.Value, len(sq.projCols))
			for j, col := range sq.projCols {
				if j < len(sq.projExprs) && sq.projExprs[j] != nil {
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
		if hasExpr && len(mapRows) == len(outRows) {
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
		sort.SliceStable(indexes, func(ii, jj int) bool {
			i, j := indexes[ii], indexes[jj]
			for oi, ob := range sq.orderBy {
				var cmp int
				if ob.expr != nil && keys != nil {
					cmp = compareValues(keys[i][oi], keys[j][oi])
				} else {
					idx, ok := colIdx[ob.colName]
					if !ok {
						continue
					}
					cmp = compareValues(outRows[i][idx], outRows[j][idx])
				}
				if cmp != 0 {
					if ob.ascending {
						return cmp < 0
					}
					return cmp > 0
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

	return &staticRows{cols: colNames, rows: outRows}, nil
}

func (c *EmbeddedConnection) execSelectQueryFull(ctx context.Context, sq *selectQuery) (driver.Rows, error) {
	if len(sq.joins) > 0 {
		return c.execSelectJoin(ctx, sq)
	}

	type row = []driver.Value
	type outField struct {
		name string
		fd   protoreflect.FieldDescriptor
	}
	var cols []string
	var data []row
	var extraSortFields []outField

	_, runErr := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil // reset on retry so duplicate rows aren't appended
		cols = nil
		extraSortFields = nil
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.dbPath, c.schema)
		if loadErr != nil {
			return nil, loadErr
		}
		rlTmpl, tmplOk := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
		if !tmplOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "schema template is not a RecordLayerSchemaTemplate")
		}
		md := rlTmpl.Underlying()

		rt := md.GetRecordType(sq.tableName)
		if rt == nil {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "table %q not found in schema", sq.tableName)
		}
		msgDesc := rt.Descriptor

		ss, ssErr := c.ks.SchemaSubspace(c.dbPath, c.schema)
		if ssErr != nil {
			return nil, ssErr
		}
		store, storeErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(md).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		cursor := store.ScanRecordsByType(sq.tableName, nil, recordlayer.ForwardScan())
		defer cursor.Close() //nolint:errcheck

		if sq.countStar {
			cols = []string{"COUNT(*)"}
			var count int64
			for {
				result, nextErr := cursor.OnNext(ctx)
				if nextErr != nil {
					return nil, nextErr
				}
				if !result.HasNext() {
					break
				}
				match, matchErr := evalPredicate(ctx, c, result.GetValue().Record, sq.whereExpr)
				if matchErr != nil {
					return nil, matchErr
				}
				if match {
					count++
				}
			}
			data = []row{{count}}
			return nil, nil
		}

		// GROUP BY aggregate query: scan → group → aggregate.
		if len(sq.aggCols) > 0 {
			// Resolve group-by field descriptors.
			groupFDs := make([]protoreflect.FieldDescriptor, len(sq.groupBy))
			for i, col := range sq.groupBy {
				fd := msgDesc.Fields().ByName(protoreflect.Name(col))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
						"GROUP BY column %q not found in table %q", col, sq.tableName)
				}
				groupFDs[i] = fd
			}
			// Resolve aggregate arg field descriptors (nil for COUNT(*)).
			aggArgFDs := make([]protoreflect.FieldDescriptor, len(sq.aggCols))
			for i, ac := range sq.aggCols {
				if ac.groupCol != "" {
					fd := msgDesc.Fields().ByName(protoreflect.Name(ac.groupCol))
					if fd == nil {
						return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
							"column %q not found in table %q", ac.groupCol, sq.tableName)
					}
					aggArgFDs[i] = fd
				} else if ac.aggArg != "" {
					fd := msgDesc.Fields().ByName(protoreflect.Name(ac.aggArg))
					if fd == nil {
						return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
							"aggregate column %q not found in table %q", ac.aggArg, sq.tableName)
					}
					aggArgFDs[i] = fd
				}
			}

			type groupState struct {
				groupVals []driver.Value // values for the group-by columns
				// accumulators parallel to sq.aggCols
				counts       []int64
				sums         []float64
				mins         []driver.Value
				maxes        []driver.Value
				avgs         []float64             // running sum for AVG
				avgsN        []int64               // count for AVG
				distinctSets []map[string]struct{} // nil unless COUNT(DISTINCT)
			}
			groupOrder := []string{} // insertion order for deterministic output
			groups := map[string]*groupState{}

			for {
				result, nextErr := cursor.OnNext(ctx)
				if nextErr != nil {
					return nil, nextErr
				}
				if !result.HasNext() {
					break
				}
				msg := result.GetValue().Record
				match, matchErr := evalPredicate(ctx, c, msg, sq.whereExpr)
				if matchErr != nil {
					return nil, matchErr
				}
				if !match {
					continue
				}

				// Build group-by key.
				gVals := make([]driver.Value, len(groupFDs))
				for i, fd := range groupFDs {
					if fd != nil && msg.ProtoReflect().Has(fd) {
						gVals[i] = protoValueToDriver(fd, msg.ProtoReflect().Get(fd))
					}
				}
				key := groupByKey(gVals)
				gs, exists := groups[key]
				if !exists {
					distinctSets := make([]map[string]struct{}, len(sq.aggCols))
					for di, ac := range sq.aggCols {
						if ac.aggDistinct {
							distinctSets[di] = make(map[string]struct{})
						}
					}
					gs = &groupState{
						groupVals:    gVals,
						counts:       make([]int64, len(sq.aggCols)),
						sums:         make([]float64, len(sq.aggCols)),
						mins:         make([]driver.Value, len(sq.aggCols)),
						maxes:        make([]driver.Value, len(sq.aggCols)),
						avgs:         make([]float64, len(sq.aggCols)),
						avgsN:        make([]int64, len(sq.aggCols)),
						distinctSets: distinctSets,
					}
					groups[key] = gs
					groupOrder = append(groupOrder, key)
				}
				// Update accumulators.
				for i, ac := range sq.aggCols {
					if ac.groupCol != "" {
						continue // group-by reference, no accumulation
					}
					if ac.aggDistinct && aggArgFDs[i] != nil {
						// COUNT(DISTINCT col): only count each distinct value once.
						if msg.ProtoReflect().Has(aggArgFDs[i]) {
							v := protoValueToDriver(aggArgFDs[i], msg.ProtoReflect().Get(aggArgFDs[i]))
							dk := fmt.Sprintf("%v", v)
							if _, seen := gs.distinctSets[i][dk]; !seen {
								gs.distinctSets[i][dk] = struct{}{}
								gs.counts[i]++
							}
						}
						continue
					}
					gs.counts[i]++
					if aggArgFDs[i] != nil && msg.ProtoReflect().Has(aggArgFDs[i]) {
						v := protoValueToDriver(aggArgFDs[i], msg.ProtoReflect().Get(aggArgFDs[i]))
						fv, _ := toFloat64(v)
						switch ac.aggFunc {
						case "SUM":
							gs.sums[i] += fv
						case "MIN":
							if gs.mins[i] == nil || compareValues(v, gs.mins[i]) < 0 {
								gs.mins[i] = v
							}
						case "MAX":
							if gs.maxes[i] == nil || compareValues(v, gs.maxes[i]) > 0 {
								gs.maxes[i] = v
							}
						case "AVG":
							gs.avgs[i] += fv
							gs.avgsN[i]++
						}
					}
				}
			}

			// Build output cols.
			groupColIdx := map[string]int{}
			for i, col := range sq.groupBy {
				groupColIdx[col] = i
			}
			cols = make([]string, len(sq.aggCols))
			for i, ac := range sq.aggCols {
				cols[i] = ac.outName
			}

			// Emit one row per group (with HAVING filter).
			for _, key := range groupOrder {
				gs := groups[key]
				rowVals := make([]driver.Value, len(sq.aggCols))
				rowMap := make(map[string]driver.Value, len(sq.aggCols))
				for i, ac := range sq.aggCols {
					if ac.groupCol != "" {
						idx, ok := groupColIdx[ac.groupCol]
						if ok {
							rowVals[i] = gs.groupVals[idx]
						}
					} else {
						switch ac.aggFunc {
						case "COUNT":
							rowVals[i] = gs.counts[i]
						case "SUM":
							rowVals[i] = gs.sums[i]
						case "MIN":
							rowVals[i] = gs.mins[i]
						case "MAX":
							rowVals[i] = gs.maxes[i]
						case "AVG":
							if gs.avgsN[i] > 0 {
								rowVals[i] = gs.avgs[i] / float64(gs.avgsN[i])
							}
						}
					}
					rowMap[ac.outName] = rowVals[i]
				}
				if sq.havingExpr != nil {
					keep, havErr := evalHaving(ctx, c, rowMap, sq.havingExpr)
					if havErr != nil {
						return nil, havErr
					}
					if !keep {
						continue
					}
				}
				data = append(data, rowVals)
			}
			return nil, nil
		}

		// Resolve output fields: either the explicit projection or all fields.
		allFields := msgDesc.Fields()
		var outFields []outField
		// extraSortFields (outer variable) are ORDER BY columns not in the projection.
		if sq.projCols == nil {
			// SELECT * — all fields in descriptor order.
			outFields = make([]outField, allFields.Len())
			for i := 0; i < allFields.Len(); i++ {
				fd := allFields.Get(i)
				outFields[i] = outField{string(fd.Name()), fd}
			}
		} else {
			// Named projection — look up each column, apply alias if present.
			outFields = make([]outField, len(sq.projCols))
			projByCol := make(map[string]bool, len(sq.projCols))
			for i, colName := range sq.projCols {
				// Computed expression: no field descriptor needed.
				if i < len(sq.projExprs) && sq.projExprs[i] != nil {
					outName := colName
					if i < len(sq.projAliases) && sq.projAliases[i] != "" {
						outName = sq.projAliases[i]
					}
					outFields[i] = outField{outName, nil}
					// Don't add to projByCol (computed cols can't be in ORDER BY as proto fields).
					continue
				}
				fd := allFields.ByName(protoreflect.Name(colName))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
						"column %q not found in table %q", colName, sq.tableName)
				}
				outName := colName
				if i < len(sq.projAliases) && sq.projAliases[i] != "" {
					outName = sq.projAliases[i]
				}
				outFields[i] = outField{outName, fd}
				projByCol[colName] = true
			}
			// Add any ORDER BY columns not already in the projection.
			for _, ob := range sq.orderBy {
				if projByCol[ob.colName] {
					continue
				}
				fd := allFields.ByName(protoreflect.Name(ob.colName))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
						"ORDER BY column %q not found in table %q", ob.colName, sq.tableName)
				}
				extraSortFields = append(extraSortFields, outField{ob.colName, fd})
				projByCol[ob.colName] = true // avoid duplicates
			}
		}
		// fullFields = projected + extra sort columns; output strips extra at end.
		fullFields := append(outFields, extraSortFields...) //nolint:gocritic
		cols = make([]string, len(outFields))
		for i, f := range outFields {
			cols[i] = f.name
		}

		for {
			result, nextErr := cursor.OnNext(ctx)
			if nextErr != nil {
				return nil, nextErr
			}
			if !result.HasNext() {
				break
			}
			rec := result.GetValue()
			msg := rec.Record
			match, matchErr := evalPredicate(ctx, c, msg, sq.whereExpr)
			if matchErr != nil {
				return nil, matchErr
			}
			if !match {
				continue
			}
			vals := make([]driver.Value, len(fullFields))
			for i, f := range fullFields {
				// Check for a computed expression at this position.
				if i < len(sq.projExprs) && sq.projExprs[i] != nil {
					v, evalErr := evalExpr(ctx, c, msg, sq.projExprs[i])
					if evalErr != nil {
						return nil, evalErr
					}
					if v != nil {
						vals[i] = v.(driver.Value) //nolint:forcetypeassert
					}
					continue
				}
				if msg.ProtoReflect().Has(f.fd) {
					vals[i] = protoValueToDriver(f.fd, msg.ProtoReflect().Get(f.fd))
				}
				// else nil (proto2 optional field absent → NULL)
			}
			data = append(data, vals)
		}
		return nil, nil
	})
	if runErr != nil {
		return nil, runErr
	}

	// Apply DISTINCT deduplication before sort.
	if sq.distinct && !sq.countStar {
		seen := make(map[string]struct{}, len(data))
		deduped := data[:0]
		for _, row := range data {
			key := rowKey(row)
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				deduped = append(deduped, row)
			}
		}
		data = deduped
	}

	// Apply ORDER BY (post-scan in-memory sort).
	if len(sq.orderBy) > 0 {
		for _, ob := range sq.orderBy {
			if ob.expr != nil {
				return nil, api.NewError(api.ErrCodeUnsupportedOperation,
					"ORDER BY on an expression is only supported in CTE / JOIN queries; use a column name or alias")
			}
		}
		// Build a map from column name to row index (covers projected + extra sort cols).
		colIdx := make(map[string]int, len(cols)+len(extraSortFields))
		for i, c := range cols {
			colIdx[c] = i
		}
		for i, f := range extraSortFields {
			colIdx[f.name] = len(cols) + i
		}
		sort.SliceStable(data, func(i, j int) bool {
			for _, ob := range sq.orderBy {
				idx, ok := colIdx[ob.colName]
				if !ok {
					// Column validated during scan setup; safe to skip.
					continue
				}
				a, b := data[i][idx], data[j][idx]
				cmp := compareValues(a, b)
				if cmp != 0 {
					if ob.ascending {
						return cmp < 0
					}
					return cmp > 0
				}
			}
			return false
		})
	}

	// Strip extra sort columns that were not in the SELECT list.
	if len(extraSortFields) > 0 {
		projLen := len(cols)
		for i, row := range data {
			data[i] = row[:projLen]
		}
	}

	// Apply OFFSET then LIMIT.
	if sq.offset > 0 {
		if sq.offset >= int64(len(data)) {
			data = data[:0]
		} else {
			data = data[sq.offset:]
		}
	}
	if sq.limit >= 0 && int64(len(data)) > sq.limit {
		data = data[:sq.limit]
	}

	return &staticRows{cols: cols, rows: data}, nil
}

// execSystemTable dispatches INFORMATION_SCHEMA.* queries.
func (c *EmbeddedConnection) execSystemTable(ctx context.Context, name string, whereExpr antlrgen.IWhereExprContext) (driver.Rows, error) {
	switch name {
	case "SCHEMATA":
		return c.execSysSchemata(ctx, whereExpr)
	case "TABLES":
		return c.execSysTables(ctx, whereExpr)
	case "COLUMNS":
		return c.execSysColumns(ctx, whereExpr)
	case "INDEXES":
		return c.execSysIndexes(ctx, whereExpr)
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unknown INFORMATION_SCHEMA table: %q", name)
	}
}

// filterSysRows applies WHERE filtering to system-table rows (map-based, no proto).
// Column names are matched case-insensitively against the cols slice.
func filterSysRows(ctx context.Context, conn *EmbeddedConnection, rows [][]driver.Value, cols []string, where antlrgen.IWhereExprContext) ([][]driver.Value, error) {
	if where == nil {
		return rows, nil
	}
	expr := where.Expression()
	var out [][]driver.Value
	for _, row := range rows {
		m := make(map[string]driver.Value, len(cols))
		for i, c := range cols {
			m[strings.ToUpper(c)] = row[i]
		}
		ok, err := evalPredicateOnMapExpr(ctx, conn, m, expr)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, row)
		}
	}
	return out, nil
}

// execSysSchemata implements SELECT * FROM INFORMATION_SCHEMA.SCHEMATA.
func (c *EmbeddedConnection) execSysSchemata(ctx context.Context, where antlrgen.IWhereExprContext) (driver.Rows, error) {
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.cat.ListSchemas(txn, nil)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck
		for rs.Next() {
			dbID, e := rs.StringByName("DATABASE_ID")
			if e != nil {
				return nil, e
			}
			schemaName, e := rs.StringByName("SCHEMA_NAME")
			if e != nil {
				return nil, e
			}
			data = append(data, row{dbID, schemaName, "", "", ""})
		}
		return nil, rs.Err()
	})
	if err != nil {
		return nil, err
	}
	cols := []string{"CATALOG_NAME", "SCHEMA_NAME", "DEFAULT_CHARACTER_SET_NAME", "DEFAULT_COLLATION_NAME", "SQL_PATH"}
	filtered, ferr := filterSysRows(ctx, c, data, cols, where)
	if ferr != nil {
		return nil, ferr
	}
	return &staticRows{cols: cols, rows: filtered}, nil
}

// execSysTables implements SELECT * FROM INFORMATION_SCHEMA.TABLES.
func (c *EmbeddedConnection) execSysTables(ctx context.Context, where antlrgen.IWhereExprContext) (driver.Rows, error) {
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.cat.ListSchemas(txn, nil)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck

		// Snapshot (db, schema) pairs first; LoadSchema below opens another txn context.
		type schemaRef struct{ db, schema string }
		var refs []schemaRef
		for rs.Next() {
			dbID, e := rs.StringByName("DATABASE_ID")
			if e != nil {
				return nil, e
			}
			schemaName, e := rs.StringByName("SCHEMA_NAME")
			if e != nil {
				return nil, e
			}
			refs = append(refs, schemaRef{dbID, schemaName})
		}
		if e := rs.Err(); e != nil {
			return nil, e
		}

		for _, ref := range refs {
			schema, loadErr := c.cachedLoadSchema(txn, ref.db, ref.schema)
			if loadErr != nil {
				return nil, loadErr
			}
			tables, tablesErr := schema.Tables()
			if tablesErr != nil {
				return nil, tablesErr
			}
			for _, tbl := range tables {
				data = append(data, row{ref.db, ref.schema, tbl.MetadataName(), "TABLE", "", "", "", "", "", ""})
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	cols := []string{
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "TABLE_TYPE",
		"REMARKS", "TYPE_CAT", "TYPE_SCHEM", "TYPE_NAME",
		"SELF_REFERENCING_COL_NAME", "REF_GENERATION",
	}
	filtered, ferr := filterSysRows(ctx, c, data, cols, where)
	if ferr != nil {
		return nil, ferr
	}
	return &staticRows{cols: cols, rows: filtered}, nil
}

// execSysColumns implements SELECT * FROM INFORMATION_SCHEMA.COLUMNS.
func (c *EmbeddedConnection) execSysColumns(ctx context.Context, where antlrgen.IWhereExprContext) (driver.Rows, error) {
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.cat.ListSchemas(txn, nil)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck

		type schemaRef struct{ db, schema string }
		var refs []schemaRef
		for rs.Next() {
			dbID, e := rs.StringByName("DATABASE_ID")
			if e != nil {
				return nil, e
			}
			schemaName, e := rs.StringByName("SCHEMA_NAME")
			if e != nil {
				return nil, e
			}
			refs = append(refs, schemaRef{dbID, schemaName})
		}
		if e := rs.Err(); e != nil {
			return nil, e
		}

		for _, ref := range refs {
			schema, loadErr := c.cachedLoadSchema(txn, ref.db, ref.schema)
			if loadErr != nil {
				return nil, loadErr
			}
			tables, tablesErr := schema.Tables()
			if tablesErr != nil {
				return nil, tablesErr
			}
			for _, tbl := range tables {
				cols := tbl.Columns()
				for i, col := range cols {
					nullable := "NO"
					if col.DataType().IsNullable() {
						nullable = "YES"
					}
					data = append(data, row{
						ref.db,
						ref.schema,
						tbl.MetadataName(),
						col.MetadataName(),
						int64(i + 1),
						"",
						nullable,
						col.DataType().Code().String(),
						"",
						"",
						"",
					})
				}
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	cols := []string{
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "COLUMN_NAME",
		"ORDINAL_POSITION", "COLUMN_DEFAULT", "IS_NULLABLE", "DATA_TYPE",
		"CHARACTER_MAXIMUM_LENGTH", "NUMERIC_PRECISION", "NUMERIC_SCALE",
	}
	filtered, ferr := filterSysRows(ctx, c, data, cols, where)
	if ferr != nil {
		return nil, ferr
	}
	return &staticRows{cols: cols, rows: filtered}, nil
}

// execSysIndexes implements SELECT * FROM INFORMATION_SCHEMA.INDEXES.
// Returns one row per index across all (database, schema, table) tuples.
func (c *EmbeddedConnection) execSysIndexes(ctx context.Context, where antlrgen.IWhereExprContext) (driver.Rows, error) {
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.cat.ListSchemas(txn, nil)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck

		type schemaRef struct{ db, schema string }
		var refs []schemaRef
		for rs.Next() {
			dbID, e := rs.StringByName("DATABASE_ID")
			if e != nil {
				return nil, e
			}
			schemaName, e := rs.StringByName("SCHEMA_NAME")
			if e != nil {
				return nil, e
			}
			refs = append(refs, schemaRef{dbID, schemaName})
		}
		if e := rs.Err(); e != nil {
			return nil, e
		}

		for _, ref := range refs {
			schema, loadErr := c.cachedLoadSchema(txn, ref.db, ref.schema)
			if loadErr != nil {
				return nil, loadErr
			}
			tables, tablesErr := schema.Tables()
			if tablesErr != nil {
				return nil, tablesErr
			}
			for _, tbl := range tables {
				for _, idx := range tbl.Indexes() {
					isUnique := "NO"
					if idx.IsUnique() {
						isUnique = "YES"
					}
					isSparse := "NO"
					if idx.IsSparse() {
						isSparse = "YES"
					}
					data = append(data, row{
						ref.db,
						ref.schema,
						tbl.MetadataName(),
						idx.MetadataName(),
						idx.IndexType(),
						isUnique,
						isSparse,
					})
				}
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	cols := []string{
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME",
		"INDEX_NAME", "INDEX_TYPE", "IS_UNIQUE", "IS_SPARSE",
	}
	data, err = filterSysRows(ctx, c, data, cols, where)
	if err != nil {
		return nil, err
	}
	return &staticRows{cols: cols, rows: data}, nil
}

// selectQuery holds the parsed components of a SELECT statement.
type selectQuery struct {
	tableName   string
	tableAlias  string   // alias or tableName if no alias given
	projCols    []string // nil = SELECT *; ignored when countStar or aggCols non-empty
	projAliases []string // parallel to projCols; empty string = no alias (use column name)
	// projExprs holds computed projection expressions parallel to projCols.
	// Non-nil entry overrides the plain column lookup for that position.
	projExprs []antlrgen.IExpressionContext
	countStar bool // true when SELECT list is exactly COUNT(*)
	distinct  bool // true when SELECT DISTINCT
	whereExpr antlrgen.IWhereExprContext
	// orderBy holds column-name + ascending pairs (nil = no ORDER BY).
	orderBy []orderByClause
	// limit < 0 means no limit.
	limit int64
	// offset >= 0 means skip that many rows after sort/group (OFFSET n).
	offset int64
	// groupBy holds GROUP BY column names (nil = no GROUP BY).
	groupBy []string
	// aggCols describes a mixed GROUP BY + aggregate SELECT list.
	// Non-nil only when groupBy is non-empty.
	aggCols []aggSelectCol
	// havingExpr is the HAVING clause expression (nil = no HAVING).
	havingExpr antlrgen.IExpressionContext
	// joins describes JOIN clauses (nil = no joins).
	joins []joinClause
	// derivedQuery is non-nil when the FROM clause is a subquery (derived table).
	// When set, tableName holds the alias; the query is materialized at execution time.
	derivedQuery antlrgen.IQueryContext
}

// joinClause describes a single JOIN part in a SELECT query.
type joinClause struct {
	tableName string
	joinType  string // "INNER", "LEFT", "RIGHT"
	alias     string
	onExpr    antlrgen.IExpressionContext
}

type orderByClause struct {
	colName   string
	ascending bool
	// expr is non-nil for ORDER BY on a non-trivial expression (e.g.
	// `ORDER BY UPPER(name)`, `ORDER BY price * qty`). When set, colName is
	// empty and the expression is evaluated per row at sort time. Only the
	// CTE and JOIN paths (which retain map rows) honor this; the proto /
	// single-table scan path still requires a column/aggregate name.
	expr antlrgen.IExpressionContext
}

// aggSelectCol describes one column in a GROUP BY aggregate SELECT list.
type aggSelectCol struct {
	outName string // output column name
	// Either groupCol or aggFunc is set.
	groupCol    string // plain group-by column reference
	aggFunc     string // COUNT/SUM/MIN/MAX/AVG
	aggArg      string // argument column name (empty for COUNT(*))
	aggDistinct bool   // true when COUNT(DISTINCT col)
}

// checkCountStar returns true if e is a bare COUNT(*) expression.
func checkCountStar(e *antlrgen.SelectExpressionElementContext) bool {
	pred, ok := e.Expression().(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return false
	}
	fc, ok := pred.ExpressionAtom().(*antlrgen.FunctionCallExpressionAtomContext)
	if !ok {
		return false
	}
	agg, ok := fc.FunctionCall().(*antlrgen.AggregateFunctionCallContext)
	if !ok {
		return false
	}
	awf, ok := agg.AggregateWindowedFunction().(*antlrgen.AggregateWindowedFunctionContext)
	if !ok {
		return false
	}
	return awf.COUNT() != nil && awf.STAR() != nil
}

// extractAggFunc attempts to parse an aggregate function (COUNT/SUM/MIN/MAX/AVG)
// from a SelectExpressionElementContext. Returns (funcName, argColName, distinct, alias, ok).
// funcName is upper-case. argColName is empty for COUNT(*).
func extractAggFunc(e *antlrgen.SelectExpressionElementContext) (funcName, argCol, alias string, distinct, ok bool) {
	pred, pok := e.Expression().(*antlrgen.PredicatedExpressionContext)
	if !pok {
		return "", "", "", false, false
	}
	fc, fcok := pred.ExpressionAtom().(*antlrgen.FunctionCallExpressionAtomContext)
	if !fcok {
		return "", "", "", false, false
	}
	agg, aggok := fc.FunctionCall().(*antlrgen.AggregateFunctionCallContext)
	if !aggok {
		return "", "", "", false, false
	}
	awf, awfok := agg.AggregateWindowedFunction().(*antlrgen.AggregateWindowedFunctionContext)
	if !awfok {
		return "", "", "", false, false
	}
	isDistinct := awf.DISTINCT() != nil
	switch {
	case awf.COUNT() != nil && awf.STAR() != nil:
		funcName = "COUNT"
	case awf.COUNT() != nil:
		funcName = "COUNT"
		if awf.FunctionArg() != nil {
			argCol = awf.FunctionArg().GetText()
		} else if awf.FunctionArgs() != nil && len(awf.FunctionArgs().AllFunctionArg()) > 0 {
			// COUNT(DISTINCT col) — FunctionArgs variant
			argCol = awf.FunctionArgs().AllFunctionArg()[0].GetText()
		}
	case awf.SUM() != nil:
		funcName = "SUM"
		if awf.FunctionArg() != nil {
			argCol = awf.FunctionArg().GetText()
		}
	case awf.MIN() != nil:
		funcName = "MIN"
		if awf.FunctionArg() != nil {
			argCol = awf.FunctionArg().GetText()
		}
	case awf.MAX() != nil:
		funcName = "MAX"
		if awf.FunctionArg() != nil {
			argCol = awf.FunctionArg().GetText()
		}
	case awf.AVG() != nil:
		funcName = "AVG"
		if awf.FunctionArg() != nil {
			argCol = awf.FunctionArg().GetText()
		}
	default:
		return "", "", "", false, false
	}
	if e.Uid() != nil {
		alias = stripIdentifierQuotes(e.Uid().GetText())
	}
	if alias == "" {
		if isDistinct && argCol != "" {
			alias = funcName + "(DISTINCT " + argCol + ")"
		} else if argCol == "" {
			alias = funcName + "(*)"
		} else {
			alias = funcName + "(" + argCol + ")"
		}
	}
	return funcName, argCol, alias, isDistinct, true
}

// columnNameFromExpr extracts a plain column name (or aggregate output name like
// "COUNT(*)") from an IExpressionContext.
// context is used in error messages (e.g. "SELECT expression", "ORDER BY expression").
func columnNameFromExpr(expr antlrgen.IExpressionContext, context string) (string, error) {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return "", api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"%s must be a column name, got %T", context, expr)
	}
	switch a := pred.ExpressionAtom().(type) {
	case *antlrgen.FullColumnNameExpressionAtomContext:
		return fullIdToName(a.FullColumnName().FullId()), nil
	case *antlrgen.FunctionCallExpressionAtomContext:
		// Aggregate function in ORDER BY — return the canonical output name
		// (e.g. COUNT(*), SUM(col)) so it can be matched against the SELECT list.
		agg, aggok := a.FunctionCall().(*antlrgen.AggregateFunctionCallContext)
		if !aggok {
			return "", api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"%s: unsupported function call %T", context, a.FunctionCall())
		}
		awf, awfok := agg.AggregateWindowedFunction().(*antlrgen.AggregateWindowedFunctionContext)
		if !awfok {
			return "", api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"%s: unsupported aggregate %T", context, agg.AggregateWindowedFunction())
		}
		switch {
		case awf.COUNT() != nil && awf.STAR() != nil:
			return "COUNT(*)", nil
		case awf.COUNT() != nil && awf.FunctionArg() != nil:
			return "COUNT(" + awf.FunctionArg().GetText() + ")", nil
		case awf.SUM() != nil && awf.FunctionArg() != nil:
			return "SUM(" + awf.FunctionArg().GetText() + ")", nil
		case awf.MIN() != nil && awf.FunctionArg() != nil:
			return "MIN(" + awf.FunctionArg().GetText() + ")", nil
		case awf.MAX() != nil && awf.FunctionArg() != nil:
			return "MAX(" + awf.FunctionArg().GetText() + ")", nil
		case awf.AVG() != nil && awf.FunctionArg() != nil:
			return "AVG(" + awf.FunctionArg().GetText() + ")", nil
		}
		return "", api.NewErrorf(api.ErrCodeUnsupportedOperation, "%s: unsupported aggregate function", context)
	default:
		return "", api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"%s must be a column name, got expression atom %T", context, pred.ExpressionAtom())
	}
}

// selectExprToColumnName extracts a plain column name and optional alias from a
// SelectExpressionElementContext. Returns (colName, alias, error).
func selectExprToColumnName(e *antlrgen.SelectExpressionElementContext) (string, string, error) {
	colName, err := columnNameFromExpr(e.Expression(), "SELECT expression")
	if err != nil {
		return "", "", err
	}
	alias := ""
	if e.Uid() != nil {
		alias = stripIdentifierQuotes(e.Uid().GetText())
	}
	return colName, alias, nil
}

// extractSelectParts navigates the parse tree of a SELECT statement.
// Supports SELECT [* | col, ...] FROM <table> [WHERE col = val]
//
//	[ORDER BY col [ASC|DESC], ...] [LIMIT n].
//
// Joins, subqueries, aliases, GROUP BY, HAVING, etc. are not supported.
func extractSelectParts(sel antlrgen.ISelectStatementContext) (*selectQuery, error) {
	query := sel.Query()
	if query == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "malformed SELECT statement")
	}
	body, ok := query.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported SELECT form %T; only simple SELECT FROM <table> is supported",
			query.QueryExpressionBody())
	}
	return extractFromQueryTerm(body)
}

func extractFromQueryTerm(body *antlrgen.QueryTermDefaultContext) (*selectQuery, error) {
	simpleTable, ok := body.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported query term %T; only simple SELECT FROM <table> is supported",
			body.QueryTerm())
	}
	return extractFromSimpleTable(simpleTable)
}

func extractFromSimpleTable(simpleTable *antlrgen.SimpleTableContext) (*selectQuery, error) {
	// Parse SELECT list: either *, a list of column name expressions, COUNT(*), or
	// a GROUP BY aggregate list (mix of group-by columns + aggregate functions).
	selElems := simpleTable.SelectElements()
	var projCols []string                       // nil = SELECT *
	var projAliases []string                    // parallel to projCols
	var projExprs []antlrgen.IExpressionContext // parallel to projCols; nil entry = plain column
	var countStar bool
	var aggCols []aggSelectCol
	if selElems != nil {
		elems := selElems.AllSelectElement()
		for _, elem := range elems {
			switch e := elem.(type) {
			case *antlrgen.SelectStarElementContext:
				if len(elems) > 1 {
					return nil, api.NewError(api.ErrCodeUnsupportedOperation,
						"cannot mix * with named columns in SELECT list")
				}
				// SELECT * — projCols stays nil
			case *antlrgen.SelectExpressionElementContext:
				if checkCountStar(e) && len(elems) == 1 {
					countStar = true
				} else if fn, argCol, alias, isDistinct, isAgg := extractAggFunc(e); isAgg {
					aggCols = append(aggCols, aggSelectCol{outName: alias, aggFunc: fn, aggArg: argCol, aggDistinct: isDistinct})
				} else {
					colName, alias, nameErr := selectExprToColumnName(e)
					var expr antlrgen.IExpressionContext
					if nameErr != nil {
						// Not a plain column name — treat as a computed expression.
						// Use alias as the output name; fall back to the raw expression text.
						alias = ""
						if e.Uid() != nil {
							alias = stripIdentifierQuotes(e.Uid().GetText())
						}
						if alias == "" {
							alias = e.Expression().GetText()
						}
						colName = alias
						expr = e.Expression()
					}
					if len(aggCols) > 0 {
						// Mixed aggregate query — this column is a group-by reference.
						aggCols = append(aggCols, aggSelectCol{outName: func() string {
							if alias != "" {
								return alias
							}
							return colName
						}(), groupCol: colName})
					} else {
						projCols = append(projCols, colName)
						projAliases = append(projAliases, alias)
						projExprs = append(projExprs, expr) // nil when it's a plain column
					}
				}
			default:
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
					"unsupported SELECT element type %T", elem)
			}
		}
		// If we found aggregate functions mixed with plain columns, the plain cols
		// that were added to projCols before the first aggregate need to be re-
		// classified as group-by references.
		if len(aggCols) > 0 && len(projCols) > 0 {
			// Prepend the already-collected plain cols as group-by cols.
			extra := make([]aggSelectCol, len(projCols))
			for i, c := range projCols {
				out := projAliases[i]
				if out == "" {
					out = c
				}
				extra[i] = aggSelectCol{outName: out, groupCol: c}
			}
			aggCols = append(extra, aggCols...)
			projCols = nil
			projAliases = nil
			projExprs = nil
		}
	}

	fromClause := simpleTable.FromClause()
	if fromClause == nil {
		// SELECT without FROM: evaluate expressions as constants (single-row result).
		return &selectQuery{
			projCols:    projCols,
			projAliases: projAliases,
			projExprs:   projExprs,
		}, nil
	}

	sources := fromClause.TableSources()
	if sources == nil || len(sources.AllTableSource()) == 0 {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"FROM clause missing table source")
	}
	srcBase, ok := sources.AllTableSource()[0].(*antlrgen.TableSourceBaseContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported table source %T", sources.AllTableSource()[0])
	}
	// Additional comma-separated sources become implicit cross joins; the
	// WHERE clause supplies any join predicate.
	var extraCrossJoins []joinClause
	for _, extra := range sources.AllTableSource()[1:] {
		eb, isBase := extra.(*antlrgen.TableSourceBaseContext)
		if !isBase {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"unsupported extra table source %T", extra)
		}
		atomItem, atomOk := eb.TableSourceItem().(*antlrgen.AtomTableItemContext)
		if !atomOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"FROM: comma-separated sources must be plain table names, got %T",
				eb.TableSourceItem())
		}
		uids := atomItem.TableName().FullId().AllUid()
		parts := make([]string, len(uids))
		for i, u := range uids {
			parts[i] = stripIdentifierQuotes(u.GetText())
		}
		tblName := strings.Join(parts, ".")
		alias := tblName
		if atomItem.AS() != nil && atomItem.Uid() != nil {
			alias = stripIdentifierQuotes(atomItem.Uid().GetText())
		}
		extraCrossJoins = append(extraCrossJoins, joinClause{
			tableName: tblName,
			joinType:  "INNER",
			alias:     alias,
			onExpr:    nil,
		})
		// Bare-source joins are not supported on extras (grammar quirk).
		if len(eb.AllJoinPart()) > 0 {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation,
				"JOIN clauses on comma-separated FROM sources are not supported")
		}
	}
	// Check for derived table: FROM (SELECT ...) AS alias
	if subItem, isSub := srcBase.TableSourceItem().(*antlrgen.SubqueryTableItemContext); isSub {
		alias := ""
		if subItem.GetAlias() != nil {
			alias = stripIdentifierQuotes(subItem.GetAlias().GetText())
		}
		if alias == "" {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation, "derived table in FROM must have an alias")
		}
		return &selectQuery{
			tableName:    alias,
			tableAlias:   alias,
			projCols:     projCols,
			projAliases:  projAliases,
			projExprs:    projExprs,
			countStar:    countStar,
			aggCols:      aggCols,
			distinct:     simpleTable.DISTINCT() != nil,
			whereExpr:    fromClause.WhereExpr(),
			limit:        -1,
			derivedQuery: subItem.Query(),
		}, nil
	}

	atomItem, ok := srcBase.TableSourceItem().(*antlrgen.AtomTableItemContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported table source item %T; only plain table names are supported",
			srcBase.TableSourceItem())
	}
	// Build table name from uid segments, stripping identifier quotes.
	// "INFORMATION_SCHEMA"."TABLES" → INFORMATION_SCHEMA.TABLES
	uids := atomItem.TableName().FullId().AllUid()
	parts := make([]string, len(uids))
	for i, u := range uids {
		parts[i] = stripIdentifierQuotes(u.GetText())
	}
	// Only use Uid() as alias when AS is explicit. Without AS, the parser may
	// greedily consume a join keyword (LEFT, RIGHT, CROSS) as the table alias
	// due to grammar ambiguity — LEFT/RIGHT are in keywordsCanBeId.
	// When the mis-parsed "alias" is LEFT or RIGHT, we promote the first
	// InnerJoinContext to a LEFT/RIGHT join.
	leftAlias := ""
	promotedJoinType := ""
	if atomItem.AS() != nil && atomItem.Uid() != nil {
		leftAlias = stripIdentifierQuotes(atomItem.Uid().GetText())
	} else if atomItem.Uid() != nil {
		misAlias := strings.ToUpper(atomItem.Uid().GetText())
		if misAlias == "LEFT" || misAlias == "RIGHT" {
			promotedJoinType = misAlias
		}
	}
	if leftAlias == "" {
		leftAlias = strings.Join(parts, ".")
	}

	// Parse JOIN clauses.
	var joins []joinClause
	for _, jp := range srcBase.AllJoinPart() {
		jc, jErr := extractJoinClause(jp)
		if jErr != nil {
			return nil, jErr
		}
		joins = append(joins, jc)
	}
	// If the first join was mis-parsed (LEFT/RIGHT consumed as alias), promote it.
	if promotedJoinType != "" && len(joins) > 0 && joins[0].joinType == "INNER" {
		joins[0].joinType = promotedJoinType
	}
	// Implicit cross joins from comma-separated FROM sources run last; the
	// WHERE predicate decides which combinations survive.
	joins = append(joins, extraCrossJoins...)

	sq := &selectQuery{
		tableName:   strings.Join(parts, "."),
		tableAlias:  leftAlias,
		joins:       joins,
		projCols:    projCols,
		projAliases: projAliases,
		projExprs:   projExprs,
		countStar:   countStar,
		aggCols:     aggCols,
		distinct:    simpleTable.DISTINCT() != nil,
		whereExpr:   fromClause.WhereExpr(),
		limit:       -1,
	}

	// Parse ORDER BY clause.
	orderByClauseCtx := simpleTable.OrderByClause()
	if orderByClauseCtx != nil {
		for _, obExpr := range orderByClauseCtx.AllOrderByExpression() {
			ascending := true
			if oc := obExpr.OrderClause(); oc != nil && oc.DESC() != nil {
				ascending = false
			}
			// Prefer plain column / aggregate lookup (works in all sort paths,
			// including the proto single-table path). Fall back to storing the
			// expression for CTE / JOIN sort keys like `ORDER BY a + b`.
			colName, nameErr := columnNameFromExpr(obExpr.Expression(), "ORDER BY expression")
			if nameErr == nil {
				sq.orderBy = append(sq.orderBy, orderByClause{colName: colName, ascending: ascending})
			} else {
				sq.orderBy = append(sq.orderBy, orderByClause{ascending: ascending, expr: obExpr.Expression()})
			}
		}
	}

	// Parse LIMIT [OFFSET] clause.
	limitClauseCtx := simpleTable.LimitClause()
	if limitClauseCtx != nil {
		parseLimitAtom := func(a antlrgen.ILimitClauseAtomContext, label string) (int64, error) {
			if a == nil || a.DecimalLiteral() == nil {
				return 0, nil
			}
			n, parseErr := strconv.ParseInt(a.DecimalLiteral().GetText(), 10, 64)
			if parseErr != nil {
				return 0, api.NewErrorf(api.ErrCodeInvalidParameter, "invalid %s value %q: %v", label, a.DecimalLiteral().GetText(), parseErr)
			}
			return n, nil
		}
		// Grammar exposes GetLimit() / GetOffset() for "LIMIT n OFFSET m" form,
		// and AllLimitClauseAtom() for the MySQL "LIMIT offset, count" form.
		if limitClauseCtx.GetLimit() != nil {
			n, parseErr := parseLimitAtom(limitClauseCtx.GetLimit(), "LIMIT")
			if parseErr != nil {
				return nil, parseErr
			}
			sq.limit = n
			if limitClauseCtx.GetOffset() != nil {
				off, parseErr := parseLimitAtom(limitClauseCtx.GetOffset(), "OFFSET")
				if parseErr != nil {
					return nil, parseErr
				}
				sq.offset = off
			}
		} else {
			atoms := limitClauseCtx.AllLimitClauseAtom()
			if len(atoms) > 1 {
				// MySQL allows "LIMIT offset, count" — reject for simplicity.
				return nil, api.NewError(api.ErrCodeUnsupportedOperation,
					"LIMIT offset,count syntax is not supported; use LIMIT count OFFSET n")
			}
			if len(atoms) == 1 {
				n, parseErr := parseLimitAtom(atoms[0], "LIMIT")
				if parseErr != nil {
					return nil, parseErr
				}
				sq.limit = n
			}
		}
	}

	// Parse GROUP BY clause.
	groupByCtx := simpleTable.GroupByClause()
	if groupByCtx != nil {
		for _, item := range groupByCtx.AllGroupByItem() {
			colName, nameErr := columnNameFromExpr(item.Expression(), "GROUP BY expression")
			if nameErr != nil {
				return nil, nameErr
			}
			sq.groupBy = append(sq.groupBy, colName)
		}
	}

	// Parse HAVING clause (only meaningful with GROUP BY).
	havingCtx := simpleTable.HavingClause()
	if havingCtx != nil {
		sq.havingExpr = havingCtx.GetHavingExpr()
	}

	return sq, nil
}

// extractJoinClause parses a single JOIN part (INNER JOIN, LEFT JOIN, etc.) from
// the grammar. Only INNER JOIN and LEFT OUTER JOIN are implemented.
func extractJoinClause(jp antlrgen.IJoinPartContext) (joinClause, error) {
	switch j := jp.(type) {
	case *antlrgen.InnerJoinContext:
		atomItem, ok := j.TableSourceItem().(*antlrgen.AtomTableItemContext)
		if !ok {
			return joinClause{}, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"JOIN: unsupported table source item %T", j.TableSourceItem())
		}
		uids := atomItem.TableName().FullId().AllUid()
		parts := make([]string, len(uids))
		for i, u := range uids {
			parts[i] = stripIdentifierQuotes(u.GetText())
		}
		tblName := strings.Join(parts, ".")
		alias := tblName
		if atomItem.AS() != nil && atomItem.Uid() != nil {
			alias = stripIdentifierQuotes(atomItem.Uid().GetText())
		}
		var onExpr antlrgen.IExpressionContext
		if j.Expression() != nil {
			onExpr = j.Expression()
		}
		return joinClause{tableName: tblName, joinType: "INNER", alias: alias, onExpr: onExpr}, nil

	case *antlrgen.OuterJoinContext:
		atomItem, ok := j.TableSourceItem().(*antlrgen.AtomTableItemContext)
		if !ok {
			return joinClause{}, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"JOIN: unsupported table source item %T", j.TableSourceItem())
		}
		uids := atomItem.TableName().FullId().AllUid()
		parts := make([]string, len(uids))
		for i, u := range uids {
			parts[i] = stripIdentifierQuotes(u.GetText())
		}
		tblName := strings.Join(parts, ".")
		alias := tblName
		if atomItem.AS() != nil && atomItem.Uid() != nil {
			alias = stripIdentifierQuotes(atomItem.Uid().GetText())
		}
		jt := "LEFT"
		if j.RIGHT() != nil {
			jt = "RIGHT"
		}
		var onExpr antlrgen.IExpressionContext
		if j.Expression() != nil {
			onExpr = j.Expression()
		}
		return joinClause{tableName: tblName, joinType: jt, alias: alias, onExpr: onExpr}, nil

	default:
		return joinClause{}, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported JOIN type %T; only INNER JOIN and LEFT/RIGHT OUTER JOIN are supported", jp)
	}
}

// stripIdentifierQuotes removes surrounding double-quotes or backticks from a
// SQL identifier produced by the ANTLR parser (e.g. `"FOO"` → `FOO`).
func stripIdentifierQuotes(s string) string {
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '`' && s[len(s)-1] == '`')) {
		return s[1 : len(s)-1]
	}
	return s
}

// fullIdToName converts a FullId parse-tree node to a dot-separated,
// quote-stripped name. Used for table names in INSERT, UPDATE, DELETE.
func fullIdToName(fid antlrgen.IFullIdContext) string {
	uids := fid.AllUid()
	parts := make([]string, len(uids))
	for i, u := range uids {
		parts[i] = stripIdentifierQuotes(u.GetText())
	}
	return strings.Join(parts, ".")
}

// protoValueToDriver converts a protoreflect.Value to a driver.Value.
// For proto2 optional fields that are not set, returns nil (SQL NULL).
func protoValueToDriver(fd protoreflect.FieldDescriptor, v protoreflect.Value) driver.Value {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return v.Bool()
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return v.Int()
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return int64(v.Uint()) //nolint:gosec
	case protoreflect.FloatKind:
		return float64(v.Float())
	case protoreflect.DoubleKind:
		return v.Float()
	case protoreflect.StringKind:
		return v.String()
	case protoreflect.BytesKind:
		return []byte(v.Bytes())
	default:
		return v.Interface()
	}
}

// execShowStatement routes SHOW … to the appropriate catalog reader.
func (c *EmbeddedConnection) execShowStatement(ctx context.Context, show antlrgen.IShowStatementContext) (driver.Rows, error) {
	switch show.(type) {
	case *antlrgen.ShowDatabasesStatementContext:
		return c.execShowDatabases(ctx)
	case *antlrgen.ShowSchemaTemplatesStatementContext:
		return c.execShowSchemaTemplates(ctx)
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported SHOW statement: %s", show.GetText())
	}
}

func (c *EmbeddedConnection) execShowDatabases(ctx context.Context) (driver.Rows, error) {
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil // reset on retry
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.cat.ListDatabases(txn, nil)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck
		for rs.Next() {
			id, strErr := rs.StringByName("DATABASE_ID")
			if strErr != nil {
				return nil, strErr
			}
			data = append(data, row{id})
		}
		return nil, rs.Err()
	})
	if err != nil {
		return nil, err
	}
	return &staticRows{cols: []string{"DATABASE_ID"}, rows: data}, nil
}

func (c *EmbeddedConnection) execShowSchemaTemplates(ctx context.Context) (driver.Rows, error) {
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil // reset on retry
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.cat.SchemaTemplateCatalog().ListTemplates(txn)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck
		for rs.Next() {
			name, strErr := rs.StringByName("TEMPLATE_NAME")
			if strErr != nil {
				return nil, strErr
			}
			ver, intErr := rs.LongByName("TEMPLATE_VERSION")
			if intErr != nil {
				return nil, intErr
			}
			data = append(data, row{name, ver})
		}
		return nil, rs.Err()
	})
	if err != nil {
		return nil, err
	}
	return &staticRows{cols: []string{"TEMPLATE_NAME", "TEMPLATE_VERSION"}, rows: data}, nil
}

// staticRows is a driver.Rows backed by a pre-materialised slice.
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

// Prepare returns a prepared statement. DDL statements have no bind parameters.
func (c *EmbeddedConnection) Prepare(query string) (driver.Stmt, error) {
	if c.closed.Load() {
		return nil, driver.ErrBadConn
	}
	return &embeddedStmt{conn: c, query: query}, nil
}

// Close marks the connection as closed.
func (c *EmbeddedConnection) Close() error {
	c.closed.Store(true)
	return nil
}

// Begin implements driver.Conn by delegating to BeginTx with default options.
func (c *EmbeddedConnection) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx implements driver.ConnBeginTx. Opens an FDB transaction that spans
// all subsequent statements until Commit or Rollback is called.
// Isolation levels other than the default and ReadCommitted return an error.
// Read-only transactions are not separately enforced at the FDB level.
func (c *EmbeddedConnection) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.closed.Load() {
		return nil, driver.ErrBadConn
	}
	if c.activeTx != nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"nested transactions are not supported")
	}
	switch sql.IsolationLevel(opts.Isolation) { //nolint:exhaustive
	case sql.LevelDefault, sql.LevelSerializable:
		// FDB is always serializable — this is fine.
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"isolation level %v is not supported; use LevelDefault or LevelSerializable",
			sql.IsolationLevel(opts.Isolation))
	}
	return c.beginTransaction()
}

func (c *EmbeddedConnection) beginTransaction() (*embeddedTx, error) {
	fdbTx, err := c.fdbDB.CreateTransaction()
	if err != nil {
		return nil, err
	}
	fdbTx.Options().SetReadSystemKeys()
	rctx := recordlayer.NewFDBRecordContext(fdbTx)
	tx := &embeddedTx{conn: c, rctx: rctx}
	c.activeTx = tx
	return tx, nil
}

// SetDefaultSchema sets the initial schema that is restored by ResetSession.
// Called by the driver when the DSN contains ?schema=.
func (c *EmbeddedConnection) SetDefaultSchema(s string) {
	c.defaultSchema = s
	c.schema = s
}

// ResetSession implements driver.SessionResetter. Resets per-request
// state (schema) so pooled connections start clean.
func (c *EmbeddedConnection) ResetSession(_ context.Context) error {
	if c.closed.Load() {
		return driver.ErrBadConn
	}
	c.schema = c.defaultSchema
	return nil
}

// IsValid implements driver.Validator. Returns true if the connection
// is open; the FDB client is stateless so a non-closed connection is
// always usable (catalog init is lazy, not a validity condition).
func (c *EmbeddedConnection) IsValid() bool {
	return !c.closed.Load()
}

// PrepareContext implements driver.ConnPrepareContext.
func (c *EmbeddedConnection) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	return c.Prepare(query)
}

// SetSchema sets the current schema label used when no schema is specified in SQL.
func (c *EmbeddedConnection) SetSchema(s string) { c.schema = s }

// GetSchema returns the current schema label.
func (c *EmbeddedConnection) GetSchema() string { return c.schema }

// GetDBPath returns the current database path.
func (c *EmbeddedConnection) GetDBPath() string { return c.dbPath }

// execStatement routes a single parsed statement to the right handler.
func (c *EmbeddedConnection) execStatement(ctx context.Context, stmt antlrgen.IStatementContext) (int64, error) {
	if ddl := stmt.DdlStatement(); ddl != nil {
		create := ddl.CreateStatement()
		drop := ddl.DropStatement()
		switch {
		case create != nil:
			return c.execCreate(ctx, create)
		case drop != nil:
			return c.execDrop(ctx, drop)
		default:
			return 0, api.NewError(api.ErrCodeUnsupportedOperation, "unsupported DDL statement")
		}
	}
	if dml := stmt.DmlStatement(); dml != nil {
		if ins := dml.InsertStatement(); ins != nil {
			return c.execInsert(ctx, ins)
		}
		if del := dml.DeleteStatement(); del != nil {
			return c.execDelete(ctx, del)
		}
		if upd := dml.UpdateStatement(); upd != nil {
			return c.execUpdate(ctx, upd)
		}
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "unsupported DML statement")
	}
	if txn := stmt.TransactionStatement(); txn != nil {
		return c.execTransactionStatement(txn)
	}
	return 0, api.NewError(api.ErrCodeUnsupportedOperation, "unsupported statement type; supported: DDL, INSERT, UPDATE, DELETE")
}

// execTransactionStatement handles SQL COMMIT, ROLLBACK, and START TRANSACTION.
// These mirror what database/sql sends when applications use explicit transactions
// via the driver rather than BeginTx directly.
func (c *EmbeddedConnection) execTransactionStatement(txn antlrgen.ITransactionStatementContext) (int64, error) {
	switch {
	case txn.CommitStatement() != nil:
		if c.activeTx == nil {
			return 0, api.NewError(api.ErrCodeUnsupportedOperation, "COMMIT: no active transaction")
		}
		if err := c.activeTx.Commit(); err != nil {
			return 0, err
		}
		return 0, nil
	case txn.RollbackStatement() != nil:
		if c.activeTx == nil {
			return 0, nil // ROLLBACK outside transaction is a no-op
		}
		return 0, c.activeTx.Rollback()
	case txn.StartTransaction() != nil:
		if c.activeTx != nil {
			return 0, api.NewError(api.ErrCodeUnsupportedOperation, "nested transactions are not supported")
		}
		if _, err := c.beginTransaction(); err != nil {
			return 0, err
		}
		return 0, nil
	default:
		return 0, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported transaction statement: %s", txn.GetText())
	}
}

// execInsert executes INSERT INTO table (col1, col2, ...) VALUES (...), (...).
func (c *EmbeddedConnection) execInsert(ctx context.Context, ins antlrgen.IInsertStatementContext) (int64, error) {
	if c.schema == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.dbPath == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}

	// Explicit column list (optional).
	colCtx := ins.UidListWithNestingsInParens()
	var explicitCols []string // nil = no column list (use schema order)
	if colCtx != nil {
		for _, uw := range colCtx.UidListWithNestings().AllUidWithNestings() {
			explicitCols = append(explicitCols, stripIdentifierQuotes(uw.Uid().GetText()))
		}
	}

	tableName := fullIdToName(ins.TableName().FullId())

	// Handle INSERT INTO ... SELECT (insertStatementValueSelect).
	if selCtx, ok := ins.InsertStatementValue().(*antlrgen.InsertStatementValueSelectContext); ok {
		return c.execInsertSelect(ctx, tableName, explicitCols, selCtx.QueryExpressionBody())
	}

	// Only handle VALUES path.
	valCtx, ok := ins.InsertStatementValue().(*antlrgen.InsertStatementValueValuesContext)
	if !ok {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "only INSERT ... VALUES (...) is supported")
	}

	var totalRows int64
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		totalRows = 0 // reset on retry
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.dbPath, c.schema)
		if loadErr != nil {
			return nil, loadErr
		}
		rlTmpl, tmplOk := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
		if !tmplOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "schema template is not a RecordLayerSchemaTemplate")
		}
		md := rlTmpl.Underlying()

		rt := md.GetRecordType(tableName)
		if rt == nil {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "table %q not found in schema", tableName)
		}
		msgDesc := rt.Descriptor

		ss, ssErr := c.ks.SchemaSubspace(c.dbPath, c.schema)
		if ssErr != nil {
			return nil, ssErr
		}
		store, storeErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(md).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		// Resolve column order: explicit list or all fields in descriptor order.
		cols := explicitCols
		if cols == nil {
			fds := msgDesc.Fields()
			cols = make([]string, fds.Len())
			for i := 0; i < fds.Len(); i++ {
				cols[i] = string(fds.Get(i).Name())
			}
		}

		for _, rowCtx := range valCtx.AllRecordConstructorForInsert() {
			exprs := rowCtx.AllExpressionWithOptionalName()
			if len(exprs) != len(cols) {
				return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
					"column count %d does not match value count %d", len(cols), len(exprs))
			}
			msg := dynamicpb.NewMessage(msgDesc)
			for i, col := range cols {
				fd := msgDesc.Fields().ByName(protoreflect.Name(col))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "column %q not found in table %q", col, tableName)
				}
				val, evalErr := evalExpr(ctx, c, nil, exprs[i].Expression())
				if evalErr != nil {
					return nil, evalErr
				}
				if val == nil {
					// NULL — leave field absent (proto2 optional semantics).
					continue
				}
				protoVal, convErr := convertToProtoValue(fd, val)
				if convErr != nil {
					return nil, convErr
				}
				msg.Set(fd, protoVal)
			}
			if _, saveErr := store.SaveRecord(msg); saveErr != nil {
				return nil, saveErr
			}
			totalRows++
		}
		return nil, nil
	})
	if err != nil {
		return 0, err
	}
	return totalRows, nil
}

// execInsertSelect implements INSERT INTO table (cols) SELECT ...
// It evaluates the SELECT query and inserts each row into the table.
func (c *EmbeddedConnection) execInsertSelect(ctx context.Context, tableName string, explicitCols []string, body antlrgen.IQueryExpressionBodyContext) (int64, error) {
	if c.schema == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.dbPath == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}

	// Execute the SELECT in a separate transaction from the INSERT. The two operations are
	// not atomic — a concurrent writer may modify rows between the SELECT and INSERT
	// (TOCTOU window). This is a known limitation of the current implementation.
	srcCols, srcRows, err := c.execQueryBodyRows(ctx, body)
	if err != nil {
		return 0, err
	}

	var totalRows int64
	_, err = c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		totalRows = 0
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.dbPath, c.schema)
		if loadErr != nil {
			return nil, loadErr
		}
		rlTmpl, tmplOk := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
		if !tmplOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "schema template is not a RecordLayerSchemaTemplate")
		}
		md := rlTmpl.Underlying()

		rt := md.GetRecordType(tableName)
		if rt == nil {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "table %q not found in schema", tableName)
		}
		msgDesc := rt.Descriptor

		ss, ssErr := c.ks.SchemaSubspace(c.dbPath, c.schema)
		if ssErr != nil {
			return nil, ssErr
		}
		store, storeErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(md).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		// Determine target columns: explicit list or use source column names.
		cols := explicitCols
		if cols == nil {
			cols = srcCols
		}
		if len(cols) != len(srcCols) {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
				"column count %d does not match SELECT column count %d", len(cols), len(srcCols))
		}

		for _, row := range srcRows {
			msg := dynamicpb.NewMessage(msgDesc)
			for i, col := range cols {
				fd := msgDesc.Fields().ByName(protoreflect.Name(col))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "column %q not found in table %q", col, tableName)
				}
				val := row[i]
				if val == nil {
					continue
				}
				protoVal, convErr := convertToProtoValue(fd, val)
				if convErr != nil {
					return nil, convErr
				}
				msg.Set(fd, protoVal)
			}
			if _, saveErr := store.SaveRecord(msg); saveErr != nil {
				return nil, saveErr
			}
			totalRows++
		}
		return nil, nil
	})
	if err != nil {
		return 0, err
	}
	return totalRows, nil
}

// evalLiteralExpr evaluates a literal expression from an INSERT VALUES list.
// Returns nil for NULL literals.
func evalLiteralExpr(expr antlrgen.IExpressionWithOptionalNameContext) (any, error) {
	pred, ok := expr.Expression().(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported expression type %T in INSERT", expr.Expression())
	}
	atomCtx, ok := pred.ExpressionAtom().(*antlrgen.ConstantExpressionAtomContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported expression atom %T in INSERT", pred.ExpressionAtom())
	}
	return evalConstant(atomCtx.Constant())
}

// convertToProtoValue converts a Go value (int64, float64, string, bool) to
// a protoreflect.Value matching the field descriptor's kind.
func convertToProtoValue(fd protoreflect.FieldDescriptor, val any) (protoreflect.Value, error) {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		switch v := val.(type) {
		case bool:
			return protoreflect.ValueOfBool(v), nil
		case int64:
			return protoreflect.ValueOfBool(v != 0), nil
		}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		if v, ok := val.(int64); ok {
			return protoreflect.ValueOfInt32(int32(v)), nil //nolint:gosec
		}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		if v, ok := val.(int64); ok {
			return protoreflect.ValueOfInt64(v), nil
		}
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		if v, ok := val.(int64); ok {
			return protoreflect.ValueOfUint32(uint32(v)), nil //nolint:gosec
		}
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		if v, ok := val.(int64); ok {
			return protoreflect.ValueOfUint64(uint64(v)), nil //nolint:gosec
		}
	case protoreflect.FloatKind:
		switch v := val.(type) {
		case float64:
			return protoreflect.ValueOfFloat32(float32(v)), nil //nolint:gosec
		case int64:
			return protoreflect.ValueOfFloat32(float32(v)), nil //nolint:gosec
		}
	case protoreflect.DoubleKind:
		switch v := val.(type) {
		case float64:
			return protoreflect.ValueOfFloat64(v), nil
		case int64:
			return protoreflect.ValueOfFloat64(float64(v)), nil
		}
	case protoreflect.StringKind:
		if v, ok := val.(string); ok {
			return protoreflect.ValueOfString(v), nil
		}
	case protoreflect.BytesKind:
		if v, ok := val.(string); ok {
			return protoreflect.ValueOfBytes([]byte(v)), nil
		}
	}
	return protoreflect.Value{}, api.NewErrorf(api.ErrCodeInvalidParameter,
		"cannot convert %T to proto field kind %s", val, fd.Kind())
}

// evalExpr evaluates an expression against msg, returning a scalar driver.Value.
// Used in SELECT projections, UPDATE SET, and WHERE/HAVING predicates.
// Supports: literals, column references, and binary arithmetic (+, -, *, /).
func evalExpr(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, expr antlrgen.IExpressionContext) (any, error) {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		// Boolean expressions (AND/OR/NOT, comparisons) return bool as a value.
		b, err := evalExprPredicate(ctx, conn, msg, expr)
		if err != nil {
			return nil, err
		}
		return b, nil
	}
	// If a predicate modifier is present (IN, IS, LIKE, BETWEEN), evaluate via
	// evalExprPredicate which handles the full predicate tree.
	if pred.Predicate() != nil {
		b, err := evalExprPredicate(ctx, conn, msg, expr)
		if err != nil {
			return nil, err
		}
		return b, nil
	}
	return evalExprAtom(ctx, conn, msg, pred.ExpressionAtom())
}

func evalExprAtom(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, atom antlrgen.IExpressionAtomContext) (any, error) {
	switch a := atom.(type) {
	case *antlrgen.ConstantExpressionAtomContext:
		return evalConstant(a.Constant())
	case *antlrgen.FullColumnNameExpressionAtomContext:
		colName := fullIdToName(a.FullColumnName().FullId())
		if msg == nil {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "column reference %q not allowed in this context", colName)
		}
		fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(colName))
		if fd == nil {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "column %q not found", colName)
		}
		return protoValueToDriver(fd, msg.ProtoReflect().Get(fd)), nil
	case *antlrgen.MathExpressionAtomContext:
		left, err := evalExprAtom(ctx, conn, msg, a.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := evalExprAtom(ctx, conn, msg, a.GetRight())
		if err != nil {
			return nil, err
		}
		return applyMathOp(left, right, a.MathOperator().GetText())
	case *antlrgen.FunctionCallExpressionAtomContext:
		return evalScalarFunctionCall(ctx, conn, msg, a.FunctionCall())
	case *antlrgen.BinaryComparisonPredicateContext:
		// Comparison used as a value (e.g. IF(a > b, ...) or CASE WHEN ... END).
		left, err := evalExprAtom(ctx, conn, msg, a.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := evalExprAtom(ctx, conn, msg, a.GetRight())
		if err != nil {
			return nil, err
		}
		cmp := compareValues(left, right)
		switch a.ComparisonOperator().GetText() {
		case "=":
			return cmp == 0, nil
		case "!=", "<>":
			return cmp != 0, nil
		case "<":
			return cmp < 0, nil
		case "<=":
			return cmp <= 0, nil
		case ">":
			return cmp > 0, nil
		case ">=":
			return cmp >= 0, nil
		}
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported comparison operator %q", a.ComparisonOperator().GetText())
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported expression atom %T", atom)
	}
}

// evalScalarFunctionCall evaluates a scalar SQL function call (COALESCE, IFNULL, CASE, etc.).
// exprEvaluator is the function-pointer adapter that abstracts over the two
// expression-evaluation contexts (proto record vs. map row). Both the scalar
// and specific function cores drive all argument evaluation through this.
type exprEvaluator func(expr antlrgen.IExpressionContext) (driver.Value, error)

// predicateEvaluator is the boolean-predicate counterpart of exprEvaluator,
// used by the searched CASE WHEN branch of evalSpecificFunctionCore.
type predicateEvaluator func(expr antlrgen.IExpressionContext) (bool, error)

// evalScalarFunctionCallCore is the unified implementation shared by
// evalScalarFunctionCall (proto path) and evalScalarFunctionCallOnMap (map
// path). The two callers differ only in how they evaluate sub-expressions;
// that variation is captured in the eval / predicateEval adapters.
//
// unsupportedFmt is the format string ("... %q ...") used for the default
// case — proto and map paths use subtly different wording which we preserve
// verbatim. It must accept exactly one %q for the function name.
func evalScalarFunctionCallCore(
	eval exprEvaluator,
	predicateEval predicateEvaluator,
	unsupportedFmt string,
	unsupportedSpecificFmt string,
	fc antlrgen.IFunctionCallContext,
) (driver.Value, error) {
	// Handle CASE expressions routed through SpecificFunctionCall.
	if sf, ok := fc.(*antlrgen.SpecificFunctionCallContext); ok {
		return evalSpecificFunctionCore(eval, predicateEval, unsupportedSpecificFmt, sf.SpecificFunction())
	}

	var name string
	var args antlrgen.IFunctionArgsContext
	switch f := fc.(type) {
	case *antlrgen.ScalarFunctionCallContext:
		name = strings.ToUpper(f.ScalarFunctionName().GetText())
		args = f.FunctionArgs()
	case *antlrgen.UserDefinedScalarFunctionCallContext:
		name = strings.ToUpper(f.UserDefinedScalarFunctionName().GetText())
		args = f.FunctionArgs()
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported function call type %T", fc)
	}
	var fArgs []antlrgen.IFunctionArgContext
	if args != nil {
		fArgs = args.AllFunctionArg()
	}
	switch name {
	case "COALESCE":
		for _, fa := range fArgs {
			v, err := eval(fa.Expression())
			if err != nil {
				return nil, err
			}
			if v != nil {
				return v, nil
			}
		}
		return nil, nil
	case "IFNULL":
		if len(fArgs) < 2 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "IFNULL requires 2 arguments")
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil {
			return nil, err
		}
		if v != nil {
			return v, nil
		}
		return eval(fArgs[1].Expression())
	case "UPPER":
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "UPPER requires 1 argument")
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		s, ok := v.(string)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "UPPER: argument must be string, got %T", v)
		}
		return strings.ToUpper(s), nil
	case "LOWER":
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "LOWER requires 1 argument")
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		s, ok := v.(string)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "LOWER: argument must be string, got %T", v)
		}
		return strings.ToLower(s), nil
	case "LENGTH", "LEN":
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "LENGTH requires 1 argument")
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		s, ok := v.(string)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "LENGTH: argument must be string, got %T", v)
		}
		return int64(utf8.RuneCountInString(s)), nil
	case "TRIM", "LTRIM", "RTRIM":
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "%s requires 1 argument", name)
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		s, ok := v.(string)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "%s: argument must be string, got %T", name, v)
		}
		switch name {
		case "LTRIM":
			return strings.TrimLeft(s, " \t\n\r"), nil
		case "RTRIM":
			return strings.TrimRight(s, " \t\n\r"), nil
		}
		return strings.TrimSpace(s), nil
	case "ABS":
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "ABS requires 1 argument")
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		switch n := v.(type) {
		case int64:
			if n < 0 {
				return -n, nil
			}
			return n, nil
		case float64:
			if n < 0 {
				return -n, nil
			}
			return n, nil
		default:
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "ABS: argument must be numeric, got %T", v)
		}
	case "FLOOR", "CEIL", "CEILING", "ROUND":
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "%s requires at least 1 argument", name)
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		var f float64
		switch n := v.(type) {
		case int64:
			return n, nil // already integer
		case float64:
			f = n
		default:
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "%s: argument must be numeric", name)
		}
		var result float64
		switch name {
		case "FLOOR":
			result = math.Floor(f)
		case "CEIL", "CEILING":
			result = math.Ceil(f)
		case "ROUND":
			decimals := int64(0)
			if len(fArgs) >= 2 {
				dv, derr := eval(fArgs[1].Expression())
				if derr == nil {
					if d, ok := dv.(int64); ok {
						decimals = d
					}
				}
			}
			if decimals == 0 {
				result = math.Round(f)
			} else {
				factor := math.Pow(10, float64(decimals))
				result = math.Round(f*factor) / factor
			}
		}
		// Return int64 if no fractional part.
		if result == math.Trunc(result) && result >= math.MinInt64 && result <= math.MaxInt64 {
			return int64(result), nil
		}
		return result, nil
	case "MOD":
		if len(fArgs) < 2 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "MOD requires 2 arguments")
		}
		av, aerr := eval(fArgs[0].Expression())
		if aerr != nil || av == nil {
			return nil, aerr
		}
		bv, berr := eval(fArgs[1].Expression())
		if berr != nil || bv == nil {
			return nil, berr
		}
		toFloat := func(v driver.Value) (float64, bool) {
			switch n := v.(type) {
			case int64:
				return float64(n), true
			case float64:
				return n, true
			}
			return 0, false
		}
		af, aok := toFloat(av)
		bf, bok := toFloat(bv)
		if !aok || !bok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "MOD: arguments must be numeric")
		}
		if bf == 0 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "MOD: division by zero")
		}
		if _, aIsInt := av.(int64); aIsInt {
			if _, bIsInt := bv.(int64); bIsInt {
				return int64(af) % int64(bf), nil
			}
		}
		return math.Mod(af, bf), nil
	case "POWER", "POW":
		if len(fArgs) < 2 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "POWER requires 2 arguments")
		}
		baseV, berr := eval(fArgs[0].Expression())
		if berr != nil || baseV == nil {
			return nil, berr
		}
		expV, eerr := eval(fArgs[1].Expression())
		if eerr != nil || expV == nil {
			return nil, eerr
		}
		base, ok := toFloat64(baseV)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "POWER: base must be numeric, got %T", baseV)
		}
		exp, ok := toFloat64(expV)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "POWER: exponent must be numeric, got %T", expV)
		}
		result := math.Pow(base, exp)
		if result == math.Trunc(result) && result >= math.MinInt64 && result <= math.MaxInt64 {
			return int64(result), nil
		}
		return result, nil
	case "SIGN":
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "SIGN requires 1 argument")
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		switch n := v.(type) {
		case int64:
			if n > 0 {
				return int64(1), nil
			} else if n < 0 {
				return int64(-1), nil
			}
			return int64(0), nil
		case float64:
			if n > 0 {
				return float64(1), nil
			} else if n < 0 {
				return float64(-1), nil
			}
			return float64(0), nil
		}
		return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "SIGN: argument must be numeric")
	case "CONCAT", "CONCAT_WS":
		// CONCAT_WS(sep, s1, s2, ...) — first arg is separator.
		// CONCAT(s1, s2, ...) — no separator.
		sep := ""
		startIdx := 0
		if name == "CONCAT_WS" {
			if len(fArgs) < 1 {
				return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "CONCAT_WS requires at least 1 argument")
			}
			sv, err := eval(fArgs[0].Expression())
			if err != nil {
				return nil, err
			}
			if sv != nil {
				sep = fmt.Sprintf("%v", sv)
			}
			startIdx = 1
		}
		var parts []string
		for _, fa := range fArgs[startIdx:] {
			v, err := eval(fa.Expression())
			if err != nil {
				return nil, err
			}
			if v == nil {
				continue // NULL args skipped per SQL standard
			}
			parts = append(parts, fmt.Sprintf("%v", v))
		}
		return strings.Join(parts, sep), nil
	case "NULLIF":
		if len(fArgs) < 2 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "NULLIF requires 2 arguments")
		}
		a, err := eval(fArgs[0].Expression())
		if err != nil {
			return nil, err
		}
		b, err2 := eval(fArgs[1].Expression())
		if err2 != nil {
			return nil, err2
		}
		if compareValues(a, b) == 0 {
			return nil, nil
		}
		return a, nil
	case "GREATEST", "LEAST":
		if len(fArgs) == 0 {
			return nil, nil
		}
		best, err := eval(fArgs[0].Expression())
		if err != nil {
			return nil, err
		}
		isGreatest := name == "GREATEST"
		for _, fa := range fArgs[1:] {
			v, verr := eval(fa.Expression())
			if verr != nil {
				return nil, verr
			}
			if v == nil {
				continue
			}
			cmp := compareValues(v, best)
			if best == nil || (isGreatest && cmp > 0) || (!isGreatest && cmp < 0) {
				best = v
			}
		}
		return best, nil
	case "SUBSTRING", "SUBSTR":
		// SUBSTRING(str, pos [, len]) — 1-based position per SQL standard.
		if len(fArgs) < 2 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "SUBSTRING requires at least 2 arguments")
		}
		sv, err := eval(fArgs[0].Expression())
		if err != nil || sv == nil {
			return nil, err
		}
		s := fmt.Sprintf("%v", sv)
		posVal, posErr := eval(fArgs[1].Expression())
		if posErr != nil {
			return nil, posErr
		}
		pos, _ := posVal.(int64)
		if pos < 1 {
			pos = 1
		}
		runes := []rune(s)
		start := int(pos) - 1
		if start >= len(runes) {
			return "", nil
		}
		if len(fArgs) >= 3 {
			lenVal, lenErr := eval(fArgs[2].Expression())
			if lenErr != nil {
				return nil, lenErr
			}
			n, _ := lenVal.(int64)
			end := start + int(n)
			if end > len(runes) {
				end = len(runes)
			}
			if end < start {
				return "", nil
			}
			return string(runes[start:end]), nil
		}
		return string(runes[start:]), nil
	case "REPLACE":
		// REPLACE(str, from, to)
		if len(fArgs) < 3 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "REPLACE requires 3 arguments")
		}
		sv, err := eval(fArgs[0].Expression())
		if err != nil || sv == nil {
			return nil, err
		}
		fromV, err := eval(fArgs[1].Expression())
		if err != nil || fromV == nil {
			return nil, err
		}
		toV, err := eval(fArgs[2].Expression())
		if err != nil {
			return nil, err
		}
		toStr := ""
		if toV != nil {
			toStr = fmt.Sprintf("%v", toV)
		}
		return strings.ReplaceAll(fmt.Sprintf("%v", sv), fmt.Sprintf("%v", fromV), toStr), nil
	case "IF", "IIF":
		// IF(cond, true_val, false_val)
		if len(fArgs) < 3 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "IF requires 3 arguments")
		}
		cond, err := eval(fArgs[0].Expression())
		if err != nil {
			return nil, err
		}
		if isTruthy(cond) {
			return eval(fArgs[1].Expression())
		}
		return eval(fArgs[2].Expression())
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, unsupportedFmt, name)
	}
}

// makeProtoExprEvaluator builds the exprEvaluator adapter for the proto path.
// evalExpr returns (any, error); driver.Value is an alias for any so the
// conversion is a no-op except we explicitly preserve nil → nil.
func makeProtoExprEvaluator(ctx context.Context, conn *EmbeddedConnection, msg proto.Message) exprEvaluator {
	return func(e antlrgen.IExpressionContext) (driver.Value, error) {
		v, err := evalExpr(ctx, conn, msg, e)
		if err != nil {
			return nil, err
		}
		if v == nil {
			return nil, nil
		}
		return driver.Value(v), nil
	}
}

// makeMapExprEvaluator builds the exprEvaluator adapter for the map path.
func makeMapExprEvaluator(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value) exprEvaluator {
	return func(e antlrgen.IExpressionContext) (driver.Value, error) {
		return evalExprOnMap(ctx, conn, row, e)
	}
}

func evalScalarFunctionCall(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, fc antlrgen.IFunctionCallContext) (any, error) {
	eval := makeProtoExprEvaluator(ctx, conn, msg)
	predEval := func(e antlrgen.IExpressionContext) (bool, error) {
		return evalExprPredicate(ctx, conn, msg, e)
	}
	return evalScalarFunctionCallCore(eval, predEval, "unsupported scalar function %q", "unsupported specific function %T", fc)
}

func evalScalarFunctionCallOnMap(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, fc antlrgen.IFunctionCallContext) (driver.Value, error) {
	eval := makeMapExprEvaluator(ctx, conn, row)
	predEval := func(e antlrgen.IExpressionContext) (bool, error) {
		return evalPredicateOnMapExpr(ctx, conn, row, e)
	}
	return evalScalarFunctionCallCore(eval, predEval, "unsupported function %q in map eval context", "unsupported specific function %T in map eval", fc)
}

// evalSpecificFunctionCore is the unified implementation shared by
// evalSpecificFunction (proto path) and evalSpecificFunctionOnMap (map path).
// Handles grammar-level SpecificFunction nodes: CASE WHEN ... END, simple CASE,
// and CAST(expr AS type). The searched CASE branch needs a boolean predicate
// evaluator, hence predicateEval in addition to eval.
//
// unsupportedFmt must accept exactly one %T for the specific-function type.
func evalSpecificFunctionCore(
	eval exprEvaluator,
	predicateEval predicateEvaluator,
	unsupportedFmt string,
	sf antlrgen.ISpecificFunctionContext,
) (driver.Value, error) {
	switch c := sf.(type) {
	case *antlrgen.CaseFunctionCallContext:
		// Searched CASE: CASE WHEN cond THEN val ... [ELSE val] END
		// WHEN conditions are full boolean expressions (comparisons, AND/OR, etc.).
		for _, alt := range c.AllCaseFuncAlternative() {
			ok, err := predicateEval(alt.GetCondition().Expression())
			if err != nil {
				return nil, err
			}
			if ok {
				return eval(alt.GetConsequent().Expression())
			}
		}
		if c.GetElseArg() != nil {
			return eval(c.GetElseArg().Expression())
		}
		return nil, nil
	case *antlrgen.CaseExpressionFunctionCallContext:
		// Simple CASE: CASE expr WHEN val THEN result ... [ELSE result] END
		subject, err := eval(c.Expression())
		if err != nil {
			return nil, err
		}
		for _, alt := range c.AllCaseFuncAlternative() {
			whenVal, wErr := eval(alt.GetCondition().Expression())
			if wErr != nil {
				return nil, wErr
			}
			if compareValues(subject, whenVal) == 0 {
				return eval(alt.GetConsequent().Expression())
			}
		}
		if c.GetElseArg() != nil {
			return eval(c.GetElseArg().Expression())
		}
		return nil, nil
	case *antlrgen.DataTypeFunctionCallContext:
		// CAST(expr AS type)
		val, err := eval(c.Expression())
		if err != nil {
			return nil, err
		}
		if val == nil {
			return nil, nil // CAST(NULL AS type) = NULL
		}
		typeName := strings.ToUpper(c.ConvertedDataType().GetText())
		return castValue(val, typeName)
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, unsupportedFmt, sf)
	}
}

// evalSpecificFunction handles grammar-level SpecificFunction nodes: CASE WHEN ... END.
func evalSpecificFunction(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, sf antlrgen.ISpecificFunctionContext) (any, error) {
	eval := makeProtoExprEvaluator(ctx, conn, msg)
	predEval := func(e antlrgen.IExpressionContext) (bool, error) {
		return evalExprPredicate(ctx, conn, msg, e)
	}
	return evalSpecificFunctionCore(eval, predEval, "unsupported specific function %T", sf)
}

// evalSpecificFunctionOnMap is the map-eval variant of evalSpecificFunction: handles CASE WHEN and CAST.
func evalSpecificFunctionOnMap(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, sf antlrgen.ISpecificFunctionContext) (driver.Value, error) {
	eval := makeMapExprEvaluator(ctx, conn, row)
	predEval := func(e antlrgen.IExpressionContext) (bool, error) {
		return evalPredicateOnMapExpr(ctx, conn, row, e)
	}
	return evalSpecificFunctionCore(eval, predEval, "unsupported specific function %T in map eval", sf)
}

// castValue converts v to the SQL type named by typeName (e.g. "BIGINT", "VARCHAR", "TEXT", "BOOLEAN").
func castValue(v any, typeName string) (any, error) {
	switch {
	case strings.HasPrefix(typeName, "BIGINT"), strings.HasPrefix(typeName, "INT"), typeName == "INTEGER", typeName == "LONG":
		switch n := v.(type) {
		case int64:
			return n, nil
		case float64:
			return int64(n), nil
		case string:
			i, err := strconv.ParseInt(n, 10, 64)
			if err != nil {
				return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "cannot CAST %q to integer: %v", n, err)
			}
			return i, nil
		case bool:
			if n {
				return int64(1), nil
			}
			return int64(0), nil
		}
	case strings.HasPrefix(typeName, "FLOAT"), strings.HasPrefix(typeName, "DOUBLE"), strings.HasPrefix(typeName, "DECIMAL"), strings.HasPrefix(typeName, "NUMERIC"):
		switch n := v.(type) {
		case float64:
			return n, nil
		case int64:
			return float64(n), nil
		case string:
			f, err := strconv.ParseFloat(n, 64)
			if err != nil {
				return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "cannot CAST %q to float: %v", n, err)
			}
			return f, nil
		}
	case strings.HasPrefix(typeName, "VARCHAR"), strings.HasPrefix(typeName, "CHAR"), typeName == "TEXT", typeName == "STRING":
		switch n := v.(type) {
		case string:
			return n, nil
		case int64:
			return strconv.FormatInt(n, 10), nil
		case float64:
			return strconv.FormatFloat(n, 'g', -1, 64), nil
		case bool:
			if n {
				return "true", nil
			}
			return "false", nil
		}
	case typeName == "BOOLEAN", typeName == "BOOL":
		switch n := v.(type) {
		case bool:
			return n, nil
		case int64:
			return n != 0, nil
		case string:
			b, err := strconv.ParseBool(n)
			if err != nil {
				return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "cannot CAST %q to boolean: %v", n, err)
			}
			return b, nil
		}
	}
	return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported CAST from %T to %s", v, typeName)
}

// isTruthy returns true when v is a non-nil, non-zero boolean or non-zero numeric.
func isTruthy(v any) bool {
	if v == nil {
		return false
	}
	switch n := v.(type) {
	case bool:
		return n
	case int64:
		return n != 0
	case float64:
		return n != 0
	case string:
		return n != ""
	}
	return true
}

func applyMathOp(left, right any, op string) (any, error) {
	lf, lok := toFloat64(left)
	rf, rok := toFloat64(right)
	if !lok || !rok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"arithmetic operator %q requires numeric operands, got %T and %T", op, left, right)
	}
	var result float64
	switch op {
	case "+":
		result = lf + rf
	case "-":
		result = lf - rf
	case "*":
		result = lf * rf
	case "/":
		if rf == 0 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "division by zero")
		}
		result = lf / rf
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported math operator %q", op)
	}
	// Preserve int64 if both operands were integers and result is whole.
	_, leftIsInt := left.(int64)
	_, rightIsInt := right.(int64)
	if leftIsInt && rightIsInt && result == float64(int64(result)) {
		return int64(result), nil
	}
	return result, nil
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// execUpdate executes UPDATE <table> SET col = val [, ...] [WHERE col = val].
func (c *EmbeddedConnection) execUpdate(ctx context.Context, upd antlrgen.IUpdateStatementContext) (int64, error) {
	if c.schema == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.dbPath == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}

	tableName := fullIdToName(upd.TableName().FullId())
	whereExpr := upd.WhereExpr()
	updatedElems := upd.AllUpdatedElement()

	var updated int64
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		updated = 0
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.dbPath, c.schema)
		if loadErr != nil {
			return nil, loadErr
		}
		rlTmpl, tmplOk := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
		if !tmplOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "schema template is not a RecordLayerSchemaTemplate")
		}
		md := rlTmpl.Underlying()

		rt := md.GetRecordType(tableName)
		if rt == nil {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "table %q not found in schema", tableName)
		}
		msgDesc := rt.Descriptor

		ss, ssErr := c.ks.SchemaSubspace(c.dbPath, c.schema)
		if ssErr != nil {
			return nil, ssErr
		}
		store, storeErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(md).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		cursor := store.ScanRecordsByType(tableName, nil, recordlayer.ForwardScan())
		defer cursor.Close() //nolint:errcheck

		for {
			result, nextErr := cursor.OnNext(ctx)
			if nextErr != nil {
				return nil, nextErr
			}
			if !result.HasNext() {
				break
			}
			rec := result.GetValue()
			match, matchErr := evalPredicate(ctx, c, rec.Record, whereExpr)
			if matchErr != nil {
				return nil, matchErr
			}
			if !match {
				continue
			}

			cloned := proto.Clone(rec.Record)
			clonedRefl := cloned.ProtoReflect()
			for _, elem := range updatedElems {
				colName := fullIdToName(elem.FullColumnName().FullId())
				fd := msgDesc.Fields().ByName(protoreflect.Name(colName))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "column %q not found in table %q", colName, tableName)
				}
				val, evalErr := evalExpr(ctx, c, cloned, elem.Expression())
				if evalErr != nil {
					return nil, evalErr
				}
				if val == nil {
					clonedRefl.Clear(fd)
					continue
				}
				protoVal, convErr := convertToProtoValue(fd, val)
				if convErr != nil {
					return nil, convErr
				}
				clonedRefl.Set(fd, protoVal)
			}
			if _, saveErr := store.SaveRecord(cloned); saveErr != nil {
				return nil, saveErr
			}
			updated++
		}
		return nil, nil
	})
	if err != nil {
		return 0, err
	}
	return updated, nil
}

// execDelete executes DELETE FROM <table> [WHERE col = value].
func (c *EmbeddedConnection) execDelete(ctx context.Context, del antlrgen.IDeleteStatementContext) (int64, error) {
	if c.schema == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.dbPath == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}

	tableName := fullIdToName(del.TableName().FullId())
	whereExpr := del.WhereExpr()

	var deleted int64
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		deleted = 0
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.dbPath, c.schema)
		if loadErr != nil {
			return nil, loadErr
		}
		rlTmpl, tmplOk := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
		if !tmplOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "schema template is not a RecordLayerSchemaTemplate")
		}
		md := rlTmpl.Underlying()

		rt := md.GetRecordType(tableName)
		if rt == nil {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "table %q not found in schema", tableName)
		}

		ss, ssErr := c.ks.SchemaSubspace(c.dbPath, c.schema)
		if ssErr != nil {
			return nil, ssErr
		}
		store, storeErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(md).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		cursor := store.ScanRecordsByType(tableName, nil, recordlayer.ForwardScan())
		defer cursor.Close() //nolint:errcheck

		for {
			result, nextErr := cursor.OnNext(ctx)
			if nextErr != nil {
				return nil, nextErr
			}
			if !result.HasNext() {
				break
			}
			rec := result.GetValue()
			match, matchErr := evalPredicate(ctx, c, rec.Record, whereExpr)
			if matchErr != nil {
				return nil, matchErr
			}
			if !match {
				continue
			}
			if _, delErr := store.DeleteRecord(rec.PrimaryKey); delErr != nil {
				return nil, delErr
			}
			deleted++
		}
		return nil, nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// evalPredicate returns true if msg satisfies whereExpr.
// Only col = constant comparisons are supported. If whereExpr is nil, returns true.
func evalPredicate(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, whereExpr antlrgen.IWhereExprContext) (bool, error) {
	if whereExpr == nil {
		return true, nil
	}
	return evalExprPredicate(ctx, conn, msg, whereExpr.Expression())
}

// evalExprPredicate evaluates an IExpressionContext as a boolean predicate.
// Supports: col = constant, col != constant, col < constant, col > constant,
// col <= constant, col >= constant, AND, OR, NOT.
func evalExprPredicate(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, expr antlrgen.IExpressionContext) (bool, error) {
	switch e := expr.(type) {
	case *antlrgen.ExistsExpressionAtomContext:
		if conn == nil {
			return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "EXISTS subquery not supported in this context")
		}
		// NOTE: Correlated subqueries are not supported — the subquery cannot
		// reference column values from the outer query row.
		_, subRows, subErr := conn.execQueryBodyRows(ctx, e.Query().QueryExpressionBody())
		if subErr != nil {
			return false, subErr
		}
		return len(subRows) > 0, nil

	case *antlrgen.LogicalExpressionContext:
		left, err := evalExprPredicate(ctx, conn, msg, e.Expression(0))
		if err != nil {
			return false, err
		}
		op := e.LogicalOperator()
		if op.AND() != nil {
			if !left {
				return false, nil // short-circuit
			}
			return evalExprPredicate(ctx, conn, msg, e.Expression(1))
		}
		if op.OR() != nil {
			if left {
				return true, nil // short-circuit
			}
			return evalExprPredicate(ctx, conn, msg, e.Expression(1))
		}
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported logical operator %q", op.GetText())

	case *antlrgen.NotExpressionContext:
		v, err := evalExprPredicate(ctx, conn, msg, e.Expression())
		if err != nil {
			return false, err
		}
		return !v, nil

	case *antlrgen.PredicatedExpressionContext:
		if e.Predicate() != nil {
			switch p := e.Predicate().(type) {
			case *antlrgen.InPredicateContext:
				return evalInPredicate(ctx, conn, msg, e, p)
			case *antlrgen.IsExpressionContext:
				return evalIsNullPredicate(ctx, conn, msg, e, p)
			case *antlrgen.LikePredicateContext:
				return evalLikePredicate(ctx, conn, msg, e, p)
			case *antlrgen.BetweenComparisonPredicateContext:
				return evalBetweenPredicate(ctx, conn, msg, e, p)
			}
		}
		return evalComparisonPredicate(ctx, conn, msg, e)

	default:
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported WHERE expression type %T", expr)
	}
}

// evalComparisonPredicate handles a leaf comparison between two arbitrary expressions.
func evalComparisonPredicate(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext) (bool, error) {
	bcp, ok := pred.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
	if !ok {
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported WHERE predicate type %T", pred.ExpressionAtom())
	}
	opText := bcp.ComparisonOperator().GetText()

	left, err := evalExprAtom(ctx, conn, msg, bcp.GetLeft())
	if err != nil {
		return false, err
	}
	right, err := evalExprAtom(ctx, conn, msg, bcp.GetRight())
	if err != nil {
		return false, err
	}

	cmp := compareValues(left, right)
	switch opText {
	case "=":
		return cmp == 0, nil
	case "!=", "<>":
		return cmp != 0, nil
	case "<":
		return cmp < 0, nil
	case ">":
		return cmp > 0, nil
	case "<=":
		return cmp <= 0, nil
	case ">=":
		return cmp >= 0, nil
	default:
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported comparison operator %q", opText)
	}
}

// matchSubqueryIN checks whether fieldVal appears in the first column of subRows.
// Returns (matched, notNegated) following SQL IN/NOT IN semantics.
func matchSubqueryIN(fieldVal driver.Value, subRows [][]driver.Value, negated bool) bool {
	for _, row := range subRows {
		if len(row) == 0 {
			continue
		}
		if valuesEqual(fieldVal, row[0]) {
			return !negated
		}
	}
	return negated
}

// evalInPredicate handles: expr [NOT] IN (val1, val2, ...) or expr [NOT] IN (subquery)
func evalInPredicate(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext, in *antlrgen.InPredicateContext) (bool, error) {
	var fieldVal driver.Value
	if colAtom, ok := pred.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext); ok {
		// Column: use proto Has() so unset optionals (SQL NULL) return false.
		colName := fullIdToName(colAtom.FullColumnName().FullId())
		fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(colName))
		if fd == nil {
			return false, api.NewErrorf(api.ErrCodeInvalidParameter, "column %q not found", colName)
		}
		if !msg.ProtoReflect().Has(fd) {
			return false, nil // NULL IN (...) = UNKNOWN → treat as false
		}
		fieldVal = protoValueToDriver(fd, msg.ProtoReflect().Get(fd))
	} else {
		v, err := evalExprAtom(ctx, conn, msg, pred.ExpressionAtom())
		if err != nil {
			return false, err
		}
		if v == nil {
			return false, nil // NULL IN (...) = UNKNOWN
		}
		fieldVal = v
	}

	if qb := in.InList().QueryExpressionBody(); qb != nil {
		if conn == nil {
			return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "subquery IN not supported in this context")
		}
		_, subRows, err := conn.execQueryBodyRows(ctx, qb)
		if err != nil {
			return false, err
		}
		return matchSubqueryIN(fieldVal, subRows, in.NOT() != nil), nil
	}

	exprs := in.InList().Expressions().AllExpression()
	for _, expr := range exprs {
		ep, ok := expr.(*antlrgen.PredicatedExpressionContext)
		if !ok {
			return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "IN list value must be a constant, got %T", expr)
		}
		cAtom, ok := ep.ExpressionAtom().(*antlrgen.ConstantExpressionAtomContext)
		if !ok {
			return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "IN list value must be a constant, got atom %T", ep.ExpressionAtom())
		}
		litVal, err := evalConstant(cAtom.Constant())
		if err != nil {
			return false, err
		}
		if valuesEqual(fieldVal, litVal) {
			if in.NOT() != nil {
				return false, nil
			}
			return true, nil
		}
	}
	// none matched
	if in.NOT() != nil {
		return true, nil
	}
	return false, nil
}

// evalIsNullPredicate handles: expr IS [NOT] NULL / IS TRUE / IS FALSE
func evalIsNullPredicate(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext, is *antlrgen.IsExpressionContext) (bool, error) {
	// Evaluate the expression on the left side (may be a column, function call, etc.).
	var fieldVal driver.Value
	if colAtom, ok := pred.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext); ok {
		// Column: use proto Has() to distinguish NULL (unset optional) from zero.
		colName := fullIdToName(colAtom.FullColumnName().FullId())
		fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(colName))
		if fd == nil {
			return false, api.NewErrorf(api.ErrCodeInvalidParameter, "column %q not found", colName)
		}
		if msg.ProtoReflect().Has(fd) {
			fieldVal = protoValueToDriver(fd, msg.ProtoReflect().Get(fd))
		}
	} else {
		v, err := evalExprAtom(ctx, conn, msg, pred.ExpressionAtom())
		if err != nil {
			return false, err
		}
		fieldVal = v
	}
	negated := is.NOT() != nil

	switch {
	case is.NULL_LITERAL() != nil:
		isNull := fieldVal == nil
		if negated {
			return !isNull, nil
		}
		return isNull, nil
	case is.TRUE() != nil:
		b, ok := fieldVal.(bool)
		result := ok && b
		if negated {
			return !result, nil
		}
		return result, nil
	case is.FALSE() != nil:
		b, ok := fieldVal.(bool)
		result := ok && !b
		if negated {
			return !result, nil
		}
		return result, nil
	default:
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported IS test value")
	}
}

// evalLikePredicate handles: expr [NOT] LIKE 'pattern'
// Supports SQL wildcards: % (any sequence) and _ (any single character).
func evalLikePredicate(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext, like *antlrgen.LikePredicateContext) (bool, error) {
	rawVal, err := evalExprAtom(ctx, conn, msg, pred.ExpressionAtom())
	if err != nil {
		return false, err
	}
	if rawVal == nil {
		return false, nil // NULL LIKE anything = false
	}
	s, ok2 := rawVal.(string)
	if !ok2 {
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "LIKE requires a string expression, got %T", rawVal)
	}

	// Pattern is the first STRING_LITERAL token; strip surrounding quotes.
	patternLit := like.GetPattern().GetText()
	pattern := stripStringLiteralQuotes(patternLit)

	matched := likeMatch(pattern, s)
	if like.NOT() != nil {
		return !matched, nil
	}
	return matched, nil
}

// likeMatch implements SQL LIKE pattern matching: % = any sequence, _ = any single char.
func likeMatch(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	p, str := []rune(pattern), []rune(s)
	return likeMatchRunes(p, str)
}

func likeMatchRunes(p, s []rune) bool {
	for len(p) > 0 {
		switch p[0] {
		case '%':
			// skip consecutive %
			for len(p) > 0 && p[0] == '%' {
				p = p[1:]
			}
			if len(p) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if likeMatchRunes(p, s[i:]) {
					return true
				}
			}
			return false
		case '_':
			if len(s) == 0 {
				return false
			}
			p, s = p[1:], s[1:]
		default:
			if len(s) == 0 || p[0] != s[0] {
				return false
			}
			p, s = p[1:], s[1:]
		}
	}
	return len(s) == 0
}

// stripStringLiteralQuotes removes surrounding single-quotes and unescapes ” → '.
func stripStringLiteralQuotes(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		s = s[1 : len(s)-1]
	}
	return strings.ReplaceAll(s, "''", "'")
}

// evalBetweenPredicate handles: expr [NOT] BETWEEN lo AND hi (inclusive).
func evalBetweenPredicate(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext, bet *antlrgen.BetweenComparisonPredicateContext) (bool, error) {
	fieldVal, err := evalExprAtom(ctx, conn, msg, pred.ExpressionAtom())
	if err != nil {
		return false, err
	}
	if fieldVal == nil {
		return false, nil // NULL BETWEEN ... = UNKNOWN
	}

	lo, err := evalExprAtom(ctx, conn, msg, bet.GetLeft())
	if err != nil {
		return false, err
	}
	hi, err := evalExprAtom(ctx, conn, msg, bet.GetRight())
	if err != nil {
		return false, err
	}

	result := compareValues(fieldVal, lo) >= 0 && compareValues(fieldVal, hi) <= 0
	if bet.NOT() != nil {
		return !result, nil
	}
	return result, nil
}

// groupByKey builds a comparable string key from the group-by column values.
func groupByKey(groupVals []driver.Value) string {
	var b strings.Builder
	for _, v := range groupVals {
		fmt.Fprintf(&b, "%v\x00", v)
	}
	return b.String()
}

// evalHaving evaluates a HAVING clause expression against a map of
// output-column-name → aggregate value.
// Supports comparisons, AND/OR/NOT, and aggregate function references.
func evalHaving(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, expr antlrgen.IExpressionContext) (bool, error) {
	// EXISTS subquery
	if exists, ok := expr.(*antlrgen.ExistsExpressionAtomContext); ok {
		if conn == nil {
			return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "EXISTS subquery not supported in this context")
		}
		_, subRows, subErr := conn.execQueryBodyRows(ctx, exists.Query().QueryExpressionBody())
		if subErr != nil {
			return false, subErr
		}
		return len(subRows) > 0, nil
	}
	// Handle logical expressions: AND / OR
	if le, ok := expr.(*antlrgen.LogicalExpressionContext); ok {
		left, err := evalHaving(ctx, conn, row, le.Expression(0))
		if err != nil {
			return false, err
		}
		op := strings.ToUpper(le.LogicalOperator().GetText())
		if op == "AND" || op == "&&" {
			if !left {
				return false, nil
			}
			return evalHaving(ctx, conn, row, le.Expression(1))
		}
		// OR
		if left {
			return true, nil
		}
		return evalHaving(ctx, conn, row, le.Expression(1))
	}
	// Handle NOT
	if ne, ok := expr.(*antlrgen.NotExpressionContext); ok {
		v, err := evalHaving(ctx, conn, row, ne.Expression())
		return !v, err
	}
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported HAVING expression %T", expr)
	}
	// WHERE-style predicate: expressionAtom + separate predicate (IS NULL, LIKE, BETWEEN, IN, =).
	if pred.Predicate() != nil {
		return evalPredicateOnMap(ctx, conn, row, pred)
	}
	// HAVING-style: the full comparison is the expression atom (BinaryComparisonPredicateContext).
	compPred, ok := pred.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
	if !ok {
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "HAVING supports only comparison predicates, got %T", pred.ExpressionAtom())
	}

	resolveAtom := func(atom antlrgen.IExpressionAtomContext) (driver.Value, error) {
		switch a := atom.(type) {
		case *antlrgen.ConstantExpressionAtomContext:
			return evalConstant(a.Constant())
		case *antlrgen.FullColumnNameExpressionAtomContext:
			name := fullIdToName(a.FullColumnName().FullId())
			v, found := row[name]
			if !found {
				return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "HAVING column %q not in SELECT list", name)
			}
			return v, nil
		case *antlrgen.FunctionCallExpressionAtomContext:
			// Aggregate function reference — match by reconstructed output name.
			agg, aggok := a.FunctionCall().(*antlrgen.AggregateFunctionCallContext)
			if !aggok {
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported HAVING function call %T", a.FunctionCall())
			}
			awf, awfok := agg.AggregateWindowedFunction().(*antlrgen.AggregateWindowedFunctionContext)
			if !awfok {
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported HAVING aggregate %T", agg.AggregateWindowedFunction())
			}
			var lookupName string
			switch {
			case awf.COUNT() != nil && awf.STAR() != nil:
				lookupName = "COUNT(*)"
			case awf.COUNT() != nil && awf.FunctionArg() != nil:
				lookupName = "COUNT(" + awf.FunctionArg().GetText() + ")"
			case awf.SUM() != nil && awf.FunctionArg() != nil:
				lookupName = "SUM(" + awf.FunctionArg().GetText() + ")"
			case awf.MIN() != nil && awf.FunctionArg() != nil:
				lookupName = "MIN(" + awf.FunctionArg().GetText() + ")"
			case awf.MAX() != nil && awf.FunctionArg() != nil:
				lookupName = "MAX(" + awf.FunctionArg().GetText() + ")"
			case awf.AVG() != nil && awf.FunctionArg() != nil:
				lookupName = "AVG(" + awf.FunctionArg().GetText() + ")"
			}
			v, found := row[lookupName]
			if !found {
				return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "HAVING aggregate %q not in SELECT list", lookupName)
			}
			return v, nil
		default:
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported HAVING atom %T", atom)
		}
	}

	leftVal, err := resolveAtom(compPred.GetLeft())
	if err != nil {
		return false, err
	}
	rightVal, err := resolveAtom(compPred.GetRight())
	if err != nil {
		return false, err
	}
	opText := compPred.ComparisonOperator().GetText()
	cmp := compareValues(leftVal, rightVal)
	switch opText {
	case "=":
		return cmp == 0, nil
	case "!=", "<>":
		return cmp != 0, nil
	case "<":
		return cmp < 0, nil
	case ">":
		return cmp > 0, nil
	case "<=":
		return cmp <= 0, nil
	case ">=":
		return cmp >= 0, nil
	}
	return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "HAVING: unsupported operator %q", opText)
}

// evalExprAtomOnMap resolves an expression atom using a map[string]driver.Value
// row (used for JOIN WHERE and ON condition evaluation).
func evalExprAtomOnMap(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, atom antlrgen.IExpressionAtomContext) (driver.Value, error) {
	switch a := atom.(type) {
	case *antlrgen.ConstantExpressionAtomContext:
		v, err := evalConstant(a.Constant())
		if err != nil {
			return nil, err
		}
		return v, nil
	case *antlrgen.FullColumnNameExpressionAtomContext:
		name := fullIdToName(a.FullColumnName().FullId())
		v, found := row[name]
		if !found {
			// Try unqualified: "Order.amount" → "amount".
			if dot := strings.LastIndex(name, "."); dot >= 0 {
				v, found = row[name[dot+1:]]
			}
		}
		if !found {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "column %q not found in row", name)
		}
		return v, nil
	case *antlrgen.BinaryComparisonPredicateContext:
		left, err := evalExprAtomOnMap(ctx, conn, row, a.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := evalExprAtomOnMap(ctx, conn, row, a.GetRight())
		if err != nil {
			return nil, err
		}
		cmp := compareValues(left, right)
		switch a.ComparisonOperator().GetText() {
		case "=":
			return cmp == 0, nil
		case "!=", "<>":
			return cmp != 0, nil
		case "<":
			return cmp < 0, nil
		case ">":
			return cmp > 0, nil
		case "<=":
			return cmp <= 0, nil
		case ">=":
			return cmp >= 0, nil
		}
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported comparison operator")
	case *antlrgen.MathExpressionAtomContext:
		left, err := evalExprAtomOnMap(ctx, conn, row, a.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := evalExprAtomOnMap(ctx, conn, row, a.GetRight())
		if err != nil {
			return nil, err
		}
		return applyArithmeticOp(left, right, a.MathOperator().GetText())
	case *antlrgen.FunctionCallExpressionAtomContext:
		return evalScalarFunctionCallOnMap(ctx, conn, row, a.FunctionCall())
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported expression atom type %T in map eval", atom)
	}
}

// evalExprOnMap evaluates a scalar IExpressionContext against a map row, returning
// a driver.Value. Handles arithmetic, column refs, constants, and nested expressions.
func evalExprOnMap(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, expr antlrgen.IExpressionContext) (driver.Value, error) {
	switch e := expr.(type) {
	case *antlrgen.PredicatedExpressionContext:
		if e.Predicate() != nil {
			ok, err := evalPredicateOnMap(ctx, conn, row, e)
			if err != nil {
				return nil, err
			}
			return ok, nil
		}
		return evalExprAtomOnMap(ctx, conn, row, e.ExpressionAtom())
	case *antlrgen.LogicalExpressionContext:
		ok, err := evalPredicateOnMapExpr(ctx, conn, row, expr)
		if err != nil {
			return nil, err
		}
		return ok, nil
	case *antlrgen.NotExpressionContext:
		v, err := evalPredicateOnMapExpr(ctx, conn, row, e.Expression())
		if err != nil {
			return nil, err
		}
		return !v, nil
	case *antlrgen.ExistsExpressionAtomContext:
		ok, err := evalPredicateOnMapExpr(ctx, conn, row, expr)
		if err != nil {
			return nil, err
		}
		return ok, nil
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported expression type %T in map eval", expr)
	}
}

// evalPredicateOnMap evaluates a WHERE-style PredicatedExpressionContext against
// a map[string]driver.Value row. Handles IS NULL, LIKE, BETWEEN, IN, comparisons.
func evalPredicateOnMap(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, pred *antlrgen.PredicatedExpressionContext) (bool, error) {
	fieldVal, err := evalExprAtomOnMap(ctx, conn, row, pred.ExpressionAtom())
	if err != nil {
		return false, err
	}

	if pred.Predicate() == nil {
		// Leaf expression (e.g. a boolean constant) — treat truthiness.
		return isTruthy(fieldVal), nil
	}

	switch p := pred.Predicate().(type) {
	case *antlrgen.IsExpressionContext:
		negated := p.NOT() != nil
		isNull := fieldVal == nil
		switch {
		case p.NULL_LITERAL() != nil:
			res := isNull
			if negated {
				return !res, nil
			}
			return res, nil
		case p.TRUE() != nil:
			b, _ := fieldVal.(bool)
			res := b
			if negated {
				return !res, nil
			}
			return res, nil
		case p.FALSE() != nil:
			b, _ := fieldVal.(bool)
			res := !b && fieldVal != nil
			if negated {
				return !res, nil
			}
			return res, nil
		}
		return false, nil

	case *antlrgen.LikePredicateContext:
		if fieldVal == nil {
			return false, nil
		}
		s, ok := fieldVal.(string)
		if !ok {
			s = fmt.Sprintf("%v", fieldVal)
		}
		patternLit := p.GetPattern().GetText()
		matched := likeMatch(stripStringLiteralQuotes(patternLit), s)
		if p.NOT() != nil {
			return !matched, nil
		}
		return matched, nil

	case *antlrgen.BetweenComparisonPredicateContext:
		if fieldVal == nil {
			return false, nil
		}
		lo, loErr := evalExprAtomOnMap(ctx, conn, row, p.GetLeft())
		if loErr != nil {
			return false, loErr
		}
		hi, hiErr := evalExprAtomOnMap(ctx, conn, row, p.GetRight())
		if hiErr != nil {
			return false, hiErr
		}
		result := compareValues(fieldVal, lo) >= 0 && compareValues(fieldVal, hi) <= 0
		if p.NOT() != nil {
			return !result, nil
		}
		return result, nil

	case *antlrgen.InPredicateContext:
		if fieldVal == nil {
			return false, nil // NULL IN/NOT IN (...) = UNKNOWN → false
		}
		if qb := p.InList().QueryExpressionBody(); qb != nil {
			if conn == nil {
				return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "subquery IN not supported in this context")
			}
			_, subRows, subErr := conn.execQueryBodyRows(ctx, qb)
			if subErr != nil {
				return false, subErr
			}
			return matchSubqueryIN(fieldVal, subRows, p.NOT() != nil), nil
		}
		if p.InList().Expressions() == nil {
			return p.NOT() != nil, nil
		}
		for _, inExpr := range p.InList().Expressions().AllExpression() {
			ep, ok := inExpr.(*antlrgen.PredicatedExpressionContext)
			if !ok {
				continue
			}
			litVal, litErr := evalExprAtomOnMap(ctx, conn, row, ep.ExpressionAtom())
			if litErr != nil {
				return false, litErr
			}
			if valuesEqual(fieldVal, litVal) {
				if p.NOT() != nil {
					return false, nil
				}
				return true, nil
			}
		}
		if p.NOT() != nil {
			return true, nil
		}
		return false, nil
	}

	// Fallback: interpret as binary comparison (the predicate part has = / <> / < / > / <= / >=).
	// This happens when the predicate is NOT a special predicate type above.
	bcp, ok := pred.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
	if ok {
		rightVal, err := evalExprAtomOnMap(ctx, conn, row, bcp.GetRight())
		if err != nil {
			return false, err
		}
		cmp := compareValues(fieldVal, rightVal)
		switch bcp.ComparisonOperator().GetText() {
		case "=":
			return cmp == 0, nil
		case "!=", "<>":
			return cmp != 0, nil
		case "<":
			return cmp < 0, nil
		case ">":
			return cmp > 0, nil
		case "<=":
			return cmp <= 0, nil
		case ">=":
			return cmp >= 0, nil
		}
	}
	return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported predicate type %T in map eval", pred.Predicate())
}

// evalPredicateOnMapExpr evaluates a general IExpressionContext against a map row.
// Mirrors evalExprPredicate but uses map-based column resolution.
func evalPredicateOnMapExpr(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, expr antlrgen.IExpressionContext) (bool, error) {
	switch e := expr.(type) {
	case *antlrgen.ExistsExpressionAtomContext:
		if conn == nil {
			return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "EXISTS subquery not supported in this context")
		}
		_, subRows, subErr := conn.execQueryBodyRows(ctx, e.Query().QueryExpressionBody())
		if subErr != nil {
			return false, subErr
		}
		return len(subRows) > 0, nil
	case *antlrgen.LogicalExpressionContext:
		left, err := evalPredicateOnMapExpr(ctx, conn, row, e.Expression(0))
		if err != nil {
			return false, err
		}
		op := e.LogicalOperator()
		if op.AND() != nil {
			if !left {
				return false, nil
			}
			return evalPredicateOnMapExpr(ctx, conn, row, e.Expression(1))
		}
		if left {
			return true, nil
		}
		return evalPredicateOnMapExpr(ctx, conn, row, e.Expression(1))
	case *antlrgen.NotExpressionContext:
		v, err := evalPredicateOnMapExpr(ctx, conn, row, e.Expression())
		if err != nil {
			return false, err
		}
		return !v, nil
	case *antlrgen.PredicatedExpressionContext:
		if e.Predicate() != nil {
			return evalPredicateOnMap(ctx, conn, row, e)
		}
		// No separate predicate — expression atom (e.g. BinaryComparisonPredicateContext).
		bcp, ok := e.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
		if ok {
			left, err := evalExprAtomOnMap(ctx, conn, row, bcp.GetLeft())
			if err != nil {
				return false, err
			}
			right, err := evalExprAtomOnMap(ctx, conn, row, bcp.GetRight())
			if err != nil {
				return false, err
			}
			cmp := compareValues(left, right)
			switch bcp.ComparisonOperator().GetText() {
			case "=":
				return cmp == 0, nil
			case "!=", "<>":
				return cmp != 0, nil
			case "<":
				return cmp < 0, nil
			case ">":
				return cmp > 0, nil
			case "<=":
				return cmp <= 0, nil
			case ">=":
				return cmp >= 0, nil
			}
		}
		v, err := evalExprAtomOnMap(ctx, conn, row, e.ExpressionAtom())
		if err != nil {
			return false, err
		}
		return isTruthy(v), nil
	default:
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported WHERE expression type %T in map eval", expr)
	}
}

// applyArithmeticOp applies +/-/*// to two driver.Values (used in map-based eval).
func applyArithmeticOp(left, right driver.Value, op string) (driver.Value, error) {
	lf, lok := toFloat64(left)
	rf, rok := toFloat64(right)
	if !lok || !rok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "arithmetic requires numeric operands, got %T and %T", left, right)
	}
	switch op {
	case "+":
		return lf + rf, nil
	case "-":
		return lf - rf, nil
	case "*":
		return lf * rf, nil
	case "/":
		if rf == 0 {
			return nil, nil // NULL for division by zero
		}
		return lf / rf, nil
	case "%":
		if rf == 0 {
			return nil, nil
		}
		return math.Mod(lf, rf), nil
	}
	return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported arithmetic operator %q", op)
}

// substituteParams replaces positional '?' placeholders in a query with
// SQL literal representations of the supplied driver values. Named params
// (@name) are not supported — only positional '?' is handled.
func substituteParams(query string, args []driver.NamedValue) (string, error) {
	if len(args) == 0 {
		return query, nil
	}
	var b strings.Builder
	argIdx := 0
	for i := 0; i < len(query); i++ {
		ch := query[i]
		// Skip single-quoted string literals so a '?' inside a string value
		// is not treated as a placeholder.
		if ch == '\'' {
			b.WriteByte(ch)
			i++
			for i < len(query) {
				c := query[i]
				b.WriteByte(c)
				if c == '\'' {
					if i+1 < len(query) && query[i+1] == '\'' {
						// escaped quote inside string
						i++
						b.WriteByte(query[i])
					} else {
						break
					}
				}
				i++
			}
			continue
		}
		if ch != '?' {
			b.WriteByte(ch)
			continue
		}
		if argIdx >= len(args) {
			return "", api.NewErrorf(api.ErrCodeInvalidParameter,
				"more '?' placeholders than bound parameters (placeholder %d, have %d args)",
				argIdx+1, len(args))
		}
		v := args[argIdx].Value
		argIdx++
		switch val := v.(type) {
		case nil:
			b.WriteString("NULL")
		case bool:
			if val {
				b.WriteString("TRUE")
			} else {
				b.WriteString("FALSE")
			}
		case int64:
			fmt.Fprintf(&b, "%d", val)
		case float64:
			fmt.Fprintf(&b, "%g", val)
		case string:
			// Escape single quotes by doubling them.
			b.WriteByte('\'')
			b.WriteString(strings.ReplaceAll(val, "'", "''"))
			b.WriteByte('\'')
		case []byte:
			// Represent as hex string literal using SQL standard X'...' is not
			// in our grammar — encode as quoted string for now.
			b.WriteByte('\'')
			for _, bv := range val {
				fmt.Fprintf(&b, "%02x", bv)
			}
			b.WriteByte('\'')
		default:
			return "", api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"unsupported parameter type %T for placeholder %d", v, argIdx)
		}
	}
	if argIdx < len(args) {
		return "", api.NewErrorf(api.ErrCodeInvalidParameter,
			"fewer '?' placeholders than bound parameters (%d placeholders, %d args)",
			argIdx, len(args))
	}
	return b.String(), nil
}

// evalConstant evaluates a constant parse-tree node to a Go value.
// Returns nil for NULL.
func evalConstant(c antlrgen.IConstantContext) (any, error) {
	switch cv := c.(type) {
	case *antlrgen.DecimalConstantContext:
		text := cv.DecimalLiteral().GetText()
		if iv, err := strconv.ParseInt(text, 10, 64); err == nil {
			return iv, nil
		}
		fv, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "cannot parse decimal literal %q: %v", text, err)
		}
		return fv, nil
	case *antlrgen.NegativeDecimalConstantContext:
		text := "-" + cv.DecimalLiteral().GetText()
		if iv, err := strconv.ParseInt(text, 10, 64); err == nil {
			return iv, nil
		}
		fv, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "cannot parse decimal literal %q: %v", text, err)
		}
		return fv, nil
	case *antlrgen.StringConstantContext:
		raw := cv.StringLiteral().GetText()
		if len(raw) >= 2 {
			raw = raw[1 : len(raw)-1]
		}
		// Unescape doubled single-quotes produced by substituteParams or typed literally.
		raw = strings.ReplaceAll(raw, "''", "'")
		return raw, nil
	case *antlrgen.NullConstantContext:
		return nil, nil
	case *antlrgen.BooleanConstantContext:
		return cv.BooleanLiteral().TRUE() != nil, nil
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported constant type %T in WHERE", c)
	}
}

// valuesEqual compares two driver values that may have different numeric types.
func valuesEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	// Exact int64 comparison avoids float64 precision loss for large integers
	// (> 2^53 cannot be represented exactly as float64).
	if ai, ok1 := a.(int64); ok1 {
		if bi, ok2 := b.(int64); ok2 {
			return ai == bi
		}
	}
	// Normalise mixed int64/float64 pairs to float64.
	toFloat := func(v any) (float64, bool) {
		switch n := v.(type) {
		case int64:
			return float64(n), true
		case float64:
			return n, true
		}
		return 0, false
	}
	fa, aIsNum := toFloat(a)
	fb, bIsNum := toFloat(b)
	if aIsNum && bIsNum {
		return fa == fb
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// compareValues returns -1, 0, or 1 for two driver.Values.
// Used by ORDER BY post-sort. NULL sorts last.
func compareValues(a, b driver.Value) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return 1
	}
	if b == nil {
		return -1
	}
	switch av := a.(type) {
	case int64:
		switch bv := b.(type) {
		case int64:
			if av < bv {
				return -1
			}
			if av > bv {
				return 1
			}
			return 0
		case float64:
			fav := float64(av)
			if fav < bv {
				return -1
			}
			if fav > bv {
				return 1
			}
			return 0
		}
	case float64:
		var fbv float64
		switch bv := b.(type) {
		case float64:
			fbv = bv
		case int64:
			fbv = float64(bv)
		default:
			return strings.Compare(fmt.Sprintf("%v", a), fmt.Sprintf("%v", b))
		}
		if av < fbv {
			return -1
		}
		if av > fbv {
			return 1
		}
		return 0
	case string:
		if bv, ok := b.(string); ok {
			return strings.Compare(av, bv)
		}
	case bool:
		if bv, ok := b.(bool); ok {
			if av == bv {
				return 0
			}
			if !av {
				return -1
			}
			return 1
		}
	}
	return strings.Compare(fmt.Sprintf("%v", a), fmt.Sprintf("%v", b))
}

// rowKey serializes a result row to a collision-free string key for DISTINCT deduplication.
// Each field is length-prefixed: "<type-tag>:<len>:<bytes>|" so that string values
// containing separator characters cannot collide with other fields or NULL markers.
func rowKey(row []driver.Value) string {
	var b strings.Builder
	for _, v := range row {
		if v == nil {
			b.WriteString("N:0:|")
			continue
		}
		s := fmt.Sprintf("%T\x00%v", v, v)
		fmt.Fprintf(&b, "V:%d:%s|", len(s), s)
	}
	return b.String()
}

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
	action := c.factory.CreateDatabase(dbPath, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execDropDatabase(ctx context.Context, s *antlrgen.DropDatabaseStatementContext) (int64, error) {
	dbPath := s.Path().GetText()
	if err := validateDatabasePath(dbPath); err != nil {
		return 0, err
	}
	throwIfNotExist := s.IfExists() == nil
	action := c.factory.DropDatabase(dbPath, throwIfNotExist, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execCreateSchema(ctx context.Context, s *antlrgen.CreateSchemaStatementContext) (int64, error) {
	schemaText := s.SchemaId().GetText()
	dbPath, schemaName, err := parseSchemaIdentifier(schemaText, c.dbPath)
	if err != nil {
		return 0, err
	}
	templateID := s.SchemaTemplateId().GetText()
	action := c.factory.CreateSchema(dbPath, schemaName, templateID, *api.NoOptions())
	return 0, c.runDDL(ctx, action)
}

func (c *EmbeddedConnection) execDropSchema(ctx context.Context, s *antlrgen.DropSchemaStatementContext) (int64, error) {
	schemaText := s.Uid().GetText()
	dbPath, schemaName, err := parseSchemaIdentifier(schemaText, c.dbPath)
	if err != nil {
		return 0, err
	}
	if dbPath == "" {
		return 0, api.NewErrorf(api.ErrCodeUnknownDatabase,
			"invalid database identifier in %q", schemaText)
	}
	action := c.factory.DropSchema(dbPath, schemaName, *api.NoOptions())
	if err := c.runDDL(ctx, action); err != nil {
		return 0, err
	}
	c.invalidateSchemaCache(dbPath, schemaName)
	return 0, nil
}

func (c *EmbeddedConnection) execDropSchemaTemplate(ctx context.Context, s *antlrgen.DropSchemaTemplateStatementContext) (int64, error) {
	templateID := s.Uid().GetText()
	throwIfNotExist := s.IfExists() == nil
	action := c.factory.DropSchemaTemplate(templateID, throwIfNotExist, *api.NoOptions())
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
		tableName := td.Uid().GetText()
		cols, pkCols, err := parseTableDefinition(td)
		if err != nil {
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
			return 0, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate, "index: %v", err)
		}
	}

	tmpl, err := b.Build()
	if err != nil {
		return 0, err
	}
	action := c.factory.SaveSchemaTemplate(tmpl, *api.NoOptions())
	if err := c.runDDL(ctx, action); err != nil {
		return 0, err
	}
	// Template change may affect any schema using it — flush the whole cache.
	c.schemaCache = make(map[string]api.Schema)
	return 0, nil
}

// parseIndexDefinition handles a single CREATE INDEX clause within a schema template.
// Only INDEX ON SOURCE form (INDEX name ON table (cols)) is supported.
func parseIndexDefinition(idxDef antlrgen.IIndexDefinitionContext, b *metadata.Builder) error {
	switch def := idxDef.(type) {
	case *antlrgen.IndexOnSourceDefinitionContext:
		indexName := def.GetIndexName().GetText()
		tableName := def.GetSource().GetText()
		unique := def.UNIQUE() != nil
		var cols []string
		if cl := def.IndexColumnList(); cl != nil {
			for _, spec := range cl.AllIndexColumnSpec() {
				cols = append(cols, spec.GetColumnName().GetText())
			}
		}
		if len(cols) == 0 {
			return fmt.Errorf("index %q has no columns", indexName)
		}
		b.AddIndex(tableName, indexName, cols, unique)
		return nil
	default:
		return fmt.Errorf("unsupported index definition type %T; only INDEX … ON … is supported", idxDef)
	}
}

// parseTableDefinition extracts column specs and primary key column
// names from a TableDefinitionContext.
func parseTableDefinition(td antlrgen.ITableDefinitionContext) ([]metadata.ColumnSpec, []string, error) {
	var cols []metadata.ColumnSpec
	var pkCols []string

	for i, colDef := range td.AllColumnDefinition() {
		colName := colDef.Uid().GetText()
		ct := colDef.ColumnType()
		if ct == nil {
			return nil, nil, fmt.Errorf("column %q has no type", colName)
		}
		nullable := true
		if cc := colDef.ColumnConstraint(); cc != nil {
			if nc, ok := cc.(*antlrgen.NullColumnConstraintContext); ok {
				if nn := nc.NullNotnull(); nn != nil && nn.NOT() != nil {
					nullable = false
				}
			}
		}
		dt, err := parseColumnType(ct, nullable)
		if err != nil {
			return nil, nil, fmt.Errorf("column %q: %w", colName, err)
		}
		cols = append(cols, metadata.NewColumnSpec(colName, dt, int32(i+1)))
	}

	if pkDef := td.PrimaryKeyDefinition(); pkDef != nil {
		for _, fullID := range pkDef.FullIdList().AllFullId() {
			pkCols = append(pkCols, fullID.GetText())
		}
	}

	return cols, pkCols, nil
}

// parseColumnType maps a ColumnTypeContext to an api.DataType.
func parseColumnType(ct antlrgen.IColumnTypeContext, nullable bool) (api.DataType, error) {
	pt := ct.PrimitiveType()
	if pt == nil {
		return nil, fmt.Errorf("only primitive column types are supported")
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
	default:
		return nil, fmt.Errorf("unsupported column type: %s", ct.GetText())
	}
}

// ensureCatalogInit bootstraps the catalog. Retries on transient failure
// (unlike sync.Once, a mutex+bool allows retry when the previous attempt failed).
func (c *EmbeddedConnection) ensureCatalogInit(ctx context.Context) error {
	c.catalogMu.Lock()
	defer c.catalogMu.Unlock()
	if c.catalogReady {
		return nil
	}
	_, err := c.fdbDB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		txn := catalog.NewFDBTransaction(rctx)
		if initErr := c.cat.Initialize(txn); initErr != nil {
			return nil, initErr
		}
		return nil, txn.Commit()
	})
	if err != nil {
		return err
	}
	c.catalogReady = true
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
	_, err := c.fdbDB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
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

// embeddedStmt is a prepared DDL statement (no bind parameters).
type embeddedStmt struct {
	conn  *EmbeddedConnection
	query string
}

func (s *embeddedStmt) Close() error { return nil }

func (s *embeddedStmt) NumInput() int { return -1 } // unknown, variadic-safe

func (s *embeddedStmt) Exec(args []driver.Value) (driver.Result, error) {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return s.conn.ExecContext(context.Background(), s.query, named)
}

func (s *embeddedStmt) Query(args []driver.Value) (driver.Rows, error) {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	// TODO: driver.Stmt.Query has no context parameter; use context.Background() until
	// database/sql upgrades all call sites to QueryContext.
	return s.conn.QueryContext(context.Background(), s.query, named)
}

// Static interface checks.
var (
	_ driver.Conn               = (*EmbeddedConnection)(nil)
	_ driver.ExecerContext      = (*EmbeddedConnection)(nil)
	_ driver.QueryerContext     = (*EmbeddedConnection)(nil)
	_ driver.Pinger             = (*EmbeddedConnection)(nil)
	_ driver.ConnBeginTx        = (*EmbeddedConnection)(nil)
	_ driver.SessionResetter    = (*EmbeddedConnection)(nil)
	_ driver.Validator          = (*EmbeddedConnection)(nil)
	_ driver.ConnPrepareContext = (*EmbeddedConnection)(nil)
)

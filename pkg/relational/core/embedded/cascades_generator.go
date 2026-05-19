package embedded

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/antlr4-go/antlr/v4"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/executor"
	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// cascadesGenerator routes SELECT queries through the Cascades planner
// and executor. DML and DDL statements fall back to the naive generator.
type cascadesGenerator struct {
	c     *EmbeddedConnection
	naive *naiveGenerator
	cache *PlanCache
}

func newCascadesGenerator(c *EmbeddedConnection) *cascadesGenerator {
	if c.planCache == nil {
		c.planCache = NewPlanCache(256)
	}
	return &cascadesGenerator{
		c:     c,
		naive: &naiveGenerator{c: c},
		cache: c.planCache,
	}
}

func (g *cascadesGenerator) Plan(ctx context.Context, sql string) (query.Plan, error) {
	root, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}

	stmts := root.Statements()
	if stmts == nil || len(stmts.AllStatement()) != 1 {
		return g.naive.Plan(ctx, sql)
	}

	stmt := stmts.AllStatement()[0]

	// DML: route INSERT/UPDATE/DELETE through Cascades.
	if dml := stmt.DmlStatement(); dml != nil {
		return g.planDML(ctx, dml)
	}

	sel := stmt.SelectStatement()
	if sel == nil {
		// SHOW / DDL / other non-SELECT/DML statements stay on naive.
		return g.naive.Plan(ctx, sql)
	}

	q := sel.Query()
	if q == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "malformed SELECT statement")
	}

	if referencesInformationSchema(q) {
		return g.naive.Plan(ctx, sql)
	}

	if err := g.c.ensureMetaData(ctx); err != nil {
		return nil, err
	}
	md := g.c.cachedMetaData()
	if md == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"no schema metadata available")
	}

	// Check plan cache before running the full Cascades pipeline.
	sqlHash := QueryHash(sql)
	if g.cache != nil {
		if cachedPlan, cachedSubs, ok := g.cache.Get(sqlHash); ok {
			return &cascadesPlan{
				conn:             g.c,
				md:               md,
				physicalPlan:     cachedPlan,
				explain:          cachedPlan.Explain(),
				scalarSubqueries: cachedSubs,
				sqlLimit:         -1,
			}, nil
		}
	}

	visitor := NewPlanVisitor(md)
	logicalOp, buildErr := visitor.VisitQuery(q)
	if buildErr != nil {
		return nil, buildErr
	}
	if logicalOp == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"Cascades planner could not plan query")
	}

	if fn := query.FindUnsupportedFunction(logicalOp); fn != "" {
		return nil, api.NewError(api.ErrCodeUndefinedFunction,
			"Unsupported operator "+fn)
	}

	if err := resolveQualifiedTableNames(logicalOp, g.c.sess.Schema); err != nil {
		return nil, err
	}

	if err := validateTablesAndColumns(logicalOp, md); err != nil {
		return nil, err
	}

	if msg := findDistinctAggregate(logicalOp); msg != "" {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, msg)
	}

	ref, scalarSubqueryPlans := query.TranslateToCascadesWithSubqueries(logicalOp)
	if ref == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"Cascades planner could not plan query")
	}

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	rules = append(rules, cascades.RewritingRules()...)
	rules = append(rules, cascades.MatchingRules()...)
	planCtx := buildCascadesPlanContext(md)
	planner := cascades.NewPlanner(rules, planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules()).
		WithMaxTasks(100_000) // Java has no low cap; EXISTS + correlated subqueries need full exploration

	bestExpr, _, planErr := planner.Plan(ref)
	if planErr != nil || bestExpr == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"Cascades planner could not plan query")
	}

	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"Cascades planner could not plan query")
	}
	physPlan := ph.GetRecordQueryPlan()
	if physPlan == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"Cascades planner could not plan query")
	}

	// Plan scalar subqueries independently through the Cascades pipeline.
	var scalarSubs []scalarSubqueryBinding
	for _, ssq := range scalarSubqueryPlans {
		subRef := query.TranslateToCascades(ssq.Plan)
		if subRef == nil {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"Cascades planner could not plan scalar subquery")
		}
		subPlanner := cascades.NewPlanner(rules, planCtx).
			WithImplementationRules(cascades.DefaultImplementationRules()).
			WithMaxTasks(100_000) // Java has no low cap; EXISTS + correlated subqueries need full exploration
		subBest, _, subErr := subPlanner.Plan(subRef)
		if subErr != nil || subBest == nil {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"Cascades planner could not plan scalar subquery")
		}
		subPh, ok := subBest.(planExtractor)
		if !ok {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"scalar subquery plan extraction failed")
		}
		subPlan := subPh.GetRecordQueryPlan()
		if subPlan == nil {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"scalar subquery physical plan nil")
		}
		scalarSubs = append(scalarSubs, scalarSubqueryBinding{
			alias: ssq.Alias,
			plan:  subPlan,
		})
	}

	sqlLimit, sqlOffset := extractLimitOffset(logicalOp)

	// Cache the planned result for future queries with the same SQL.
	// Don't cache LIMIT/OFFSET queries — the limit is applied post-execution
	// and not stored in the cached plan.
	if g.cache != nil && sqlLimit < 0 && sqlOffset == 0 {
		g.cache.Put(sqlHash, sql, physPlan, scalarSubs)
	}
	return &cascadesPlan{
		conn:             g.c,
		md:               md,
		physicalPlan:     physPlan,
		explain:          logicalOp.Explain(""),
		scalarSubqueries: scalarSubs,
		sqlLimit:         sqlLimit,
		sqlOffset:        sqlOffset,
	}, nil
}

func extractLimitOffset(op logical.LogicalOperator) (int64, int64) {
	for op != nil {
		if lim, ok := op.(*logical.LogicalLimit); ok {
			return lim.Limit, lim.Offset
		}
		children := op.Children()
		if len(children) == 0 {
			break
		}
		op = children[0]
	}
	return -1, 0
}

func (g *cascadesGenerator) planDML(ctx context.Context, dml antlrgen.IDmlStatementContext) (query.Plan, error) {
	if err := g.c.ensureMetaData(ctx); err != nil {
		return nil, err
	}
	md := g.c.cachedMetaData()
	if md == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "no schema metadata available")
	}

	var logicalOp logical.LogicalOperator
	if del := dml.DeleteStatement(); del != nil {
		logicalOp = buildLogicalPlanForDeleteWithCatalog(del, md)
	} else if upd := dml.UpdateStatement(); upd != nil {
		logicalOp = buildLogicalPlanForUpdateWithCatalog(upd, md)
	} else if ins := dml.InsertStatement(); ins != nil {
		logicalOp = buildLogicalPlanForInsertWithCatalog(ins, md)
	}
	if logicalOp == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "DML logical plan failed")
	}

	if err := resolveQualifiedTableNames(logicalOp, g.c.sess.Schema); err != nil {
		return nil, err
	}

	if fn := query.FindUnsupportedFunction(logicalOp); fn != "" {
		return nil, api.NewError(api.ErrCodeUndefinedFunction,
			"Unsupported operator "+fn)
	}

	ref := query.TranslateToCascades(logicalOp)
	if ref == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "DML Cascades translation failed")
	}

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	rules = append(rules, cascades.RewritingRules()...)
	rules = append(rules, cascades.MatchingRules()...)
	planCtx := buildCascadesPlanContext(md)
	planner := cascades.NewPlanner(rules, planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules()).
		WithMaxTasks(100_000) // Java has no low cap; EXISTS + correlated subqueries need full exploration

	bestExpr, _, planErr := planner.Plan(ref)
	if planErr != nil || bestExpr == nil {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedQuery, "DML Cascades planning failed: %v", planErr)
	}

	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "DML plan extraction failed")
	}
	physPlan := ph.GetRecordQueryPlan()
	if physPlan == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "DML physical plan nil")
	}

	return &cascadesPlan{
		conn:         g.c,
		md:           md,
		physicalPlan: physPlan,
		explain:      logicalOp.Explain(""),
		isUpdate:     true,
	}, nil
}

// scalarSubqueryBinding pairs a correlation alias with a planned
// inner RecordQueryPlan for a scalar subquery. The executor pre-runs
// each and binds the scalar result under the alias before running
// the outer plan.
type scalarSubqueryBinding struct {
	alias values.CorrelationIdentifier
	plan  plans.RecordQueryPlan
}

// cascadesPlan wraps a Cascades-planned SELECT query with a pre-computed
// physical plan. Planning happens at Plan-time; Execute only runs the plan.
type cascadesPlan struct {
	conn             *EmbeddedConnection
	md               *recordlayer.RecordMetaData
	physicalPlan     plans.RecordQueryPlan
	explain          string
	isUpdate         bool
	scalarSubqueries []scalarSubqueryBinding
	sqlLimit         int64 // Go extension: <0 means no limit
	sqlOffset        int64
}

func (p *cascadesPlan) IsUpdate() bool { return p.isUpdate }

func (p *cascadesPlan) Explain() string {
	if p.physicalPlan != nil {
		return p.physicalPlan.Explain()
	}
	return p.explain
}

// txPageTimeLimit is the per-transaction time budget for SQL query
// execution. Set below FDB's 5s hard wall to leave margin for commit
// and cleanup. Matches Java's ExecuteProperties.setTimeLimit pattern.
const txPageTimeLimit = 4 * time.Second

func (p *cascadesPlan) Execute(ctx context.Context) (query.Result, error) {
	c := p.conn
	ss, ssErr := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
	if ssErr != nil {
		return query.Result{}, ssErr
	}

	cols := deriveColumnsFromPlan(p.physicalPlan, p.md)

	// Each fetchPage creates a fresh cursor hierarchy from the plan +
	// continuation. The continuation carries all intermediate state
	// (aggregate accumulators, sort buffers) serialized as protobuf.
	// No cursor persists across transactions — this matches Java's
	// architecture.

	pr := &paginatingRows{
		ctx:              ctx,
		conn:             c,
		ss:               ss,
		plan:             p.physicalPlan,
		md:               p.md,
		scalarSubqueries: p.scalarSubqueries,
		sqlLimit:         p.sqlLimit,
		sqlOffset:        p.sqlOffset,
		cols:             cols,
	}

	// Eagerly fetch the first page so execution errors (type mismatches,
	// plan failures) surface at QueryContext time, not during row iteration.
	if err := pr.fetchPage(); err != nil {
		return query.Result{}, err
	}

	return query.Result{Rows: pr}, nil
}

// paginatingRows implements driver.Rows with cross-transaction pagination.
// Each fetchPage creates a fresh cursor hierarchy from the plan +
// continuation. The continuation carries all intermediate state
// (aggregate accumulators, sort buffers) serialized as protobuf. No
// cursor persists across transactions — this matches Java's architecture.
type paginatingRows struct {
	ctx              context.Context
	conn             *EmbeddedConnection
	ss               subspace.Subspace
	plan             plans.RecordQueryPlan
	md               *recordlayer.RecordMetaData
	scalarSubqueries []scalarSubqueryBinding
	cols             []executor.ColumnDef

	// Go extension: SQL LIMIT/OFFSET applied post-execution.
	sqlLimit  int64 // <0 means no limit
	sqlOffset int64
	emitted   int64
	skipped   int64

	buf          [][]driver.Value
	bufPos       int
	continuation []byte
	exhausted    bool
	closed       bool
	fetchErr     error
}

func (r *paginatingRows) Columns() []string {
	cols := make([]string, len(r.cols))
	for i, c := range r.cols {
		if c.Label != "" {
			cols[i] = c.Label
		} else {
			cols[i] = c.Name
		}
	}
	return cols
}

func (r *paginatingRows) Close() error {
	r.closed = true
	return nil
}

func (r *paginatingRows) ColumnTypeDatabaseTypeName(index int) string {
	if index < 0 || index >= len(r.cols) {
		return ""
	}
	return r.cols[index].TypeName
}

func (r *paginatingRows) ColumnTypeScanType(index int) reflect.Type {
	switch r.ColumnTypeDatabaseTypeName(index) {
	case "BIGINT":
		return reflect.TypeOf((*int64)(nil)).Elem()
	case "INTEGER":
		return reflect.TypeOf((*int32)(nil)).Elem()
	case "DOUBLE":
		return reflect.TypeOf((*float64)(nil)).Elem()
	case "FLOAT":
		return reflect.TypeOf((*float32)(nil)).Elem()
	case "STRING":
		return reflect.TypeOf((*string)(nil)).Elem()
	case "BOOLEAN":
		return reflect.TypeOf((*bool)(nil)).Elem()
	case "BYTES":
		return reflect.TypeOf((*[]byte)(nil)).Elem()
	case "DATE", "TIMESTAMP":
		return reflect.TypeOf((*time.Time)(nil)).Elem()
	default:
		return reflect.TypeOf((*any)(nil)).Elem()
	}
}

func (r *paginatingRows) ColumnTypeNullable(index int) (nullable, ok bool) {
	if index < 0 || index >= len(r.cols) {
		return true, true
	}
	return r.cols[index].Nullable != api.ColumnNoNulls, true
}

func (r *paginatingRows) ColumnTypeLength(index int) (length int64, ok bool) {
	switch r.ColumnTypeDatabaseTypeName(index) {
	case "STRING", "BYTES":
		return math.MaxInt64, true
	case "DATE":
		return 10, true
	case "TIMESTAMP":
		return 19, true
	}
	return 0, false
}

func (r *paginatingRows) ColumnTypePrecisionScale(index int) (precision, scale int64, ok bool) {
	return 0, 0, false
}

func (r *paginatingRows) Next(dest []driver.Value) error {
	if r.closed {
		return io.EOF
	}
	// LIMIT exhausted — stop early.
	if r.sqlLimit >= 0 && r.emitted >= r.sqlLimit {
		return io.EOF
	}

	for {
		row, err := r.nextRow()
		if err != nil {
			return err
		}
		// OFFSET: skip rows.
		if r.skipped < r.sqlOffset {
			r.skipped++
			continue
		}
		copy(dest, row)
		r.emitted++
		return nil
	}
}

func (r *paginatingRows) nextRow() ([]driver.Value, error) {
	if r.closed {
		return nil, io.EOF
	}

	// Serve from buffer if available.
	if r.bufPos < len(r.buf) {
		row := r.buf[r.bufPos]
		r.bufPos++
		return row, nil
	}

	// Buffer exhausted. If source is done, we're done.
	if r.exhausted {
		return nil, io.EOF
	}
	if r.fetchErr != nil {
		return nil, r.fetchErr
	}

	// Fetch pages until we have rows or the source is truly exhausted.
	// Blocking operators (aggregate, sort) may produce 0 result rows per
	// page while accumulating — they only emit after the inner scan is
	// fully drained. Keep fetching until rows appear or exhaustion.
	for {
		if err := r.fetchPage(); err != nil {
			r.fetchErr = err
			return nil, err
		}
		if len(r.buf) > 0 {
			break
		}
		if r.exhausted {
			return nil, io.EOF
		}
	}

	row := r.buf[r.bufPos]
	r.bufPos++
	return row, nil
}

// fetchPage opens a fresh FDB transaction, creates the cursor hierarchy
// (or recreates it from the continuation), drains the cursor until it
// stops, and buffers the results. Everything happens INSIDE DB.Run so
// FDB reads are against a live transaction.
//
// This matches Java's architecture: each transaction creates a fresh
// cursor hierarchy from the plan + continuation. The continuation
// carries ALL intermediate state (aggregate accumulators, sort buffers)
// serialized as protobuf. No cursor persists across transactions.
func (r *paginatingRows) fetchPage() error {
	c := r.conn

	_, txErr := c.sess.DB.Run(r.ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		r.buf = r.buf[:0]
		r.bufPos = 0

		store, storeErr := c.newStoreBuilder().
			SetContext(rctx).
			SetSubspace(r.ss).
			SetMetaDataProvider(c.cachedMetaData()).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		evalCtx := executor.EmptyEvaluationContext()
		if len(r.scalarSubqueries) > 0 {
			scalarResults := make(map[values.CorrelationIdentifier]any, len(r.scalarSubqueries))
			for _, ssq := range r.scalarSubqueries {
				result, ssqErr := executor.EvaluateScalarSubquery(r.ctx, ssq.plan, store, evalCtx)
				if ssqErr != nil {
					return nil, ssqErr
				}
				scalarResults[ssq.alias] = result
			}
			evalCtx = evalCtx.WithScalarSubqueries(scalarResults)
		}

		props := recordlayer.DefaultExecuteProperties().WithTimeLimit(txPageTimeLimit)
		cursor, execErr := executor.ExecutePlan(r.ctx, r.plan, store, evalCtx, r.continuation, props)
		if execErr != nil {
			return nil, translateExecError(execErr)
		}

		rs := executor.NewRecordLayerResultSet(r.ctx, cursor, r.cols)
		defer rs.Close()

		for rs.Next() {
			row := make([]driver.Value, len(r.cols))
			for i := range row {
				v, err := rs.Object(i + 1)
				if err != nil {
					return nil, err
				}
				row[i] = v
			}
			r.buf = append(r.buf, row)
		}
		if err := rs.Err(); err != nil {
			return nil, translateExecError(err)
		}

		cont := rs.GetContinuation()
		if cont == nil || cont.IsEnd() {
			r.exhausted = true
			r.continuation = nil
		} else {
			contBytes, contErr := cont.ToBytes()
			if contErr != nil {
				return nil, contErr
			}
			if contBytes == nil {
				r.exhausted = true
			} else {
				r.continuation = contBytes
			}
		}
		return nil, nil
	})

	if txErr != nil {
		return translateExecError(txErr)
	}
	return nil
}

func translateExecError(err error) error {
	if err == nil {
		return nil
	}
	var typeMismatch *predicates.TypeMismatchError
	if errors.As(err, &typeMismatch) {
		return api.NewError(api.ErrCodeDatatypeMismatch,
			"The operands of a comparison operator are not compatible.")
	}
	var depthExceeded *executor.RecursiveCTEDepthExceededError
	if errors.As(err, &depthExceeded) {
		return api.NewError(api.ErrCodeExecutionLimitReached, depthExceeded.Error())
	}
	var aggTypeMismatch *executor.AggregateTypeMismatchError
	if errors.As(err, &aggTypeMismatch) {
		return api.NewError(api.ErrCodeUnsupportedOperation, aggTypeMismatch.Error())
	}
	var rangeOverflow *executor.NumericRangeOverflowError
	if errors.As(err, &rangeOverflow) {
		return api.NewError(api.ErrCodeNumericValueOutOfRange, rangeOverflow.Error())
	}
	var sumOverflow *executor.SumOverflowError
	if errors.As(err, &sumOverflow) {
		return api.NewError(api.ErrCodeNumericValueOutOfRange, sumOverflow.Error())
	}
	var divZero *values.ArithmeticDivisionByZeroError
	if errors.As(err, &divZero) {
		return api.NewError(api.ErrCodeDivisionByZero, "/ by zero")
	}
	var overflow *values.ArithmeticOverflowError
	if errors.As(err, &overflow) {
		return api.NewError(api.ErrCodeNumericValueOutOfRange, "integer overflow")
	}
	var scalarMismatch *values.ScalarTypeMismatchError
	if errors.As(err, &scalarMismatch) {
		return api.NewError(api.ErrCodeCannotConvertType, scalarMismatch.Error())
	}
	var castErr *values.InvalidCastError
	if errors.As(err, &castErr) {
		return api.NewError(api.ErrCodeInvalidCast, castErr.Error())
	}
	return err
}

func buildCascadesPlanContext(md *recordlayer.RecordMetaData) cascades.PlanContext {
	if md == nil {
		return cascades.EmptyPlanContext()
	}
	return &metadataPlanContext{md: md}
}

type metadataPlanContext struct {
	md *recordlayer.RecordMetaData
}

func (c *metadataPlanContext) GetPlannerConfiguration() cascades.PlannerConfiguration {
	return cascades.DefaultPlannerConfiguration()
}

func (c *metadataPlanContext) GetMatchCandidates() []cascades.MatchCandidate {
	if c.md == nil {
		return nil
	}

	var candidates []cascades.MatchCandidate

	// Register PrimaryScanMatchCandidates for each record type's PK.
	// Mirrors Java's RecordStoreScope which creates a PrimaryScanMatchCandidate
	// from the common primary key.
	allTypes := c.md.RecordTypes()
	allTypeNames := make([]string, 0, len(allTypes))
	for name := range allTypes {
		allTypeNames = append(allTypeNames, name)
	}
	for _, rt := range allTypes {
		if rt.PrimaryKey == nil {
			continue
		}
		pkCols := rt.PrimaryKey.FieldNames()
		if len(pkCols) == 0 {
			continue
		}
		upperPK := make([]string, len(pkCols))
		aliases := make([]values.CorrelationIdentifier, len(pkCols))
		for i, col := range pkCols {
			upperPK[i] = strings.ToUpper(col)
			aliases[i] = values.UniqueCorrelationIdentifier()
		}
		candidates = append(candidates, cascades.NewPrimaryScanMatchCandidate(
			nil,
			aliases,
			allTypeNames,
			[]string{rt.Name},
			upperPK,
			values.UnknownType,
		))
	}

	// Register secondary index candidates.
	allIndexes := c.md.GetAllIndexes()
	defs := make([]cascades.IndexDef, 0, len(allIndexes))
	for _, idx := range allIndexes {
		if idx.RootExpression == nil {
			continue
		}
		defs = append(defs, &metadataIndexDef{idx: idx, md: c.md})
	}
	if len(defs) > 0 {
		ctx := cascades.NewPlanContextFromIndexDefs(defs)
		candidates = append(candidates, ctx.GetMatchCandidates()...)
	}

	return candidates
}

type metadataIndexDef struct {
	idx *recordlayer.Index
	md  *recordlayer.RecordMetaData
}

func (d *metadataIndexDef) IndexName() string          { return d.idx.Name }
func (d *metadataIndexDef) IndexColumnNames() []string { return d.idx.RootExpression.FieldNames() }
func (d *metadataIndexDef) IndexIsUnique() bool        { return d.idx.IsUnique() }

func (d *metadataIndexDef) IndexRecordTypes() []string {
	rts := d.md.RecordTypesForIndex(d.idx)
	names := make([]string, len(rts))
	for i, rt := range rts {
		names[i] = rt.Name
	}
	return names
}

func (c *metadataPlanContext) GetPrimaryKeyColumns(recordType string) []string {
	if c.md == nil {
		return nil
	}
	rt := c.md.GetRecordType(recordType)
	if rt == nil || rt.PrimaryKey == nil {
		return nil
	}
	return rt.PrimaryKey.FieldNames()
}

func deriveColumnsFromPlan(plan plans.RecordQueryPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	if md == nil {
		return nil
	}
	if proj, ok := plan.(*plans.RecordQueryProjectionPlan); ok {
		return deriveColumnsFromProjection(proj, md)
	}
	if agg, ok := plan.(*plans.RecordQueryStreamingAggregationPlan); ok {
		return deriveColumnsFromAggregation(agg, md)
	}
	if nlj, ok := plan.(*plans.RecordQueryNestedLoopJoinPlan); ok {
		return deriveColumnsFromJoin(nlj, md)
	}
	if fm, ok := plan.(*plans.RecordQueryFlatMapPlan); ok {
		return deriveColumnsFromFlatMap(fm, md)
	}
	if u := findUnionPlan(plan); u != nil {
		return deriveColumnsFromPlan(u[0], md)
	}
	if ip, ok := plan.(innerPlan); ok {
		return deriveColumnsFromPlan(ip.GetInner(), md)
	}
	// Leaf plan: either a primary-key scan or an index scan. Both
	// carry GetRecordTypes(); the index scan's executor fetches the
	// full record via indexFetchCursor, so all columns are available.
	var recordTypes []string
	if scan := findScanPlan(plan); scan != nil {
		recordTypes = scan.GetRecordTypes()
	} else if idxPlan := findIndexPlan(plan); idxPlan != nil {
		recordTypes = idxPlan.GetRecordTypes()
	}
	if len(recordTypes) == 0 {
		return nil
	}
	rt := md.GetRecordType(recordTypes[0])
	if rt == nil || rt.Descriptor == nil {
		return nil
	}
	fields := rt.Descriptor.Fields()
	cols := make([]executor.ColumnDef, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		nullable := api.ColumnNullable
		if fd.Cardinality() == protoreflect.Required {
			nullable = api.ColumnNoNulls
		}
		cols[i] = executor.ColumnDef{
			Name:     strings.ToUpper(string(fd.Name())),
			TypeName: protoKindToTypeName(fd.Kind()),
			Nullable: nullable,
		}
	}
	return cols
}

type innerPlan interface {
	GetInner() plans.RecordQueryPlan
}

func findScanPlan(p plans.RecordQueryPlan) *plans.RecordQueryScanPlan {
	for {
		if s, ok := p.(*plans.RecordQueryScanPlan); ok {
			return s
		}
		if ip, ok := p.(innerPlan); ok {
			p = ip.GetInner()
		} else {
			return nil
		}
	}
}

// findIndexPlan walks through innerPlan wrappers (filters, type
// filters, etc.) to find a leaf RecordQueryIndexPlan.
func findIndexPlan(p plans.RecordQueryPlan) *plans.RecordQueryIndexPlan {
	for {
		if idx, ok := p.(*plans.RecordQueryIndexPlan); ok {
			return idx
		}
		if ip, ok := p.(innerPlan); ok {
			p = ip.GetInner()
		} else {
			return nil
		}
	}
}

// findLeafDescriptor locates the protoreflect.MessageDescriptor for
// the record type at the leaf of the plan tree. Works for both
// primary-key scans (RecordQueryScanPlan) and secondary-index scans
// (RecordQueryIndexPlan).
func findLeafDescriptor(p plans.RecordQueryPlan, md *recordlayer.RecordMetaData) protoreflect.MessageDescriptor {
	var recordTypes []string
	if scan := findScanPlan(p); scan != nil {
		recordTypes = scan.GetRecordTypes()
	} else if idx := findIndexPlan(p); idx != nil {
		recordTypes = idx.GetRecordTypes()
	}
	if len(recordTypes) == 0 {
		return nil
	}
	rt := md.GetRecordType(recordTypes[0])
	if rt == nil {
		return nil
	}
	return rt.Descriptor
}

func deriveColumnsFromProjection(proj *plans.RecordQueryProjectionPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	desc := findLeafDescriptor(proj.GetInner(), md)
	aliases := proj.GetAliases()
	cols := make([]executor.ColumnDef, len(proj.GetProjections()))
	for i, v := range proj.GetProjections() {
		var name string
		if fv, ok := v.(*values.FieldValue); ok {
			if fv.Child != nil {
				name = values.ExplainValue(v)
			} else {
				name = fv.Field
			}
		} else {
			name = values.ExplainValue(v)
		}
		var label string
		if i < len(aliases) && aliases[i] != "" {
			label = strings.ToUpper(aliases[i])
		} else if _, isField := v.(*values.FieldValue); !isField {
			label = fmt.Sprintf("_%d", i)
		}
		typeName := valueTypeName(v, desc)
		if typeName == "" && desc != nil {
			typeName = protoFieldTypeName(desc, name)
		}
		if typeName == "" {
			typeName = "UNKNOWN"
		}
		// Use the alias as the datum lookup key (Name) when available.
		// executeProjection stores values under both the original name
		// and the alias, so the alias is a valid lookup key and gives
		// CTE consumers the column name they reference.
		colName := strings.ToUpper(name)
		if label != "" {
			colName = label
		}
		nullable := api.ColumnNullable
		if desc != nil {
			if fd := desc.Fields().ByName(protoreflect.Name(name)); fd != nil && fd.Cardinality() == protoreflect.Required {
				nullable = api.ColumnNoNulls
			}
		}
		cols[i] = executor.ColumnDef{
			Name:     colName,
			Label:    label,
			TypeName: typeName,
			Nullable: nullable,
		}
	}
	return cols
}

func deriveColumnsFromAggregation(agg *plans.RecordQueryStreamingAggregationPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	desc := findLeafDescriptor(agg.GetInner(), md)
	return buildAggColumns(agg.GetGroupingKeys(), agg.GetAggregates(), desc)
}

type multiInnerPlan interface {
	GetInners() []plans.RecordQueryPlan
}

func findUnionPlan(p plans.RecordQueryPlan) []plans.RecordQueryPlan {
	for {
		if mi, ok := p.(multiInnerPlan); ok {
			inners := mi.GetInners()
			if len(inners) > 0 {
				return inners
			}
			return nil
		}
		if ip, ok := p.(innerPlan); ok {
			p = ip.GetInner()
		} else {
			return nil
		}
	}
}

func deriveColumnsFromJoin(nlj *plans.RecordQueryNestedLoopJoinPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	outerCols := deriveColumnsFromPlan(nlj.GetOuter(), md)
	if nlj.GetJoinType() == plans.JoinExists || nlj.GetJoinType() == plans.JoinNotExists {
		return outerCols
	}
	innerCols := deriveColumnsFromPlan(nlj.GetInner(), md)
	if outerCols == nil && innerCols == nil {
		return nil
	}
	// When outer and inner share column names (e.g. SELECT * FROM a, b
	// where both have "id"), the merged row map's bare keys hold the
	// inner side's values (last-write-wins in mergeRows). To read the
	// correct values for each side, qualify bare column names with the
	// join alias so the ResultSet reads "A.ID" / "B.ID" from the map
	// instead of both hitting the bare "ID" key. Already-qualified
	// columns (from a nested NLJ) are left as-is to avoid double-
	// qualification like "B.A.ID".
	outerAlias := strings.ToUpper(nlj.GetOuterAlias())
	innerAlias := strings.ToUpper(nlj.GetInnerAlias())

	// Determine SQL-level first/second based on whether the physical
	// join direction was swapped by the ChildrenAsSet optimization.
	// When reversed, inner columns come first in SQL order.
	firstCols, secondCols := outerCols, innerCols
	firstAlias, secondAlias := outerAlias, innerAlias
	if nlj.IsSQLColumnOrderReversed() {
		firstCols, secondCols = innerCols, outerCols
		firstAlias, secondAlias = innerAlias, outerAlias
	}

	cols := make([]executor.ColumnDef, 0, len(firstCols)+len(secondCols))
	for _, c := range firstCols {
		qual := c
		if firstAlias != "" && !parseColRef(c.Name).isQualified() {
			qual.Name = firstAlias + "." + strings.ToUpper(c.Name)
		}
		cols = append(cols, qual)
	}
	for _, c := range secondCols {
		qual := c
		if secondAlias != "" && !parseColRef(c.Name).isQualified() {
			qual.Name = secondAlias + "." + strings.ToUpper(c.Name)
		}
		cols = append(cols, qual)
	}
	return cols
}

func deriveColumnsFromFlatMap(fm *plans.RecordQueryFlatMapPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	outerCols := deriveColumnsFromPlan(fm.GetOuter(), md)
	innerCols := deriveColumnsFromPlan(fm.GetInner(), md)
	if outerCols == nil && innerCols == nil {
		return nil
	}

	outerAlias := strings.ToUpper(fm.GetOuterAlias().Name())
	innerAlias := strings.ToUpper(fm.GetInnerAlias().Name())

	cols := make([]executor.ColumnDef, 0, len(outerCols)+len(innerCols))
	for _, c := range outerCols {
		qual := c
		if outerAlias != "" && !parseColRef(c.Name).isQualified() {
			qual.Name = outerAlias + "." + strings.ToUpper(c.Name)
		}
		cols = append(cols, qual)
	}
	for _, c := range innerCols {
		qual := c
		if innerAlias != "" && !parseColRef(c.Name).isQualified() {
			qual.Name = innerAlias + "." + strings.ToUpper(c.Name)
		}
		cols = append(cols, qual)
	}
	return cols
}

func buildAggColumns(
	groupKeys []values.Value,
	aggregates []expressions.AggregateSpec,
	desc protoreflect.MessageDescriptor,
) []executor.ColumnDef {
	cols := make([]executor.ColumnDef, 0, len(groupKeys)+len(aggregates))
	for _, k := range groupKeys {
		name := values.ExplainValue(k)
		typeName := "UNKNOWN"
		if desc != nil {
			typeName = protoFieldTypeName(desc, name)
		}
		nullable := api.ColumnNullable
		if desc != nil {
			if fd := desc.Fields().ByName(protoreflect.Name(name)); fd != nil && fd.Cardinality() == protoreflect.Required {
				nullable = api.ColumnNoNulls
			}
		}
		cols = append(cols, executor.ColumnDef{
			Name:     strings.ToUpper(name),
			TypeName: typeName,
			Nullable: nullable,
		})
	}
	for _, a := range aggregates {
		name := aggregateSpecName(a)
		typeName := aggregateResultType(a, desc)
		cols = append(cols, executor.ColumnDef{
			Name:     strings.ToUpper(name),
			TypeName: typeName,
			Nullable: api.ColumnNullable,
		})
	}
	return cols
}

func aggregateSpecName(a expressions.AggregateSpec) string {
	operand := aggOperandName(a)
	switch a.Function {
	case expressions.AggCount:
		return "COUNT(" + operand + ")"
	case expressions.AggSum:
		return "SUM(" + operand + ")"
	case expressions.AggAvg:
		return "AVG(" + operand + ")"
	case expressions.AggMin:
		return "MIN(" + operand + ")"
	case expressions.AggMax:
		return "MAX(" + operand + ")"
	default:
		return "AGG(" + operand + ")"
	}
}

func aggOperandName(a expressions.AggregateSpec) string {
	if cv, ok := a.Operand.(*values.ConstantValue); ok && cv.Value == nil {
		return "*"
	}
	return values.ExplainValue(a.Operand)
}

func aggregateResultType(a expressions.AggregateSpec, desc protoreflect.MessageDescriptor) string {
	switch a.Function {
	case expressions.AggCount:
		return "BIGINT"
	case expressions.AggAvg:
		return "DOUBLE"
	case expressions.AggSum, expressions.AggMin, expressions.AggMax:
		if desc != nil {
			operandName := values.ExplainValue(a.Operand)
			if t := protoFieldTypeName(desc, operandName); t != "UNKNOWN" {
				return t
			}
		}
		return "BIGINT"
	default:
		return "UNKNOWN"
	}
}

// valueTypeName resolves the SQL type name for a Value. For
// AggregateValue nodes, it inspects the typed Op field instead of
// string-parsing the ExplainValue output. For plain field references,
// it falls through and returns "".
func valueTypeName(v values.Value, desc protoreflect.MessageDescriptor) string {
	if av, ok := v.(*values.AggregateValue); ok {
		switch av.Op {
		case values.AggCount, values.AggCountStar:
			return "BIGINT"
		case values.AggAvg:
			return "DOUBLE"
		case values.AggSum, values.AggMin, values.AggMax:
			if av.Operand != nil && desc != nil {
				operandName := values.ExplainValue(av.Operand)
				if t := protoFieldTypeName(desc, operandName); t != "UNKNOWN" {
					return t
				}
			}
			return "BIGINT"
		default:
			return "UNKNOWN"
		}
	}
	if t := v.Type(); t != nil {
		switch t.Code() {
		case values.TypeCodeInt:
			return "INTEGER"
		case values.TypeCodeLong:
			return "BIGINT"
		case values.TypeCodeFloat:
			return "FLOAT"
		case values.TypeCodeDouble:
			return "DOUBLE"
		case values.TypeCodeString:
			return "STRING"
		case values.TypeCodeBoolean:
			return "BOOLEAN"
		case values.TypeCodeDate:
			return "DATE"
		case values.TypeCodeTimestamp:
			return "TIMESTAMP"
		}
	}
	return ""
}

func protoFieldTypeName(desc protoreflect.MessageDescriptor, name string) string {
	fields := desc.Fields()
	fd := fields.ByName(protoreflect.Name(name))
	if fd != nil {
		return protoKindToTypeName(fd.Kind())
	}
	return "UNKNOWN"
}

func protoKindToTypeName(k protoreflect.Kind) string {
	switch k {
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
	default:
		return "UNKNOWN"
	}
}

// cascadesRows wraps a RecordLayerResultSet as driver.Rows.
type cascadesRows struct {
	rs *executor.RecordLayerResultSet
}

func newCascadesRows(rs *executor.RecordLayerResultSet) *cascadesRows {
	return &cascadesRows{rs: rs}
}

func (r *cascadesRows) Columns() []string {
	md := r.rs.MetaData()
	cols := make([]string, md.ColumnCount())
	for i := range cols {
		cols[i], _ = md.ColumnLabel(i + 1)
	}
	return cols
}

func (r *cascadesRows) Close() error {
	return r.rs.Close()
}

func (r *cascadesRows) ColumnTypeDatabaseTypeName(index int) string {
	md := r.rs.MetaData()
	name, err := md.ColumnTypeName(index + 1)
	if err != nil {
		return ""
	}
	return name
}

func (r *cascadesRows) ColumnTypeScanType(index int) reflect.Type {
	typeName := r.ColumnTypeDatabaseTypeName(index)
	switch typeName {
	case "BIGINT":
		return reflect.TypeOf((*int64)(nil)).Elem()
	case "INTEGER":
		return reflect.TypeOf((*int32)(nil)).Elem()
	case "DOUBLE":
		return reflect.TypeOf((*float64)(nil)).Elem()
	case "FLOAT":
		return reflect.TypeOf((*float32)(nil)).Elem()
	case "STRING":
		return reflect.TypeOf((*string)(nil)).Elem()
	case "BOOLEAN":
		return reflect.TypeOf((*bool)(nil)).Elem()
	case "BYTES":
		return reflect.TypeOf((*[]byte)(nil)).Elem()
	case "DATE", "TIMESTAMP":
		return reflect.TypeOf((*time.Time)(nil)).Elem()
	default:
		return reflect.TypeOf((*any)(nil)).Elem()
	}
}

func (r *cascadesRows) ColumnTypeNullable(index int) (nullable, ok bool) {
	md := r.rs.MetaData()
	n, err := md.ColumnNullable(index + 1)
	if err != nil {
		return true, true
	}
	return n != api.ColumnNoNulls, true
}

func (r *cascadesRows) ColumnTypeLength(index int) (length int64, ok bool) {
	typeName := r.ColumnTypeDatabaseTypeName(index)
	switch typeName {
	case "STRING", "BYTES":
		return math.MaxInt64, true
	case "DATE":
		return 10, true // "2006-01-02"
	case "TIMESTAMP":
		return 19, true // "2006-01-02 15:04:05"
	}
	return 0, false
}

func (r *cascadesRows) ColumnTypePrecisionScale(index int) (precision, scale int64, ok bool) {
	return 0, 0, false
}

func (r *cascadesRows) Next(dest []driver.Value) error {
	if !r.rs.Next() {
		if err := r.rs.Err(); err != nil {
			return translateExecError(err)
		}
		return io.EOF
	}
	for i := range dest {
		v, err := r.rs.Object(i + 1)
		if err != nil {
			return err
		}
		dest[i] = v
	}
	return nil
}

func findDistinctAggregate(op logical.LogicalOperator) string {
	if op == nil {
		return ""
	}
	if agg, ok := op.(*logical.LogicalAggregate); ok && agg.HasDistinctAggregate {
		return "DISTINCT aggregates are not supported"
	}
	for _, ch := range op.Children() {
		if msg := findDistinctAggregate(ch); msg != "" {
			return msg
		}
	}
	return ""
}

// resolveQualifiedTableNames walks the logical plan tree and resolves
// schema-qualified table names (schema.table → table) in LogicalScan
// nodes. Mirrors Java's SemanticAnalyzer.tableExists qualifier validation.
func resolveQualifiedTableNames(op logical.LogicalOperator, schemaName string) error {
	if op == nil {
		return nil
	}
	if scan, ok := op.(*logical.LogicalScan); ok {
		resolved, err := functions.ResolveQualifiedTableName(scan.Table, schemaName)
		if err != nil {
			return err
		}
		scan.Table = resolved
	}
	if ins, ok := op.(*logical.LogicalInsert); ok {
		resolved, err := functions.ResolveQualifiedTableName(ins.Table, schemaName)
		if err != nil {
			return err
		}
		ins.Table = resolved
	}
	if del, ok := op.(*logical.LogicalDelete); ok {
		resolved, err := functions.ResolveQualifiedTableName(del.Target, schemaName)
		if err != nil {
			return err
		}
		del.Target = resolved
	}
	if upd, ok := op.(*logical.LogicalUpdate); ok {
		resolved, err := functions.ResolveQualifiedTableName(upd.Target, schemaName)
		if err != nil {
			return err
		}
		upd.Target = resolved
	}
	for _, ch := range op.Children() {
		if err := resolveQualifiedTableNames(ch, schemaName); err != nil {
			return err
		}
	}
	return nil
}

func validateTablesAndColumns(op logical.LogicalOperator, md *recordlayer.RecordMetaData) error {
	cteNames := collectCTENames(op)
	return validateTablesAndColumnsInner(op, md, cteNames)
}

func validateTablesAndColumnsInner(op logical.LogicalOperator, md *recordlayer.RecordMetaData, cteNames map[string]bool) error {
	if op == nil {
		return nil
	}
	if scan, ok := op.(*logical.LogicalScan); ok {
		if !cteNames[strings.ToUpper(scan.Table)] {
			rt := md.GetRecordType(scan.Table)
			if rt == nil {
				return api.NewErrorf(api.ErrCodeUndefinedTable, "table %q does not exist", scan.Table)
			}
		}
	}
	if proj, ok := op.(*logical.LogicalProject); ok && !hasJoin(op) && !hasAggregate(op) {
		scan := findLogicalScan(op)
		if scan != nil && !cteNames[strings.ToUpper(scan.Table)] {
			rt := md.GetRecordType(scan.Table)
			if rt != nil && rt.Descriptor != nil {
				for i, col := range proj.Projections {
					if i < len(proj.IsComputed) && proj.IsComputed[i] {
						continue
					}
					if i < len(proj.ProjectedValues) && proj.ProjectedValues[i] != nil {
						continue
					}
					upper := strings.ToUpper(col)
					ref := parseColRef(upper)
					if ref.isQualified() {
						qual := ref.table
						scanName := strings.ToUpper(scan.Table)
						if scan.Alias != "" {
							scanName = strings.ToUpper(scan.Alias)
						}
						if qual != scanName {
							return api.NewErrorf(api.ErrCodeUndefinedColumn,
								"column reference with qualifier %q cannot be resolved", qual)
						}
						upper = ref.bare()
					}
					if rt.Descriptor.Fields().ByName(protoreflect.Name(upper)) == nil {
						return api.NewErrorf(api.ErrCodeUndefinedColumn, "column %q does not exist", col)
					}
				}
			}
		}
	}
	for _, child := range op.Children() {
		if err := validateTablesAndColumnsInner(child, md, cteNames); err != nil {
			return err
		}
	}
	return nil
}

func collectCTENames(op logical.LogicalOperator) map[string]bool {
	names := make(map[string]bool)
	collectCTENamesInner(op, names)
	return names
}

func collectCTENamesInner(op logical.LogicalOperator, names map[string]bool) {
	if op == nil {
		return
	}
	if cte, ok := op.(*logical.LogicalCTE); ok {
		names[strings.ToUpper(cte.Name)] = true
	}
	for _, ch := range op.Children() {
		collectCTENamesInner(ch, names)
	}
}

func hasAggregate(op logical.LogicalOperator) bool {
	if op == nil {
		return false
	}
	if _, ok := op.(*logical.LogicalAggregate); ok {
		return true
	}
	for _, ch := range op.Children() {
		if hasAggregate(ch) {
			return true
		}
	}
	return false
}

func hasJoin(op logical.LogicalOperator) bool {
	if op == nil {
		return false
	}
	if _, ok := op.(*logical.LogicalJoin); ok {
		return true
	}
	for _, ch := range op.Children() {
		if hasJoin(ch) {
			return true
		}
	}
	return false
}

func findLogicalScan(op logical.LogicalOperator) *logical.LogicalScan {
	if op == nil {
		return nil
	}
	if s, ok := op.(*logical.LogicalScan); ok {
		return s
	}
	for _, ch := range op.Children() {
		if s := findLogicalScan(ch); s != nil {
			return s
		}
	}
	return nil
}

// referencesInformationSchema walks the ANTLR parse tree and returns
// true if any table name references the INFORMATION_SCHEMA. Walks
// typed FullId → Uid nodes — no GetText on the table name.
func referencesInformationSchema(ctx antlr.Tree) bool {
	if ctx == nil {
		return false
	}
	if atom, ok := ctx.(*antlrgen.AtomTableItemContext); ok {
		if tn := atom.TableName(); tn != nil {
			if fid := tn.FullId(); fid != nil {
				for _, uid := range fid.AllUid() {
					if strings.EqualFold(functions.StripIdentifierQuotes(uid.GetText()), "INFORMATION_SCHEMA") {
						return true
					}
				}
			}
		}
	}
	for i := 0; i < ctx.GetChildCount(); i++ {
		if referencesInformationSchema(ctx.GetChild(i)) {
			return true
		}
	}
	return false
}

// findUnsupportedFunctionInParseTree walks an ANTLR expression tree
// and returns the name of the first scalar function call that isn't
// in the Cascades-safe set. Uses typed parse tree nodes — no text
// matching.
func findUnsupportedFunctionInParseTree(ctx antlr.Tree) string {
	if ctx == nil {
		return ""
	}
	switch n := ctx.(type) {
	case *antlrgen.FunctionCallExpressionAtomContext:
		if fc := n.FunctionCall(); fc != nil {
			if name := extractFunctionNameFromCall(fc); name != "" {
				if !isAllowedFunction(name) {
					return name
				}
			}
		}
	case *antlrgen.BitExpressionAtomContext:
		if bo := n.BitOperator(); bo != nil {
			boc, _ := bo.(*antlrgen.BitOperatorContext)
			if boc != nil && len(boc.AllLESS_SYMBOL()) >= 2 {
				return "<<"
			}
			if boc != nil && len(boc.AllGREATER_SYMBOL()) >= 2 {
				return ">>"
			}
		}
	}
	for i := 0; i < ctx.GetChildCount(); i++ {
		if fn := findUnsupportedFunctionInParseTree(ctx.GetChild(i)); fn != "" {
			return fn
		}
	}
	return ""
}

func extractFunctionNameFromCall(fc antlrgen.IFunctionCallContext) string {
	switch f := fc.(type) {
	case *antlrgen.ScalarFunctionCallContext:
		if f.ScalarFunctionName() != nil {
			return strings.ToUpper(f.ScalarFunctionName().GetText())
		}
	case *antlrgen.UserDefinedScalarFunctionCallContext:
		if f.UserDefinedScalarFunctionName() != nil {
			return strings.ToUpper(f.UserDefinedScalarFunctionName().GetText())
		}
	case *antlrgen.NonAggregateFunctionCallContext:
		if wf := f.NonAggregateWindowedFunction(); wf != nil {
			if wfc, ok := wf.(*antlrgen.NonAggregateWindowedFunctionContext); ok {
				switch {
				case wfc.ROW_NUMBER() != nil:
					return "ROW_NUMBER"
				case wfc.RANK() != nil:
					return "RANK"
				case wfc.DENSE_RANK() != nil:
					return "DENSE_RANK"
				case wfc.PERCENT_RANK() != nil:
					return "PERCENT_RANK"
				default:
					return "WINDOW_FUNCTION"
				}
			}
		}
	case *antlrgen.SpecificFunctionCallContext:
		if f.SpecificFunction() != nil {
			switch sf := f.SpecificFunction().(type) {
			case *antlrgen.SimpleFunctionCallContext:
				if sf.CURRENT_DATE() != nil {
					return "CURRENT_DATE"
				}
				if sf.CURRENT_TIME() != nil {
					return "CURRENT_TIME"
				}
				if sf.CURRENT_TIMESTAMP() != nil {
					return "CURRENT_TIMESTAMP"
				}
				if sf.LOCALTIME() != nil {
					return "LOCALTIME"
				}
				if sf.CURRENT_USER() != nil {
					return "CURRENT_USER"
				}
			}
		}
	}
	return ""
}

func isAllowedFunction(name string) bool {
	switch name {
	case "COUNT", "SUM", "MIN", "MAX", "AVG",
		"CASE", "CAST", "IF",
		"CURRENT_DATE", "CURRENT_TIME", "CURRENT_TIMESTAMP", "LOCALTIME",
		"CURRENT_USER":
		return true
	}
	return values.IsCascadesSafeScalarFunction(name)
}

// findUnsupportedFunctionInSelectQuery walks the ANTLR expression
// contexts in a selectQuery's projections and returns the first
// unsupported function name, or "".
func findUnsupportedFunctionInSelectQuery(sq *selectQuery) string {
	if sq == nil {
		return ""
	}
	for _, expr := range sq.projExprs {
		if fn := findUnsupportedFunctionInParseTree(expr); fn != "" {
			return fn
		}
	}
	return ""
}

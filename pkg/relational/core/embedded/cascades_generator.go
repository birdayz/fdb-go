package embedded

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"

	"github.com/antlr4-go/antlr/v4"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/executor"
	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
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
}

func newCascadesGenerator(c *EmbeddedConnection) *cascadesGenerator {
	return &cascadesGenerator{
		c:     c,
		naive: &naiveGenerator{c: c},
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

	logicalOp, buildErr := buildLogicalPlanForQueryWithCatalog(q, md)
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

	if err := validateTablesAndColumns(logicalOp, md); err != nil {
		return nil, err
	}

	if msg := findDistinctAggregate(logicalOp); msg != "" {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, msg)
	}

	ref := query.TranslateToCascades(logicalOp)
	if ref == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"Cascades planner could not plan query")
	}

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	planCtx := buildCascadesPlanContext(md)
	planner := cascades.NewPlanner(rules, planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules()).
		WithMaxTasks(10_000)

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

	return &cascadesPlan{
		conn:         g.c,
		md:           md,
		physicalPlan: physPlan,
		explain:      logicalOp.Explain(""),
	}, nil
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

	if fn := query.FindUnsupportedFunction(logicalOp); fn != "" {
		return nil, api.NewError(api.ErrCodeUndefinedFunction,
			"Unsupported operator "+fn)
	}

	ref := query.TranslateToCascades(logicalOp)
	if ref == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "DML Cascades translation failed")
	}

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	planCtx := buildCascadesPlanContext(md)
	planner := cascades.NewPlanner(rules, planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules()).
		WithMaxTasks(10_000)

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

// cascadesPlan wraps a Cascades-planned SELECT query with a pre-computed
// physical plan. Planning happens at Plan-time; Execute only runs the plan.
type cascadesPlan struct {
	conn         *EmbeddedConnection
	md           *recordlayer.RecordMetaData
	physicalPlan plans.RecordQueryPlan
	explain      string
	isUpdate     bool
}

func (p *cascadesPlan) IsUpdate() bool { return p.isUpdate }

func (p *cascadesPlan) Explain() string {
	if p.physicalPlan != nil {
		return p.physicalPlan.Explain()
	}
	return p.explain
}

func (p *cascadesPlan) Execute(ctx context.Context) (query.Result, error) {
	c := p.conn
	ss, ssErr := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
	if ssErr != nil {
		return query.Result{}, ssErr
	}

	var rows driver.Rows
	_, txErr := c.sess.DB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		store, storeErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(c.cachedMetaData()).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		evalCtx := executor.EmptyEvaluationContext()
		cursor, execErr := executor.ExecutePlan(ctx, p.physicalPlan, store, evalCtx, nil,
			recordlayer.DefaultExecuteProperties())
		if execErr != nil {
			var typeMismatch *predicates.TypeMismatchError
			if errors.As(execErr, &typeMismatch) {
				return nil, api.NewError(api.ErrCodeCannotConvertType, typeMismatch.Error())
			}
			return nil, execErr
		}

		cols := deriveColumnsFromPlan(p.physicalPlan, p.md)
		rs := executor.NewRecordLayerResultSet(ctx, cursor, cols)
		rows = newCascadesRows(rs)
		return nil, nil
	})
	if txErr != nil {
		if strings.Contains(txErr.Error(), "cannot aggregate non-numeric") {
			return query.Result{}, api.NewError(api.ErrCodeInvalidParameter, txErr.Error())
		}
		return query.Result{}, txErr
	}

	return query.Result{Rows: rows}, nil
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
	allIndexes := c.md.GetAllIndexes()
	if len(allIndexes) == 0 {
		return nil
	}
	defs := make([]cascades.IndexDef, 0, len(allIndexes))
	for _, idx := range allIndexes {
		if idx.RootExpression == nil {
			continue
		}
		defs = append(defs, &metadataIndexDef{idx: idx, md: c.md})
	}
	if len(defs) == 0 {
		return nil
	}
	ctx := cascades.NewPlanContextFromIndexDefs(defs)
	return ctx.GetMatchCandidates()
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
	if agg, ok := plan.(*plans.RecordQueryHashAggregationPlan); ok {
		return deriveColumnsFromHashAggregation(agg, md)
	}
	if nlj, ok := plan.(*plans.RecordQueryNestedLoopJoinPlan); ok {
		return deriveColumnsFromJoin(nlj, md)
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
		cols[i] = executor.ColumnDef{
			Name:     strings.ToUpper(string(fd.Name())),
			TypeName: protoKindToTypeName(fd.Kind()),
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
			name = fv.Field
		} else {
			name = values.ExplainValue(v)
		}
		var label string
		if i < len(aliases) && aliases[i] != "" {
			label = strings.ToUpper(aliases[i])
		}
		typeName := aggregateTypeName(name, desc)
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
		cols[i] = executor.ColumnDef{
			Name:     colName,
			Label:    label,
			TypeName: typeName,
		}
	}
	return cols
}

func deriveColumnsFromAggregation(agg *plans.RecordQueryStreamingAggregationPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	desc := findLeafDescriptor(agg.GetInner(), md)
	return buildAggColumns(agg.GetGroupingKeys(), agg.GetAggregates(), desc)
}

func deriveColumnsFromHashAggregation(agg *plans.RecordQueryHashAggregationPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
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
	innerCols := deriveColumnsFromPlan(nlj.GetInner(), md)
	if outerCols == nil && innerCols == nil {
		return nil
	}
	cols := make([]executor.ColumnDef, 0, len(outerCols)+len(innerCols))
	cols = append(cols, outerCols...)
	cols = append(cols, innerCols...)
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
		cols = append(cols, executor.ColumnDef{
			Name:     strings.ToUpper(name),
			TypeName: typeName,
		})
	}
	for _, a := range aggregates {
		name := aggregateSpecName(a)
		typeName := aggregateResultType(a, desc)
		cols = append(cols, executor.ColumnDef{
			Name:     strings.ToUpper(name),
			TypeName: typeName,
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

func aggregateTypeName(name string, desc protoreflect.MessageDescriptor) string {
	upper := strings.ToUpper(strings.TrimSpace(name))
	if strings.HasPrefix(upper, "COUNT(") {
		return "BIGINT"
	}
	if strings.HasPrefix(upper, "AVG(") {
		return "DOUBLE"
	}
	if strings.HasPrefix(upper, "SUM(") || strings.HasPrefix(upper, "MIN(") || strings.HasPrefix(upper, "MAX(") {
		lparen := strings.Index(upper, "(")
		rparen := strings.LastIndex(upper, ")")
		if lparen >= 0 && rparen > lparen && desc != nil {
			operand := strings.TrimSpace(upper[lparen+1 : rparen])
			if t := protoFieldTypeName(desc, operand); t != "UNKNOWN" {
				return t
			}
		}
		return "BIGINT"
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
	default:
		return reflect.TypeOf((*any)(nil)).Elem()
	}
}

func (r *cascadesRows) ColumnTypeNullable(index int) (nullable, ok bool) {
	return true, true
}

func (r *cascadesRows) ColumnTypeLength(index int) (length int64, ok bool) {
	typeName := r.ColumnTypeDatabaseTypeName(index)
	if typeName == "STRING" || typeName == "BYTES" {
		return math.MaxInt64, true
	}
	return 0, false
}

func (r *cascadesRows) ColumnTypePrecisionScale(index int) (precision, scale int64, ok bool) {
	return 0, 0, false
}

func (r *cascadesRows) Next(dest []driver.Value) error {
	if !r.rs.Next() {
		if err := r.rs.Err(); err != nil {
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
			var typeMismatch *predicates.TypeMismatchError
			if errors.As(err, &typeMismatch) {
				return api.NewError(api.ErrCodeCannotConvertType, typeMismatch.Error())
			}
			return err
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
	if agg, ok := op.(*logical.LogicalAggregate); ok {
		for _, a := range agg.Aggregates {
			upper := strings.ToUpper(a)
			if strings.Contains(upper, "DISTINCT ") {
				return "DISTINCT aggregates are not supported"
			}
		}
	}
	for _, ch := range op.Children() {
		if msg := findDistinctAggregate(ch); msg != "" {
			return msg
		}
	}
	return ""
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
					if dot := strings.IndexByte(upper, '.'); dot >= 0 {
						upper = upper[dot+1:]
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
// true if any table name contains "INFORMATION_SCHEMA". Uses typed
// parse tree nodes — no string matching on raw SQL text.
func referencesInformationSchema(ctx antlr.Tree) bool {
	if ctx == nil {
		return false
	}
	if atom, ok := ctx.(*antlrgen.AtomTableItemContext); ok {
		if tn := atom.TableName(); tn != nil {
			if strings.Contains(strings.ToUpper(tn.GetText()), "INFORMATION_SCHEMA") {
				return true
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
			opText := bo.GetText()
			if opText == "<<" || opText == ">>" {
				return opText
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

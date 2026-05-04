package embedded

import (
	"context"
	"database/sql/driver"
	"io"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/executor"
	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
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
		return g.naive.Plan(ctx, sql)
	}

	stmts := root.Statements()
	if stmts == nil || len(stmts.AllStatement()) != 1 {
		return g.naive.Plan(ctx, sql)
	}

	stmt := stmts.AllStatement()[0]
	sel := stmt.SelectStatement()
	if sel == nil {
		return g.naive.Plan(ctx, sql)
	}

	q := sel.Query()
	if q == nil {
		return g.naive.Plan(ctx, sql)
	}

	md := g.c.cachedMetaData()
	var logicalOp logical.LogicalOperator
	if md != nil {
		logicalOp = buildLogicalPlanForQueryWithCatalog(q, md)
	} else {
		logicalOp = buildLogicalPlanForQuery(q)
	}

	if logicalOp == nil {
		return g.naive.Plan(ctx, sql)
	}

	ref := query.TranslateToCascades(logicalOp)
	if ref == nil {
		return g.naive.Plan(ctx, sql)
	}

	if md == nil {
		return g.naive.Plan(ctx, sql)
	}

	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	planCtx := buildCascadesPlanContext(md)
	planner := cascades.NewPlanner(rules, planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules()).
		WithMaxTasks(1_000)

	bestExpr, _, planErr := planner.Plan(ref)
	if planErr != nil || bestExpr == nil {
		return g.naive.Plan(ctx, sql)
	}

	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		return g.naive.Plan(ctx, sql)
	}
	physPlan := ph.GetRecordQueryPlan()
	if physPlan == nil {
		return g.naive.Plan(ctx, sql)
	}

	return &cascadesPlan{
		conn:         g.c,
		md:           md,
		physicalPlan: physPlan,
		explain:      logicalOp.Explain(""),
	}, nil
}

// cascadesPlan wraps a Cascades-planned SELECT query with a pre-computed
// physical plan. Planning happens at Plan-time; Execute only runs the plan.
type cascadesPlan struct {
	conn         *EmbeddedConnection
	md           *recordlayer.RecordMetaData
	physicalPlan plans.RecordQueryPlan
	explain      string
}

func (p *cascadesPlan) IsUpdate() bool { return false }

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
			return nil, execErr
		}

		cols := deriveColumnsFromPlan(p.physicalPlan, p.md)
		rs := executor.NewRecordLayerResultSet(ctx, cursor, cols)
		rows = newCascadesRows(rs)
		return nil, nil
	})
	if txErr != nil {
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
func (d *metadataIndexDef) IndexIsUnique() bool        { return d.idx.Type == "value" && false }

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
	scan := findScanPlan(plan)
	if scan == nil || len(scan.GetRecordTypes()) == 0 {
		return nil
	}
	rt := md.GetRecordType(scan.GetRecordTypes()[0])
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

func deriveColumnsFromProjection(proj *plans.RecordQueryProjectionPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	scan := findScanPlan(proj.GetInner())
	var desc protoreflect.MessageDescriptor
	if scan != nil && len(scan.GetRecordTypes()) > 0 {
		rt := md.GetRecordType(scan.GetRecordTypes()[0])
		if rt != nil && rt.Descriptor != nil {
			desc = rt.Descriptor
		}
	}
	cols := make([]executor.ColumnDef, len(proj.GetProjections()))
	for i, v := range proj.GetProjections() {
		var name string
		if fv, ok := v.(*values.FieldValue); ok {
			name = fv.Field
		} else {
			name = v.Name()
		}
		typeName := aggregateTypeName(name, desc)
		if typeName == "" && desc != nil {
			typeName = protoFieldTypeName(desc, name)
		}
		if typeName == "" {
			typeName = "UNKNOWN"
		}
		cols[i] = executor.ColumnDef{
			Name:     strings.ToUpper(name),
			TypeName: typeName,
		}
	}
	return cols
}

func deriveColumnsFromAggregation(agg *plans.RecordQueryStreamingAggregationPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	scan := findScanPlan(agg.GetInner())
	var desc protoreflect.MessageDescriptor
	if scan != nil && len(scan.GetRecordTypes()) > 0 {
		rt := md.GetRecordType(scan.GetRecordTypes()[0])
		if rt != nil {
			desc = rt.Descriptor
		}
	}
	return buildAggColumns(agg.GetGroupingKeys(), agg.GetAggregates(), desc)
}

func deriveColumnsFromHashAggregation(agg *plans.RecordQueryHashAggregationPlan, md *recordlayer.RecordMetaData) []executor.ColumnDef {
	scan := findScanPlan(agg.GetInner())
	var desc protoreflect.MessageDescriptor
	if scan != nil && len(scan.GetRecordTypes()) > 0 {
		rt := md.GetRecordType(scan.GetRecordTypes()[0])
		if rt != nil {
			desc = rt.Descriptor
		}
	}
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
	operand := values.ExplainValue(a.Operand)
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
	fd := fields.ByName(protoreflect.Name(strings.ToLower(name)))
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
		cols[i], _ = md.ColumnName(i + 1)
	}
	return cols
}

func (r *cascadesRows) Close() error {
	return r.rs.Close()
}

func (r *cascadesRows) Next(dest []driver.Value) error {
	if !r.rs.Next() {
		if err := r.rs.Err(); err != nil {
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

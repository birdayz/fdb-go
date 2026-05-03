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
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// QueryEngine selects which query engine to use.
type QueryEngine int

const (
	// QueryEngineNaive uses the naive executor (default).
	QueryEngineNaive QueryEngine = iota
	// QueryEngineCascades routes SELECT through the Cascades planner.
	QueryEngineCascades
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

	naivePlan, _ := g.naive.Plan(ctx, sql)

	return &cascadesPlan{
		ref:     ref,
		conn:    g.c,
		md:      md,
		naive:   naivePlan,
		explain: logicalOp.Explain(""),
	}, nil
}

// cascadesPlan wraps a Cascades-planned SELECT query.
type cascadesPlan struct {
	ref             *expressions.Reference
	conn            *EmbeddedConnection
	md              *recordlayer.RecordMetaData
	naive           query.Plan
	explain         string
	physicalExplain string // set after Execute — the physical plan's Explain string
}

func (p *cascadesPlan) IsUpdate() bool { return false }

func (p *cascadesPlan) Explain() string {
	if p.physicalExplain != "" {
		return p.physicalExplain
	}
	// Run the planner to get the physical plan without executing.
	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	planCtx := buildCascadesPlanContext(p.md)
	planner := cascades.NewPlanner(rules, planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules())
	bestExpr, _, err := planner.Plan(p.ref)
	if err != nil || bestExpr == nil {
		return p.explain
	}
	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		return p.explain
	}
	physPlan := ph.GetRecordQueryPlan()
	if physPlan == nil {
		return p.explain
	}
	return physPlan.Explain()
}

func (p *cascadesPlan) Execute(ctx context.Context) (query.Result, error) {
	rules := append(cascades.DefaultExpressionRules(), cascades.BatchAExpressionRules()...)
	planCtx := buildCascadesPlanContext(p.md)

	planner := cascades.NewPlanner(rules, planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules())

	bestExpr, _, err := planner.Plan(p.ref)
	if err != nil || bestExpr == nil {
		return p.fallback(ctx)
	}

	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		return p.fallback(ctx)
	}

	physicalPlan := ph.GetRecordQueryPlan()
	if physicalPlan == nil {
		return p.fallback(ctx)
	}

	// Store the physical plan's Explain for introspection by tests.
	p.physicalExplain = physicalPlan.Explain()

	c := p.conn
	ss, ssErr := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
	if ssErr != nil {
		return p.fallback(ctx)
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
		cursor, execErr := executor.ExecutePlan(ctx, physicalPlan, store, evalCtx, nil,
			recordlayer.DefaultExecuteProperties())
		if execErr != nil {
			return nil, execErr
		}

		cols := deriveColumnsFromPlan(physicalPlan, p.md)
		rs := executor.NewRecordLayerResultSet(ctx, cursor, cols)
		rows = newCascadesRows(rs)
		return nil, nil
	})
	if txErr != nil {
		return p.fallback(ctx)
	}

	return query.Result{Rows: rows}, nil
}

func (p *cascadesPlan) fallback(ctx context.Context) (query.Result, error) {
	if p.naive != nil {
		return p.naive.Execute(ctx)
	}
	return query.Result{}, nil
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
	scan, ok := plan.(*plans.RecordQueryScanPlan)
	if !ok || len(scan.GetRecordTypes()) == 0 {
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

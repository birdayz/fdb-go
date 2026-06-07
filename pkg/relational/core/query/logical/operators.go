package logical

import (
	"fmt"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// ExistsSubquery pairs an existential alias with the logical plan for
// an EXISTS subquery. Carried on LogicalFilter so the Cascades
// translator can build ExistentialQuantifiers over the subquery plans.
type ExistsSubquery struct {
	Alias         values.CorrelationIdentifier
	Plan          LogicalOperator
	JoinPredicate predicates.QueryPredicate
}

// ScalarSubquery pairs a correlation alias with the logical plan for
// a scalar subquery `(SELECT MAX(v) FROM t2)`. Carried on
// LogicalFilter and LogicalProject so the Cascades translator can
// build inner plans. The executor pre-evaluates these and binds the
// scalar result under Alias before evaluating the outer plan's
// predicates/projections.
type ScalarSubquery struct {
	Alias values.CorrelationIdentifier
	Plan  LogicalOperator
}

// CorrelatedScalarSubquery pairs a correlation alias with a logical
// plan for a correlated scalar subquery like
// `(SELECT COUNT(*) FROM orders o WHERE o.customer_id = c.id)`.
// The inner plan has the correlation predicate baked in as a filter
// child of the aggregate — the executor re-evaluates it per outer
// row via FlatMap. Carried on LogicalProject.
type CorrelatedScalarSubquery struct {
	Alias      values.CorrelationIdentifier
	InnerPlan  LogicalOperator
	InnerAlias string
	ScalarCol  string // output column name from the inner aggregate
}

// --- Leaf operators (no children) ----------------------------------

// LogicalScan reads a single table. Empty Alias means "use the table
// name as the source alias."
type LogicalScan struct {
	Table string
	Alias string
}

// NewScan constructs a LogicalScan.
func NewScan(table, alias string) *LogicalScan {
	return &LogicalScan{Table: table, Alias: alias}
}

func (*LogicalScan) Children() []LogicalOperator { return []LogicalOperator{} }
func (s *LogicalScan) Explain(indent string) string {
	if s.Alias != "" && s.Alias != s.Table {
		return fmt.Sprintf("%sScan(%s AS %s)", indent, s.Table, s.Alias)
	}
	return fmt.Sprintf("%sScan(%s)", indent, s.Table)
}

// --- Unary operators (single child) --------------------------------

// LogicalFilter applies a WHERE/HAVING predicate to its child.
//
// Predicate is the preferred representation — a cascades
// QueryPredicate tree produced by the expr walker. When non-nil,
// Explain renders it via Predicate.Explain(), which yields the
// normalised form after simplification (tautology-folded, NOTs
// pushed to leaves, operands tree-walked).
//
// PredicateText is the fallback: the canonical source text of the
// WHERE expression. Used when the expression shape is out of the
// walker's scope (UnsupportedExpressionShapeError) or when the
// builder is constructed without a metadata-backed catalog (today's
// naive_generator Explain path, which has no transaction in scope).
type LogicalFilter struct {
	Input            LogicalOperator
	Predicate        predicates.QueryPredicate // preferred when non-nil
	PredicateText    string                    // source-text fallback
	ExistsSubqueries []ExistsSubquery          // subquery plans for EXISTS predicates
	ScalarSubqueries []ScalarSubquery          // subquery plans for scalar subqueries
}

// NewFilter constructs a text-only LogicalFilter — used by the
// non-catalog-aware logical-builder path where only canonical
// source text is available. Pair with NewFilterWithPredicate when
// a predicates.QueryPredicate tree is in scope (catalog-aware
// builder); the predicate-tree form takes precedence in Explain
// output when both are set.
func NewFilter(input LogicalOperator, pred string) *LogicalFilter {
	return &LogicalFilter{Input: input, PredicateText: pred}
}

// NewFilterWithPredicate constructs a LogicalFilter whose predicate
// is a cascades QueryPredicate tree. The text form is retained for
// diagnostics so Explain output stays stable even when the
// Predicate render differs from the source text (e.g. after
// tautology-folding).
func NewFilterWithPredicate(input LogicalOperator, pred predicates.QueryPredicate, text string) *LogicalFilter {
	return &LogicalFilter{Input: input, Predicate: pred, PredicateText: text}
}

func (f *LogicalFilter) Children() []LogicalOperator { return []LogicalOperator{f.Input} }
func (f *LogicalFilter) Explain(indent string) string {
	body := f.PredicateText
	if f.Predicate != nil {
		body = f.Predicate.Explain()
	}
	return fmt.Sprintf("%sFilter(%s)\n%s", indent, body, f.Input.Explain(indent+"  "))
}

// LogicalProject selects / renames columns and computes expressions.
// Each element of Projections is the canonical text of the projected
// expression or column name. Aliases (parallel slice) hold the output
// name; empty string means "use the underlying name."
//
// ProjectedValues (parallel to Projections) carries resolved Value
// trees when the catalog-aware builder successfully walks the ANTLR
// expression. nil slots mean the walker declined (unsupported shape)
// — the Cascades translator treats nil as "cannot translate" and
// returns nil for the whole query. Non-nil slots are used directly
// as projection Values in the Cascades plan.
type LogicalProject struct {
	Input           LogicalOperator
	Projections     []string
	Aliases         []string       // parallel to Projections; "" means no alias
	ProjectedValues []values.Value // parallel to Projections; nil slot = walker declined
	IsComputed      []bool         // parallel to Projections; true = expression, not plain column ref
	// AggregateSlots is parallel to Projections; true = the slot's value tree
	// CONTAINS an aggregate. Captured pre-rewrite, where the *AggregateValue
	// node is still present (rewriteAggregateValuesInTree destructively replaces
	// it with a typed FieldValue). Read once by the INSERT…SELECT promotion guard
	// to identify reliably-typed aggregate-result columns — plain columns are
	// concrete-typed too (ResolveIdentifier), so type-presence cannot
	// discriminate. A bridge until the Java end-state (PromoteValue projection
	// nodes), which dissolves this marker.
	AggregateSlots             []bool
	ScalarSubqueries           []ScalarSubquery           // uncorrelated scalar subquery plans (pre-evaluated)
	CorrelatedScalarSubqueries []CorrelatedScalarSubquery // correlated scalar subquery plans (re-evaluated per outer row via FlatMap)
}

func NewProject(input LogicalOperator, projs, aliases []string) *LogicalProject {
	return &LogicalProject{Input: input, Projections: projs, Aliases: aliases}
}

func (p *LogicalProject) Children() []LogicalOperator { return []LogicalOperator{p.Input} }
func (p *LogicalProject) Explain(indent string) string {
	parts := make([]string, len(p.Projections))
	for i, pj := range p.Projections {
		if i < len(p.Aliases) && p.Aliases[i] != "" {
			parts[i] = fmt.Sprintf("%s AS %s", pj, p.Aliases[i])
		} else {
			parts[i] = pj
		}
	}
	return fmt.Sprintf("%sProject(%s)\n%s", indent, strings.Join(parts, ", "), p.Input.Explain(indent+"  "))
}

// SortDir distinguishes ASC (default) from DESC.
type SortDir int

const (
	SortAsc SortDir = iota
	SortDesc
)

func (d SortDir) String() string {
	if d == SortDesc {
		return "DESC"
	}
	return "ASC"
}

// SortKey is one ORDER BY entry.
type SortKey struct {
	Expr       string // canonical text
	Dir        SortDir
	NullsFirst bool
	Value      values.Value // resolved Value expression (nil = use text as FieldValue)
}

// LogicalSort sorts its child rows by the given keys.
type LogicalSort struct {
	Input LogicalOperator
	Keys  []SortKey
}

func NewSort(input LogicalOperator, keys []SortKey) *LogicalSort {
	return &LogicalSort{Input: input, Keys: keys}
}

func (s *LogicalSort) Children() []LogicalOperator { return []LogicalOperator{s.Input} }
func (s *LogicalSort) Explain(indent string) string {
	parts := make([]string, len(s.Keys))
	for i, k := range s.Keys {
		parts[i] = fmt.Sprintf("%s %s", k.Expr, k.Dir.String())
	}
	return fmt.Sprintf("%sSort(%s)\n%s", indent, strings.Join(parts, ", "), s.Input.Explain(indent+"  "))
}

// LogicalLimit caps the row count, optionally after skipping Offset.
// Negative Limit means "no limit" (pure offset).
type LogicalLimit struct {
	Input  LogicalOperator
	Limit  int64
	Offset int64
}

func NewLimit(input LogicalOperator, limit, offset int64) *LogicalLimit {
	return &LogicalLimit{Input: input, Limit: limit, Offset: offset}
}

func (l *LogicalLimit) Children() []LogicalOperator { return []LogicalOperator{l.Input} }
func (l *LogicalLimit) Explain(indent string) string {
	// Negative Limit means "no cap" — plan output reads better as
	// Offset(N) than as Limit(-1 offset N).
	if l.Limit < 0 {
		return fmt.Sprintf("%sOffset(%d)\n%s", indent, l.Offset, l.Input.Explain(indent+"  "))
	}
	if l.Offset > 0 {
		return fmt.Sprintf("%sLimit(%d offset %d)\n%s", indent, l.Limit, l.Offset, l.Input.Explain(indent+"  "))
	}
	return fmt.Sprintf("%sLimit(%d)\n%s", indent, l.Limit, l.Input.Explain(indent+"  "))
}

// LogicalDistinct removes duplicate rows from its input.
type LogicalDistinct struct {
	Input LogicalOperator
}

func NewDistinct(input LogicalOperator) *LogicalDistinct {
	return &LogicalDistinct{Input: input}
}

func (d *LogicalDistinct) Children() []LogicalOperator { return []LogicalOperator{d.Input} }
func (d *LogicalDistinct) Explain(indent string) string {
	return fmt.Sprintf("%sDistinct\n%s", indent, d.Input.Explain(indent+"  "))
}

// LogicalAggregate runs GROUP BY + aggregate functions on its child.
// GroupKeys are the grouping-column expressions; Aggregates holds the
// aggregate-call text with aliases.
type LogicalAggregate struct {
	Input                  LogicalOperator
	GroupKeys              []string
	GroupKeyValues         []values.Value // resolved Value trees for GROUP BY expressions; nil slot = bare column
	Aggregates             []string       // e.g. "SUM(a)", "COUNT(*)"
	Aliases                []string       // parallel to Aggregates
	AggregateOperands      []values.Value // resolved operand Values (parallel to Aggregates); nil slot = use text
	HasDistinctAggregate   bool           // true when any aggregate uses DISTINCT (e.g. COUNT(DISTINCT x))
	Having                 string         // canonical HAVING predicate, "" when absent
	HavingPredicate        predicates.QueryPredicate
	HavingExistsSubqueries []ExistsSubquery // EXISTS subquery plans inside HAVING
	HavingScalarSubqueries []ScalarSubquery // scalar subquery plans inside HAVING
}

func NewAggregate(input LogicalOperator, groupKeys, aggs, aliases []string, having string) *LogicalAggregate {
	return &LogicalAggregate{
		Input:      input,
		GroupKeys:  groupKeys,
		Aggregates: aggs,
		Aliases:    aliases,
		Having:     having,
	}
}

func (a *LogicalAggregate) Children() []LogicalOperator { return []LogicalOperator{a.Input} }
func (a *LogicalAggregate) Explain(indent string) string {
	aggs := make([]string, len(a.Aggregates))
	for i, ag := range a.Aggregates {
		if i < len(a.Aliases) && a.Aliases[i] != "" {
			aggs[i] = fmt.Sprintf("%s AS %s", ag, a.Aliases[i])
		} else {
			aggs[i] = ag
		}
	}
	line := fmt.Sprintf("%sAggregate(group=[%s], agg=[%s]", indent,
		strings.Join(a.GroupKeys, ", "), strings.Join(aggs, ", "))
	if a.Having != "" {
		line += ", having=" + a.Having
	}
	line += ")"
	return fmt.Sprintf("%s\n%s", line, a.Input.Explain(indent+"  "))
}

// --- Binary operators (two children) -------------------------------

// JoinKind mirrors the SQL join flavour.
type JoinKind int

const (
	JoinInner JoinKind = iota
	JoinLeft
	JoinRight
	JoinFull // FULL OUTER JOIN (Go-only extension; Java has no outer joins)
)

func (k JoinKind) String() string {
	switch k {
	case JoinLeft:
		return "LeftJoin"
	case JoinRight:
		return "RightJoin"
	case JoinFull:
		return "FullJoin"
	default:
		return "InnerJoin"
	}
}

// LogicalJoin combines two children. Empty OnText means "no ON
// condition" (comma cross-join form — the outer WHERE provides the
// predicate). OnPredicate is the optional structured form (used by
// the catalog-aware walker); when non-nil, it takes precedence over
// OnText for Cascades lowering.
type LogicalJoin struct {
	Left        LogicalOperator
	Right       LogicalOperator
	Kind        JoinKind
	OnText      string
	OnPredicate any // predicates.QueryPredicate when set
}

func NewJoin(left, right LogicalOperator, kind JoinKind, on string) *LogicalJoin {
	return &LogicalJoin{Left: left, Right: right, Kind: kind, OnText: on}
}

// NewJoinWithPredicate builds a LogicalJoin with a structured ON predicate.
func NewJoinWithPredicate(left, right LogicalOperator, kind JoinKind, pred any) *LogicalJoin {
	return &LogicalJoin{Left: left, Right: right, Kind: kind, OnPredicate: pred}
}

func (j *LogicalJoin) Children() []LogicalOperator {
	return []LogicalOperator{j.Left, j.Right}
}

func (j *LogicalJoin) Explain(indent string) string {
	header := fmt.Sprintf("%s%s", indent, j.Kind.String())
	if j.OnText != "" {
		header += "(on " + j.OnText + ")"
	}
	return fmt.Sprintf("%s\n%s\n%s", header, j.Left.Explain(indent+"  "), j.Right.Explain(indent+"  "))
}

// LogicalUnion ties together two (or more) children with UNION [ALL]
// semantics. Distinct = true applies a DISTINCT dedup across the
// union.
type LogicalUnion struct {
	Inputs   []LogicalOperator
	Distinct bool
}

func NewUnion(inputs []LogicalOperator, distinct bool) *LogicalUnion {
	return &LogicalUnion{Inputs: inputs, Distinct: distinct}
}

func (u *LogicalUnion) Children() []LogicalOperator {
	return append([]LogicalOperator(nil), u.Inputs...)
}

func (u *LogicalUnion) Explain(indent string) string {
	tag := "UnionAll"
	if u.Distinct {
		tag = "UnionDistinct"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s", indent, tag)
	for _, in := range u.Inputs {
		fmt.Fprintf(&b, "\n%s", in.Explain(indent+"  "))
	}
	return b.String()
}

// --- DML -----------------------------------------------------------

// LogicalInsert describes an INSERT into Table. Source is the row-
// producing child (a SELECT); Columns is the projected-column list
// (may be empty to mean "all columns").
//
// ValuesArray holds the literal VALUES rows as a Cascades array Value
// (an ArrayConstructorValue of one RecordConstructorValue per row),
// mutually exclusive with Source. The translator wraps it in an
// ExplodeExpression so INSERT … VALUES streams through the same
// Cascades path as INSERT … SELECT — matching Java's
// RecordConstructorValue → array → Explode → Insert shape. It is a
// typed Value rather than a child operator because the rows are built
// from evaluated literals (parameters are already substituted at plan
// time), which needs the connection's evaluation context the pure
// logical builder lacks.
type LogicalInsert struct {
	Table       string
	Columns     []string
	Source      LogicalOperator
	ValuesArray values.Value
}

func NewInsert(table string, cols []string, source LogicalOperator) *LogicalInsert {
	return &LogicalInsert{Table: table, Columns: cols, Source: source}
}

func (i *LogicalInsert) Children() []LogicalOperator {
	if i.Source == nil {
		return []LogicalOperator{}
	}
	return []LogicalOperator{i.Source}
}

func (i *LogicalInsert) Explain(indent string) string {
	header := fmt.Sprintf("%sInsert(%s", indent, i.Table)
	if len(i.Columns) > 0 {
		header += "(" + strings.Join(i.Columns, ", ") + ")"
	}
	header += ")"
	if i.Source == nil {
		return header
	}
	return fmt.Sprintf("%s\n%s", header, i.Source.Explain(indent+"  "))
}

// LogicalUpdate updates Target rows matching Input with the per-col
// expression assignments in Sets.
type LogicalUpdate struct {
	Target string
	Sets   []Assignment
	Input  LogicalOperator // the scan + filter producing target rows
}

// Assignment is one SET clause entry. Expr is the canonical text (used
// for explain and as a fallback); Value is the resolved RHS expression
// Value (populated by the catalog-aware builder) that the executor
// evaluates against each target row. A nil Value means the text builder
// ran without catalog resolution.
type Assignment struct {
	Column string
	Expr   string // canonical text
	Value  values.Value
}

func NewUpdate(target string, sets []Assignment, input LogicalOperator) *LogicalUpdate {
	return &LogicalUpdate{Target: target, Sets: sets, Input: input}
}

func (u *LogicalUpdate) Children() []LogicalOperator {
	if u.Input == nil {
		return []LogicalOperator{}
	}
	return []LogicalOperator{u.Input}
}

func (u *LogicalUpdate) Explain(indent string) string {
	sets := make([]string, len(u.Sets))
	for i, a := range u.Sets {
		sets[i] = fmt.Sprintf("%s=%s", a.Column, a.Expr)
	}
	header := fmt.Sprintf("%sUpdate(%s SET %s)", indent, u.Target, strings.Join(sets, ", "))
	if u.Input == nil {
		return header
	}
	return fmt.Sprintf("%s\n%s", header, u.Input.Explain(indent+"  "))
}

// LogicalDelete removes rows matching Input from Target.
type LogicalDelete struct {
	Target string
	Input  LogicalOperator
}

func NewDelete(target string, input LogicalOperator) *LogicalDelete {
	return &LogicalDelete{Target: target, Input: input}
}

func (d *LogicalDelete) Children() []LogicalOperator {
	if d.Input == nil {
		return []LogicalOperator{}
	}
	return []LogicalOperator{d.Input}
}

func (d *LogicalDelete) Explain(indent string) string {
	header := fmt.Sprintf("%sDelete(%s)", indent, d.Target)
	if d.Input == nil {
		return header
	}
	return fmt.Sprintf("%s\n%s", header, d.Input.Explain(indent+"  "))
}

// --- LogicalValues (SELECT without FROM) ---------------------------

// LogicalValues is a leaf operator that yields a single row of
// constant/expression projections — the canonical target for a
// SELECT without a FROM clause (`SELECT 1 + 2, 'hello'`). Rows is
// a list of expression-texts per output column; Aliases is parallel
// (empty string = no AS clause). The number of rows is always 1 in
// this seed; a future VALUES (…), (…) literal table would extend to
// multi-row. Java equivalent: a ConstantExpression flowing through
// LogicalProjectionExpression.
type LogicalValues struct {
	Rows    []string
	Aliases []string
}

// NewValues constructs a LogicalValues with per-column expression
// text + parallel aliases.
func NewValues(rows, aliases []string) *LogicalValues {
	return &LogicalValues{Rows: rows, Aliases: aliases}
}

func (*LogicalValues) Children() []LogicalOperator { return []LogicalOperator{} }

func (v *LogicalValues) Explain(indent string) string {
	parts := make([]string, len(v.Rows))
	for i, r := range v.Rows {
		if i < len(v.Aliases) && v.Aliases[i] != "" {
			parts[i] = fmt.Sprintf("%s AS %s", r, v.Aliases[i])
		} else {
			parts[i] = r
		}
	}
	return fmt.Sprintf("%sValues(%s)", indent, strings.Join(parts, ", "))
}

// --- CTE -----------------------------------------------------------

// LogicalCTE wraps a named Common Table Expression around a Main
// query. The Body is the CTE's own plan; Main references Body via a
// LogicalScan on Name. Recursive CTEs set Recursive=true — Body may
// self-reference (the recursive evaluator lives at the executor
// layer for now).
type LogicalCTE struct {
	Name           string
	Body           LogicalOperator
	Main           LogicalOperator
	Recursive      bool
	ColumnAliases  []string // WITH c(a, b) AS (...) → renames body's output columns
	TraversalOrder TraversalOrder
}

type TraversalOrder int

const (
	TraversalLevelOrder TraversalOrder = iota
	TraversalPreOrder
	TraversalPostOrder
)

// NewCTE constructs a LogicalCTE.
func NewCTE(name string, body, main LogicalOperator, recursive bool) *LogicalCTE {
	return &LogicalCTE{Name: name, Body: body, Main: main, Recursive: recursive}
}

func (c *LogicalCTE) Children() []LogicalOperator {
	return []LogicalOperator{c.Body, c.Main}
}

func (c *LogicalCTE) Explain(indent string) string {
	tag := "CTE"
	if c.Recursive {
		tag = "RecursiveCTE"
	}
	header := fmt.Sprintf("%s%s(%s)", indent, tag, c.Name)
	return fmt.Sprintf("%s\n%s\n%s", header, c.Body.Explain(indent+"  "), c.Main.Explain(indent+"  "))
}

// --- DDL + passthrough ---------------------------------------------

// LogicalDDL wraps a DDL statement that has no meaningful tree
// shape (CREATE TABLE, DROP INDEX, …). Kind carries the DDL command
// ("CREATE TABLE" etc.) and Text the canonical source.
type LogicalDDL struct {
	Kind string
	Text string
}

func NewDDL(kind, text string) *LogicalDDL {
	return &LogicalDDL{Kind: kind, Text: text}
}

func (*LogicalDDL) Children() []LogicalOperator { return []LogicalOperator{} }
func (d *LogicalDDL) Explain(indent string) string {
	return fmt.Sprintf("%sDDL(%s: %s)", indent, d.Kind, d.Text)
}

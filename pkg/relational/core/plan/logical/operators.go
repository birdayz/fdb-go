package logical

import (
	"fmt"
	"strings"
)

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

// LogicalFilter applies a WHERE/HAVING predicate to its child. The
// PredicateText carries the canonical source text of the predicate
// until Phase 4.0 ports Value / QueryPredicate.
type LogicalFilter struct {
	Input         LogicalOperator
	PredicateText string
}

func NewFilter(input LogicalOperator, pred string) *LogicalFilter {
	return &LogicalFilter{Input: input, PredicateText: pred}
}

func (f *LogicalFilter) Children() []LogicalOperator { return []LogicalOperator{f.Input} }
func (f *LogicalFilter) Explain(indent string) string {
	return fmt.Sprintf("%sFilter(%s)\n%s", indent, f.PredicateText, f.Input.Explain(indent+"  "))
}

// LogicalProject selects / renames columns and computes expressions.
// Each element of Projections is the canonical text of the projected
// expression or column name. Aliases (parallel slice) hold the output
// name; empty string means "use the underlying name."
type LogicalProject struct {
	Input       LogicalOperator
	Projections []string
	Aliases     []string // parallel to Projections; "" means no alias
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
	Expr string // canonical text
	Dir  SortDir
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
	if l.Offset > 0 {
		return fmt.Sprintf("%sLimit(%d offset %d)\n%s", indent, l.Limit, l.Offset, l.Input.Explain(indent+"  "))
	}
	return fmt.Sprintf("%sLimit(%d)\n%s", indent, l.Limit, l.Input.Explain(indent+"  "))
}

// LogicalAggregate runs GROUP BY + aggregate functions on its child.
// GroupKeys are the grouping-column expressions; Aggregates holds the
// aggregate-call text with aliases.
type LogicalAggregate struct {
	Input      LogicalOperator
	GroupKeys  []string
	Aggregates []string // e.g. "SUM(a)", "COUNT(*)"
	Aliases    []string // parallel to Aggregates
	Having     string   // canonical HAVING predicate, "" when absent
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
)

func (k JoinKind) String() string {
	switch k {
	case JoinLeft:
		return "LeftJoin"
	case JoinRight:
		return "RightJoin"
	default:
		return "InnerJoin"
	}
}

// LogicalJoin combines two children. Empty OnText means "no ON
// condition" (comma cross-join form — the outer WHERE provides the
// predicate).
type LogicalJoin struct {
	Left   LogicalOperator
	Right  LogicalOperator
	Kind   JoinKind
	OnText string
}

func NewJoin(left, right LogicalOperator, kind JoinKind, on string) *LogicalJoin {
	return &LogicalJoin{Left: left, Right: right, Kind: kind, OnText: on}
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
// producing child (values list or a SELECT); Columns is the
// projected-column list (may be empty to mean "all columns").
type LogicalInsert struct {
	Table   string
	Columns []string
	Source  LogicalOperator
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

// Assignment is one SET clause entry.
type Assignment struct {
	Column string
	Expr   string // canonical text
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

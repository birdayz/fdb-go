package logical

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ExistsSubquery pairs an existential alias with the logical plan for
// an EXISTS subquery. Carried on LogicalFilter so the Cascades
// translator can build ExistentialQuantifiers over the subquery plans.
type ExistsSubquery struct {
	Alias         values.CorrelationIdentifier
	Plan          LogicalOperator
	JoinPredicate predicates.QueryPredicate
	// KnownTruth is non-nil when the front-end can prove the EXISTS result from
	// relational cardinality alone. A non-grouped aggregate produces exactly one
	// row before pagination; applying a literal LIMIT/OFFSET therefore makes the
	// result statically TRUE (the row survives) or FALSE (the row is skipped).
	// LIMIT 0 is likewise empty for every supported inner shape. The translator
	// substitutes the constant in positive and negated WHERE consumers instead
	// of building a correlated semi-join. Nil means the result is data-dependent.
	KnownTruth predicates.TriBool
	// OuterOnlyJoinConjuncts marks a JoinPredicate carrying conjuncts with
	// NO inner-source reference that can FILTER (a nested-EXISTS middle
	// routes them here; the inside placement does not plan for that
	// composition; statically-TRUE tautologies are excluded - routing TRUE
	// is a no-op). The outer-routing validity matrix, enforced by
	// declineNegatedOuterOnlyEsq (predicate side) and
	// declineNegatedOuterOnlyEsqValue (value side) in the translator:
	//
	//	WHERE/ON + positive  -> VALID   (P AND EXISTS(Q) == EXISTS(P AND Q))
	//	WHERE/ON + negative  -> DECLINE (would compute P AND NOT-EXISTS(Q))
	//	projected + either   -> DECLINE (outer-routing filters the row
	//	                        stream; a projected boolean must not)
	//
	// HAVING is kept out of the surface by translateAggregate's blanket
	// rejection of HavingExistsSubqueries.
	OuterOnlyJoinConjuncts bool
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
// row via FlatMap. Carried on LogicalProject or LogicalFilter,
// depending on whether the scalar is consumed by SELECT or WHERE.
type CorrelatedScalarSubquery struct {
	Alias      values.CorrelationIdentifier
	InnerPlan  LogicalOperator
	InnerAlias string
	ScalarCol  string // output column name from the inner aggregate
	// StrictSingle is true when the source can produce multiple post-pagination
	// rows, so SQL standard at-most-one-row semantics apply: a second inner row
	// per outer row is a cardinality violation (21000), not silent truncation.
	// It is false when exact pagination caps a multi-row-capable source to 0/1,
	// or when the source is intrinsically <=1 (for example a non-grouped real
	// aggregate, for which even LIMIT >1 remains safe). Data-dependent LIMIT >1
	// is rejected before this IR until post-pagination scalar collapse exists.
	StrictSingle bool
}

// --- Leaf operators (no children) ----------------------------------

// LogicalScan reads a single table. Empty Alias means "use the table
// name as the source alias."
type LogicalScan struct {
	Table string
	Alias string
	// Binding is the scan's binding correlation name when its FROM alias
	// DUPLICATES an earlier leg's at the same level ("" = the alias binds —
	// every non-duplicate leg). Carried from the
	// parser's single mint authority (assignFromLegBindingIDs); consumers
	// read it via sourceBinding and never re-derive binding identity from
	// the alias. The SQL alias stays the DISPLAY qualifier (sourceAlias).
	Binding string
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

// LogicalUnnest is a lateral array UNNEST source in the FROM list
// (`FROM t, t.arr AS x [AT ord]`). It is the RIGHT child of a lateral
// LogicalJoin whose LEFT child is the source `t` it correlates to. The
// translator lowers it to an Explode of the correlated array field under a
// FlatMap of the outer source. Mirrors Java's
// `LogicalOperator.generateCorrelatedFieldAccess`. RFC-142 R5.
type LogicalUnnest struct {
	// Segments is the un-flattened dotted name of the array source
	// (`["T1","ARR1"]` for `T1.arr1`). Segment 0 names the in-scope outer
	// source; the remaining segments name the array field on it. Kept
	// un-flattened (no re-split of a joined string) so the translator
	// resolves segment-by-segment against the scope.
	Segments []string
	// Binding carries the comma source's duplicate-alias binding id
	// (see LogicalScan.Binding) so the
	// TABLE-FIRST demotion (demoteSchemaQualifiedUnnest) can restore it on
	// the demoted LogicalScan — a mis-classified schema-qualified TABLE leg
	// is a table leg for binding purposes. A GENUINE unnest never consumes
	// it: a duplicate unnest AS/AT alias is rejected outright (RFC-142).
	Binding string
	// Alias is the AS alias (`x` in `... AS x`) bound to each unnested
	// element. Empty when the AS alias is omitted (AT-only form).
	Alias string
	// AtAlias is the AT ordinal alias (`ord` in `... AT ord`), empty when
	// absent. Its presence makes the Explode WITH ORDINALITY.
	AtAlias string
	// CorrelatedCollection is set only when this unnest is the primary source
	// of a correlated subquery (`EXISTS (SELECT ... FROM R.TAGS AS E)`). It is
	// the already-resolved array FieldValue over the outer correlation. A
	// regular lateral FROM leg gets that value from the LogicalJoin on its left
	// and leaves this nil. Carrying the resolved Value preserves minted outer
	// correlations and avoids resolving the owner a second time.
	CorrelatedCollection values.Value
}

func (*LogicalUnnest) Children() []LogicalOperator { return []LogicalOperator{} }
func (u *LogicalUnnest) Explain(indent string) string {
	src := strings.Join(u.Segments, ".")
	suffix := ""
	if u.Alias != "" {
		suffix += " AS " + u.Alias
	}
	if u.AtAlias != "" {
		suffix += " AT " + u.AtAlias
	}
	return fmt.Sprintf("%sUnnest(%s%s)", indent, src, suffix)
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
// builder is constructed without a metadata-backed catalog (the
// catalog-less Explain path, which has no transaction in scope).
type LogicalFilter struct {
	Input                      LogicalOperator
	Predicate                  predicates.QueryPredicate  // preferred when non-nil
	PredicateText              string                     // source-text fallback
	ExistsSubqueries           []ExistsSubquery           // subquery plans for EXISTS predicates
	ScalarSubqueries           []ScalarSubquery           // uncorrelated scalar plans (pre-evaluated)
	CorrelatedScalarSubqueries []CorrelatedScalarSubquery // correlated scalar plans (per-row LEFT scalar join)
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
	// Pos is the 1-based SELECT-list position for a positional key
	// (`ORDER BY <n>`); 0 = not positional. A positional key IS an output
	// ordinal by SQL definition, so the translator bakes it directly to the
	// projection's output slot — no text-rendering round-trip, which
	// diverges for computed items whose canonical source text differs from the
	// baked output spelling.
	Pos int
	// Bare/Qualifier/Qualified: parse-tree segments of a plain column
	// reference key; zero values for positional and expression keys (their
	// Expr is a rendering only). Qualification is FullId SEGMENT COUNT.
	Bare      string
	Qualifier string
	Qualified bool
	// BareRef marks a key whose source text is a plain ONE-segment column
	// reference — the only shape SQL binds to an output alias. False for
	// qualified references (`ORDER BY d.x`), aggregates and computed
	// expressions: their canonical renderings ("X" after qualifier
	// stripping, "SUM(S.SCORE)") are indistinguishable from a delimited
	// alias's spelling, so alias-binding passes REQUIRE BareRef instead of
	// inspecting Expr text.
	BareRef bool
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
//
// LimitValue is an OPTIONAL runtime row cap (RFC-156 parameterized vector rank
// limit `... <= ?`): when non-nil the cap is evaluated at execution against the
// bound parameters and Limit is the no-cap sentinel (-1).
type LogicalLimit struct {
	Input      LogicalOperator
	Limit      int64
	Offset     int64
	LimitValue values.Value
}

func NewLimit(input LogicalOperator, limit, offset int64) *LogicalLimit {
	return &LogicalLimit{Input: input, Limit: limit, Offset: offset}
}

// NewRuntimeLimit builds a LIMIT whose row cap is a runtime Value (evaluated at
// execution). The static Limit is the no-cap sentinel (-1).
func NewRuntimeLimit(input LogicalOperator, limitValue values.Value, offset int64) *LogicalLimit {
	return &LogicalLimit{Input: input, Limit: -1, Offset: offset, LimitValue: limitValue}
}

// FoldTransparentUnaryInput returns (input, true) when op is a fold-transparent
// unary operator — one the RFC-141 projected-EXISTS fold descends THROUGH to
// reach the existential filter without changing the row shape. Only Sort and
// Limit qualify (a Project/Join/Aggregate/Distinct/Union reshapes the rows and is
// NOT fold-transparent). This is the SINGLE source of truth for the transparency
// set: both the translator's `findExistsFilterUnderUnaryChain` (which folds the
// projection through the chain) and the generator's `existsFilterReachableForFold`
// (which rejects a projected EXISTS the fold cannot reach) consult it, so the two
// can never silently diverge. Returns (nil, false) for any non-transparent op.
func FoldTransparentUnaryInput(op LogicalOperator) (LogicalOperator, bool) {
	switch o := op.(type) {
	case *LogicalSort:
		return o.Input, true
	case *LogicalLimit:
		return o.Input, true
	default:
		return nil, false
	}
}

func (l *LogicalLimit) Children() []LogicalOperator { return []LogicalOperator{l.Input} }
func (l *LogicalLimit) Explain(indent string) string {
	if l.LimitValue != nil {
		return fmt.Sprintf("%sLimit(%s)\n%s", indent, values.ExplainValue(l.LimitValue), l.Input.Explain(indent+"  "))
	}
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
// GroupKeys are the grouping-column expressions; Calls holds the
// structured aggregate calls (see AggregateCall) with parallel Aliases.
// GroupKey is one GROUP BY key in structured form (RFC-180 F-3): the display
// text is a rendering for output naming and diagnostics, never re-parsed;
// qualification and segments are parse-tree truth; Value is the resolved key
// (expressions always resolve; a bare column's Value is filled by the upgrade
// passes, nil = lazy bare read).
type GroupKey struct {
	Display   string
	Bare      string // last segment of a bare column ref; "" for expression keys
	Qualifier string // leading segment(s) when qualified; "" otherwise
	Qualified bool   // parse-tree segment count > 1
	Value     values.Value
}

type LogicalAggregate struct {
	Input     LogicalOperator
	GroupKeys []GroupKey
	// Calls is the SOLE aggregate-call representation. Builders populate it
	// from the parse tree; the translator requires it (RFC-180 F-3 retired
	// the parallel display-text slice).
	Calls                []AggregateCall
	Aliases              []string       // parallel to Calls
	AggregateOperands    []values.Value // resolved operand Values (parallel to Calls); nil slot = use text
	HasDistinctAggregate bool           // true when any aggregate uses DISTINCT (e.g. COUNT(DISTINCT x))
	// HasHaving: the query carried a HAVING clause. Presence SENTINEL only
	// (RFC-180 F-3 — the former canonical-text field was never re-parsed):
	// the translator declines when HasHaving is set but HavingPredicate is
	// nil, so an unstructured HAVING can never be silently dropped.
	HasHaving              bool
	HavingPredicate        predicates.QueryPredicate
	HavingExistsSubqueries []ExistsSubquery // EXISTS subquery plans inside HAVING
	HavingScalarSubqueries []ScalarSubquery // scalar subquery plans inside HAVING
}

// AggregateCall is the STRUCTURED form of one aggregate in the SELECT list,
// captured from the parse tree at build time — function, operand text (for
// result-map keying), and the star/distinct/bare-column classification. The
// translator consumes this instead of re-parsing the display text in
// Aggregates: a missing entry is a typed decline, never a string split
// (RFC-180 F-1 — the text reparse mangled nested arithmetic and silently
// dropped HAVING groups).
type AggregateCall struct {
	Func     string // upper-case aggregate function name (COUNT/SUM/MIN/MAX/AVG)
	Operand  string // canonical operand text; "*" for COUNT(*)
	Star     bool   // COUNT(*) (or COUNT(<non-null const>) collapsed to it)
	Distinct bool
	// BareColumn: the operand is a single column reference PER THE PARSE
	// TREE — a lazy FieldValue read of Operand is well-defined without
	// catalog resolution. False for computed/expression operands, which
	// require a resolved Value.
	BareColumn bool
	// Qualified: a BareColumn operand carried a table qualifier PER THE
	// PARSE TREE (FullId segment count > 1) — never a dot scan of the
	// rendered name, which a delimited identifier containing a literal dot
	// would false-positive. Always false for computed operands; consumers
	// gating on qualification fall back to a conservative canonical-text
	// scan there (a dot inside a computed rendering only ever comes from a
	// real qualified reference or a quoted literal, and the gate is
	// optimization-only).
	Qualified bool
}

// CanonicalName renders the call in the canonical upper-case display form —
// `FUNC(OPERAND)`, `FUNC(DISTINCT OPERAND)`, `COUNT(*)` — the alias-free
// output-column name for an aggregate. Every consumer compares these keys
// case-insensitively (upper-cased or via normalizeAggOutputName), so the
// upper-case Func is safe even where the SQL wrote the function lower-case.
func (c AggregateCall) CanonicalName() string {
	if c.Star {
		return c.Func + "(*)"
	}
	if c.Distinct {
		return c.Func + "(DISTINCT " + c.Operand + ")"
	}
	return c.Func + "(" + c.Operand + ")"
}

func NewAggregate(input LogicalOperator, groupKeys []GroupKey, calls []AggregateCall, aliases []string, hasHaving bool) *LogicalAggregate {
	return &LogicalAggregate{
		Input:     input,
		GroupKeys: groupKeys,
		Calls:     calls,
		Aliases:   aliases,
		HasHaving: hasHaving,
	}
}

func (a *LogicalAggregate) Children() []LogicalOperator { return []LogicalOperator{a.Input} }
func (a *LogicalAggregate) Explain(indent string) string {
	aggs := make([]string, len(a.Calls))
	for i, c := range a.Calls {
		if i < len(a.Aliases) && a.Aliases[i] != "" {
			aggs[i] = fmt.Sprintf("%s AS %s", c.CanonicalName(), a.Aliases[i])
		} else {
			aggs[i] = c.CanonicalName()
		}
	}
	keyNames := make([]string, len(a.GroupKeys))
	for i, k := range a.GroupKeys {
		keyNames[i] = k.Display
	}
	line := fmt.Sprintf("%sAggregate(group=[%s], agg=[%s]", indent,
		strings.Join(keyNames, ", "), strings.Join(aggs, ", "))
	if a.HavingPredicate != nil {
		line += ", having=" + a.HavingPredicate.Explain()
	} else if a.HasHaving {
		line += ", having=<unresolved>"
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
	// OnExistsSubqueries carries EXISTS subqueries lifted from the ON clause
	// (RFC-154 §5). The cascades translator turns each into an existential
	// quantifier on the join's SelectExpression, so the NLJ rule's
	// implementJoinWithExistential path builds the semi-join. Only populated for
	// INNER joins (OUTER EXISTS-in-ON is deferred — RFC-154 §5.2b).
	OnExistsSubqueries []ExistsSubquery
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
	// Binding is the derived/CTE leg's binding correlation name when its
	// FROM alias duplicates an earlier leg's ("" = Name binds). See
	// LogicalScan.Binding.
	Binding string
}

type TraversalOrder int

// The zero value is ANY — a recursion with NO traversal clause leaves the
// order to the planner (Java TraversalStrategy.ANY: both the level union
// and the DFS join implement, the cost model picks). An EXPLICIT
// `TRAVERSAL ORDER level_order` is TraversalLevelOrder and PINS the level
// union (Java's LEVEL gates the DFS rule off) — the two were once
// conflated, which made the explicit clause unable to force the plan.
const (
	TraversalAnyOrder TraversalOrder = iota
	TraversalLevelOrder
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

// --- Lateral array UNNEST source resolution (RFC-142) ---------------------

// FindOuterScanTable resolves a lateral unnest's outer source alias to the
// scanned table name by matching a LogicalScan whose source alias is `alias`
// (case-insensitive) among the VISIBLE FROM-scope sources of the outer leg
// `op`. It is used to resolve the outer source of a lateral unnest
// (`FROM t, t.arr AS x` → the scan of `t`) so its proto descriptor can be
// inspected for the array field.
//
// The walk MUST NOT descend into a CTE / derived-table BODY. A derived table
// `(SELECT … FROM T1) AS d` lowers to a `LogicalCTE{Body: <…scan of T1…>,
// Main: Scan(d)}`; only `d` is a visible source — `T1` is hidden inside the
// body and out of scope. Descending into `Body` would match the hidden `T1`
// scan and explode a correlated array against a source the query can't see (a
// silent-wrong / mis-classification). So at a `LogicalCTE` we resolve ONLY
// against its `Main` (the visible alias projection), never its `Body`. This
// mirrors Java's `resolveCorrelatedIdentifier` resolving against
// `getLogicalOperatorsIncludingOuter()` — the in-scope quantifiers, not nested
// query bodies.
//
// Returns "" when no VISIBLE matching scan is found (a non-scan outer, or a
// name hidden behind a derived-table boundary), so the caller falls back to
// the table path.
func FindOuterScanTable(op LogicalOperator, alias string) string {
	want := strings.ToUpper(alias)
	var walk func(LogicalOperator) string
	walk = func(o LogicalOperator) string {
		switch n := o.(type) {
		case *LogicalScan:
			a := n.Alias
			if a == "" {
				a = n.Table
			}
			if strings.EqualFold(a, want) {
				return n.Table
			}
			return ""
		case *LogicalUnnest:
			return ""
		case *LogicalCTE:
			// A derived table / CTE exposes ONLY its outer alias (its Main leg).
			// Do NOT descend into Body — the body's scans are out of scope.
			return walk(n.Main)
		default:
			for _, c := range o.Children() {
				if r := walk(c); r != "" {
					return r
				}
			}
			return ""
		}
	}
	return walk(op)
}

// FindOwnerUnnest returns the LogicalUnnest in the outer sub-plan `op` whose
// element (AS) alias matches `alias` — the OWNER of a CHAINED lateral unnest
// (`FROM t, t.arr AS x, x.sub AS y`: the second unnest's segment-0 `x` names
// the first unnest's element). Mirrors FindOuterScanTable's walk (does not
// descend into a CTE/derived Body — a derived table is its own FROM scope, so
// a chain rooted inside a derived body is out of scope), but matches unnest
// element aliases instead of scans. nil when no such prior unnest.
//
// The AT ordinal alias (`t.arr AS x AT o`) is deliberately NOT matched: `o`
// binds a scalar INTEGER ordinal, not a struct, so `o.sub` can never be a valid
// chain root. Matching it would misclassify `o.sub` as owned by this unnest and
// resolve `sub` against the ELEMENT's descriptor — emitting a plan that
// silently returns zero rows where Java rejects `o.sub` (a field access on a
// scalar). Only the AS element alias carries a chainable struct.
func FindOwnerUnnest(op LogicalOperator, alias string) *LogicalUnnest {
	var walk func(LogicalOperator) *LogicalUnnest
	walk = func(o LogicalOperator) *LogicalUnnest {
		switch n := o.(type) {
		case *LogicalUnnest:
			if strings.EqualFold(n.Alias, alias) {
				return n
			}
			return nil
		case *LogicalCTE:
			return walk(n.Main)
		default:
			for _, c := range o.Children() {
				if r := walk(c); r != nil {
					return r
				}
			}
			return nil
		}
	}
	return walk(op)
}

// IsUnnestOrdinalAlias reports whether `alias` names a prior lateral unnest's AT
// ORDINAL alias (`t.arr AS x AT o` → `o`) in the outer sub-plan `op`. That alias
// binds a scalar INTEGER ordinal, so a field access on it (`o.sub`) is a
// resolution error, not a chainable source — the caller surfaces the honest
// UNDEFINED_COLUMN Java produces instead of the generic unnest fallback. Same
// walk as FindOwnerUnnest (no CTE-Body descent), matching the AT alias only.
func IsUnnestOrdinalAlias(op LogicalOperator, alias string) bool {
	var walk func(LogicalOperator) bool
	walk = func(o LogicalOperator) bool {
		switch n := o.(type) {
		case *LogicalUnnest:
			return n.AtAlias != "" && strings.EqualFold(n.AtAlias, alias)
		case *LogicalCTE:
			return walk(n.Main)
		default:
			for _, c := range o.Children() {
				if walk(c) {
					return true
				}
			}
			return false
		}
	}
	return walk(op)
}

// OuterSourceIsDerivedTable reports whether `alias` (a lateral unnest's
// segment-0 outer source name) is bound, in the outer sub-plan `op`, to a
// DERIVED-TABLE / CTE leg — i.e. a `LogicalCTE` whose Name equals `alias`. It
// reads the logical tree STRUCTURALLY (not a translator-internal cteScope
// map), so it fires independent of cteScope population order and regardless of
// whether the alias also names a real same-named base table.
//
// A derived table `(SELECT …) AS D` lowers to a `LogicalCTE{Name:D,
// Main:Scan(D)}` inside the outer leg. When such a leg shadows a real
// same-named table, validating the unnest's array field against the base-table
// descriptor would explode the WRONG column (a derived-output silent-wrong).
// Detecting the derived/CTE leg here — by the in-scope quantifier alias,
// exactly as Java's generateCorrelatedFieldAccess resolves the in-scope source
// rather than the catalog table — lets the caller reject (or skip a base-table
// check for) the derived-output unnest in ALL cases.
//
// Like FindOuterScanTable, it does NOT descend into a CTE's Body (only its
// Main): a derived table is its own FROM scope; a same-named CTE nested inside
// another derived body is out of the current scope.
func OuterSourceIsDerivedTable(op LogicalOperator, alias string) bool {
	want := strings.ToUpper(alias)
	var walk func(LogicalOperator) bool
	walk = func(o LogicalOperator) bool {
		switch n := o.(type) {
		case *LogicalCTE:
			if strings.EqualFold(n.Name, want) {
				return true
			}
			// Only the visible Main leg is in scope; never the Body.
			return walk(n.Main)
		case *LogicalUnnest:
			return false
		default:
			for _, c := range o.Children() {
				if walk(c) {
					return true
				}
			}
			return false
		}
	}
	return walk(op)
}

// AttachedPlans returns the SUBQUERY plans attached to op beyond Children():
// EXISTS/scalar plans on filters, projections, aggregates (HAVING) and joins
// (ON). Tree walkers that must see EVERY built operator (e.g. the CTE
// alias-arity validator) traverse Children() + AttachedPlans(). Keep this
// switch in lockstep with the attachment fields on the structs above — a new
// attachment field added without an arm here silently escapes whole-tree
// validation.
func AttachedPlans(op LogicalOperator) []LogicalOperator {
	var out []LogicalOperator
	add := func(p LogicalOperator) {
		if p != nil {
			out = append(out, p)
		}
	}
	switch o := op.(type) {
	case *LogicalFilter:
		for _, sq := range o.ExistsSubqueries {
			add(sq.Plan)
		}
		for _, sq := range o.ScalarSubqueries {
			add(sq.Plan)
		}
		for _, sq := range o.CorrelatedScalarSubqueries {
			add(sq.InnerPlan)
		}
	case *LogicalProject:
		for _, sq := range o.ScalarSubqueries {
			add(sq.Plan)
		}
		for _, sq := range o.CorrelatedScalarSubqueries {
			add(sq.InnerPlan)
		}
	case *LogicalAggregate:
		for _, sq := range o.HavingExistsSubqueries {
			add(sq.Plan)
		}
		for _, sq := range o.HavingScalarSubqueries {
			add(sq.Plan)
		}
	case *LogicalJoin:
		for _, sq := range o.OnExistsSubqueries {
			add(sq.Plan)
		}
	}
	return out
}

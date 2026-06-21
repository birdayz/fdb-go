package query

import (
	"strconv"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
)

// ScalarSubqueryPlan pairs a correlation alias with a logical operator
// tree for a scalar subquery. Collected during translation and passed
// to the executor for pre-evaluation.
type ScalarSubqueryPlan struct {
	Alias values.CorrelationIdentifier
	Plan  logical.LogicalOperator
}

// TranslateToCascades converts a logical.LogicalOperator tree into a
// cascades RelationalExpression tree rooted in a Reference. This is
// the bridge between the SQL parser's logical plan and the Cascades
// optimizer.
//
// Returns the root Reference suitable for passing to Planner.Plan().
// Returns nil if the operator tree contains shapes that can't be
// translated (unsupported operators fall through to nil).
func TranslateToCascades(op logical.LogicalOperator) *expressions.Reference {
	ref, _ := TranslateToCascadesWithSubqueries(op, nil)
	return ref
}

// TranslateToCascadesWithSubqueries is like TranslateToCascades but
// also returns any scalar subquery plans collected during translation.
// These must be planned independently and pre-evaluated by the
// executor before running the main plan.
//
// md carries the record metadata used to source join-leg columns when building
// the source-anchored join result value (RFC-077 7.6). Pass nil to keep the legacy
// opaque-seed behavior — the no-md callers today are TranslateToCascades (used for
// scalar-subquery translation, which has no md in scope) and DML translation.
// (Tests pass real md where they exercise anchoring.) The scan leaf is NEVER typed
// from md (it stays Type.AnyRecord/UnknownType, matching Java — see RFC-077 v3
// amendment); md is consulted only to enumerate a leg's columns for the anchored
// RecordConstructor.
func TranslateToCascadesWithSubqueries(op logical.LogicalOperator, md *recordlayer.RecordMetaData) (*expressions.Reference, []ScalarSubqueryPlan) {
	t := &cascadesTranslator{
		md:              md,
		cteScope:        make(map[string]logical.LogicalOperator),
		cteExprScope:    make(map[string]expressions.RelationalExpression),
		cteColumnsScope: make(map[string][]values.Field),
	}
	ref := t.translateRef(op)
	return ref, t.scalarSubqueries
}

type cascadesTranslator struct {
	md           *recordlayer.RecordMetaData
	cteScope     map[string]logical.LogicalOperator
	cteExprScope map[string]expressions.RelationalExpression
	// cteColumnsScope holds the OUTPUT column schema of each pre-translated CTE
	// (recursive CTE / temp-table self-reference) registered in cteExprScope,
	// keyed by upper-cased CTE name (RFC-077 7.6). cteExprScope stores an opaque
	// RelationalExpression whose column names legColumns cannot recover; this
	// parallel map records them so a CTE reference used as a JOIN LEG anchors
	// (FieldValue(QOV(cteAlias), col) per column). nil/absent entry → not
	// column-derivable → the leg cannot anchor (a join over it is untranslatable;
	// the opaque-merge fallback was retired in RFC-077 7.6).
	cteColumnsScope  map[string][]values.Field
	scalarSubqueries []ScalarSubqueryPlan
}

// tableColumns returns a real table's columns (name + proto-derived type) from
// metadata, or nil when md is absent or the table can't be resolved. Field names
// are upper-cased to match the rest of the cascades layer's column naming. Used to
// source join-leg columns for the source-anchored join result value (RFC-077 7.6);
// it does NOT type the scan leaf.
func (t *cascadesTranslator) tableColumns(table string) []values.Field {
	if t.md == nil {
		return nil
	}
	rt := t.resolveRecordType(table)
	if rt == nil || rt.Descriptor == nil {
		return nil
	}
	protoFields := rt.Descriptor.Fields()
	fields := make([]values.Field, 0, protoFields.Len())
	for i := 0; i < protoFields.Len(); i++ {
		fd := protoFields.Get(i)
		fields = append(fields, values.Field{
			Name:      strings.ToUpper(string(fd.Name())),
			FieldType: fieldTypeForFD(fd),
			Ordinal:   i,
		})
	}
	return fields
}

// resolveRecordType resolves a table name to its record type CASE-INSENSITIVELY.
// The SQL path upper-cases table names in the logical plan (Scan(ORDER)), but the
// metadata keys record types under their proto names (mixed case, e.g. "Order"),
// so a direct GetRecordType("ORDER") misses. The relational layer is
// case-insensitive (Java's SemanticAnalyzer resolves identifiers case-folded), so
// fall back to a case-insensitive scan when the exact lookup misses. Without this
// every real-table join seed fell back to the opaque merge — the columns were
// unreachable (RFC-077 7.6).
//
// The fallback picks the lexicographically-smallest matching proto name so the
// result is DETERMINISTIC even in the (metadata-invalid) case of two record types
// that differ only by case — map iteration order is not stable. In well-formed
// metadata proto names are unique, so at most one name matches and the order is moot.
func (t *cascadesTranslator) resolveRecordType(table string) *recordlayer.RecordType {
	if rt := t.md.GetRecordType(table); rt != nil {
		return rt
	}
	var bestName string
	var best *recordlayer.RecordType
	for name, rt := range t.md.RecordTypes() {
		if strings.EqualFold(name, table) && (best == nil || name < bestName) {
			bestName, best = name, rt
		}
	}
	return best
}

// fieldTypeForFD maps a protoreflect.FieldDescriptor to a values.Type, mirroring
// jdbcTypeNameForFD (pkg/relational/core/embedded/select_helpers.go). Repeated/map
// and non-UUID message fields collapse to values.UnknownType — 7.6 doesn't model
// nested/array element types for the anchored leg columns. Columns are nullable
// (the flowed leg row doesn't carry per-column NOT NULL constraints).
func fieldTypeForFD(fd protoreflect.FieldDescriptor) values.Type {
	if fd.IsList() || fd.IsMap() {
		return values.UnknownType
	}
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return values.NewPrimitiveType(values.TypeCodeBoolean, true)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return values.NewPrimitiveType(values.TypeCodeInt, true)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return values.NewPrimitiveType(values.TypeCodeLong, true)
	case protoreflect.FloatKind:
		return values.NewPrimitiveType(values.TypeCodeFloat, true)
	case protoreflect.DoubleKind:
		return values.NewPrimitiveType(values.TypeCodeDouble, true)
	case protoreflect.StringKind:
		return values.NewPrimitiveType(values.TypeCodeString, true)
	case protoreflect.BytesKind:
		return values.NewPrimitiveType(values.TypeCodeBytes, true)
	case protoreflect.MessageKind:
		if msg := fd.Message(); msg != nil && string(msg.FullName()) == functions.UUIDProtoMessageName {
			return values.NewPrimitiveType(values.TypeCodeUuid, true)
		}
		return values.UnknownType
	}
	return values.UnknownType
}

// legColumns derives the OUTPUT columns of a logical sub-plan as the field set a
// source-anchored join result value would carry for that leg (RFC-077 7.6 Option
// B). The names it returns are EXACTLY the field names NewAnchoredJoinRecord
// emits, so a parent join's anchored RC composes its legs consistently — a leg
// that is itself a join exposes already-qualified (dotted) names that the parent
// propagates verbatim (the nested-join case NewAnchoredJoinRecord handles).
//
// Per-shape derivation (mirrors Option B's legOutputColumns):
//   - LogicalScan      → the table's bare columns from metadata (tableColumns).
//   - LogicalFilter    → its inner's columns (a filter is row-shape-preserving).
//   - LogicalLimit     → its inner's columns (limit is row-shape-preserving).
//   - LogicalJoin      → the field set of the join's anchored RC over its two
//     legs (qualified ALIAS.COL + bare-unique, dotted-propagated).
//   - LogicalProject   → the projected column names (the SELECT list).
//   - anything else (aggregate / distinct / union / cte / subquery) → nil.
//
// Returns nil whenever any required source is unavailable (no md, an unresolvable
// table, an unsupported shape) — the seed site (buildJoinResultValue) then treats
// the join as untranslatable (the retired opaque seed fallback is gone). The Field
// types are best-effort (UnknownType for derived shapes); only the NAMES are
// load-bearing for name-based resolution.
func (t *cascadesTranslator) legColumns(op logical.LogicalOperator) []values.Field {
	switch o := op.(type) {
	case *logical.LogicalScan:
		// A CTE/derived-table scan resolves to its BODY, not a real table —
		// translateScan honors cteScope/cteExprScope (a CTE name SHADOWS a real
		// table). legColumns mirrors that (RFC-077 7.6):
		//   - cteExprScope holds a PRE-TRANSLATED body (recursive-CTE reference /
		//     temp-table self-reference); its output columns are not readable from
		//     the RelationalExpression, so cteColumnsScope records them alongside —
		//     return that schema so the recursive-CTE leg anchors (nil entry → not
		//     derivable → the leg cannot anchor, a join over it is untranslatable);
		//   - cteScope holds the logical body: derive its output columns so the CTE
		//     leg anchors. The CTE is REMOVED from scope while deriving the body
		//     (exactly like translateScan) so a scan inside the body that references
		//     the same name resolves to the REAL table, not back to the CTE —
		//     otherwise legColumns recurses forever (the CTE-shadow stack overflow).
		key := strings.ToUpper(o.Table)
		if _, ok := t.cteExprScope[key]; ok {
			return t.cteColumnsScope[key]
		}
		if body, ok := t.cteScope[key]; ok {
			delete(t.cteScope, key)
			cols := t.derivedOutputColumns(body)
			t.cteScope[key] = body
			return cols
		}
		return t.tableColumns(o.Table)
	case *logical.LogicalFilter:
		return t.legColumns(o.Input)
	case *logical.LogicalLimit:
		return t.legColumns(o.Input)
	case *logical.LogicalJoin:
		left, right := o.Left, o.Right
		if o.Kind == logical.JoinRight {
			left, right = right, left
		}
		leftCols := t.legColumns(left)
		rightCols := t.legColumns(right)
		if leftCols == nil || rightCols == nil {
			return nil
		}
		leftAlias := values.NamedCorrelationIdentifier(sourceAlias(left))
		rightAlias := values.NamedCorrelationIdentifier(sourceAlias(right))
		rc := values.NewAnchoredJoinRecord([]values.AnchoredJoinLeg{
			{Alias: leftAlias, Columns: leftCols},
			{Alias: rightAlias, Columns: rightCols},
		})
		// A join leg exposes ONLY its already-qualified (DOTTED) columns to a parent
		// — the SOURCE-ACCURATE per-table forms (O.ID, C.PRICE, …). The anchored RC
		// ALSO carries bare names (its OWN resolution convenience at this level), but
		// those must NOT propagate: a parent re-qualifies a propagated bare under
		// sourceAlias(join)=right-leg, and a name from the right leg then collides
		// with its verbatim dotted key (NewRecordConstructorValue would suffix it
		// "_2" — a spurious key the opaque merge never produces). A buried column is
		// referenced via its dotted form after PartitionSelectRule rebasing, never
		// bare. (RFC-077 7.6; Torvalds nested-parity catch — codex's unique-bare
		// concern is pinned by TestFDB_NestedJoinUnqualifiedProjection.)
		var fields []values.Field
		for _, f := range rc.Fields {
			if strings.Contains(f.Name, ".") {
				fields = append(fields, values.Field{Name: f.Name, FieldType: values.UnknownType, Ordinal: len(fields)})
			}
		}
		return fields
	case *logical.LogicalProject:
		if len(o.Projections) == 0 {
			return nil
		}
		fields := make([]values.Field, len(o.Projections))
		for i := range o.Projections {
			name := o.Projections[i]
			if i < len(o.Aliases) && o.Aliases[i] != "" {
				name = o.Aliases[i]
			}
			fields[i] = values.Field{Name: strings.ToUpper(name), FieldType: values.UnknownType, Ordinal: i}
		}
		return fields
	case *logical.LogicalSort:
		// Row-shape-preserving: the sort's output columns are its inner's.
		return t.legColumns(o.Input)
	case *logical.LogicalDistinct:
		// Row-shape-preserving: DISTINCT does not change the column set.
		return t.legColumns(o.Input)
	case *logical.LogicalUnion:
		return t.unionOutputColumns(o)
	case *logical.LogicalAggregate:
		// Output columns = the GROUP BY keys followed by the aggregate output
		// column names (alias when present, else the aggregate text), mirroring
		// extractOutputColumns / buildAggColumns.
		return aggregateOutputColumns(o)
	case *logical.LogicalCTE:
		// A CTE-wrapped derived table used as a JOIN LEG (e.g. FROM a,
		// (SELECT …) b): translateCTE registers the body under the CTE name and
		// translates Main (a pass-through Scan of the name), so the leg's output
		// columns ARE the body's output columns — renamed by ColumnAliases when
		// present (WITH b(x,y) AS …), exactly as translateCTE wraps the body in a
		// renaming Project. A recursive CTE leg is not column-derivable here → nil
		// (the leg cannot anchor; the opaque-merge fallback was retired).
		if o.Recursive {
			return nil
		}
		if len(o.ColumnAliases) > 0 {
			fields := make([]values.Field, len(o.ColumnAliases))
			for i, name := range o.ColumnAliases {
				fields[i] = values.Field{Name: strings.ToUpper(name), FieldType: values.UnknownType, Ordinal: i}
			}
			return fields
		}
		return t.derivedOutputColumns(o.Body)
	default:
		// Subquery / Explode / DML and other non-row-producing shapes are not
		// column-derivable here → nil. A join seed with a non-derivable leg is
		// untranslatable (the opaque-merge fallback was retired in RFC-077 7.6);
		// every production query reaches a derivable leg shape (proven no-fallback).
		return nil
	}
}

// derivedOutputColumns derives a logical sub-plan's OUTPUT columns as a
// values.Field list (RFC-077 7.6) for shapes that define a column SCHEMA but
// are not themselves a join leg's quantifier source — used for CTE/derived-table
// bodies. It mirrors legColumns for the row-shape-preserving / project / aggregate
// shapes but, for a Project, returns the projected column NAMES (the body's
// output schema) so the CTE leg's columns match what the body flows. Returns nil
// for an underivable shape.
func (t *cascadesTranslator) derivedOutputColumns(op logical.LogicalOperator) []values.Field {
	switch o := op.(type) {
	case *logical.LogicalProject:
		if len(o.Projections) == 0 {
			return nil
		}
		fields := make([]values.Field, len(o.Projections))
		for i := range o.Projections {
			name := o.Projections[i]
			if i < len(o.Aliases) && o.Aliases[i] != "" {
				name = o.Aliases[i]
			}
			fields[i] = values.Field{Name: strings.ToUpper(name), FieldType: values.UnknownType, Ordinal: i}
		}
		return fields
	case *logical.LogicalAggregate:
		return aggregateOutputColumns(o)
	case *logical.LogicalDistinct:
		return t.derivedOutputColumns(o.Input)
	case *logical.LogicalSort:
		return t.derivedOutputColumns(o.Input)
	case *logical.LogicalLimit:
		return t.derivedOutputColumns(o.Input)
	case *logical.LogicalFilter:
		return t.derivedOutputColumns(o.Input)
	case *logical.LogicalUnion:
		return t.unionOutputColumns(o)
	case *logical.LogicalScan:
		return t.legColumns(o)
	case *logical.LogicalJoin:
		return t.legColumns(o)
	}
	return nil
}

// unionOutputColumns returns a UNION's output column schema for anchoring it as a
// join leg. SQL exposes the FIRST branch's names; the executor unions later
// branches by POSITION (remapUnionColumnsByPosition, keyed on planColumnNamesWithMD).
// That position-remap is reliable for PROJECTION/scan-schema'd branches — verified
// e2e: `(SELECT id AS x … UNION ALL SELECT v AS y …)` joins correctly — so anchoring
// a leg with mismatched branch aliases to the first branch's names is sound there.
//
// It is NOT reliable for an AGGREGATE-schema'd branch: planColumnNamesWithMD unwraps
// the aggregate to its input scan's column names, so a differently-aliased aggregate
// branch is not remapped to the first branch's name and its rows read as NULL —
// silently dropping join matches (a pre-existing executor gap, verified wrong on
// master too; tracked as TODO 7.6-union-remap). So when branch names DIFFER, anchor
// only if every branch's schema-defining node is normalizable (projection/scan); an
// aggregate-schema'd mismatched-alias union leg returns nil → untranslatable, a clean
// "unsupported" error rather than silently-wrong rows (codex). When branch names
// AGREE the remap is a no-op, so any shape is safe. Returns nil for no branches / an
// underivable first branch.
func (t *cascadesTranslator) unionOutputColumns(u *logical.LogicalUnion) []values.Field {
	if len(u.Inputs) == 0 {
		return nil
	}
	first := t.derivedOutputColumns(u.Inputs[0])
	if first == nil {
		return nil
	}
	allAgree := true
	allNormalizable := true
	for _, br := range u.Inputs {
		bc := t.derivedOutputColumns(br)
		if len(bc) != len(first) {
			return nil
		}
		for i := range bc {
			if bc[i].Name != first[i].Name {
				allAgree = false
			}
		}
		if !t.unionBranchNormalizable(br) {
			allNormalizable = false
		}
	}
	if allAgree || allNormalizable {
		return first
	}
	return nil
}

// unionBranchNormalizable reports whether the executor's union position-remap can
// remap this branch's columns to the first branch's names — i.e. whether the
// branch's SCHEMA-defining node is a projection or scan (planColumnNamesWithMD
// reports those branches' true output names). An AGGREGATE-schema'd branch is NOT
// normalizable (the executor unwraps it to its input scan's names — TODO
// 7.6-union-remap). Mirrors derivedOutputColumns's recursion through the
// row-shape-preserving wrappers; an unknown shape is conservatively not normalizable.
func (t *cascadesTranslator) unionBranchNormalizable(op logical.LogicalOperator) bool {
	switch o := op.(type) {
	case *logical.LogicalProject, *logical.LogicalJoin:
		return true
	case *logical.LogicalScan:
		// A scan may be a CTE/derived-table reference (translateScan resolves it from
		// the CTE body, not a real table). A real-table scan is remappable, but a
		// CTE-reference scan is only remappable if its BODY is — a CTE whose body is a
		// bare aggregate is NOT (the executor unwraps it to its input scan's names,
		// codex). Resolve cteScope and recurse, mirroring legColumns (remove-while-
		// recursing so a same-named scan inside the body resolves to the real table,
		// not back to the CTE). A pre-translated (recursive) CTE ref is unverifiable →
		// conservatively not normalizable.
		key := strings.ToUpper(o.Table)
		if _, ok := t.cteExprScope[key]; ok {
			return false
		}
		if body, ok := t.cteScope[key]; ok {
			delete(t.cteScope, key)
			n := t.unionBranchNormalizable(body)
			t.cteScope[key] = body
			return n
		}
		return true
	case *logical.LogicalAggregate:
		// Bare aggregate branch (no Project).
		if len(o.Aggregates) < 1 {
			return false // 0-aggregate (group-only) shape — distinct concern, gated.
		}
		// UNGROUPED: unchanged from RFC-080. An ungrouped aggregate has no aggregate-index
		// candidate (groupingCount==0) so it always plans as StreamingAgg; RFC-080 allowed these
		// union join legs and they work — do NOT re-gate them here (regressing previously-working
		// ungrouped legs, codex). Any residual ungrouped logical-vs-physical name divergence is a
		// pre-existing RFC-080 matter for the naming-unification follow-up, not RFC-081's scope.
		if len(o.GroupKeys) == 0 {
			return true
		}
		// GROUPED (RFC-081): a bare grouped aggregate can plan as AggregateIndex / MultiIntersection,
		// whose canonical row key can DIVERGE from the logical leg-schema name (aggregateOutputColumns,
		// the raw aggregate text) — so the executor's position-remap reads a missing key → NULL. The
		// names agree only for COUNT(*) and FUNC(<bare column>); a qualified operand (SUM(t.c) →
		// physical SUM(C)), a constant (COUNT(1)/COUNT(NULL) → grouped count-star index COUNT(*)), an
		// expression, or DISTINCT diverge → gate (clean error, never wrong rows). Unifying logical and
		// physical aggregate naming so the divergent forms work is a follow-up.
		return aggregateNamesStableForUnion(o)
	case *logical.LogicalDistinct:
		return t.unionBranchNormalizable(o.Input)
	case *logical.LogicalSort:
		return t.unionBranchNormalizable(o.Input)
	case *logical.LogicalLimit:
		return t.unionBranchNormalizable(o.Input)
	case *logical.LogicalFilter:
		return t.unionBranchNormalizable(o.Input)
	case *logical.LogicalUnion:
		if len(o.Inputs) == 0 {
			return false
		}
		for _, br := range o.Inputs {
			if !t.unionBranchNormalizable(br) {
				return false
			}
		}
		return true
	case *logical.LogicalCTE:
		return t.unionBranchNormalizable(o.Body)
	}
	return false
}

// aggregateNamesStableForUnion reports whether every aggregate in a bare aggregate union
// branch has a STABLE output name — i.e. the logical leg-schema name (aggregateOutputColumns,
// the raw aggregate text) equals the physical row key the executor writes (StreamingAgg
// aggResultName / AggregateIndex canonical). Stable iff each aggregate is COUNT(*) or
// FUNC(<bare column identifier>); a qualified operand (SUM(t.c)), a constant (COUNT(1)), an
// expression (SUM(a*b)), or DISTINCT canonicalizes differently between the two, so the union
// position-remap would read a missing key → NULL (RFC-081). False for a 0-aggregate branch.
//
// The aggregate TEXT is the reliable signal: AggregateOperands is nil for many shapes (e.g.
// SUM(col)) depending on the build path, and a.Aggregates is canonical planner output (not raw
// SQL), so inspecting it is sound here.
func aggregateNamesStableForUnion(a *logical.LogicalAggregate) bool {
	if len(a.Aggregates) == 0 || a.HasDistinctAggregate {
		return false
	}
	for i, text := range a.Aggregates {
		// A constant operand — COUNT(1), COUNT(NULL), COUNT(TRUE) — folds into count-star,
		// so a grouped aggregate index reports COUNT(*) ≠ the logical text. The resolved
		// operand reliably distinguishes a literal (ConstantValue) from a column, which the
		// text cannot (COUNT(NULL)'s arg "NULL" looks like an identifier). Literals resolve
		// even where a column operand is left nil, so this catch is sound.
		if i < len(a.AggregateOperands) {
			if _, isConst := a.AggregateOperands[i].(*values.ConstantValue); isConst {
				return false
			}
		}
		arg, ok := aggregateArgText(text)
		if !ok {
			return false
		}
		if arg == "*" {
			continue // COUNT(*)
		}
		if !isBareColumnIdentifier(arg) {
			return false // qualified / expression / numeric-literal operand → name diverges
		}
	}
	return true
}

// aggregateArgText returns the argument of a canonical aggregate text "FUNC(arg)" — the
// content between the first '(' and the last ')'. ok=false when not in that shape.
func aggregateArgText(text string) (string, bool) {
	openIdx := strings.IndexByte(text, '(')
	closeIdx := strings.LastIndexByte(text, ')')
	if openIdx < 0 || closeIdx <= openIdx {
		return "", false
	}
	return text[openIdx+1 : closeIdx], true
}

// isBareColumnIdentifier reports whether s is a single unqualified SQL identifier
// ([A-Za-z_][A-Za-z0-9_]*): no qualifier dot, whitespace (DISTINCT), operator (expression),
// '*', or leading digit (numeric literal). Exactly the operands whose FUNC(s) name is identical
// in the logical schema and the physical row key.
func isBareColumnIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// aggregateOutputColumns returns a LogicalAggregate's output column schema:
// the GROUP BY keys (bare column names, upper-cased) followed by each
// aggregate's output name (alias when present, else the aggregate text).
// Mirrors extractOutputColumns(LogicalAggregate). Types are UnknownType
// (only names are load-bearing for name-based resolution). Returns nil if
// the aggregate has no output columns.
func aggregateOutputColumns(a *logical.LogicalAggregate) []values.Field {
	var fields []values.Field
	for _, k := range a.GroupKeys {
		fields = append(fields, values.Field{Name: strings.ToUpper(k), FieldType: values.UnknownType, Ordinal: len(fields)})
	}
	for i, agg := range a.Aggregates {
		name := agg
		if i < len(a.Aliases) && a.Aliases[i] != "" {
			name = a.Aliases[i]
		}
		fields = append(fields, values.Field{Name: strings.ToUpper(name), FieldType: values.UnknownType, Ordinal: len(fields)})
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// buildJoinResultValue builds the result value for a binary join seed (RFC-077
// 7.6 Option B): the source-anchored RecordConstructorValue —
// FieldValue(QOV(leg), col) per column, named by NewAnchoredJoinRecord — so field
// pull-up resolves through composeFieldOverConstructor by name, anchored to the
// source quantifier. left/right are the POST-swap operands (RIGHT-join
// normalization happens at the call site), so the leg order matches the [outer,
// inner] ordering the column derivation + reversal signal read.
//
// Returns nil when a leg's output columns are not derivable (legColumns nil) or a
// leg alias is empty — the retired opaque merge seed fallback was removed in
// RFC-077 7.6 (proven unreachable for every md-bearing production query; only the
// catalog-free nil-md TranslateToCascades path, used by unit tests, can't derive a
// leg's columns). The caller treats nil as an untranslatable join.
func (t *cascadesTranslator) buildJoinResultValue(left, right logical.LogicalOperator, leftAlias, rightAlias string) values.Value {
	leftCols := t.legColumns(left)
	rightCols := t.legColumns(right)
	// Both legs must have a non-empty alias (the anchored RC keys columns by
	// QOV(alias); a zero alias would panic NewQuantifiedObjectValue) AND derivable
	// columns.
	if leftAlias == "" || rightAlias == "" || leftCols == nil || rightCols == nil {
		return nil
	}
	return values.NewAnchoredJoinRecord([]values.AnchoredJoinLeg{
		{Alias: values.NamedCorrelationIdentifier(leftAlias), Columns: leftCols},
		{Alias: values.NamedCorrelationIdentifier(rightAlias), Columns: rightCols},
	})
}

func (t *cascadesTranslator) translateRef(op logical.LogicalOperator) *expressions.Reference {
	expr := t.translateOp(op)
	if expr == nil {
		return nil
	}
	return expressions.InitialOf(expr)
}

func (t *cascadesTranslator) translateOp(op logical.LogicalOperator) expressions.RelationalExpression {
	if op == nil {
		return nil
	}
	switch o := op.(type) {
	case *logical.LogicalScan:
		return t.translateScan(o)
	case *logical.LogicalFilter:
		return t.translateFilter(o)
	case *logical.LogicalLimit:
		// Every LIMIT — top-level and nested alike — is translated to a
		// LogicalLimitExpression (→ RecordQueryLimitPlan) so it is applied
		// at its correct pipeline position by the operator. There is no
		// post-execution hoist anymore (see RFC-128): a nested LIMIT under
		// a Filter/Sort/Join inside a derived table must NOT be lifted to
		// the top-level pagination, which produced wrong rows. This mirrors
		// the correlated-scalar path (translateProjectWithCorrelatedScalar),
		// which already peels the inner LIMIT and re-emits it here.
		innerRef := t.translateRef(o.Input)
		if innerRef == nil {
			return nil
		}
		limitQ := t.namedQuantifier(sourceAlias(o.Input), innerRef)
		return expressions.NewLogicalLimitExpression(o.Limit, o.Offset, limitQ)
	case *logical.LogicalUnion:
		return t.translateUnion(o)
	case *logical.LogicalSort:
		return t.translateSort(o)
	case *logical.LogicalProject:
		return t.translateProject(o)
	case *logical.LogicalJoin:
		return t.translateJoin(o)
	case *logical.LogicalAggregate:
		return t.translateAggregate(o)
	case *logical.LogicalDistinct:
		return t.translateDistinct(o)
	case *logical.LogicalCTE:
		return t.translateCTE(o)
	case *logical.LogicalInsert:
		return t.translateInsert(o)
	case *logical.LogicalUpdate:
		return t.translateUpdate(o)
	case *logical.LogicalDelete:
		return t.translateDelete(o)
	default:
		return nil
	}
}

func (t *cascadesTranslator) translateScan(s *logical.LogicalScan) expressions.RelationalExpression {
	key := strings.ToUpper(s.Table)
	// Pre-translated expression scope (recursive CTE references).
	if expr, ok := t.cteExprScope[key]; ok {
		return expr
	}
	if body, ok := t.cteScope[key]; ok {
		// Remove this CTE from scope while translating its body so that
		// scans inside the body resolve to real tables, not back to the
		// CTE itself (which would cause infinite recursion when the CTE
		// name shadows the underlying table name).
		delete(t.cteScope, key)
		result := t.translateOp(body)
		t.cteScope[key] = body
		return result
	}
	return expressions.NewFullUnorderedScanExpression(
		[]string{s.Table}, values.UnknownType)
}

func (t *cascadesTranslator) translateFilter(f *logical.LogicalFilter) expressions.RelationalExpression {
	if f.Predicate == nil && f.PredicateText != "" {
		return nil
	}
	if f.Predicate != nil && isBareFieldPredicate(f.Predicate) {
		return nil
	}
	if f.Predicate != nil && predicateContainsUnsafeFunction(f.Predicate) {
		return nil
	}

	// Collect scalar subquery plans — they'll be planned independently
	// and pre-evaluated by the executor.
	for _, ssq := range f.ScalarSubqueries {
		t.scalarSubqueries = append(t.scalarSubqueries, ScalarSubqueryPlan{
			Alias: ssq.Alias,
			Plan:  ssq.Plan,
		})
	}

	// EXISTS subqueries: when the filter carries existential subquery
	// plans, build a SelectExpression with ForEach + Existential
	// quantifiers. The ExistentialValuePredicate in the predicate tree references
	// the existential alias; the planner's ImplementSimpleSelectRule
	// handles the existential quantifier via FirstOrDefaultPlan.
	// RFC-141: the existential quantifier attaches whenever the filter
	// carries existential subqueries — including a PROJECTED EXISTS with no
	// WHERE (f.Predicate == nil). For a projected-only EXISTS the existential
	// boolean is computed by the projection's ExistsValue, so no existential
	// WHERE filter is generated; the quantifier still must attach so the
	// FlatMap (FirstOrDefault inner) is built.
	if len(f.ExistsSubqueries) > 0 {
		// When the filter's input is a join, flatten into a single
		// SelectExpression with ForEach(left), ForEach(right), and
		// Existential(exists_scan). This avoids nesting one
		// SelectExpression (the join) inside another (the EXISTS filter),
		// which causes the Cascades planner to diverge. The NLJ rule
		// handles the 2+1 quantifier shape directly.
		if join, ok := f.Input.(*logical.LogicalJoin); ok {
			return t.translateJoinWithExists(join, f)
		}
	}

	// When the filter wraps an INNER join (FROM a, b WHERE ...), merge
	// the WHERE predicates into the SelectExpression so the NLJ rule
	// sees them as join predicates. For LEFT OUTER joins, the WHERE
	// must stay as a filter ABOVE the join — merging would turn WHERE
	// conditions into ON conditions, preventing NULL-padded rows from
	// being properly filtered.
	if join, ok := f.Input.(*logical.LogicalJoin); ok && f.Predicate != nil && len(f.ExistsSubqueries) == 0 && join.Kind != logical.JoinLeft && join.Kind != logical.JoinRight && join.Kind != logical.JoinFull {
		joinExpr := t.translateJoin(join)
		if joinExpr == nil {
			return nil
		}
		if sel, ok := joinExpr.(*expressions.SelectExpression); ok {
			merged := append(sel.GetPredicates(), f.Predicate)
			return expressions.NewSelectExpressionWithJoinType(
				sel.GetResultValue(),
				sel.GetQuantifiers(),
				merged,
				sel.GetSourceAliases(),
				sel.GetJoinType(),
			)
		}
	}

	innerRef := t.translateRef(f.Input)
	if innerRef == nil {
		return nil
	}

	if len(f.ExistsSubqueries) > 0 {
		// resultOverride nil ⇒ WHERE-EXISTS: the SelectExpression returns the
		// bare outer row (a projection above handles the SELECT list). RFC-141
		// projected-EXISTS folds the projection's RecordConstructor in here as
		// the result value (see translateProjectOverExistsFilter).
		return t.buildExistentialSelect(f, innerRef, nil)
	}

	var preds []predicates.QueryPredicate
	if f.Predicate != nil {
		preds = []predicates.QueryPredicate{f.Predicate}
	}
	return expressions.NewLogicalFilterExpression(
		preds,
		t.namedQuantifier(sourceAlias(f.Input), innerRef),
	)
}

// buildExistentialSelect builds the SelectExpression for a LogicalFilter that
// carries existential subqueries (RFC-141). It attaches a ForEach(outer) plus
// one Existential quantifier per subquery, threading the WHERE predicates (the
// ExistentialValuePredicate routes to the residual semi-join filter in the NLJ
// rule) and each subquery's correlation predicate. The resultValue is:
//
//   - resultOverride when non-nil — a PROJECTED EXISTS folds its projection's
//     RecordConstructor in here so the existential boolean is evaluated by the
//     FlatMap's result value with the inner binding live (matching Java's
//     "FLATMAP q0 -> { ... DEFAULT NULL AS q1 RETURN (q0.ID, exists(q1)) }");
//   - the bare outer QuantifiedObjectValue otherwise (WHERE-EXISTS — a separate
//     projection above handles the SELECT list).
func (t *cascadesTranslator) buildExistentialSelect(
	f *logical.LogicalFilter,
	innerRef *expressions.Reference,
	resultOverride values.Value,
) expressions.RelationalExpression {
	// Projected EXISTS + JOIN in FROM (no WHERE): the existential filter's input
	// is a LogicalJoin. Flatten the join's two ForEach quantifiers and the
	// existential quantifier(s) into ONE SelectExpression with the projection as
	// the result value — the same 2+1 flatten translateJoinWithExists does for
	// WHERE-EXISTS, but emitting the folded projection. Nesting the join
	// SelectExpression inside the existential one (the non-join path's single
	// outer quantifier over translateJoin(join)) would put the projected
	// ExistsValue above the join's own select; the flatten keeps the projection —
	// and its ExistsValue — in the same SelectExpression that owns the existential
	// quantifier, so the §8 guard passes and the boolean is computed with the
	// inner binding live (Java's single SelectExpression: all FROM quantifiers +
	// the existential + the projection).
	if join, ok := f.Input.(*logical.LogicalJoin); ok && resultOverride != nil {
		return t.buildExistentialJoinSelect(join, f, resultOverride)
	}

	outerAlias := sourceAlias(f.Input)
	outerQ := t.namedQuantifier(outerAlias, innerRef)
	quantifiers := []expressions.Quantifier{outerQ}

	allPreds := splitNonExistsPredicates(f.Predicate)
	allPreds = append(allPreds, extractExistsPredicates(f.Predicate)...)
	var innerCorrNames []string
	for _, esq := range f.ExistsSubqueries {
		subRef := t.translateRef(esq.Plan)
		if subRef == nil {
			return nil
		}
		existQ := expressions.NamedExistentialQuantifier(esq.Alias, subRef)
		quantifiers = append(quantifiers, existQ)
		// Register the existential inner under its UNIQUE alias (esq.Alias) and
		// rebase the join predicate onto it, so the inner correlation can never
		// collide with the outer source alias (the alias-shadow regression).
		innerCorrName, joinPred := existsInnerCorrelation(esq)
		innerCorrNames = append(innerCorrNames, innerCorrName)
		if joinPred != nil {
			allPreds = append(allPreds, joinPred)
		}
	}

	var sourceAliases []string
	if outerAlias != "" {
		sourceAliases = []string{outerAlias}
		sourceAliases = append(sourceAliases, innerCorrNames...)
	}

	resultValue := resultOverride
	if resultValue == nil {
		resultValue = values.NewQuantifiedObjectValue(outerQ.GetAlias())
	}
	return expressions.NewSelectExpressionWithAliases(
		resultValue,
		quantifiers,
		allPreds,
		sourceAliases,
	)
}

// buildExistentialJoinSelect folds a projection (resultValue) over a
// JOIN-in-FROM that carries projected-EXISTS subqueries into a single
// SelectExpression: ForEach(left), ForEach(right), and one Existential
// quantifier per subquery, with the join ON predicate and each subquery's
// correlation predicate threaded. Mirrors translateJoinWithExists but emits the
// folded projection as the result value instead of the join's anchored record,
// so the projected ExistsValue is evaluated by the FlatMap with the inner
// binding live (RFC-141 §8). Only INNER joins reach here for the projected fold;
// a LEFT/RIGHT/FULL outer FROM join with a projected EXISTS is left unfolded and
// the §8 guard rejects it cleanly.
func (t *cascadesTranslator) buildExistentialJoinSelect(
	j *logical.LogicalJoin,
	f *logical.LogicalFilter,
	resultValue values.Value,
) expressions.RelationalExpression {
	if j.Kind != logical.JoinInner {
		// Outer-join FROM with a projected EXISTS is not folded — the existential
		// semi-join cannot carry the NULL-padded drain. Return nil so the caller
		// falls back to the ordinary projection path; the §8 guard then rejects
		// the unfolded projected EXISTS cleanly (never a wrong result).
		return nil
	}
	leftRef := t.translateRef(j.Left)
	if leftRef == nil {
		return nil
	}
	rightRef := t.translateRef(j.Right)
	if rightRef == nil {
		return nil
	}
	leftAlias := sourceAlias(j.Left)
	rightAlias := sourceAlias(j.Right)

	leftQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(leftAlias), leftRef)
	rightQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(rightAlias), rightRef)
	quantifiers := []expressions.Quantifier{leftQ, rightQ}

	var allPreds []predicates.QueryPredicate
	if j.OnPredicate != nil {
		if qp, ok := j.OnPredicate.(predicates.QueryPredicate); ok {
			allPreds = append(allPreds, qp)
		}
	}
	// A projected EXISTS with no WHERE carries no filter predicate, but defend
	// against a residual non-EXISTS predicate riding on the synthesized filter.
	allPreds = append(allPreds, splitNonExistsPredicates(f.Predicate)...)
	allPreds = append(allPreds, extractExistsPredicates(f.Predicate)...)

	sourceAliases := []string{leftAlias, rightAlias}
	for _, esq := range f.ExistsSubqueries {
		subRef := t.translateRef(esq.Plan)
		if subRef == nil {
			return nil
		}
		existQ := expressions.NamedExistentialQuantifier(esq.Alias, subRef)
		quantifiers = append(quantifiers, existQ)
		innerCorrName, joinPred := existsInnerCorrelation(esq)
		if joinPred != nil {
			allPreds = append(allPreds, joinPred)
		}
		sourceAliases = append(sourceAliases, innerCorrName)
	}

	return expressions.NewSelectExpressionWithJoinType(
		resultValue,
		quantifiers,
		allPreds,
		sourceAliases,
		expressions.JoinInner,
	)
}

// translateProjectOverExistsFilter folds a projection that references a
// projected EXISTS into the existential SelectExpression's result value
// (RFC-141 Phase 2). It builds a RecordConstructorValue from the projection's
// values + output aliases and passes it as the SelectExpression result value,
// so the FlatMap's result computes the projected columns — including the
// existential boolean (ExistsValue.eval reads the inner binding the FlatMap
// establishes). Returns nil to fall back to the ordinary projection path when
// any projected Value is unresolved (the walker couldn't build it).
//
// chain holds the intervening unary operators (ORDER BY / LIMIT) that sat
// between the project and the existential filter, ordered top-to-bottom (the
// element closest to the project first). They are re-applied ON TOP of the
// folded SelectExpression so ORDER BY / LIMIT semantics are preserved — the
// sort/limit operates over the projected output rows (including the computed
// existential boolean), matching Java's
// `generateSort(generateSimpleSelect(output...), orderBys)`.
func (t *cascadesTranslator) translateProjectOverExistsFilter(
	p *logical.LogicalProject,
	f *logical.LogicalFilter,
	chain []logical.LogicalOperator,
) expressions.RelationalExpression {
	// Collect the FILTER's (uncorrelated) scalar subqueries. The fold's early
	// return in translateProject bypasses translateFilter — which is where
	// f.ScalarSubqueries would otherwise be registered — so a scalar subquery in
	// the WHERE of a projected-EXISTS query (`SELECT id, EXISTS(...) FROM t1 WHERE
	// price > (SELECT MAX(x) FROM t2)`) would never be pre-evaluated, leaving its
	// value unbound (NULL) and the comparison silently dropped (RFC-141 R4
	// round-11, the projected variant). Register them here, exactly as
	// translateFilter does, so the executor pre-evaluates and binds them. A
	// CORRELATED scalar subquery in the WHERE is not collected here (Java's
	// grammar cannot place one there either); only the uncorrelated list is
	// pre-evaluated.
	for _, ssq := range f.ScalarSubqueries {
		t.scalarSubqueries = append(t.scalarSubqueries, ScalarSubqueryPlan{
			Alias: ssq.Alias,
			Plan:  ssq.Plan,
		})
	}

	innerRef := t.translateRef(f.Input)
	if innerRef == nil {
		return nil
	}

	fields := make([]values.RecordConstructorField, len(p.Projections))
	outputNames := make(map[string]struct{}, len(p.Projections))
	for i, col := range p.Projections {
		var v values.Value
		if i < len(p.ProjectedValues) && p.ProjectedValues[i] != nil {
			v = p.ProjectedValues[i]
		} else if i < len(p.IsComputed) && p.IsComputed[i] {
			// A computed projection the walker couldn't resolve — bail so the
			// ordinary projection path (text fallback) handles it.
			return nil
		} else {
			v = &values.FieldValue{Field: strings.ToUpper(col), Typ: values.UnknownType}
		}
		name := strings.ToUpper(col)
		if i < len(p.Aliases) && p.Aliases[i] != "" {
			name = strings.ToUpper(p.Aliases[i])
		} else if _, isField := v.(*values.FieldValue); !isField {
			// An UNALIASED COMPUTED (non-field) expression — `id + 1`, `COUNT(*)`,
			// CASE, etc. The normal projection path names it with the GENERATED
			// positional `_i` (deriveProjectionColumnDef's `_idx` rule;
			// executeProjection also stores the value under the `_i` key). Using the
			// expression TEXT (`ID + 1`) here would change Rows.Columns() from `_0`
			// to `ID + 1` purely because an EXISTS was added — and break a downstream
			// positional reference to the generated column. Use the SAME positional
			// name so the folded column's record key + Name + Label are identical to
			// the non-EXISTS control on every axis (RecordConstructorValue.Evaluate
			// keys the row by f.Name; foldedColumnDef derives Name/Label from it).
			name = "_" + strconv.Itoa(i)
		}
		fields[i] = values.RecordConstructorField{Name: name, Value: v}
		outputNames[name] = struct{}{}
	}
	outputCount := len(fields)

	// Classify the FROM source as single-table or a (binary INNER) JOIN. This
	// drives how qualified ORDER BY keys are handled: for a single-table source
	// the merged outer row carries columns under BARE keys, so a qualified key is
	// stripped to its bare column; for a JOIN source the merged outer row carries
	// columns under QUALIFIED `LEG.COL` keys (the bare key is last-leg-wins and
	// would pick the WRONG leg — mergeRows writes both), so the qualified key must
	// be PRESERVED and resolve against the qualified merged-row key. This is the
	// sort-key analog of rebaseOuterLegValue / the P1a alias-binding fix.
	src := classifySortSource(f.Input)

	// A COMPUTED (non-column) ORDER BY key that is NOT one of the projected SELECT outputs
	// cannot be carried through the fold: collectExtraSortColumns can only append NAMED
	// columns, so the sort re-applied above the folded FlatMap would read a record that lacks
	// the expression's input columns and silently mis-order (e.g. `... ORDER BY col1 + 1`
	// where `col1 + 1` is not selected — codex). Bail the fold (→ the projected-EXISTS guard
	// rejects the query cleanly with ErrCodeUnsupportedQuery) rather than return wrong rows. A
	// SELECTED computed expression pulls up to its own output field and remains foldable.
	for _, op := range chain {
		s, ok := op.(*logical.LogicalSort)
		if !ok {
			continue
		}
		for _, k := range s.Keys {
			if src.sortKeyName(k) != "" {
				continue // a nameable column — appended as a hidden field or already in output
			}
			if k.Value == nil {
				return nil // computed via raw ORDER BY text, not nameable → unfoldable
			}
			if _, ok := pullUpToOutputField(k.Value, fields); !ok {
				return nil // computed key absent from the projection → unfoldable; guard rejects
			}
		}
	}

	// ORDER BY a column that is NOT in the SELECT output (e.g.
	// `SELECT id, EXISTS(...) FROM t1 ORDER BY col1`) needs Java's
	// remainingOrderByExpressions branch (LogicalOperator.generateSelect):
	// concat the extra sort columns onto the folded projection, sort, then
	// re-project to drop them. Without this the sort key (a FieldValue over a
	// column the result record doesn't carry) silently fails to resolve and the
	// sort becomes a no-op (wrong order). We therefore append every sort-key
	// column missing from the output as an extra trailing field — those
	// reference the outer scan row, which the existential SelectExpression's
	// outer quantifier flows in full, so they resolve.
	extraSortCols := collectExtraSortColumns(chain, fields, src)
	for _, ec := range extraSortCols {
		// The hidden field is named by its QUALIFIED field reference (`T1.ID`,
		// `T2.SK`) — collision-free with an output alias that shares the bare column
		// name — and carries the source-column VALUE: a bare field over the outer
		// scan row for single-table (`FieldValue{ID}`), a QUALIFIED leg reference for
		// a JOIN (`FieldValue{Field:COL, Child:QOV(LEG)}`, which the NLJ rule's
		// rebaseOuterLegValue rewrites onto the merged row's `LEG.COL` key). The sort
		// above resolves the key to this field; the final cleanup pull-up drops it.
		fields = append(fields, values.RecordConstructorField{Name: ec.name, Value: ec.val})
		outputNames[ec.name] = struct{}{}
	}

	resultValue := values.NewRecordConstructorValue(fields...)
	folded := t.buildExistentialSelect(f, innerRef, resultValue)
	if folded == nil {
		return nil
	}

	// Re-apply the intervening sort/limit on top of the folded projection.
	// chain is top-to-bottom; we rebuild bottom-up, wrapping the folded result
	// with the operator nearest the filter first, so the original nesting is
	// preserved (Project(Sort(Limit(Filter))) → Sort(Limit(FoldedSelect))).
	expr := folded
	for i := len(chain) - 1; i >= 0; i-- {
		ref := expressions.InitialOf(expr)
		switch op := chain[i].(type) {
		case *logical.LogicalSort:
			expr = t.applySortOverRef(op, ref, fields, src)
		case *logical.LogicalLimit:
			expr = expressions.NewLogicalLimitExpression(op.Limit, op.Offset, expressions.ForEachQuantifier(ref))
		default:
			// findExistsFilterUnderUnaryChain only collects Sort/Limit; any
			// other operator here is a bug — bail to the ordinary path.
			return nil
		}
		if expr == nil {
			return nil
		}
	}

	// Java's final pull-up: when extra ORDER BY columns were appended, re-project
	// only the original output so the sort columns don't leak into the result.
	//
	// RFC-141 round-8 ROOT FIX (P2): the cleanup MUST reuse the ORIGINAL per-column
	// alias provenance — leaving an unaliased column UNALIASED — so adding a hidden
	// sort column does not change any visible column's public label. The earlier
	// code force-aliased EVERY field to its datum Name (projAliases[i] = name),
	// which turned `SELECT t1.id` into an explicit alias `T1.ID` (label leaked the
	// qualifier) and re-aliased the EXISTS column to its raw expression. The fold's
	// first `outputCount` fields are the original SELECT outputs (extras are
	// appended after), so p.Aliases[i] (""==unaliased) is the original provenance.
	// We also preserve each projected value's TYPE (FieldValue.Typ = the folded
	// field's value type), so the EXISTS column stays BOOLEAN through the cleanup.
	if len(extraSortCols) > 0 {
		projVals := make([]values.Value, outputCount)
		projAliases := make([]string, outputCount)
		for i := 0; i < outputCount; i++ {
			// FieldValue.Field MUST equal the fold's f.Name exactly: the folded
			// output record is keyed by f.Name and FieldValue.Evaluate does an
			// exact-key lookup (no qualified→bare fallback). The cleanup column's
			// datum Name then equals that key, so a Scan never reads NULL.
			name := fields[i].Name
			typ := values.UnknownType
			if fields[i].Value != nil {
				if vt := fields[i].Value.Type(); vt != nil {
					typ = vt
				}
			}
			projVals[i] = &values.FieldValue{Field: name, Typ: typ}
			// Reuse the original SELECT-list alias (""==unaliased) so the cleanup's
			// label derivation matches the non-hidden-sort path exactly.
			if i < len(p.Aliases) {
				projAliases[i] = strings.ToUpper(p.Aliases[i])
			}
		}
		expr = expressions.NewLogicalProjectionExpressionWithAliases(
			projVals, projAliases, expressions.ForEachQuantifier(expressions.InitialOf(expr)))
	}
	return expr
}

// sortSource classifies a projected-EXISTS fold's FROM source for ORDER BY
// key handling. The FlatMap's merged outer row carries columns under different
// key shapes depending on the source:
//
//   - single-table (isJoin=false): the outer scan row flows columns under their
//     BARE names (`ID`, `COL1`). A qualified ORDER BY key (`t1.col1`) is stripped
//     to its bare column so it resolves against that row.
//   - binary INNER JOIN (isJoin=true): mergeRows writes BOTH bare last-leg-wins
//     keys AND authoritative QUALIFIED `LEG.COL` keys. The bare key picks the
//     WRONG leg, so a qualified ORDER BY key (`t2.sk`) must be PRESERVED as the
//     QUALIFIED key (`T2.SK`) and resolve against the qualified merged-row key.
//     legAliases are the join's leg FROM-aliases (left, right) — a qualified
//     sort key whose qualifier names a leg is rebased onto that leg; one whose
//     qualifier names neither leg is treated as bare (defensive: it cannot be
//     attributed to a known leg).
//
// This is the sort-key analog of the NLJ rule's rebaseOuterLegValue / the P1a
// alias-binding fix: a join's merged row is resolved by qualified key, never the
// last-leg-wins bare key.
type sortSource struct {
	isJoin     bool
	legAliases []string
}

// classifySortSource inspects the fold's FROM input. A binary INNER LogicalJoin
// is a join source (its two legs flow under qualified merged-row keys); anything
// else (a single scan, a CTE/derived table) is single-table. Only INNER joins
// reach the projected-EXISTS fold (buildExistentialJoinSelect rejects outer
// joins), so we classify only that shape as a join.
func classifySortSource(input logical.LogicalOperator) sortSource {
	if j, ok := input.(*logical.LogicalJoin); ok && j.Kind == logical.JoinInner {
		return sortSource{
			isJoin:     true,
			legAliases: []string{sourceAlias(j.Left), sourceAlias(j.Right)},
		}
	}
	return sortSource{isJoin: false}
}

// sortKeyName returns the upper-cased name a sort key resolves against the folded
// output record. Single-table: the BARE column (`T1.COL1`→`COL1`). JOIN: the
// QUALIFIED key when the qualifier names a known leg (`T2.SK`→`T2.SK`), else the
// bare column. Returns "" when the key is not a simple column reference (a
// computed expression). Used only by the computed-key nameability guard;
// output membership is VALUE-based (sortKeyInOutput), not by this name.
func (s sortSource) sortKeyName(k logical.SortKey) string {
	field := sortKeyFieldRef(k)
	if field == "" {
		return ""
	}
	return s.resolveKeyName(field)
}

// sortKeyFieldRef returns the RAW (possibly-qualified) upper-cased field reference
// a column sort key names — `T1.ID`, `COL1` — or "" when the key is a computed
// expression. Unlike sortKeyName it does NOT strip the qualifier, so callers can
// (a) build the source-column VALUE the key references for value-based output
// membership, and (b) name an appended hidden field by the qualified provenance
// (collision-free with an output alias — RFC-141 R4 round-10 P2b).
func sortKeyFieldRef(k logical.SortKey) string {
	if fv, ok := k.Value.(*values.FieldValue); ok {
		if fv.Child == nil {
			return strings.ToUpper(fv.Field)
		}
		// A composite leg reference (FieldValue{col, QOV(leg)}) — render LEG.COL.
		return strings.ToUpper(values.ExplainValue(fv))
	}
	if k.Value != nil {
		// Non-field Value (computed expression) — not a nameable column.
		return ""
	}
	field := strings.TrimSpace(k.Expr)
	if field == "" {
		return ""
	}
	// A bare or qualified identifier only — reject anything with operators,
	// parentheses, or whitespace (a computed expression), which the folded
	// record cannot expose by a single name.
	for _, r := range field {
		if !(r == '_' || r == '.' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
			return ""
		}
	}
	return strings.ToUpper(field)
}

// sortKeySourceValue returns the SOURCE-COLUMN value a column sort key references
// — the value that, when evaluated against the existential SelectExpression's
// flowed outer/merged row, yields the column the user asked to sort by:
//
//   - single-table: a BARE FieldValue over the outer scan row (`t1.id`→FieldValue{ID}).
//   - JOIN leg: a QUALIFIED leg reference (`t2.sk`→FieldValue{Field:SK, Child:QOV(T2)})
//     so the NLJ rule's rebaseOuterLegValue rewrites it onto the authoritative
//     merged-row `LEG.COL` key (never the last-leg-wins bare key).
//
// This is the value used for VALUE-based output membership: a sort key is "in the
// output" iff an output field's VALUE equals this source-column value — NOT iff
// its bare name matches an output field NAME. The bare-name test conflated a
// qualified source reference (`t1.id`) with an unrelated output alias that shares
// the bare name (`col1 AS id` → output column named `ID`), so the sort ordered by
// the wrong column (RFC-141 R4 round-10 P2b). Returns nil for a computed key.
func (s sortSource) sortKeySourceValue(k logical.SortKey) values.Value {
	field := sortKeyFieldRef(k)
	if field == "" {
		return nil
	}
	if s.isJoin {
		if qual, col, ok := splitQualifier(field); ok {
			for _, leg := range s.legAliases {
				if leg != "" && strings.ToUpper(leg) == qual {
					return values.NewFieldValue(
						values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(qual)),
						col, values.UnknownType)
				}
			}
		}
		return &values.FieldValue{Field: stripSortQualifier(field), Typ: values.UnknownType}
	}
	// Single-table: the outer scan row carries bare keys, so the source column is
	// the bare leaf (`t1.id`→ID), regardless of the qualifier.
	return &values.FieldValue{Field: stripSortQualifier(field), Typ: values.UnknownType}
}

// sortKeyInOutput reports whether some output field genuinely PROJECTS the source
// column a sort key references — a VALUE match against the source-column value,
// not a bare-name match against an output field's NAME. Returns the matching
// output field name (so the caller can pull the key up to it), or "" when the
// source column is not projected (a hidden remainingOrderBy column is appended).
func (s sortSource) sortKeyInOutput(k logical.SortKey, fields []values.RecordConstructorField) (string, bool) {
	src := s.sortKeySourceValue(k)
	if src == nil {
		return "", false
	}
	for _, f := range fields {
		if f.Value != nil && values.SemanticEqualsUnderAliasMap(src, f.Value, values.AliasMap{}) {
			return f.Name, true
		}
	}
	return "", false
}

// resolveKeyName maps a (possibly qualified) sort-key field reference to the name
// it resolves against the folded output record, per the source's key shape.
func (s sortSource) resolveKeyName(field string) string {
	up := strings.ToUpper(field)
	if !s.isJoin {
		return stripSortQualifier(up)
	}
	// JOIN source: keep the qualified `LEG.COL` key when the qualifier names a
	// known leg (it resolves the authoritative merged-row qualified key). A bare
	// key, or one whose qualifier is not a leg, falls back to the bare column.
	if qual, _, ok := splitQualifier(up); ok {
		for _, leg := range s.legAliases {
			if leg != "" && strings.ToUpper(leg) == qual {
				return up
			}
		}
	}
	return stripSortQualifier(up)
}

// extraSortCol is a hidden remainingOrderBy column appended to the folded
// projection: a collision-free NAME (the qualified field reference) and the
// source-column VALUE it reads (bare for single-table, qualified leg ref for a
// JOIN). The qualified name guarantees the hidden column never shadows an output
// alias that happens to share the bare column name (RFC-141 R4 round-10 P2b).
type extraSortCol struct {
	name string
	val  values.Value
}

// collectExtraSortColumns returns the hidden columns to append to the folded
// projection: the ORDER BY columns whose SOURCE column is NOT already projected
// by an output field (Java's remainingOrderByExpressions). Membership is
// VALUE-based (sortKeyInOutput) — a key is "in output" only when an output field
// genuinely projects its source column, never merely sharing a bare name with an
// output alias. Each appended column is named by its QUALIFIED field reference so
// it cannot collide with an output column. A sort key whose column can't be named
// (a computed expression) is skipped here — the caller
// (translateProjectOverExistsFilter) has already bailed the fold for any computed
// key absent from the projection, so a computed key reaching this point is a
// SELECTED expression that pulls up to its own output field. Order is stable and
// de-duplicated by name.
func collectExtraSortColumns(chain []logical.LogicalOperator, fields []values.RecordConstructorField, src sortSource) []extraSortCol {
	var extra []extraSortCol
	seen := map[string]struct{}{}
	for _, op := range chain {
		s, ok := op.(*logical.LogicalSort)
		if !ok {
			continue
		}
		for _, k := range s.Keys {
			name := sortKeyFieldRef(k)
			if name == "" {
				continue
			}
			if _, inOutput := src.sortKeyInOutput(k, fields); inOutput {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			val := src.sortKeySourceValue(k)
			if val == nil {
				continue
			}
			seen[name] = struct{}{}
			extra = append(extra, extraSortCol{name: name, val: val})
		}
	}
	return extra
}

// stripSortQualifier returns the upper-cased BARE column name of a (possibly
// qualified) sort-key field reference: `T1.COL1` → `COL1`, `COL1` → `COL1`. A SQL
// `alias.column` reference has the column as its FINAL dotted segment, so we take
// everything after the last `.`. An empty or trailing-dot input yields the
// upper-cased whole (defensive).
func stripSortQualifier(field string) string {
	up := strings.ToUpper(field)
	if i := strings.LastIndex(up, "."); i >= 0 && i+1 < len(up) {
		return up[i+1:]
	}
	return up
}

// splitQualifier splits an upper-cased `QUAL.COL` reference into (QUAL, COL, true).
// A bare name, an empty string, or a trailing/leading dot yields ("", "", false).
// Only a SINGLE qualifier is split (the LAST dot) — a deeper `A.B.C` is uncommon
// in the EXISTS fold and is treated as qualifier `A.B`, column `C`.
func splitQualifier(field string) (string, string, bool) {
	up := strings.ToUpper(field)
	i := strings.LastIndex(up, ".")
	if i <= 0 || i+1 >= len(up) {
		return "", "", false
	}
	return up[:i], up[i+1:], true
}

// applySortOverRef builds a LogicalSortExpression with the given inner
// reference, deriving its sort keys from the LogicalSort's keys. The keys
// reference the projected output record's columns (the folded SelectExpression
// flows a record whose fields are the projected columns by name), so a
// FieldValue over the column name resolves against that output — mirroring
// Java's OrderByExpression.pullUp onto the projection's result value.
//
// fields are the folded projection's output fields. A sort key that references a
// SELECT-list alias whose value is a COMPUTED expression — most importantly the
// projected ExistsValue for `ORDER BY has_t2 DESC` — arrives with k.Value set to
// the raw expression (upgradeSortKeyValues copies proj.ProjectedValues[idx]). If
// that raw value were re-applied here, it would be evaluated ABOVE the FlatMap,
// where the existential binding is dead — the EXISTS sort key would be false for
// every row and the order would be wrong. pullUpSortKeyValue rewrites such a key
// to a FieldValue over the already-computed output column (Java's pull-up onto
// the lower select's getResultValue()), so the sort orders by the materialized
// boolean column.
func (t *cascadesTranslator) applySortOverRef(s *logical.LogicalSort, ref *expressions.Reference, fields []values.RecordConstructorField, src sortSource) expressions.RelationalExpression {
	sortKeys := make([]expressions.SortKey, len(s.Keys))
	for i, k := range s.Keys {
		nf := k.NullsFirst
		v := k.Value
		if v == nil {
			v = &values.FieldValue{Field: k.Expr, Typ: values.UnknownType}
		}
		v = pullUpSortKeyValue(k, v, fields, src)
		sortKeys[i] = expressions.SortKey{
			Value:      v,
			Reverse:    k.Dir == logical.SortDesc,
			NullsFirst: &nf,
		}
	}
	return expressions.NewLogicalSortExpression(sortKeys, expressions.ForEachQuantifier(ref))
}

// pullUpSortKeyValue rewrites a sort-key Value onto the folded projection's
// output record (Java's OrderByExpression.pullUp against the select's result
// value). The fold re-applies the ORDER BY ON TOP of the folded projection, so a
// sort key must resolve to the OUTPUT field that produced it — exactly the
// correspondence the normal ORDER BY path (upgradeSortKeyValues) establishes
// when it resolves a SELECT-list alias to its projected Value.
//
// The resolution is, in priority order:
//
//  1. OUTPUT-FIELD-VALUE MATCH (pullUpToOutputField): the key's Value
//     semantically equals one of the projected output fields' Values → it is the
//     pull-up of a SELECT-list alias (or the computed EXISTS boolean), so it is
//     replaced by a FieldValue over THAT output column's name. This is the same
//     match the normal ORDER BY alias path performs: `upgradeSortKeyValues` set
//     the key's Value to the exact projected Value (pointer identity), so an
//     alias key — even one rewritten to a flat FieldValue over the underlying
//     column (`col1 AS id, id AS x ORDER BY x` rewrites `x`→FieldValue{ID} =
//     ProjectedValues[X]) — pulls up to the output field whose value it IS (`X`),
//     NOT to the output column that merely shares the underlying name (`ID`,
//     which here holds col1). Running this match FIRST for every key shape is the
//     fix for the round-6 P2a divergence: previously the FieldValue case returned
//     before trying it, so an alias whose value was a simple column read the
//     wrong output field.
//
//  2. SOURCE-COLUMN-VALUE MATCH (column keys only): a key that is a (possibly
//     qualified) column reference — `t1.id`, `col1`, `t2.sk` — resolves to the
//     output field whose VALUE is that SOURCE column. The source-column value is
//     built source-aware (src.sortKeySourceValue): a BARE FieldValue over the
//     outer scan row for single-table, a QUALIFIED leg reference for a JOIN leg
//     (resolving the AUTHORITATIVE merged-row `LEG.COL` key, never the last-leg-
//     wins bare key). The match runs against the EXTENDED output fields, which
//     include the hidden remainingOrderBy columns appended for keys not otherwise
//     projected — so a non-selected key (`ORDER BY t1.id` over `SELECT col1 AS
//     id`) pulls up to its hidden field (named by the QUALIFIED ref, collision-
//     free), and a SELECTED source column (`SELECT t1.id … ORDER BY t1.id`) pulls
//     up to that output field. Matching by VALUE — not by stripping the qualifier
//     to a bare name and searching output NAMES — is the round-10 P2b fix: the
//     bare-name search resolved a qualified source key to an unrelated output
//     alias that merely shared the bare name (sorting by the wrong column).
//
// A key matching neither is left unchanged — it resolves against the flowed
// record as-is.
func pullUpSortKeyValue(k logical.SortKey, v values.Value, fields []values.RecordConstructorField, src sortSource) values.Value {
	// (1) Output-field-value match on the key's RAW value — runs for EVERY key
	// shape, mirroring the normal ORDER BY alias resolution. Handles SELECT-list
	// aliases (incl. the computed EXISTS boolean) whose Value upgradeSortKeyValues
	// set to the projected Value.
	if pulled, ok := pullUpToOutputField(v, fields); ok {
		return pulled
	}
	// (2) Source-column-value match — a column key resolves to the output field
	// (incl. the hidden remainingOrderBy columns) whose VALUE is its source column.
	if srcVal := src.sortKeySourceValue(k); srcVal != nil {
		if pulled, ok := pullUpToOutputField(srcVal, fields); ok {
			return pulled
		}
	}
	// Bare/already-resolved key (or an outer-row reference): resolves against the
	// flowed record unchanged.
	return v
}

// pullUpToOutputField rewrites a sort-key Value to a FieldValue over the folded
// projection's OUTPUT column whose Value the key semantically equals — Java's
// OrderByExpression.pullUp onto the lower select's getResultValue(). This is the
// shared key↔output-field correspondence: a SELECT-list alias key (whose Value
// upgradeSortKeyValues set to the exact projected Value) pulls up to the output
// field it IS, not to a same-named column. Returns (rewritten, true) on a match,
// (nil, false) otherwise.
//
// A flat-name FieldValue key that is already an output column BY NAME (a bare
// column carried straight through, e.g. `ORDER BY id` where `id` is also the
// output name) is intentionally NOT matched here unless its VALUE matches an
// output field — it falls to the name-based resolution so it keeps resolving by
// name. The value match only fires when the key's Value is structurally the
// projected expression (the alias / computed case), which is precisely when
// pulling up to the output field is required for correctness.
//
// POINTER IDENTITY is preferred over structural semantic equality: when two
// output fields share a semantically-equal value (`id AS a, id AS b ORDER BY
// b`), `upgradeSortKeyValues` copied the EXACT projected Value pointer into the
// sort key, so the pointer-identical field is the one the key actually names
// (`b`). A single semantic-equality pass alone would return the first equal
// field (`a`) — harmless for the sort result (the values are equal so the order
// is identical), but it would pull up to the wrong output column name. The two
// passes keep the pulled-up name faithful to the named alias.
func pullUpToOutputField(v values.Value, fields []values.RecordConstructorField) (values.Value, bool) {
	// Pass 1: exact pointer identity — the field whose Value the sort key IS.
	for _, f := range fields {
		if f.Value != nil && f.Value == v {
			return &values.FieldValue{Field: f.Name, Typ: values.UnknownType}, true
		}
	}
	// Pass 2: structural semantic equality — for keys whose Value was rebuilt
	// (not pointer-copied) but is structurally the projected expression.
	for _, f := range fields {
		if f.Value != nil && values.SemanticEqualsUnderAliasMap(v, f.Value, values.AliasMap{}) {
			return &values.FieldValue{Field: f.Name, Typ: values.UnknownType}, true
		}
	}
	return nil, false
}

// findExistsFilterUnderUnaryChain descends from a project's input through any
// intervening single-child unary operators (ORDER BY / LIMIT) to find a
// LogicalFilter that carries existential subqueries. It returns that filter and
// the chain of intervening operators ordered top-to-bottom (closest to the
// project first), or (nil, nil) when the input is not such a shape. Only Sort
// and Limit are treated as "transparent" intervening operators — a Project,
// Join, Aggregate, etc. between the outer project and the filter changes the
// row shape and is NOT folded through.
func findExistsFilterUnderUnaryChain(input logical.LogicalOperator) (*logical.LogicalFilter, []logical.LogicalOperator) {
	var chain []logical.LogicalOperator
	cur := input
	for {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			if len(f.ExistsSubqueries) > 0 {
				return f, chain
			}
			return nil, nil
		}
		// Descend only through fold-transparent unary operators (Sort/Limit);
		// logical.FoldTransparentUnaryInput is the shared transparency set the
		// generator's existsFilterReachableForFold also consults.
		next, ok := logical.FoldTransparentUnaryInput(cur)
		if !ok {
			return nil, nil
		}
		chain = append(chain, cur)
		cur = next
	}
}

// projectionReferencesExistsSubquery reports whether any projected Value is (or
// contains) an ExistsValue — the structural signal that the projection must be
// folded into the existential SelectExpression's result value (RFC-141 Phase 2)
// so the boolean is computed with the inner existential binding live.
func projectionReferencesExistsSubquery(projected []values.Value) bool {
	for _, v := range projected {
		if v == nil {
			continue
		}
		found := false
		values.WalkValue(v, func(node values.Value) bool {
			if _, ok := node.(*values.ExistsValue); ok {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

func valueContainsUnsafeScalarFunction(v values.Value) bool {
	unsafe := false
	values.WalkValue(v, func(node values.Value) bool {
		if sf, ok := node.(*values.ScalarFunctionValue); ok {
			if !values.IsCascadesSafeScalarFunction(sf.FuncName) {
				unsafe = true
				return false
			}
		}
		return true
	})
	return unsafe
}

func predicateContainsUnsafeFunction(p predicates.QueryPredicate) bool {
	unsafe := false
	predicates.WalkPredicate(p, func(qp predicates.QueryPredicate) bool {
		switch pred := qp.(type) {
		case *predicates.ComparisonPredicate:
			if valueContainsUnsafeScalarFunction(pred.Operand) {
				unsafe = true
				return false
			}
			if pred.Comparison.Operand != nil && valueContainsUnsafeScalarFunction(pred.Comparison.Operand) {
				unsafe = true
				return false
			}
		case *predicates.ValuePredicate:
			if valueContainsUnsafeScalarFunction(pred.Value) {
				unsafe = true
				return false
			}
		}
		return true
	})
	return unsafe
}

func isBareFieldPredicate(p predicates.QueryPredicate) bool {
	vp, ok := p.(*predicates.ValuePredicate)
	if !ok {
		return false
	}
	_, isField := vp.Value.(*values.FieldValue)
	return isField
}

func (t *cascadesTranslator) translateUnion(u *logical.LogicalUnion) expressions.RelationalExpression {
	quantifiers := make([]expressions.Quantifier, 0, len(u.Inputs))
	for _, branch := range u.Inputs {
		ref := t.translateRef(branch)
		if ref == nil {
			return nil
		}
		quantifiers = append(quantifiers, expressions.ForEachQuantifier(ref))
	}
	if u.Distinct {
		return nil
	}
	return expressions.NewLogicalUnionExpression(quantifiers)
}

func (t *cascadesTranslator) translateSort(s *logical.LogicalSort) expressions.RelationalExpression {
	innerRef := t.translateRef(s.Input)
	if innerRef == nil {
		return nil
	}
	sortKeys := make([]expressions.SortKey, len(s.Keys))
	for i, k := range s.Keys {
		nf := k.NullsFirst
		v := k.Value
		if v == nil {
			v = &values.FieldValue{Field: k.Expr, Typ: values.UnknownType}
		}
		sortKeys[i] = expressions.SortKey{
			Value:      v,
			Reverse:    k.Dir == logical.SortDesc,
			NullsFirst: &nf,
		}
	}
	return expressions.NewLogicalSortExpression(
		sortKeys,
		t.namedQuantifier(sourceAlias(s.Input), innerRef),
	)
}

func (t *cascadesTranslator) translateProject(p *logical.LogicalProject) expressions.RelationalExpression {
	// Collect scalar subquery plans from projections. This MUST run for every
	// projection — including the RFC-141 projected-EXISTS fold below — because
	// a SELECT can mix a projected EXISTS with an (uncorrelated) scalar
	// subquery, e.g. `SELECT id, EXISTS(...), (SELECT MAX(id) FROM t2) FROM t1`.
	// The scalar subquery's plan is pre-evaluated by the executor and bound by
	// alias; skipping this collection (as the early-return fold path used to)
	// left the scalar column unbound → it came back NULL.
	for _, ssq := range p.ScalarSubqueries {
		t.scalarSubqueries = append(t.scalarSubqueries, ScalarSubqueryPlan{
			Alias: ssq.Alias,
			Plan:  ssq.Plan,
		})
	}

	// RFC-141 Phase 2: a projection over a filter that carries existential
	// subqueries, where the projection itself references a projected EXISTS,
	// folds INTO the existential SelectExpression's result value — so the
	// existential boolean is computed by the FlatMap with the inner binding
	// live (a separate Map above the FlatMap could not see that binding). Java
	// builds exactly this single FlatMap whose RETURN is the projection.
	//
	// The filter may not be the project's DIRECT input: an ORDER BY / LIMIT
	// sits between them (the builder emits Project(Sort(Filter)), with LIMIT
	// hoisted above the Project). findExistsFilterUnderUnaryChain sees THROUGH
	// those intervening unary operators to the existential filter. The fold
	// then re-applies the sort/limit ON TOP of the folded SelectExpression —
	// matching Java's `generateSort(generateSimpleSelect(output...), orderBys)`
	// (LogicalOperator.generateSelect): the projection is built first with the
	// existential binding live, then the sort wraps it, its keys rebased onto
	// the projected output record.
	if filter, chain := findExistsFilterUnderUnaryChain(p.Input); filter != nil &&
		projectionReferencesExistsSubquery(p.ProjectedValues) {
		// A projected EXISTS combined with a CORRELATED scalar subquery in the same
		// SELECT list cannot be folded (the fold's existential SelectExpression and
		// the correlated-scalar LEFT-OUTER join select are incompatible structures —
		// see findUnfoldableProjectedExists). The logical guard rejects this shape
		// before translation; this is defense-in-depth so the fold's early return can
		// NEVER bypass the correlated-scalar dispatch below and silently drop the
		// scalar column. Bailing here returns nil → the caller emits a clean
		// ErrCodeUnsupportedQuery rather than wrong rows.
		if len(p.CorrelatedScalarSubqueries) > 0 {
			return nil
		}
		if sel := t.translateProjectOverExistsFilter(p, filter, chain); sel != nil {
			return sel
		}
	}

	if len(p.CorrelatedScalarSubqueries) > 1 {
		return nil
	}
	if len(p.CorrelatedScalarSubqueries) == 1 {
		return t.translateProjectWithCorrelatedScalar(p)
	}

	innerRef := t.translateRef(p.Input)
	if innerRef == nil {
		return nil
	}
	projected := make([]values.Value, len(p.Projections))
	for i, col := range p.Projections {
		if i < len(p.ProjectedValues) && p.ProjectedValues[i] != nil {
			projected[i] = p.ProjectedValues[i]
			continue
		}
		// Computed expression without a resolved Value — the walker
		// couldn't handle this shape. Bail so the query falls back.
		if i < len(p.IsComputed) && p.IsComputed[i] {
			return nil
		}
		projected[i] = &values.FieldValue{Field: strings.ToUpper(col), Typ: values.UnknownType}
	}
	return expressions.NewLogicalProjectionExpressionWithAliases(
		projected,
		p.Aliases,
		t.namedQuantifier(sourceAlias(p.Input), innerRef),
	)
}

func (t *cascadesTranslator) translateProjectWithCorrelatedScalar(p *logical.LogicalProject) expressions.RelationalExpression {
	csq := p.CorrelatedScalarSubqueries[0]

	outerRef := t.translateRef(p.Input)
	if outerRef == nil {
		return nil
	}
	outerAlias := sourceAlias(p.Input)
	outerQ := t.namedQuantifier(outerAlias, outerRef)

	// Peel LogicalLimit off the inner plan and re-attach it explicitly here, so
	// the limit caps each per-outer-row evaluation of the correlated scalar
	// subquery. (translateOp now translates a LogicalLimit to a
	// LogicalLimitExpression at the inner's own position — RFC-128; for the
	// correlated case we instead bind it to the quantifier the join drives, so
	// we peel it before translating the inner.)
	innerPlan := csq.InnerPlan
	var innerLimit *logical.LogicalLimit
	if lim, ok := innerPlan.(*logical.LogicalLimit); ok {
		innerLimit = lim
		innerPlan = lim.Input
	}

	innerRef := t.translateRef(innerPlan)
	if innerRef == nil {
		return nil
	}

	// Wrap with LogicalLimitExpression if the inner plan had a LIMIT.
	if innerLimit != nil {
		innerAlias := sourceAlias(innerPlan)
		limitQ := t.namedQuantifier(innerAlias, innerRef)
		limitExpr := expressions.NewLogicalLimitExpression(innerLimit.Limit, innerLimit.Offset, limitQ)
		innerRef = expressions.InitialOf(limitExpr)
	}

	innerQ := t.namedQuantifier(csq.InnerAlias, innerRef)

	// Source-anchored correlated-scalar-subquery join seed (RFC-077 7.6).
	//
	// The inner is a scalar SUBQUERY exposing exactly ONE value. The projection
	// reads it as the QUALIFIED name <innerAlias>.<scalarCol> — replaceScalarSubqueryRef
	// builds that field name (upper(innerAlias)+"."+upper(scalarCol)) — and the inner
	// quantifier's row carries the scalar under the key scalarCol (the runtime
	// mergeRows PREFIXES every inner key with innerAlias, dots and all, so
	// <innerAlias>.<scalarCol> resolves iff the inner key == scalarCol; it does).
	// NewScalarSubqueryAnchoredRecord anchors the inner leg with that SINGLE field —
	// named EXACTLY <innerAlias>.<scalarCol> and valued FieldValue(QOV(innerAlias),
	// scalarCol) — so composeFieldOverConstructor folds the scalar reference onto the
	// inner leg with no NULL, whether or not scalarCol is itself dotted (a
	// non-aggregate subquery keeps its table qualifier, "C.NAME"; the dedicated
	// builder re-qualifies it under innerAlias rather than propagating it verbatim as
	// NewAnchoredJoinRecord would). The outer leg carries its derivable columns so the
	// (bare or qualified) outer projections resolve too.
	//
	// Untranslatable when the outer columns are not derivable (only the catalog-free
	// nil-md path — production always passes md): the opaque-seed fallback was RETIRED
	// in RFC-077 7.6, so there is no result value to flow.
	scalarCol := strings.ToUpper(csq.ScalarCol)
	outerCols := t.legColumns(p.Input)
	if outerCols == nil || outerAlias == "" || scalarCol == "" {
		return nil
	}
	resultValue := values.NewScalarSubqueryAnchoredRecord(
		values.AnchoredJoinLeg{Alias: values.NamedCorrelationIdentifier(outerAlias), Columns: outerCols},
		values.NamedCorrelationIdentifier(csq.InnerAlias),
		scalarCol,
	)

	joinSelect := expressions.NewSelectExpressionWithJoinType(
		resultValue,
		[]expressions.Quantifier{outerQ, innerQ},
		nil,
		[]string{outerAlias, csq.InnerAlias},
		expressions.JoinLeftOuter,
	)
	joinRef := expressions.InitialOf(joinSelect)

	projected := make([]values.Value, len(p.Projections))
	innerCorr := values.NamedCorrelationIdentifier(csq.InnerAlias)
	for i, col := range p.Projections {
		if i < len(p.ProjectedValues) && p.ProjectedValues[i] != nil {
			projected[i] = replaceScalarSubqueryRef(p.ProjectedValues[i], csq, innerCorr)
			continue
		}
		if i < len(p.IsComputed) && p.IsComputed[i] {
			return nil
		}
		projected[i] = &values.FieldValue{Field: strings.ToUpper(col), Typ: values.UnknownType}
	}

	projQ := t.namedQuantifier("", joinRef)
	return expressions.NewLogicalProjectionExpressionWithAliases(
		projected,
		p.Aliases,
		projQ,
	)
}

func replaceScalarSubqueryRef(v values.Value, csq logical.CorrelatedScalarSubquery, innerCorr values.CorrelationIdentifier) values.Value {
	return values.Replace(v, func(node values.Value) values.Value {
		if ssq, ok := node.(*values.ScalarSubqueryValue); ok && ssq.Alias == csq.Alias {
			qualifiedName := strings.ToUpper(innerCorr.Name()) + "." + strings.ToUpper(csq.ScalarCol)
			return &values.FieldValue{Field: qualifiedName, Typ: values.UnknownType}
		}
		return node
	})
}

func (t *cascadesTranslator) translateDistinct(d *logical.LogicalDistinct) expressions.RelationalExpression {
	innerRef := t.translateRef(d.Input)
	if innerRef == nil {
		return nil
	}
	return expressions.NewLogicalDistinctExpression(
		t.namedQuantifier(sourceAlias(d.Input), innerRef))
}

// Go extension: Java's fdb-relational 4.11.1.0 does not support GROUP BY;
// its AstNormalizer rejects it with UNSUPPORTED_QUERY before reaching the planner.
func (t *cascadesTranslator) translateAggregate(a *logical.LogicalAggregate) expressions.RelationalExpression {
	if a.Having != "" && a.HavingPredicate == nil {
		return nil
	}
	for _, ssq := range a.HavingScalarSubqueries {
		t.scalarSubqueries = append(t.scalarSubqueries, ScalarSubqueryPlan{
			Alias: ssq.Alias,
			Plan:  ssq.Plan,
		})
	}
	innerRef := t.translateRef(a.Input)
	if innerRef == nil {
		return nil
	}
	groupKeys := make([]values.Value, len(a.GroupKeys))
	for i, key := range a.GroupKeys {
		if i < len(a.GroupKeyValues) && a.GroupKeyValues[i] != nil {
			groupKeys[i] = a.GroupKeyValues[i]
		} else {
			groupKeys[i] = &values.FieldValue{Field: key, Typ: values.UnknownType}
		}
	}
	aggSpecs := make([]expressions.AggregateSpec, 0, len(a.Aggregates))
	for i, aggText := range a.Aggregates {
		spec, ok := parseAggregateText(aggText)
		if !ok {
			return nil
		}
		// The resolved operand (set by upgradeAggregateOperands /
		// buildCorrelatedScalar via resolver.WalkExpression) is the single
		// source of truth. parseAggregateText only reconstructs the operand by
		// re-scanning the slot-name text, and parseOperandValue is a naive
		// left-to-right splitter that mangles nested/parenthesised arithmetic
		// (e.g. "(AMOUNT+10)*2" splits on the inner '+' into garbage atoms),
		// yielding an unresolvable operand that accumulates to NULL and silently
		// drops HAVING groups. Whenever a resolved operand is present, it wins —
		// never the lossy reparse. (A prior `!isArith` guard preferred the
		// reparse for arithmetic operands; that was the operand-routing hole.)
		if i < len(a.AggregateOperands) && a.AggregateOperands[i] != nil {
			spec.Operand = a.AggregateOperands[i]
		}
		if i < len(a.Aliases) && a.Aliases[i] != "" {
			spec.Alias = strings.ToUpper(a.Aliases[i])
		}
		aggSpecs = append(aggSpecs, spec)
	}
	groupBy := expressions.NewGroupByExpression(
		groupKeys,
		aggSpecs,
		t.namedQuantifier(sourceAlias(a.Input), innerRef),
	)
	if a.HavingPredicate == nil {
		return groupBy
	}
	groupByRef := expressions.InitialOf(groupBy)

	// HAVING with EXISTS subqueries is not supported — the correlation
	// references pre-GROUP-BY scope (table columns) but the HAVING
	// evaluates in post-GROUP-BY scope (group keys + aggregates).
	// Java doesn't support this either (no test coverage). Return nil
	// so the planner produces "could not plan query" instead of
	// silently returning wrong results.
	if len(a.HavingExistsSubqueries) > 0 {
		return nil
	}

	return expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{a.HavingPredicate},
		expressions.ForEachQuantifier(groupByRef),
	)
}

func parseAggregateText(text string) (expressions.AggregateSpec, bool) {
	upper := strings.ToUpper(strings.TrimSpace(text))
	lparen := strings.Index(upper, "(")
	if lparen < 0 {
		return expressions.AggregateSpec{}, false
	}
	rparen := strings.LastIndex(upper, ")")
	if rparen < lparen {
		return expressions.AggregateSpec{}, false
	}
	funcName := strings.TrimSpace(upper[:lparen])
	operandText := strings.TrimSpace(upper[lparen+1 : rparen])

	var fn expressions.AggregateFunction
	switch funcName {
	case "COUNT":
		fn = expressions.AggCount
	case "SUM":
		fn = expressions.AggSum
	case "MIN":
		fn = expressions.AggMin
	case "MAX":
		fn = expressions.AggMax
	case "AVG":
		fn = expressions.AggAvg
	default:
		return expressions.AggregateSpec{}, false
	}

	if strings.HasPrefix(operandText, "DISTINCT ") {
		return expressions.AggregateSpec{}, false
	}

	var operand values.Value
	if operandText == "*" {
		operand = &values.ConstantValue{Value: nil, Typ: values.UnknownType}
	} else {
		operand = parseOperandValue(operandText)
	}

	return expressions.AggregateSpec{Function: fn, Operand: operand, OperandName: operandText}, true
}

func parseOperandValue(text string) values.Value {
	for _, op := range []struct {
		sym string
		op  values.ArithmeticOp
	}{
		{"+", values.OpAdd},
		{"-", values.OpSub},
		{"*", values.OpMul},
		{"/", values.OpDiv},
	} {
		idx := strings.Index(text, op.sym)
		if idx > 0 && idx < len(text)-1 {
			left := strings.TrimSpace(text[:idx])
			right := strings.TrimSpace(text[idx+1:])
			if left != "" && right != "" {
				return &values.ArithmeticValue{
					Op:    op.op,
					Left:  parseAtomValue(left),
					Right: parseAtomValue(right),
				}
			}
		}
	}
	return parseAtomValue(text)
}

func parseAtomValue(text string) values.Value {
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return &values.ConstantValue{Value: n, Typ: values.NullableLong}
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		return &values.ConstantValue{Value: f, Typ: values.NullableDouble}
	}
	return &values.FieldValue{Field: text, Typ: values.UnknownType}
}

func (t *cascadesTranslator) translateJoin(j *logical.LogicalJoin) expressions.RelationalExpression {
	// For RIGHT JOIN, swap branches and treat as LEFT JOIN. The NLJ
	// executor iterates the "outer" (left) and for each unmatched row
	// emits NULLs for the inner (right) columns. Swapping makes the
	// originally-right table the outer, which is exactly RIGHT JOIN
	// semantics. This matches the standard approach — Java's Cascades
	// doesn't distinguish RIGHT from LEFT either; the planner
	// normalises RIGHT → LEFT with swapped children.
	left := j.Left
	right := j.Right
	kind := j.Kind
	if kind == logical.JoinRight {
		left, right = right, left
		kind = logical.JoinLeft
	}

	leftRef := t.translateRef(left)
	if leftRef == nil {
		return nil
	}
	rightRef := t.translateRef(right)
	if rightRef == nil {
		return nil
	}
	leftAlias := sourceAlias(left)
	rightAlias := sourceAlias(right)

	// Use named quantifiers so aliases match the predicate QOV
	// correlations created by the SQL resolver.
	leftQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(leftAlias), leftRef)
	rightQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(rightAlias), rightRef)

	var preds []predicates.QueryPredicate
	if j.OnPredicate != nil {
		if qp, ok := j.OnPredicate.(predicates.QueryPredicate); ok {
			preds = []predicates.QueryPredicate{qp}
		}
	}

	var joinType expressions.JoinType
	switch kind {
	case logical.JoinLeft:
		joinType = expressions.JoinLeftOuter
	case logical.JoinFull:
		// FULL OUTER is symmetric — no operand swap (the JoinRight swap
		// above does not fire for JoinFull). The materialized NLJ keeps
		// the original left/right column layout.
		joinType = expressions.JoinFullOuter
	default:
		joinType = expressions.JoinInner
	}

	resultValue := t.buildJoinResultValue(left, right, leftAlias, rightAlias)
	if resultValue == nil {
		// A leg's columns are not derivable (only the catalog-free nil-md path;
		// every md-bearing production query anchors — RFC-077 7.6). Untranslatable.
		return nil
	}
	return expressions.NewSelectExpressionWithJoinType(
		resultValue,
		[]expressions.Quantifier{leftQ, rightQ},
		preds,
		[]string{leftAlias, rightAlias},
		joinType,
	)
}

// translateJoinWithExists builds a flat SelectExpression from a LogicalJoin
// + LogicalFilter that carries EXISTS subqueries. Instead of nesting one
// SelectExpression (the join) inside another (the EXISTS filter), this
// method produces a single SelectExpression with ForEach(left),
// ForEach(right), and Existential quantifiers. The combined predicate
// covers both the join ON and the filter WHERE. The NLJ rule's
// implementJoinWithExistential path handles this 2+1 pattern.
func (t *cascadesTranslator) translateJoinWithExists(
	j *logical.LogicalJoin,
	f *logical.LogicalFilter,
) expressions.RelationalExpression {
	// FULL OUTER cannot be expressed through the join+EXISTS flatten shape
	// (the semi-join cannot carry the FULL drain). The production path
	// rejects this earlier with a clear error (findFullOuterWithExists),
	// but harness callers (plan_harness) invoke the translator directly and
	// bypass that guard — refuse here too so FULL+EXISTS is never silently
	// mistranslated to INNER (the kind switch below defaults to JoinInner).
	if j.Kind == logical.JoinFull {
		return nil
	}
	left := j.Left
	right := j.Right
	kind := j.Kind
	if kind == logical.JoinRight {
		left, right = right, left
		kind = logical.JoinLeft
	}

	// Collect scalar subquery plans from the filter.
	for _, ssq := range f.ScalarSubqueries {
		t.scalarSubqueries = append(t.scalarSubqueries, ScalarSubqueryPlan{
			Alias: ssq.Alias,
			Plan:  ssq.Plan,
		})
	}

	// Flatten join + EXISTS into a single SelectExpression
	// with ForEach(left), ForEach(right), and Existential quantifiers.
	leftRef := t.translateRef(left)
	if leftRef == nil {
		return nil
	}
	rightRef := t.translateRef(right)
	if rightRef == nil {
		return nil
	}

	leftAlias := sourceAlias(left)
	rightAlias := sourceAlias(right)

	leftQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(leftAlias), leftRef)
	rightQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(rightAlias), rightRef)
	quantifiers := []expressions.Quantifier{leftQ, rightQ}

	// Combine join ON predicates + filter WHERE predicates.
	var allPreds []predicates.QueryPredicate
	if j.OnPredicate != nil {
		if qp, ok := j.OnPredicate.(predicates.QueryPredicate); ok {
			allPreds = append(allPreds, qp)
		}
	}
	if f.Predicate != nil {
		if and, ok := f.Predicate.(*predicates.AndPredicate); ok {
			allPreds = append(allPreds, and.SubPredicates...)
		} else {
			allPreds = append(allPreds, f.Predicate)
		}
	}

	// Add EXISTS subqueries as existential quantifiers.
	sourceAliases := []string{leftAlias, rightAlias}
	for _, esq := range f.ExistsSubqueries {
		subRef := t.translateRef(esq.Plan)
		if subRef == nil {
			return nil
		}
		existQ := expressions.NamedExistentialQuantifier(esq.Alias, subRef)
		quantifiers = append(quantifiers, existQ)
		innerCorrName, joinPred := existsInnerCorrelation(esq)
		if joinPred != nil {
			allPreds = append(allPreds, joinPred)
		}
		sourceAliases = append(sourceAliases, innerCorrName)
	}

	var joinType expressions.JoinType
	switch kind {
	case logical.JoinLeft:
		joinType = expressions.JoinLeftOuter
	default:
		joinType = expressions.JoinInner
	}

	resultValue := t.buildJoinResultValue(left, right, leftAlias, rightAlias)
	if resultValue == nil {
		// A leg's columns are not derivable (only the catalog-free nil-md path;
		// every md-bearing production query anchors — RFC-077 7.6). Untranslatable.
		// Mirrors translateJoin: the opaque-seed fallback was retired, so a nil
		// result value must not flow into the SelectExpression (it would nil-deref
		// downstream, e.g. GetCorrelatedToOfValue).
		return nil
	}
	return expressions.NewSelectExpressionWithJoinType(
		resultValue,
		quantifiers,
		allPreds,
		sourceAliases,
		joinType,
	)
}

// splitNonExistsPredicates extracts the non-EXISTS parts of a predicate
// tree. EXISTS predicates (and NOT EXISTS) are dropped — they're
// represented by the Existential quantifier in the SelectExpression.
// Compound AND predicates are flattened: AND(ExistentialValuePredicate, c.id < 10)
// yields just [c.id < 10].
func splitNonExistsPredicates(pred predicates.QueryPredicate) []predicates.QueryPredicate {
	if pred == nil {
		return nil
	}
	if _, ok := predicates.IsExistentialPredicate(pred); ok {
		return nil
	}
	if _, ok := predicates.IsNotExistentialPredicate(pred); ok {
		return nil
	}
	if and, ok := pred.(*predicates.AndPredicate); ok {
		var result []predicates.QueryPredicate
		for _, sub := range and.SubPredicates {
			result = append(result, splitNonExistsPredicates(sub)...)
		}
		return result
	}
	return []predicates.QueryPredicate{pred}
}

// extractExistsPredicates returns the EXISTS-related predicates that
// splitNonExistsPredicates drops: bare ExistentialValuePredicate or
// NOT(ExistentialValuePredicate). The rule's implementExistentialSelect
// needs these to detect EXISTS vs NOT EXISTS.
func extractExistsPredicates(pred predicates.QueryPredicate) []predicates.QueryPredicate {
	if pred == nil {
		return nil
	}
	if _, ok := predicates.IsExistentialPredicate(pred); ok {
		return []predicates.QueryPredicate{pred}
	}
	if _, ok := predicates.IsNotExistentialPredicate(pred); ok {
		return []predicates.QueryPredicate{pred}
	}
	if and, ok := pred.(*predicates.AndPredicate); ok {
		var result []predicates.QueryPredicate
		for _, sub := range and.SubPredicates {
			result = append(result, extractExistsPredicates(sub)...)
		}
		return result
	}
	return nil
}

func (t *cascadesTranslator) namedQuantifier(alias string, ref *expressions.Reference) expressions.Quantifier {
	if alias != "" {
		return expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier(alias), ref)
	}
	return expressions.ForEachQuantifier(ref)
}

// existsInnerCorrelation registers an existential subquery's inner correlation
// under the existential quantifier's UNIQUE alias (esq.Alias, minted by
// values.UniqueCorrelationIdentifier()) rather than the subquery's SOURCE table
// name (sourceAlias(esq.Plan)). It returns:
//
//   - the source alias string to register in the SelectExpression's
//     GetSourceAliases() (the unique alias name), so the NLJ rule derives the
//     existential INNER correlation from it; and
//   - the join predicate with its inner-leg references rebased from the source
//     alias to the unique alias, so the predicate's QOV correlation MATCHES the
//     FlatMap inner binding (the join-pred filter binds under the same alias).
//
// Java gives every existential quantifier its own unique correlation identity;
// the inner correlation predicate references THAT identity, never the source
// table's name. Go's buildCorrelatedExists qualified inner refs under the source
// table name, which COLLIDES with the outer source alias when the subquery scans
// the same table (`... FROM t WHERE id > 1 AND EXISTS (SELECT 1 FROM t ...)`):
// the FlatMap would bind both the outer row and the FirstOrDefault inner under
// the SAME correlation (the inner clobbers the outer → NULL pass-through row),
// and an outer-only predicate (`id > 1`, correlated to the shared name) would be
// misclassified as an INNER join predicate and pushed below the FOD. Routing the
// existential inner through the unique alias makes outer and inner correlations
// distinct by construction, so neither the binding nor the predicate
// classification can collide. The source table's columns still flow up under
// their bare names inside the subquery plan; only the JOIN-LEVEL correlation
// identity changes, so field lookups (bm["COL"]) are unaffected.
func existsInnerCorrelation(esq logical.ExistsSubquery) (string, predicates.QueryPredicate) {
	// The rename is ONLY safe when the inner is a plain single-table scan whose
	// ENTIRE correlation to the parent is captured in esq.JoinPredicate. Two
	// inner shapes carry references to their OWN source alias that the rename
	// cannot reach, so renaming the binding orphans them and the EXISTS goes
	// silently false:
	//
	//   - a JOIN inner emits a MERGED row resolved by qualified leg keys
	//     (T2.ID, T3.T2_ID, …), never a single-alias binding
	//     (executePredicatesFilter: producesMergedRows ⇒ bindAlias=false);
	//     pointing the predicate at a `<uniqueAlias>.*` namespace nothing writes
	//     yields NULL; and
	//   - a NESTED-EXISTS inner (a LogicalFilter carrying its own
	//     ExistsSubqueries) has a nested existential correlation that references
	//     the MIDDLE scan's source alias from INSIDE esq.Plan — not in
	//     esq.JoinPredicate — so the rename leaves it bound to the old alias.
	//
	// Both keep the leg/source-alias routing. The alias-shadow collision the
	// rename fixes only arises for a clean single-table inner (one bare
	// namespace bound under one alias); the merged-row / nested-EXISTS inners
	// route by distinct qualified keys and cannot clobber the outer binding.
	if !existsInnerSafeToRename(esq.Plan) {
		return sourceAlias(esq.Plan), esq.JoinPredicate
	}
	uniqueAlias := esq.Alias
	srcAlias := values.NamedCorrelationIdentifier(sourceAlias(esq.Plan))
	joinPred := esq.JoinPredicate
	if joinPred != nil && srcAlias != uniqueAlias {
		joinPred = predicates.RebasePredicate(joinPred, values.AliasMap{srcAlias: uniqueAlias})
	}
	return uniqueAlias.Name(), joinPred
}

// existsInnerSafeToRename reports whether an existential subquery's plan is a
// clean single-table scan whose only correlation to the parent lives in
// esq.JoinPredicate — the only shape for which renaming the inner correlation to
// the unique existential alias is safe. Returns false for a JOIN (merged-row
// keyed by leg aliases), a CTE/derived-table (its own correlation namespace), or
// a LogicalFilter carrying ExistsSubqueries (a nested EXISTS whose correlation
// references the inner scan's alias from inside the plan). Walks the single-child
// chain the same way sourceAlias does; a plain WHERE filter (no nested EXISTS) is
// transparent.
func existsInnerSafeToRename(op logical.LogicalOperator) bool {
	for cur := op; cur != nil; {
		switch o := cur.(type) {
		case *logical.LogicalScan:
			return true
		case *logical.LogicalJoin:
			return false
		case *logical.LogicalCTE:
			return false
		case *logical.LogicalFilter:
			// A nested EXISTS inside the inner WHERE references the inner scan's
			// own alias from within esq.Plan — the rename can't reach it.
			if len(o.ExistsSubqueries) > 0 {
				return false
			}
			ch := o.Children()
			if len(ch) == 1 {
				cur = ch[0]
				continue
			}
			return false
		default:
			ch := cur.Children()
			if len(ch) == 1 {
				cur = ch[0]
				continue
			}
			return false
		}
	}
	return false
}

func sourceAlias(op logical.LogicalOperator) string {
	for cur := op; cur != nil; {
		switch o := cur.(type) {
		case *logical.LogicalScan:
			if o.Alias != "" {
				return strings.ToUpper(o.Alias)
			}
			return strings.ToUpper(o.Table)
		case *logical.LogicalJoin:
			return sourceAlias(o.Right)
		case *logical.LogicalCTE:
			// CTE-wrapped derived tables: the CTE name IS the
			// derived-table alias. Return it directly so the NLJ
			// executor qualifies merged-row keys under the alias
			// the user specified (e.g. "sq1"), not the underlying
			// table name buried inside the CTE body.
			return strings.ToUpper(o.Name)
		default:
			ch := cur.Children()
			if len(ch) == 1 {
				cur = ch[0]
				continue
			}
			return ""
		}
	}
	return ""
}

func (t *cascadesTranslator) translateCTE(c *logical.LogicalCTE) expressions.RelationalExpression {
	if c.Recursive {
		return t.translateRecursiveCTE(c)
	}
	body := c.Body
	if len(c.ColumnAliases) > 0 {
		if origCols := extractOutputColumns(body); len(origCols) == len(c.ColumnAliases) {
			body = logical.NewProject(body, origCols, c.ColumnAliases)
		}
	}
	t.cteScope[strings.ToUpper(c.Name)] = body
	result := t.translateOp(c.Main)
	delete(t.cteScope, strings.ToUpper(c.Name))
	return result
}

func extractOutputColumns(op logical.LogicalOperator) []string {
	switch o := op.(type) {
	case *logical.LogicalProject:
		return o.Projections
	case *logical.LogicalAggregate:
		var cols []string
		cols = append(cols, o.GroupKeys...)
		for i, agg := range o.Aggregates {
			if i < len(o.Aliases) && o.Aliases[i] != "" {
				cols = append(cols, o.Aliases[i])
			} else {
				cols = append(cols, agg)
			}
		}
		return cols
	case *logical.LogicalDistinct:
		return extractOutputColumns(o.Input)
	case *logical.LogicalSort:
		return extractOutputColumns(o.Input)
	case *logical.LogicalLimit:
		return extractOutputColumns(o.Input)
	case *logical.LogicalFilter:
		return extractOutputColumns(o.Input)
	}
	return nil
}

// translateRecursiveCTE translates a WITH RECURSIVE CTE into a
// RecursiveUnionExpression. Mirrors Java's
// QueryVisitor.handleRecursiveNamedQuery:
//  1. Partition the UNION ALL body into seed (non-recursive) and
//     recursive (self-referencing) branches.
//  2. Translate the seed branch normally.
//  3. Translate the recursive branch with the CTE self-reference
//     resolving to a TempTableScanExpression.
//  4. Wrap both legs in TempTableInsertExpression.
//  5. Create RecursiveUnionExpression with scan/insert aliases.
//  6. Translate the Main query with the CTE name resolving to the
//     RecursiveUnionExpression.
func (t *cascadesTranslator) translateRecursiveCTE(c *logical.LogicalCTE) expressions.RelationalExpression {
	cteName := strings.ToUpper(c.Name)

	// The body must be a UNION ALL or UNION DISTINCT.
	union, ok := c.Body.(*logical.LogicalUnion)
	if !ok || len(union.Inputs) < 2 {
		return nil
	}

	// Partition branches into seed (no self-reference) and recursive
	// (references the CTE name).
	var seedBranches, recursiveBranches []logical.LogicalOperator
	for _, branch := range union.Inputs {
		if logicalOpReferencesCTE(branch, cteName) {
			recursiveBranches = append(recursiveBranches, branch)
		} else {
			seedBranches = append(seedBranches, branch)
		}
	}
	if len(seedBranches) == 0 || len(recursiveBranches) == 0 {
		return nil
	}

	scanAlias := values.NamedCorrelationIdentifier(cteName + "forScan")
	insertAlias := values.NamedCorrelationIdentifier(cteName + "forInsert")

	// Translate the seed leg. Multiple seed branches become a union.
	var seedExpr expressions.RelationalExpression
	if len(seedBranches) == 1 {
		seedExpr = t.translateOp(seedBranches[0])
	} else {
		seedExpr = t.translateUnion(&logical.LogicalUnion{Inputs: seedBranches, Distinct: false})
	}
	if seedExpr == nil {
		return nil
	}

	// Wrap seed in TempTableInsert.
	seedRef := expressions.InitialOf(seedExpr)
	seedInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(seedRef), insertAlias, false)

	// Translate the recursive leg with the CTE self-reference resolving
	// to a TempTableScanExpression(scanAlias). The self-reference temp table
	// carries the seed's ORIGINAL column names (see the normalization note
	// below), so a join leg referencing the CTE inside the recursive branch
	// (e.g. FROM descendants AS a, t AS b) anchors on those columns (RFC-077 7.6).
	t.cteExprScope[cteName] = expressions.NewTempTableScanExpression(scanAlias)
	t.cteColumnsScope[cteName] = fieldsFromColumnNames(extractOuterProjectionColumns(seedBranches[0]))
	var recursiveExpr expressions.RelationalExpression
	if len(recursiveBranches) == 1 {
		recursiveExpr = t.translateOp(recursiveBranches[0])
	} else {
		recursiveExpr = t.translateUnion(&logical.LogicalUnion{Inputs: recursiveBranches, Distinct: false})
	}
	delete(t.cteExprScope, cteName)
	delete(t.cteColumnsScope, cteName)
	if recursiveExpr == nil {
		return nil
	}

	// Normalize the recursive leg's output columns to match the seed's
	// schema. In standard SQL, the CTE's output column names are defined
	// by the seed. The recursive branch often uses qualified column
	// references (e.g. SELECT b.id, b.parent) which produce datum keys
	// like "B.ID" instead of the seed's unqualified "ID". Without this
	// normalization, the outer query (and DFS recursion) can't find the
	// expected columns, yielding NULL for every row.
	//
	// The temp table MUST use the seed's original column names (not the
	// CTE column aliases). The semantic analyzer's ColumnAliasMap
	// reverse-maps aliased references (e.g. `a.up`) back to the
	// original column names (e.g. `A.PARENT`) in the WHERE predicate's
	// FieldValues. So the temp table datum keys must be the originals
	// for the recursive branch's join predicates to match.
	seedCols := extractOuterProjectionColumns(seedBranches[0])
	recCols := extractOuterProjectionColumns(recursiveBranches[0])
	if len(seedCols) > 0 && len(recCols) > 0 && len(seedCols) == len(recCols) {
		// ALWAYS wrap the recursive leg in a normalization projection that reads
		// the body's output columns and re-emits them under the seed's schema
		// column names (the projection's aliases). This is what lets the Go-only
		// PushProjectionBelowJoinRule be removed — it was the only other
		// mechanism that narrowed the recursive body's columns (RFC-042 L1).
		//
		// When the recursive body is a join, its output is the merged
		// source-anchored join RC row carrying QUALIFIED keys (B.ID, A.ID, ...). The
		// load-bearing fix is that we never copy a qualified key into the temp
		// table: each remap value is FieldValue{Field: <bare col>, Child:
		// QOV(<qualifier>)} — evaluateCorrelated reads the qualified datum key
		// ("B.ID") while projectionColumnName returns the BARE field. So the
		// qualified key (which would collide with the NEXT recursion level's
		// same-qualified join side and clobber the live row, stalling the
		// recursion one level early — the exact bug that produced missing
		// deepest descendants) is gone.
		//
		// Emit-key precision: executeProjection stores the value under BOTH the
		// projectionColumnName (the bare body column) AND the alias (the seed
		// name). When the recursive branch projects the same column names as the
		// seed (the common case, e.g. seed `id`, body `b.id` → both "ID"), those
		// coincide and the row has exactly the clean schema column. When they
		// differ (a column rename across the recursive boundary, e.g. seed `n`,
		// body `e.dst`), the bare body name ("DST") is also emitted as an extra
		// key — but it is INERT: it is unqualified, so the next level's temp
		// scan re-qualifies it under the scan alias and the live join side wins
		// the bare key; it cannot clobber the recursion the way a qualified
		// collision did. (A future cleanup could drop the extra key by teaching
		// executeProjection to emit alias-only for an aliased correlated field.)
		remapVals := make([]values.Value, len(recCols))
		for i, rc := range recCols {
			ru := strings.ToUpper(rc)
			var rv values.Value
			if dot := strings.IndexByte(ru, '.'); dot >= 0 {
				qualifier := ru[:dot]
				col := ru[dot+1:]
				rv = &values.FieldValue{
					Field: col,
					Typ:   values.UnknownType,
					Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(qualifier)),
				}
			} else {
				rv = &values.FieldValue{Field: ru, Typ: values.UnknownType}
			}
			remapVals[i] = rv
		}
		remapAliases := make([]string, len(seedCols))
		for i, sc := range seedCols {
			remapAliases[i] = strings.ToUpper(sc)
		}
		recursiveExpr = expressions.NewLogicalProjectionExpressionWithAliases(
			remapVals,
			remapAliases,
			expressions.ForEachQuantifier(expressions.InitialOf(recursiveExpr)),
		)
	}

	// Wrap recursive leg in TempTableInsert.
	recursiveRef := expressions.InitialOf(recursiveExpr)
	recursiveInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(recursiveRef), insertAlias, false)

	// Build RecursiveUnionExpression.
	seedInsertRef := expressions.InitialOf(seedInsert)
	recursiveInsertRef := expressions.InitialOf(recursiveInsert)
	strategy := expressions.TraversalAny
	switch c.TraversalOrder {
	case logical.TraversalPreOrder:
		strategy = expressions.TraversalPreorder
	case logical.TraversalPostOrder:
		strategy = expressions.TraversalPostorder
	}
	var recUnion *expressions.RecursiveUnionExpression
	if union.Distinct {
		recUnion = expressions.NewRecursiveUnionExpressionDistinct(
			expressions.ForEachQuantifier(seedInsertRef),
			expressions.ForEachQuantifier(recursiveInsertRef),
			scanAlias, insertAlias,
			strategy,
		)
	} else {
		recUnion = expressions.NewRecursiveUnionExpression(
			expressions.ForEachQuantifier(seedInsertRef),
			expressions.ForEachQuantifier(recursiveInsertRef),
			scanAlias, insertAlias,
			strategy,
		)
	}

	// Apply CTE column aliases as a rename projection over the
	// recursive union's output. The temp table internally uses the
	// seed's original column names (ID, PARENT) because the semantic
	// analyzer's ColumnAliasMap reverse-maps aliased references
	// (`a.up` → `A.PARENT`) in the recursive branch's predicates.
	// The rename only applies to the outward-facing datum so the
	// main query can reference the aliased names (NODE, UP).
	var cteResult expressions.RelationalExpression = recUnion
	if len(c.ColumnAliases) > 0 && len(seedCols) > 0 && len(seedCols) == len(c.ColumnAliases) {
		needsRename := false
		for i := range seedCols {
			if !strings.EqualFold(seedCols[i], c.ColumnAliases[i]) {
				needsRename = true
				break
			}
		}
		if needsRename {
			renameVals := make([]values.Value, len(seedCols))
			for i, sc := range seedCols {
				renameVals[i] = &values.FieldValue{
					Field: strings.ToUpper(sc),
					Typ:   values.UnknownType,
				}
			}
			renameAliases := make([]string, len(c.ColumnAliases))
			for i, a := range c.ColumnAliases {
				renameAliases[i] = strings.ToUpper(a)
			}
			cteResult = expressions.NewLogicalProjectionExpressionWithAliases(
				renameVals,
				renameAliases,
				expressions.ForEachQuantifier(expressions.InitialOf(recUnion)),
			)
		}
	}

	// Register the (possibly-renamed) result so that the Main query's
	// scan of the CTE name resolves to it. The OUTWARD column schema (the names
	// the Main query references) is the CTE column aliases when present, else the
	// seed's columns — so a CTE reference used as a JOIN LEG in the Main query
	// anchors instead of falling back to the opaque merge (RFC-077 7.6).
	t.cteExprScope[cteName] = cteResult
	outwardCols := extractOuterProjectionColumns(seedBranches[0])
	if len(c.ColumnAliases) > 0 {
		outwardCols = c.ColumnAliases
	}
	t.cteColumnsScope[cteName] = fieldsFromColumnNames(outwardCols)
	result := t.translateOp(c.Main)
	delete(t.cteExprScope, cteName)
	delete(t.cteColumnsScope, cteName)
	return result
}

// fieldsFromColumnNames builds an anchored-RC leg's column schema from a list of
// output column NAMES (upper-cased), typed UnknownType (only names are
// load-bearing for name-based field resolution). Returns nil for an empty list,
// marking the leg's columns as not derivable (RFC-077 7.6).
func fieldsFromColumnNames(names []string) []values.Field {
	if len(names) == 0 {
		return nil
	}
	fields := make([]values.Field, len(names))
	for i, n := range names {
		fields[i] = values.Field{Name: strings.ToUpper(n), FieldType: values.UnknownType, Ordinal: i}
	}
	return fields
}

// extractOuterProjectionColumns returns the column names from the
// outermost LogicalProject in a logical operator tree. Returns nil if
// no LogicalProject is found. Used by translateRecursiveCTE to detect
// schema mismatches between seed and recursive branches.
func extractOuterProjectionColumns(op logical.LogicalOperator) []string {
	if p, ok := op.(*logical.LogicalProject); ok {
		return p.Projections
	}
	return nil
}

// logicalOpReferencesCTE walks a LogicalOperator tree and reports
// whether any LogicalScan references the given CTE name (case-
// insensitive). Used to partition UNION ALL branches into seed vs
// recursive legs.
func logicalOpReferencesCTE(op logical.LogicalOperator, cteName string) bool {
	if op == nil {
		return false
	}
	if scan, ok := op.(*logical.LogicalScan); ok {
		if strings.EqualFold(scan.Table, cteName) {
			return true
		}
	}
	for _, child := range op.Children() {
		if logicalOpReferencesCTE(child, cteName) {
			return true
		}
	}
	return false
}

func (t *cascadesTranslator) translateInsert(ins *logical.LogicalInsert) expressions.RelationalExpression {
	var innerRef *expressions.Reference
	switch {
	case ins.Source != nil:
		// INSERT … SELECT: the source plan produces the rows.
		innerRef = t.translateRef(ins.Source)
		if innerRef == nil {
			return nil
		}
	case ins.ValuesArray != nil:
		// INSERT … VALUES: explode the literal array of records into a
		// stream, matching Java (ExplodeExpression over the array Value).
		explode := expressions.NewExplodeExpression(ins.ValuesArray)
		innerRef = expressions.InitialOf(explode)
	}
	var q expressions.Quantifier
	if innerRef != nil {
		q = expressions.ForEachQuantifier(innerRef)
	}
	return expressions.NewInsertExpression(q, ins.Table, values.UnknownType)
}

func (t *cascadesTranslator) translateUpdate(upd *logical.LogicalUpdate) expressions.RelationalExpression {
	var innerRef *expressions.Reference
	if upd.Input != nil {
		innerRef = t.translateRef(upd.Input)
		if innerRef == nil {
			return nil
		}
	}
	transforms := make([]expressions.UpdateTransform, len(upd.Sets))
	for i, a := range upd.Sets {
		// Prefer the catalog-resolved RHS Value (evaluated per row by the
		// executor); fall back to the canonical text only when the builder
		// ran without catalog resolution (then the executor cannot evaluate
		// it — but this keeps the structure for explain/legacy paths).
		newVal := a.Value
		if newVal == nil {
			newVal = &values.ConstantValue{Value: a.Expr, Typ: values.UnknownType}
		}
		transforms[i] = expressions.UpdateTransform{
			FieldPath: strings.ToUpper(a.Column),
			NewValue:  newVal,
		}
	}
	var q expressions.Quantifier
	if innerRef != nil {
		q = expressions.ForEachQuantifier(innerRef)
	}
	return expressions.NewUpdateExpression(q, upd.Target, transforms)
}

func (t *cascadesTranslator) translateDelete(del *logical.LogicalDelete) expressions.RelationalExpression {
	var innerRef *expressions.Reference
	if del.Input != nil {
		innerRef = t.translateRef(del.Input)
		if innerRef == nil {
			return nil
		}
	}
	var q expressions.Quantifier
	if innerRef != nil {
		q = expressions.ForEachQuantifier(innerRef)
	}
	return expressions.NewDeleteExpression(q, del.Target)
}

// FindUnsupportedFunction walks the logical plan tree and returns the
// name of the first ScalarFunctionValue that isn't in the supported set.
// Returns "" if all functions are supported.
func FindUnsupportedFunction(op logical.LogicalOperator) string {
	if op == nil {
		return ""
	}
	if proj, ok := op.(*logical.LogicalProject); ok {
		for _, v := range proj.ProjectedValues {
			if fn := findUnsafeFuncInValue(v); fn != "" {
				return fn
			}
		}
	}
	if f, ok := op.(*logical.LogicalFilter); ok && f.Predicate != nil {
		if fn := findUnsafeFuncInPredicate(f.Predicate); fn != "" {
			return fn
		}
	}
	if u, ok := op.(*logical.LogicalUpdate); ok {
		// UPDATE SET RHS expressions must reject unsupported functions
		// just like projections, matching the naive path.
		for _, a := range u.Sets {
			if a.Value != nil {
				if fn := findUnsafeFuncInValue(a.Value); fn != "" {
					return fn
				}
			}
		}
	}
	for _, child := range op.Children() {
		if fn := FindUnsupportedFunction(child); fn != "" {
			return fn
		}
	}
	return ""
}

func findUnsafeFuncInValue(v values.Value) string {
	if v == nil {
		return ""
	}
	var found string
	values.WalkValue(v, func(node values.Value) bool {
		if sf, ok := node.(*values.ScalarFunctionValue); ok {
			if !values.IsCascadesSafeScalarFunction(sf.FuncName) {
				found = sf.FuncName
				return false
			}
		}
		return true
	})
	return found
}

func findUnsafeFuncInPredicate(p predicates.QueryPredicate) string {
	var found string
	predicates.WalkPredicate(p, func(qp predicates.QueryPredicate) bool {
		switch pred := qp.(type) {
		case *predicates.ComparisonPredicate:
			if fn := findUnsafeFuncInValue(pred.Operand); fn != "" {
				found = fn
				return false
			}
			if pred.Comparison.Operand != nil {
				if fn := findUnsafeFuncInValue(pred.Comparison.Operand); fn != "" {
					found = fn
					return false
				}
			}
		case *predicates.ValuePredicate:
			if fn := findUnsafeFuncInValue(pred.Value); fn != "" {
				found = fn
				return false
			}
		}
		return true
	})
	return found
}

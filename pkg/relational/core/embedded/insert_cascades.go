package embedded

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/antlr4-go/antlr/v4"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
	"fdb.dev/pkg/relational/core/query/semantic/rlcatalog"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// fieldTypeIsNotNullable reports whether the target field's declared TYPE
// is non-nullable — Java's `fieldType.isNullable()` gate
// (ExpressionVisitor.parseRecordFieldsUnderReorderings:1067). With scalar
// NOT NULL unexpressible, the only non-nullable declarable type is a
// NOT NULL array, stored as a FLAT repeated field (a nullable array is the
// NullableArrayWrapper message instead). proto2 `required` is kept for
// hand-authored/Java-app descriptors, where it is the only non-null signal.
func fieldTypeIsNotNullable(fd protoreflect.FieldDescriptor) bool {
	return fd.Cardinality() == protoreflect.Required || fd.IsList()
}

// buildInsertValuesArray converts an INSERT … VALUES row list into a
// Cascades array literal: one RecordConstructorValue per row, gathered
// into an ArrayConstructorValue. translateInsert wraps this array in an
// ExplodeExpression that streams it as the InsertExpression's inner —
// the same shape Java builds (RecordConstructorValue → array → Explode →
// Insert), so INSERT … VALUES rides the single Cascades path instead of
// the naive execInsert.
//
// Validation (arity, NOT NULL, "expected Record but got Primitive")
// runs here at plan time, matching Java's visitor and the SQLSTATE codes
// the naive execInsert produced. VALUES expressions are constant after
// parameter substitution, so they fold NOW through the ONE evaluator:
// expr.WalkExpressionForProjection lowers each cell to a values.Value
// (Java-typed literals — an int32-range literal is INT, so the INT32
// arithmetic/cast lanes apply exactly as they do on the SELECT path)
// and Evaluate runs the same typed lanes SELECT runs. The legacy
// proto-path evalExpr interpreter was a second, int64-only evaluator:
// it silently widened INT overflow Java rejects (22003) and rejected
// CAST(<int literal> AS BOOLEAN) Java accepts.
//
// Returns (nil, nil) when ins is not a VALUES insert (e.g. INSERT …
// SELECT), leaving Source-based translation in charge.
func (c *EmbeddedConnection) buildInsertValuesArray(
	ins antlrgen.IInsertStatementContext,
	desc protoreflect.MessageDescriptor,
	tableName string,
	md *recordlayer.RecordMetaData,
) (values.Value, error) {
	valCtx, ok := ins.InsertStatementValue().(*antlrgen.InsertStatementValueValuesContext)
	if !ok {
		return nil, nil
	}

	// Resolve column order: explicit list or all fields in descriptor order.
	var explicitCols []string
	if colCtx := ins.UidListWithNestingsInParens(); colCtx != nil {
		for _, uw := range colCtx.UidListWithNestings().AllUidWithNestings() {
			explicitCols = append(explicitCols, functions.NormalizeIdentifier(uw.Uid().GetText()))
		}
	}
	cols := explicitCols
	if cols == nil {
		fds := desc.Fields()
		cols = make([]string, fds.Len())
		for i := 0; i < fds.Len(); i++ {
			cols[i] = string(fds.Get(i).Name())
		}
	}

	// One resolver for the whole statement: an EMPTY scope (a VALUES cell
	// has no FROM — a column reference resolves nowhere and dies 42703)
	// over the schema catalog, lowering each cell with the SAME walk the
	// SELECT projection path uses.
	analyzer := semantic.NewAnalyzer(rlcatalog.Wrap(md), false)
	resolver := expr.New(analyzer, semantic.NewScope(nil))
	// One clock for the whole statement: SQL fixes CURRENT_TIMESTAMP /
	// CURRENT_DATE per statement, so every cell in a multi-row VALUES
	// folds against the same instant (values.StatementClock).
	clock := stmtClock{now: c.statementNow()}

	var rows []values.Value
	for _, rowCtx := range valCtx.AllRecordConstructorForInsert() {
		exprs := rowCtx.AllExpressionWithOptionalName()
		// Arity and column-list semantics follow Java's
		// parseRecordFieldsUnderReorderings (ExpressionVisitor.java:1040-1078):
		//
		//   - explicit list: more VALUES than named columns → 42601 "Too many
		//     parameters" (:1055). The row is then built by iterating the
		//     TARGET fields and looking each up in the named list — a named
		//     column that is NOT a target field is silently ignored, its
		//     value with it (indexOf never finds it; the corpus's
		//     composite-aggregates.yamsql inserts into T2(…, COL3) on a
		//     3-column T2 and Java accepts). A target field named past the
		//     provided values → 42601 "Value of column X is not provided"
		//     (:1064); an unnamed non-nullable target field → 23502
		//     (:1068); an unnamed nullable one gets NULL.
		//   - implicit: the tuple must cover every field → 22000 (:1080-1082).
		if explicitCols != nil {
			if len(exprs) > len(cols) {
				return nil, api.NewError(api.ErrCodeSyntaxError, "Too many parameters")
			}
		} else if len(exprs) != len(cols) {
			return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
				"provided record cannot be assigned as its type is incompatible with the target type")
		}

		type slot struct {
			fd   protoreflect.FieldDescriptor
			expr antlrgen.IExpressionWithOptionalNameContext // nil → NULL fill
		}
		var slots []slot
		if explicitCols != nil {
			fds := desc.Fields()
			for i := 0; i < fds.Len(); i++ {
				fd := fds.Get(i)
				// The explicit column list carries USER identifiers; the
				// descriptor carries STORAGE names (Java keeps both on the
				// field — Type.java:2874-2877). Comparing them raw silently
				// failed to match an escaped column ("a$b" vs A__1B): the
				// name matched nothing, the slot fell through to the
				// NULL-fill arm, and the INSERT SUCCEEDED writing NULL over
				// the value the caller supplied. Match on the user
				// identifier, which is what the SQL text can spell.
				fdUserName := recordlayer.ToUserIdentifier(string(fd.Name()))
				idx := -1
				for j, col := range cols {
					if col == fdUserName || col == string(fd.Name()) {
						idx = j
						break
					}
				}
				switch {
				case idx >= 0 && idx < len(exprs):
					slots = append(slots, slot{fd: fd, expr: exprs[idx]})
				case idx >= len(exprs):
					return nil, api.NewErrorf(api.ErrCodeSyntaxError,
						"Value of column \"%s\" is not provided", fd.Name())
				case fieldTypeIsNotNullable(fd):
					return nil, api.NewErrorf(api.ErrCodeNotNullViolation,
						"null value in column \"%s\" violates not-null constraint", fd.Name())
				default:
					slots = append(slots, slot{fd: fd})
				}
			}
		} else {
			for i, col := range cols {
				fd := desc.Fields().ByName(protoreflect.Name(col))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
						"column %q not found in table %q", col, tableName)
				}
				slots = append(slots, slot{fd: fd, expr: exprs[i]})
			}
		}

		fields := make([]values.RecordConstructorField, 0, len(slots))
		for _, s := range slots {
			fd := s.fd
			col := string(fd.Name())
			if s.expr == nil {
				// Unnamed nullable target field → a TYPED NULL, exactly
				// Java's `new NullValue(fieldType)` (ExpressionVisitor
				// parseRecordFieldsUnderReorderings): the column's type is
				// known from the descriptor, so the NULL states it instead
				// of entering the tree untyped.
				fields = append(fields, values.RecordConstructorField{
					Name:  col,
					Value: values.NewNullValue(query.FieldTypeForFD(fd)),
				})
				continue
			}
			cellExpr := s.expr
			// The pre-scan mirrors the SELECT path's registry gate: a
			// function outside the Cascades-safe set rejects with Java's
			// byte-equal "Unsupported operator <name>" before the walk.
			if fn := findUnsupportedFunctionInParseTree(cellExpr.Expression()); fn != "" {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery, "Unsupported operator "+fn)
			}
			// Severed arms (RFC-145): a scalar subquery or EXISTS inside a
			// VALUES cell keeps its deliberate decline — the scalar-subquery
			// atom is a Go-only grammar extension Java does not parse in this
			// position, and the fold has no SubqueryPlanner. Typed-node scan,
			// message contract pinned by TestFDB_RFC145_SeveredArms_InsertValues.
			if atom := firstSubqueryOrExistsAtom(cellExpr.Expression()); atom != "" {
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
					"%s is not supported in this context", atom)
			}
			cell, walkErr := parseRecordField(fd, cellExpr, resolver)
			if walkErr != nil {
				return nil, walkErr
			}
			val, evalErr := cell.Evaluate(clock)
			if evalErr != nil {
				return nil, translateExecError(evalErr)
			}
			if val == nil && fieldTypeIsNotNullable(fd) {
				return nil, api.NewErrorf(api.ErrCodeNotNullViolation,
					"NULL value in column %q violates NOT NULL constraint", col)
			}
			// Convert + type-check against the target column at plan time —
			// matching Java's visitor, where INSERT type mismatches surface
			// as CANNOT_CONVERT_TYPE (22000) rather than an opaque executor
			// error. ConvertToProtoValue is the authoritative converter
			// (enums by name, nested records, numeric width) that the
			// executor's scalar-only goToProtoValue cannot match, so we
			// carry the resulting protoreflect.Value through and the
			// executor sets it verbatim (buildInsertRecord). NULL stays nil.
			var fieldVal any
			if val != nil {
				pv, convErr := functions.ConvertToProtoValue(fd, val)
				if convErr != nil {
					return nil, convErr
				}
				fieldVal = pv
			}
			// The conversion just proved the value IS of the column's type,
			// so the constant carries that type rather than discarding it.
			fields = append(fields, values.RecordConstructorField{
				Name:  string(fd.Name()),
				Value: &values.ConstantValue{Value: fieldVal, Typ: query.FieldTypeForFD(fd)},
			})
		}

		rows = append(rows, values.NewRecordConstructorValue(fields...))
	}

	// The Explode expression is executable memo input, so its array element
	// cannot use the former Unknown placeholder. Every VALUES row is built in
	// descriptor order and every cell above is stamped with its target field's
	// exact type; state that row type once on the array and verify that no row
	// construction path drifted from it before publishing the Value.
	rowFields := make([]values.Field, desc.Fields().Len())
	for i := range rowFields {
		fd := desc.Fields().Get(i)
		rowFields[i] = values.Field{
			Name:      string(fd.Name()),
			FieldType: query.FieldTypeForFD(fd),
			Ordinal:   i,
		}
	}
	var elementType values.Type = &values.RecordType{Fields: rowFields}
	if len(rows) > 0 {
		rowTypes := make([]values.Type, len(rows))
		for i, row := range rows {
			if row == nil || row.Type() == nil {
				return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
					"INSERT VALUES row %d has no exact type", i)
			}
			rowTypes[i] = row.Type()
		}
		elementType = values.MaximumTypeOfMany(rowTypes...)
		if elementType == nil || values.IsUnresolved(elementType) {
			return nil, api.NewError(api.ErrCodeCannotConvertType,
				"INSERT VALUES rows do not have one exact compatible type")
		}
	}
	return values.NewArrayConstructorValue(elementType, rows), nil
}

// parseRecordField is the target-type push-down: Java's
// ExpressionVisitor.parseRecordField (ExpressionVisitor.java:967-1008),
// which pushes the TARGET field's type as visitor state before visiting the
// cell, so a bare positional tuple acquires the target's names and types
// instead of having to state them.
//
// Go pushes the target as an argument rather than as visitor state because
// the slot mapping that Java performs in parseRecordFieldsUnderReorderings
// already lives here (buildInsertValuesArray's slot loop), holding the
// descriptor the state would have carried. The recursion is Java's:
//
//   - a record constructor against a STRUCT target recurses per field
//     (Java: visitRecordConstructor → parseRecordFieldsUnderReorderings,
//     ExpressionVisitor.java:917-924, :1078-1084);
//   - a record constructor against any other target is the structural type
//     error Java raises when it casts the pushed target type to Type.Record
//     (ExpressionVisitor.java:1047 through Assert.castUnchecked,
//     Assert.java:211-212 — "expected Record but got Primitive");
//   - an array constructor pushes the ELEMENT type before visiting the
//     elements (Java: visitArrayConstructor, ExpressionVisitor.java:929-950),
//     which is what types the structs inside `[(1, 'a'), (2, 'b')]`;
//   - anything else is a scalar cell and takes the ordinary projection walk.
func parseRecordField(
	fd protoreflect.FieldDescriptor,
	cell antlrgen.IExpressionWithOptionalNameContext,
	resolver *expr.Resolver,
) (values.Value, error) {
	atom := unwrappedCellAtom(cell)
	switch a := atom.(type) {
	case *antlrgen.RecordConstructorExpressionAtomContext:
		if !targetIsStruct(fd) {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
				"expected Record but got Primitive")
		}
		return parseStructLiteral(fd.Message(), a.RecordConstructor(), resolver)
	case *antlrgen.ArrayConstructorExpressionAtomContext:
		list, _, isList := values.EffectiveListField(fd)
		if isList && elementIsStruct(list) {
			// Array of STRUCT: the element target types each element, so
			// the bare tuples inside the brackets are record constructors
			// against the element struct rather than the unsupported
			// multi-element shape the projection walker declines.
			return parseStructArrayLiteral(list, a.ArrayConstructor(), resolver)
		}
	}
	v, walkErr := resolver.WalkExpressionForProjection(cell.Expression())
	if walkErr != nil {
		if mapped := mapPredicateWalkError(walkErr); mapped != nil {
			return nil, mapped
		}
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported INSERT VALUES expression: %v", walkErr)
	}
	if err := admitAssignedValue(query.FieldTypeForFD(fd), v); err != nil {
		return nil, err
	}
	return v, nil
}

// admitAssignedValue is the verdict of Java's coercion of a cell to its target
// field (ExpressionVisitor.parseRecordField's coerceIfNecessary over
// coerceValueIfNecessary and PromoteValue.inject, ExpressionVisitor.java:1118-
// 1130, PromoteValue.java:445-455): a NULL takes the field's type
// (NullValue.canResultInType), and anything else must promote to it
// (computePromotionsTrie; an array literal element by element, which is the
// same walk), or the INSERT is refused while planning with INCOMPATIBLE_TYPE.
// So a DOUBLE literal does not narrow into a FLOAT column, nor a LONG literal
// into an INTEGER one: the target writes CAST(… AS FLOAT) for that.
func admitAssignedValue(target values.Type, v values.Value) error {
	if v == nil || isStaticNull(v) {
		return nil
	}
	var incompatible *values.IncompatibleTypeError
	if err := values.CheckPromotionsTrie(target, v.Type()); errors.As(err, &incompatible) {
		return api.WrapError(api.ErrCodeCannotConvertType,
			"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.",
			err)
	}
	return nil
}

// parseStructLiteral builds the typed row constructor for a struct literal
// against its target struct descriptor — Java's
// parseRecordFieldsUnderReorderings positional arm plus the
// RecordConstructorValue.ofColumns it feeds (ExpressionVisitor.java:1078-1084,
// :922-924).
//
// Each field is folded HERE, at the level that holds its descriptor, because
// that is where Java decides whether a NULL is admissible
// (ExpressionVisitor.java:1068). The resulting constructor therefore carries
// already-evaluated constants: a VALUES cell is constant after parameter
// substitution, which is the same property the top-level fold relies on.
func parseStructLiteral(
	md protoreflect.MessageDescriptor,
	rc antlrgen.IRecordConstructorContext,
	resolver *expr.Resolver,
) (values.Value, error) {
	rcc, ok := rc.(*antlrgen.RecordConstructorContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported record constructor context %T", rc)
	}
	if rcc.OfTypeClause() != nil || rcc.Uid() != nil || rcc.STAR() != nil {
		// `(…) of type S`, `S(…)` and `t.*` are projection-side record
		// constructors (Java: visitRecordConstructor's uid/STAR/ofType arms,
		// ExpressionVisitor.java:889-921). They resolve against the query's
		// scope, which a VALUES cell does not have — RFC-204 §4.4 is where
		// the scope-resolving forms live.
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported INSERT VALUES expression: named or star record constructor")
	}
	exprs := rcc.AllExpressionWithOptionalName()
	fds := md.Fields()
	if fds.Len() != len(exprs) {
		return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
			"provided record cannot be assigned as its type is incompatible with the target type")
	}
	fields := make([]values.RecordConstructorField, 0, len(exprs))
	for i, sub := range exprs {
		subFD := fds.Get(i)
		name := string(subFD.Name())
		if ewon, isEwon := sub.(*antlrgen.ExpressionWithOptionalNameContext); isEwon && ewon.Uid() != nil {
			// Java asserts a provided name equals the target field's name
			// (ExpressionVisitor.java:1002-1003) rather than letting the
			// literal rename the target's field.
			given := functions.NormalizeIdentifier(ewon.Uid().GetText())
			if !strings.EqualFold(given, name) {
				return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
					"field %q cannot be assigned to target field %q", given, name)
			}
		}
		v, err := parseRecordField(subFD, sub, resolver)
		if err != nil {
			return nil, err
		}
		fields = append(fields, values.RecordConstructorField{Name: name, Value: v})
	}
	return values.NewRecordConstructorValue(fields...), nil
}

// parseStructArrayLiteral builds the array literal whose ELEMENTS are struct
// literals typed by the array's element descriptor — Java's
// visitArrayConstructor element-type push
// (ExpressionVisitor.java:943-949 → handleArray).
func parseStructArrayLiteral(
	elemFD protoreflect.FieldDescriptor,
	ac antlrgen.IArrayConstructorContext,
	resolver *expr.Resolver,
) (values.Value, error) {
	acc, ok := ac.(*antlrgen.ArrayConstructorContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported array constructor context %T", ac)
	}
	elemType := query.TargetElementType(elemFD)
	if acc.Expressions() == nil {
		// `[]` against a struct array: the empty array of the element type,
		// Java's LightArrayConstructorValue.emptyArray(elementType)
		// (ExpressionVisitor.java:934-937).
		return values.NewArrayConstructorValue(elemType, nil), nil
	}
	elems := acc.Expressions().AllExpression()
	children := make([]values.Value, 0, len(elems))
	for _, e := range elems {
		if pred, isPred := e.(*antlrgen.PredicatedExpressionContext); isPred && pred.Predicate() == nil {
			if rc, isRC := pred.ExpressionAtom().(*antlrgen.RecordConstructorExpressionAtomContext); isRC {
				v, err := parseStructLiteral(elemFD.Message(), rc.RecordConstructor(), resolver)
				if err != nil {
					return nil, err
				}
				children = append(children, v)
				continue
			}
		}
		// A NON-tuple element against a struct element type: walked as an
		// ordinary expression and left for the converter to reject
		// element-wise, which is where Java rejects it too — coercing the
		// array literal coerces each element to the target element type
		// (ExpressionVisitor.coerceValueIfNecessary:1030-1039) and a
		// primitive→record coercion has no physical operator, so
		// SemanticException INCOMPATIBLE_TYPE (PromoteValue.java:370-371)
		// surfaces as the verbatim CANNOT_CONVERT_TYPE. Keeping the element
		// in the array (rather than declining the whole literal here) is what
		// makes a MIXED literal fail on its bad element rather than on shape.
		v, err := resolver.WalkExpressionForProjection(e)
		if err != nil {
			if mapped := mapPredicateWalkError(err); mapped != nil {
				return nil, mapped
			}
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"unsupported INSERT VALUES expression: %v", err)
		}
		children = append(children, v)
	}
	return values.NewArrayConstructorValue(elemType, children), nil
}

// targetIsStruct reports whether the target FIELD is a STRUCT column — a
// message that is neither an array (flat repeated or NullableArrayWrapper)
// nor the tuple_fields.UUID message, both of which are SQL scalars at this
// boundary.
func targetIsStruct(fd protoreflect.FieldDescriptor) bool {
	if _, _, isList := values.EffectiveListField(fd); isList {
		return false
	}
	return elementIsStruct(fd)
}

// elementIsStruct is targetIsStruct for a slot whose repetition the caller
// has already accounted for — an array column's repeated field IS a list,
// so asking targetIsStruct about it would answer about the array rather
// than about its elements.
func elementIsStruct(fd protoreflect.FieldDescriptor) bool {
	if fd == nil || fd.Kind() != protoreflect.MessageKind || fd.IsMap() {
		return false
	}
	msg := fd.Message()
	return msg != nil &&
		string(msg.FullName()) != functions.UUIDProtoMessageName &&
		!values.IsWrappedArrayDescriptor(msg)
}

// unwrappedCellAtom returns the cell's bare expression atom, or nil when the
// cell is anything else (a comparison, a predicate-bearing expression). The
// typed-node unwrap the record/array-literal arms dispatch on.
func unwrappedCellAtom(cell antlrgen.IExpressionWithOptionalNameContext) antlrgen.IExpressionAtomContext {
	pred, ok := cell.Expression().(*antlrgen.PredicatedExpressionContext)
	if !ok || pred.Predicate() != nil {
		return nil
	}
	return pred.ExpressionAtom()
}

// firstSubqueryOrExistsAtom walks an ANTLR expression tree with typed
// nodes and reports the first severed subquery form found: "subquery"
// for a scalar-subquery atom, "EXISTS" for an EXISTS atom, "" when the
// tree has neither. The wording feeds the RFC-145 severed-arm message.
// stmtClock carries the session's statement-stable timestamp into the
// VALUES fold as the values.StatementClock capability.
type stmtClock struct{ now time.Time }

func (c stmtClock) StatementNow() time.Time { return c.now }

func firstSubqueryOrExistsAtom(ctx antlr.Tree) string {
	if ctx == nil {
		return ""
	}
	switch ctx.(type) {
	case *antlrgen.SubqueryExpressionAtomContext:
		return "subquery"
	case *antlrgen.ExistsExpressionAtomContext:
		return "EXISTS"
	}
	for i := 0; i < ctx.GetChildCount(); i++ {
		if found := firstSubqueryOrExistsAtom(ctx.GetChild(i)); found != "" {
			return found
		}
	}
	return ""
}

// isStaticNull reports whether a SET RHS Value is a plan-time-known NULL
// (the NULL literal or a constant that folded to nil).
func isStaticNull(v values.Value) bool {
	switch t := v.(type) {
	case *values.NullValue:
		return true
	case *values.ConstantValue:
		return t.Value == nil
	default:
		return false
	}
}

// resolveUpdateColumn resolves an UPDATE SET column's identifier, once, to the
// target field it names, as Java's SemanticAnalyzer.resolveIdentifier resolves
// it against the target's quantifier, which the table names (name: the bare
// table, or the whole qualified identifier when the statement qualifies it,
// dmlTargetNamePath): the qualified reading first, the leading segments naming
// the target (`t.f`, `t.s.f`, `T.t.f`), and
// only when that names nothing the unqualified one (`f`, `s.f`). In each, the
// first remaining segment is a column and every later one a field of the
// struct before it (lookupNestedField, which descends STRUCT types only, by
// field position). Java compares every name exactly, so both readings are
// tried exactly first; only when neither names a field are they tried again
// with one case-insensitive match per name (values.ResolveAssignmentColumn),
// the over-resolution DIVERGENCES.md declares, which never outranks an exact
// match ("Identifier resolution: Go over-resolves case"), and whose two
// readings both folding to a field are ambiguous. The qualifier itself is never
// folded. A column spelled like its table up to case (`"w".f` in table w) is
// therefore the column, as in Java; the table named twice before a top-level
// column (`t.t.c`) is Java's too (below). Every name is a user identifier, a
// descriptor field's storage name decoded once. It returns the fields' ordinals
// and names; hits is 1 when resolved, 0 when no reading names a field, and more
// than 1 when a step is ambiguous or two readings each name a field.
func resolveUpdateColumn(desc protoreflect.MessageDescriptor, name []string, segments []string) (ordinals []int, names []string, hits int) {
	match := func(candidates []string, name string, fold bool) (int, int) {
		if fold {
			return values.ResolveAssignmentColumn(candidates, name)
		}
		idx, hits := -1, 0
		for i, c := range candidates {
			if c == name {
				idx, hits = i, hits+1
			}
		}
		if hits != 1 {
			return -1, hits
		}
		return idx, 1
	}
	path := func(segments []string, fold bool) ([]int, []string, int) {
		msg := desc
		var ordinals []int
		var names []string
		for i, segment := range segments {
			fields := msg.Fields()
			fieldNames := make([]string, fields.Len())
			for j := range fieldNames {
				fieldNames[j] = values.FieldNameForProtoField(fields.Get(j))
			}
			idx, hits := match(fieldNames, segment, fold)
			if hits != 1 {
				return nil, nil, hits
			}
			ordinals = append(ordinals, idx)
			names = append(names, fieldNames[idx])
			if i < len(segments)-1 {
				fd := fields.Get(idx)
				if !targetIsStruct(fd) {
					return nil, nil, 0
				}
				msg = fd.Message()
			}
		}
		return ordinals, names, 1
	}
	if len(segments) == 0 || desc == nil {
		return nil, nil, 0
	}
	for _, fold := range []bool{false, true} {
		// The qualifier is compared exactly in both passes: it names the
		// target, an alias SQL's scope compares exactly (semantic.Scope), not a
		// descriptor's field, which is all the declared fold repairs.
		var qualified []int
		var qualifiedNames []string
		qualifiedHits := 0
		if len(segments) > len(name) && slices.Equal(segments[:len(name)], name) {
			qualified, qualifiedNames, qualifiedHits = path(segments[len(name):], fold)
		}
		// The table named twice before a top-level column (`t.t.c`) is a
		// qualified reading too: Java's qualified lookup matches an attribute
		// by its name with the operator's name prepended (SemanticAnalyzer
		// lookup, attributeIdentifier.withQualifier(operatorName.getName()),
		// which PREPENDS the name's last segment to the qualifier the
		// attribute already carries, the whole name: `t.t.c`, and `w.T.w.c`
		// for a target named T.w), a top-level column only, never a nested
		// path. It counts beside the reading above, so a
		// struct column named like its table (`x.x.f`, where x has a column
		// f and a struct column x with a field f) is two candidates, 42702, as
		// Java answers (measured).
		if len(segments) == len(name)+2 && segments[0] == name[len(name)-1] && slices.Equal(segments[1:len(name)+1], name) {
			fields := desc.Fields()
			columns := make([]string, fields.Len())
			for j := range columns {
				columns[j] = values.FieldNameForProtoField(fields.Get(j))
			}
			if idx, hits := match(columns, segments[len(name)+1], fold); hits == 1 {
				switch {
				case qualifiedHits == 1 && qualified[0] == idx:
					// The same column by both readings: Java tries an
					// attribute's direct forms before its nested path and
					// stops at the first that matches, so the doubled reading
					// replaces the path through that column (`ss.ss.ss` is the
					// struct column ss, measured).
					qualified, qualifiedNames = []int{idx}, []string{columns[idx]}
				case qualifiedHits == 0:
					qualified, qualifiedNames = []int{idx}, []string{columns[idx]}
					qualifiedHits = 1
				default:
					qualifiedHits++
				}
			} else {
				qualifiedHits += hits
			}
		}
		if qualifiedHits > 1 {
			return nil, nil, qualifiedHits
		}
		if qualifiedHits != 0 && !fold {
			return qualified, qualifiedNames, qualifiedHits
		}
		ordinals, names, hits := path(segments, fold)
		switch {
		case fold && qualifiedHits != 0 && hits != 0:
			// Both readings fold to a field: two candidates, as a fold onto
			// two columns is (DIVERGENCES.md "Identifier resolution: Go
			// over-resolves case"), ambiguous rather than the first reading's.
			return nil, nil, qualifiedHits + hits
		case qualifiedHits != 0:
			return qualified, qualifiedNames, qualifiedHits
		case hits != 0:
			return ordinals, names, hits
		}
	}
	return nil, nil, 0
}

// structSetTargetField returns the descriptor of the field an UPDATE SET
// assigns (its resolved ordinals, resolveUpdateColumn) when that field is a
// STRUCT (or an array of structs) — the only targets whose right-hand side
// needs the type pushed into it. Anything else keeps the ordinary expression
// walk.
func structSetTargetField(rt *recordlayer.RecordType, ordinals []int) protoreflect.FieldDescriptor {
	if rt == nil || rt.Descriptor == nil || len(ordinals) == 0 {
		return nil
	}
	msg := rt.Descriptor
	var fd protoreflect.FieldDescriptor
	for i, ordinal := range ordinals {
		fields := msg.Fields()
		if ordinal < 0 || ordinal >= fields.Len() {
			return nil
		}
		fd = fields.Get(ordinal)
		if i < len(ordinals)-1 {
			if !targetIsStruct(fd) {
				return nil
			}
			msg = fd.Message()
		}
	}
	if targetIsStruct(fd) {
		return fd
	}
	if list, _, ok := values.EffectiveListField(fd); ok && elementIsStruct(list) {
		return fd
	}
	return nil
}

// parseUpdateSetValue types an UPDATE SET right-hand side against its target
// column. The cell arrives as a bare expression rather than the
// name-carrying form an INSERT row uses, so it is adapted to the one
// push-down (parseRecordField) rather than given a second implementation.
func parseUpdateSetValue(
	fd protoreflect.FieldDescriptor,
	e antlrgen.IExpressionContext,
	resolver *expr.Resolver,
) (values.Value, error) {
	pred, isPred := e.(*antlrgen.PredicatedExpressionContext)
	if !isPred || pred.Predicate() != nil {
		return resolver.WalkExpression(e)
	}
	switch a := pred.ExpressionAtom().(type) {
	case *antlrgen.RecordConstructorExpressionAtomContext:
		if !targetIsStruct(fd) {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "expected Record but got Primitive")
		}
		return parseStructLiteral(fd.Message(), a.RecordConstructor(), resolver)
	case *antlrgen.ArrayConstructorExpressionAtomContext:
		if list, _, ok := values.EffectiveListField(fd); ok && elementIsStruct(list) {
			return parseStructArrayLiteral(list, a.ArrayConstructor(), resolver)
		}
	}
	return resolver.WalkExpression(e)
}

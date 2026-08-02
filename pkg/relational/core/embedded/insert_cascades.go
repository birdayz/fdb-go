package embedded

import (
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
	"fdb.dev/pkg/relational/core/query/logical"
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
			explicitCols = append(explicitCols, functions.StripIdentifierQuotes(uw.Uid().GetText()))
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
				idx := -1
				for j, col := range cols {
					if col == string(fd.Name()) {
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

	return values.NewArrayConstructorValue(values.UnknownType, rows), nil
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
	return v, nil
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
			given := functions.StripIdentifierQuotes(ewon.Uid().GetText())
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
		// (ExpressionVisitor.coerceValueIfNecessary:1036-1039) and a
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

func colsContainFold(cols []string, name string) bool {
	for _, c := range cols {
		if strings.EqualFold(c, name) {
			return true
		}
	}
	return false
}

// validateUpdateAssignments enforces NOT NULL on UPDATE SET at plan time
// (matching Java's visitor and the naive execUpdate): assigning a
// statically-NULL value to a NOT NULL column is a NOT_NULL_VIOLATION.
// Runtime NULLs (from a nullable-column RHS) are caught by the record
// store's Required-field marshal at save time.
func validateUpdateAssignments(upd *logical.LogicalUpdate, md *recordlayer.RecordMetaData) error {
	rt := md.GetRecordType(upd.Target)
	if rt == nil {
		return nil
	}
	fields := rt.Descriptor.Fields()
	for _, a := range upd.Sets {
		fd := fields.ByName(protoreflect.Name(a.Column))
		if fd == nil {
			for i := 0; i < fields.Len(); i++ {
				if strings.EqualFold(string(fields.Get(i).Name()), a.Column) {
					fd = fields.Get(i)
					break
				}
			}
		}
		if fd == nil {
			continue
		}
		if fieldTypeIsNotNullable(fd) && isStaticNull(a.Value) {
			return api.NewErrorf(api.ErrCodeNotNullViolation,
				"NULL value in column %q violates NOT NULL constraint", a.Column)
		}
	}
	return nil
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

// structSetTargetField returns the descriptor of an UPDATE SET target column
// when that column is a STRUCT (or an array of structs) — the only targets
// whose right-hand side needs the type pushed into it. Anything else keeps
// the ordinary expression walk.
func structSetTargetField(rt *recordlayer.RecordType, column string) protoreflect.FieldDescriptor {
	if rt == nil || rt.Descriptor == nil {
		return nil
	}
	fields := rt.Descriptor.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if !strings.EqualFold(string(fd.Name()), column) {
			continue
		}
		if targetIsStruct(fd) {
			return fd
		}
		if list, _, ok := values.EffectiveListField(fd); ok && elementIsStruct(list) {
			return fd
		}
		return nil
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

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
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
	"fdb.dev/pkg/relational/core/query/semantic/rlcatalog"
	"google.golang.org/protobuf/reflect/protoreflect"
)

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

	// A NOT NULL column absent from the (explicit) column list gets no value
	// in any row — reject once up front (constant across rows), matching
	// naive execInsert.
	fds := desc.Fields()
	for i := 0; i < fds.Len(); i++ {
		fd := fds.Get(i)
		if fd.Cardinality() == protoreflect.Required && !colsContainFold(cols, string(fd.Name())) {
			return nil, api.NewErrorf(api.ErrCodeNotNullViolation,
				"column %q has NOT NULL constraint but no value was provided", fd.Name())
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
		// Arity mismatch: explicit column list → 42601 SYNTAX_ERROR
		// (the user named the columns); implicit → 22000 the partial
		// tuple can't be coerced to the full row. Matches naive execInsert.
		if len(exprs) != len(cols) {
			if explicitCols != nil {
				return nil, api.NewErrorf(api.ErrCodeSyntaxError,
					"INSERT column list has %d columns but VALUES has %d", len(cols), len(exprs))
			}
			return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
				"provided record cannot be assigned as its type is incompatible with the target type")
		}

		fields := make([]values.RecordConstructorField, 0, len(cols))
		for i, col := range cols {
			fd := desc.Fields().ByName(protoreflect.Name(col))
			if fd == nil {
				return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
					"column %q not found in table %q", col, tableName)
			}
			// A parenthesized record-constructor at a scalar slot —
			// `VALUES (1, (2, 3))` — is a structural type error; Java
			// rejects with byte-equal "expected Record but got Primitive".
			if pred, ok := exprs[i].Expression().(*antlrgen.PredicatedExpressionContext); ok && pred.Predicate() == nil {
				if _, isRC := pred.ExpressionAtom().(*antlrgen.RecordConstructorExpressionAtomContext); isRC {
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
						"expected Record but got Primitive")
				}
			}
			// The pre-scan mirrors the SELECT path's registry gate: a
			// function outside the Cascades-safe set rejects with Java's
			// byte-equal "Unsupported operator <name>" before the walk.
			if fn := findUnsupportedFunctionInParseTree(exprs[i].Expression()); fn != "" {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery, "Unsupported operator "+fn)
			}
			// Severed arms (RFC-145): a scalar subquery or EXISTS inside a
			// VALUES cell keeps its deliberate decline — the scalar-subquery
			// atom is a Go-only grammar extension Java does not parse in this
			// position, and the fold has no SubqueryPlanner. Typed-node scan,
			// message contract pinned by TestFDB_RFC145_SeveredArms_InsertValues.
			if atom := firstSubqueryOrExistsAtom(exprs[i].Expression()); atom != "" {
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
					"%s is not supported in this context", atom)
			}
			cell, walkErr := resolver.WalkExpressionForProjection(exprs[i].Expression())
			if walkErr != nil {
				if mapped := mapPredicateWalkError(walkErr); mapped != nil {
					return nil, mapped
				}
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
					"unsupported INSERT VALUES expression: %v", walkErr)
			}
			val, evalErr := cell.Evaluate(clock)
			if evalErr != nil {
				return nil, translateExecError(evalErr)
			}
			if val == nil && fd.Cardinality() == protoreflect.Required {
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
			fields = append(fields, values.RecordConstructorField{
				Name:  string(fd.Name()),
				Value: &values.ConstantValue{Value: fieldVal, Typ: values.UnknownType},
			})
		}

		rows = append(rows, values.NewRecordConstructorValue(fields...))
	}

	return values.NewArrayConstructorValue(values.UnknownType, rows), nil
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
		if fd.Cardinality() == protoreflect.Required && isStaticNull(a.Value) {
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

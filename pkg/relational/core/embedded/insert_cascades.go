package embedded

import (
	"context"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
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
// parameter substitution, so they are evaluated now via the same
// evalExpr the naive path used; the resulting literals are wrapped as
// ConstantValues, with per-column proto coercion deferred to the
// executor's buildInsertRecord (matching how UPDATE coerces SET values).
//
// Returns (nil, nil) when ins is not a VALUES insert (e.g. INSERT …
// SELECT), leaving Source-based translation in charge.
func (c *EmbeddedConnection) buildInsertValuesArray(
	ctx context.Context,
	ins antlrgen.IInsertStatementContext,
	desc protoreflect.MessageDescriptor,
	tableName string,
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
			val, evalErr := evalExpr(ctx, c, nil, exprs[i].Expression())
			if evalErr != nil {
				return nil, evalErr
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

		// A NOT NULL column absent from the explicit column list gets no
		// value at all — reject, matching naive execInsert.
		fds := desc.Fields()
		for i := 0; i < fds.Len(); i++ {
			fd := fds.Get(i)
			if fd.Cardinality() == protoreflect.Required && !colsContainFold(cols, string(fd.Name())) {
				return nil, api.NewErrorf(api.ErrCodeNotNullViolation,
					"column %q has NOT NULL constraint but no value was provided", fd.Name())
			}
		}

		rows = append(rows, values.NewRecordConstructorValue(fields...))
	}

	return values.NewArrayConstructorValue(values.UnknownType, rows), nil
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

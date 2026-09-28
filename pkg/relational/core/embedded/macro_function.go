package embedded

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
	"fdb.dev/pkg/relational/core/query/semantic/rlcatalog"
	"github.com/antlr4-go/antlr/v4"
	"github.com/google/uuid"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// buildMacroFunction is DdlVisitor.visitSqlInvokedFunction for a macro: each
// parameter a quantified object under a fresh unique alias, the body resolved
// over them, promoted to the declared return type.
func buildMacroFunction(fd antlrgen.ISqlInvokedFunctionContext, body *antlrgen.UserDefinedMacroFunctionStatementBodyContext,
	md *recordlayer.RecordMetaData,
) (*values.MacroFunction, error) {
	spec := fd.FunctionSpecification()
	m := &values.MacroFunction{Name: functions.FullIdToName(spec.GetSchemaQualifiedRoutineName())}
	analyzer := rlcatalog.NewAnalyzer(md, false)
	var names []semantic.Identifier
	var bound []values.Value
	if decls := spec.SqlParameterDeclarationList().SqlParameterDeclarations(); decls != nil {
		for _, d := range decls.AllSqlParameterDeclaration() {
			if d.GetSqlParameterName() == nil {
				return nil, api.NewError(api.ErrCodeUnsupportedOperation, "unnamed parameters not supported")
			}
			if mode := d.ParameterMode(); mode != nil && mode.IN() == nil {
				return nil, api.NewError(api.ErrCodeUnsupportedOperation, "only IN parameters are supported")
			}
			id := semantic.FromUidContext(d.GetSqlParameterName(), false)
			for _, n := range names {
				if n.Name() == id.Name() {
					return nil, api.NewErrorf(api.ErrCodeInvalidFunctionDefinition, "unexpected duplicate parameter(s) %s", id.Name())
				}
			}
			typ, err := functionParameterType(d.GetParameterType(), md)
			if err != nil {
				return nil, err
			}
			// Java draws a random CorrelationIdentifier; one derived from the
			// function and position keeps a build a function of its DDL.
			alias := "c" + strings.ReplaceAll(uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%s/%d", m.Name, len(names)))).String(), "-", "_")
			qov, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(alias), typ)
			if err != nil {
				return nil, err
			}
			var def values.Value
			if d.GetParameterDefault() != nil {
				v, err := expr.New(analyzer, semantic.NewScope(nil)).WalkExpressionForProjection(d.GetParameterDefault())
				if err != nil {
					return nil, err
				}
				if def, err = promoteIfNeeded(v, typ); err != nil {
					return nil, err
				}
			}
			names = append(names, id)
			bound = append(bound, qov)
			m.Params = append(m.Params, qov)
			m.ParamTypes = append(m.ParamTypes, typ)
			m.ParamNames = append(m.ParamNames, id.Name())
			m.Defaults = append(m.Defaults, def)
		}
	}
	r := expr.New(analyzer, semantic.NewScope(nil))
	r.SetMacroParameters(names, bound)
	v, err := r.WalkExpressionForProjection(body.Expression())
	if err != nil {
		return nil, err
	}
	if rc := spec.ReturnsClause(); rc != nil {
		rt, ok := rc.ReturnsType().(*antlrgen.ReturnsTypeContext)
		if !ok || rt.ReturnsTableType() != nil {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation, "table return type is not supported")
		}
		target, err := columnTypeOf(rt.GetReturnsDataType(), rt.ARRAY() != nil, md)
		if err != nil {
			return nil, err
		}
		if v, err = promoteIfNeeded(v, target); err != nil {
			return nil, err
		}
	}
	m.Body = v
	return m, nil
}

// promoteIfNeeded is PromoteValue.inject.
func promoteIfNeeded(v values.Value, target values.Type) (values.Value, error) {
	from := v.Type()
	if from != nil && from.Equals(target) {
		return v, nil
	}
	if !values.IsPromotable(from, target) {
		return nil, api.NewErrorf(api.ErrCodeCannotConvertType, "cannot promote %s to %s", from, target)
	}
	return values.NewPromoteValue(v, target), nil
}

func functionParameterType(ctx antlrgen.IFunctionColumnTypeContext, md *recordlayer.RecordMetaData) (values.Type, error) {
	c := ctx.(*antlrgen.FunctionColumnTypeContext)
	var elem values.Type
	var err error
	if c.GetCustomType() != nil {
		elem, err = customType(functions.NormalizeIdentifier(c.GetCustomType().GetText()), md)
	} else {
		elem, err = primitiveTypeOf(c.PrimitiveType())
	}
	if err != nil || c.ARRAY() == nil {
		return elem, err
	}
	return values.NewArrayType(true, elem), nil
}

func columnTypeOf(ctx antlrgen.IColumnTypeContext, array bool, md *recordlayer.RecordMetaData) (values.Type, error) {
	c := ctx.(*antlrgen.ColumnTypeContext)
	var elem values.Type
	var err error
	if c.GetCustomType() != nil {
		elem, err = customType(functions.NormalizeIdentifier(c.GetCustomType().GetText()), md)
	} else {
		elem, err = primitiveTypeOf(c.PrimitiveType())
	}
	if err != nil || !array {
		return elem, err
	}
	return values.NewArrayType(true, elem), nil
}

func primitiveTypeOf(ctx antlrgen.IPrimitiveTypeContext) (values.Type, error) {
	p := ctx.(*antlrgen.PrimitiveTypeContext)
	code := map[bool]values.TypeCode{
		p.BOOLEAN() != nil: values.TypeCodeBoolean,
		p.INTEGER() != nil: values.TypeCodeInt,
		p.BIGINT() != nil:  values.TypeCodeLong,
		p.FLOAT() != nil:   values.TypeCodeFloat,
		p.DOUBLE() != nil:  values.TypeCodeDouble,
		p.STRING() != nil:  values.TypeCodeString,
		p.BYTES() != nil:   values.TypeCodeBytes,
		p.UUID() != nil:    values.TypeCodeUuid,
	}
	if c, ok := code[true]; ok {
		return values.NewPrimitiveType(c, true), nil
	}
	return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported function parameter type %s", p.GetText())
}

// customType is a declared struct as a nullable record named after it.
func customType(name string, md *recordlayer.RecordMetaData) (values.Type, error) {
	storage, err := recordlayer.ToProtoBufCompliantName(name)
	if err != nil {
		return nil, err
	}
	if md != nil {
		if msg := md.FileDescriptor().Messages().ByName(protoreflect.Name(storage)); msg != nil {
			fields := make([]values.Field, msg.Fields().Len())
			for i := range fields {
				f := msg.Fields().Get(i)
				fields[i] = values.Field{Name: string(f.Name()), FieldType: query.TargetTypeForFD(f), Ordinal: i}
			}
			t := values.NewRecordType(name, true, fields)
			t.StorageName = storage
			return t, nil
		}
	}
	return nil, api.NewErrorf(api.ErrCodeUnknownType, "unknown type %s", name)
}

// unknownScalarFunction is the first bare-name call under n that names
// neither a built-in nor a schema macro: Java refuses it while resolving the
// SELECT list, before any grouping rule.
func unknownScalarFunction(n antlr.Tree, md *recordlayer.RecordMetaData) string {
	if n == nil {
		return ""
	}
	if udf, ok := n.(*antlrgen.UserDefinedScalarFunctionCallContext); ok && udf.UserDefinedScalarFunctionName() != nil {
		name := udf.UserDefinedScalarFunctionName().GetText()
		if !isAllowedFunction(strings.ToUpper(name)) && !hasMacro(md, functions.NormalizeIdentifier(name)) {
			return name
		}
	}
	for i := 0; i < n.GetChildCount(); i++ {
		if fn := unknownScalarFunction(n.GetChild(i), md); fn != "" {
			return fn
		}
	}
	return ""
}

func hasMacro(md *recordlayer.RecordMetaData, name string) bool {
	if md == nil {
		return false
	}
	for _, f := range md.UserDefinedFunctions() {
		if f.GetUserDefinedMacroFunction().GetFunctionName() == name {
			return true
		}
	}
	return false
}

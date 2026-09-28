package embedded

import (
	"database/sql/driver"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/expr"
	"github.com/antlr4-go/antlr/v4"
	"github.com/google/uuid"
)

// bindStatementParameters binds every prepared-statement parameter of tree to
// a constant typed from its driver value, as Java's embedded JDBC does
// (Type.fromObject, MutablePlanGenerationContext.java:473-495): a positional
// `?` takes the next unnamed argument, a named `?x` or `$x` the argument named
// x (repeatable, mixable with positional ones). A missing value is 42F02; an
// unused argument is ignored. It returns the bindings' rendering for the plan
// cache key, and a release that removes the bindings.
func bindStatementParameters(tree antlr.Tree, args []driver.NamedValue) (string, func(), error) {
	var positional []any
	named := map[string]any{}
	for _, a := range args {
		if a.Name != "" {
			named[strings.ToUpper(a.Name)] = a.Value
		} else {
			positional = append(positional, a.Value)
		}
	}
	var bound []antlr.Token
	release := func() {
		for _, tok := range bound {
			expr.UnbindParameter(tok)
		}
	}
	var key strings.Builder
	next := 0
	var walk func(antlr.Tree) error
	walk = func(n antlr.Tree) error {
		// DDL binds nothing; a view refuses its parameters itself.
		if _, ok := n.(*antlrgen.CreateSchemaTemplateStatementContext); ok {
			return nil
		}
		if pp, ok := n.(*antlrgen.PreparedStatementParameterContext); ok {
			var raw any
			switch {
			case pp.NAMED_PARAMETER() != nil:
				name := pp.NAMED_PARAMETER().GetText()[1:]
				v, ok := named[strings.ToUpper(name)]
				if !ok {
					return api.NewErrorf(api.ErrCodeUndefinedParameter, "No value found for parameter %s", name)
				}
				raw = v
			default:
				if next >= len(positional) {
					return api.NewErrorf(api.ErrCodeUndefinedParameter, "No value found for parameter %d", next+1)
				}
				raw = positional[next]
				next++
			}
			v, err := parameterConstant(raw)
			if err != nil {
				return err
			}
			expr.BindParameter(pp.GetStart(), v)
			bound = append(bound, pp.GetStart())
			fmt.Fprintf(&key, "\x00%s=%#v", v.Type(), constantPayload(v))
			return nil
		}
		for i := 0; i < n.GetChildCount(); i++ {
			if err := walk(n.GetChild(i)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(tree); err != nil {
		release()
		return "", func() {}, err
	}
	return key.String(), release, nil
}

func constantPayload(v values.Value) any {
	if c, ok := v.(*values.ConstantValue); ok {
		return c.Value
	}
	return nil
}

// parameterConstant types a driver value as Java's Type.fromObject types a
// JDBC parameter: int32 is INT and int64 LONG (setInt, setLong), a Go int or
// other narrow integer takes an unsuffixed literal's type, float32 is FLOAT,
// float64 DOUBLE, a UUID is UUID, []byte is BYTES, any other slice an ARRAY of
// its element type, and nil an untyped NULL.
func parameterConstant(raw any) (values.Value, error) {
	switch v := raw.(type) {
	case nil:
		return values.NewNullValue(values.NullType), nil
	case uuid.UUID:
		return &values.ConstantValue{Value: [16]byte(v), Typ: values.NotNullUuid}, nil
	case time.Time:
		// DATE and TIMESTAMP are Go-only; a time binds as its canonical text,
		// which the column assignment converts.
		text := functions.FormatTimestamp(v)
		if v.Hour() == 0 && v.Minute() == 0 && v.Second() == 0 && v.Nanosecond() == 0 {
			text = functions.FormatDate(v)
		}
		return &values.ConstantValue{Value: text, Typ: values.NotNullString}, nil
	case []byte:
		return &values.ConstantValue{Value: v, Typ: values.NotNullBytes}, nil
	case driver.Valuer:
		dv, err := v.Value()
		if err != nil {
			return nil, err
		}
		if i, ok := dv.(int64); ok {
			return intParameter(i), nil
		}
		return parameterConstant(dv)
	}
	rv := reflect.ValueOf(raw)
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return values.NewNullValue(values.NullType), nil
		}
		return parameterConstant(rv.Elem().Interface())
	case reflect.Int32:
		return &values.ConstantValue{Value: rv.Int(), Typ: values.NotNullInt}, nil
	case reflect.Int64:
		return &values.ConstantValue{Value: rv.Int(), Typ: values.NotNullLong}, nil
	case reflect.Int, reflect.Int8, reflect.Int16:
		return intParameter(rv.Int()), nil
	case reflect.Uint8, reflect.Uint16, reflect.Uint32:
		return intParameter(int64(rv.Uint())), nil
	case reflect.Uint, reflect.Uint64:
		if rv.Uint() > math.MaxInt64 {
			return nil, api.NewErrorf(api.ErrCodeNumericValueOutOfRange, "parameter value %d is out of range for LONG", rv.Uint())
		}
		return &values.ConstantValue{Value: int64(rv.Uint()), Typ: values.NotNullLong}, nil
	case reflect.Float32:
		return &values.ConstantValue{Value: rv.Float(), Typ: values.NotNullFloat}, nil
	case reflect.Float64:
		return &values.ConstantValue{Value: rv.Float(), Typ: values.NotNullDouble}, nil
	case reflect.String:
		return &values.ConstantValue{Value: rv.String(), Typ: values.NotNullString}, nil
	case reflect.Bool:
		return values.NewBooleanValue(rv.Bool()), nil
	case reflect.Slice, reflect.Array:
		return arrayParameter(rv)
	}
	return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "unsupported parameter type %T", raw)
}

func intParameter(i int64) values.Value {
	if i >= math.MinInt32 && i <= math.MaxInt32 {
		return &values.ConstantValue{Value: i, Typ: values.NotNullInt}
	}
	return &values.ConstantValue{Value: i, Typ: values.NotNullLong}
}

// arrayParameter types a slice as an ARRAY whose element type is the maximum
// of its elements' types. A NULL element is refused, as every ARRAY element is.
func arrayParameter(rv reflect.Value) (values.Value, error) {
	elems := make([]any, rv.Len())
	var types []values.Type
	for i := range elems {
		ev, err := parameterConstant(rv.Index(i).Interface())
		if err != nil {
			return nil, err
		}
		c, ok := ev.(*values.ConstantValue)
		if !ok || c.Value == nil {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation, "An ARRAY value cannot have NULL elements")
		}
		elems[i] = c.Value
		types = append(types, c.Typ)
	}
	var elemType values.Type = values.TypeUnknown
	if len(types) > 0 {
		elemType = values.MaximumTypeOfMany(types...)
		if elemType == nil {
			return nil, api.NewError(api.ErrCodeInvalidParameter, "ARRAY parameter elements have no common type")
		}
		elemType = values.WithNullability(elemType, false)
	}
	return &values.ConstantValue{Value: elems, Typ: values.NewArrayType(false, elemType)}, nil
}

package embedded

import (
	"bytes"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/vectorcodec"
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
			var param string
			switch {
			case pp.NAMED_PARAMETER() != nil:
				name := pp.NAMED_PARAMETER().GetText()[1:]
				param = name
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
				param = strconv.Itoa(next)
			}
			v, err := parameterConstant(raw)
			var badText *invalidUTF8ParameterError
			if errors.As(err, &badText) {
				return api.NewErrorf(api.ErrCodeCharacterNotInRepertoire, "parameter %s is not valid UTF-8", param)
			}
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
	case api.Vector:
		_, payload, stride, ok := vectorcodec.Payload(v)
		if !ok || len(payload)%stride != 0 {
			return nil, api.NewError(api.ErrCodeInvalidParameter, "invalid serialized VECTOR parameter")
		}
		return &values.ConstantValue{Value: bytes.Clone(v), Typ: values.NewVectorType(false, stride*8, len(payload)/stride)}, nil
	case []byte:
		// Copied: the caller may reuse the buffer while the constant lives on in
		// lazily fetched pages.
		return &values.ConstantValue{Value: bytes.Clone(v), Typ: values.NotNullBytes}, nil
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
		if !utf8.ValidString(rv.String()) {
			return nil, &invalidUTF8ParameterError{}
		}
		return &values.ConstantValue{Value: rv.String(), Typ: values.NotNullString}, nil
	case reflect.Bool:
		return values.NewBooleanValue(rv.Bool()), nil
	case reflect.Slice, reflect.Array:
		return arrayParameter(rv)
	}
	return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "unsupported parameter type %T", raw)
}

// invalidUTF8ParameterError marks a string parameter that is not valid UTF-8;
// the binder reports it as 22021 naming the parameter.
type invalidUTF8ParameterError struct{}

func (*invalidUTF8ParameterError) Error() string { return "parameter is not valid UTF-8" }

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
		// A bool binds as a BooleanValue, not a constant.
		if b, isBool := ev.(*values.BooleanValue); isBool && b.Value != nil {
			elems[i] = *b.Value
			types = append(types, values.NotNullBoolean)
			continue
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

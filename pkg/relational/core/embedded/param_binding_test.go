package embedded

import (
	"database/sql/driver"
	"errors"
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/parser"
	"github.com/google/uuid"
)

type textValuer string

func (v textValuer) Value() (driver.Value, error) { return string(v), nil }

func TestParameterConstant_Types(t *testing.T) {
	t.Parallel()
	u := uuid.MustParse("123e4567-e89b-12d3-a456-426614174000")
	one := int64(1)
	for _, c := range []struct {
		name string
		in   any
		code values.TypeCode
	}{
		{"nil", nil, values.TypeCodeNull},
		{"nil pointer", (*int64)(nil), values.TypeCodeNull},
		{"int32", int32(5), values.TypeCodeInt},
		{"int64", int64(5), values.TypeCodeLong},
		{"int fits", 5, values.TypeCodeInt},
		{"int wide", 3000000000, values.TypeCodeLong},
		{"uint64", uint64(5), values.TypeCodeLong},
		{"float32", float32(1.5), values.TypeCodeFloat},
		{"float64", 1.5, values.TypeCodeDouble},
		{"string", "a", values.TypeCodeString},
		{"bool", true, values.TypeCodeBoolean},
		{"bytes", []byte{1}, values.TypeCodeBytes},
		{"uuid", u, values.TypeCodeUuid},
		{"pointer", &one, values.TypeCodeLong},
		{"valuer", textValuer("x"), values.TypeCodeString},
		{"int64 slice", []int64{1, 3}, values.TypeCodeArray},
		{"empty slice", []int64{}, values.TypeCodeArray},
	} {
		v, err := parameterConstant(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := v.Type().Code(); got != c.code {
			t.Errorf("%s: type %v, want %v", c.name, got, c.code)
		}
	}
	// A NaN payload binds bit-exact: nothing is rendered as text.
	nan := math.Float64frombits(0x7ff8000000000abc)
	v, err := parameterConstant(nan)
	if err != nil || math.Float64bits(v.(*values.ConstantValue).Value.(float64)) != 0x7ff8000000000abc {
		t.Fatalf("NaN payload: %v, %v", v, err)
	}
	if _, err := parameterConstant([]*int64{nil}); err == nil {
		t.Fatal("a NULL array element must be refused")
	}
	if _, err := parameterConstant(uint64(math.MaxUint64)); err == nil {
		t.Fatal("a uint64 beyond LONG must be refused")
	}
	if _, err := parameterConstant(struct{}{}); err == nil {
		t.Fatal("an unsupported type must be refused")
	}
}

func TestBindStatementParameters(t *testing.T) {
	t.Parallel()
	root, err := parser.Parse("SELECT id FROM t WHERE id = ? OR id = ?x OR id = $x OR id = ?")
	if err != nil {
		t.Fatal(err)
	}
	args := []driver.NamedValue{
		{Ordinal: 1, Value: int64(1)},
		{Name: "x", Ordinal: 2, Value: int32(2)},
		{Ordinal: 3, Value: "three"},
		{Ordinal: 4, Value: int64(9)}, // unused: ignored
	}
	key, release, err := bindStatementParameters(root, args)
	if err != nil {
		t.Fatal(err)
	}
	release()
	key2, release2, _ := bindStatementParameters(root, []driver.NamedValue{
		{Ordinal: 1, Value: int32(1)}, {Name: "x", Value: int32(2)}, {Ordinal: 3, Value: "three"},
	})
	release2()
	if key == key2 {
		t.Fatal("a LONG and an INT binding share a plan-cache key")
	}
	var apiErr *api.Error
	if _, _, err := bindStatementParameters(root, args[:1]); !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUndefinedParameter {
		t.Fatalf("missing parameter: want 42F02, got %v", err)
	}
}

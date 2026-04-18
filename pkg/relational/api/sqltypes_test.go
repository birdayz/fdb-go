package api

import "testing"

func TestSQLTypeNameFromJDBC(t *testing.T) {
	t.Parallel()
	cases := []struct {
		jdbc int
		want SQLTypeName
	}{
		{JDBCInteger, SQLTypeNameInteger},
		{JDBCBigInt, SQLTypeNameBigInt},
		{JDBCFloat, SQLTypeNameFloat},
		{JDBCDouble, SQLTypeNameDouble},
		{JDBCVarchar, SQLTypeNameString},
		{JDBCStruct, SQLTypeNameStruct},
		{JDBCArray, SQLTypeNameArray},
		{JDBCBinary, SQLTypeNameBinary},
		{JDBCNull, SQLTypeNameNull},
		{JDBCOther, SQLTypeNameOther},
		{JDBCBoolean, SQLTypeNameBoolean},
		{-999, ""},
	}
	for _, c := range cases {
		if got := SQLTypeNameFromJDBC(c.jdbc); got != c.want {
			t.Errorf("SQLTypeNameFromJDBC(%d) = %q, want %q", c.jdbc, got, c.want)
		}
	}
}

func TestJDBCFromSQLTypeName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name SQLTypeName
		want int
	}{
		{SQLTypeNameInteger, JDBCInteger},
		{SQLTypeNameBinary, JDBCBinary},
		{SQLTypeNameBigInt, JDBCBigInt},
		{SQLTypeNameFloat, JDBCFloat},
		{SQLTypeNameDouble, JDBCDouble},
		{SQLTypeNameString, JDBCVarchar},
		{SQLTypeNameStruct, JDBCStruct},
		{SQLTypeNameArray, JDBCArray},
		{SQLTypeNameNull, JDBCNull},
		{SQLTypeNameBoolean, JDBCBoolean},
		{"UNRECOGNIZED", -1},
	}
	for _, c := range cases {
		if got := JDBCFromSQLTypeName(c.name); got != c.want {
			t.Errorf("JDBCFromSQLTypeName(%q) = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestDataTypeFromSQLTypeName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name SQLTypeName
		want DataType
	}{
		{SQLTypeNameInteger, NewIntegerType(false)},
		{SQLTypeNameBinary, NewBytesType(false)},
		{SQLTypeNameBigInt, NewLongType(false)},
		{SQLTypeNameFloat, NewFloatType(false)},
		{SQLTypeNameDouble, NewDoubleType(false)},
		{SQLTypeNameString, NewStringType(false)},
		{SQLTypeNameBoolean, NewBooleanType(false)},
		{SQLTypeNameNull, NewNullType()},
	}
	for _, c := range cases {
		got := DataTypeFromSQLTypeName(c.name)
		if got == nil || !got.Equal(c.want) {
			t.Errorf("DataTypeFromSQLTypeName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
	// Composite types return nil (matches Java).
	for _, n := range []SQLTypeName{SQLTypeNameStruct, SQLTypeNameArray, SQLTypeNameOther, "UNRECOGNIZED"} {
		if got := DataTypeFromSQLTypeName(n); got != nil {
			t.Errorf("DataTypeFromSQLTypeName(%q) = %v, want nil", n, got)
		}
	}
}

// RoundTrip: every name → JDBC → name is stable.
func TestSQLTypeRoundTrip(t *testing.T) {
	t.Parallel()
	allNames := []SQLTypeName{
		SQLTypeNameInteger, SQLTypeNameBigInt, SQLTypeNameFloat,
		SQLTypeNameDouble, SQLTypeNameString, SQLTypeNameStruct,
		SQLTypeNameArray, SQLTypeNameBinary, SQLTypeNameNull,
		SQLTypeNameBoolean,
	}
	for _, n := range allNames {
		code := JDBCFromSQLTypeName(n)
		if code == -1 {
			t.Errorf("name %q has no JDBC mapping", n)
			continue
		}
		back := SQLTypeNameFromJDBC(code)
		if back != n {
			t.Errorf("%q → %d → %q, want %q", n, code, back, n)
		}
	}
}

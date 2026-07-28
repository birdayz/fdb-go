package values

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScalarFunctionCatalogCapabilitySets(t *testing.T) {
	t.Parallel()

	const allNames = `
		ABS BITAND BITOR BITXOR CEIL CEILING CHAR_LENGTH CHARACTER_LENGTH
		COALESCE CONCAT CONCAT_WS CURRENT_DATE CURRENT_TIME CURRENT_TIMESTAMP
		DAY DAYOFMONTH DAYOFWEEK DAYOFYEAR EXP FLOOR GREATEST HOUR IF IFNULL
		IIF LEAST LEFT LEN LENGTH LN LOCALTIME LOG LOWER LTRIM MINUTE MOD MONTH
		NULLIF OCTET_LENGTH PI POSITION POW POWER REPLACE REVERSE RIGHT ROUND
		RTRIM SECOND SIGN SQRT SUBSTR SUBSTRING TRIM UPPER YEAR
	`
	const cascadesSafeNames = `
		ABS BITAND BITOR BITXOR CEIL CEILING CHAR_LENGTH CHARACTER_LENGTH
		COALESCE CONCAT CONCAT_WS CURRENT_DATE CURRENT_TIME CURRENT_TIMESTAMP
		DAY DAYOFMONTH DAYOFWEEK DAYOFYEAR EXP FLOOR GREATEST HOUR IFNULL
		LEAST LEFT LEN LENGTH LN LOCALTIME LOG LOWER LTRIM MINUTE MOD MONTH
		OCTET_LENGTH PI POSITION POW POWER REPLACE REVERSE RIGHT ROUND RTRIM
		SECOND SIGN SQRT SUBSTR SUBSTRING TRIM UPPER YEAR
	`
	const scalarCallNames = `
		ABS CEIL CEILING CHAR_LENGTH CHARACTER_LENGTH COALESCE CONCAT CONCAT_WS
		DAY DAYOFMONTH DAYOFWEEK DAYOFYEAR EXP FLOOR GREATEST HOUR IF IFNULL
		IIF LEAST LEFT LEN LENGTH LN LOG LOWER LTRIM MINUTE MOD MONTH NULLIF
		OCTET_LENGTH PI POSITION POW POWER REPLACE REVERSE RIGHT ROUND RTRIM
		SECOND SIGN SQRT SUBSTR SUBSTRING TRIM UPPER YEAR
	`
	const legacyMapNames = `
		COALESCE DAY DAYOFMONTH DAYOFWEEK DAYOFYEAR GREATEST HOUR LEAST MINUTE
		MONTH SECOND YEAR
	`
	const commonNumericArgumentNames = `GREATEST LEAST MOD`

	require.Equal(t, sortedWords(allNames), catalogNamesMatching(
		func(scalarFunctionDefinition) bool { return true }))
	require.Equal(t, sortedWords(cascadesSafeNames), catalogNamesMatching(
		func(definition scalarFunctionDefinition) bool {
			return definition.cascadesSafe
		}))
	require.Equal(t, sortedWords(scalarCallNames), catalogNamesMatching(
		func(definition scalarFunctionDefinition) bool {
			return definition.scalarCall
		}))
	require.Equal(t, sortedWords(legacyMapNames), catalogNamesMatching(
		func(definition scalarFunctionDefinition) bool {
			return definition.legacyMapFunction != legacyMapScalarFunctionUnsupported
		}))
	require.Equal(t, sortedWords(commonNumericArgumentNames), catalogNamesMatching(
		func(definition scalarFunctionDefinition) bool {
			return definition.argumentStrategy == scalarFunctionCommonNumericArguments
		}))

	require.Len(t, scalarFunctionCatalog, 56)
	require.Len(t, sortedWords(cascadesSafeNames), 53)
	require.Len(t, sortedWords(scalarCallNames), 49)
	require.Len(t, sortedWords(legacyMapNames), 12)
}

func TestScalarFunctionCatalogOperatorCoverageAndAliases(t *testing.T) {
	t.Parallel()

	operators := make(map[scalarFunctionOperator]bool)
	for name, definition := range scalarFunctionCatalog {
		require.NotNil(t, definition.resultType, name)
		operators[definition.operator] = true
	}
	for operator := scalarFunctionUpper; operator <= scalarFunctionStatementTimestamp; operator++ {
		require.True(t, operators[operator], "operator %d has no catalog spelling", operator)
	}

	for _, aliases := range [][]string{
		{"LENGTH", "LEN", "CHAR_LENGTH", "CHARACTER_LENGTH"},
		{"SUBSTRING", "SUBSTR"},
		{"CEIL", "CEILING"},
		{"POWER", "POW"},
		{"IF", "IIF"},
		{"DAY", "DAYOFMONTH"},
		{"CURRENT_TIME", "LOCALTIME", "CURRENT_TIMESTAMP"},
	} {
		first := scalarFunctionCatalog[aliases[0]]
		for _, alias := range aliases[1:] {
			definition := scalarFunctionCatalog[alias]
			require.Equal(t, first.operator, definition.operator, aliases)
			require.Equal(t, first.resultStrategy, definition.resultStrategy, aliases)
			require.Equal(t, first.argumentStrategy, definition.argumentStrategy, aliases)
			require.True(t, first.resultType.Equals(definition.resultType), aliases)
			require.Equal(t, first.cascadesSafe, definition.cascadesSafe, aliases)
			require.Equal(t, first.scalarCall, definition.scalarCall, aliases)
		}
	}
}

func TestScalarFunctionCatalogEveryOperatorDispatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []any
	}{
		{name: "UPPER", args: []any{"x"}},
		{name: "LOWER", args: []any{"X"}},
		{name: "LENGTH", args: []any{"x"}},
		{name: "OCTET_LENGTH", args: []any{"x"}},
		{name: "ABS", args: []any{int64(-1)}},
		{name: "FLOOR", args: []any{2.7}},
		{name: "CEIL", args: []any{2.1}},
		{name: "ROUND", args: []any{2.1}},
		{name: "PI"},
		{name: "SQRT", args: []any{4.0}},
		{name: "POWER", args: []any{int64(2), int64(3)}},
		{name: "COALESCE", args: []any{nil, int64(1)}},
		{name: "NULLIF", args: []any{int64(1), int64(2)}},
		{name: "TRIM", args: []any{" x "}},
		{name: "LTRIM", args: []any{" x"}},
		{name: "RTRIM", args: []any{"x "}},
		{name: "CONCAT", args: []any{"x", "y"}},
		{name: "CONCAT_WS", args: []any{",", "x", "y"}},
		{name: "SUBSTRING", args: []any{"xy", int64(1)}},
		{name: "REPLACE", args: []any{"xy", "x", "z"}},
		{name: "SIGN", args: []any{int64(1)}},
		{name: "MOD", args: []any{int64(3), int64(2)}},
		{name: "IFNULL", args: []any{int64(1), int64(2)}},
		{name: "IF", args: []any{true, int64(1), int64(2)}},
		{name: "GREATEST", args: []any{int64(1), int64(2)}},
		{name: "LEAST", args: []any{int64(1), int64(2)}},
		{name: "EXP", args: []any{0.0}},
		{name: "LN", args: []any{1.0}},
		{name: "LOG", args: []any{10.0}},
		{name: "REVERSE", args: []any{"xy"}},
		{name: "POSITION", args: []any{"x", "xy"}},
		{name: "LEFT", args: []any{"xy", int64(1)}},
		{name: "RIGHT", args: []any{"xy", int64(1)}},
		{name: "BITAND", args: []any{int64(3), int64(1)}},
		{name: "BITOR", args: []any{int64(2), int64(1)}},
		{name: "BITXOR", args: []any{int64(3), int64(1)}},
		{name: "YEAR", args: []any{"2020-01-02"}},
		{name: "CURRENT_DATE"},
		{name: "CURRENT_TIMESTAMP"},
	}

	covered := make(map[scalarFunctionOperator]bool, len(tests))
	for _, test := range tests {
		test := test
		definition := scalarFunctionCatalog[test.name]
		covered[definition.operator] = true
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := evalScalarFunctionCtx(test.name, test.args, nil)
			require.NoError(t, err)
			require.NotNil(t, got)
		})
	}
	for operator := scalarFunctionUpper; operator <= scalarFunctionStatementTimestamp; operator++ {
		require.True(t, covered[operator], "operator %d lacks a dispatch witness", operator)
	}
}

func TestScalarFunctionCatalogResultStrategies(t *testing.T) {
	t.Parallel()

	intValue := &ConstantValue{Value: int64(3), Typ: NotNullInt}
	doubleValue := &ConstantValue{Value: float64(1.5), Typ: NotNullDouble}
	boolValue := &ConstantValue{Value: true, Typ: NotNullBoolean}
	unknownValue := &FieldValue{Field: "mystery", Typ: UnknownType}

	tests := []struct {
		name string
		args []Value
		want Type
	}{
		{name: "UPPER", want: TypeString},
		{name: "COALESCE", args: []Value{intValue, doubleValue}, want: NullableDouble},
		{name: "GREATEST", args: []Value{intValue, doubleValue}, want: NullableDouble},
		{name: "IF", args: []Value{boolValue, intValue, doubleValue}, want: NullableDouble},
		{name: "NULLIF", args: []Value{intValue, doubleValue}, want: NullableInt},
		{name: "ABS", args: []Value{intValue}, want: NotNullInt},
		{name: "MOD", args: []Value{intValue, doubleValue}, want: NullableDouble},
		{name: "COALESCE", args: []Value{unknownValue, doubleValue}, want: UnknownType},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ScalarFunctionResultType(test.name, test.args)
			require.True(t, ok)
			require.Truef(t, got.Equals(test.want), "got %s, want %s", got, test.want)
		})
	}

	for _, name := range []string{"BITAND", "CURRENT_DATE", "not_a_function", "upper"} {
		_, ok := ScalarFunctionResultType(name, nil)
		require.False(t, ok, name)
	}
}

func TestScalarFunctionCatalogRouteBoundaries(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"NULLIF", "IF", "IIF"} {
		require.False(t, IsCascadesSafeScalarFunction(name), name)
		_, scalarCall := ScalarFunctionResultType(name, nil)
		require.True(t, scalarCall, name)
	}
	for _, name := range []string{
		"BITAND", "BITOR", "BITXOR", "CURRENT_DATE",
		"CURRENT_TIME", "LOCALTIME", "CURRENT_TIMESTAMP",
	} {
		require.True(t, IsCascadesSafeScalarFunction(name), name)
		_, scalarCall := ScalarFunctionResultType(name, nil)
		require.False(t, scalarCall, name)
	}
	for name, want := range map[string]Type{
		"BITAND":            NullableLong,
		"BITOR":             NullableLong,
		"BITXOR":            NullableLong,
		"CURRENT_DATE":      NullableDate,
		"CURRENT_TIME":      NullableTimestamp,
		"LOCALTIME":         NullableTimestamp,
		"CURRENT_TIMESTAMP": NullableTimestamp,
	} {
		got, ok := ScalarFunctionDeclaredResultType(name)
		require.True(t, ok, name)
		require.True(t, got.Equals(want), name)
	}
	for _, name := range []string{"IFNULL", "MOD", "UPPER", "CURRENT_DATE", "upper"} {
		_, legacyMapCall := LookupLegacyMapScalarFunction(name)
		require.False(t, legacyMapCall, name)
	}
	for name, want := range map[string]LegacyMapScalarFunction{
		"COALESCE":   LegacyMapScalarFunctionCoalesce,
		"GREATEST":   LegacyMapScalarFunctionGreatest,
		"LEAST":      LegacyMapScalarFunctionLeast,
		"YEAR":       LegacyMapScalarFunctionYear,
		"MONTH":      LegacyMapScalarFunctionMonth,
		"DAY":        LegacyMapScalarFunctionDay,
		"HOUR":       LegacyMapScalarFunctionHour,
		"MINUTE":     LegacyMapScalarFunctionMinute,
		"SECOND":     LegacyMapScalarFunctionSecond,
		"DAYOFMONTH": LegacyMapScalarFunctionDayOfMonth,
		"DAYOFWEEK":  LegacyMapScalarFunctionDayOfWeek,
		"DAYOFYEAR":  LegacyMapScalarFunctionDayOfYear,
	} {
		got, legacyMapCall := LookupLegacyMapScalarFunction(name)
		require.True(t, legacyMapCall, name)
		require.Equal(t, want, got, name)
	}
}

func catalogNamesMatching(
	matches func(scalarFunctionDefinition) bool,
) []string {
	names := make([]string, 0, len(scalarFunctionCatalog))
	for name, definition := range scalarFunctionCatalog {
		if matches(definition) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func sortedWords(input string) []string {
	words := strings.Fields(input)
	sort.Strings(words)
	return words
}

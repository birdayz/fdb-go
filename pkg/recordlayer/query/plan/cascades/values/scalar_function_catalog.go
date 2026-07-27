package values

// scalarFunctionOperator is the evaluator dispatch key stored in the scalar
// function catalog. Aliases share an operator, so adding or removing a spelling
// changes evaluator reachability and planner metadata together.
type scalarFunctionOperator uint8

const (
	scalarFunctionUpper scalarFunctionOperator = iota
	scalarFunctionLower
	scalarFunctionLength
	scalarFunctionOctetLength
	scalarFunctionAbs
	scalarFunctionFloor
	scalarFunctionCeil
	scalarFunctionRound
	scalarFunctionPi
	scalarFunctionSqrt
	scalarFunctionPower
	scalarFunctionCoalesce
	scalarFunctionNullIf
	scalarFunctionTrim
	scalarFunctionLTrim
	scalarFunctionRTrim
	scalarFunctionConcat
	scalarFunctionConcatWS
	scalarFunctionSubstring
	scalarFunctionReplace
	scalarFunctionSign
	scalarFunctionMod
	scalarFunctionIfNull
	scalarFunctionIf
	scalarFunctionGreatest
	scalarFunctionLeast
	scalarFunctionExp
	scalarFunctionLn
	scalarFunctionLog
	scalarFunctionReverse
	scalarFunctionPosition
	scalarFunctionLeft
	scalarFunctionRight
	scalarFunctionBitAnd
	scalarFunctionBitOr
	scalarFunctionBitXor
	scalarFunctionDatePart
	scalarFunctionStatementDate
	scalarFunctionStatementTimestamp
)

type scalarFunctionResultStrategy uint8

const (
	scalarFunctionFixedResult scalarFunctionResultStrategy = iota
	scalarFunctionCommonResult
	scalarFunctionBranchResult
	scalarFunctionFirstArgumentResult
	scalarFunctionNullableFirstArgumentResult
)

type scalarFunctionArgumentStrategy uint8

const (
	scalarFunctionRawArguments scalarFunctionArgumentStrategy = iota
	scalarFunctionCommonNumericArguments
)

// LegacyMapScalarFunction is the compatibility evaluator operation used for
// INFORMATION_SCHEMA map filtering. Its deliberately smaller surface is
// catalogued beside the main Cascades capabilities, while the compatibility
// evaluator retains its distinct argument, carrier, and SQLSTATE semantics.
type LegacyMapScalarFunction uint8

const (
	legacyMapScalarFunctionUnsupported LegacyMapScalarFunction = iota
	LegacyMapScalarFunctionCoalesce
	LegacyMapScalarFunctionGreatest
	LegacyMapScalarFunctionLeast
	LegacyMapScalarFunctionYear
	LegacyMapScalarFunctionMonth
	LegacyMapScalarFunctionDay
	LegacyMapScalarFunctionHour
	LegacyMapScalarFunctionMinute
	LegacyMapScalarFunctionSecond
	LegacyMapScalarFunctionDayOfMonth
	LegacyMapScalarFunctionDayOfWeek
	LegacyMapScalarFunctionDayOfYear
)

type scalarFunctionDefinition struct {
	operator          scalarFunctionOperator
	resultType        Type
	resultStrategy    scalarFunctionResultStrategy
	argumentStrategy  scalarFunctionArgumentStrategy
	cascadesSafe      bool
	scalarCall        bool
	legacyMapFunction LegacyMapScalarFunction
}

func scalarCallFunction(
	operator scalarFunctionOperator,
	resultType Type,
) scalarFunctionDefinition {
	return scalarFunctionDefinition{
		operator:       operator,
		resultType:     resultType,
		resultStrategy: scalarFunctionFixedResult,
		cascadesSafe:   true,
		scalarCall:     true,
	}
}

func polymorphicScalarCall(
	operator scalarFunctionOperator,
	strategy scalarFunctionResultStrategy,
) scalarFunctionDefinition {
	definition := scalarCallFunction(operator, TypeUnknown)
	definition.resultStrategy = strategy
	return definition
}

func internalScalarCall(
	operator scalarFunctionOperator,
	strategy scalarFunctionResultStrategy,
) scalarFunctionDefinition {
	definition := polymorphicScalarCall(operator, strategy)
	definition.cascadesSafe = false
	return definition
}

func routedScalarFunction(
	operator scalarFunctionOperator,
	resultType Type,
) scalarFunctionDefinition {
	return scalarFunctionDefinition{
		operator:       operator,
		resultType:     resultType,
		resultStrategy: scalarFunctionFixedResult,
		cascadesSafe:   true,
	}
}

func legacyMapScalarCall(
	definition scalarFunctionDefinition,
	function LegacyMapScalarFunction,
) scalarFunctionDefinition {
	definition.legacyMapFunction = function
	return definition
}

func commonNumericArguments(
	definition scalarFunctionDefinition,
) scalarFunctionDefinition {
	definition.argumentStrategy = scalarFunctionCommonNumericArguments
	return definition
}

// scalarFunctionCatalog is the single authority for scalar-function names,
// alias families, result-type strategies, and route capabilities.
//
// The capability sets intentionally differ:
//   - NULLIF and IF/IIF can be represented and evaluated but are rejected by
//     the Java-compatibility Cascades admission gate.
//   - BIT* and CURRENT_* enter through dedicated grammar routes rather than
//     ScalarFunctionCall.
//   - the legacy INFORMATION_SCHEMA map evaluator supports only its
//     Java-aligned subset and retains separate evaluation semantics.
var scalarFunctionCatalog = map[string]scalarFunctionDefinition{
	// String functions.
	"UPPER":            scalarCallFunction(scalarFunctionUpper, TypeString),
	"LOWER":            scalarCallFunction(scalarFunctionLower, TypeString),
	"LENGTH":           scalarCallFunction(scalarFunctionLength, TypeInt),
	"LEN":              scalarCallFunction(scalarFunctionLength, TypeInt),
	"CHAR_LENGTH":      scalarCallFunction(scalarFunctionLength, TypeInt),
	"CHARACTER_LENGTH": scalarCallFunction(scalarFunctionLength, TypeInt),
	"OCTET_LENGTH":     scalarCallFunction(scalarFunctionOctetLength, TypeInt),
	"SUBSTRING":        scalarCallFunction(scalarFunctionSubstring, TypeString),
	"SUBSTR":           scalarCallFunction(scalarFunctionSubstring, TypeString),
	"TRIM":             scalarCallFunction(scalarFunctionTrim, TypeString),
	"LTRIM":            scalarCallFunction(scalarFunctionLTrim, TypeString),
	"RTRIM":            scalarCallFunction(scalarFunctionRTrim, TypeString),
	"CONCAT":           scalarCallFunction(scalarFunctionConcat, TypeString),
	"CONCAT_WS":        scalarCallFunction(scalarFunctionConcatWS, TypeString),
	"REPLACE":          scalarCallFunction(scalarFunctionReplace, TypeString),
	"LEFT":             scalarCallFunction(scalarFunctionLeft, TypeString),
	"RIGHT":            scalarCallFunction(scalarFunctionRight, TypeString),
	"POSITION":         scalarCallFunction(scalarFunctionPosition, TypeInt),
	"REVERSE":          scalarCallFunction(scalarFunctionReverse, TypeString),

	// Math functions.
	"ABS": polymorphicScalarCall(
		scalarFunctionAbs, scalarFunctionFirstArgumentResult),
	"MOD": commonNumericArguments(polymorphicScalarCall(
		scalarFunctionMod, scalarFunctionCommonResult)),
	"FLOOR": polymorphicScalarCall(
		scalarFunctionFloor, scalarFunctionFirstArgumentResult),
	"CEIL": polymorphicScalarCall(
		scalarFunctionCeil, scalarFunctionFirstArgumentResult),
	"CEILING": polymorphicScalarCall(
		scalarFunctionCeil, scalarFunctionFirstArgumentResult),
	"ROUND": polymorphicScalarCall(
		scalarFunctionRound, scalarFunctionFirstArgumentResult),
	"SQRT":  scalarCallFunction(scalarFunctionSqrt, NullableDouble),
	"POWER": scalarCallFunction(scalarFunctionPower, NullableDouble),
	"POW":   scalarCallFunction(scalarFunctionPower, NullableDouble),
	"SIGN": polymorphicScalarCall(
		scalarFunctionSign, scalarFunctionFirstArgumentResult),
	"PI":  scalarCallFunction(scalarFunctionPi, NullableDouble),
	"EXP": scalarCallFunction(scalarFunctionExp, NullableDouble),
	"LN":  scalarCallFunction(scalarFunctionLn, NullableDouble),
	"LOG": scalarCallFunction(scalarFunctionLog, NullableDouble),

	// Null/comparison helpers.
	"COALESCE": legacyMapScalarCall(
		polymorphicScalarCall(scalarFunctionCoalesce, scalarFunctionCommonResult),
		LegacyMapScalarFunctionCoalesce),
	"IFNULL": polymorphicScalarCall(
		scalarFunctionIfNull, scalarFunctionCommonResult),
	"GREATEST": legacyMapScalarCall(commonNumericArguments(
		polymorphicScalarCall(scalarFunctionGreatest, scalarFunctionCommonResult)),
		LegacyMapScalarFunctionGreatest),
	"LEAST": legacyMapScalarCall(commonNumericArguments(
		polymorphicScalarCall(scalarFunctionLeast, scalarFunctionCommonResult)),
		LegacyMapScalarFunctionLeast),
	"NULLIF": internalScalarCall(
		scalarFunctionNullIf, scalarFunctionNullableFirstArgumentResult),
	"IF": internalScalarCall(
		scalarFunctionIf, scalarFunctionBranchResult),
	"IIF": internalScalarCall(
		scalarFunctionIf, scalarFunctionBranchResult),

	// Bit operators lower directly from BitExpressionAtom.
	"BITAND": routedScalarFunction(scalarFunctionBitAnd, TypeInt),
	"BITOR":  routedScalarFunction(scalarFunctionBitOr, TypeInt),
	"BITXOR": routedScalarFunction(scalarFunctionBitXor, TypeInt),

	// Date/time functions.
	"YEAR": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, TypeInt),
		LegacyMapScalarFunctionYear),
	"MONTH": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, TypeInt),
		LegacyMapScalarFunctionMonth),
	"DAY": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, TypeInt),
		LegacyMapScalarFunctionDay),
	"DAYOFMONTH": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, TypeInt),
		LegacyMapScalarFunctionDayOfMonth),
	"HOUR": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, TypeInt),
		LegacyMapScalarFunctionHour),
	"MINUTE": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, TypeInt),
		LegacyMapScalarFunctionMinute),
	"SECOND": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, TypeInt),
		LegacyMapScalarFunctionSecond),
	"DAYOFWEEK": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, TypeInt),
		LegacyMapScalarFunctionDayOfWeek),
	"DAYOFYEAR": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, TypeInt),
		LegacyMapScalarFunctionDayOfYear),
	"CURRENT_DATE": routedScalarFunction(
		scalarFunctionStatementDate, NullableDate),
	"CURRENT_TIME": routedScalarFunction(
		scalarFunctionStatementTimestamp, NullableTimestamp),
	"LOCALTIME": routedScalarFunction(
		scalarFunctionStatementTimestamp, NullableTimestamp),
	"CURRENT_TIMESTAMP": routedScalarFunction(
		scalarFunctionStatementTimestamp, NullableTimestamp),
}

func scalarFunctionDefinitionFor(
	name string,
) (scalarFunctionDefinition, bool) {
	definition, ok := scalarFunctionCatalog[name]
	return definition, ok
}

// ScalarFunctionDeclaredResultType returns the catalogued base result type for
// any route, including dedicated BIT* and CURRENT_* grammar forms. Generic
// polymorphic calls return TypeUnknown here and use ScalarFunctionResultType
// with their arguments for concrete inference.
func ScalarFunctionDeclaredResultType(name string) (Type, bool) {
	definition, ok := scalarFunctionDefinitionFor(name)
	if !ok {
		return TypeUnknown, false
	}
	return definition.resultType, true
}

// ScalarFunctionResultType resolves the result type for a name admitted by the
// generic ScalarFunctionCall grammar route. Dedicated BIT* and CURRENT_* routes
// are intentionally excluded even though their definitions live in the same
// catalog.
func ScalarFunctionResultType(name string, args []Value) (Type, bool) {
	definition, ok := scalarFunctionDefinitionFor(name)
	if !ok || !definition.scalarCall {
		return TypeUnknown, false
	}

	switch definition.resultStrategy {
	case scalarFunctionFixedResult:
		return definition.resultType, true
	case scalarFunctionCommonResult:
		return concreteScalarFunctionType(CommonValueType(args)), true
	case scalarFunctionBranchResult:
		if len(args) < 2 {
			return TypeUnknown, true
		}
		return concreteScalarFunctionType(CommonValueType(args[1:])), true
	case scalarFunctionFirstArgumentResult:
		return firstScalarFunctionArgumentType(args, false), true
	case scalarFunctionNullableFirstArgumentResult:
		return firstScalarFunctionArgumentType(args, true), true
	default:
		return TypeUnknown, true
	}
}

func concreteScalarFunctionType(typ Type) Type {
	if typ == nil || typ.Code() == TypeCodeUnknown {
		return TypeUnknown
	}
	return typ
}

func firstScalarFunctionArgumentType(args []Value, nullable bool) Type {
	if len(args) == 0 || args[0] == nil {
		return TypeUnknown
	}
	typ := concreteScalarFunctionType(args[0].Type())
	if typ.Code() == TypeCodeUnknown {
		return typ
	}
	if nullable {
		return WithNullability(typ, true)
	}
	return typ
}

// CommonValueType computes the common supertype of value branches and makes it
// nullable. Literal NULL branches carry no constraint; a non-NULL branch with
// unknown type keeps the result unknown.
func CommonValueType(branches []Value) Type {
	types := make([]Type, 0, len(branches))
	for _, branch := range branches {
		if branch == nil {
			continue
		}
		if _, isNull := branch.(*NullValue); isNull {
			continue
		}
		typ := branch.Type()
		if typ == nil || typ.Code() == TypeCodeNull {
			continue
		}
		if typ.Code() == TypeCodeUnknown {
			return UnknownType
		}
		types = append(types, typ)
	}
	if len(types) == 0 {
		return UnknownType
	}
	typ := MaximumTypeOfMany(types...)
	if typ == nil {
		return UnknownType
	}
	return WithNullability(typ, true)
}

// LookupLegacyMapScalarFunction returns the operation admitted by the legacy
// INFORMATION_SCHEMA map evaluator without widening that evaluator's surface.
func LookupLegacyMapScalarFunction(name string) (LegacyMapScalarFunction, bool) {
	definition, ok := scalarFunctionDefinitionFor(name)
	if !ok || definition.legacyMapFunction == legacyMapScalarFunctionUnsupported {
		return legacyMapScalarFunctionUnsupported, false
	}
	return definition.legacyMapFunction, true
}

// IsCascadesSafeScalarFunction reports whether the named scalar function is
// admitted to the Cascades SQL pipeline.
func IsCascadesSafeScalarFunction(name string) bool {
	definition, ok := scalarFunctionDefinitionFor(name)
	return ok && definition.cascadesSafe
}

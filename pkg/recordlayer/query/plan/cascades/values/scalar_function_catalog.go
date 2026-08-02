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
	scalarFunctionBitmapBucketOffset
	scalarFunctionBitmapBitPosition
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
	// physicalOperatorTypes is the set of RESULT type codes for which a
	// physical implementation of this function exists. Empty means
	// unrestricted.
	//
	// This is Java's operator map, not a hand-written reject list. Java
	// registers one PhysicalOperator per (comparison function, type code)
	// pair and looks the pair up during encapsulation; a type code with no
	// registered operator has no implementation, and Java raises
	// FUNCTION_UNDEFINED_FOR_GIVEN_ARGUMENT_TYPES rather than planning a call
	// it cannot run (VariadicFunctionValue.java:208-212). Modelling the
	// SUPPORTED set the same way means a type the engine later learns to
	// compare is enabled by registering it here, exactly as adding a
	// PhysicalOperator enables it in Java.
	physicalOperatorTypes []TypeCode
}

// comparisonPhysicalOperatorTypes is Java's PhysicalOperator enum for
// GREATEST/LEAST, which registers exactly these six result type codes
// (VariadicFunctionValue.java, GREATEST_INT/LONG/BOOLEAN/STRING/FLOAT/DOUBLE
// and the LEAST mirror). Notably absent: RECORD, ARRAY, BYTES, UUID and the
// temporal codes — `least(struct_col, struct_col)` has no operator and is a
// 22F00, not a runtime type error.
var comparisonPhysicalOperatorTypes = []TypeCode{
	TypeCodeInt, TypeCodeLong, TypeCodeBoolean,
	TypeCodeString, TypeCodeFloat, TypeCodeDouble,
}

// withPhysicalOperatorTypes restricts a definition to the result type codes
// that have a physical implementation.
func withPhysicalOperatorTypes(
	definition scalarFunctionDefinition,
	codes []TypeCode,
) scalarFunctionDefinition {
	definition.physicalOperatorTypes = codes
	return definition
}

// ScalarFunctionArgumentDiagnosis is the outcome of Java's two-step argument
// admission for a function that declares a physical-operator map.
type ScalarFunctionArgumentDiagnosis int

const (
	// ScalarFunctionArgumentsOK — the call is admissible.
	ScalarFunctionArgumentsOK ScalarFunctionArgumentDiagnosis = iota
	// ScalarFunctionArgumentsIncompatible — the argument types have no common
	// type. Java's SemanticException INCOMPATIBLE_TYPE; the caller raises
	// 22000.
	ScalarFunctionArgumentsIncompatible
	// ScalarFunctionArgumentsNoOperator — the arguments agree on a type, but
	// no physical operator implements the function for it. Java's
	// FUNCTION_UNDEFINED_FOR_GIVEN_ARGUMENT_TYPES; the caller raises 22F00.
	ScalarFunctionArgumentsNoOperator
)

// DiagnoseScalarFunctionArguments runs Java's argument admission for a
// function that declares an operator map, and reports WHICH of the two
// rejections applies — they carry different SQLSTATEs and the corpus asserts
// both, so collapsing them into one "bad arguments" answer would be wrong half
// the time.
//
// Java (VariadicFunctionValue.encapsulate, :190-212):
//
//  1. fold the argument types with Type.maximumType; a null fold is
//     INCOMPATIBLE_TYPE → 22000.
//  2. look up a PhysicalOperator for the folded type; a miss is
//     FUNCTION_UNDEFINED_FOR_GIVEN_ARGUMENT_TYPES → 22F00.
//
// Both steps are decided here rather than deferring step 1 to the existing
// runtime path, because Go's CommonValueType is MORE PERMISSIVE than Java's
// maximumType: it folds (BYTES, STRING) to BYTES, so
// `greatest(bytes_col, 'a')` would otherwise plan and return a value where
// Java rejects the query outright. Folding with maximumTypeCode here is what
// makes the rejection happen at all.
//
// This does not touch CommonValueType itself, which serves COALESCE, IFNULL
// and IF as well; those follow Java's own (different) admission and are not in
// this function's scope.
func DiagnoseScalarFunctionArguments(name string, args []Value) ScalarFunctionArgumentDiagnosis {
	definition, ok := scalarFunctionDefinitionFor(name)
	if !ok || len(definition.physicalOperatorTypes) == 0 {
		return ScalarFunctionArgumentsOK
	}
	folded, state := foldArgumentTypeCodes(args)
	switch state {
	case argumentFoldUnresolved:
		return ScalarFunctionArgumentsOK
	case argumentFoldIncompatible:
		return ScalarFunctionArgumentsIncompatible
	}
	for _, c := range definition.physicalOperatorTypes {
		if c == folded {
			return ScalarFunctionArgumentsOK
		}
	}
	return ScalarFunctionArgumentsNoOperator
}

type argumentFoldState int

const (
	argumentFoldOK argumentFoldState = iota
	argumentFoldIncompatible
	argumentFoldUnresolved
)

// foldArgumentTypeCodes folds the argument type codes left to right, the way
// Java's encapsulate folds them with Type.maximumType.
//
// A NULL literal is skipped rather than folded: Java's maximumType absorbs it
// into the other operand's concrete type (marking the result nullable), so
// `least(x, null)` is typed by x alone.
func foldArgumentTypeCodes(args []Value) (TypeCode, argumentFoldState) {
	var folded TypeCode
	found := false
	for _, a := range args {
		if a == nil {
			return folded, argumentFoldUnresolved
		}
		t := a.Type()
		if t == nil {
			return folded, argumentFoldUnresolved
		}
		code := t.Code()
		if code == TypeCodeNull {
			continue
		}
		if code == TypeCodeUnknown {
			return folded, argumentFoldUnresolved
		}
		if !found {
			folded, found = code, true
			continue
		}
		next, ok := maximumTypeCode(folded, code)
		if !ok {
			return folded, argumentFoldIncompatible
		}
		folded = next
	}
	if !found {
		return folded, argumentFoldUnresolved
	}
	return folded, argumentFoldOK
}

// numericPromotionRank orders the numeric codes by Java's widening lattice.
// rank 0 means "not numeric".
func numericPromotionRank(code TypeCode) int {
	switch code {
	case TypeCodeInt:
		return 1
	case TypeCodeLong:
		return 2
	case TypeCodeFloat:
		return 3
	case TypeCodeDouble:
		return 4
	}
	return 0
}

// maximumTypeCode is Type.maximumType reduced to type CODES: identical codes
// fold to themselves, two numeric codes fold to the wider, and every other
// pairing has no maximum (Java returns null, which encapsulate turns into
// INCOMPATIBLE_TYPE). That last clause is the one that matters here — it is
// why `greatest(bytes_col, 'a')` is an error rather than a BYTES comparison.
func maximumTypeCode(a, b TypeCode) (TypeCode, bool) {
	if a == b {
		return a, true
	}
	ra, rb := numericPromotionRank(a), numericPromotionRank(b)
	if ra > 0 && rb > 0 {
		if ra >= rb {
			return a, true
		}
		return b, true
	}
	return a, false
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
//
// Integer-returning entries below are NullableLong (64-bit), not 32-bit INT,
// and that is deliberate. SQL nominally types LENGTH/POSITION/DATE_PART as
// INTEGER, so this looks wrong at a glance — it was previously written as the
// `TypeInt` alias, which read as INT and WAS NullableLong, hiding the question
// entirely.
//
// Java defines none of these functions (fdb-relational ships a deliberately
// small scalar subset: COALESCE/GREATEST/LEAST/date-part), so there is no port
// to match and CockroachDB is the reference for Go-only extensions. CRDB's
// `length` returns `types.Int`, and CRDB's `types.Int` is Width: 64
// (pkg/sql/types/types.go — int8 OID; its 32-bit type is a separate `Int4`).
// So 64-bit is exactly what the reference engine returns, and narrowing these
// to INT would diverge from it while gaining nothing.
var scalarFunctionCatalog = map[string]scalarFunctionDefinition{
	// String functions.
	"UPPER":            scalarCallFunction(scalarFunctionUpper, TypeString),
	"LOWER":            scalarCallFunction(scalarFunctionLower, TypeString),
	"LENGTH":           scalarCallFunction(scalarFunctionLength, NullableLong),
	"LEN":              scalarCallFunction(scalarFunctionLength, NullableLong),
	"CHAR_LENGTH":      scalarCallFunction(scalarFunctionLength, NullableLong),
	"CHARACTER_LENGTH": scalarCallFunction(scalarFunctionLength, NullableLong),
	"OCTET_LENGTH":     scalarCallFunction(scalarFunctionOctetLength, NullableLong),
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
	"POSITION":         scalarCallFunction(scalarFunctionPosition, NullableLong),
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
	"GREATEST": legacyMapScalarCall(withPhysicalOperatorTypes(commonNumericArguments(
		polymorphicScalarCall(scalarFunctionGreatest, scalarFunctionCommonResult)),
		comparisonPhysicalOperatorTypes),
		LegacyMapScalarFunctionGreatest),
	"LEAST": legacyMapScalarCall(withPhysicalOperatorTypes(commonNumericArguments(
		polymorphicScalarCall(scalarFunctionLeast, scalarFunctionCommonResult)),
		comparisonPhysicalOperatorTypes),
		LegacyMapScalarFunctionLeast),
	"NULLIF": internalScalarCall(
		scalarFunctionNullIf, scalarFunctionNullableFirstArgumentResult),
	"IF": internalScalarCall(
		scalarFunctionIf, scalarFunctionBranchResult),
	"IIF": internalScalarCall(
		scalarFunctionIf, scalarFunctionBranchResult),

	// Bit operators lower directly from BitExpressionAtom.
	"BITAND": routedScalarFunction(scalarFunctionBitAnd, NullableLong),
	"BITOR":  routedScalarFunction(scalarFunctionBitOr, NullableLong),
	"BITXOR": routedScalarFunction(scalarFunctionBitXor, NullableLong),

	// Bitmap bucketing functions (Java ArithmeticValue.java:513-520): binary
	// under the hood — the SQL surface is unary and the walker appends the
	// default entry size 10000 (SemanticAnalyzer.java:988-990). floorDiv
	// semantics, exactly Java's physical operators. These enter through the
	// generic ScalarFunctionCall grammar route (keyword functionName), so
	// scalarCall is set. bitmap_bucket_number is deliberately absent: Java's
	// SQL catalog registers only these two plus bitmap_construct_agg
	// (SqlFunctionCatalogImpl.java:126-128).
	"BITMAP_BUCKET_OFFSET": scalarCallFunction(scalarFunctionBitmapBucketOffset, NullableLong),
	"BITMAP_BIT_POSITION":  scalarCallFunction(scalarFunctionBitmapBitPosition, NullableLong),

	// Date/time functions.
	"YEAR": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, NullableLong),
		LegacyMapScalarFunctionYear),
	"MONTH": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, NullableLong),
		LegacyMapScalarFunctionMonth),
	"DAY": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, NullableLong),
		LegacyMapScalarFunctionDay),
	"DAYOFMONTH": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, NullableLong),
		LegacyMapScalarFunctionDayOfMonth),
	"HOUR": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, NullableLong),
		LegacyMapScalarFunctionHour),
	"MINUTE": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, NullableLong),
		LegacyMapScalarFunctionMinute),
	"SECOND": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, NullableLong),
		LegacyMapScalarFunctionSecond),
	"DAYOFWEEK": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, NullableLong),
		LegacyMapScalarFunctionDayOfWeek),
	"DAYOFYEAR": legacyMapScalarCall(
		scalarCallFunction(scalarFunctionDatePart, NullableLong),
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

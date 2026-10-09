package values

import (
	"embed"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"
	"testing"
)

// valueSources embeds the package's sources so the census below can find every
// class that defines Type() (Bazel does not stage sources beside the test
// binary; gazelle lists them in embedsrcs).
//
//go:embed *.go
var valueSources embed.FS

// censusStatus is how a class's result nullability compares with the Java
// class it ports (WS-E design 5.3).
type censusStatus int

const (
	// censusSame: Go derives nullability as the Java class does.
	censusSame censusStatus = iota + 1
	// censusGoLooser: Go types it nullable where Java can type it NOT NULL.
	// Never a wrong answer; Go only folds less.
	censusGoLooser
	// censusGoStricter: Go types it NOT NULL where Java types it nullable.
	// OPEN: loosen it (design 5.3) before the EffectiveConstant NOT_NULL arm
	// lands, which would let an IS NULL over it fold without evaluating it.
	censusGoStricter
	// censusGoOnly: no Java class; the rule is stated with its reason.
	censusGoOnly
	// censusNotAValue: defines Type() but is not a Value.
	censusNotAValue
)

type nullabilityCensusEntry struct {
	status censusStatus
	// goRule is how Type() derives nullability.
	goRule string
	// java is the Java class and its rule, or why there is none.
	java string
	// probe, for a class whose type does not depend on its fields, builds a
	// zero value whose Type() the test reads, so the table cannot drift from
	// the code.
	probe func() Value
}

// valueNullabilityCensus is every class of this package that defines Type(),
// with its nullability rule and the Java class the rule derives from (Java
// 4.14.2.0). Type.primitiveType(code) is nullable in Java (Type.java:404-405),
// and a BooleanValue is a nullable BOOLEAN (BooleanValue.java:40-42).
var valueNullabilityCensus = map[string]nullabilityCensusEntry{
	"AggregateValue": {
		censusSame,
		"COUNT/COUNT(*) LONG, AVG DOUBLE, BITMAP BYTES, SUM/MIN/MAX the operand's type, ARRAY_AGG an array: all nullable",
		"CountValue/NumericAggregationValue: primitiveType(operator result code)", nil,
	},
	"AndOrValue": {
		censusSame,
		"nullable BOOLEAN",
		"AndOrValue is a BooleanValue: nullable BOOLEAN",
		func() Value { return &AndOrValue{} },
	},
	"ArithmeticValue": {
		censusSame,
		"the lane's result type, nullable",
		"ArithmeticValue: primitiveType(operator result code)", nil,
	},
	"ArrayConstructorValue": {
		censusSame,
		"NOT NULL array (Evaluate always returns a slice or an error)",
		"AbstractArrayConstructorValue: new Type.Array(elementType), not nullable", nil,
	},
	"ArrayDistinctValue": {censusSame, "as built", "ArrayDistinctValue: resultType as built", nil},
	"BooleanValue": {
		censusSame,
		"a boolean literal: NOT NULL unless its value is NULL",
		"LiteralValue: the literal's type", nil,
	},
	"CardinalityValue": {
		censusSame, "nullable INT", "CardinalityValue: primitiveType(INT)",
		func() Value { return &CardinalityValue{} },
	},
	"CastValue": {
		censusSame,
		"the target with the operand's nullability; never NULL from a non-NULL operand (TestCastValue_NonNullOperandNeverCastsToNull)",
		"ExpressionVisitor.java:532 targetType.withNullability(operand nullable)", nil,
	},
	"CollateValue": {
		censusSame,
		"nullable BYTES",
		"CollateValue: primitiveType(BYTES)",
		func() Value { return &CollateValue{} },
	},
	"ConditionSelectorValue": {
		censusSame,
		"nullable INT (NULL when no implication holds)",
		"ConditionSelectorValue: primitiveType(INT)",
		func() Value { return &ConditionSelectorValue{} },
	},
	"ConstantObjectValue": {censusSame, "as built", "ConstantObjectValue: resultType as built", nil},
	"ConstantValue": {
		censusSame,
		"the constant's type, NOT NULL unless its value is NULL",
		"LiteralValue: the literal's type", nil,
	},
	"CosineDistanceRowNumberValue": {
		censusSame,
		"nullable LONG (index-only)",
		"CosineDistanceRowNumberValue: primitiveType(LONG)",
		func() Value { return &CosineDistanceRowNumberValue{} },
	},
	"DerivedValue": {censusSame, "as built (non-evaluable)", "DerivedValue: resultType as built", nil},
	"DistanceRowNumberValue": {
		censusGoOnly,
		"nullable LONG (index-only)",
		"no Java class: Go's operator-generic form of the four *DistanceRowNumberValue classes, which are primitiveType(LONG)",
		func() Value { return &DistanceRowNumberValue{} },
	},
	"DistanceValue": {
		censusSame,
		"nullable DOUBLE (NULL for a NULL operand or a dimension mismatch)",
		"DistanceValue: primitiveType(DOUBLE)",
		func() Value { return &DistanceValue{} },
	},
	"DotProductDistanceRowNumberValue": {
		censusSame,
		"nullable LONG (index-only)",
		"DotProductDistanceRowNumberValue: primitiveType(LONG)",
		func() Value { return &DotProductDistanceRowNumberValue{} },
	},
	"EmptyValue": {
		censusSame,
		"nullable empty record (Evaluate returns NULL)",
		"EmptyValue: Value's default primitiveType(UNKNOWN)",
		func() Value { return &EmptyValue{} },
	},
	"EuclideanDistanceRowNumberValue": {
		censusSame,
		"nullable LONG (index-only)",
		"EuclideanDistanceRowNumberValue: primitiveType(LONG)",
		func() Value { return &EuclideanDistanceRowNumberValue{} },
	},
	"EuclideanSquareDistanceRowNumberValue": {
		censusSame,
		"nullable LONG (index-only)",
		"EuclideanSquareDistanceRowNumberValue: primitiveType(LONG)",
		func() Value { return &EuclideanSquareDistanceRowNumberValue{} },
	},
	"EvaluatesToValue": {
		censusSame,
		"nullable BOOLEAN",
		"EvaluatesToValue: Value's default primitiveType(UNKNOWN), nullable; Go keeps the BOOLEAN code",
		func() Value { return &EvaluatesToValue{} },
	},
	"ExistsValue": {
		censusSame,
		"nullable BOOLEAN",
		"ExistsValue is a BooleanValue: nullable BOOLEAN",
		func() Value { return &ExistsValue{} },
	},
	"exactType": {censusNotAValue, "a Type wrapper", "not a Value", nil},
	"fieldValue": {
		censusSame,
		"the field's declared type, widened to nullable on a null-on-empty edge or gated path",
		"FieldValue: the field's type", nil,
	},
	"FirstOrDefaultStreamingValue": {censusSame, "its child's type", "FirstOrDefaultStreamingValue: resultType as built from its child", nil},
	"FirstOrDefaultValue":          {censusSame, "as built", "FirstOrDefaultValue: resultType as built", nil},
	"FromOrderedBytesValue": {
		censusGoLooser,
		"the target type, made nullable",
		"FromOrderedBytesValue: resultType as built", nil,
	},
	"IncarnationValue": {
		censusSame,
		"nullable INT (NULL without an incarnation in its context)",
		"IncarnationValue: primitiveType(INT)",
		func() Value { return &IncarnationValue{} },
	},
	"IndexedValue":          {censusSame, "as built (non-evaluable)", "IndexedValue: resultType as built", nil},
	"IndexEntryObjectValue": {censusSame, "as built", "IndexEntryObjectValue: resultType as built", nil},
	"IndexOnlyAggregateValue": {
		censusSame, "its child's type",
		"IndexOnlyAggregateValue: child.getResultType()", nil,
	},
	"BinaryRelOpValue": {
		censusSame, "nullable BOOLEAN", "RelOpValue is a BooleanValue",
		func() Value { return &BinaryRelOpValue{} },
	},
	"TautologicalValue": {
		censusSame, "nullable BOOLEAN", "TautologicalValue is a BooleanValue",
		func() Value { return TautologicalValue{} },
	},
	"UnaryRelOpValue": {
		censusSame, "nullable BOOLEAN", "RelOpValue is a BooleanValue",
		func() Value { return &UnaryRelOpValue{} },
	},
	"InOpValue": {
		censusSame, "nullable BOOLEAN", "InOpValue is a BooleanValue",
		func() Value { return &InOpValue{} },
	},
	"LikeOperatorValue": {
		censusSame, "nullable BOOLEAN", "LikeOperatorValue is a BooleanValue",
		func() Value { return &LikeOperatorValue{} },
	},
	"NarrowValue": {
		censusGoOnly, "as built",
		"no Java class: Go's checked narrowing to a declared target", nil,
	},
	"NotValue": {
		censusSame,
		"nullable BOOLEAN",
		"NotValue is a BooleanValue: nullable BOOLEAN",
		func() Value { return &NotValue{} },
	},
	"NullValue":   {censusSame, "nullable", "NullValue: its type, nullable", nil},
	"ObjectValue": {censusSame, "as built", "ObjectValue: resultType as built", nil},
	"OfTypeValue": {
		censusSame, "nullable BOOLEAN",
		"OfTypeValue: Value's default primitiveType(UNKNOWN), also nullable",
		func() Value { return &OfTypeValue{} },
	},
	"ParameterObjectValue": {
		censusGoLooser, "as built, made nullable",
		"ParameterObjectValue: resultType as built", nil,
	},
	"ParameterValue": {
		censusGoOnly, "nullable",
		"no Java class: an unbound `?` placeholder whose value is not known", nil,
	},
	"PatternForLikeValue": {
		censusSame, "NOT NULL record of two nullable strings",
		"PatternForLikeValue.TYPE: Record.fromFields(false, two nullable STRINGs)",
		func() Value { return &PatternForLikeValue{} },
	},
	"PickValue": {
		censusSame,
		"as built; the CASE builder makes it nullable (CommonValueType)",
		"PickValue: resultType as built, made nullable by its builder (PickValue.java:203)", nil,
	},
	"PromoteValue": {censusSame, "the target as built", "PromoteValue: promoteToType", nil},
	"quantifiedObjectValue": {
		censusSame, "the flowed type",
		"QuantifiedObjectValue: resultType", nil,
	},
	"QuantifiedRecordValue": {censusSame, "as built", "QuantifiedRecordValue: resultType as built", nil},
	"QueriedValue":          {censusSame, "as built (non-evaluable)", "QueriedValue: resultType as built", nil},
	"RangeValue": {
		censusSame, "NOT NULL record of its ID",
		"RangeValue: a record constructor's type (RangeValue.java:100)",
		func() Value { return &RangeValue{} },
	},
	"RankValue": {
		censusSame,
		"nullable LONG (NULL without a rank in its context)",
		"RankValue: primitiveType(LONG)",
		func() Value { return &RankValue{} },
	},
	"RecordConstructorValue": {
		censusSame, "NOT NULL record",
		"RecordConstructorValue: computeResultType(columns, false)", nil,
	},
	"RecordTypeValue": {
		censusSame,
		"nullable LONG (NULL for a record without a type key)",
		"RecordTypeValue: primitiveType(LONG)",
		func() Value { return &RecordTypeValue{} },
	},
	"RowNumberValue": {
		censusSame,
		"nullable LONG (NULL without a row number in its context)",
		"RowNumberValue: primitiveType(LONG)",
		func() Value { return &RowNumberValue{} },
	},
	"ScalarFunctionValue": {
		censusSame,
		"per catalog entry: COALESCE nullable iff every argument is, GREATEST/LEAST iff any is, Go-only functions always",
		"VariadicFunctionValue.java:339-376; the Go-only functions have no Java class", nil,
	},
	"ScalarSubqueryValue": {
		censusGoOnly, "as built",
		"no Java class: Java refuses a scalar subquery in an expression (42601)", nil,
	},
	"StrictRankLimitValue": {
		censusGoOnly, "nullable LONG",
		"no Java class: Go's rank-cap arithmetic, NULL when its bound is",
		func() Value { return &StrictRankLimitValue{} },
	},
	"SubscriptValue": {
		censusSame, "the array's element type, nullable",
		"SubscriptValue.java:63 elementType.nullable()", nil,
	},
	"ThrowsValue": {censusSame, "as built", "ThrowsValue: resultType as built", nil},
	"ToOrderedBytesValue": {
		censusSame,
		"nullable BYTES",
		"ToOrderedBytesValue: primitiveType(BYTES)",
		func() Value { return &ToOrderedBytesValue{} },
	},
	"UdfValue": {censusSame, "as built", "UdfValue: resultType as built", nil},
	"UnmatchedAggregateValue": {
		censusGoOnly, "UNKNOWN (non-evaluable marker)",
		"no Java class: marks an aggregate no index matched",
		func() Value { return &UnmatchedAggregateValue{} },
	},
	"VersionValue": {
		censusGoOnly, "nullable VERSION",
		"no Java class of this name: Go's record-version read",
		func() Value { return &VersionValue{} },
	},
}

// typeDefiningClasses returns the receiver type of every Type() Type method in
// the package's non-test sources.
func typeDefiningClasses(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(valueSources, ".")
	if err != nil {
		t.Fatalf("read embedded sources: %v", err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := fs.ReadFile(valueSources, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != "Type" || fn.Type.Params.NumFields() != 0 ||
				fn.Type.Results.NumFields() != 1 {
				continue
			}
			if result, ok := fn.Type.Results.List[0].Type.(*ast.Ident); !ok || result.Name != "Type" {
				continue
			}
			recv := fn.Recv.List[0].Type
			if star, ok := recv.(*ast.StarExpr); ok {
				recv = star.X
			}
			if ident, ok := recv.(*ast.Ident); ok {
				seen[ident.Name] = true
			}
		}
	}
	classes := make([]string, 0, len(seen))
	for name := range seen {
		classes = append(classes, name)
	}
	sort.Strings(classes)
	return classes
}

// Every class that defines Type() states its nullability rule and the Java
// class it derives from (WS-E design 5.3); a new class without an entry, or an
// entry for a class that no longer exists, fails.
func TestValueNullabilityCensus_CoversEveryTypeDefiningClass(t *testing.T) {
	t.Parallel()
	classes := typeDefiningClasses(t)
	if len(classes) < 50 {
		t.Fatalf("found only %d Type() classes in the embedded sources; the census is misconfigured", len(classes))
	}
	present := map[string]bool{}
	for _, class := range classes {
		present[class] = true
		entry, ok := valueNullabilityCensus[class]
		if !ok {
			t.Errorf("%s defines Type() but has no nullability census entry", class)
			continue
		}
		if entry.status == 0 || entry.goRule == "" || entry.java == "" {
			t.Errorf("%s: census entry is incomplete: %+v", class, entry)
		}
	}
	for class := range valueNullabilityCensus {
		if !present[class] {
			t.Errorf("census entry %s names no class that defines Type()", class)
		}
	}
}

// A fixed-type class's entry is checked against its Type(): a NOT NULL claim
// (censusGoStricter, or a NOT NULL rule in the other statuses) must be what
// Type() returns, so loosening a class forces its entry to change.
func TestValueNullabilityCensus_FixedTypesMatchTheirEntries(t *testing.T) {
	t.Parallel()
	for class, entry := range valueNullabilityCensus {
		if entry.probe == nil {
			continue
		}
		typ := entry.probe().Type()
		if typ == nil {
			t.Errorf("%s: Type() is nil", class)
			continue
		}
		notNull := strings.HasPrefix(entry.goRule, "NOT NULL")
		if entry.status == censusGoStricter && !notNull {
			t.Errorf("%s: a GoStricter entry must state its NOT NULL rule, got %q", class, entry.goRule)
		}
		if typ.IsNullable() == notNull {
			t.Errorf("%s: Type() nullable=%t, but its census rule is %q", class, typ.IsNullable(), entry.goRule)
		}
	}
}

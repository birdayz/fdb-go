package values

import "strings"

// ArithmeticLane is one row of Java's ArithmeticValue.PhysicalOperator table
// (ArithmeticValue.java:406-522): a logical operator, the operand type codes it
// is defined over, and its result type code. Function is the logical operator's
// lower-case name, the name a key expression's function carries
// (MaterializedViewIndexGenerator.java:574).
type ArithmeticLane struct {
	Function    string
	Left, Right TypeCode
	Result      TypeCode
}

// arithmeticLanes is ArithmeticValue.PhysicalOperator, whole and in Java's
// order: ADD over every pair of INT, LONG, FLOAT, DOUBLE and STRING (25); SUB,
// MUL, DIV and MOD over every pair of INT, LONG, FLOAT and DOUBLE (16 each);
// BITOR, BITAND and BITXOR over the four integer pairs (4 each); and the three
// bitmap functions over (LONG, INT) and (INT, INT) (2 each): 107 rows. Java's
// encapsulate looks a call up here by (operator, left type, right type) and
// fails with "unable to encapsulate arithmetic operation due to type
// mismatch(es)" when no row names it; a query over an index key with no row
// fails the same way, so the table decides which index keys the target can plan
// (RFC-257 WS-J section 3.2).
var arithmeticLanes = [...]ArithmeticLane{
	{"add", TypeCodeInt, TypeCodeInt, TypeCodeInt},          // ADD_II
	{"add", TypeCodeInt, TypeCodeLong, TypeCodeLong},        // ADD_IL
	{"add", TypeCodeInt, TypeCodeFloat, TypeCodeFloat},      // ADD_IF
	{"add", TypeCodeInt, TypeCodeDouble, TypeCodeDouble},    // ADD_ID
	{"add", TypeCodeInt, TypeCodeString, TypeCodeString},    // ADD_IS
	{"add", TypeCodeLong, TypeCodeInt, TypeCodeLong},        // ADD_LI
	{"add", TypeCodeLong, TypeCodeLong, TypeCodeLong},       // ADD_LL
	{"add", TypeCodeLong, TypeCodeFloat, TypeCodeFloat},     // ADD_LF
	{"add", TypeCodeLong, TypeCodeDouble, TypeCodeDouble},   // ADD_LD
	{"add", TypeCodeLong, TypeCodeString, TypeCodeString},   // ADD_LS
	{"add", TypeCodeFloat, TypeCodeInt, TypeCodeFloat},      // ADD_FI
	{"add", TypeCodeFloat, TypeCodeLong, TypeCodeFloat},     // ADD_FL
	{"add", TypeCodeFloat, TypeCodeFloat, TypeCodeFloat},    // ADD_FF
	{"add", TypeCodeFloat, TypeCodeDouble, TypeCodeDouble},  // ADD_FD
	{"add", TypeCodeFloat, TypeCodeString, TypeCodeString},  // ADD_FS
	{"add", TypeCodeDouble, TypeCodeInt, TypeCodeDouble},    // ADD_DI
	{"add", TypeCodeDouble, TypeCodeLong, TypeCodeDouble},   // ADD_DL
	{"add", TypeCodeDouble, TypeCodeFloat, TypeCodeDouble},  // ADD_DF
	{"add", TypeCodeDouble, TypeCodeDouble, TypeCodeDouble}, // ADD_DD
	{"add", TypeCodeDouble, TypeCodeString, TypeCodeString}, // ADD_DS
	{"add", TypeCodeString, TypeCodeInt, TypeCodeString},    // ADD_SI
	{"add", TypeCodeString, TypeCodeLong, TypeCodeString},   // ADD_SL
	{"add", TypeCodeString, TypeCodeFloat, TypeCodeString},  // ADD_SF
	{"add", TypeCodeString, TypeCodeDouble, TypeCodeString}, // ADD_SD
	{"add", TypeCodeString, TypeCodeString, TypeCodeString}, // ADD_SS

	{"sub", TypeCodeInt, TypeCodeInt, TypeCodeInt},          // SUB_II
	{"sub", TypeCodeInt, TypeCodeLong, TypeCodeLong},        // SUB_IL
	{"sub", TypeCodeInt, TypeCodeFloat, TypeCodeFloat},      // SUB_IF
	{"sub", TypeCodeInt, TypeCodeDouble, TypeCodeDouble},    // SUB_ID
	{"sub", TypeCodeLong, TypeCodeInt, TypeCodeLong},        // SUB_LI
	{"sub", TypeCodeLong, TypeCodeLong, TypeCodeLong},       // SUB_LL
	{"sub", TypeCodeLong, TypeCodeFloat, TypeCodeFloat},     // SUB_LF
	{"sub", TypeCodeLong, TypeCodeDouble, TypeCodeDouble},   // SUB_LD
	{"sub", TypeCodeFloat, TypeCodeInt, TypeCodeFloat},      // SUB_FI
	{"sub", TypeCodeFloat, TypeCodeLong, TypeCodeFloat},     // SUB_FL
	{"sub", TypeCodeFloat, TypeCodeFloat, TypeCodeFloat},    // SUB_FF
	{"sub", TypeCodeFloat, TypeCodeDouble, TypeCodeDouble},  // SUB_FD
	{"sub", TypeCodeDouble, TypeCodeInt, TypeCodeDouble},    // SUB_DI
	{"sub", TypeCodeDouble, TypeCodeLong, TypeCodeDouble},   // SUB_DL
	{"sub", TypeCodeDouble, TypeCodeFloat, TypeCodeDouble},  // SUB_DF
	{"sub", TypeCodeDouble, TypeCodeDouble, TypeCodeDouble}, // SUB_DD

	{"mul", TypeCodeInt, TypeCodeInt, TypeCodeInt},          // MUL_II
	{"mul", TypeCodeInt, TypeCodeLong, TypeCodeLong},        // MUL_IL
	{"mul", TypeCodeInt, TypeCodeFloat, TypeCodeFloat},      // MUL_IF
	{"mul", TypeCodeInt, TypeCodeDouble, TypeCodeDouble},    // MUL_ID
	{"mul", TypeCodeLong, TypeCodeInt, TypeCodeLong},        // MUL_LI
	{"mul", TypeCodeLong, TypeCodeLong, TypeCodeLong},       // MUL_LL
	{"mul", TypeCodeLong, TypeCodeFloat, TypeCodeFloat},     // MUL_LF
	{"mul", TypeCodeLong, TypeCodeDouble, TypeCodeDouble},   // MUL_LD
	{"mul", TypeCodeFloat, TypeCodeInt, TypeCodeFloat},      // MUL_FI
	{"mul", TypeCodeFloat, TypeCodeLong, TypeCodeFloat},     // MUL_FL
	{"mul", TypeCodeFloat, TypeCodeFloat, TypeCodeFloat},    // MUL_FF
	{"mul", TypeCodeFloat, TypeCodeDouble, TypeCodeDouble},  // MUL_FD
	{"mul", TypeCodeDouble, TypeCodeInt, TypeCodeDouble},    // MUL_DI
	{"mul", TypeCodeDouble, TypeCodeLong, TypeCodeDouble},   // MUL_DL
	{"mul", TypeCodeDouble, TypeCodeFloat, TypeCodeDouble},  // MUL_DF
	{"mul", TypeCodeDouble, TypeCodeDouble, TypeCodeDouble}, // MUL_DD

	{"div", TypeCodeInt, TypeCodeInt, TypeCodeInt},          // DIV_II
	{"div", TypeCodeInt, TypeCodeLong, TypeCodeLong},        // DIV_IL
	{"div", TypeCodeInt, TypeCodeFloat, TypeCodeFloat},      // DIV_IF
	{"div", TypeCodeInt, TypeCodeDouble, TypeCodeDouble},    // DIV_ID
	{"div", TypeCodeLong, TypeCodeInt, TypeCodeLong},        // DIV_LI
	{"div", TypeCodeLong, TypeCodeLong, TypeCodeLong},       // DIV_LL
	{"div", TypeCodeLong, TypeCodeFloat, TypeCodeFloat},     // DIV_LF
	{"div", TypeCodeLong, TypeCodeDouble, TypeCodeDouble},   // DIV_LD
	{"div", TypeCodeFloat, TypeCodeInt, TypeCodeFloat},      // DIV_FI
	{"div", TypeCodeFloat, TypeCodeLong, TypeCodeFloat},     // DIV_FL
	{"div", TypeCodeFloat, TypeCodeFloat, TypeCodeFloat},    // DIV_FF
	{"div", TypeCodeFloat, TypeCodeDouble, TypeCodeDouble},  // DIV_FD
	{"div", TypeCodeDouble, TypeCodeInt, TypeCodeDouble},    // DIV_DI
	{"div", TypeCodeDouble, TypeCodeLong, TypeCodeDouble},   // DIV_DL
	{"div", TypeCodeDouble, TypeCodeFloat, TypeCodeDouble},  // DIV_DF
	{"div", TypeCodeDouble, TypeCodeDouble, TypeCodeDouble}, // DIV_DD

	{"mod", TypeCodeInt, TypeCodeInt, TypeCodeInt},          // MOD_II
	{"mod", TypeCodeInt, TypeCodeLong, TypeCodeLong},        // MOD_IL
	{"mod", TypeCodeInt, TypeCodeFloat, TypeCodeFloat},      // MOD_IF
	{"mod", TypeCodeInt, TypeCodeDouble, TypeCodeDouble},    // MOD_ID
	{"mod", TypeCodeLong, TypeCodeInt, TypeCodeLong},        // MOD_LI
	{"mod", TypeCodeLong, TypeCodeLong, TypeCodeLong},       // MOD_LL
	{"mod", TypeCodeLong, TypeCodeFloat, TypeCodeFloat},     // MOD_LF
	{"mod", TypeCodeLong, TypeCodeDouble, TypeCodeDouble},   // MOD_LD
	{"mod", TypeCodeFloat, TypeCodeInt, TypeCodeFloat},      // MOD_FI
	{"mod", TypeCodeFloat, TypeCodeLong, TypeCodeFloat},     // MOD_FL
	{"mod", TypeCodeFloat, TypeCodeFloat, TypeCodeFloat},    // MOD_FF
	{"mod", TypeCodeFloat, TypeCodeDouble, TypeCodeDouble},  // MOD_FD
	{"mod", TypeCodeDouble, TypeCodeInt, TypeCodeDouble},    // MOD_DI
	{"mod", TypeCodeDouble, TypeCodeLong, TypeCodeDouble},   // MOD_DL
	{"mod", TypeCodeDouble, TypeCodeFloat, TypeCodeDouble},  // MOD_DF
	{"mod", TypeCodeDouble, TypeCodeDouble, TypeCodeDouble}, // MOD_DD

	{"bitor", TypeCodeInt, TypeCodeInt, TypeCodeInt},    // BITOR_II
	{"bitor", TypeCodeInt, TypeCodeLong, TypeCodeLong},  // BITOR_IL
	{"bitor", TypeCodeLong, TypeCodeInt, TypeCodeLong},  // BITOR_LI
	{"bitor", TypeCodeLong, TypeCodeLong, TypeCodeLong}, // BITOR_LL

	{"bitand", TypeCodeInt, TypeCodeInt, TypeCodeInt},    // BITAND_II
	{"bitand", TypeCodeInt, TypeCodeLong, TypeCodeLong},  // BITAND_IL
	{"bitand", TypeCodeLong, TypeCodeInt, TypeCodeLong},  // BITAND_LI
	{"bitand", TypeCodeLong, TypeCodeLong, TypeCodeLong}, // BITAND_LL

	{"bitxor", TypeCodeInt, TypeCodeInt, TypeCodeInt},    // BITXOR_II
	{"bitxor", TypeCodeInt, TypeCodeLong, TypeCodeLong},  // BITXOR_IL
	{"bitxor", TypeCodeLong, TypeCodeInt, TypeCodeLong},  // BITXOR_LI
	{"bitxor", TypeCodeLong, TypeCodeLong, TypeCodeLong}, // BITXOR_LL

	{"bitmap_bucket_offset", TypeCodeLong, TypeCodeInt, TypeCodeLong}, // BITMAP_BUCKET_OFFSET_LI
	{"bitmap_bucket_offset", TypeCodeInt, TypeCodeInt, TypeCodeInt},   // BITMAP_BUCKET_OFFSET_II

	{"bitmap_bucket_number", TypeCodeLong, TypeCodeInt, TypeCodeLong}, // BITMAP_BUCKET_NUMBER_LI
	{"bitmap_bucket_number", TypeCodeInt, TypeCodeInt, TypeCodeInt},   // BITMAP_BUCKET_NUMBER_II

	{"bitmap_bit_position", TypeCodeLong, TypeCodeInt, TypeCodeLong}, // BITMAP_BIT_POSITION_LI
	{"bitmap_bit_position", TypeCodeInt, TypeCodeInt, TypeCodeInt},   // BITMAP_BIT_POSITION_II
}

type arithmeticLaneKey struct {
	function    string
	left, right TypeCode
}

var arithmeticLaneIndex = func() map[arithmeticLaneKey]ArithmeticLane {
	m := make(map[arithmeticLaneKey]ArithmeticLane, len(arithmeticLanes))
	for _, l := range arithmeticLanes {
		m[arithmeticLaneKey{l.Function, l.Left, l.Right}] = l
	}
	return m
}()

// ArithmeticLanes is a copy of the whole table, in Java's order.
func ArithmeticLanes() []ArithmeticLane {
	return append([]ArithmeticLane(nil), arithmeticLanes[:]...)
}

// IsArithmeticFunction reports whether function (any case) is one of
// ArithmeticValue's logical operators, the functions Java builds through
// encapsulate and so the ones a lane decides.
func IsArithmeticFunction(function string) bool {
	f := strings.ToLower(function)
	for _, l := range arithmeticLanes {
		if l.Function == f {
			return true
		}
	}
	return false
}

// LookupArithmeticLane is Java's getOperatorMap().get(new
// BinaryOperatorSignature(operator, left, right)): the row for function (any
// case) over the two operand type codes, if there is one.
func LookupArithmeticLane(function string, left, right TypeCode) (ArithmeticLane, bool) {
	l, ok := arithmeticLaneIndex[arithmeticLaneKey{strings.ToLower(function), left, right}]
	return l, ok
}

// ArithmeticOperandIsPrimitive is Java's TypeCode.isPrimitive() as
// ArithmeticValue.encapsulate checks each operand before it looks up a lane
// (ArithmeticValue.java:215-220): UNKNOWN, NULL, BOOLEAN, BYTES, DOUBLE, FLOAT,
// INT, LONG, STRING, VECTOR and VERSION are primitive; ENUM, RECORD, UUID,
// ARRAY, RELATION, NONE and ANY are not (Type.java:774-791). It differs from
// TypeCode.IsPrimitive, which calls UUID primitive and UNKNOWN and NULL not.
// Go has no VECTOR code (a vector column types as BYTES here, primitive as
// Java's VECTOR is); DATE and TIMESTAMP, Go-only codes, are primitive (they have
// no lane either way).
func ArithmeticOperandIsPrimitive(tc TypeCode) bool {
	switch tc {
	case TypeCodeUnknown, TypeCodeNull, TypeCodeBoolean, TypeCodeBytes,
		TypeCodeDouble, TypeCodeFloat, TypeCodeInt, TypeCodeLong, TypeCodeString,
		TypeCodeVersion, TypeCodeDate, TypeCodeTimestamp:
		return true
	}
	return false
}

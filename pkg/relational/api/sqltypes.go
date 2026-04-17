package api

// Mappings between the SQL type name syntax used in the parser and
// their JDBC type codes / DataType instances. Mirrors Java's
// SqlTypeNamesSupport.
//
// These are used by the SQL parser (to resolve unqualified type
// references in DDL) and the ResultSetMetaData implementation (to
// translate an internal type back to a user-visible name).

// SQLTypeName is a display name used in DDL ("INTEGER", "BIGINT", ...).
//
// Values are uppercase, matching Java's switch arm labels.
const (
	SQLTypeNameInteger SQLTypeName = "INTEGER"
	SQLTypeNameBigInt  SQLTypeName = "BIGINT"
	SQLTypeNameFloat   SQLTypeName = "FLOAT"
	SQLTypeNameDouble  SQLTypeName = "DOUBLE"
	SQLTypeNameString  SQLTypeName = "STRING"
	SQLTypeNameStruct  SQLTypeName = "STRUCT"
	SQLTypeNameArray   SQLTypeName = "ARRAY"
	SQLTypeNameBinary  SQLTypeName = "BINARY"
	SQLTypeNameNull    SQLTypeName = "NULL"
	SQLTypeNameOther   SQLTypeName = "OTHER"
	SQLTypeNameBoolean SQLTypeName = "BOOLEAN"
)

// SQLTypeName is a type-safe alias for SQL type display names.
type SQLTypeName string

// SQLTypeNameFromJDBC returns the display name for a JDBC type code.
// Returns an empty string for unrecognised codes (Java throws
// IllegalStateException; Go-idiomatic is an empty return — callers
// check with name == "").
func SQLTypeNameFromJDBC(jdbcCode int) SQLTypeName {
	switch jdbcCode {
	case JDBCInteger:
		return SQLTypeNameInteger
	case JDBCBigInt:
		return SQLTypeNameBigInt
	case JDBCFloat:
		return SQLTypeNameFloat
	case JDBCDouble:
		return SQLTypeNameDouble
	case JDBCVarchar:
		return SQLTypeNameString
	case JDBCStruct:
		return SQLTypeNameStruct
	case JDBCArray:
		return SQLTypeNameArray
	case JDBCBinary:
		return SQLTypeNameBinary
	case JDBCNull:
		return SQLTypeNameNull
	case JDBCOther:
		return SQLTypeNameOther
	case JDBCBoolean:
		return SQLTypeNameBoolean
	default:
		return ""
	}
}

// JDBCFromSQLTypeName returns the JDBC type code for a display name.
// Returns -1 for unrecognised names. Java throws
// IllegalStateException; we prefer a sentinel so callers can decide.
func JDBCFromSQLTypeName(name SQLTypeName) int {
	switch name {
	case SQLTypeNameInteger:
		return JDBCInteger
	case SQLTypeNameBinary:
		return JDBCBinary
	case SQLTypeNameBigInt:
		return JDBCBigInt
	case SQLTypeNameFloat:
		return JDBCFloat
	case SQLTypeNameDouble:
		return JDBCDouble
	case SQLTypeNameString:
		return JDBCVarchar
	case SQLTypeNameStruct:
		return JDBCStruct
	case SQLTypeNameArray:
		return JDBCArray
	case SQLTypeNameNull:
		return JDBCNull
	case SQLTypeNameBoolean:
		return JDBCBoolean
	default:
		return -1
	}
}

// DataTypeFromSQLTypeName returns the non-nullable DataType for a
// display name, or nil if no primitive mapping applies (STRUCT /
// ARRAY cannot be reconstructed from a name alone — Java also
// returns null in those cases).
func DataTypeFromSQLTypeName(name SQLTypeName) DataType {
	switch name {
	case SQLTypeNameInteger:
		return NewIntegerType(false)
	case SQLTypeNameBinary:
		return NewBytesType(false)
	case SQLTypeNameBigInt:
		return NewLongType(false)
	case SQLTypeNameFloat:
		return NewFloatType(false)
	case SQLTypeNameDouble:
		return NewDoubleType(false)
	case SQLTypeNameString:
		return NewStringType(false)
	case SQLTypeNameBoolean:
		return NewBooleanType(false)
	case SQLTypeNameNull:
		return NewNullType()
	default:
		return nil
	}
}

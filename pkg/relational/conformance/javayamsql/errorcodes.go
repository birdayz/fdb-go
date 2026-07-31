package javayamsql

// errorCodes maps ErrorCode enum names to their 5-character SQLSTATE, mirroring
// fdb-relational-api's ErrorCode enum.
//
// An `error:` config may name either form (QueryConfig.resolveErrorCode: a
// 5-character string is taken as a SQLSTATE verbatim, anything else must resolve
// as an enum name). Keeping the table means a typo'd enum name is a parse error
// here exactly as it is there, instead of silently becoming an unmatchable code.
//
// Extracted from ErrorCode.java at the pinned tag; re-extract on a version bump.
var errorCodes = map[string]string{
	"AMBIGUOUS_COLUMN":                       "42702",
	"ANNOTATION_ALREADY_EXISTS":              "42F56",
	"ARRAY_ELEMENT_ERROR":                    "2202E",
	"CANNOT_COMMIT_ROLLBACK_WITH_AUTOCOMMIT": "08F02",
	"CANNOT_CONVERT_TYPE":                    "22000",
	"COLUMN_ALREADY_EXISTS":                  "42701",
	"CONNECTION_DOES_NOT_EXIST":              "08003",
	"COPY_IMPORT_VALIDATION_ERROR":           "22F08",
	"COPY_SERIALIZATION_ERROR":               "22F04",
	"DATABASE_ALREADY_EXISTS":                "42F04",
	"DATATYPE_MISMATCH":                      "42804",
	"DESERIALIZATION_FAILURE":                "XXF01",
	"DUPLICATE_ALIAS":                        "42712",
	"DUPLICATE_FUNCTION":                     "42723",
	"DUPLICATE_SCHEMA_TEMPLATE":              "42F62",
	"EXECUTE_UPDATE_RETURNED_RESULT_SET":     "42F61",
	"EXECUTION_LIMIT_REACHED":                "54F01",
	"GROUPING_ERROR":                         "42803",
	"INCOMPATIBLE_TABLE_ALIAS":               "42F20",
	"INCORRECT_METADATA_TABLE_VERSION":       "42F58",
	"INDEX_ALREADY_EXISTS":                   "42F57",
	"INSUFFICIENT_PRIVILEGE":                 "42501",
	"INTERNAL_ERROR":                         "XX000",
	"INVALID_ARGUMENT_FOR_FUNCTION":          "22F00",
	"INVALID_BINARY_REPRESENTATION":          "22F03",
	"INVALID_CAST":                           "22F3H",
	"INVALID_COLUMN_REFERENCE":               "42F10",
	"INVALID_CONTINUATION":                   "24F00",
	"INVALID_CURSOR_STATE":                   "24000",
	"INVALID_DATABASE":                       "42F66",
	"INVALID_FUNCTION_DEFINITION":            "42F13",
	"INVALID_NAME":                           "42602",
	"INVALID_PARAMETER":                      "22023",
	"INVALID_PATH":                           "08F01",
	"INVALID_PREPARED_STATEMENT_PARAMETER":   "42F60",
	"INVALID_RECURSION":                      "42F19",
	"INVALID_ROW_COUNT_IN_LIMIT_CLAUSE":      "2201W",
	"INVALID_SCHEMA_TEMPLATE":                "42F59",
	"INVALID_TABLE_DEFINITION":               "42F16",
	"INVALID_TRANSACTION_STATE":              "25000",
	"NOT_NULL_VIOLATION":                     "23502",
	"NO_RESULT_SET":                          "02F01",
	"SCHEMA_ALREADY_EXISTS":                  "42F06",
	"SCHEMA_MAPPING_ALREADY_EXISTS":          "42F50",
	"SERIALIZATION_FAILURE":                  "40001",
	"STATEMENT_CLOSED":                       "55F00",
	"SUCCESS":                                "00000",
	"SYNTAX_ERROR":                           "42601",
	"SYNTAX_OR_ACCESS_VIOLATION":             "42000",
	"TABLE_ALREADY_EXISTS":                   "42F07",
	"TOO_MANY_COLUMNS":                       "54011",
	"TRANSACTION_INACTIVE":                   "25F01",
	"TRANSACTION_TIMEOUT":                    "53F00",
	"UNABLE_TO_ESTABLISH_SQL_CONNECTION":     "08001",
	"UNDEFINED_COLUMN":                       "42703",
	"UNDEFINED_DATABASE":                     "42F00",
	"UNDEFINED_FILE":                         "58F01",
	"UNDEFINED_FUNCTION":                     "42883",
	"UNDEFINED_INDEX":                        "42F54",
	"UNDEFINED_PARAMETER":                    "42F02",
	"UNDEFINED_SCHEMA":                       "42F51",
	"UNDEFINED_TABLE":                        "42F01",
	"UNION_INCOMPATIBLE_COLUMNS":             "42F65",
	"UNION_INCORRECT_COLUMN_COUNT":           "42F64",
	"UNIQUE_CONSTRAINT_VIOLATION":            "23505",
	"UNKNOWN":                                "XXXXX",
	"UNKNOWN_DATABASE":                       "42F63",
	"UNKNOWN_SCHEMA_TEMPLATE":                "42F55",
	"UNKNOWN_TYPE":                           "42F18",
	"UNSUPPORTED_OPERATION":                  "0A000",
	"UNSUPPORTED_QUERY":                      "0AF00",
	"UNSUPPORTED_SORT":                       "0AF01",
	"WINDOWING_ERROR":                        "42F21",
	"WRONG_OBJECT_TYPE":                      "42809",
}

// resolveErrorCode mirrors QueryConfig.resolveErrorCode.
func resolveErrorCode(s string) (string, bool) {
	if len(s) == 5 {
		return s, true
	}
	code, ok := errorCodes[s]
	return code, ok
}

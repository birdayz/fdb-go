package sqlapi_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/sqlapi"
)

func TestErrorConstructorsAndFormatting(t *testing.T) {
	t.Parallel()

	t.Run("NewError and SQLState", func(t *testing.T) {
		t.Parallel()
		err := sqlapi.NewError(sqlapi.ErrCodeUndefinedTable, "table missing")
		if err.Code != sqlapi.ErrCodeUndefinedTable {
			t.Fatalf("Code = %q, want %q", err.Code, sqlapi.ErrCodeUndefinedTable)
		}
		if err.SQLState() != "42F01" {
			t.Fatalf("SQLState() = %q, want %q", err.SQLState(), "42F01")
		}
		if err.Error() != "42F01: table missing" {
			t.Fatalf("Error() = %q, want %q", err.Error(), "42F01: table missing")
		}
	})

	t.Run("nil Error SQLState", func(t *testing.T) {
		t.Parallel()
		var nilErr *sqlapi.Error
		if got := nilErr.SQLState(); got != "" {
			t.Fatalf("nil.SQLState() = %q, want empty", got)
		}
	})

	t.Run("NewErrorf", func(t *testing.T) {
		t.Parallel()
		err := sqlapi.NewErrorf(sqlapi.ErrCodeUndefinedColumn, "missing column %q", "id")
		if err.Code != sqlapi.ErrCodeUndefinedColumn || err.Message != `missing column "id"` {
			t.Fatalf("unexpected error: %+v", err)
		}
		if err.Cause != nil {
			t.Fatalf("Cause = %v, want nil", err.Cause)
		}
	})

	t.Run("WrapError and WrapErrorf", func(t *testing.T) {
		t.Parallel()
		cause := errors.New("disk io")
		e1 := sqlapi.WrapError(sqlapi.ErrCodeInternalError, "read failed", cause)
		if !errors.Is(e1, cause) || e1.Unwrap() != cause {
			t.Fatalf("WrapError failed to preserve cause: %v", e1)
		}
		if e1.Error() != "XX000: read failed: disk io" {
			t.Fatalf("WrapError.Error() = %q", e1.Error())
		}

		e2 := sqlapi.WrapErrorf(cause, sqlapi.ErrCodeInternalError, "load %s", "orders")
		if !errors.Is(e2, cause) || e2.Error() != "XX000: load orders: disk io" {
			t.Fatalf("WrapErrorf unexpected: %q", e2.Error())
		}
	})

	t.Run("WrapStatedError renders once and unwraps", func(t *testing.T) {
		t.Parallel()
		cause := errors.New("bad input")
		wrapped := fmt.Errorf("layer: %w", cause)
		err := sqlapi.WrapStatedError(sqlapi.ErrCodeInvalidParameter, cause.Error(), wrapped)
		if err.Error() != "22023: bad input" {
			t.Fatalf("Error() = %q, want %q", err.Error(), "22023: bad input")
		}
		if !errors.Is(err, cause) {
			t.Fatal("WrapStatedError broke cause unwrap chain")
		}
	})

	t.Run("WithContext is immutable and deterministic", func(t *testing.T) {
		t.Parallel()
		base := sqlapi.NewError(sqlapi.ErrCodeUndefinedTable, "missing")
		c1 := base.WithContext("table", "orders")
		c2 := c1.WithContext("schema", "S")
		if len(base.Context) != 0 {
			t.Fatalf("base Context mutated: %+v", base.Context)
		}
		if _, ok := c1.Context["schema"]; ok {
			t.Fatalf("c1 Context mutated: %+v", c1.Context)
		}
		msg := c2.Error()
		if msg != "42F01: missing [schema=S, table=orders]" {
			t.Fatalf("Error() = %q", msg)
		}
	})
}

func TestErrorCodeOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want sqlapi.ErrorCode
	}{
		{"nil", nil, ""},
		{"plain", errors.New("plain"), ""},
		{"direct sqlapi.Error", sqlapi.NewError(sqlapi.ErrCodeSyntaxError, "bad"), sqlapi.ErrCodeSyntaxError},
		{"wrapped sqlapi.Error", fmt.Errorf("outer: %w", sqlapi.NewError(sqlapi.ErrCodeUndefinedTable, "t")), sqlapi.ErrCodeUndefinedTable},
		{"joined sqlapi.Error", errors.Join(errors.New("other"), sqlapi.NewError(sqlapi.ErrCodeUniqueConstraintViolation, "dup")), sqlapi.ErrCodeUniqueConstraintViolation},
		{"api.Error alias", api.NewError(api.ErrCodeInternalError, "boom"), sqlapi.ErrCodeInternalError},
		{"explicit ErrCodeUnknown", sqlapi.NewError(sqlapi.ErrCodeUnknown, "unknown"), sqlapi.ErrCodeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sqlapi.ErrorCodeOf(tc.err); got != tc.want {
				t.Fatalf("ErrorCodeOf = %q, want %q", got, tc.want)
			}
			if got := api.ErrorCodeOf(tc.err); got != tc.want {
				t.Fatalf("api.ErrorCodeOf = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsRetryable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("not_committed"), false},
		{"serialization failure 40001", sqlapi.NewError(sqlapi.ErrCodeSerializationFailure, "conflict"), true},
		{"wrapped 40001", fmt.Errorf("commit: %w", sqlapi.WrapError(sqlapi.ErrCodeSerializationFailure, "conflict", errors.New("1020"))), true},
		{"api alias 40001", api.NewError(api.ErrCodeSerializationFailure, "conflict"), true},
		{"transaction time limit preemption 40001", sqlapi.NewTransactionTimeLimitError(4200*time.Millisecond, 4*time.Second), true},
		{"fdb transaction too old 40001", sqlapi.MarkFDBTransactionTooOld(errors.New("1007")), true},
		{"statement completion unknown 40003", sqlapi.NewError(sqlapi.ErrCodeStatementCompletionUnknown, "unknown"), false},
		{"transaction timeout 53F00", sqlapi.NewError(sqlapi.ErrCodeTransactionTimeout, "timed out"), false},
		{"transaction inactive 25F01", sqlapi.NewError(sqlapi.ErrCodeTransactionInactive, "cancelled"), false},
		{"execution limit 54F01", sqlapi.NewError(sqlapi.ErrCodeExecutionLimitReached, "limit"), false},
		{"plan complexity limit 54F02", sqlapi.NewError(sqlapi.ErrCodePlanComplexityLimitReached, "complex"), false},
		{"unique violation 23505", sqlapi.NewError(sqlapi.ErrCodeUniqueConstraintViolation, "dup"), false},
		{"syntax error 42601", sqlapi.NewError(sqlapi.ErrCodeSyntaxError, "syntax"), false},
		{"unknown XXXXX", sqlapi.NewError(sqlapi.ErrCodeUnknown, "unknown"), false},
		{"cross db ddl 42501", sqlapi.NewCrossDatabaseDDLError("DROP DATABASE", "/FRL/a", "/FRL/b"), false},
		{"template ddl restricted 42501", sqlapi.NewSchemaTemplateDDLRestrictedError("CREATE SCHEMA TEMPLATE", "/FRL/a"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sqlapi.IsRetryable(tc.err); got != tc.want {
				t.Fatalf("sqlapi.IsRetryable = %v, want %v (err=%v)", got, tc.want, tc.err)
			}
			if got := api.IsRetryable(tc.err); got != tc.want {
				t.Fatalf("api.IsRetryable = %v, want %v (err=%v)", got, tc.want, tc.err)
			}
		})
	}
}

func TestErrorsAsCrossPackageAliasIdentity(t *testing.T) {
	t.Parallel()

	t.Run("sqlapi.Error matched as api.Error and RelationalException", func(t *testing.T) {
		t.Parallel()
		orig := sqlapi.WrapError(sqlapi.ErrCodeUndefinedTable, "missing table", errors.New("cause"))
		wrapped := fmt.Errorf("exec: %w", orig)

		var apiErr *api.Error
		if !errors.As(wrapped, &apiErr) {
			t.Fatal("errors.As(*api.Error) failed on wrapped *sqlapi.Error")
		}
		if apiErr != orig {
			t.Fatalf("apiErr pointer %p != orig %p", apiErr, orig)
		}

		var sqlRel *sqlapi.RelationalException
		if !errors.As(wrapped, &sqlRel) || sqlRel != orig {
			t.Fatal("errors.As(*sqlapi.RelationalException) failed")
		}

		var apiRel *api.RelationalException
		if !errors.As(wrapped, &apiRel) || apiRel != orig {
			t.Fatal("errors.As(*api.RelationalException) failed")
		}
		if apiRel.SQLState() != "42F01" {
			t.Fatalf("SQLState() = %q, want 42F01", apiRel.SQLState())
		}
	})

	t.Run("api.Error matched as sqlapi.Error", func(t *testing.T) {
		t.Parallel()
		orig := &api.Error{Code: api.ErrCodeSerializationFailure, Message: "conflict"}
		wrapped := fmt.Errorf("commit: %w", orig)

		var sqlErr *sqlapi.Error
		if !errors.As(wrapped, &sqlErr) {
			t.Fatal("errors.As(*sqlapi.Error) failed on wrapped *api.Error")
		}
		if sqlErr != orig {
			t.Fatalf("sqlErr pointer %p != orig %p", sqlErr, orig)
		}
		if sqlapi.AsError(wrapped) != orig || api.AsError(wrapped) != orig {
			t.Fatal("AsError did not return identical pointer")
		}
	})

	t.Run("TransactionTimeLimitError identity across packages", func(t *testing.T) {
		t.Parallel()
		fromSQLAPI := sqlapi.NewTransactionTimeLimitError(4100*time.Millisecond, 4*time.Second)
		fromAPI := api.MarkFDBTransactionTooOld(errors.New("1007"))

		var ttlFromAPI *api.TransactionTimeLimitError
		if !errors.As(fromSQLAPI, &ttlFromAPI) || ttlFromAPI.Source != api.TimeLimitPreempted {
			t.Fatalf("errors.As(*api.TransactionTimeLimitError) failed: %v", fromSQLAPI)
		}
		if !api.IsTransactionTimeLimit(fromSQLAPI) {
			t.Fatal("api.IsTransactionTimeLimit false on sqlapi error")
		}

		var ttlFromSQLAPI *sqlapi.TransactionTimeLimitError
		if !errors.As(fromAPI, &ttlFromSQLAPI) || ttlFromSQLAPI.Source != sqlapi.TimeLimitFDBTooOld {
			t.Fatalf("errors.As(*sqlapi.TransactionTimeLimitError) failed: %v", fromAPI)
		}
		if !sqlapi.IsTransactionTimeLimit(fromAPI) {
			t.Fatal("sqlapi.IsTransactionTimeLimit false on api error")
		}
	})

	t.Run("DDL scope errors identity across packages", func(t *testing.T) {
		t.Parallel()
		cross := sqlapi.NewCrossDatabaseDDLError("DROP DATABASE", "/FRL/a", "/FRL/b")
		var apiCross *api.CrossDatabaseDDLError
		if !errors.As(cross, &apiCross) || apiCross.TargetDatabase != "/FRL/b" {
			t.Fatalf("errors.As(*api.CrossDatabaseDDLError) failed: %v", cross)
		}

		tmpl := api.NewSchemaTemplateDDLRestrictedError("DROP SCHEMA TEMPLATE", "/FRL/a")
		var sqlTmpl *sqlapi.SchemaTemplateDDLRestrictedError
		if !errors.As(tmpl, &sqlTmpl) || sqlTmpl.SessionDatabase != "/FRL/a" {
			t.Fatalf("errors.As(*sqlapi.SchemaTemplateDDLRestrictedError) failed: %v", tmpl)
		}
	})
}

func TestErrorCodeAliasesAndMethods(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		sql  sqlapi.ErrorCode
		api  api.ErrorCode
	}{
		{"ErrCodeSuccess", sqlapi.ErrCodeSuccess, api.ErrCodeSuccess},
		{"ErrCodeNoResultSet", sqlapi.ErrCodeNoResultSet, api.ErrCodeNoResultSet},
		{"ErrCodeUnableToEstablishSQLConnection", sqlapi.ErrCodeUnableToEstablishSQLConnection, api.ErrCodeUnableToEstablishSQLConnection},
		{"ErrCodeConnectionDoesNotExist", sqlapi.ErrCodeConnectionDoesNotExist, api.ErrCodeConnectionDoesNotExist},
		{"ErrCodeInvalidPath", sqlapi.ErrCodeInvalidPath, api.ErrCodeInvalidPath},
		{"ErrCodeCannotCommitRollbackWithAutocommit", sqlapi.ErrCodeCannotCommitRollbackWithAutocommit, api.ErrCodeCannotCommitRollbackWithAutocommit},
		{"ErrCodeUnsupportedOperation", sqlapi.ErrCodeUnsupportedOperation, api.ErrCodeUnsupportedOperation},
		{"ErrCodeUnsupportedQuery", sqlapi.ErrCodeUnsupportedQuery, api.ErrCodeUnsupportedQuery},
		{"ErrCodeUnsupportedSort", sqlapi.ErrCodeUnsupportedSort, api.ErrCodeUnsupportedSort},
		{"ErrCodeCardinalityViolation", sqlapi.ErrCodeCardinalityViolation, api.ErrCodeCardinalityViolation},
		{"ErrCodeCannotConvertType", sqlapi.ErrCodeCannotConvertType, api.ErrCodeCannotConvertType},
		{"ErrCodeNumericValueOutOfRange", sqlapi.ErrCodeNumericValueOutOfRange, api.ErrCodeNumericValueOutOfRange},
		{"ErrCodeDatetimeFieldOverflow", sqlapi.ErrCodeDatetimeFieldOverflow, api.ErrCodeDatetimeFieldOverflow},
		{"ErrCodeDivisionByZero", sqlapi.ErrCodeDivisionByZero, api.ErrCodeDivisionByZero},
		{"ErrCodeInvalidRowCountInLimitClause", sqlapi.ErrCodeInvalidRowCountInLimitClause, api.ErrCodeInvalidRowCountInLimitClause},
		{"ErrCodeEscapeCharacterConflict", sqlapi.ErrCodeEscapeCharacterConflict, api.ErrCodeEscapeCharacterConflict},
		{"ErrCodeInvalidEscapeCharacter", sqlapi.ErrCodeInvalidEscapeCharacter, api.ErrCodeInvalidEscapeCharacter},
		{"ErrCodeCharacterNotInRepertoire", sqlapi.ErrCodeCharacterNotInRepertoire, api.ErrCodeCharacterNotInRepertoire},
		{"ErrCodeInvalidParameter", sqlapi.ErrCodeInvalidParameter, api.ErrCodeInvalidParameter},
		{"ErrCodeInvalidEscapeSequence", sqlapi.ErrCodeInvalidEscapeSequence, api.ErrCodeInvalidEscapeSequence},
		{"ErrCodeArrayElementError", sqlapi.ErrCodeArrayElementError, api.ErrCodeArrayElementError},
		{"ErrCodeInvalidBinaryRepresentation", sqlapi.ErrCodeInvalidBinaryRepresentation, api.ErrCodeInvalidBinaryRepresentation},
		{"ErrCodeInvalidArgumentForFunction", sqlapi.ErrCodeInvalidArgumentForFunction, api.ErrCodeInvalidArgumentForFunction},
		{"ErrCodeInvalidCast", sqlapi.ErrCodeInvalidCast, api.ErrCodeInvalidCast},
		{"ErrCodeCopySerializationError", sqlapi.ErrCodeCopySerializationError, api.ErrCodeCopySerializationError},
		{"ErrCodeCopyImportValidationError", sqlapi.ErrCodeCopyImportValidationError, api.ErrCodeCopyImportValidationError},
		{"ErrCodeNotNullViolation", sqlapi.ErrCodeNotNullViolation, api.ErrCodeNotNullViolation},
		{"ErrCodeUniqueConstraintViolation", sqlapi.ErrCodeUniqueConstraintViolation, api.ErrCodeUniqueConstraintViolation},
		{"ErrCodeInvalidCursorState", sqlapi.ErrCodeInvalidCursorState, api.ErrCodeInvalidCursorState},
		{"ErrCodeInvalidContinuation", sqlapi.ErrCodeInvalidContinuation, api.ErrCodeInvalidContinuation},
		{"ErrCodeInvalidTransactionState", sqlapi.ErrCodeInvalidTransactionState, api.ErrCodeInvalidTransactionState},
		{"ErrCodeTransactionInactive", sqlapi.ErrCodeTransactionInactive, api.ErrCodeTransactionInactive},
		{"ErrCodeSerializationFailure", sqlapi.ErrCodeSerializationFailure, api.ErrCodeSerializationFailure},
		{"ErrCodeStatementCompletionUnknown", sqlapi.ErrCodeStatementCompletionUnknown, api.ErrCodeStatementCompletionUnknown},
		{"ErrCodeSyntaxOrAccessViolation", sqlapi.ErrCodeSyntaxOrAccessViolation, api.ErrCodeSyntaxOrAccessViolation},
		{"ErrCodeInsufficientPrivilege", sqlapi.ErrCodeInsufficientPrivilege, api.ErrCodeInsufficientPrivilege},
		{"ErrCodeSyntaxError", sqlapi.ErrCodeSyntaxError, api.ErrCodeSyntaxError},
		{"ErrCodeInvalidName", sqlapi.ErrCodeInvalidName, api.ErrCodeInvalidName},
		{"ErrCodeColumnAlreadyExists", sqlapi.ErrCodeColumnAlreadyExists, api.ErrCodeColumnAlreadyExists},
		{"ErrCodeAmbiguousColumn", sqlapi.ErrCodeAmbiguousColumn, api.ErrCodeAmbiguousColumn},
		{"ErrCodeUndefinedColumn", sqlapi.ErrCodeUndefinedColumn, api.ErrCodeUndefinedColumn},
		{"ErrCodeDuplicateAlias", sqlapi.ErrCodeDuplicateAlias, api.ErrCodeDuplicateAlias},
		{"ErrCodeDuplicateFunction", sqlapi.ErrCodeDuplicateFunction, api.ErrCodeDuplicateFunction},
		{"ErrCodeGroupingError", sqlapi.ErrCodeGroupingError, api.ErrCodeGroupingError},
		{"ErrCodeDatatypeMismatch", sqlapi.ErrCodeDatatypeMismatch, api.ErrCodeDatatypeMismatch},
		{"ErrCodeWrongObjectType", sqlapi.ErrCodeWrongObjectType, api.ErrCodeWrongObjectType},
		{"ErrCodeUndefinedFunction", sqlapi.ErrCodeUndefinedFunction, api.ErrCodeUndefinedFunction},
		{"ErrCodeUndefinedDatabase", sqlapi.ErrCodeUndefinedDatabase, api.ErrCodeUndefinedDatabase},
		{"ErrCodeUndefinedTable", sqlapi.ErrCodeUndefinedTable, api.ErrCodeUndefinedTable},
		{"ErrCodeUndefinedParameter", sqlapi.ErrCodeUndefinedParameter, api.ErrCodeUndefinedParameter},
		{"ErrCodeDatabaseAlreadyExists", sqlapi.ErrCodeDatabaseAlreadyExists, api.ErrCodeDatabaseAlreadyExists},
		{"ErrCodeSchemaAlreadyExists", sqlapi.ErrCodeSchemaAlreadyExists, api.ErrCodeSchemaAlreadyExists},
		{"ErrCodeTableAlreadyExists", sqlapi.ErrCodeTableAlreadyExists, api.ErrCodeTableAlreadyExists},
		{"ErrCodeInvalidColumnReference", sqlapi.ErrCodeInvalidColumnReference, api.ErrCodeInvalidColumnReference},
		{"ErrCodeInvalidFunctionDefinition", sqlapi.ErrCodeInvalidFunctionDefinition, api.ErrCodeInvalidFunctionDefinition},
		{"ErrCodeInvalidTableDefinition", sqlapi.ErrCodeInvalidTableDefinition, api.ErrCodeInvalidTableDefinition},
		{"ErrCodeUnknownType", sqlapi.ErrCodeUnknownType, api.ErrCodeUnknownType},
		{"ErrCodeInvalidRecursion", sqlapi.ErrCodeInvalidRecursion, api.ErrCodeInvalidRecursion},
		{"ErrCodeIncompatibleTableAlias", sqlapi.ErrCodeIncompatibleTableAlias, api.ErrCodeIncompatibleTableAlias},
		{"ErrCodeWindowingError", sqlapi.ErrCodeWindowingError, api.ErrCodeWindowingError},
		{"ErrCodeSchemaMappingAlreadyExists", sqlapi.ErrCodeSchemaMappingAlreadyExists, api.ErrCodeSchemaMappingAlreadyExists},
		{"ErrCodeUndefinedSchema", sqlapi.ErrCodeUndefinedSchema, api.ErrCodeUndefinedSchema},
		{"ErrCodeUndefinedIndex", sqlapi.ErrCodeUndefinedIndex, api.ErrCodeUndefinedIndex},
		{"ErrCodeUnknownSchemaTemplate", sqlapi.ErrCodeUnknownSchemaTemplate, api.ErrCodeUnknownSchemaTemplate},
		{"ErrCodeAnnotationAlreadyExists", sqlapi.ErrCodeAnnotationAlreadyExists, api.ErrCodeAnnotationAlreadyExists},
		{"ErrCodeIndexAlreadyExists", sqlapi.ErrCodeIndexAlreadyExists, api.ErrCodeIndexAlreadyExists},
		{"ErrCodeIncorrectMetadataTableVersion", sqlapi.ErrCodeIncorrectMetadataTableVersion, api.ErrCodeIncorrectMetadataTableVersion},
		{"ErrCodeInvalidSchemaTemplate", sqlapi.ErrCodeInvalidSchemaTemplate, api.ErrCodeInvalidSchemaTemplate},
		{"ErrCodeInvalidPreparedStatementParameter", sqlapi.ErrCodeInvalidPreparedStatementParameter, api.ErrCodeInvalidPreparedStatementParameter},
		{"ErrCodeExecuteUpdateReturnedResultSet", sqlapi.ErrCodeExecuteUpdateReturnedResultSet, api.ErrCodeExecuteUpdateReturnedResultSet},
		{"ErrCodeDuplicateSchemaTemplate", sqlapi.ErrCodeDuplicateSchemaTemplate, api.ErrCodeDuplicateSchemaTemplate},
		{"ErrCodeUnknownDatabase", sqlapi.ErrCodeUnknownDatabase, api.ErrCodeUnknownDatabase},
		{"ErrCodeUnionIncorrectColumnCount", sqlapi.ErrCodeUnionIncorrectColumnCount, api.ErrCodeUnionIncorrectColumnCount},
		{"ErrCodeUnionIncompatibleColumns", sqlapi.ErrCodeUnionIncompatibleColumns, api.ErrCodeUnionIncompatibleColumns},
		{"ErrCodeInvalidDatabase", sqlapi.ErrCodeInvalidDatabase, api.ErrCodeInvalidDatabase},
		{"ErrCodeTransactionTimeout", sqlapi.ErrCodeTransactionTimeout, api.ErrCodeTransactionTimeout},
		{"ErrCodeTooManyColumns", sqlapi.ErrCodeTooManyColumns, api.ErrCodeTooManyColumns},
		{"ErrCodeExecutionLimitReached", sqlapi.ErrCodeExecutionLimitReached, api.ErrCodeExecutionLimitReached},
		{"ErrCodePlanComplexityLimitReached", sqlapi.ErrCodePlanComplexityLimitReached, api.ErrCodePlanComplexityLimitReached},
		{"ErrCodeStatementClosed", sqlapi.ErrCodeStatementClosed, api.ErrCodeStatementClosed},
		{"ErrCodeUndefinedFile", sqlapi.ErrCodeUndefinedFile, api.ErrCodeUndefinedFile},
		{"ErrCodeUnknown", sqlapi.ErrCodeUnknown, api.ErrCodeUnknown},
		{"ErrCodeInternalError", sqlapi.ErrCodeInternalError, api.ErrCodeInternalError},
		{"ErrCodeDeserializationFailure", sqlapi.ErrCodeDeserializationFailure, api.ErrCodeDeserializationFailure},
	}
	for _, tc := range cases {
		if tc.sql != tc.api {
			t.Errorf("%s: sqlapi=%q != api=%q", tc.name, tc.sql, tc.api)
		}
		if got := sqlapi.ErrorCodeFromString(string(tc.sql)); got != tc.sql {
			t.Errorf("%s: sqlapi.ErrorCodeFromString = %q, want %q", tc.name, got, tc.sql)
		}
		if got := api.ErrorCodeFromString(string(tc.api)); got != tc.api {
			t.Errorf("%s: api.ErrorCodeFromString = %q, want %q", tc.name, got, tc.api)
		}
		if tc.sql.String() != string(tc.sql) {
			t.Errorf("%s: String() = %q", tc.name, tc.sql.String())
		}
		if tc.sql.Class() != string(tc.sql)[:2] {
			t.Errorf("%s: Class() = %q", tc.name, tc.sql.Class())
		}
	}
	if got := sqlapi.ErrorCodeFromString("NOPE"); got != sqlapi.ErrCodeUnknown {
		t.Errorf("unknown code fallback = %q, want %q", got, sqlapi.ErrCodeUnknown)
	}
	if got := sqlapi.ErrorCode("X").Class(); got != "" {
		t.Errorf("short code Class() = %q, want empty", got)
	}
}

type stubContinuation struct {
	state  []byte
	reason sqlapi.ContinuationReason
}

func (s *stubContinuation) Serialize() []byte {
	return s.state
}

func (s *stubContinuation) ExecutionState() []byte {
	return s.state
}

func (s *stubContinuation) Reason() sqlapi.ContinuationReason {
	return s.reason
}

func TestContinuationAliasIdentity(t *testing.T) {
	t.Parallel()

	var sc sqlapi.Continuation = &stubContinuation{state: nil, reason: sqlapi.ContinuationUserRequested}
	var ac api.Continuation = sc
	if !sqlapi.AtBeginning(ac) || !api.AtBeginning(sc) {
		t.Fatal("AtBeginning mismatch across aliases")
	}
	if sqlapi.AtEnd(ac) || api.AtEnd(sc) {
		t.Fatal("AtEnd should be false for nil state")
	}

	endCont := &stubContinuation{state: []byte{}, reason: api.ContinuationCursorAfterLast}
	if !sqlapi.AtEnd(endCont) || !api.AtEnd(endCont) {
		t.Fatal("AtEnd should be true for empty non-nil state")
	}
	if endCont.Reason().String() != "CURSOR_AFTER_LAST" {
		t.Fatalf("Reason().String() = %q", endCont.Reason().String())
	}
	if !strings.Contains(api.ContinuationTransactionLimitReached.String(), "TRANSACTION_LIMIT_REACHED") {
		t.Fatalf("unexpected reason string: %q", api.ContinuationTransactionLimitReached.String())
	}
}

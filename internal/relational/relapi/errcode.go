// Portions derived from FoundationDB Record Layer (ErrorCode.java,
// ExceptionUtil.java, FDBExceptions.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2024 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

// Package api mirrors Java's fdb-relational-api module.
//
// It defines the public interfaces and types for the relational (SQL)
// layer: Connection, Statement, ResultSet, DataType, Options, ErrorCode,
// and related metadata types. Implementations live under
// pkg/relational/core (embedded) and pkg/relational/sqldriver
// (database/sql driver adapter).
package api

import "fdb.dev/pkg/relational/sqlapi"

// ErrorCode is a 5-character SQLSTATE-formatted error code.
type ErrorCode = sqlapi.ErrorCode

// SQLSTATE error codes re-exported from sqlapi.
const (
	ErrCodeSuccess                            ErrorCode = sqlapi.ErrCodeSuccess
	ErrCodeNoResultSet                        ErrorCode = sqlapi.ErrCodeNoResultSet
	ErrCodeUnableToEstablishSQLConnection     ErrorCode = sqlapi.ErrCodeUnableToEstablishSQLConnection
	ErrCodeConnectionDoesNotExist             ErrorCode = sqlapi.ErrCodeConnectionDoesNotExist
	ErrCodeInvalidPath                        ErrorCode = sqlapi.ErrCodeInvalidPath
	ErrCodeCannotCommitRollbackWithAutocommit ErrorCode = sqlapi.ErrCodeCannotCommitRollbackWithAutocommit
	ErrCodeUnsupportedOperation               ErrorCode = sqlapi.ErrCodeUnsupportedOperation
	ErrCodeUnsupportedQuery                   ErrorCode = sqlapi.ErrCodeUnsupportedQuery
	ErrCodeUnsupportedSort                    ErrorCode = sqlapi.ErrCodeUnsupportedSort
	ErrCodeCardinalityViolation               ErrorCode = sqlapi.ErrCodeCardinalityViolation
	ErrCodeCannotConvertType                  ErrorCode = sqlapi.ErrCodeCannotConvertType
	ErrCodeNumericValueOutOfRange             ErrorCode = sqlapi.ErrCodeNumericValueOutOfRange
	ErrCodeDatetimeFieldOverflow              ErrorCode = sqlapi.ErrCodeDatetimeFieldOverflow
	ErrCodeDivisionByZero                     ErrorCode = sqlapi.ErrCodeDivisionByZero
	ErrCodeInvalidRowCountInLimitClause       ErrorCode = sqlapi.ErrCodeInvalidRowCountInLimitClause
	ErrCodeEscapeCharacterConflict            ErrorCode = sqlapi.ErrCodeEscapeCharacterConflict
	ErrCodeInvalidEscapeCharacter             ErrorCode = sqlapi.ErrCodeInvalidEscapeCharacter
	ErrCodeCharacterNotInRepertoire           ErrorCode = sqlapi.ErrCodeCharacterNotInRepertoire
	ErrCodeInvalidParameter                   ErrorCode = sqlapi.ErrCodeInvalidParameter
	ErrCodeInvalidEscapeSequence              ErrorCode = sqlapi.ErrCodeInvalidEscapeSequence
	ErrCodeArrayElementError                  ErrorCode = sqlapi.ErrCodeArrayElementError
	ErrCodeInvalidBinaryRepresentation        ErrorCode = sqlapi.ErrCodeInvalidBinaryRepresentation
	ErrCodeInvalidArgumentForFunction         ErrorCode = sqlapi.ErrCodeInvalidArgumentForFunction
	ErrCodeInvalidCast                        ErrorCode = sqlapi.ErrCodeInvalidCast
	ErrCodeCopySerializationError             ErrorCode = sqlapi.ErrCodeCopySerializationError
	ErrCodeCopyImportValidationError          ErrorCode = sqlapi.ErrCodeCopyImportValidationError
	ErrCodeNotNullViolation                   ErrorCode = sqlapi.ErrCodeNotNullViolation
	ErrCodeUniqueConstraintViolation          ErrorCode = sqlapi.ErrCodeUniqueConstraintViolation
	ErrCodeInvalidCursorState                 ErrorCode = sqlapi.ErrCodeInvalidCursorState
	ErrCodeInvalidContinuation                ErrorCode = sqlapi.ErrCodeInvalidContinuation
	ErrCodeInvalidTransactionState            ErrorCode = sqlapi.ErrCodeInvalidTransactionState
	ErrCodeTransactionInactive                ErrorCode = sqlapi.ErrCodeTransactionInactive
	ErrCodeSerializationFailure               ErrorCode = sqlapi.ErrCodeSerializationFailure
	ErrCodeStatementCompletionUnknown         ErrorCode = sqlapi.ErrCodeStatementCompletionUnknown
	ErrCodeSyntaxOrAccessViolation            ErrorCode = sqlapi.ErrCodeSyntaxOrAccessViolation
	ErrCodeInsufficientPrivilege              ErrorCode = sqlapi.ErrCodeInsufficientPrivilege
	ErrCodeSyntaxError                        ErrorCode = sqlapi.ErrCodeSyntaxError
	ErrCodeInvalidName                        ErrorCode = sqlapi.ErrCodeInvalidName
	ErrCodeColumnAlreadyExists                ErrorCode = sqlapi.ErrCodeColumnAlreadyExists
	ErrCodeAmbiguousColumn                    ErrorCode = sqlapi.ErrCodeAmbiguousColumn
	ErrCodeUndefinedColumn                    ErrorCode = sqlapi.ErrCodeUndefinedColumn
	ErrCodeDuplicateAlias                     ErrorCode = sqlapi.ErrCodeDuplicateAlias
	ErrCodeDuplicateFunction                  ErrorCode = sqlapi.ErrCodeDuplicateFunction
	ErrCodeGroupingError                      ErrorCode = sqlapi.ErrCodeGroupingError
	ErrCodeDatatypeMismatch                   ErrorCode = sqlapi.ErrCodeDatatypeMismatch
	ErrCodeWrongObjectType                    ErrorCode = sqlapi.ErrCodeWrongObjectType
	ErrCodeUndefinedFunction                  ErrorCode = sqlapi.ErrCodeUndefinedFunction
	ErrCodeUndefinedDatabase                  ErrorCode = sqlapi.ErrCodeUndefinedDatabase
	ErrCodeUndefinedTable                     ErrorCode = sqlapi.ErrCodeUndefinedTable
	ErrCodeUndefinedParameter                 ErrorCode = sqlapi.ErrCodeUndefinedParameter
	ErrCodeDatabaseAlreadyExists              ErrorCode = sqlapi.ErrCodeDatabaseAlreadyExists
	ErrCodeSchemaAlreadyExists                ErrorCode = sqlapi.ErrCodeSchemaAlreadyExists
	ErrCodeTableAlreadyExists                 ErrorCode = sqlapi.ErrCodeTableAlreadyExists
	ErrCodeInvalidColumnReference             ErrorCode = sqlapi.ErrCodeInvalidColumnReference
	ErrCodeInvalidFunctionDefinition          ErrorCode = sqlapi.ErrCodeInvalidFunctionDefinition
	ErrCodeInvalidTableDefinition             ErrorCode = sqlapi.ErrCodeInvalidTableDefinition
	ErrCodeUnknownType                        ErrorCode = sqlapi.ErrCodeUnknownType
	ErrCodeInvalidRecursion                   ErrorCode = sqlapi.ErrCodeInvalidRecursion
	ErrCodeIncompatibleTableAlias             ErrorCode = sqlapi.ErrCodeIncompatibleTableAlias
	ErrCodeWindowingError                     ErrorCode = sqlapi.ErrCodeWindowingError
	ErrCodeSchemaMappingAlreadyExists         ErrorCode = sqlapi.ErrCodeSchemaMappingAlreadyExists
	ErrCodeUndefinedSchema                    ErrorCode = sqlapi.ErrCodeUndefinedSchema
	ErrCodeUndefinedIndex                     ErrorCode = sqlapi.ErrCodeUndefinedIndex
	ErrCodeUnknownSchemaTemplate              ErrorCode = sqlapi.ErrCodeUnknownSchemaTemplate
	ErrCodeAnnotationAlreadyExists            ErrorCode = sqlapi.ErrCodeAnnotationAlreadyExists
	ErrCodeIndexAlreadyExists                 ErrorCode = sqlapi.ErrCodeIndexAlreadyExists
	ErrCodeIncorrectMetadataTableVersion      ErrorCode = sqlapi.ErrCodeIncorrectMetadataTableVersion
	ErrCodeInvalidSchemaTemplate              ErrorCode = sqlapi.ErrCodeInvalidSchemaTemplate
	ErrCodeInvalidPreparedStatementParameter  ErrorCode = sqlapi.ErrCodeInvalidPreparedStatementParameter
	ErrCodeExecuteUpdateReturnedResultSet     ErrorCode = sqlapi.ErrCodeExecuteUpdateReturnedResultSet
	ErrCodeDuplicateSchemaTemplate            ErrorCode = sqlapi.ErrCodeDuplicateSchemaTemplate
	ErrCodeUnknownDatabase                    ErrorCode = sqlapi.ErrCodeUnknownDatabase
	ErrCodeUnionIncorrectColumnCount          ErrorCode = sqlapi.ErrCodeUnionIncorrectColumnCount
	ErrCodeUnionIncompatibleColumns           ErrorCode = sqlapi.ErrCodeUnionIncompatibleColumns
	ErrCodeInvalidDatabase                    ErrorCode = sqlapi.ErrCodeInvalidDatabase
	ErrCodeTransactionTimeout                 ErrorCode = sqlapi.ErrCodeTransactionTimeout
	ErrCodeTooManyColumns                     ErrorCode = sqlapi.ErrCodeTooManyColumns
	ErrCodeExecutionLimitReached              ErrorCode = sqlapi.ErrCodeExecutionLimitReached
	ErrCodePlanComplexityLimitReached         ErrorCode = sqlapi.ErrCodePlanComplexityLimitReached
	ErrCodeStatementClosed                    ErrorCode = sqlapi.ErrCodeStatementClosed
	ErrCodeUndefinedFile                      ErrorCode = sqlapi.ErrCodeUndefinedFile
	ErrCodeUnknown                            ErrorCode = sqlapi.ErrCodeUnknown
	ErrCodeInternalError                      ErrorCode = sqlapi.ErrCodeInternalError
	ErrCodeDeserializationFailure             ErrorCode = sqlapi.ErrCodeDeserializationFailure
)

// ErrorCodeFromString looks up an ErrorCode by its 5-character SQLSTATE string.
func ErrorCodeFromString(s string) ErrorCode {
	return sqlapi.ErrorCodeFromString(s)
}

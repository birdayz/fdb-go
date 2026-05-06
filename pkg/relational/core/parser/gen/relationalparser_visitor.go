// Code generated from RelationalParser.g4 by ANTLR 4.13.1. DO NOT EDIT.

package antlrgen // RelationalParser
import "github.com/antlr4-go/antlr/v4"

// A complete Visitor for a parse tree produced by RelationalParser.
type RelationalParserVisitor interface {
	antlr.ParseTreeVisitor

	// Visit a parse tree produced by RelationalParser#root.
	VisitRoot(ctx *RootContext) interface{}

	// Visit a parse tree produced by RelationalParser#statements.
	VisitStatements(ctx *StatementsContext) interface{}

	// Visit a parse tree produced by RelationalParser#statement.
	VisitStatement(ctx *StatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#dmlStatement.
	VisitDmlStatement(ctx *DmlStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#ddlStatement.
	VisitDdlStatement(ctx *DdlStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#transactionStatement.
	VisitTransactionStatement(ctx *TransactionStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#preparedStatement.
	VisitPreparedStatement(ctx *PreparedStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#administrationStatement.
	VisitAdministrationStatement(ctx *AdministrationStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#utilityStatement.
	VisitUtilityStatement(ctx *UtilityStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#templateClause.
	VisitTemplateClause(ctx *TemplateClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#createSchemaStatement.
	VisitCreateSchemaStatement(ctx *CreateSchemaStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#createSchemaTemplateStatement.
	VisitCreateSchemaTemplateStatement(ctx *CreateSchemaTemplateStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#createDatabaseStatement.
	VisitCreateDatabaseStatement(ctx *CreateDatabaseStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#optionsClause.
	VisitOptionsClause(ctx *OptionsClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#option.
	VisitOption(ctx *OptionContext) interface{}

	// Visit a parse tree produced by RelationalParser#dropDatabaseStatement.
	VisitDropDatabaseStatement(ctx *DropDatabaseStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#dropSchemaTemplateStatement.
	VisitDropSchemaTemplateStatement(ctx *DropSchemaTemplateStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#dropSchemaStatement.
	VisitDropSchemaStatement(ctx *DropSchemaStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#structDefinition.
	VisitStructDefinition(ctx *StructDefinitionContext) interface{}

	// Visit a parse tree produced by RelationalParser#tableDefinition.
	VisitTableDefinition(ctx *TableDefinitionContext) interface{}

	// Visit a parse tree produced by RelationalParser#columnDefinition.
	VisitColumnDefinition(ctx *ColumnDefinitionContext) interface{}

	// Visit a parse tree produced by RelationalParser#functionColumnType.
	VisitFunctionColumnType(ctx *FunctionColumnTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#columnType.
	VisitColumnType(ctx *ColumnTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#primitiveType.
	VisitPrimitiveType(ctx *PrimitiveTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#vectorType.
	VisitVectorType(ctx *VectorTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#vectorElementType.
	VisitVectorElementType(ctx *VectorElementTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#nullColumnConstraint.
	VisitNullColumnConstraint(ctx *NullColumnConstraintContext) interface{}

	// Visit a parse tree produced by RelationalParser#primaryKeyDefinition.
	VisitPrimaryKeyDefinition(ctx *PrimaryKeyDefinitionContext) interface{}

	// Visit a parse tree produced by RelationalParser#fullIdList.
	VisitFullIdList(ctx *FullIdListContext) interface{}

	// Visit a parse tree produced by RelationalParser#enumDefinition.
	VisitEnumDefinition(ctx *EnumDefinitionContext) interface{}

	// Visit a parse tree produced by RelationalParser#indexAsSelectDefinition.
	VisitIndexAsSelectDefinition(ctx *IndexAsSelectDefinitionContext) interface{}

	// Visit a parse tree produced by RelationalParser#indexOnSourceDefinition.
	VisitIndexOnSourceDefinition(ctx *IndexOnSourceDefinitionContext) interface{}

	// Visit a parse tree produced by RelationalParser#vectorIndexDefinition.
	VisitVectorIndexDefinition(ctx *VectorIndexDefinitionContext) interface{}

	// Visit a parse tree produced by RelationalParser#indexColumnList.
	VisitIndexColumnList(ctx *IndexColumnListContext) interface{}

	// Visit a parse tree produced by RelationalParser#indexColumnSpec.
	VisitIndexColumnSpec(ctx *IndexColumnSpecContext) interface{}

	// Visit a parse tree produced by RelationalParser#includeClause.
	VisitIncludeClause(ctx *IncludeClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#indexType.
	VisitIndexType(ctx *IndexTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#indexPartitionClause.
	VisitIndexPartitionClause(ctx *IndexPartitionClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#indexOptions.
	VisitIndexOptions(ctx *IndexOptionsContext) interface{}

	// Visit a parse tree produced by RelationalParser#indexOption.
	VisitIndexOption(ctx *IndexOptionContext) interface{}

	// Visit a parse tree produced by RelationalParser#vectorIndexOptions.
	VisitVectorIndexOptions(ctx *VectorIndexOptionsContext) interface{}

	// Visit a parse tree produced by RelationalParser#vectorIndexOption.
	VisitVectorIndexOption(ctx *VectorIndexOptionContext) interface{}

	// Visit a parse tree produced by RelationalParser#hnswMetric.
	VisitHnswMetric(ctx *HnswMetricContext) interface{}

	// Visit a parse tree produced by RelationalParser#indexAttributes.
	VisitIndexAttributes(ctx *IndexAttributesContext) interface{}

	// Visit a parse tree produced by RelationalParser#indexAttribute.
	VisitIndexAttribute(ctx *IndexAttributeContext) interface{}

	// Visit a parse tree produced by RelationalParser#createTempFunction.
	VisitCreateTempFunction(ctx *CreateTempFunctionContext) interface{}

	// Visit a parse tree produced by RelationalParser#dropTempFunction.
	VisitDropTempFunction(ctx *DropTempFunctionContext) interface{}

	// Visit a parse tree produced by RelationalParser#viewDefinition.
	VisitViewDefinition(ctx *ViewDefinitionContext) interface{}

	// Visit a parse tree produced by RelationalParser#tempSqlInvokedFunction.
	VisitTempSqlInvokedFunction(ctx *TempSqlInvokedFunctionContext) interface{}

	// Visit a parse tree produced by RelationalParser#sqlInvokedFunction.
	VisitSqlInvokedFunction(ctx *SqlInvokedFunctionContext) interface{}

	// Visit a parse tree produced by RelationalParser#functionSpecification.
	VisitFunctionSpecification(ctx *FunctionSpecificationContext) interface{}

	// Visit a parse tree produced by RelationalParser#sqlParameterDeclarationList.
	VisitSqlParameterDeclarationList(ctx *SqlParameterDeclarationListContext) interface{}

	// Visit a parse tree produced by RelationalParser#sqlParameterDeclarations.
	VisitSqlParameterDeclarations(ctx *SqlParameterDeclarationsContext) interface{}

	// Visit a parse tree produced by RelationalParser#sqlParameterDeclaration.
	VisitSqlParameterDeclaration(ctx *SqlParameterDeclarationContext) interface{}

	// Visit a parse tree produced by RelationalParser#parameterMode.
	VisitParameterMode(ctx *ParameterModeContext) interface{}

	// Visit a parse tree produced by RelationalParser#returnsClause.
	VisitReturnsClause(ctx *ReturnsClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#returnsType.
	VisitReturnsType(ctx *ReturnsTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#returnsTableType.
	VisitReturnsTableType(ctx *ReturnsTableTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#tableFunctionColumnList.
	VisitTableFunctionColumnList(ctx *TableFunctionColumnListContext) interface{}

	// Visit a parse tree produced by RelationalParser#tableFunctionColumnListElement.
	VisitTableFunctionColumnListElement(ctx *TableFunctionColumnListElementContext) interface{}

	// Visit a parse tree produced by RelationalParser#routineCharacteristics.
	VisitRoutineCharacteristics(ctx *RoutineCharacteristicsContext) interface{}

	// Visit a parse tree produced by RelationalParser#languageClause.
	VisitLanguageClause(ctx *LanguageClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#languageName.
	VisitLanguageName(ctx *LanguageNameContext) interface{}

	// Visit a parse tree produced by RelationalParser#parameterStyle.
	VisitParameterStyle(ctx *ParameterStyleContext) interface{}

	// Visit a parse tree produced by RelationalParser#deterministicCharacteristic.
	VisitDeterministicCharacteristic(ctx *DeterministicCharacteristicContext) interface{}

	// Visit a parse tree produced by RelationalParser#nullCallClause.
	VisitNullCallClause(ctx *NullCallClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#dispatchClause.
	VisitDispatchClause(ctx *DispatchClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#statementBody.
	VisitStatementBody(ctx *StatementBodyContext) interface{}

	// Visit a parse tree produced by RelationalParser#userDefinedScalarFunctionStatementBody.
	VisitUserDefinedScalarFunctionStatementBody(ctx *UserDefinedScalarFunctionStatementBodyContext) interface{}

	// Visit a parse tree produced by RelationalParser#expressionBody.
	VisitExpressionBody(ctx *ExpressionBodyContext) interface{}

	// Visit a parse tree produced by RelationalParser#sqlReturnStatement.
	VisitSqlReturnStatement(ctx *SqlReturnStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#returnValue.
	VisitReturnValue(ctx *ReturnValueContext) interface{}

	// Visit a parse tree produced by RelationalParser#charSet.
	VisitCharSet(ctx *CharSetContext) interface{}

	// Visit a parse tree produced by RelationalParser#intervalType.
	VisitIntervalType(ctx *IntervalTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#schemaId.
	VisitSchemaId(ctx *SchemaIdContext) interface{}

	// Visit a parse tree produced by RelationalParser#path.
	VisitPath(ctx *PathContext) interface{}

	// Visit a parse tree produced by RelationalParser#schemaTemplateId.
	VisitSchemaTemplateId(ctx *SchemaTemplateIdContext) interface{}

	// Visit a parse tree produced by RelationalParser#deleteStatement.
	VisitDeleteStatement(ctx *DeleteStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#insertStatement.
	VisitInsertStatement(ctx *InsertStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#continuationAtom.
	VisitContinuationAtom(ctx *ContinuationAtomContext) interface{}

	// Visit a parse tree produced by RelationalParser#selectStatement.
	VisitSelectStatement(ctx *SelectStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#query.
	VisitQuery(ctx *QueryContext) interface{}

	// Visit a parse tree produced by RelationalParser#ctes.
	VisitCtes(ctx *CtesContext) interface{}

	// Visit a parse tree produced by RelationalParser#traversalOrderClause.
	VisitTraversalOrderClause(ctx *TraversalOrderClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#namedQuery.
	VisitNamedQuery(ctx *NamedQueryContext) interface{}

	// Visit a parse tree produced by RelationalParser#tableFunction.
	VisitTableFunction(ctx *TableFunctionContext) interface{}

	// Visit a parse tree produced by RelationalParser#tableFunctionArgs.
	VisitTableFunctionArgs(ctx *TableFunctionArgsContext) interface{}

	// Visit a parse tree produced by RelationalParser#tableFunctionName.
	VisitTableFunctionName(ctx *TableFunctionNameContext) interface{}

	// Visit a parse tree produced by RelationalParser#queryTermDefault.
	VisitQueryTermDefault(ctx *QueryTermDefaultContext) interface{}

	// Visit a parse tree produced by RelationalParser#setQuery.
	VisitSetQuery(ctx *SetQueryContext) interface{}

	// Visit a parse tree produced by RelationalParser#insertStatementValueSelect.
	VisitInsertStatementValueSelect(ctx *InsertStatementValueSelectContext) interface{}

	// Visit a parse tree produced by RelationalParser#insertStatementValueValues.
	VisitInsertStatementValueValues(ctx *InsertStatementValueValuesContext) interface{}

	// Visit a parse tree produced by RelationalParser#updatedElement.
	VisitUpdatedElement(ctx *UpdatedElementContext) interface{}

	// Visit a parse tree produced by RelationalParser#assignmentField.
	VisitAssignmentField(ctx *AssignmentFieldContext) interface{}

	// Visit a parse tree produced by RelationalParser#updateStatement.
	VisitUpdateStatement(ctx *UpdateStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#orderByClause.
	VisitOrderByClause(ctx *OrderByClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#orderByExpression.
	VisitOrderByExpression(ctx *OrderByExpressionContext) interface{}

	// Visit a parse tree produced by RelationalParser#orderClause.
	VisitOrderClause(ctx *OrderClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#tableSources.
	VisitTableSources(ctx *TableSourcesContext) interface{}

	// Visit a parse tree produced by RelationalParser#tableSourceBase.
	VisitTableSourceBase(ctx *TableSourceBaseContext) interface{}

	// Visit a parse tree produced by RelationalParser#atomTableItem.
	VisitAtomTableItem(ctx *AtomTableItemContext) interface{}

	// Visit a parse tree produced by RelationalParser#subqueryTableItem.
	VisitSubqueryTableItem(ctx *SubqueryTableItemContext) interface{}

	// Visit a parse tree produced by RelationalParser#inlineTableItem.
	VisitInlineTableItem(ctx *InlineTableItemContext) interface{}

	// Visit a parse tree produced by RelationalParser#tableValuedFunction.
	VisitTableValuedFunction(ctx *TableValuedFunctionContext) interface{}

	// Visit a parse tree produced by RelationalParser#indexHint.
	VisitIndexHint(ctx *IndexHintContext) interface{}

	// Visit a parse tree produced by RelationalParser#indexHintType.
	VisitIndexHintType(ctx *IndexHintTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#inlineTableDefinition.
	VisitInlineTableDefinition(ctx *InlineTableDefinitionContext) interface{}

	// Visit a parse tree produced by RelationalParser#innerJoin.
	VisitInnerJoin(ctx *InnerJoinContext) interface{}

	// Visit a parse tree produced by RelationalParser#straightJoin.
	VisitStraightJoin(ctx *StraightJoinContext) interface{}

	// Visit a parse tree produced by RelationalParser#outerJoin.
	VisitOuterJoin(ctx *OuterJoinContext) interface{}

	// Visit a parse tree produced by RelationalParser#naturalJoin.
	VisitNaturalJoin(ctx *NaturalJoinContext) interface{}

	// Visit a parse tree produced by RelationalParser#simpleTable.
	VisitSimpleTable(ctx *SimpleTableContext) interface{}

	// Visit a parse tree produced by RelationalParser#parenthesisQuery.
	VisitParenthesisQuery(ctx *ParenthesisQueryContext) interface{}

	// Visit a parse tree produced by RelationalParser#selectElements.
	VisitSelectElements(ctx *SelectElementsContext) interface{}

	// Visit a parse tree produced by RelationalParser#selectStarElement.
	VisitSelectStarElement(ctx *SelectStarElementContext) interface{}

	// Visit a parse tree produced by RelationalParser#selectQualifierStarElement.
	VisitSelectQualifierStarElement(ctx *SelectQualifierStarElementContext) interface{}

	// Visit a parse tree produced by RelationalParser#selectExpressionElement.
	VisitSelectExpressionElement(ctx *SelectExpressionElementContext) interface{}

	// Visit a parse tree produced by RelationalParser#fromClause.
	VisitFromClause(ctx *FromClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#groupByClause.
	VisitGroupByClause(ctx *GroupByClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#whereExpr.
	VisitWhereExpr(ctx *WhereExprContext) interface{}

	// Visit a parse tree produced by RelationalParser#havingClause.
	VisitHavingClause(ctx *HavingClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#qualifyClause.
	VisitQualifyClause(ctx *QualifyClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#groupByItem.
	VisitGroupByItem(ctx *GroupByItemContext) interface{}

	// Visit a parse tree produced by RelationalParser#limitClause.
	VisitLimitClause(ctx *LimitClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#limitClauseAtom.
	VisitLimitClauseAtom(ctx *LimitClauseAtomContext) interface{}

	// Visit a parse tree produced by RelationalParser#queryOptions.
	VisitQueryOptions(ctx *QueryOptionsContext) interface{}

	// Visit a parse tree produced by RelationalParser#queryOption.
	VisitQueryOption(ctx *QueryOptionContext) interface{}

	// Visit a parse tree produced by RelationalParser#startTransaction.
	VisitStartTransaction(ctx *StartTransactionContext) interface{}

	// Visit a parse tree produced by RelationalParser#commitStatement.
	VisitCommitStatement(ctx *CommitStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#rollbackStatement.
	VisitRollbackStatement(ctx *RollbackStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#setAutocommitStatement.
	VisitSetAutocommitStatement(ctx *SetAutocommitStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#setTransactionStatement.
	VisitSetTransactionStatement(ctx *SetTransactionStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#transactionOption.
	VisitTransactionOption(ctx *TransactionOptionContext) interface{}

	// Visit a parse tree produced by RelationalParser#transactionLevel.
	VisitTransactionLevel(ctx *TransactionLevelContext) interface{}

	// Visit a parse tree produced by RelationalParser#prepareStatement.
	VisitPrepareStatement(ctx *PrepareStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#executeStatement.
	VisitExecuteStatement(ctx *ExecuteStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#showDatabasesStatement.
	VisitShowDatabasesStatement(ctx *ShowDatabasesStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#showSchemaTemplatesStatement.
	VisitShowSchemaTemplatesStatement(ctx *ShowSchemaTemplatesStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#setVariable.
	VisitSetVariable(ctx *SetVariableContext) interface{}

	// Visit a parse tree produced by RelationalParser#setCharset.
	VisitSetCharset(ctx *SetCharsetContext) interface{}

	// Visit a parse tree produced by RelationalParser#setNames.
	VisitSetNames(ctx *SetNamesContext) interface{}

	// Visit a parse tree produced by RelationalParser#setTransaction.
	VisitSetTransaction(ctx *SetTransactionContext) interface{}

	// Visit a parse tree produced by RelationalParser#setAutocommit.
	VisitSetAutocommit(ctx *SetAutocommitContext) interface{}

	// Visit a parse tree produced by RelationalParser#setNewValueInsideTrigger.
	VisitSetNewValueInsideTrigger(ctx *SetNewValueInsideTriggerContext) interface{}

	// Visit a parse tree produced by RelationalParser#variableClause.
	VisitVariableClause(ctx *VariableClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#killStatement.
	VisitKillStatement(ctx *KillStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#resetStatement.
	VisitResetStatement(ctx *ResetStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#executeContinuationStatement.
	VisitExecuteContinuationStatement(ctx *ExecuteContinuationStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#copyExportStatement.
	VisitCopyExportStatement(ctx *CopyExportStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#copyImportStatement.
	VisitCopyImportStatement(ctx *CopyImportStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#tableIndexes.
	VisitTableIndexes(ctx *TableIndexesContext) interface{}

	// Visit a parse tree produced by RelationalParser#loadedTableIndexes.
	VisitLoadedTableIndexes(ctx *LoadedTableIndexesContext) interface{}

	// Visit a parse tree produced by RelationalParser#simpleDescribeSchemaStatement.
	VisitSimpleDescribeSchemaStatement(ctx *SimpleDescribeSchemaStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#simpleDescribeSchemaTemplateStatement.
	VisitSimpleDescribeSchemaTemplateStatement(ctx *SimpleDescribeSchemaTemplateStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#fullDescribeStatement.
	VisitFullDescribeStatement(ctx *FullDescribeStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#helpStatement.
	VisitHelpStatement(ctx *HelpStatementContext) interface{}

	// Visit a parse tree produced by RelationalParser#describeStatements.
	VisitDescribeStatements(ctx *DescribeStatementsContext) interface{}

	// Visit a parse tree produced by RelationalParser#describeConnection.
	VisitDescribeConnection(ctx *DescribeConnectionContext) interface{}

	// Visit a parse tree produced by RelationalParser#fullId.
	VisitFullId(ctx *FullIdContext) interface{}

	// Visit a parse tree produced by RelationalParser#tableName.
	VisitTableName(ctx *TableNameContext) interface{}

	// Visit a parse tree produced by RelationalParser#fullColumnName.
	VisitFullColumnName(ctx *FullColumnNameContext) interface{}

	// Visit a parse tree produced by RelationalParser#indexColumnName.
	VisitIndexColumnName(ctx *IndexColumnNameContext) interface{}

	// Visit a parse tree produced by RelationalParser#charsetName.
	VisitCharsetName(ctx *CharsetNameContext) interface{}

	// Visit a parse tree produced by RelationalParser#collationName.
	VisitCollationName(ctx *CollationNameContext) interface{}

	// Visit a parse tree produced by RelationalParser#uid.
	VisitUid(ctx *UidContext) interface{}

	// Visit a parse tree produced by RelationalParser#simpleId.
	VisitSimpleId(ctx *SimpleIdContext) interface{}

	// Visit a parse tree produced by RelationalParser#nullNotnull.
	VisitNullNotnull(ctx *NullNotnullContext) interface{}

	// Visit a parse tree produced by RelationalParser#decimalLiteral.
	VisitDecimalLiteral(ctx *DecimalLiteralContext) interface{}

	// Visit a parse tree produced by RelationalParser#stringLiteral.
	VisitStringLiteral(ctx *StringLiteralContext) interface{}

	// Visit a parse tree produced by RelationalParser#booleanLiteral.
	VisitBooleanLiteral(ctx *BooleanLiteralContext) interface{}

	// Visit a parse tree produced by RelationalParser#bytesLiteral.
	VisitBytesLiteral(ctx *BytesLiteralContext) interface{}

	// Visit a parse tree produced by RelationalParser#nullLiteral.
	VisitNullLiteral(ctx *NullLiteralContext) interface{}

	// Visit a parse tree produced by RelationalParser#stringConstant.
	VisitStringConstant(ctx *StringConstantContext) interface{}

	// Visit a parse tree produced by RelationalParser#decimalConstant.
	VisitDecimalConstant(ctx *DecimalConstantContext) interface{}

	// Visit a parse tree produced by RelationalParser#negativeDecimalConstant.
	VisitNegativeDecimalConstant(ctx *NegativeDecimalConstantContext) interface{}

	// Visit a parse tree produced by RelationalParser#bytesConstant.
	VisitBytesConstant(ctx *BytesConstantContext) interface{}

	// Visit a parse tree produced by RelationalParser#booleanConstant.
	VisitBooleanConstant(ctx *BooleanConstantContext) interface{}

	// Visit a parse tree produced by RelationalParser#bitStringConstant.
	VisitBitStringConstant(ctx *BitStringConstantContext) interface{}

	// Visit a parse tree produced by RelationalParser#nullConstant.
	VisitNullConstant(ctx *NullConstantContext) interface{}

	// Visit a parse tree produced by RelationalParser#stringDataType.
	VisitStringDataType(ctx *StringDataTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#nationalStringDataType.
	VisitNationalStringDataType(ctx *NationalStringDataTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#nationalVaryingStringDataType.
	VisitNationalVaryingStringDataType(ctx *NationalVaryingStringDataTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#dimensionDataType.
	VisitDimensionDataType(ctx *DimensionDataTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#simpleDataType.
	VisitSimpleDataType(ctx *SimpleDataTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#collectionDataType.
	VisitCollectionDataType(ctx *CollectionDataTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#spatialDataType.
	VisitSpatialDataType(ctx *SpatialDataTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#longVarcharDataType.
	VisitLongVarcharDataType(ctx *LongVarcharDataTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#longVarbinaryDataType.
	VisitLongVarbinaryDataType(ctx *LongVarbinaryDataTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#collectionOptions.
	VisitCollectionOptions(ctx *CollectionOptionsContext) interface{}

	// Visit a parse tree produced by RelationalParser#convertedDataType.
	VisitConvertedDataType(ctx *ConvertedDataTypeContext) interface{}

	// Visit a parse tree produced by RelationalParser#lengthOneDimension.
	VisitLengthOneDimension(ctx *LengthOneDimensionContext) interface{}

	// Visit a parse tree produced by RelationalParser#lengthTwoDimension.
	VisitLengthTwoDimension(ctx *LengthTwoDimensionContext) interface{}

	// Visit a parse tree produced by RelationalParser#lengthTwoOptionalDimension.
	VisitLengthTwoOptionalDimension(ctx *LengthTwoOptionalDimensionContext) interface{}

	// Visit a parse tree produced by RelationalParser#uidList.
	VisitUidList(ctx *UidListContext) interface{}

	// Visit a parse tree produced by RelationalParser#uidWithNestings.
	VisitUidWithNestings(ctx *UidWithNestingsContext) interface{}

	// Visit a parse tree produced by RelationalParser#uidListWithNestingsInParens.
	VisitUidListWithNestingsInParens(ctx *UidListWithNestingsInParensContext) interface{}

	// Visit a parse tree produced by RelationalParser#uidListWithNestings.
	VisitUidListWithNestings(ctx *UidListWithNestingsContext) interface{}

	// Visit a parse tree produced by RelationalParser#tables.
	VisitTables(ctx *TablesContext) interface{}

	// Visit a parse tree produced by RelationalParser#indexColumnNames.
	VisitIndexColumnNames(ctx *IndexColumnNamesContext) interface{}

	// Visit a parse tree produced by RelationalParser#expressions.
	VisitExpressions(ctx *ExpressionsContext) interface{}

	// Visit a parse tree produced by RelationalParser#expressionsWithDefaults.
	VisitExpressionsWithDefaults(ctx *ExpressionsWithDefaultsContext) interface{}

	// Visit a parse tree produced by RelationalParser#recordConstructorForInsert.
	VisitRecordConstructorForInsert(ctx *RecordConstructorForInsertContext) interface{}

	// Visit a parse tree produced by RelationalParser#recordConstructorForInlineTable.
	VisitRecordConstructorForInlineTable(ctx *RecordConstructorForInlineTableContext) interface{}

	// Visit a parse tree produced by RelationalParser#recordConstructor.
	VisitRecordConstructor(ctx *RecordConstructorContext) interface{}

	// Visit a parse tree produced by RelationalParser#ofTypeClause.
	VisitOfTypeClause(ctx *OfTypeClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#arrayConstructor.
	VisitArrayConstructor(ctx *ArrayConstructorContext) interface{}

	// Visit a parse tree produced by RelationalParser#userVariables.
	VisitUserVariables(ctx *UserVariablesContext) interface{}

	// Visit a parse tree produced by RelationalParser#defaultValue.
	VisitDefaultValue(ctx *DefaultValueContext) interface{}

	// Visit a parse tree produced by RelationalParser#currentTimestamp.
	VisitCurrentTimestamp(ctx *CurrentTimestampContext) interface{}

	// Visit a parse tree produced by RelationalParser#expressionOrDefault.
	VisitExpressionOrDefault(ctx *ExpressionOrDefaultContext) interface{}

	// Visit a parse tree produced by RelationalParser#expressionWithOptionalName.
	VisitExpressionWithOptionalName(ctx *ExpressionWithOptionalNameContext) interface{}

	// Visit a parse tree produced by RelationalParser#ifExists.
	VisitIfExists(ctx *IfExistsContext) interface{}

	// Visit a parse tree produced by RelationalParser#ifNotExists.
	VisitIfNotExists(ctx *IfNotExistsContext) interface{}

	// Visit a parse tree produced by RelationalParser#aggregateFunctionCall.
	VisitAggregateFunctionCall(ctx *AggregateFunctionCallContext) interface{}

	// Visit a parse tree produced by RelationalParser#nonAggregateFunctionCall.
	VisitNonAggregateFunctionCall(ctx *NonAggregateFunctionCallContext) interface{}

	// Visit a parse tree produced by RelationalParser#specificFunctionCall.
	VisitSpecificFunctionCall(ctx *SpecificFunctionCallContext) interface{}

	// Visit a parse tree produced by RelationalParser#scalarFunctionCall.
	VisitScalarFunctionCall(ctx *ScalarFunctionCallContext) interface{}

	// Visit a parse tree produced by RelationalParser#userDefinedScalarFunctionCall.
	VisitUserDefinedScalarFunctionCall(ctx *UserDefinedScalarFunctionCallContext) interface{}

	// Visit a parse tree produced by RelationalParser#simpleFunctionCall.
	VisitSimpleFunctionCall(ctx *SimpleFunctionCallContext) interface{}

	// Visit a parse tree produced by RelationalParser#dataTypeFunctionCall.
	VisitDataTypeFunctionCall(ctx *DataTypeFunctionCallContext) interface{}

	// Visit a parse tree produced by RelationalParser#valuesFunctionCall.
	VisitValuesFunctionCall(ctx *ValuesFunctionCallContext) interface{}

	// Visit a parse tree produced by RelationalParser#caseExpressionFunctionCall.
	VisitCaseExpressionFunctionCall(ctx *CaseExpressionFunctionCallContext) interface{}

	// Visit a parse tree produced by RelationalParser#caseFunctionCall.
	VisitCaseFunctionCall(ctx *CaseFunctionCallContext) interface{}

	// Visit a parse tree produced by RelationalParser#charFunctionCall.
	VisitCharFunctionCall(ctx *CharFunctionCallContext) interface{}

	// Visit a parse tree produced by RelationalParser#positionFunctionCall.
	VisitPositionFunctionCall(ctx *PositionFunctionCallContext) interface{}

	// Visit a parse tree produced by RelationalParser#substrFunctionCall.
	VisitSubstrFunctionCall(ctx *SubstrFunctionCallContext) interface{}

	// Visit a parse tree produced by RelationalParser#trimFunctionCall.
	VisitTrimFunctionCall(ctx *TrimFunctionCallContext) interface{}

	// Visit a parse tree produced by RelationalParser#weightFunctionCall.
	VisitWeightFunctionCall(ctx *WeightFunctionCallContext) interface{}

	// Visit a parse tree produced by RelationalParser#extractFunctionCall.
	VisitExtractFunctionCall(ctx *ExtractFunctionCallContext) interface{}

	// Visit a parse tree produced by RelationalParser#getFormatFunctionCall.
	VisitGetFormatFunctionCall(ctx *GetFormatFunctionCallContext) interface{}

	// Visit a parse tree produced by RelationalParser#caseFuncAlternative.
	VisitCaseFuncAlternative(ctx *CaseFuncAlternativeContext) interface{}

	// Visit a parse tree produced by RelationalParser#levelWeightList.
	VisitLevelWeightList(ctx *LevelWeightListContext) interface{}

	// Visit a parse tree produced by RelationalParser#levelWeightRange.
	VisitLevelWeightRange(ctx *LevelWeightRangeContext) interface{}

	// Visit a parse tree produced by RelationalParser#levelInWeightListElement.
	VisitLevelInWeightListElement(ctx *LevelInWeightListElementContext) interface{}

	// Visit a parse tree produced by RelationalParser#aggregateWindowedFunction.
	VisitAggregateWindowedFunction(ctx *AggregateWindowedFunctionContext) interface{}

	// Visit a parse tree produced by RelationalParser#nonAggregateWindowedFunction.
	VisitNonAggregateWindowedFunction(ctx *NonAggregateWindowedFunctionContext) interface{}

	// Visit a parse tree produced by RelationalParser#overClause.
	VisitOverClause(ctx *OverClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#windowName.
	VisitWindowName(ctx *WindowNameContext) interface{}

	// Visit a parse tree produced by RelationalParser#windowSpec.
	VisitWindowSpec(ctx *WindowSpecContext) interface{}

	// Visit a parse tree produced by RelationalParser#windowOptionsClause.
	VisitWindowOptionsClause(ctx *WindowOptionsClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#windowOption.
	VisitWindowOption(ctx *WindowOptionContext) interface{}

	// Visit a parse tree produced by RelationalParser#partitionClause.
	VisitPartitionClause(ctx *PartitionClauseContext) interface{}

	// Visit a parse tree produced by RelationalParser#scalarFunctionName.
	VisitScalarFunctionName(ctx *ScalarFunctionNameContext) interface{}

	// Visit a parse tree produced by RelationalParser#userDefinedScalarFunctionName.
	VisitUserDefinedScalarFunctionName(ctx *UserDefinedScalarFunctionNameContext) interface{}

	// Visit a parse tree produced by RelationalParser#functionArgs.
	VisitFunctionArgs(ctx *FunctionArgsContext) interface{}

	// Visit a parse tree produced by RelationalParser#functionArg.
	VisitFunctionArg(ctx *FunctionArgContext) interface{}

	// Visit a parse tree produced by RelationalParser#namedFunctionArg.
	VisitNamedFunctionArg(ctx *NamedFunctionArgContext) interface{}

	// Visit a parse tree produced by RelationalParser#predicatedExpression.
	VisitPredicatedExpression(ctx *PredicatedExpressionContext) interface{}

	// Visit a parse tree produced by RelationalParser#notExpression.
	VisitNotExpression(ctx *NotExpressionContext) interface{}

	// Visit a parse tree produced by RelationalParser#logicalExpression.
	VisitLogicalExpression(ctx *LogicalExpressionContext) interface{}

	// Visit a parse tree produced by RelationalParser#existsExpressionAtom.
	VisitExistsExpressionAtom(ctx *ExistsExpressionAtomContext) interface{}

	// Visit a parse tree produced by RelationalParser#betweenComparisonPredicate.
	VisitBetweenComparisonPredicate(ctx *BetweenComparisonPredicateContext) interface{}

	// Visit a parse tree produced by RelationalParser#inPredicate.
	VisitInPredicate(ctx *InPredicateContext) interface{}

	// Visit a parse tree produced by RelationalParser#likePredicate.
	VisitLikePredicate(ctx *LikePredicateContext) interface{}

	// Visit a parse tree produced by RelationalParser#isExpression.
	VisitIsExpression(ctx *IsExpressionContext) interface{}

	// Visit a parse tree produced by RelationalParser#subqueryExpressionAtom.
	VisitSubqueryExpressionAtom(ctx *SubqueryExpressionAtomContext) interface{}

	// Visit a parse tree produced by RelationalParser#binaryComparisonPredicate.
	VisitBinaryComparisonPredicate(ctx *BinaryComparisonPredicateContext) interface{}

	// Visit a parse tree produced by RelationalParser#subscriptExpression.
	VisitSubscriptExpression(ctx *SubscriptExpressionContext) interface{}

	// Visit a parse tree produced by RelationalParser#constantExpressionAtom.
	VisitConstantExpressionAtom(ctx *ConstantExpressionAtomContext) interface{}

	// Visit a parse tree produced by RelationalParser#functionCallExpressionAtom.
	VisitFunctionCallExpressionAtom(ctx *FunctionCallExpressionAtomContext) interface{}

	// Visit a parse tree produced by RelationalParser#fullColumnNameExpressionAtom.
	VisitFullColumnNameExpressionAtom(ctx *FullColumnNameExpressionAtomContext) interface{}

	// Visit a parse tree produced by RelationalParser#bitExpressionAtom.
	VisitBitExpressionAtom(ctx *BitExpressionAtomContext) interface{}

	// Visit a parse tree produced by RelationalParser#preparedStatementParameterAtom.
	VisitPreparedStatementParameterAtom(ctx *PreparedStatementParameterAtomContext) interface{}

	// Visit a parse tree produced by RelationalParser#recordConstructorExpressionAtom.
	VisitRecordConstructorExpressionAtom(ctx *RecordConstructorExpressionAtomContext) interface{}

	// Visit a parse tree produced by RelationalParser#arrayConstructorExpressionAtom.
	VisitArrayConstructorExpressionAtom(ctx *ArrayConstructorExpressionAtomContext) interface{}

	// Visit a parse tree produced by RelationalParser#mathExpressionAtom.
	VisitMathExpressionAtom(ctx *MathExpressionAtomContext) interface{}

	// Visit a parse tree produced by RelationalParser#inList.
	VisitInList(ctx *InListContext) interface{}

	// Visit a parse tree produced by RelationalParser#preparedStatementParameter.
	VisitPreparedStatementParameter(ctx *PreparedStatementParameterContext) interface{}

	// Visit a parse tree produced by RelationalParser#unaryOperator.
	VisitUnaryOperator(ctx *UnaryOperatorContext) interface{}

	// Visit a parse tree produced by RelationalParser#comparisonOperator.
	VisitComparisonOperator(ctx *ComparisonOperatorContext) interface{}

	// Visit a parse tree produced by RelationalParser#logicalOperator.
	VisitLogicalOperator(ctx *LogicalOperatorContext) interface{}

	// Visit a parse tree produced by RelationalParser#bitOperator.
	VisitBitOperator(ctx *BitOperatorContext) interface{}

	// Visit a parse tree produced by RelationalParser#mathOperator.
	VisitMathOperator(ctx *MathOperatorContext) interface{}

	// Visit a parse tree produced by RelationalParser#jsonOperator.
	VisitJsonOperator(ctx *JsonOperatorContext) interface{}

	// Visit a parse tree produced by RelationalParser#charsetNameBase.
	VisitCharsetNameBase(ctx *CharsetNameBaseContext) interface{}

	// Visit a parse tree produced by RelationalParser#intervalTypeBase.
	VisitIntervalTypeBase(ctx *IntervalTypeBaseContext) interface{}

	// Visit a parse tree produced by RelationalParser#keywordsCanBeId.
	VisitKeywordsCanBeId(ctx *KeywordsCanBeIdContext) interface{}

	// Visit a parse tree produced by RelationalParser#functionNameBase.
	VisitFunctionNameBase(ctx *FunctionNameBaseContext) interface{}
}

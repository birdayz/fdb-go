// Code generated from RelationalParser.g4 by ANTLR 4.13.1. DO NOT EDIT.

package antlrgen // RelationalParser
import "github.com/antlr4-go/antlr/v4"

// RelationalParserListener is a complete listener for a parse tree produced by RelationalParser.
type RelationalParserListener interface {
	antlr.ParseTreeListener

	// EnterRoot is called when entering the root production.
	EnterRoot(c *RootContext)

	// EnterStatements is called when entering the statements production.
	EnterStatements(c *StatementsContext)

	// EnterStatement is called when entering the statement production.
	EnterStatement(c *StatementContext)

	// EnterDmlStatement is called when entering the dmlStatement production.
	EnterDmlStatement(c *DmlStatementContext)

	// EnterDdlStatement is called when entering the ddlStatement production.
	EnterDdlStatement(c *DdlStatementContext)

	// EnterTransactionStatement is called when entering the transactionStatement production.
	EnterTransactionStatement(c *TransactionStatementContext)

	// EnterPreparedStatement is called when entering the preparedStatement production.
	EnterPreparedStatement(c *PreparedStatementContext)

	// EnterAdministrationStatement is called when entering the administrationStatement production.
	EnterAdministrationStatement(c *AdministrationStatementContext)

	// EnterUtilityStatement is called when entering the utilityStatement production.
	EnterUtilityStatement(c *UtilityStatementContext)

	// EnterTemplateClause is called when entering the templateClause production.
	EnterTemplateClause(c *TemplateClauseContext)

	// EnterCreateSchemaStatement is called when entering the createSchemaStatement production.
	EnterCreateSchemaStatement(c *CreateSchemaStatementContext)

	// EnterCreateSchemaTemplateStatement is called when entering the createSchemaTemplateStatement production.
	EnterCreateSchemaTemplateStatement(c *CreateSchemaTemplateStatementContext)

	// EnterCreateDatabaseStatement is called when entering the createDatabaseStatement production.
	EnterCreateDatabaseStatement(c *CreateDatabaseStatementContext)

	// EnterOptionsClause is called when entering the optionsClause production.
	EnterOptionsClause(c *OptionsClauseContext)

	// EnterOption is called when entering the option production.
	EnterOption(c *OptionContext)

	// EnterDropDatabaseStatement is called when entering the dropDatabaseStatement production.
	EnterDropDatabaseStatement(c *DropDatabaseStatementContext)

	// EnterDropSchemaTemplateStatement is called when entering the dropSchemaTemplateStatement production.
	EnterDropSchemaTemplateStatement(c *DropSchemaTemplateStatementContext)

	// EnterDropSchemaStatement is called when entering the dropSchemaStatement production.
	EnterDropSchemaStatement(c *DropSchemaStatementContext)

	// EnterStructDefinition is called when entering the structDefinition production.
	EnterStructDefinition(c *StructDefinitionContext)

	// EnterTableDefinition is called when entering the tableDefinition production.
	EnterTableDefinition(c *TableDefinitionContext)

	// EnterColumnDefinition is called when entering the columnDefinition production.
	EnterColumnDefinition(c *ColumnDefinitionContext)

	// EnterFunctionColumnType is called when entering the functionColumnType production.
	EnterFunctionColumnType(c *FunctionColumnTypeContext)

	// EnterColumnType is called when entering the columnType production.
	EnterColumnType(c *ColumnTypeContext)

	// EnterPrimitiveType is called when entering the primitiveType production.
	EnterPrimitiveType(c *PrimitiveTypeContext)

	// EnterVectorType is called when entering the vectorType production.
	EnterVectorType(c *VectorTypeContext)

	// EnterVectorElementType is called when entering the vectorElementType production.
	EnterVectorElementType(c *VectorElementTypeContext)

	// EnterNullColumnConstraint is called when entering the nullColumnConstraint production.
	EnterNullColumnConstraint(c *NullColumnConstraintContext)

	// EnterPrimaryKeyDefinition is called when entering the primaryKeyDefinition production.
	EnterPrimaryKeyDefinition(c *PrimaryKeyDefinitionContext)

	// EnterFullIdList is called when entering the fullIdList production.
	EnterFullIdList(c *FullIdListContext)

	// EnterEnumDefinition is called when entering the enumDefinition production.
	EnterEnumDefinition(c *EnumDefinitionContext)

	// EnterIndexAsSelectDefinition is called when entering the indexAsSelectDefinition production.
	EnterIndexAsSelectDefinition(c *IndexAsSelectDefinitionContext)

	// EnterIndexOnSourceDefinition is called when entering the indexOnSourceDefinition production.
	EnterIndexOnSourceDefinition(c *IndexOnSourceDefinitionContext)

	// EnterVectorIndexDefinition is called when entering the vectorIndexDefinition production.
	EnterVectorIndexDefinition(c *VectorIndexDefinitionContext)

	// EnterIndexColumnList is called when entering the indexColumnList production.
	EnterIndexColumnList(c *IndexColumnListContext)

	// EnterIndexColumnSpec is called when entering the indexColumnSpec production.
	EnterIndexColumnSpec(c *IndexColumnSpecContext)

	// EnterIncludeClause is called when entering the includeClause production.
	EnterIncludeClause(c *IncludeClauseContext)

	// EnterIndexType is called when entering the indexType production.
	EnterIndexType(c *IndexTypeContext)

	// EnterIndexPartitionClause is called when entering the indexPartitionClause production.
	EnterIndexPartitionClause(c *IndexPartitionClauseContext)

	// EnterIndexOptions is called when entering the indexOptions production.
	EnterIndexOptions(c *IndexOptionsContext)

	// EnterIndexOption is called when entering the indexOption production.
	EnterIndexOption(c *IndexOptionContext)

	// EnterVectorIndexOptions is called when entering the vectorIndexOptions production.
	EnterVectorIndexOptions(c *VectorIndexOptionsContext)

	// EnterVectorIndexOption is called when entering the vectorIndexOption production.
	EnterVectorIndexOption(c *VectorIndexOptionContext)

	// EnterHnswMetric is called when entering the hnswMetric production.
	EnterHnswMetric(c *HnswMetricContext)

	// EnterIndexAttributes is called when entering the indexAttributes production.
	EnterIndexAttributes(c *IndexAttributesContext)

	// EnterIndexAttribute is called when entering the indexAttribute production.
	EnterIndexAttribute(c *IndexAttributeContext)

	// EnterCreateTempFunction is called when entering the createTempFunction production.
	EnterCreateTempFunction(c *CreateTempFunctionContext)

	// EnterDropTempFunction is called when entering the dropTempFunction production.
	EnterDropTempFunction(c *DropTempFunctionContext)

	// EnterViewDefinition is called when entering the viewDefinition production.
	EnterViewDefinition(c *ViewDefinitionContext)

	// EnterTempSqlInvokedFunction is called when entering the tempSqlInvokedFunction production.
	EnterTempSqlInvokedFunction(c *TempSqlInvokedFunctionContext)

	// EnterSqlInvokedFunction is called when entering the sqlInvokedFunction production.
	EnterSqlInvokedFunction(c *SqlInvokedFunctionContext)

	// EnterFunctionSpecification is called when entering the functionSpecification production.
	EnterFunctionSpecification(c *FunctionSpecificationContext)

	// EnterSqlParameterDeclarationList is called when entering the sqlParameterDeclarationList production.
	EnterSqlParameterDeclarationList(c *SqlParameterDeclarationListContext)

	// EnterSqlParameterDeclarations is called when entering the sqlParameterDeclarations production.
	EnterSqlParameterDeclarations(c *SqlParameterDeclarationsContext)

	// EnterSqlParameterDeclaration is called when entering the sqlParameterDeclaration production.
	EnterSqlParameterDeclaration(c *SqlParameterDeclarationContext)

	// EnterParameterMode is called when entering the parameterMode production.
	EnterParameterMode(c *ParameterModeContext)

	// EnterReturnsClause is called when entering the returnsClause production.
	EnterReturnsClause(c *ReturnsClauseContext)

	// EnterReturnsType is called when entering the returnsType production.
	EnterReturnsType(c *ReturnsTypeContext)

	// EnterReturnsTableType is called when entering the returnsTableType production.
	EnterReturnsTableType(c *ReturnsTableTypeContext)

	// EnterTableFunctionColumnList is called when entering the tableFunctionColumnList production.
	EnterTableFunctionColumnList(c *TableFunctionColumnListContext)

	// EnterTableFunctionColumnListElement is called when entering the tableFunctionColumnListElement production.
	EnterTableFunctionColumnListElement(c *TableFunctionColumnListElementContext)

	// EnterRoutineCharacteristics is called when entering the routineCharacteristics production.
	EnterRoutineCharacteristics(c *RoutineCharacteristicsContext)

	// EnterLanguageClause is called when entering the languageClause production.
	EnterLanguageClause(c *LanguageClauseContext)

	// EnterLanguageName is called when entering the languageName production.
	EnterLanguageName(c *LanguageNameContext)

	// EnterParameterStyle is called when entering the parameterStyle production.
	EnterParameterStyle(c *ParameterStyleContext)

	// EnterDeterministicCharacteristic is called when entering the deterministicCharacteristic production.
	EnterDeterministicCharacteristic(c *DeterministicCharacteristicContext)

	// EnterNullCallClause is called when entering the nullCallClause production.
	EnterNullCallClause(c *NullCallClauseContext)

	// EnterDispatchClause is called when entering the dispatchClause production.
	EnterDispatchClause(c *DispatchClauseContext)

	// EnterStatementBody is called when entering the statementBody production.
	EnterStatementBody(c *StatementBodyContext)

	// EnterUserDefinedScalarFunctionStatementBody is called when entering the userDefinedScalarFunctionStatementBody production.
	EnterUserDefinedScalarFunctionStatementBody(c *UserDefinedScalarFunctionStatementBodyContext)

	// EnterExpressionBody is called when entering the expressionBody production.
	EnterExpressionBody(c *ExpressionBodyContext)

	// EnterSqlReturnStatement is called when entering the sqlReturnStatement production.
	EnterSqlReturnStatement(c *SqlReturnStatementContext)

	// EnterReturnValue is called when entering the returnValue production.
	EnterReturnValue(c *ReturnValueContext)

	// EnterCharSet is called when entering the charSet production.
	EnterCharSet(c *CharSetContext)

	// EnterIntervalType is called when entering the intervalType production.
	EnterIntervalType(c *IntervalTypeContext)

	// EnterSchemaId is called when entering the schemaId production.
	EnterSchemaId(c *SchemaIdContext)

	// EnterPath is called when entering the path production.
	EnterPath(c *PathContext)

	// EnterSchemaTemplateId is called when entering the schemaTemplateId production.
	EnterSchemaTemplateId(c *SchemaTemplateIdContext)

	// EnterDeleteStatement is called when entering the deleteStatement production.
	EnterDeleteStatement(c *DeleteStatementContext)

	// EnterInsertStatement is called when entering the insertStatement production.
	EnterInsertStatement(c *InsertStatementContext)

	// EnterContinuationAtom is called when entering the continuationAtom production.
	EnterContinuationAtom(c *ContinuationAtomContext)

	// EnterSelectStatement is called when entering the selectStatement production.
	EnterSelectStatement(c *SelectStatementContext)

	// EnterQuery is called when entering the query production.
	EnterQuery(c *QueryContext)

	// EnterCtes is called when entering the ctes production.
	EnterCtes(c *CtesContext)

	// EnterTraversalOrderClause is called when entering the traversalOrderClause production.
	EnterTraversalOrderClause(c *TraversalOrderClauseContext)

	// EnterNamedQuery is called when entering the namedQuery production.
	EnterNamedQuery(c *NamedQueryContext)

	// EnterTableFunction is called when entering the tableFunction production.
	EnterTableFunction(c *TableFunctionContext)

	// EnterTableFunctionArgs is called when entering the tableFunctionArgs production.
	EnterTableFunctionArgs(c *TableFunctionArgsContext)

	// EnterTableFunctionName is called when entering the tableFunctionName production.
	EnterTableFunctionName(c *TableFunctionNameContext)

	// EnterQueryTermDefault is called when entering the queryTermDefault production.
	EnterQueryTermDefault(c *QueryTermDefaultContext)

	// EnterSetQuery is called when entering the setQuery production.
	EnterSetQuery(c *SetQueryContext)

	// EnterInsertStatementValueSelect is called when entering the insertStatementValueSelect production.
	EnterInsertStatementValueSelect(c *InsertStatementValueSelectContext)

	// EnterInsertStatementValueValues is called when entering the insertStatementValueValues production.
	EnterInsertStatementValueValues(c *InsertStatementValueValuesContext)

	// EnterUpdatedElement is called when entering the updatedElement production.
	EnterUpdatedElement(c *UpdatedElementContext)

	// EnterAssignmentField is called when entering the assignmentField production.
	EnterAssignmentField(c *AssignmentFieldContext)

	// EnterUpdateStatement is called when entering the updateStatement production.
	EnterUpdateStatement(c *UpdateStatementContext)

	// EnterOrderByClause is called when entering the orderByClause production.
	EnterOrderByClause(c *OrderByClauseContext)

	// EnterOrderByExpression is called when entering the orderByExpression production.
	EnterOrderByExpression(c *OrderByExpressionContext)

	// EnterOrderClause is called when entering the orderClause production.
	EnterOrderClause(c *OrderClauseContext)

	// EnterTableSources is called when entering the tableSources production.
	EnterTableSources(c *TableSourcesContext)

	// EnterTableSourceBase is called when entering the tableSourceBase production.
	EnterTableSourceBase(c *TableSourceBaseContext)

	// EnterAtomTableItem is called when entering the atomTableItem production.
	EnterAtomTableItem(c *AtomTableItemContext)

	// EnterSubqueryTableItem is called when entering the subqueryTableItem production.
	EnterSubqueryTableItem(c *SubqueryTableItemContext)

	// EnterInlineTableItem is called when entering the inlineTableItem production.
	EnterInlineTableItem(c *InlineTableItemContext)

	// EnterTableValuedFunction is called when entering the tableValuedFunction production.
	EnterTableValuedFunction(c *TableValuedFunctionContext)

	// EnterIndexHint is called when entering the indexHint production.
	EnterIndexHint(c *IndexHintContext)

	// EnterIndexHintType is called when entering the indexHintType production.
	EnterIndexHintType(c *IndexHintTypeContext)

	// EnterInlineTableDefinition is called when entering the inlineTableDefinition production.
	EnterInlineTableDefinition(c *InlineTableDefinitionContext)

	// EnterInnerJoin is called when entering the innerJoin production.
	EnterInnerJoin(c *InnerJoinContext)

	// EnterStraightJoin is called when entering the straightJoin production.
	EnterStraightJoin(c *StraightJoinContext)

	// EnterOuterJoin is called when entering the outerJoin production.
	EnterOuterJoin(c *OuterJoinContext)

	// EnterNaturalJoin is called when entering the naturalJoin production.
	EnterNaturalJoin(c *NaturalJoinContext)

	// EnterSimpleTable is called when entering the simpleTable production.
	EnterSimpleTable(c *SimpleTableContext)

	// EnterParenthesisQuery is called when entering the parenthesisQuery production.
	EnterParenthesisQuery(c *ParenthesisQueryContext)

	// EnterSelectElements is called when entering the selectElements production.
	EnterSelectElements(c *SelectElementsContext)

	// EnterSelectStarElement is called when entering the selectStarElement production.
	EnterSelectStarElement(c *SelectStarElementContext)

	// EnterSelectQualifierStarElement is called when entering the selectQualifierStarElement production.
	EnterSelectQualifierStarElement(c *SelectQualifierStarElementContext)

	// EnterSelectExpressionElement is called when entering the selectExpressionElement production.
	EnterSelectExpressionElement(c *SelectExpressionElementContext)

	// EnterFromClause is called when entering the fromClause production.
	EnterFromClause(c *FromClauseContext)

	// EnterGroupByClause is called when entering the groupByClause production.
	EnterGroupByClause(c *GroupByClauseContext)

	// EnterWhereExpr is called when entering the whereExpr production.
	EnterWhereExpr(c *WhereExprContext)

	// EnterHavingClause is called when entering the havingClause production.
	EnterHavingClause(c *HavingClauseContext)

	// EnterQualifyClause is called when entering the qualifyClause production.
	EnterQualifyClause(c *QualifyClauseContext)

	// EnterGroupByItem is called when entering the groupByItem production.
	EnterGroupByItem(c *GroupByItemContext)

	// EnterLimitClause is called when entering the limitClause production.
	EnterLimitClause(c *LimitClauseContext)

	// EnterLimitClauseAtom is called when entering the limitClauseAtom production.
	EnterLimitClauseAtom(c *LimitClauseAtomContext)

	// EnterQueryOptions is called when entering the queryOptions production.
	EnterQueryOptions(c *QueryOptionsContext)

	// EnterQueryOption is called when entering the queryOption production.
	EnterQueryOption(c *QueryOptionContext)

	// EnterStartTransaction is called when entering the startTransaction production.
	EnterStartTransaction(c *StartTransactionContext)

	// EnterCommitStatement is called when entering the commitStatement production.
	EnterCommitStatement(c *CommitStatementContext)

	// EnterRollbackStatement is called when entering the rollbackStatement production.
	EnterRollbackStatement(c *RollbackStatementContext)

	// EnterSetAutocommitStatement is called when entering the setAutocommitStatement production.
	EnterSetAutocommitStatement(c *SetAutocommitStatementContext)

	// EnterSetTransactionStatement is called when entering the setTransactionStatement production.
	EnterSetTransactionStatement(c *SetTransactionStatementContext)

	// EnterTransactionOption is called when entering the transactionOption production.
	EnterTransactionOption(c *TransactionOptionContext)

	// EnterTransactionLevel is called when entering the transactionLevel production.
	EnterTransactionLevel(c *TransactionLevelContext)

	// EnterPrepareStatement is called when entering the prepareStatement production.
	EnterPrepareStatement(c *PrepareStatementContext)

	// EnterExecuteStatement is called when entering the executeStatement production.
	EnterExecuteStatement(c *ExecuteStatementContext)

	// EnterShowDatabasesStatement is called when entering the showDatabasesStatement production.
	EnterShowDatabasesStatement(c *ShowDatabasesStatementContext)

	// EnterShowSchemaTemplatesStatement is called when entering the showSchemaTemplatesStatement production.
	EnterShowSchemaTemplatesStatement(c *ShowSchemaTemplatesStatementContext)

	// EnterSetVariable is called when entering the setVariable production.
	EnterSetVariable(c *SetVariableContext)

	// EnterSetCharset is called when entering the setCharset production.
	EnterSetCharset(c *SetCharsetContext)

	// EnterSetNames is called when entering the setNames production.
	EnterSetNames(c *SetNamesContext)

	// EnterSetTransaction is called when entering the setTransaction production.
	EnterSetTransaction(c *SetTransactionContext)

	// EnterSetAutocommit is called when entering the setAutocommit production.
	EnterSetAutocommit(c *SetAutocommitContext)

	// EnterSetNewValueInsideTrigger is called when entering the setNewValueInsideTrigger production.
	EnterSetNewValueInsideTrigger(c *SetNewValueInsideTriggerContext)

	// EnterVariableClause is called when entering the variableClause production.
	EnterVariableClause(c *VariableClauseContext)

	// EnterKillStatement is called when entering the killStatement production.
	EnterKillStatement(c *KillStatementContext)

	// EnterResetStatement is called when entering the resetStatement production.
	EnterResetStatement(c *ResetStatementContext)

	// EnterExecuteContinuationStatement is called when entering the executeContinuationStatement production.
	EnterExecuteContinuationStatement(c *ExecuteContinuationStatementContext)

	// EnterCopyExportStatement is called when entering the copyExportStatement production.
	EnterCopyExportStatement(c *CopyExportStatementContext)

	// EnterCopyImportStatement is called when entering the copyImportStatement production.
	EnterCopyImportStatement(c *CopyImportStatementContext)

	// EnterTableIndexes is called when entering the tableIndexes production.
	EnterTableIndexes(c *TableIndexesContext)

	// EnterLoadedTableIndexes is called when entering the loadedTableIndexes production.
	EnterLoadedTableIndexes(c *LoadedTableIndexesContext)

	// EnterSimpleDescribeSchemaStatement is called when entering the simpleDescribeSchemaStatement production.
	EnterSimpleDescribeSchemaStatement(c *SimpleDescribeSchemaStatementContext)

	// EnterSimpleDescribeSchemaTemplateStatement is called when entering the simpleDescribeSchemaTemplateStatement production.
	EnterSimpleDescribeSchemaTemplateStatement(c *SimpleDescribeSchemaTemplateStatementContext)

	// EnterFullDescribeStatement is called when entering the fullDescribeStatement production.
	EnterFullDescribeStatement(c *FullDescribeStatementContext)

	// EnterHelpStatement is called when entering the helpStatement production.
	EnterHelpStatement(c *HelpStatementContext)

	// EnterDescribeStatements is called when entering the describeStatements production.
	EnterDescribeStatements(c *DescribeStatementsContext)

	// EnterDescribeConnection is called when entering the describeConnection production.
	EnterDescribeConnection(c *DescribeConnectionContext)

	// EnterFullId is called when entering the fullId production.
	EnterFullId(c *FullIdContext)

	// EnterTableName is called when entering the tableName production.
	EnterTableName(c *TableNameContext)

	// EnterFullColumnName is called when entering the fullColumnName production.
	EnterFullColumnName(c *FullColumnNameContext)

	// EnterIndexColumnName is called when entering the indexColumnName production.
	EnterIndexColumnName(c *IndexColumnNameContext)

	// EnterCharsetName is called when entering the charsetName production.
	EnterCharsetName(c *CharsetNameContext)

	// EnterCollationName is called when entering the collationName production.
	EnterCollationName(c *CollationNameContext)

	// EnterUid is called when entering the uid production.
	EnterUid(c *UidContext)

	// EnterSimpleId is called when entering the simpleId production.
	EnterSimpleId(c *SimpleIdContext)

	// EnterNullNotnull is called when entering the nullNotnull production.
	EnterNullNotnull(c *NullNotnullContext)

	// EnterDecimalLiteral is called when entering the decimalLiteral production.
	EnterDecimalLiteral(c *DecimalLiteralContext)

	// EnterStringLiteral is called when entering the stringLiteral production.
	EnterStringLiteral(c *StringLiteralContext)

	// EnterBooleanLiteral is called when entering the booleanLiteral production.
	EnterBooleanLiteral(c *BooleanLiteralContext)

	// EnterBytesLiteral is called when entering the bytesLiteral production.
	EnterBytesLiteral(c *BytesLiteralContext)

	// EnterNullLiteral is called when entering the nullLiteral production.
	EnterNullLiteral(c *NullLiteralContext)

	// EnterStringConstant is called when entering the stringConstant production.
	EnterStringConstant(c *StringConstantContext)

	// EnterDecimalConstant is called when entering the decimalConstant production.
	EnterDecimalConstant(c *DecimalConstantContext)

	// EnterNegativeDecimalConstant is called when entering the negativeDecimalConstant production.
	EnterNegativeDecimalConstant(c *NegativeDecimalConstantContext)

	// EnterBytesConstant is called when entering the bytesConstant production.
	EnterBytesConstant(c *BytesConstantContext)

	// EnterBooleanConstant is called when entering the booleanConstant production.
	EnterBooleanConstant(c *BooleanConstantContext)

	// EnterBitStringConstant is called when entering the bitStringConstant production.
	EnterBitStringConstant(c *BitStringConstantContext)

	// EnterNullConstant is called when entering the nullConstant production.
	EnterNullConstant(c *NullConstantContext)

	// EnterStringDataType is called when entering the stringDataType production.
	EnterStringDataType(c *StringDataTypeContext)

	// EnterNationalStringDataType is called when entering the nationalStringDataType production.
	EnterNationalStringDataType(c *NationalStringDataTypeContext)

	// EnterNationalVaryingStringDataType is called when entering the nationalVaryingStringDataType production.
	EnterNationalVaryingStringDataType(c *NationalVaryingStringDataTypeContext)

	// EnterDimensionDataType is called when entering the dimensionDataType production.
	EnterDimensionDataType(c *DimensionDataTypeContext)

	// EnterSimpleDataType is called when entering the simpleDataType production.
	EnterSimpleDataType(c *SimpleDataTypeContext)

	// EnterCollectionDataType is called when entering the collectionDataType production.
	EnterCollectionDataType(c *CollectionDataTypeContext)

	// EnterSpatialDataType is called when entering the spatialDataType production.
	EnterSpatialDataType(c *SpatialDataTypeContext)

	// EnterLongVarcharDataType is called when entering the longVarcharDataType production.
	EnterLongVarcharDataType(c *LongVarcharDataTypeContext)

	// EnterLongVarbinaryDataType is called when entering the longVarbinaryDataType production.
	EnterLongVarbinaryDataType(c *LongVarbinaryDataTypeContext)

	// EnterCollectionOptions is called when entering the collectionOptions production.
	EnterCollectionOptions(c *CollectionOptionsContext)

	// EnterConvertedDataType is called when entering the convertedDataType production.
	EnterConvertedDataType(c *ConvertedDataTypeContext)

	// EnterLengthOneDimension is called when entering the lengthOneDimension production.
	EnterLengthOneDimension(c *LengthOneDimensionContext)

	// EnterLengthTwoDimension is called when entering the lengthTwoDimension production.
	EnterLengthTwoDimension(c *LengthTwoDimensionContext)

	// EnterLengthTwoOptionalDimension is called when entering the lengthTwoOptionalDimension production.
	EnterLengthTwoOptionalDimension(c *LengthTwoOptionalDimensionContext)

	// EnterUidList is called when entering the uidList production.
	EnterUidList(c *UidListContext)

	// EnterUidWithNestings is called when entering the uidWithNestings production.
	EnterUidWithNestings(c *UidWithNestingsContext)

	// EnterUidListWithNestingsInParens is called when entering the uidListWithNestingsInParens production.
	EnterUidListWithNestingsInParens(c *UidListWithNestingsInParensContext)

	// EnterUidListWithNestings is called when entering the uidListWithNestings production.
	EnterUidListWithNestings(c *UidListWithNestingsContext)

	// EnterTables is called when entering the tables production.
	EnterTables(c *TablesContext)

	// EnterIndexColumnNames is called when entering the indexColumnNames production.
	EnterIndexColumnNames(c *IndexColumnNamesContext)

	// EnterExpressions is called when entering the expressions production.
	EnterExpressions(c *ExpressionsContext)

	// EnterExpressionsWithDefaults is called when entering the expressionsWithDefaults production.
	EnterExpressionsWithDefaults(c *ExpressionsWithDefaultsContext)

	// EnterRecordConstructorForInsert is called when entering the recordConstructorForInsert production.
	EnterRecordConstructorForInsert(c *RecordConstructorForInsertContext)

	// EnterRecordConstructorForInlineTable is called when entering the recordConstructorForInlineTable production.
	EnterRecordConstructorForInlineTable(c *RecordConstructorForInlineTableContext)

	// EnterRecordConstructor is called when entering the recordConstructor production.
	EnterRecordConstructor(c *RecordConstructorContext)

	// EnterOfTypeClause is called when entering the ofTypeClause production.
	EnterOfTypeClause(c *OfTypeClauseContext)

	// EnterArrayConstructor is called when entering the arrayConstructor production.
	EnterArrayConstructor(c *ArrayConstructorContext)

	// EnterUserVariables is called when entering the userVariables production.
	EnterUserVariables(c *UserVariablesContext)

	// EnterDefaultValue is called when entering the defaultValue production.
	EnterDefaultValue(c *DefaultValueContext)

	// EnterCurrentTimestamp is called when entering the currentTimestamp production.
	EnterCurrentTimestamp(c *CurrentTimestampContext)

	// EnterExpressionOrDefault is called when entering the expressionOrDefault production.
	EnterExpressionOrDefault(c *ExpressionOrDefaultContext)

	// EnterExpressionWithOptionalName is called when entering the expressionWithOptionalName production.
	EnterExpressionWithOptionalName(c *ExpressionWithOptionalNameContext)

	// EnterIfExists is called when entering the ifExists production.
	EnterIfExists(c *IfExistsContext)

	// EnterIfNotExists is called when entering the ifNotExists production.
	EnterIfNotExists(c *IfNotExistsContext)

	// EnterAggregateFunctionCall is called when entering the aggregateFunctionCall production.
	EnterAggregateFunctionCall(c *AggregateFunctionCallContext)

	// EnterNonAggregateFunctionCall is called when entering the nonAggregateFunctionCall production.
	EnterNonAggregateFunctionCall(c *NonAggregateFunctionCallContext)

	// EnterSpecificFunctionCall is called when entering the specificFunctionCall production.
	EnterSpecificFunctionCall(c *SpecificFunctionCallContext)

	// EnterScalarFunctionCall is called when entering the scalarFunctionCall production.
	EnterScalarFunctionCall(c *ScalarFunctionCallContext)

	// EnterUserDefinedScalarFunctionCall is called when entering the userDefinedScalarFunctionCall production.
	EnterUserDefinedScalarFunctionCall(c *UserDefinedScalarFunctionCallContext)

	// EnterSimpleFunctionCall is called when entering the simpleFunctionCall production.
	EnterSimpleFunctionCall(c *SimpleFunctionCallContext)

	// EnterDataTypeFunctionCall is called when entering the dataTypeFunctionCall production.
	EnterDataTypeFunctionCall(c *DataTypeFunctionCallContext)

	// EnterValuesFunctionCall is called when entering the valuesFunctionCall production.
	EnterValuesFunctionCall(c *ValuesFunctionCallContext)

	// EnterCaseExpressionFunctionCall is called when entering the caseExpressionFunctionCall production.
	EnterCaseExpressionFunctionCall(c *CaseExpressionFunctionCallContext)

	// EnterCaseFunctionCall is called when entering the caseFunctionCall production.
	EnterCaseFunctionCall(c *CaseFunctionCallContext)

	// EnterCharFunctionCall is called when entering the charFunctionCall production.
	EnterCharFunctionCall(c *CharFunctionCallContext)

	// EnterPositionFunctionCall is called when entering the positionFunctionCall production.
	EnterPositionFunctionCall(c *PositionFunctionCallContext)

	// EnterSubstrFunctionCall is called when entering the substrFunctionCall production.
	EnterSubstrFunctionCall(c *SubstrFunctionCallContext)

	// EnterTrimFunctionCall is called when entering the trimFunctionCall production.
	EnterTrimFunctionCall(c *TrimFunctionCallContext)

	// EnterWeightFunctionCall is called when entering the weightFunctionCall production.
	EnterWeightFunctionCall(c *WeightFunctionCallContext)

	// EnterExtractFunctionCall is called when entering the extractFunctionCall production.
	EnterExtractFunctionCall(c *ExtractFunctionCallContext)

	// EnterGetFormatFunctionCall is called when entering the getFormatFunctionCall production.
	EnterGetFormatFunctionCall(c *GetFormatFunctionCallContext)

	// EnterCaseFuncAlternative is called when entering the caseFuncAlternative production.
	EnterCaseFuncAlternative(c *CaseFuncAlternativeContext)

	// EnterLevelWeightList is called when entering the levelWeightList production.
	EnterLevelWeightList(c *LevelWeightListContext)

	// EnterLevelWeightRange is called when entering the levelWeightRange production.
	EnterLevelWeightRange(c *LevelWeightRangeContext)

	// EnterLevelInWeightListElement is called when entering the levelInWeightListElement production.
	EnterLevelInWeightListElement(c *LevelInWeightListElementContext)

	// EnterAggregateWindowedFunction is called when entering the aggregateWindowedFunction production.
	EnterAggregateWindowedFunction(c *AggregateWindowedFunctionContext)

	// EnterNonAggregateWindowedFunction is called when entering the nonAggregateWindowedFunction production.
	EnterNonAggregateWindowedFunction(c *NonAggregateWindowedFunctionContext)

	// EnterOverClause is called when entering the overClause production.
	EnterOverClause(c *OverClauseContext)

	// EnterWindowName is called when entering the windowName production.
	EnterWindowName(c *WindowNameContext)

	// EnterWindowSpec is called when entering the windowSpec production.
	EnterWindowSpec(c *WindowSpecContext)

	// EnterWindowOptionsClause is called when entering the windowOptionsClause production.
	EnterWindowOptionsClause(c *WindowOptionsClauseContext)

	// EnterWindowOption is called when entering the windowOption production.
	EnterWindowOption(c *WindowOptionContext)

	// EnterPartitionClause is called when entering the partitionClause production.
	EnterPartitionClause(c *PartitionClauseContext)

	// EnterScalarFunctionName is called when entering the scalarFunctionName production.
	EnterScalarFunctionName(c *ScalarFunctionNameContext)

	// EnterUserDefinedScalarFunctionName is called when entering the userDefinedScalarFunctionName production.
	EnterUserDefinedScalarFunctionName(c *UserDefinedScalarFunctionNameContext)

	// EnterFunctionArgs is called when entering the functionArgs production.
	EnterFunctionArgs(c *FunctionArgsContext)

	// EnterFunctionArg is called when entering the functionArg production.
	EnterFunctionArg(c *FunctionArgContext)

	// EnterNamedFunctionArg is called when entering the namedFunctionArg production.
	EnterNamedFunctionArg(c *NamedFunctionArgContext)

	// EnterPredicatedExpression is called when entering the predicatedExpression production.
	EnterPredicatedExpression(c *PredicatedExpressionContext)

	// EnterNotExpression is called when entering the notExpression production.
	EnterNotExpression(c *NotExpressionContext)

	// EnterLogicalExpression is called when entering the logicalExpression production.
	EnterLogicalExpression(c *LogicalExpressionContext)

	// EnterExistsExpressionAtom is called when entering the existsExpressionAtom production.
	EnterExistsExpressionAtom(c *ExistsExpressionAtomContext)

	// EnterBetweenComparisonPredicate is called when entering the betweenComparisonPredicate production.
	EnterBetweenComparisonPredicate(c *BetweenComparisonPredicateContext)

	// EnterInPredicate is called when entering the inPredicate production.
	EnterInPredicate(c *InPredicateContext)

	// EnterLikePredicate is called when entering the likePredicate production.
	EnterLikePredicate(c *LikePredicateContext)

	// EnterIsExpression is called when entering the isExpression production.
	EnterIsExpression(c *IsExpressionContext)

	// EnterSubqueryExpressionAtom is called when entering the subqueryExpressionAtom production.
	EnterSubqueryExpressionAtom(c *SubqueryExpressionAtomContext)

	// EnterBinaryComparisonPredicate is called when entering the binaryComparisonPredicate production.
	EnterBinaryComparisonPredicate(c *BinaryComparisonPredicateContext)

	// EnterSubscriptExpression is called when entering the subscriptExpression production.
	EnterSubscriptExpression(c *SubscriptExpressionContext)

	// EnterConstantExpressionAtom is called when entering the constantExpressionAtom production.
	EnterConstantExpressionAtom(c *ConstantExpressionAtomContext)

	// EnterFunctionCallExpressionAtom is called when entering the functionCallExpressionAtom production.
	EnterFunctionCallExpressionAtom(c *FunctionCallExpressionAtomContext)

	// EnterFullColumnNameExpressionAtom is called when entering the fullColumnNameExpressionAtom production.
	EnterFullColumnNameExpressionAtom(c *FullColumnNameExpressionAtomContext)

	// EnterBitExpressionAtom is called when entering the bitExpressionAtom production.
	EnterBitExpressionAtom(c *BitExpressionAtomContext)

	// EnterPreparedStatementParameterAtom is called when entering the preparedStatementParameterAtom production.
	EnterPreparedStatementParameterAtom(c *PreparedStatementParameterAtomContext)

	// EnterRecordConstructorExpressionAtom is called when entering the recordConstructorExpressionAtom production.
	EnterRecordConstructorExpressionAtom(c *RecordConstructorExpressionAtomContext)

	// EnterArrayConstructorExpressionAtom is called when entering the arrayConstructorExpressionAtom production.
	EnterArrayConstructorExpressionAtom(c *ArrayConstructorExpressionAtomContext)

	// EnterMathExpressionAtom is called when entering the mathExpressionAtom production.
	EnterMathExpressionAtom(c *MathExpressionAtomContext)

	// EnterInList is called when entering the inList production.
	EnterInList(c *InListContext)

	// EnterPreparedStatementParameter is called when entering the preparedStatementParameter production.
	EnterPreparedStatementParameter(c *PreparedStatementParameterContext)

	// EnterUnaryOperator is called when entering the unaryOperator production.
	EnterUnaryOperator(c *UnaryOperatorContext)

	// EnterComparisonOperator is called when entering the comparisonOperator production.
	EnterComparisonOperator(c *ComparisonOperatorContext)

	// EnterLogicalOperator is called when entering the logicalOperator production.
	EnterLogicalOperator(c *LogicalOperatorContext)

	// EnterBitOperator is called when entering the bitOperator production.
	EnterBitOperator(c *BitOperatorContext)

	// EnterMathOperator is called when entering the mathOperator production.
	EnterMathOperator(c *MathOperatorContext)

	// EnterJsonOperator is called when entering the jsonOperator production.
	EnterJsonOperator(c *JsonOperatorContext)

	// EnterCharsetNameBase is called when entering the charsetNameBase production.
	EnterCharsetNameBase(c *CharsetNameBaseContext)

	// EnterIntervalTypeBase is called when entering the intervalTypeBase production.
	EnterIntervalTypeBase(c *IntervalTypeBaseContext)

	// EnterKeywordsCanBeId is called when entering the keywordsCanBeId production.
	EnterKeywordsCanBeId(c *KeywordsCanBeIdContext)

	// EnterFunctionNameBase is called when entering the functionNameBase production.
	EnterFunctionNameBase(c *FunctionNameBaseContext)

	// ExitRoot is called when exiting the root production.
	ExitRoot(c *RootContext)

	// ExitStatements is called when exiting the statements production.
	ExitStatements(c *StatementsContext)

	// ExitStatement is called when exiting the statement production.
	ExitStatement(c *StatementContext)

	// ExitDmlStatement is called when exiting the dmlStatement production.
	ExitDmlStatement(c *DmlStatementContext)

	// ExitDdlStatement is called when exiting the ddlStatement production.
	ExitDdlStatement(c *DdlStatementContext)

	// ExitTransactionStatement is called when exiting the transactionStatement production.
	ExitTransactionStatement(c *TransactionStatementContext)

	// ExitPreparedStatement is called when exiting the preparedStatement production.
	ExitPreparedStatement(c *PreparedStatementContext)

	// ExitAdministrationStatement is called when exiting the administrationStatement production.
	ExitAdministrationStatement(c *AdministrationStatementContext)

	// ExitUtilityStatement is called when exiting the utilityStatement production.
	ExitUtilityStatement(c *UtilityStatementContext)

	// ExitTemplateClause is called when exiting the templateClause production.
	ExitTemplateClause(c *TemplateClauseContext)

	// ExitCreateSchemaStatement is called when exiting the createSchemaStatement production.
	ExitCreateSchemaStatement(c *CreateSchemaStatementContext)

	// ExitCreateSchemaTemplateStatement is called when exiting the createSchemaTemplateStatement production.
	ExitCreateSchemaTemplateStatement(c *CreateSchemaTemplateStatementContext)

	// ExitCreateDatabaseStatement is called when exiting the createDatabaseStatement production.
	ExitCreateDatabaseStatement(c *CreateDatabaseStatementContext)

	// ExitOptionsClause is called when exiting the optionsClause production.
	ExitOptionsClause(c *OptionsClauseContext)

	// ExitOption is called when exiting the option production.
	ExitOption(c *OptionContext)

	// ExitDropDatabaseStatement is called when exiting the dropDatabaseStatement production.
	ExitDropDatabaseStatement(c *DropDatabaseStatementContext)

	// ExitDropSchemaTemplateStatement is called when exiting the dropSchemaTemplateStatement production.
	ExitDropSchemaTemplateStatement(c *DropSchemaTemplateStatementContext)

	// ExitDropSchemaStatement is called when exiting the dropSchemaStatement production.
	ExitDropSchemaStatement(c *DropSchemaStatementContext)

	// ExitStructDefinition is called when exiting the structDefinition production.
	ExitStructDefinition(c *StructDefinitionContext)

	// ExitTableDefinition is called when exiting the tableDefinition production.
	ExitTableDefinition(c *TableDefinitionContext)

	// ExitColumnDefinition is called when exiting the columnDefinition production.
	ExitColumnDefinition(c *ColumnDefinitionContext)

	// ExitFunctionColumnType is called when exiting the functionColumnType production.
	ExitFunctionColumnType(c *FunctionColumnTypeContext)

	// ExitColumnType is called when exiting the columnType production.
	ExitColumnType(c *ColumnTypeContext)

	// ExitPrimitiveType is called when exiting the primitiveType production.
	ExitPrimitiveType(c *PrimitiveTypeContext)

	// ExitVectorType is called when exiting the vectorType production.
	ExitVectorType(c *VectorTypeContext)

	// ExitVectorElementType is called when exiting the vectorElementType production.
	ExitVectorElementType(c *VectorElementTypeContext)

	// ExitNullColumnConstraint is called when exiting the nullColumnConstraint production.
	ExitNullColumnConstraint(c *NullColumnConstraintContext)

	// ExitPrimaryKeyDefinition is called when exiting the primaryKeyDefinition production.
	ExitPrimaryKeyDefinition(c *PrimaryKeyDefinitionContext)

	// ExitFullIdList is called when exiting the fullIdList production.
	ExitFullIdList(c *FullIdListContext)

	// ExitEnumDefinition is called when exiting the enumDefinition production.
	ExitEnumDefinition(c *EnumDefinitionContext)

	// ExitIndexAsSelectDefinition is called when exiting the indexAsSelectDefinition production.
	ExitIndexAsSelectDefinition(c *IndexAsSelectDefinitionContext)

	// ExitIndexOnSourceDefinition is called when exiting the indexOnSourceDefinition production.
	ExitIndexOnSourceDefinition(c *IndexOnSourceDefinitionContext)

	// ExitVectorIndexDefinition is called when exiting the vectorIndexDefinition production.
	ExitVectorIndexDefinition(c *VectorIndexDefinitionContext)

	// ExitIndexColumnList is called when exiting the indexColumnList production.
	ExitIndexColumnList(c *IndexColumnListContext)

	// ExitIndexColumnSpec is called when exiting the indexColumnSpec production.
	ExitIndexColumnSpec(c *IndexColumnSpecContext)

	// ExitIncludeClause is called when exiting the includeClause production.
	ExitIncludeClause(c *IncludeClauseContext)

	// ExitIndexType is called when exiting the indexType production.
	ExitIndexType(c *IndexTypeContext)

	// ExitIndexPartitionClause is called when exiting the indexPartitionClause production.
	ExitIndexPartitionClause(c *IndexPartitionClauseContext)

	// ExitIndexOptions is called when exiting the indexOptions production.
	ExitIndexOptions(c *IndexOptionsContext)

	// ExitIndexOption is called when exiting the indexOption production.
	ExitIndexOption(c *IndexOptionContext)

	// ExitVectorIndexOptions is called when exiting the vectorIndexOptions production.
	ExitVectorIndexOptions(c *VectorIndexOptionsContext)

	// ExitVectorIndexOption is called when exiting the vectorIndexOption production.
	ExitVectorIndexOption(c *VectorIndexOptionContext)

	// ExitHnswMetric is called when exiting the hnswMetric production.
	ExitHnswMetric(c *HnswMetricContext)

	// ExitIndexAttributes is called when exiting the indexAttributes production.
	ExitIndexAttributes(c *IndexAttributesContext)

	// ExitIndexAttribute is called when exiting the indexAttribute production.
	ExitIndexAttribute(c *IndexAttributeContext)

	// ExitCreateTempFunction is called when exiting the createTempFunction production.
	ExitCreateTempFunction(c *CreateTempFunctionContext)

	// ExitDropTempFunction is called when exiting the dropTempFunction production.
	ExitDropTempFunction(c *DropTempFunctionContext)

	// ExitViewDefinition is called when exiting the viewDefinition production.
	ExitViewDefinition(c *ViewDefinitionContext)

	// ExitTempSqlInvokedFunction is called when exiting the tempSqlInvokedFunction production.
	ExitTempSqlInvokedFunction(c *TempSqlInvokedFunctionContext)

	// ExitSqlInvokedFunction is called when exiting the sqlInvokedFunction production.
	ExitSqlInvokedFunction(c *SqlInvokedFunctionContext)

	// ExitFunctionSpecification is called when exiting the functionSpecification production.
	ExitFunctionSpecification(c *FunctionSpecificationContext)

	// ExitSqlParameterDeclarationList is called when exiting the sqlParameterDeclarationList production.
	ExitSqlParameterDeclarationList(c *SqlParameterDeclarationListContext)

	// ExitSqlParameterDeclarations is called when exiting the sqlParameterDeclarations production.
	ExitSqlParameterDeclarations(c *SqlParameterDeclarationsContext)

	// ExitSqlParameterDeclaration is called when exiting the sqlParameterDeclaration production.
	ExitSqlParameterDeclaration(c *SqlParameterDeclarationContext)

	// ExitParameterMode is called when exiting the parameterMode production.
	ExitParameterMode(c *ParameterModeContext)

	// ExitReturnsClause is called when exiting the returnsClause production.
	ExitReturnsClause(c *ReturnsClauseContext)

	// ExitReturnsType is called when exiting the returnsType production.
	ExitReturnsType(c *ReturnsTypeContext)

	// ExitReturnsTableType is called when exiting the returnsTableType production.
	ExitReturnsTableType(c *ReturnsTableTypeContext)

	// ExitTableFunctionColumnList is called when exiting the tableFunctionColumnList production.
	ExitTableFunctionColumnList(c *TableFunctionColumnListContext)

	// ExitTableFunctionColumnListElement is called when exiting the tableFunctionColumnListElement production.
	ExitTableFunctionColumnListElement(c *TableFunctionColumnListElementContext)

	// ExitRoutineCharacteristics is called when exiting the routineCharacteristics production.
	ExitRoutineCharacteristics(c *RoutineCharacteristicsContext)

	// ExitLanguageClause is called when exiting the languageClause production.
	ExitLanguageClause(c *LanguageClauseContext)

	// ExitLanguageName is called when exiting the languageName production.
	ExitLanguageName(c *LanguageNameContext)

	// ExitParameterStyle is called when exiting the parameterStyle production.
	ExitParameterStyle(c *ParameterStyleContext)

	// ExitDeterministicCharacteristic is called when exiting the deterministicCharacteristic production.
	ExitDeterministicCharacteristic(c *DeterministicCharacteristicContext)

	// ExitNullCallClause is called when exiting the nullCallClause production.
	ExitNullCallClause(c *NullCallClauseContext)

	// ExitDispatchClause is called when exiting the dispatchClause production.
	ExitDispatchClause(c *DispatchClauseContext)

	// ExitStatementBody is called when exiting the statementBody production.
	ExitStatementBody(c *StatementBodyContext)

	// ExitUserDefinedScalarFunctionStatementBody is called when exiting the userDefinedScalarFunctionStatementBody production.
	ExitUserDefinedScalarFunctionStatementBody(c *UserDefinedScalarFunctionStatementBodyContext)

	// ExitExpressionBody is called when exiting the expressionBody production.
	ExitExpressionBody(c *ExpressionBodyContext)

	// ExitSqlReturnStatement is called when exiting the sqlReturnStatement production.
	ExitSqlReturnStatement(c *SqlReturnStatementContext)

	// ExitReturnValue is called when exiting the returnValue production.
	ExitReturnValue(c *ReturnValueContext)

	// ExitCharSet is called when exiting the charSet production.
	ExitCharSet(c *CharSetContext)

	// ExitIntervalType is called when exiting the intervalType production.
	ExitIntervalType(c *IntervalTypeContext)

	// ExitSchemaId is called when exiting the schemaId production.
	ExitSchemaId(c *SchemaIdContext)

	// ExitPath is called when exiting the path production.
	ExitPath(c *PathContext)

	// ExitSchemaTemplateId is called when exiting the schemaTemplateId production.
	ExitSchemaTemplateId(c *SchemaTemplateIdContext)

	// ExitDeleteStatement is called when exiting the deleteStatement production.
	ExitDeleteStatement(c *DeleteStatementContext)

	// ExitInsertStatement is called when exiting the insertStatement production.
	ExitInsertStatement(c *InsertStatementContext)

	// ExitContinuationAtom is called when exiting the continuationAtom production.
	ExitContinuationAtom(c *ContinuationAtomContext)

	// ExitSelectStatement is called when exiting the selectStatement production.
	ExitSelectStatement(c *SelectStatementContext)

	// ExitQuery is called when exiting the query production.
	ExitQuery(c *QueryContext)

	// ExitCtes is called when exiting the ctes production.
	ExitCtes(c *CtesContext)

	// ExitTraversalOrderClause is called when exiting the traversalOrderClause production.
	ExitTraversalOrderClause(c *TraversalOrderClauseContext)

	// ExitNamedQuery is called when exiting the namedQuery production.
	ExitNamedQuery(c *NamedQueryContext)

	// ExitTableFunction is called when exiting the tableFunction production.
	ExitTableFunction(c *TableFunctionContext)

	// ExitTableFunctionArgs is called when exiting the tableFunctionArgs production.
	ExitTableFunctionArgs(c *TableFunctionArgsContext)

	// ExitTableFunctionName is called when exiting the tableFunctionName production.
	ExitTableFunctionName(c *TableFunctionNameContext)

	// ExitQueryTermDefault is called when exiting the queryTermDefault production.
	ExitQueryTermDefault(c *QueryTermDefaultContext)

	// ExitSetQuery is called when exiting the setQuery production.
	ExitSetQuery(c *SetQueryContext)

	// ExitInsertStatementValueSelect is called when exiting the insertStatementValueSelect production.
	ExitInsertStatementValueSelect(c *InsertStatementValueSelectContext)

	// ExitInsertStatementValueValues is called when exiting the insertStatementValueValues production.
	ExitInsertStatementValueValues(c *InsertStatementValueValuesContext)

	// ExitUpdatedElement is called when exiting the updatedElement production.
	ExitUpdatedElement(c *UpdatedElementContext)

	// ExitAssignmentField is called when exiting the assignmentField production.
	ExitAssignmentField(c *AssignmentFieldContext)

	// ExitUpdateStatement is called when exiting the updateStatement production.
	ExitUpdateStatement(c *UpdateStatementContext)

	// ExitOrderByClause is called when exiting the orderByClause production.
	ExitOrderByClause(c *OrderByClauseContext)

	// ExitOrderByExpression is called when exiting the orderByExpression production.
	ExitOrderByExpression(c *OrderByExpressionContext)

	// ExitOrderClause is called when exiting the orderClause production.
	ExitOrderClause(c *OrderClauseContext)

	// ExitTableSources is called when exiting the tableSources production.
	ExitTableSources(c *TableSourcesContext)

	// ExitTableSourceBase is called when exiting the tableSourceBase production.
	ExitTableSourceBase(c *TableSourceBaseContext)

	// ExitAtomTableItem is called when exiting the atomTableItem production.
	ExitAtomTableItem(c *AtomTableItemContext)

	// ExitSubqueryTableItem is called when exiting the subqueryTableItem production.
	ExitSubqueryTableItem(c *SubqueryTableItemContext)

	// ExitInlineTableItem is called when exiting the inlineTableItem production.
	ExitInlineTableItem(c *InlineTableItemContext)

	// ExitTableValuedFunction is called when exiting the tableValuedFunction production.
	ExitTableValuedFunction(c *TableValuedFunctionContext)

	// ExitIndexHint is called when exiting the indexHint production.
	ExitIndexHint(c *IndexHintContext)

	// ExitIndexHintType is called when exiting the indexHintType production.
	ExitIndexHintType(c *IndexHintTypeContext)

	// ExitInlineTableDefinition is called when exiting the inlineTableDefinition production.
	ExitInlineTableDefinition(c *InlineTableDefinitionContext)

	// ExitInnerJoin is called when exiting the innerJoin production.
	ExitInnerJoin(c *InnerJoinContext)

	// ExitStraightJoin is called when exiting the straightJoin production.
	ExitStraightJoin(c *StraightJoinContext)

	// ExitOuterJoin is called when exiting the outerJoin production.
	ExitOuterJoin(c *OuterJoinContext)

	// ExitNaturalJoin is called when exiting the naturalJoin production.
	ExitNaturalJoin(c *NaturalJoinContext)

	// ExitSimpleTable is called when exiting the simpleTable production.
	ExitSimpleTable(c *SimpleTableContext)

	// ExitParenthesisQuery is called when exiting the parenthesisQuery production.
	ExitParenthesisQuery(c *ParenthesisQueryContext)

	// ExitSelectElements is called when exiting the selectElements production.
	ExitSelectElements(c *SelectElementsContext)

	// ExitSelectStarElement is called when exiting the selectStarElement production.
	ExitSelectStarElement(c *SelectStarElementContext)

	// ExitSelectQualifierStarElement is called when exiting the selectQualifierStarElement production.
	ExitSelectQualifierStarElement(c *SelectQualifierStarElementContext)

	// ExitSelectExpressionElement is called when exiting the selectExpressionElement production.
	ExitSelectExpressionElement(c *SelectExpressionElementContext)

	// ExitFromClause is called when exiting the fromClause production.
	ExitFromClause(c *FromClauseContext)

	// ExitGroupByClause is called when exiting the groupByClause production.
	ExitGroupByClause(c *GroupByClauseContext)

	// ExitWhereExpr is called when exiting the whereExpr production.
	ExitWhereExpr(c *WhereExprContext)

	// ExitHavingClause is called when exiting the havingClause production.
	ExitHavingClause(c *HavingClauseContext)

	// ExitQualifyClause is called when exiting the qualifyClause production.
	ExitQualifyClause(c *QualifyClauseContext)

	// ExitGroupByItem is called when exiting the groupByItem production.
	ExitGroupByItem(c *GroupByItemContext)

	// ExitLimitClause is called when exiting the limitClause production.
	ExitLimitClause(c *LimitClauseContext)

	// ExitLimitClauseAtom is called when exiting the limitClauseAtom production.
	ExitLimitClauseAtom(c *LimitClauseAtomContext)

	// ExitQueryOptions is called when exiting the queryOptions production.
	ExitQueryOptions(c *QueryOptionsContext)

	// ExitQueryOption is called when exiting the queryOption production.
	ExitQueryOption(c *QueryOptionContext)

	// ExitStartTransaction is called when exiting the startTransaction production.
	ExitStartTransaction(c *StartTransactionContext)

	// ExitCommitStatement is called when exiting the commitStatement production.
	ExitCommitStatement(c *CommitStatementContext)

	// ExitRollbackStatement is called when exiting the rollbackStatement production.
	ExitRollbackStatement(c *RollbackStatementContext)

	// ExitSetAutocommitStatement is called when exiting the setAutocommitStatement production.
	ExitSetAutocommitStatement(c *SetAutocommitStatementContext)

	// ExitSetTransactionStatement is called when exiting the setTransactionStatement production.
	ExitSetTransactionStatement(c *SetTransactionStatementContext)

	// ExitTransactionOption is called when exiting the transactionOption production.
	ExitTransactionOption(c *TransactionOptionContext)

	// ExitTransactionLevel is called when exiting the transactionLevel production.
	ExitTransactionLevel(c *TransactionLevelContext)

	// ExitPrepareStatement is called when exiting the prepareStatement production.
	ExitPrepareStatement(c *PrepareStatementContext)

	// ExitExecuteStatement is called when exiting the executeStatement production.
	ExitExecuteStatement(c *ExecuteStatementContext)

	// ExitShowDatabasesStatement is called when exiting the showDatabasesStatement production.
	ExitShowDatabasesStatement(c *ShowDatabasesStatementContext)

	// ExitShowSchemaTemplatesStatement is called when exiting the showSchemaTemplatesStatement production.
	ExitShowSchemaTemplatesStatement(c *ShowSchemaTemplatesStatementContext)

	// ExitSetVariable is called when exiting the setVariable production.
	ExitSetVariable(c *SetVariableContext)

	// ExitSetCharset is called when exiting the setCharset production.
	ExitSetCharset(c *SetCharsetContext)

	// ExitSetNames is called when exiting the setNames production.
	ExitSetNames(c *SetNamesContext)

	// ExitSetTransaction is called when exiting the setTransaction production.
	ExitSetTransaction(c *SetTransactionContext)

	// ExitSetAutocommit is called when exiting the setAutocommit production.
	ExitSetAutocommit(c *SetAutocommitContext)

	// ExitSetNewValueInsideTrigger is called when exiting the setNewValueInsideTrigger production.
	ExitSetNewValueInsideTrigger(c *SetNewValueInsideTriggerContext)

	// ExitVariableClause is called when exiting the variableClause production.
	ExitVariableClause(c *VariableClauseContext)

	// ExitKillStatement is called when exiting the killStatement production.
	ExitKillStatement(c *KillStatementContext)

	// ExitResetStatement is called when exiting the resetStatement production.
	ExitResetStatement(c *ResetStatementContext)

	// ExitExecuteContinuationStatement is called when exiting the executeContinuationStatement production.
	ExitExecuteContinuationStatement(c *ExecuteContinuationStatementContext)

	// ExitCopyExportStatement is called when exiting the copyExportStatement production.
	ExitCopyExportStatement(c *CopyExportStatementContext)

	// ExitCopyImportStatement is called when exiting the copyImportStatement production.
	ExitCopyImportStatement(c *CopyImportStatementContext)

	// ExitTableIndexes is called when exiting the tableIndexes production.
	ExitTableIndexes(c *TableIndexesContext)

	// ExitLoadedTableIndexes is called when exiting the loadedTableIndexes production.
	ExitLoadedTableIndexes(c *LoadedTableIndexesContext)

	// ExitSimpleDescribeSchemaStatement is called when exiting the simpleDescribeSchemaStatement production.
	ExitSimpleDescribeSchemaStatement(c *SimpleDescribeSchemaStatementContext)

	// ExitSimpleDescribeSchemaTemplateStatement is called when exiting the simpleDescribeSchemaTemplateStatement production.
	ExitSimpleDescribeSchemaTemplateStatement(c *SimpleDescribeSchemaTemplateStatementContext)

	// ExitFullDescribeStatement is called when exiting the fullDescribeStatement production.
	ExitFullDescribeStatement(c *FullDescribeStatementContext)

	// ExitHelpStatement is called when exiting the helpStatement production.
	ExitHelpStatement(c *HelpStatementContext)

	// ExitDescribeStatements is called when exiting the describeStatements production.
	ExitDescribeStatements(c *DescribeStatementsContext)

	// ExitDescribeConnection is called when exiting the describeConnection production.
	ExitDescribeConnection(c *DescribeConnectionContext)

	// ExitFullId is called when exiting the fullId production.
	ExitFullId(c *FullIdContext)

	// ExitTableName is called when exiting the tableName production.
	ExitTableName(c *TableNameContext)

	// ExitFullColumnName is called when exiting the fullColumnName production.
	ExitFullColumnName(c *FullColumnNameContext)

	// ExitIndexColumnName is called when exiting the indexColumnName production.
	ExitIndexColumnName(c *IndexColumnNameContext)

	// ExitCharsetName is called when exiting the charsetName production.
	ExitCharsetName(c *CharsetNameContext)

	// ExitCollationName is called when exiting the collationName production.
	ExitCollationName(c *CollationNameContext)

	// ExitUid is called when exiting the uid production.
	ExitUid(c *UidContext)

	// ExitSimpleId is called when exiting the simpleId production.
	ExitSimpleId(c *SimpleIdContext)

	// ExitNullNotnull is called when exiting the nullNotnull production.
	ExitNullNotnull(c *NullNotnullContext)

	// ExitDecimalLiteral is called when exiting the decimalLiteral production.
	ExitDecimalLiteral(c *DecimalLiteralContext)

	// ExitStringLiteral is called when exiting the stringLiteral production.
	ExitStringLiteral(c *StringLiteralContext)

	// ExitBooleanLiteral is called when exiting the booleanLiteral production.
	ExitBooleanLiteral(c *BooleanLiteralContext)

	// ExitBytesLiteral is called when exiting the bytesLiteral production.
	ExitBytesLiteral(c *BytesLiteralContext)

	// ExitNullLiteral is called when exiting the nullLiteral production.
	ExitNullLiteral(c *NullLiteralContext)

	// ExitStringConstant is called when exiting the stringConstant production.
	ExitStringConstant(c *StringConstantContext)

	// ExitDecimalConstant is called when exiting the decimalConstant production.
	ExitDecimalConstant(c *DecimalConstantContext)

	// ExitNegativeDecimalConstant is called when exiting the negativeDecimalConstant production.
	ExitNegativeDecimalConstant(c *NegativeDecimalConstantContext)

	// ExitBytesConstant is called when exiting the bytesConstant production.
	ExitBytesConstant(c *BytesConstantContext)

	// ExitBooleanConstant is called when exiting the booleanConstant production.
	ExitBooleanConstant(c *BooleanConstantContext)

	// ExitBitStringConstant is called when exiting the bitStringConstant production.
	ExitBitStringConstant(c *BitStringConstantContext)

	// ExitNullConstant is called when exiting the nullConstant production.
	ExitNullConstant(c *NullConstantContext)

	// ExitStringDataType is called when exiting the stringDataType production.
	ExitStringDataType(c *StringDataTypeContext)

	// ExitNationalStringDataType is called when exiting the nationalStringDataType production.
	ExitNationalStringDataType(c *NationalStringDataTypeContext)

	// ExitNationalVaryingStringDataType is called when exiting the nationalVaryingStringDataType production.
	ExitNationalVaryingStringDataType(c *NationalVaryingStringDataTypeContext)

	// ExitDimensionDataType is called when exiting the dimensionDataType production.
	ExitDimensionDataType(c *DimensionDataTypeContext)

	// ExitSimpleDataType is called when exiting the simpleDataType production.
	ExitSimpleDataType(c *SimpleDataTypeContext)

	// ExitCollectionDataType is called when exiting the collectionDataType production.
	ExitCollectionDataType(c *CollectionDataTypeContext)

	// ExitSpatialDataType is called when exiting the spatialDataType production.
	ExitSpatialDataType(c *SpatialDataTypeContext)

	// ExitLongVarcharDataType is called when exiting the longVarcharDataType production.
	ExitLongVarcharDataType(c *LongVarcharDataTypeContext)

	// ExitLongVarbinaryDataType is called when exiting the longVarbinaryDataType production.
	ExitLongVarbinaryDataType(c *LongVarbinaryDataTypeContext)

	// ExitCollectionOptions is called when exiting the collectionOptions production.
	ExitCollectionOptions(c *CollectionOptionsContext)

	// ExitConvertedDataType is called when exiting the convertedDataType production.
	ExitConvertedDataType(c *ConvertedDataTypeContext)

	// ExitLengthOneDimension is called when exiting the lengthOneDimension production.
	ExitLengthOneDimension(c *LengthOneDimensionContext)

	// ExitLengthTwoDimension is called when exiting the lengthTwoDimension production.
	ExitLengthTwoDimension(c *LengthTwoDimensionContext)

	// ExitLengthTwoOptionalDimension is called when exiting the lengthTwoOptionalDimension production.
	ExitLengthTwoOptionalDimension(c *LengthTwoOptionalDimensionContext)

	// ExitUidList is called when exiting the uidList production.
	ExitUidList(c *UidListContext)

	// ExitUidWithNestings is called when exiting the uidWithNestings production.
	ExitUidWithNestings(c *UidWithNestingsContext)

	// ExitUidListWithNestingsInParens is called when exiting the uidListWithNestingsInParens production.
	ExitUidListWithNestingsInParens(c *UidListWithNestingsInParensContext)

	// ExitUidListWithNestings is called when exiting the uidListWithNestings production.
	ExitUidListWithNestings(c *UidListWithNestingsContext)

	// ExitTables is called when exiting the tables production.
	ExitTables(c *TablesContext)

	// ExitIndexColumnNames is called when exiting the indexColumnNames production.
	ExitIndexColumnNames(c *IndexColumnNamesContext)

	// ExitExpressions is called when exiting the expressions production.
	ExitExpressions(c *ExpressionsContext)

	// ExitExpressionsWithDefaults is called when exiting the expressionsWithDefaults production.
	ExitExpressionsWithDefaults(c *ExpressionsWithDefaultsContext)

	// ExitRecordConstructorForInsert is called when exiting the recordConstructorForInsert production.
	ExitRecordConstructorForInsert(c *RecordConstructorForInsertContext)

	// ExitRecordConstructorForInlineTable is called when exiting the recordConstructorForInlineTable production.
	ExitRecordConstructorForInlineTable(c *RecordConstructorForInlineTableContext)

	// ExitRecordConstructor is called when exiting the recordConstructor production.
	ExitRecordConstructor(c *RecordConstructorContext)

	// ExitOfTypeClause is called when exiting the ofTypeClause production.
	ExitOfTypeClause(c *OfTypeClauseContext)

	// ExitArrayConstructor is called when exiting the arrayConstructor production.
	ExitArrayConstructor(c *ArrayConstructorContext)

	// ExitUserVariables is called when exiting the userVariables production.
	ExitUserVariables(c *UserVariablesContext)

	// ExitDefaultValue is called when exiting the defaultValue production.
	ExitDefaultValue(c *DefaultValueContext)

	// ExitCurrentTimestamp is called when exiting the currentTimestamp production.
	ExitCurrentTimestamp(c *CurrentTimestampContext)

	// ExitExpressionOrDefault is called when exiting the expressionOrDefault production.
	ExitExpressionOrDefault(c *ExpressionOrDefaultContext)

	// ExitExpressionWithOptionalName is called when exiting the expressionWithOptionalName production.
	ExitExpressionWithOptionalName(c *ExpressionWithOptionalNameContext)

	// ExitIfExists is called when exiting the ifExists production.
	ExitIfExists(c *IfExistsContext)

	// ExitIfNotExists is called when exiting the ifNotExists production.
	ExitIfNotExists(c *IfNotExistsContext)

	// ExitAggregateFunctionCall is called when exiting the aggregateFunctionCall production.
	ExitAggregateFunctionCall(c *AggregateFunctionCallContext)

	// ExitNonAggregateFunctionCall is called when exiting the nonAggregateFunctionCall production.
	ExitNonAggregateFunctionCall(c *NonAggregateFunctionCallContext)

	// ExitSpecificFunctionCall is called when exiting the specificFunctionCall production.
	ExitSpecificFunctionCall(c *SpecificFunctionCallContext)

	// ExitScalarFunctionCall is called when exiting the scalarFunctionCall production.
	ExitScalarFunctionCall(c *ScalarFunctionCallContext)

	// ExitUserDefinedScalarFunctionCall is called when exiting the userDefinedScalarFunctionCall production.
	ExitUserDefinedScalarFunctionCall(c *UserDefinedScalarFunctionCallContext)

	// ExitSimpleFunctionCall is called when exiting the simpleFunctionCall production.
	ExitSimpleFunctionCall(c *SimpleFunctionCallContext)

	// ExitDataTypeFunctionCall is called when exiting the dataTypeFunctionCall production.
	ExitDataTypeFunctionCall(c *DataTypeFunctionCallContext)

	// ExitValuesFunctionCall is called when exiting the valuesFunctionCall production.
	ExitValuesFunctionCall(c *ValuesFunctionCallContext)

	// ExitCaseExpressionFunctionCall is called when exiting the caseExpressionFunctionCall production.
	ExitCaseExpressionFunctionCall(c *CaseExpressionFunctionCallContext)

	// ExitCaseFunctionCall is called when exiting the caseFunctionCall production.
	ExitCaseFunctionCall(c *CaseFunctionCallContext)

	// ExitCharFunctionCall is called when exiting the charFunctionCall production.
	ExitCharFunctionCall(c *CharFunctionCallContext)

	// ExitPositionFunctionCall is called when exiting the positionFunctionCall production.
	ExitPositionFunctionCall(c *PositionFunctionCallContext)

	// ExitSubstrFunctionCall is called when exiting the substrFunctionCall production.
	ExitSubstrFunctionCall(c *SubstrFunctionCallContext)

	// ExitTrimFunctionCall is called when exiting the trimFunctionCall production.
	ExitTrimFunctionCall(c *TrimFunctionCallContext)

	// ExitWeightFunctionCall is called when exiting the weightFunctionCall production.
	ExitWeightFunctionCall(c *WeightFunctionCallContext)

	// ExitExtractFunctionCall is called when exiting the extractFunctionCall production.
	ExitExtractFunctionCall(c *ExtractFunctionCallContext)

	// ExitGetFormatFunctionCall is called when exiting the getFormatFunctionCall production.
	ExitGetFormatFunctionCall(c *GetFormatFunctionCallContext)

	// ExitCaseFuncAlternative is called when exiting the caseFuncAlternative production.
	ExitCaseFuncAlternative(c *CaseFuncAlternativeContext)

	// ExitLevelWeightList is called when exiting the levelWeightList production.
	ExitLevelWeightList(c *LevelWeightListContext)

	// ExitLevelWeightRange is called when exiting the levelWeightRange production.
	ExitLevelWeightRange(c *LevelWeightRangeContext)

	// ExitLevelInWeightListElement is called when exiting the levelInWeightListElement production.
	ExitLevelInWeightListElement(c *LevelInWeightListElementContext)

	// ExitAggregateWindowedFunction is called when exiting the aggregateWindowedFunction production.
	ExitAggregateWindowedFunction(c *AggregateWindowedFunctionContext)

	// ExitNonAggregateWindowedFunction is called when exiting the nonAggregateWindowedFunction production.
	ExitNonAggregateWindowedFunction(c *NonAggregateWindowedFunctionContext)

	// ExitOverClause is called when exiting the overClause production.
	ExitOverClause(c *OverClauseContext)

	// ExitWindowName is called when exiting the windowName production.
	ExitWindowName(c *WindowNameContext)

	// ExitWindowSpec is called when exiting the windowSpec production.
	ExitWindowSpec(c *WindowSpecContext)

	// ExitWindowOptionsClause is called when exiting the windowOptionsClause production.
	ExitWindowOptionsClause(c *WindowOptionsClauseContext)

	// ExitWindowOption is called when exiting the windowOption production.
	ExitWindowOption(c *WindowOptionContext)

	// ExitPartitionClause is called when exiting the partitionClause production.
	ExitPartitionClause(c *PartitionClauseContext)

	// ExitScalarFunctionName is called when exiting the scalarFunctionName production.
	ExitScalarFunctionName(c *ScalarFunctionNameContext)

	// ExitUserDefinedScalarFunctionName is called when exiting the userDefinedScalarFunctionName production.
	ExitUserDefinedScalarFunctionName(c *UserDefinedScalarFunctionNameContext)

	// ExitFunctionArgs is called when exiting the functionArgs production.
	ExitFunctionArgs(c *FunctionArgsContext)

	// ExitFunctionArg is called when exiting the functionArg production.
	ExitFunctionArg(c *FunctionArgContext)

	// ExitNamedFunctionArg is called when exiting the namedFunctionArg production.
	ExitNamedFunctionArg(c *NamedFunctionArgContext)

	// ExitPredicatedExpression is called when exiting the predicatedExpression production.
	ExitPredicatedExpression(c *PredicatedExpressionContext)

	// ExitNotExpression is called when exiting the notExpression production.
	ExitNotExpression(c *NotExpressionContext)

	// ExitLogicalExpression is called when exiting the logicalExpression production.
	ExitLogicalExpression(c *LogicalExpressionContext)

	// ExitExistsExpressionAtom is called when exiting the existsExpressionAtom production.
	ExitExistsExpressionAtom(c *ExistsExpressionAtomContext)

	// ExitBetweenComparisonPredicate is called when exiting the betweenComparisonPredicate production.
	ExitBetweenComparisonPredicate(c *BetweenComparisonPredicateContext)

	// ExitInPredicate is called when exiting the inPredicate production.
	ExitInPredicate(c *InPredicateContext)

	// ExitLikePredicate is called when exiting the likePredicate production.
	ExitLikePredicate(c *LikePredicateContext)

	// ExitIsExpression is called when exiting the isExpression production.
	ExitIsExpression(c *IsExpressionContext)

	// ExitSubqueryExpressionAtom is called when exiting the subqueryExpressionAtom production.
	ExitSubqueryExpressionAtom(c *SubqueryExpressionAtomContext)

	// ExitBinaryComparisonPredicate is called when exiting the binaryComparisonPredicate production.
	ExitBinaryComparisonPredicate(c *BinaryComparisonPredicateContext)

	// ExitSubscriptExpression is called when exiting the subscriptExpression production.
	ExitSubscriptExpression(c *SubscriptExpressionContext)

	// ExitConstantExpressionAtom is called when exiting the constantExpressionAtom production.
	ExitConstantExpressionAtom(c *ConstantExpressionAtomContext)

	// ExitFunctionCallExpressionAtom is called when exiting the functionCallExpressionAtom production.
	ExitFunctionCallExpressionAtom(c *FunctionCallExpressionAtomContext)

	// ExitFullColumnNameExpressionAtom is called when exiting the fullColumnNameExpressionAtom production.
	ExitFullColumnNameExpressionAtom(c *FullColumnNameExpressionAtomContext)

	// ExitBitExpressionAtom is called when exiting the bitExpressionAtom production.
	ExitBitExpressionAtom(c *BitExpressionAtomContext)

	// ExitPreparedStatementParameterAtom is called when exiting the preparedStatementParameterAtom production.
	ExitPreparedStatementParameterAtom(c *PreparedStatementParameterAtomContext)

	// ExitRecordConstructorExpressionAtom is called when exiting the recordConstructorExpressionAtom production.
	ExitRecordConstructorExpressionAtom(c *RecordConstructorExpressionAtomContext)

	// ExitArrayConstructorExpressionAtom is called when exiting the arrayConstructorExpressionAtom production.
	ExitArrayConstructorExpressionAtom(c *ArrayConstructorExpressionAtomContext)

	// ExitMathExpressionAtom is called when exiting the mathExpressionAtom production.
	ExitMathExpressionAtom(c *MathExpressionAtomContext)

	// ExitInList is called when exiting the inList production.
	ExitInList(c *InListContext)

	// ExitPreparedStatementParameter is called when exiting the preparedStatementParameter production.
	ExitPreparedStatementParameter(c *PreparedStatementParameterContext)

	// ExitUnaryOperator is called when exiting the unaryOperator production.
	ExitUnaryOperator(c *UnaryOperatorContext)

	// ExitComparisonOperator is called when exiting the comparisonOperator production.
	ExitComparisonOperator(c *ComparisonOperatorContext)

	// ExitLogicalOperator is called when exiting the logicalOperator production.
	ExitLogicalOperator(c *LogicalOperatorContext)

	// ExitBitOperator is called when exiting the bitOperator production.
	ExitBitOperator(c *BitOperatorContext)

	// ExitMathOperator is called when exiting the mathOperator production.
	ExitMathOperator(c *MathOperatorContext)

	// ExitJsonOperator is called when exiting the jsonOperator production.
	ExitJsonOperator(c *JsonOperatorContext)

	// ExitCharsetNameBase is called when exiting the charsetNameBase production.
	ExitCharsetNameBase(c *CharsetNameBaseContext)

	// ExitIntervalTypeBase is called when exiting the intervalTypeBase production.
	ExitIntervalTypeBase(c *IntervalTypeBaseContext)

	// ExitKeywordsCanBeId is called when exiting the keywordsCanBeId production.
	ExitKeywordsCanBeId(c *KeywordsCanBeIdContext)

	// ExitFunctionNameBase is called when exiting the functionNameBase production.
	ExitFunctionNameBase(c *FunctionNameBaseContext)
}

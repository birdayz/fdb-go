// Code generated from RelationalParser.g4 by ANTLR 4.13.1. DO NOT EDIT.

package antlrgen // RelationalParser
import "github.com/antlr4-go/antlr/v4"

// BaseRelationalParserListener is a complete listener for a parse tree produced by RelationalParser.
type BaseRelationalParserListener struct{}

var _ RelationalParserListener = &BaseRelationalParserListener{}

// VisitTerminal is called when a terminal node is visited.
func (s *BaseRelationalParserListener) VisitTerminal(node antlr.TerminalNode) {}

// VisitErrorNode is called when an error node is visited.
func (s *BaseRelationalParserListener) VisitErrorNode(node antlr.ErrorNode) {}

// EnterEveryRule is called when any rule is entered.
func (s *BaseRelationalParserListener) EnterEveryRule(ctx antlr.ParserRuleContext) {}

// ExitEveryRule is called when any rule is exited.
func (s *BaseRelationalParserListener) ExitEveryRule(ctx antlr.ParserRuleContext) {}

// EnterRoot is called when production root is entered.
func (s *BaseRelationalParserListener) EnterRoot(ctx *RootContext) {}

// ExitRoot is called when production root is exited.
func (s *BaseRelationalParserListener) ExitRoot(ctx *RootContext) {}

// EnterStatements is called when production statements is entered.
func (s *BaseRelationalParserListener) EnterStatements(ctx *StatementsContext) {}

// ExitStatements is called when production statements is exited.
func (s *BaseRelationalParserListener) ExitStatements(ctx *StatementsContext) {}

// EnterStatement is called when production statement is entered.
func (s *BaseRelationalParserListener) EnterStatement(ctx *StatementContext) {}

// ExitStatement is called when production statement is exited.
func (s *BaseRelationalParserListener) ExitStatement(ctx *StatementContext) {}

// EnterDmlStatement is called when production dmlStatement is entered.
func (s *BaseRelationalParserListener) EnterDmlStatement(ctx *DmlStatementContext) {}

// ExitDmlStatement is called when production dmlStatement is exited.
func (s *BaseRelationalParserListener) ExitDmlStatement(ctx *DmlStatementContext) {}

// EnterDdlStatement is called when production ddlStatement is entered.
func (s *BaseRelationalParserListener) EnterDdlStatement(ctx *DdlStatementContext) {}

// ExitDdlStatement is called when production ddlStatement is exited.
func (s *BaseRelationalParserListener) ExitDdlStatement(ctx *DdlStatementContext) {}

// EnterTransactionStatement is called when production transactionStatement is entered.
func (s *BaseRelationalParserListener) EnterTransactionStatement(ctx *TransactionStatementContext) {}

// ExitTransactionStatement is called when production transactionStatement is exited.
func (s *BaseRelationalParserListener) ExitTransactionStatement(ctx *TransactionStatementContext) {}

// EnterPreparedStatement is called when production preparedStatement is entered.
func (s *BaseRelationalParserListener) EnterPreparedStatement(ctx *PreparedStatementContext) {}

// ExitPreparedStatement is called when production preparedStatement is exited.
func (s *BaseRelationalParserListener) ExitPreparedStatement(ctx *PreparedStatementContext) {}

// EnterAdministrationStatement is called when production administrationStatement is entered.
func (s *BaseRelationalParserListener) EnterAdministrationStatement(ctx *AdministrationStatementContext) {
}

// ExitAdministrationStatement is called when production administrationStatement is exited.
func (s *BaseRelationalParserListener) ExitAdministrationStatement(ctx *AdministrationStatementContext) {
}

// EnterUtilityStatement is called when production utilityStatement is entered.
func (s *BaseRelationalParserListener) EnterUtilityStatement(ctx *UtilityStatementContext) {}

// ExitUtilityStatement is called when production utilityStatement is exited.
func (s *BaseRelationalParserListener) ExitUtilityStatement(ctx *UtilityStatementContext) {}

// EnterTemplateClause is called when production templateClause is entered.
func (s *BaseRelationalParserListener) EnterTemplateClause(ctx *TemplateClauseContext) {}

// ExitTemplateClause is called when production templateClause is exited.
func (s *BaseRelationalParserListener) ExitTemplateClause(ctx *TemplateClauseContext) {}

// EnterCreateSchemaStatement is called when production createSchemaStatement is entered.
func (s *BaseRelationalParserListener) EnterCreateSchemaStatement(ctx *CreateSchemaStatementContext) {
}

// ExitCreateSchemaStatement is called when production createSchemaStatement is exited.
func (s *BaseRelationalParserListener) ExitCreateSchemaStatement(ctx *CreateSchemaStatementContext) {}

// EnterCreateSchemaTemplateStatement is called when production createSchemaTemplateStatement is entered.
func (s *BaseRelationalParserListener) EnterCreateSchemaTemplateStatement(ctx *CreateSchemaTemplateStatementContext) {
}

// ExitCreateSchemaTemplateStatement is called when production createSchemaTemplateStatement is exited.
func (s *BaseRelationalParserListener) ExitCreateSchemaTemplateStatement(ctx *CreateSchemaTemplateStatementContext) {
}

// EnterCreateDatabaseStatement is called when production createDatabaseStatement is entered.
func (s *BaseRelationalParserListener) EnterCreateDatabaseStatement(ctx *CreateDatabaseStatementContext) {
}

// ExitCreateDatabaseStatement is called when production createDatabaseStatement is exited.
func (s *BaseRelationalParserListener) ExitCreateDatabaseStatement(ctx *CreateDatabaseStatementContext) {
}

// EnterOptionsClause is called when production optionsClause is entered.
func (s *BaseRelationalParserListener) EnterOptionsClause(ctx *OptionsClauseContext) {}

// ExitOptionsClause is called when production optionsClause is exited.
func (s *BaseRelationalParserListener) ExitOptionsClause(ctx *OptionsClauseContext) {}

// EnterOption is called when production option is entered.
func (s *BaseRelationalParserListener) EnterOption(ctx *OptionContext) {}

// ExitOption is called when production option is exited.
func (s *BaseRelationalParserListener) ExitOption(ctx *OptionContext) {}

// EnterDropDatabaseStatement is called when production dropDatabaseStatement is entered.
func (s *BaseRelationalParserListener) EnterDropDatabaseStatement(ctx *DropDatabaseStatementContext) {
}

// ExitDropDatabaseStatement is called when production dropDatabaseStatement is exited.
func (s *BaseRelationalParserListener) ExitDropDatabaseStatement(ctx *DropDatabaseStatementContext) {}

// EnterDropSchemaTemplateStatement is called when production dropSchemaTemplateStatement is entered.
func (s *BaseRelationalParserListener) EnterDropSchemaTemplateStatement(ctx *DropSchemaTemplateStatementContext) {
}

// ExitDropSchemaTemplateStatement is called when production dropSchemaTemplateStatement is exited.
func (s *BaseRelationalParserListener) ExitDropSchemaTemplateStatement(ctx *DropSchemaTemplateStatementContext) {
}

// EnterDropSchemaStatement is called when production dropSchemaStatement is entered.
func (s *BaseRelationalParserListener) EnterDropSchemaStatement(ctx *DropSchemaStatementContext) {}

// ExitDropSchemaStatement is called when production dropSchemaStatement is exited.
func (s *BaseRelationalParserListener) ExitDropSchemaStatement(ctx *DropSchemaStatementContext) {}

// EnterStructDefinition is called when production structDefinition is entered.
func (s *BaseRelationalParserListener) EnterStructDefinition(ctx *StructDefinitionContext) {}

// ExitStructDefinition is called when production structDefinition is exited.
func (s *BaseRelationalParserListener) ExitStructDefinition(ctx *StructDefinitionContext) {}

// EnterTableDefinition is called when production tableDefinition is entered.
func (s *BaseRelationalParserListener) EnterTableDefinition(ctx *TableDefinitionContext) {}

// ExitTableDefinition is called when production tableDefinition is exited.
func (s *BaseRelationalParserListener) ExitTableDefinition(ctx *TableDefinitionContext) {}

// EnterColumnDefinition is called when production columnDefinition is entered.
func (s *BaseRelationalParserListener) EnterColumnDefinition(ctx *ColumnDefinitionContext) {}

// ExitColumnDefinition is called when production columnDefinition is exited.
func (s *BaseRelationalParserListener) ExitColumnDefinition(ctx *ColumnDefinitionContext) {}

// EnterFunctionColumnType is called when production functionColumnType is entered.
func (s *BaseRelationalParserListener) EnterFunctionColumnType(ctx *FunctionColumnTypeContext) {}

// ExitFunctionColumnType is called when production functionColumnType is exited.
func (s *BaseRelationalParserListener) ExitFunctionColumnType(ctx *FunctionColumnTypeContext) {}

// EnterColumnType is called when production columnType is entered.
func (s *BaseRelationalParserListener) EnterColumnType(ctx *ColumnTypeContext) {}

// ExitColumnType is called when production columnType is exited.
func (s *BaseRelationalParserListener) ExitColumnType(ctx *ColumnTypeContext) {}

// EnterPrimitiveType is called when production primitiveType is entered.
func (s *BaseRelationalParserListener) EnterPrimitiveType(ctx *PrimitiveTypeContext) {}

// ExitPrimitiveType is called when production primitiveType is exited.
func (s *BaseRelationalParserListener) ExitPrimitiveType(ctx *PrimitiveTypeContext) {}

// EnterVectorType is called when production vectorType is entered.
func (s *BaseRelationalParserListener) EnterVectorType(ctx *VectorTypeContext) {}

// ExitVectorType is called when production vectorType is exited.
func (s *BaseRelationalParserListener) ExitVectorType(ctx *VectorTypeContext) {}

// EnterVectorElementType is called when production vectorElementType is entered.
func (s *BaseRelationalParserListener) EnterVectorElementType(ctx *VectorElementTypeContext) {}

// ExitVectorElementType is called when production vectorElementType is exited.
func (s *BaseRelationalParserListener) ExitVectorElementType(ctx *VectorElementTypeContext) {}

// EnterNullColumnConstraint is called when production nullColumnConstraint is entered.
func (s *BaseRelationalParserListener) EnterNullColumnConstraint(ctx *NullColumnConstraintContext) {}

// ExitNullColumnConstraint is called when production nullColumnConstraint is exited.
func (s *BaseRelationalParserListener) ExitNullColumnConstraint(ctx *NullColumnConstraintContext) {}

// EnterPrimaryKeyDefinition is called when production primaryKeyDefinition is entered.
func (s *BaseRelationalParserListener) EnterPrimaryKeyDefinition(ctx *PrimaryKeyDefinitionContext) {}

// ExitPrimaryKeyDefinition is called when production primaryKeyDefinition is exited.
func (s *BaseRelationalParserListener) ExitPrimaryKeyDefinition(ctx *PrimaryKeyDefinitionContext) {}

// EnterFullIdList is called when production fullIdList is entered.
func (s *BaseRelationalParserListener) EnterFullIdList(ctx *FullIdListContext) {}

// ExitFullIdList is called when production fullIdList is exited.
func (s *BaseRelationalParserListener) ExitFullIdList(ctx *FullIdListContext) {}

// EnterEnumDefinition is called when production enumDefinition is entered.
func (s *BaseRelationalParserListener) EnterEnumDefinition(ctx *EnumDefinitionContext) {}

// ExitEnumDefinition is called when production enumDefinition is exited.
func (s *BaseRelationalParserListener) ExitEnumDefinition(ctx *EnumDefinitionContext) {}

// EnterIndexAsSelectDefinition is called when production indexAsSelectDefinition is entered.
func (s *BaseRelationalParserListener) EnterIndexAsSelectDefinition(ctx *IndexAsSelectDefinitionContext) {
}

// ExitIndexAsSelectDefinition is called when production indexAsSelectDefinition is exited.
func (s *BaseRelationalParserListener) ExitIndexAsSelectDefinition(ctx *IndexAsSelectDefinitionContext) {
}

// EnterIndexOnSourceDefinition is called when production indexOnSourceDefinition is entered.
func (s *BaseRelationalParserListener) EnterIndexOnSourceDefinition(ctx *IndexOnSourceDefinitionContext) {
}

// ExitIndexOnSourceDefinition is called when production indexOnSourceDefinition is exited.
func (s *BaseRelationalParserListener) ExitIndexOnSourceDefinition(ctx *IndexOnSourceDefinitionContext) {
}

// EnterVectorIndexDefinition is called when production vectorIndexDefinition is entered.
func (s *BaseRelationalParserListener) EnterVectorIndexDefinition(ctx *VectorIndexDefinitionContext) {
}

// ExitVectorIndexDefinition is called when production vectorIndexDefinition is exited.
func (s *BaseRelationalParserListener) ExitVectorIndexDefinition(ctx *VectorIndexDefinitionContext) {}

// EnterIndexColumnList is called when production indexColumnList is entered.
func (s *BaseRelationalParserListener) EnterIndexColumnList(ctx *IndexColumnListContext) {}

// ExitIndexColumnList is called when production indexColumnList is exited.
func (s *BaseRelationalParserListener) ExitIndexColumnList(ctx *IndexColumnListContext) {}

// EnterIndexColumnSpec is called when production indexColumnSpec is entered.
func (s *BaseRelationalParserListener) EnterIndexColumnSpec(ctx *IndexColumnSpecContext) {}

// ExitIndexColumnSpec is called when production indexColumnSpec is exited.
func (s *BaseRelationalParserListener) ExitIndexColumnSpec(ctx *IndexColumnSpecContext) {}

// EnterIncludeClause is called when production includeClause is entered.
func (s *BaseRelationalParserListener) EnterIncludeClause(ctx *IncludeClauseContext) {}

// ExitIncludeClause is called when production includeClause is exited.
func (s *BaseRelationalParserListener) ExitIncludeClause(ctx *IncludeClauseContext) {}

// EnterIndexType is called when production indexType is entered.
func (s *BaseRelationalParserListener) EnterIndexType(ctx *IndexTypeContext) {}

// ExitIndexType is called when production indexType is exited.
func (s *BaseRelationalParserListener) ExitIndexType(ctx *IndexTypeContext) {}

// EnterIndexPartitionClause is called when production indexPartitionClause is entered.
func (s *BaseRelationalParserListener) EnterIndexPartitionClause(ctx *IndexPartitionClauseContext) {}

// ExitIndexPartitionClause is called when production indexPartitionClause is exited.
func (s *BaseRelationalParserListener) ExitIndexPartitionClause(ctx *IndexPartitionClauseContext) {}

// EnterIndexOptions is called when production indexOptions is entered.
func (s *BaseRelationalParserListener) EnterIndexOptions(ctx *IndexOptionsContext) {}

// ExitIndexOptions is called when production indexOptions is exited.
func (s *BaseRelationalParserListener) ExitIndexOptions(ctx *IndexOptionsContext) {}

// EnterIndexOption is called when production indexOption is entered.
func (s *BaseRelationalParserListener) EnterIndexOption(ctx *IndexOptionContext) {}

// ExitIndexOption is called when production indexOption is exited.
func (s *BaseRelationalParserListener) ExitIndexOption(ctx *IndexOptionContext) {}

// EnterVectorIndexOptions is called when production vectorIndexOptions is entered.
func (s *BaseRelationalParserListener) EnterVectorIndexOptions(ctx *VectorIndexOptionsContext) {}

// ExitVectorIndexOptions is called when production vectorIndexOptions is exited.
func (s *BaseRelationalParserListener) ExitVectorIndexOptions(ctx *VectorIndexOptionsContext) {}

// EnterVectorIndexOption is called when production vectorIndexOption is entered.
func (s *BaseRelationalParserListener) EnterVectorIndexOption(ctx *VectorIndexOptionContext) {}

// ExitVectorIndexOption is called when production vectorIndexOption is exited.
func (s *BaseRelationalParserListener) ExitVectorIndexOption(ctx *VectorIndexOptionContext) {}

// EnterHnswMetric is called when production hnswMetric is entered.
func (s *BaseRelationalParserListener) EnterHnswMetric(ctx *HnswMetricContext) {}

// ExitHnswMetric is called when production hnswMetric is exited.
func (s *BaseRelationalParserListener) ExitHnswMetric(ctx *HnswMetricContext) {}

// EnterIndexAttributes is called when production indexAttributes is entered.
func (s *BaseRelationalParserListener) EnterIndexAttributes(ctx *IndexAttributesContext) {}

// ExitIndexAttributes is called when production indexAttributes is exited.
func (s *BaseRelationalParserListener) ExitIndexAttributes(ctx *IndexAttributesContext) {}

// EnterIndexAttribute is called when production indexAttribute is entered.
func (s *BaseRelationalParserListener) EnterIndexAttribute(ctx *IndexAttributeContext) {}

// ExitIndexAttribute is called when production indexAttribute is exited.
func (s *BaseRelationalParserListener) ExitIndexAttribute(ctx *IndexAttributeContext) {}

// EnterCreateTempFunction is called when production createTempFunction is entered.
func (s *BaseRelationalParserListener) EnterCreateTempFunction(ctx *CreateTempFunctionContext) {}

// ExitCreateTempFunction is called when production createTempFunction is exited.
func (s *BaseRelationalParserListener) ExitCreateTempFunction(ctx *CreateTempFunctionContext) {}

// EnterDropTempFunction is called when production dropTempFunction is entered.
func (s *BaseRelationalParserListener) EnterDropTempFunction(ctx *DropTempFunctionContext) {}

// ExitDropTempFunction is called when production dropTempFunction is exited.
func (s *BaseRelationalParserListener) ExitDropTempFunction(ctx *DropTempFunctionContext) {}

// EnterViewDefinition is called when production viewDefinition is entered.
func (s *BaseRelationalParserListener) EnterViewDefinition(ctx *ViewDefinitionContext) {}

// ExitViewDefinition is called when production viewDefinition is exited.
func (s *BaseRelationalParserListener) ExitViewDefinition(ctx *ViewDefinitionContext) {}

// EnterTempSqlInvokedFunction is called when production tempSqlInvokedFunction is entered.
func (s *BaseRelationalParserListener) EnterTempSqlInvokedFunction(ctx *TempSqlInvokedFunctionContext) {
}

// ExitTempSqlInvokedFunction is called when production tempSqlInvokedFunction is exited.
func (s *BaseRelationalParserListener) ExitTempSqlInvokedFunction(ctx *TempSqlInvokedFunctionContext) {
}

// EnterSqlInvokedFunction is called when production sqlInvokedFunction is entered.
func (s *BaseRelationalParserListener) EnterSqlInvokedFunction(ctx *SqlInvokedFunctionContext) {}

// ExitSqlInvokedFunction is called when production sqlInvokedFunction is exited.
func (s *BaseRelationalParserListener) ExitSqlInvokedFunction(ctx *SqlInvokedFunctionContext) {}

// EnterFunctionSpecification is called when production functionSpecification is entered.
func (s *BaseRelationalParserListener) EnterFunctionSpecification(ctx *FunctionSpecificationContext) {
}

// ExitFunctionSpecification is called when production functionSpecification is exited.
func (s *BaseRelationalParserListener) ExitFunctionSpecification(ctx *FunctionSpecificationContext) {}

// EnterSqlParameterDeclarationList is called when production sqlParameterDeclarationList is entered.
func (s *BaseRelationalParserListener) EnterSqlParameterDeclarationList(ctx *SqlParameterDeclarationListContext) {
}

// ExitSqlParameterDeclarationList is called when production sqlParameterDeclarationList is exited.
func (s *BaseRelationalParserListener) ExitSqlParameterDeclarationList(ctx *SqlParameterDeclarationListContext) {
}

// EnterSqlParameterDeclarations is called when production sqlParameterDeclarations is entered.
func (s *BaseRelationalParserListener) EnterSqlParameterDeclarations(ctx *SqlParameterDeclarationsContext) {
}

// ExitSqlParameterDeclarations is called when production sqlParameterDeclarations is exited.
func (s *BaseRelationalParserListener) ExitSqlParameterDeclarations(ctx *SqlParameterDeclarationsContext) {
}

// EnterSqlParameterDeclaration is called when production sqlParameterDeclaration is entered.
func (s *BaseRelationalParserListener) EnterSqlParameterDeclaration(ctx *SqlParameterDeclarationContext) {
}

// ExitSqlParameterDeclaration is called when production sqlParameterDeclaration is exited.
func (s *BaseRelationalParserListener) ExitSqlParameterDeclaration(ctx *SqlParameterDeclarationContext) {
}

// EnterParameterMode is called when production parameterMode is entered.
func (s *BaseRelationalParserListener) EnterParameterMode(ctx *ParameterModeContext) {}

// ExitParameterMode is called when production parameterMode is exited.
func (s *BaseRelationalParserListener) ExitParameterMode(ctx *ParameterModeContext) {}

// EnterReturnsClause is called when production returnsClause is entered.
func (s *BaseRelationalParserListener) EnterReturnsClause(ctx *ReturnsClauseContext) {}

// ExitReturnsClause is called when production returnsClause is exited.
func (s *BaseRelationalParserListener) ExitReturnsClause(ctx *ReturnsClauseContext) {}

// EnterReturnsType is called when production returnsType is entered.
func (s *BaseRelationalParserListener) EnterReturnsType(ctx *ReturnsTypeContext) {}

// ExitReturnsType is called when production returnsType is exited.
func (s *BaseRelationalParserListener) ExitReturnsType(ctx *ReturnsTypeContext) {}

// EnterReturnsTableType is called when production returnsTableType is entered.
func (s *BaseRelationalParserListener) EnterReturnsTableType(ctx *ReturnsTableTypeContext) {}

// ExitReturnsTableType is called when production returnsTableType is exited.
func (s *BaseRelationalParserListener) ExitReturnsTableType(ctx *ReturnsTableTypeContext) {}

// EnterTableFunctionColumnList is called when production tableFunctionColumnList is entered.
func (s *BaseRelationalParserListener) EnterTableFunctionColumnList(ctx *TableFunctionColumnListContext) {
}

// ExitTableFunctionColumnList is called when production tableFunctionColumnList is exited.
func (s *BaseRelationalParserListener) ExitTableFunctionColumnList(ctx *TableFunctionColumnListContext) {
}

// EnterTableFunctionColumnListElement is called when production tableFunctionColumnListElement is entered.
func (s *BaseRelationalParserListener) EnterTableFunctionColumnListElement(ctx *TableFunctionColumnListElementContext) {
}

// ExitTableFunctionColumnListElement is called when production tableFunctionColumnListElement is exited.
func (s *BaseRelationalParserListener) ExitTableFunctionColumnListElement(ctx *TableFunctionColumnListElementContext) {
}

// EnterRoutineCharacteristics is called when production routineCharacteristics is entered.
func (s *BaseRelationalParserListener) EnterRoutineCharacteristics(ctx *RoutineCharacteristicsContext) {
}

// ExitRoutineCharacteristics is called when production routineCharacteristics is exited.
func (s *BaseRelationalParserListener) ExitRoutineCharacteristics(ctx *RoutineCharacteristicsContext) {
}

// EnterLanguageClause is called when production languageClause is entered.
func (s *BaseRelationalParserListener) EnterLanguageClause(ctx *LanguageClauseContext) {}

// ExitLanguageClause is called when production languageClause is exited.
func (s *BaseRelationalParserListener) ExitLanguageClause(ctx *LanguageClauseContext) {}

// EnterLanguageName is called when production languageName is entered.
func (s *BaseRelationalParserListener) EnterLanguageName(ctx *LanguageNameContext) {}

// ExitLanguageName is called when production languageName is exited.
func (s *BaseRelationalParserListener) ExitLanguageName(ctx *LanguageNameContext) {}

// EnterParameterStyle is called when production parameterStyle is entered.
func (s *BaseRelationalParserListener) EnterParameterStyle(ctx *ParameterStyleContext) {}

// ExitParameterStyle is called when production parameterStyle is exited.
func (s *BaseRelationalParserListener) ExitParameterStyle(ctx *ParameterStyleContext) {}

// EnterDeterministicCharacteristic is called when production deterministicCharacteristic is entered.
func (s *BaseRelationalParserListener) EnterDeterministicCharacteristic(ctx *DeterministicCharacteristicContext) {
}

// ExitDeterministicCharacteristic is called when production deterministicCharacteristic is exited.
func (s *BaseRelationalParserListener) ExitDeterministicCharacteristic(ctx *DeterministicCharacteristicContext) {
}

// EnterNullCallClause is called when production nullCallClause is entered.
func (s *BaseRelationalParserListener) EnterNullCallClause(ctx *NullCallClauseContext) {}

// ExitNullCallClause is called when production nullCallClause is exited.
func (s *BaseRelationalParserListener) ExitNullCallClause(ctx *NullCallClauseContext) {}

// EnterDispatchClause is called when production dispatchClause is entered.
func (s *BaseRelationalParserListener) EnterDispatchClause(ctx *DispatchClauseContext) {}

// ExitDispatchClause is called when production dispatchClause is exited.
func (s *BaseRelationalParserListener) ExitDispatchClause(ctx *DispatchClauseContext) {}

// EnterStatementBody is called when production statementBody is entered.
func (s *BaseRelationalParserListener) EnterStatementBody(ctx *StatementBodyContext) {}

// ExitStatementBody is called when production statementBody is exited.
func (s *BaseRelationalParserListener) ExitStatementBody(ctx *StatementBodyContext) {}

// EnterUserDefinedScalarFunctionStatementBody is called when production userDefinedScalarFunctionStatementBody is entered.
func (s *BaseRelationalParserListener) EnterUserDefinedScalarFunctionStatementBody(ctx *UserDefinedScalarFunctionStatementBodyContext) {
}

// ExitUserDefinedScalarFunctionStatementBody is called when production userDefinedScalarFunctionStatementBody is exited.
func (s *BaseRelationalParserListener) ExitUserDefinedScalarFunctionStatementBody(ctx *UserDefinedScalarFunctionStatementBodyContext) {
}

// EnterExpressionBody is called when production expressionBody is entered.
func (s *BaseRelationalParserListener) EnterExpressionBody(ctx *ExpressionBodyContext) {}

// ExitExpressionBody is called when production expressionBody is exited.
func (s *BaseRelationalParserListener) ExitExpressionBody(ctx *ExpressionBodyContext) {}

// EnterSqlReturnStatement is called when production sqlReturnStatement is entered.
func (s *BaseRelationalParserListener) EnterSqlReturnStatement(ctx *SqlReturnStatementContext) {}

// ExitSqlReturnStatement is called when production sqlReturnStatement is exited.
func (s *BaseRelationalParserListener) ExitSqlReturnStatement(ctx *SqlReturnStatementContext) {}

// EnterReturnValue is called when production returnValue is entered.
func (s *BaseRelationalParserListener) EnterReturnValue(ctx *ReturnValueContext) {}

// ExitReturnValue is called when production returnValue is exited.
func (s *BaseRelationalParserListener) ExitReturnValue(ctx *ReturnValueContext) {}

// EnterCharSet is called when production charSet is entered.
func (s *BaseRelationalParserListener) EnterCharSet(ctx *CharSetContext) {}

// ExitCharSet is called when production charSet is exited.
func (s *BaseRelationalParserListener) ExitCharSet(ctx *CharSetContext) {}

// EnterIntervalType is called when production intervalType is entered.
func (s *BaseRelationalParserListener) EnterIntervalType(ctx *IntervalTypeContext) {}

// ExitIntervalType is called when production intervalType is exited.
func (s *BaseRelationalParserListener) ExitIntervalType(ctx *IntervalTypeContext) {}

// EnterSchemaId is called when production schemaId is entered.
func (s *BaseRelationalParserListener) EnterSchemaId(ctx *SchemaIdContext) {}

// ExitSchemaId is called when production schemaId is exited.
func (s *BaseRelationalParserListener) ExitSchemaId(ctx *SchemaIdContext) {}

// EnterPath is called when production path is entered.
func (s *BaseRelationalParserListener) EnterPath(ctx *PathContext) {}

// ExitPath is called when production path is exited.
func (s *BaseRelationalParserListener) ExitPath(ctx *PathContext) {}

// EnterSchemaTemplateId is called when production schemaTemplateId is entered.
func (s *BaseRelationalParserListener) EnterSchemaTemplateId(ctx *SchemaTemplateIdContext) {}

// ExitSchemaTemplateId is called when production schemaTemplateId is exited.
func (s *BaseRelationalParserListener) ExitSchemaTemplateId(ctx *SchemaTemplateIdContext) {}

// EnterDeleteStatement is called when production deleteStatement is entered.
func (s *BaseRelationalParserListener) EnterDeleteStatement(ctx *DeleteStatementContext) {}

// ExitDeleteStatement is called when production deleteStatement is exited.
func (s *BaseRelationalParserListener) ExitDeleteStatement(ctx *DeleteStatementContext) {}

// EnterInsertStatement is called when production insertStatement is entered.
func (s *BaseRelationalParserListener) EnterInsertStatement(ctx *InsertStatementContext) {}

// ExitInsertStatement is called when production insertStatement is exited.
func (s *BaseRelationalParserListener) ExitInsertStatement(ctx *InsertStatementContext) {}

// EnterContinuationAtom is called when production continuationAtom is entered.
func (s *BaseRelationalParserListener) EnterContinuationAtom(ctx *ContinuationAtomContext) {}

// ExitContinuationAtom is called when production continuationAtom is exited.
func (s *BaseRelationalParserListener) ExitContinuationAtom(ctx *ContinuationAtomContext) {}

// EnterSelectStatement is called when production selectStatement is entered.
func (s *BaseRelationalParserListener) EnterSelectStatement(ctx *SelectStatementContext) {}

// ExitSelectStatement is called when production selectStatement is exited.
func (s *BaseRelationalParserListener) ExitSelectStatement(ctx *SelectStatementContext) {}

// EnterQuery is called when production query is entered.
func (s *BaseRelationalParserListener) EnterQuery(ctx *QueryContext) {}

// ExitQuery is called when production query is exited.
func (s *BaseRelationalParserListener) ExitQuery(ctx *QueryContext) {}

// EnterCtes is called when production ctes is entered.
func (s *BaseRelationalParserListener) EnterCtes(ctx *CtesContext) {}

// ExitCtes is called when production ctes is exited.
func (s *BaseRelationalParserListener) ExitCtes(ctx *CtesContext) {}

// EnterTraversalOrderClause is called when production traversalOrderClause is entered.
func (s *BaseRelationalParserListener) EnterTraversalOrderClause(ctx *TraversalOrderClauseContext) {}

// ExitTraversalOrderClause is called when production traversalOrderClause is exited.
func (s *BaseRelationalParserListener) ExitTraversalOrderClause(ctx *TraversalOrderClauseContext) {}

// EnterNamedQuery is called when production namedQuery is entered.
func (s *BaseRelationalParserListener) EnterNamedQuery(ctx *NamedQueryContext) {}

// ExitNamedQuery is called when production namedQuery is exited.
func (s *BaseRelationalParserListener) ExitNamedQuery(ctx *NamedQueryContext) {}

// EnterTableFunction is called when production tableFunction is entered.
func (s *BaseRelationalParserListener) EnterTableFunction(ctx *TableFunctionContext) {}

// ExitTableFunction is called when production tableFunction is exited.
func (s *BaseRelationalParserListener) ExitTableFunction(ctx *TableFunctionContext) {}

// EnterTableFunctionArgs is called when production tableFunctionArgs is entered.
func (s *BaseRelationalParserListener) EnterTableFunctionArgs(ctx *TableFunctionArgsContext) {}

// ExitTableFunctionArgs is called when production tableFunctionArgs is exited.
func (s *BaseRelationalParserListener) ExitTableFunctionArgs(ctx *TableFunctionArgsContext) {}

// EnterTableFunctionName is called when production tableFunctionName is entered.
func (s *BaseRelationalParserListener) EnterTableFunctionName(ctx *TableFunctionNameContext) {}

// ExitTableFunctionName is called when production tableFunctionName is exited.
func (s *BaseRelationalParserListener) ExitTableFunctionName(ctx *TableFunctionNameContext) {}

// EnterQueryTermDefault is called when production queryTermDefault is entered.
func (s *BaseRelationalParserListener) EnterQueryTermDefault(ctx *QueryTermDefaultContext) {}

// ExitQueryTermDefault is called when production queryTermDefault is exited.
func (s *BaseRelationalParserListener) ExitQueryTermDefault(ctx *QueryTermDefaultContext) {}

// EnterSetQuery is called when production setQuery is entered.
func (s *BaseRelationalParserListener) EnterSetQuery(ctx *SetQueryContext) {}

// ExitSetQuery is called when production setQuery is exited.
func (s *BaseRelationalParserListener) ExitSetQuery(ctx *SetQueryContext) {}

// EnterInsertStatementValueSelect is called when production insertStatementValueSelect is entered.
func (s *BaseRelationalParserListener) EnterInsertStatementValueSelect(ctx *InsertStatementValueSelectContext) {
}

// ExitInsertStatementValueSelect is called when production insertStatementValueSelect is exited.
func (s *BaseRelationalParserListener) ExitInsertStatementValueSelect(ctx *InsertStatementValueSelectContext) {
}

// EnterInsertStatementValueValues is called when production insertStatementValueValues is entered.
func (s *BaseRelationalParserListener) EnterInsertStatementValueValues(ctx *InsertStatementValueValuesContext) {
}

// ExitInsertStatementValueValues is called when production insertStatementValueValues is exited.
func (s *BaseRelationalParserListener) ExitInsertStatementValueValues(ctx *InsertStatementValueValuesContext) {
}

// EnterUpdatedElement is called when production updatedElement is entered.
func (s *BaseRelationalParserListener) EnterUpdatedElement(ctx *UpdatedElementContext) {}

// ExitUpdatedElement is called when production updatedElement is exited.
func (s *BaseRelationalParserListener) ExitUpdatedElement(ctx *UpdatedElementContext) {}

// EnterAssignmentField is called when production assignmentField is entered.
func (s *BaseRelationalParserListener) EnterAssignmentField(ctx *AssignmentFieldContext) {}

// ExitAssignmentField is called when production assignmentField is exited.
func (s *BaseRelationalParserListener) ExitAssignmentField(ctx *AssignmentFieldContext) {}

// EnterUpdateStatement is called when production updateStatement is entered.
func (s *BaseRelationalParserListener) EnterUpdateStatement(ctx *UpdateStatementContext) {}

// ExitUpdateStatement is called when production updateStatement is exited.
func (s *BaseRelationalParserListener) ExitUpdateStatement(ctx *UpdateStatementContext) {}

// EnterOrderByClause is called when production orderByClause is entered.
func (s *BaseRelationalParserListener) EnterOrderByClause(ctx *OrderByClauseContext) {}

// ExitOrderByClause is called when production orderByClause is exited.
func (s *BaseRelationalParserListener) ExitOrderByClause(ctx *OrderByClauseContext) {}

// EnterOrderByExpression is called when production orderByExpression is entered.
func (s *BaseRelationalParserListener) EnterOrderByExpression(ctx *OrderByExpressionContext) {}

// ExitOrderByExpression is called when production orderByExpression is exited.
func (s *BaseRelationalParserListener) ExitOrderByExpression(ctx *OrderByExpressionContext) {}

// EnterOrderClause is called when production orderClause is entered.
func (s *BaseRelationalParserListener) EnterOrderClause(ctx *OrderClauseContext) {}

// ExitOrderClause is called when production orderClause is exited.
func (s *BaseRelationalParserListener) ExitOrderClause(ctx *OrderClauseContext) {}

// EnterTableSources is called when production tableSources is entered.
func (s *BaseRelationalParserListener) EnterTableSources(ctx *TableSourcesContext) {}

// ExitTableSources is called when production tableSources is exited.
func (s *BaseRelationalParserListener) ExitTableSources(ctx *TableSourcesContext) {}

// EnterTableSourceBase is called when production tableSourceBase is entered.
func (s *BaseRelationalParserListener) EnterTableSourceBase(ctx *TableSourceBaseContext) {}

// ExitTableSourceBase is called when production tableSourceBase is exited.
func (s *BaseRelationalParserListener) ExitTableSourceBase(ctx *TableSourceBaseContext) {}

// EnterAtomTableItem is called when production atomTableItem is entered.
func (s *BaseRelationalParserListener) EnterAtomTableItem(ctx *AtomTableItemContext) {}

// ExitAtomTableItem is called when production atomTableItem is exited.
func (s *BaseRelationalParserListener) ExitAtomTableItem(ctx *AtomTableItemContext) {}

// EnterSubqueryTableItem is called when production subqueryTableItem is entered.
func (s *BaseRelationalParserListener) EnterSubqueryTableItem(ctx *SubqueryTableItemContext) {}

// ExitSubqueryTableItem is called when production subqueryTableItem is exited.
func (s *BaseRelationalParserListener) ExitSubqueryTableItem(ctx *SubqueryTableItemContext) {}

// EnterInlineTableItem is called when production inlineTableItem is entered.
func (s *BaseRelationalParserListener) EnterInlineTableItem(ctx *InlineTableItemContext) {}

// ExitInlineTableItem is called when production inlineTableItem is exited.
func (s *BaseRelationalParserListener) ExitInlineTableItem(ctx *InlineTableItemContext) {}

// EnterTableValuedFunction is called when production tableValuedFunction is entered.
func (s *BaseRelationalParserListener) EnterTableValuedFunction(ctx *TableValuedFunctionContext) {}

// ExitTableValuedFunction is called when production tableValuedFunction is exited.
func (s *BaseRelationalParserListener) ExitTableValuedFunction(ctx *TableValuedFunctionContext) {}

// EnterIndexHint is called when production indexHint is entered.
func (s *BaseRelationalParserListener) EnterIndexHint(ctx *IndexHintContext) {}

// ExitIndexHint is called when production indexHint is exited.
func (s *BaseRelationalParserListener) ExitIndexHint(ctx *IndexHintContext) {}

// EnterIndexHintType is called when production indexHintType is entered.
func (s *BaseRelationalParserListener) EnterIndexHintType(ctx *IndexHintTypeContext) {}

// ExitIndexHintType is called when production indexHintType is exited.
func (s *BaseRelationalParserListener) ExitIndexHintType(ctx *IndexHintTypeContext) {}

// EnterInlineTableDefinition is called when production inlineTableDefinition is entered.
func (s *BaseRelationalParserListener) EnterInlineTableDefinition(ctx *InlineTableDefinitionContext) {
}

// ExitInlineTableDefinition is called when production inlineTableDefinition is exited.
func (s *BaseRelationalParserListener) ExitInlineTableDefinition(ctx *InlineTableDefinitionContext) {}

// EnterInnerJoin is called when production innerJoin is entered.
func (s *BaseRelationalParserListener) EnterInnerJoin(ctx *InnerJoinContext) {}

// ExitInnerJoin is called when production innerJoin is exited.
func (s *BaseRelationalParserListener) ExitInnerJoin(ctx *InnerJoinContext) {}

// EnterStraightJoin is called when production straightJoin is entered.
func (s *BaseRelationalParserListener) EnterStraightJoin(ctx *StraightJoinContext) {}

// ExitStraightJoin is called when production straightJoin is exited.
func (s *BaseRelationalParserListener) ExitStraightJoin(ctx *StraightJoinContext) {}

// EnterOuterJoin is called when production outerJoin is entered.
func (s *BaseRelationalParserListener) EnterOuterJoin(ctx *OuterJoinContext) {}

// ExitOuterJoin is called when production outerJoin is exited.
func (s *BaseRelationalParserListener) ExitOuterJoin(ctx *OuterJoinContext) {}

// EnterNaturalJoin is called when production naturalJoin is entered.
func (s *BaseRelationalParserListener) EnterNaturalJoin(ctx *NaturalJoinContext) {}

// ExitNaturalJoin is called when production naturalJoin is exited.
func (s *BaseRelationalParserListener) ExitNaturalJoin(ctx *NaturalJoinContext) {}

// EnterSimpleTable is called when production simpleTable is entered.
func (s *BaseRelationalParserListener) EnterSimpleTable(ctx *SimpleTableContext) {}

// ExitSimpleTable is called when production simpleTable is exited.
func (s *BaseRelationalParserListener) ExitSimpleTable(ctx *SimpleTableContext) {}

// EnterParenthesisQuery is called when production parenthesisQuery is entered.
func (s *BaseRelationalParserListener) EnterParenthesisQuery(ctx *ParenthesisQueryContext) {}

// ExitParenthesisQuery is called when production parenthesisQuery is exited.
func (s *BaseRelationalParserListener) ExitParenthesisQuery(ctx *ParenthesisQueryContext) {}

// EnterSelectElements is called when production selectElements is entered.
func (s *BaseRelationalParserListener) EnterSelectElements(ctx *SelectElementsContext) {}

// ExitSelectElements is called when production selectElements is exited.
func (s *BaseRelationalParserListener) ExitSelectElements(ctx *SelectElementsContext) {}

// EnterSelectStarElement is called when production selectStarElement is entered.
func (s *BaseRelationalParserListener) EnterSelectStarElement(ctx *SelectStarElementContext) {}

// ExitSelectStarElement is called when production selectStarElement is exited.
func (s *BaseRelationalParserListener) ExitSelectStarElement(ctx *SelectStarElementContext) {}

// EnterSelectQualifierStarElement is called when production selectQualifierStarElement is entered.
func (s *BaseRelationalParserListener) EnterSelectQualifierStarElement(ctx *SelectQualifierStarElementContext) {
}

// ExitSelectQualifierStarElement is called when production selectQualifierStarElement is exited.
func (s *BaseRelationalParserListener) ExitSelectQualifierStarElement(ctx *SelectQualifierStarElementContext) {
}

// EnterSelectExpressionElement is called when production selectExpressionElement is entered.
func (s *BaseRelationalParserListener) EnterSelectExpressionElement(ctx *SelectExpressionElementContext) {
}

// ExitSelectExpressionElement is called when production selectExpressionElement is exited.
func (s *BaseRelationalParserListener) ExitSelectExpressionElement(ctx *SelectExpressionElementContext) {
}

// EnterFromClause is called when production fromClause is entered.
func (s *BaseRelationalParserListener) EnterFromClause(ctx *FromClauseContext) {}

// ExitFromClause is called when production fromClause is exited.
func (s *BaseRelationalParserListener) ExitFromClause(ctx *FromClauseContext) {}

// EnterGroupByClause is called when production groupByClause is entered.
func (s *BaseRelationalParserListener) EnterGroupByClause(ctx *GroupByClauseContext) {}

// ExitGroupByClause is called when production groupByClause is exited.
func (s *BaseRelationalParserListener) ExitGroupByClause(ctx *GroupByClauseContext) {}

// EnterWhereExpr is called when production whereExpr is entered.
func (s *BaseRelationalParserListener) EnterWhereExpr(ctx *WhereExprContext) {}

// ExitWhereExpr is called when production whereExpr is exited.
func (s *BaseRelationalParserListener) ExitWhereExpr(ctx *WhereExprContext) {}

// EnterHavingClause is called when production havingClause is entered.
func (s *BaseRelationalParserListener) EnterHavingClause(ctx *HavingClauseContext) {}

// ExitHavingClause is called when production havingClause is exited.
func (s *BaseRelationalParserListener) ExitHavingClause(ctx *HavingClauseContext) {}

// EnterQualifyClause is called when production qualifyClause is entered.
func (s *BaseRelationalParserListener) EnterQualifyClause(ctx *QualifyClauseContext) {}

// ExitQualifyClause is called when production qualifyClause is exited.
func (s *BaseRelationalParserListener) ExitQualifyClause(ctx *QualifyClauseContext) {}

// EnterGroupByItem is called when production groupByItem is entered.
func (s *BaseRelationalParserListener) EnterGroupByItem(ctx *GroupByItemContext) {}

// ExitGroupByItem is called when production groupByItem is exited.
func (s *BaseRelationalParserListener) ExitGroupByItem(ctx *GroupByItemContext) {}

// EnterLimitClause is called when production limitClause is entered.
func (s *BaseRelationalParserListener) EnterLimitClause(ctx *LimitClauseContext) {}

// ExitLimitClause is called when production limitClause is exited.
func (s *BaseRelationalParserListener) ExitLimitClause(ctx *LimitClauseContext) {}

// EnterLimitClauseAtom is called when production limitClauseAtom is entered.
func (s *BaseRelationalParserListener) EnterLimitClauseAtom(ctx *LimitClauseAtomContext) {}

// ExitLimitClauseAtom is called when production limitClauseAtom is exited.
func (s *BaseRelationalParserListener) ExitLimitClauseAtom(ctx *LimitClauseAtomContext) {}

// EnterQueryOptions is called when production queryOptions is entered.
func (s *BaseRelationalParserListener) EnterQueryOptions(ctx *QueryOptionsContext) {}

// ExitQueryOptions is called when production queryOptions is exited.
func (s *BaseRelationalParserListener) ExitQueryOptions(ctx *QueryOptionsContext) {}

// EnterQueryOption is called when production queryOption is entered.
func (s *BaseRelationalParserListener) EnterQueryOption(ctx *QueryOptionContext) {}

// ExitQueryOption is called when production queryOption is exited.
func (s *BaseRelationalParserListener) ExitQueryOption(ctx *QueryOptionContext) {}

// EnterStartTransaction is called when production startTransaction is entered.
func (s *BaseRelationalParserListener) EnterStartTransaction(ctx *StartTransactionContext) {}

// ExitStartTransaction is called when production startTransaction is exited.
func (s *BaseRelationalParserListener) ExitStartTransaction(ctx *StartTransactionContext) {}

// EnterCommitStatement is called when production commitStatement is entered.
func (s *BaseRelationalParserListener) EnterCommitStatement(ctx *CommitStatementContext) {}

// ExitCommitStatement is called when production commitStatement is exited.
func (s *BaseRelationalParserListener) ExitCommitStatement(ctx *CommitStatementContext) {}

// EnterRollbackStatement is called when production rollbackStatement is entered.
func (s *BaseRelationalParserListener) EnterRollbackStatement(ctx *RollbackStatementContext) {}

// ExitRollbackStatement is called when production rollbackStatement is exited.
func (s *BaseRelationalParserListener) ExitRollbackStatement(ctx *RollbackStatementContext) {}

// EnterSetAutocommitStatement is called when production setAutocommitStatement is entered.
func (s *BaseRelationalParserListener) EnterSetAutocommitStatement(ctx *SetAutocommitStatementContext) {
}

// ExitSetAutocommitStatement is called when production setAutocommitStatement is exited.
func (s *BaseRelationalParserListener) ExitSetAutocommitStatement(ctx *SetAutocommitStatementContext) {
}

// EnterSetTransactionStatement is called when production setTransactionStatement is entered.
func (s *BaseRelationalParserListener) EnterSetTransactionStatement(ctx *SetTransactionStatementContext) {
}

// ExitSetTransactionStatement is called when production setTransactionStatement is exited.
func (s *BaseRelationalParserListener) ExitSetTransactionStatement(ctx *SetTransactionStatementContext) {
}

// EnterTransactionOption is called when production transactionOption is entered.
func (s *BaseRelationalParserListener) EnterTransactionOption(ctx *TransactionOptionContext) {}

// ExitTransactionOption is called when production transactionOption is exited.
func (s *BaseRelationalParserListener) ExitTransactionOption(ctx *TransactionOptionContext) {}

// EnterTransactionLevel is called when production transactionLevel is entered.
func (s *BaseRelationalParserListener) EnterTransactionLevel(ctx *TransactionLevelContext) {}

// ExitTransactionLevel is called when production transactionLevel is exited.
func (s *BaseRelationalParserListener) ExitTransactionLevel(ctx *TransactionLevelContext) {}

// EnterPrepareStatement is called when production prepareStatement is entered.
func (s *BaseRelationalParserListener) EnterPrepareStatement(ctx *PrepareStatementContext) {}

// ExitPrepareStatement is called when production prepareStatement is exited.
func (s *BaseRelationalParserListener) ExitPrepareStatement(ctx *PrepareStatementContext) {}

// EnterExecuteStatement is called when production executeStatement is entered.
func (s *BaseRelationalParserListener) EnterExecuteStatement(ctx *ExecuteStatementContext) {}

// ExitExecuteStatement is called when production executeStatement is exited.
func (s *BaseRelationalParserListener) ExitExecuteStatement(ctx *ExecuteStatementContext) {}

// EnterShowDatabasesStatement is called when production showDatabasesStatement is entered.
func (s *BaseRelationalParserListener) EnterShowDatabasesStatement(ctx *ShowDatabasesStatementContext) {
}

// ExitShowDatabasesStatement is called when production showDatabasesStatement is exited.
func (s *BaseRelationalParserListener) ExitShowDatabasesStatement(ctx *ShowDatabasesStatementContext) {
}

// EnterShowSchemaTemplatesStatement is called when production showSchemaTemplatesStatement is entered.
func (s *BaseRelationalParserListener) EnterShowSchemaTemplatesStatement(ctx *ShowSchemaTemplatesStatementContext) {
}

// ExitShowSchemaTemplatesStatement is called when production showSchemaTemplatesStatement is exited.
func (s *BaseRelationalParserListener) ExitShowSchemaTemplatesStatement(ctx *ShowSchemaTemplatesStatementContext) {
}

// EnterSetVariable is called when production setVariable is entered.
func (s *BaseRelationalParserListener) EnterSetVariable(ctx *SetVariableContext) {}

// ExitSetVariable is called when production setVariable is exited.
func (s *BaseRelationalParserListener) ExitSetVariable(ctx *SetVariableContext) {}

// EnterSetCharset is called when production setCharset is entered.
func (s *BaseRelationalParserListener) EnterSetCharset(ctx *SetCharsetContext) {}

// ExitSetCharset is called when production setCharset is exited.
func (s *BaseRelationalParserListener) ExitSetCharset(ctx *SetCharsetContext) {}

// EnterSetNames is called when production setNames is entered.
func (s *BaseRelationalParserListener) EnterSetNames(ctx *SetNamesContext) {}

// ExitSetNames is called when production setNames is exited.
func (s *BaseRelationalParserListener) ExitSetNames(ctx *SetNamesContext) {}

// EnterSetTransaction is called when production setTransaction is entered.
func (s *BaseRelationalParserListener) EnterSetTransaction(ctx *SetTransactionContext) {}

// ExitSetTransaction is called when production setTransaction is exited.
func (s *BaseRelationalParserListener) ExitSetTransaction(ctx *SetTransactionContext) {}

// EnterSetAutocommit is called when production setAutocommit is entered.
func (s *BaseRelationalParserListener) EnterSetAutocommit(ctx *SetAutocommitContext) {}

// ExitSetAutocommit is called when production setAutocommit is exited.
func (s *BaseRelationalParserListener) ExitSetAutocommit(ctx *SetAutocommitContext) {}

// EnterSetNewValueInsideTrigger is called when production setNewValueInsideTrigger is entered.
func (s *BaseRelationalParserListener) EnterSetNewValueInsideTrigger(ctx *SetNewValueInsideTriggerContext) {
}

// ExitSetNewValueInsideTrigger is called when production setNewValueInsideTrigger is exited.
func (s *BaseRelationalParserListener) ExitSetNewValueInsideTrigger(ctx *SetNewValueInsideTriggerContext) {
}

// EnterVariableClause is called when production variableClause is entered.
func (s *BaseRelationalParserListener) EnterVariableClause(ctx *VariableClauseContext) {}

// ExitVariableClause is called when production variableClause is exited.
func (s *BaseRelationalParserListener) ExitVariableClause(ctx *VariableClauseContext) {}

// EnterKillStatement is called when production killStatement is entered.
func (s *BaseRelationalParserListener) EnterKillStatement(ctx *KillStatementContext) {}

// ExitKillStatement is called when production killStatement is exited.
func (s *BaseRelationalParserListener) ExitKillStatement(ctx *KillStatementContext) {}

// EnterResetStatement is called when production resetStatement is entered.
func (s *BaseRelationalParserListener) EnterResetStatement(ctx *ResetStatementContext) {}

// ExitResetStatement is called when production resetStatement is exited.
func (s *BaseRelationalParserListener) ExitResetStatement(ctx *ResetStatementContext) {}

// EnterExecuteContinuationStatement is called when production executeContinuationStatement is entered.
func (s *BaseRelationalParserListener) EnterExecuteContinuationStatement(ctx *ExecuteContinuationStatementContext) {
}

// ExitExecuteContinuationStatement is called when production executeContinuationStatement is exited.
func (s *BaseRelationalParserListener) ExitExecuteContinuationStatement(ctx *ExecuteContinuationStatementContext) {
}

// EnterCopyExportStatement is called when production copyExportStatement is entered.
func (s *BaseRelationalParserListener) EnterCopyExportStatement(ctx *CopyExportStatementContext) {}

// ExitCopyExportStatement is called when production copyExportStatement is exited.
func (s *BaseRelationalParserListener) ExitCopyExportStatement(ctx *CopyExportStatementContext) {}

// EnterCopyImportStatement is called when production copyImportStatement is entered.
func (s *BaseRelationalParserListener) EnterCopyImportStatement(ctx *CopyImportStatementContext) {}

// ExitCopyImportStatement is called when production copyImportStatement is exited.
func (s *BaseRelationalParserListener) ExitCopyImportStatement(ctx *CopyImportStatementContext) {}

// EnterTableIndexes is called when production tableIndexes is entered.
func (s *BaseRelationalParserListener) EnterTableIndexes(ctx *TableIndexesContext) {}

// ExitTableIndexes is called when production tableIndexes is exited.
func (s *BaseRelationalParserListener) ExitTableIndexes(ctx *TableIndexesContext) {}

// EnterLoadedTableIndexes is called when production loadedTableIndexes is entered.
func (s *BaseRelationalParserListener) EnterLoadedTableIndexes(ctx *LoadedTableIndexesContext) {}

// ExitLoadedTableIndexes is called when production loadedTableIndexes is exited.
func (s *BaseRelationalParserListener) ExitLoadedTableIndexes(ctx *LoadedTableIndexesContext) {}

// EnterSimpleDescribeSchemaStatement is called when production simpleDescribeSchemaStatement is entered.
func (s *BaseRelationalParserListener) EnterSimpleDescribeSchemaStatement(ctx *SimpleDescribeSchemaStatementContext) {
}

// ExitSimpleDescribeSchemaStatement is called when production simpleDescribeSchemaStatement is exited.
func (s *BaseRelationalParserListener) ExitSimpleDescribeSchemaStatement(ctx *SimpleDescribeSchemaStatementContext) {
}

// EnterSimpleDescribeSchemaTemplateStatement is called when production simpleDescribeSchemaTemplateStatement is entered.
func (s *BaseRelationalParserListener) EnterSimpleDescribeSchemaTemplateStatement(ctx *SimpleDescribeSchemaTemplateStatementContext) {
}

// ExitSimpleDescribeSchemaTemplateStatement is called when production simpleDescribeSchemaTemplateStatement is exited.
func (s *BaseRelationalParserListener) ExitSimpleDescribeSchemaTemplateStatement(ctx *SimpleDescribeSchemaTemplateStatementContext) {
}

// EnterFullDescribeStatement is called when production fullDescribeStatement is entered.
func (s *BaseRelationalParserListener) EnterFullDescribeStatement(ctx *FullDescribeStatementContext) {
}

// ExitFullDescribeStatement is called when production fullDescribeStatement is exited.
func (s *BaseRelationalParserListener) ExitFullDescribeStatement(ctx *FullDescribeStatementContext) {}

// EnterHelpStatement is called when production helpStatement is entered.
func (s *BaseRelationalParserListener) EnterHelpStatement(ctx *HelpStatementContext) {}

// ExitHelpStatement is called when production helpStatement is exited.
func (s *BaseRelationalParserListener) ExitHelpStatement(ctx *HelpStatementContext) {}

// EnterDescribeStatements is called when production describeStatements is entered.
func (s *BaseRelationalParserListener) EnterDescribeStatements(ctx *DescribeStatementsContext) {}

// ExitDescribeStatements is called when production describeStatements is exited.
func (s *BaseRelationalParserListener) ExitDescribeStatements(ctx *DescribeStatementsContext) {}

// EnterDescribeConnection is called when production describeConnection is entered.
func (s *BaseRelationalParserListener) EnterDescribeConnection(ctx *DescribeConnectionContext) {}

// ExitDescribeConnection is called when production describeConnection is exited.
func (s *BaseRelationalParserListener) ExitDescribeConnection(ctx *DescribeConnectionContext) {}

// EnterFullId is called when production fullId is entered.
func (s *BaseRelationalParserListener) EnterFullId(ctx *FullIdContext) {}

// ExitFullId is called when production fullId is exited.
func (s *BaseRelationalParserListener) ExitFullId(ctx *FullIdContext) {}

// EnterTableName is called when production tableName is entered.
func (s *BaseRelationalParserListener) EnterTableName(ctx *TableNameContext) {}

// ExitTableName is called when production tableName is exited.
func (s *BaseRelationalParserListener) ExitTableName(ctx *TableNameContext) {}

// EnterFullColumnName is called when production fullColumnName is entered.
func (s *BaseRelationalParserListener) EnterFullColumnName(ctx *FullColumnNameContext) {}

// ExitFullColumnName is called when production fullColumnName is exited.
func (s *BaseRelationalParserListener) ExitFullColumnName(ctx *FullColumnNameContext) {}

// EnterIndexColumnName is called when production indexColumnName is entered.
func (s *BaseRelationalParserListener) EnterIndexColumnName(ctx *IndexColumnNameContext) {}

// ExitIndexColumnName is called when production indexColumnName is exited.
func (s *BaseRelationalParserListener) ExitIndexColumnName(ctx *IndexColumnNameContext) {}

// EnterCharsetName is called when production charsetName is entered.
func (s *BaseRelationalParserListener) EnterCharsetName(ctx *CharsetNameContext) {}

// ExitCharsetName is called when production charsetName is exited.
func (s *BaseRelationalParserListener) ExitCharsetName(ctx *CharsetNameContext) {}

// EnterCollationName is called when production collationName is entered.
func (s *BaseRelationalParserListener) EnterCollationName(ctx *CollationNameContext) {}

// ExitCollationName is called when production collationName is exited.
func (s *BaseRelationalParserListener) ExitCollationName(ctx *CollationNameContext) {}

// EnterUid is called when production uid is entered.
func (s *BaseRelationalParserListener) EnterUid(ctx *UidContext) {}

// ExitUid is called when production uid is exited.
func (s *BaseRelationalParserListener) ExitUid(ctx *UidContext) {}

// EnterSimpleId is called when production simpleId is entered.
func (s *BaseRelationalParserListener) EnterSimpleId(ctx *SimpleIdContext) {}

// ExitSimpleId is called when production simpleId is exited.
func (s *BaseRelationalParserListener) ExitSimpleId(ctx *SimpleIdContext) {}

// EnterNullNotnull is called when production nullNotnull is entered.
func (s *BaseRelationalParserListener) EnterNullNotnull(ctx *NullNotnullContext) {}

// ExitNullNotnull is called when production nullNotnull is exited.
func (s *BaseRelationalParserListener) ExitNullNotnull(ctx *NullNotnullContext) {}

// EnterDecimalLiteral is called when production decimalLiteral is entered.
func (s *BaseRelationalParserListener) EnterDecimalLiteral(ctx *DecimalLiteralContext) {}

// ExitDecimalLiteral is called when production decimalLiteral is exited.
func (s *BaseRelationalParserListener) ExitDecimalLiteral(ctx *DecimalLiteralContext) {}

// EnterStringLiteral is called when production stringLiteral is entered.
func (s *BaseRelationalParserListener) EnterStringLiteral(ctx *StringLiteralContext) {}

// ExitStringLiteral is called when production stringLiteral is exited.
func (s *BaseRelationalParserListener) ExitStringLiteral(ctx *StringLiteralContext) {}

// EnterBooleanLiteral is called when production booleanLiteral is entered.
func (s *BaseRelationalParserListener) EnterBooleanLiteral(ctx *BooleanLiteralContext) {}

// ExitBooleanLiteral is called when production booleanLiteral is exited.
func (s *BaseRelationalParserListener) ExitBooleanLiteral(ctx *BooleanLiteralContext) {}

// EnterBytesLiteral is called when production bytesLiteral is entered.
func (s *BaseRelationalParserListener) EnterBytesLiteral(ctx *BytesLiteralContext) {}

// ExitBytesLiteral is called when production bytesLiteral is exited.
func (s *BaseRelationalParserListener) ExitBytesLiteral(ctx *BytesLiteralContext) {}

// EnterNullLiteral is called when production nullLiteral is entered.
func (s *BaseRelationalParserListener) EnterNullLiteral(ctx *NullLiteralContext) {}

// ExitNullLiteral is called when production nullLiteral is exited.
func (s *BaseRelationalParserListener) ExitNullLiteral(ctx *NullLiteralContext) {}

// EnterStringConstant is called when production stringConstant is entered.
func (s *BaseRelationalParserListener) EnterStringConstant(ctx *StringConstantContext) {}

// ExitStringConstant is called when production stringConstant is exited.
func (s *BaseRelationalParserListener) ExitStringConstant(ctx *StringConstantContext) {}

// EnterDecimalConstant is called when production decimalConstant is entered.
func (s *BaseRelationalParserListener) EnterDecimalConstant(ctx *DecimalConstantContext) {}

// ExitDecimalConstant is called when production decimalConstant is exited.
func (s *BaseRelationalParserListener) ExitDecimalConstant(ctx *DecimalConstantContext) {}

// EnterNegativeDecimalConstant is called when production negativeDecimalConstant is entered.
func (s *BaseRelationalParserListener) EnterNegativeDecimalConstant(ctx *NegativeDecimalConstantContext) {
}

// ExitNegativeDecimalConstant is called when production negativeDecimalConstant is exited.
func (s *BaseRelationalParserListener) ExitNegativeDecimalConstant(ctx *NegativeDecimalConstantContext) {
}

// EnterBytesConstant is called when production bytesConstant is entered.
func (s *BaseRelationalParserListener) EnterBytesConstant(ctx *BytesConstantContext) {}

// ExitBytesConstant is called when production bytesConstant is exited.
func (s *BaseRelationalParserListener) ExitBytesConstant(ctx *BytesConstantContext) {}

// EnterBooleanConstant is called when production booleanConstant is entered.
func (s *BaseRelationalParserListener) EnterBooleanConstant(ctx *BooleanConstantContext) {}

// ExitBooleanConstant is called when production booleanConstant is exited.
func (s *BaseRelationalParserListener) ExitBooleanConstant(ctx *BooleanConstantContext) {}

// EnterBitStringConstant is called when production bitStringConstant is entered.
func (s *BaseRelationalParserListener) EnterBitStringConstant(ctx *BitStringConstantContext) {}

// ExitBitStringConstant is called when production bitStringConstant is exited.
func (s *BaseRelationalParserListener) ExitBitStringConstant(ctx *BitStringConstantContext) {}

// EnterNullConstant is called when production nullConstant is entered.
func (s *BaseRelationalParserListener) EnterNullConstant(ctx *NullConstantContext) {}

// ExitNullConstant is called when production nullConstant is exited.
func (s *BaseRelationalParserListener) ExitNullConstant(ctx *NullConstantContext) {}

// EnterStringDataType is called when production stringDataType is entered.
func (s *BaseRelationalParserListener) EnterStringDataType(ctx *StringDataTypeContext) {}

// ExitStringDataType is called when production stringDataType is exited.
func (s *BaseRelationalParserListener) ExitStringDataType(ctx *StringDataTypeContext) {}

// EnterNationalStringDataType is called when production nationalStringDataType is entered.
func (s *BaseRelationalParserListener) EnterNationalStringDataType(ctx *NationalStringDataTypeContext) {
}

// ExitNationalStringDataType is called when production nationalStringDataType is exited.
func (s *BaseRelationalParserListener) ExitNationalStringDataType(ctx *NationalStringDataTypeContext) {
}

// EnterNationalVaryingStringDataType is called when production nationalVaryingStringDataType is entered.
func (s *BaseRelationalParserListener) EnterNationalVaryingStringDataType(ctx *NationalVaryingStringDataTypeContext) {
}

// ExitNationalVaryingStringDataType is called when production nationalVaryingStringDataType is exited.
func (s *BaseRelationalParserListener) ExitNationalVaryingStringDataType(ctx *NationalVaryingStringDataTypeContext) {
}

// EnterDimensionDataType is called when production dimensionDataType is entered.
func (s *BaseRelationalParserListener) EnterDimensionDataType(ctx *DimensionDataTypeContext) {}

// ExitDimensionDataType is called when production dimensionDataType is exited.
func (s *BaseRelationalParserListener) ExitDimensionDataType(ctx *DimensionDataTypeContext) {}

// EnterSimpleDataType is called when production simpleDataType is entered.
func (s *BaseRelationalParserListener) EnterSimpleDataType(ctx *SimpleDataTypeContext) {}

// ExitSimpleDataType is called when production simpleDataType is exited.
func (s *BaseRelationalParserListener) ExitSimpleDataType(ctx *SimpleDataTypeContext) {}

// EnterCollectionDataType is called when production collectionDataType is entered.
func (s *BaseRelationalParserListener) EnterCollectionDataType(ctx *CollectionDataTypeContext) {}

// ExitCollectionDataType is called when production collectionDataType is exited.
func (s *BaseRelationalParserListener) ExitCollectionDataType(ctx *CollectionDataTypeContext) {}

// EnterSpatialDataType is called when production spatialDataType is entered.
func (s *BaseRelationalParserListener) EnterSpatialDataType(ctx *SpatialDataTypeContext) {}

// ExitSpatialDataType is called when production spatialDataType is exited.
func (s *BaseRelationalParserListener) ExitSpatialDataType(ctx *SpatialDataTypeContext) {}

// EnterLongVarcharDataType is called when production longVarcharDataType is entered.
func (s *BaseRelationalParserListener) EnterLongVarcharDataType(ctx *LongVarcharDataTypeContext) {}

// ExitLongVarcharDataType is called when production longVarcharDataType is exited.
func (s *BaseRelationalParserListener) ExitLongVarcharDataType(ctx *LongVarcharDataTypeContext) {}

// EnterLongVarbinaryDataType is called when production longVarbinaryDataType is entered.
func (s *BaseRelationalParserListener) EnterLongVarbinaryDataType(ctx *LongVarbinaryDataTypeContext) {
}

// ExitLongVarbinaryDataType is called when production longVarbinaryDataType is exited.
func (s *BaseRelationalParserListener) ExitLongVarbinaryDataType(ctx *LongVarbinaryDataTypeContext) {}

// EnterCollectionOptions is called when production collectionOptions is entered.
func (s *BaseRelationalParserListener) EnterCollectionOptions(ctx *CollectionOptionsContext) {}

// ExitCollectionOptions is called when production collectionOptions is exited.
func (s *BaseRelationalParserListener) ExitCollectionOptions(ctx *CollectionOptionsContext) {}

// EnterConvertedDataType is called when production convertedDataType is entered.
func (s *BaseRelationalParserListener) EnterConvertedDataType(ctx *ConvertedDataTypeContext) {}

// ExitConvertedDataType is called when production convertedDataType is exited.
func (s *BaseRelationalParserListener) ExitConvertedDataType(ctx *ConvertedDataTypeContext) {}

// EnterLengthOneDimension is called when production lengthOneDimension is entered.
func (s *BaseRelationalParserListener) EnterLengthOneDimension(ctx *LengthOneDimensionContext) {}

// ExitLengthOneDimension is called when production lengthOneDimension is exited.
func (s *BaseRelationalParserListener) ExitLengthOneDimension(ctx *LengthOneDimensionContext) {}

// EnterLengthTwoDimension is called when production lengthTwoDimension is entered.
func (s *BaseRelationalParserListener) EnterLengthTwoDimension(ctx *LengthTwoDimensionContext) {}

// ExitLengthTwoDimension is called when production lengthTwoDimension is exited.
func (s *BaseRelationalParserListener) ExitLengthTwoDimension(ctx *LengthTwoDimensionContext) {}

// EnterLengthTwoOptionalDimension is called when production lengthTwoOptionalDimension is entered.
func (s *BaseRelationalParserListener) EnterLengthTwoOptionalDimension(ctx *LengthTwoOptionalDimensionContext) {
}

// ExitLengthTwoOptionalDimension is called when production lengthTwoOptionalDimension is exited.
func (s *BaseRelationalParserListener) ExitLengthTwoOptionalDimension(ctx *LengthTwoOptionalDimensionContext) {
}

// EnterUidList is called when production uidList is entered.
func (s *BaseRelationalParserListener) EnterUidList(ctx *UidListContext) {}

// ExitUidList is called when production uidList is exited.
func (s *BaseRelationalParserListener) ExitUidList(ctx *UidListContext) {}

// EnterUidWithNestings is called when production uidWithNestings is entered.
func (s *BaseRelationalParserListener) EnterUidWithNestings(ctx *UidWithNestingsContext) {}

// ExitUidWithNestings is called when production uidWithNestings is exited.
func (s *BaseRelationalParserListener) ExitUidWithNestings(ctx *UidWithNestingsContext) {}

// EnterUidListWithNestingsInParens is called when production uidListWithNestingsInParens is entered.
func (s *BaseRelationalParserListener) EnterUidListWithNestingsInParens(ctx *UidListWithNestingsInParensContext) {
}

// ExitUidListWithNestingsInParens is called when production uidListWithNestingsInParens is exited.
func (s *BaseRelationalParserListener) ExitUidListWithNestingsInParens(ctx *UidListWithNestingsInParensContext) {
}

// EnterUidListWithNestings is called when production uidListWithNestings is entered.
func (s *BaseRelationalParserListener) EnterUidListWithNestings(ctx *UidListWithNestingsContext) {}

// ExitUidListWithNestings is called when production uidListWithNestings is exited.
func (s *BaseRelationalParserListener) ExitUidListWithNestings(ctx *UidListWithNestingsContext) {}

// EnterTables is called when production tables is entered.
func (s *BaseRelationalParserListener) EnterTables(ctx *TablesContext) {}

// ExitTables is called when production tables is exited.
func (s *BaseRelationalParserListener) ExitTables(ctx *TablesContext) {}

// EnterIndexColumnNames is called when production indexColumnNames is entered.
func (s *BaseRelationalParserListener) EnterIndexColumnNames(ctx *IndexColumnNamesContext) {}

// ExitIndexColumnNames is called when production indexColumnNames is exited.
func (s *BaseRelationalParserListener) ExitIndexColumnNames(ctx *IndexColumnNamesContext) {}

// EnterExpressions is called when production expressions is entered.
func (s *BaseRelationalParserListener) EnterExpressions(ctx *ExpressionsContext) {}

// ExitExpressions is called when production expressions is exited.
func (s *BaseRelationalParserListener) ExitExpressions(ctx *ExpressionsContext) {}

// EnterExpressionsWithDefaults is called when production expressionsWithDefaults is entered.
func (s *BaseRelationalParserListener) EnterExpressionsWithDefaults(ctx *ExpressionsWithDefaultsContext) {
}

// ExitExpressionsWithDefaults is called when production expressionsWithDefaults is exited.
func (s *BaseRelationalParserListener) ExitExpressionsWithDefaults(ctx *ExpressionsWithDefaultsContext) {
}

// EnterRecordConstructorForInsert is called when production recordConstructorForInsert is entered.
func (s *BaseRelationalParserListener) EnterRecordConstructorForInsert(ctx *RecordConstructorForInsertContext) {
}

// ExitRecordConstructorForInsert is called when production recordConstructorForInsert is exited.
func (s *BaseRelationalParserListener) ExitRecordConstructorForInsert(ctx *RecordConstructorForInsertContext) {
}

// EnterRecordConstructorForInlineTable is called when production recordConstructorForInlineTable is entered.
func (s *BaseRelationalParserListener) EnterRecordConstructorForInlineTable(ctx *RecordConstructorForInlineTableContext) {
}

// ExitRecordConstructorForInlineTable is called when production recordConstructorForInlineTable is exited.
func (s *BaseRelationalParserListener) ExitRecordConstructorForInlineTable(ctx *RecordConstructorForInlineTableContext) {
}

// EnterRecordConstructor is called when production recordConstructor is entered.
func (s *BaseRelationalParserListener) EnterRecordConstructor(ctx *RecordConstructorContext) {}

// ExitRecordConstructor is called when production recordConstructor is exited.
func (s *BaseRelationalParserListener) ExitRecordConstructor(ctx *RecordConstructorContext) {}

// EnterOfTypeClause is called when production ofTypeClause is entered.
func (s *BaseRelationalParserListener) EnterOfTypeClause(ctx *OfTypeClauseContext) {}

// ExitOfTypeClause is called when production ofTypeClause is exited.
func (s *BaseRelationalParserListener) ExitOfTypeClause(ctx *OfTypeClauseContext) {}

// EnterArrayConstructor is called when production arrayConstructor is entered.
func (s *BaseRelationalParserListener) EnterArrayConstructor(ctx *ArrayConstructorContext) {}

// ExitArrayConstructor is called when production arrayConstructor is exited.
func (s *BaseRelationalParserListener) ExitArrayConstructor(ctx *ArrayConstructorContext) {}

// EnterUserVariables is called when production userVariables is entered.
func (s *BaseRelationalParserListener) EnterUserVariables(ctx *UserVariablesContext) {}

// ExitUserVariables is called when production userVariables is exited.
func (s *BaseRelationalParserListener) ExitUserVariables(ctx *UserVariablesContext) {}

// EnterDefaultValue is called when production defaultValue is entered.
func (s *BaseRelationalParserListener) EnterDefaultValue(ctx *DefaultValueContext) {}

// ExitDefaultValue is called when production defaultValue is exited.
func (s *BaseRelationalParserListener) ExitDefaultValue(ctx *DefaultValueContext) {}

// EnterCurrentTimestamp is called when production currentTimestamp is entered.
func (s *BaseRelationalParserListener) EnterCurrentTimestamp(ctx *CurrentTimestampContext) {}

// ExitCurrentTimestamp is called when production currentTimestamp is exited.
func (s *BaseRelationalParserListener) ExitCurrentTimestamp(ctx *CurrentTimestampContext) {}

// EnterExpressionOrDefault is called when production expressionOrDefault is entered.
func (s *BaseRelationalParserListener) EnterExpressionOrDefault(ctx *ExpressionOrDefaultContext) {}

// ExitExpressionOrDefault is called when production expressionOrDefault is exited.
func (s *BaseRelationalParserListener) ExitExpressionOrDefault(ctx *ExpressionOrDefaultContext) {}

// EnterExpressionWithOptionalName is called when production expressionWithOptionalName is entered.
func (s *BaseRelationalParserListener) EnterExpressionWithOptionalName(ctx *ExpressionWithOptionalNameContext) {
}

// ExitExpressionWithOptionalName is called when production expressionWithOptionalName is exited.
func (s *BaseRelationalParserListener) ExitExpressionWithOptionalName(ctx *ExpressionWithOptionalNameContext) {
}

// EnterIfExists is called when production ifExists is entered.
func (s *BaseRelationalParserListener) EnterIfExists(ctx *IfExistsContext) {}

// ExitIfExists is called when production ifExists is exited.
func (s *BaseRelationalParserListener) ExitIfExists(ctx *IfExistsContext) {}

// EnterIfNotExists is called when production ifNotExists is entered.
func (s *BaseRelationalParserListener) EnterIfNotExists(ctx *IfNotExistsContext) {}

// ExitIfNotExists is called when production ifNotExists is exited.
func (s *BaseRelationalParserListener) ExitIfNotExists(ctx *IfNotExistsContext) {}

// EnterAggregateFunctionCall is called when production aggregateFunctionCall is entered.
func (s *BaseRelationalParserListener) EnterAggregateFunctionCall(ctx *AggregateFunctionCallContext) {
}

// ExitAggregateFunctionCall is called when production aggregateFunctionCall is exited.
func (s *BaseRelationalParserListener) ExitAggregateFunctionCall(ctx *AggregateFunctionCallContext) {}

// EnterNonAggregateFunctionCall is called when production nonAggregateFunctionCall is entered.
func (s *BaseRelationalParserListener) EnterNonAggregateFunctionCall(ctx *NonAggregateFunctionCallContext) {
}

// ExitNonAggregateFunctionCall is called when production nonAggregateFunctionCall is exited.
func (s *BaseRelationalParserListener) ExitNonAggregateFunctionCall(ctx *NonAggregateFunctionCallContext) {
}

// EnterSpecificFunctionCall is called when production specificFunctionCall is entered.
func (s *BaseRelationalParserListener) EnterSpecificFunctionCall(ctx *SpecificFunctionCallContext) {}

// ExitSpecificFunctionCall is called when production specificFunctionCall is exited.
func (s *BaseRelationalParserListener) ExitSpecificFunctionCall(ctx *SpecificFunctionCallContext) {}

// EnterScalarFunctionCall is called when production scalarFunctionCall is entered.
func (s *BaseRelationalParserListener) EnterScalarFunctionCall(ctx *ScalarFunctionCallContext) {}

// ExitScalarFunctionCall is called when production scalarFunctionCall is exited.
func (s *BaseRelationalParserListener) ExitScalarFunctionCall(ctx *ScalarFunctionCallContext) {}

// EnterUserDefinedScalarFunctionCall is called when production userDefinedScalarFunctionCall is entered.
func (s *BaseRelationalParserListener) EnterUserDefinedScalarFunctionCall(ctx *UserDefinedScalarFunctionCallContext) {
}

// ExitUserDefinedScalarFunctionCall is called when production userDefinedScalarFunctionCall is exited.
func (s *BaseRelationalParserListener) ExitUserDefinedScalarFunctionCall(ctx *UserDefinedScalarFunctionCallContext) {
}

// EnterSimpleFunctionCall is called when production simpleFunctionCall is entered.
func (s *BaseRelationalParserListener) EnterSimpleFunctionCall(ctx *SimpleFunctionCallContext) {}

// ExitSimpleFunctionCall is called when production simpleFunctionCall is exited.
func (s *BaseRelationalParserListener) ExitSimpleFunctionCall(ctx *SimpleFunctionCallContext) {}

// EnterDataTypeFunctionCall is called when production dataTypeFunctionCall is entered.
func (s *BaseRelationalParserListener) EnterDataTypeFunctionCall(ctx *DataTypeFunctionCallContext) {}

// ExitDataTypeFunctionCall is called when production dataTypeFunctionCall is exited.
func (s *BaseRelationalParserListener) ExitDataTypeFunctionCall(ctx *DataTypeFunctionCallContext) {}

// EnterValuesFunctionCall is called when production valuesFunctionCall is entered.
func (s *BaseRelationalParserListener) EnterValuesFunctionCall(ctx *ValuesFunctionCallContext) {}

// ExitValuesFunctionCall is called when production valuesFunctionCall is exited.
func (s *BaseRelationalParserListener) ExitValuesFunctionCall(ctx *ValuesFunctionCallContext) {}

// EnterCaseExpressionFunctionCall is called when production caseExpressionFunctionCall is entered.
func (s *BaseRelationalParserListener) EnterCaseExpressionFunctionCall(ctx *CaseExpressionFunctionCallContext) {
}

// ExitCaseExpressionFunctionCall is called when production caseExpressionFunctionCall is exited.
func (s *BaseRelationalParserListener) ExitCaseExpressionFunctionCall(ctx *CaseExpressionFunctionCallContext) {
}

// EnterCaseFunctionCall is called when production caseFunctionCall is entered.
func (s *BaseRelationalParserListener) EnterCaseFunctionCall(ctx *CaseFunctionCallContext) {}

// ExitCaseFunctionCall is called when production caseFunctionCall is exited.
func (s *BaseRelationalParserListener) ExitCaseFunctionCall(ctx *CaseFunctionCallContext) {}

// EnterCharFunctionCall is called when production charFunctionCall is entered.
func (s *BaseRelationalParserListener) EnterCharFunctionCall(ctx *CharFunctionCallContext) {}

// ExitCharFunctionCall is called when production charFunctionCall is exited.
func (s *BaseRelationalParserListener) ExitCharFunctionCall(ctx *CharFunctionCallContext) {}

// EnterPositionFunctionCall is called when production positionFunctionCall is entered.
func (s *BaseRelationalParserListener) EnterPositionFunctionCall(ctx *PositionFunctionCallContext) {}

// ExitPositionFunctionCall is called when production positionFunctionCall is exited.
func (s *BaseRelationalParserListener) ExitPositionFunctionCall(ctx *PositionFunctionCallContext) {}

// EnterSubstrFunctionCall is called when production substrFunctionCall is entered.
func (s *BaseRelationalParserListener) EnterSubstrFunctionCall(ctx *SubstrFunctionCallContext) {}

// ExitSubstrFunctionCall is called when production substrFunctionCall is exited.
func (s *BaseRelationalParserListener) ExitSubstrFunctionCall(ctx *SubstrFunctionCallContext) {}

// EnterTrimFunctionCall is called when production trimFunctionCall is entered.
func (s *BaseRelationalParserListener) EnterTrimFunctionCall(ctx *TrimFunctionCallContext) {}

// ExitTrimFunctionCall is called when production trimFunctionCall is exited.
func (s *BaseRelationalParserListener) ExitTrimFunctionCall(ctx *TrimFunctionCallContext) {}

// EnterWeightFunctionCall is called when production weightFunctionCall is entered.
func (s *BaseRelationalParserListener) EnterWeightFunctionCall(ctx *WeightFunctionCallContext) {}

// ExitWeightFunctionCall is called when production weightFunctionCall is exited.
func (s *BaseRelationalParserListener) ExitWeightFunctionCall(ctx *WeightFunctionCallContext) {}

// EnterExtractFunctionCall is called when production extractFunctionCall is entered.
func (s *BaseRelationalParserListener) EnterExtractFunctionCall(ctx *ExtractFunctionCallContext) {}

// ExitExtractFunctionCall is called when production extractFunctionCall is exited.
func (s *BaseRelationalParserListener) ExitExtractFunctionCall(ctx *ExtractFunctionCallContext) {}

// EnterGetFormatFunctionCall is called when production getFormatFunctionCall is entered.
func (s *BaseRelationalParserListener) EnterGetFormatFunctionCall(ctx *GetFormatFunctionCallContext) {
}

// ExitGetFormatFunctionCall is called when production getFormatFunctionCall is exited.
func (s *BaseRelationalParserListener) ExitGetFormatFunctionCall(ctx *GetFormatFunctionCallContext) {}

// EnterCaseFuncAlternative is called when production caseFuncAlternative is entered.
func (s *BaseRelationalParserListener) EnterCaseFuncAlternative(ctx *CaseFuncAlternativeContext) {}

// ExitCaseFuncAlternative is called when production caseFuncAlternative is exited.
func (s *BaseRelationalParserListener) ExitCaseFuncAlternative(ctx *CaseFuncAlternativeContext) {}

// EnterLevelWeightList is called when production levelWeightList is entered.
func (s *BaseRelationalParserListener) EnterLevelWeightList(ctx *LevelWeightListContext) {}

// ExitLevelWeightList is called when production levelWeightList is exited.
func (s *BaseRelationalParserListener) ExitLevelWeightList(ctx *LevelWeightListContext) {}

// EnterLevelWeightRange is called when production levelWeightRange is entered.
func (s *BaseRelationalParserListener) EnterLevelWeightRange(ctx *LevelWeightRangeContext) {}

// ExitLevelWeightRange is called when production levelWeightRange is exited.
func (s *BaseRelationalParserListener) ExitLevelWeightRange(ctx *LevelWeightRangeContext) {}

// EnterLevelInWeightListElement is called when production levelInWeightListElement is entered.
func (s *BaseRelationalParserListener) EnterLevelInWeightListElement(ctx *LevelInWeightListElementContext) {
}

// ExitLevelInWeightListElement is called when production levelInWeightListElement is exited.
func (s *BaseRelationalParserListener) ExitLevelInWeightListElement(ctx *LevelInWeightListElementContext) {
}

// EnterAggregateWindowedFunction is called when production aggregateWindowedFunction is entered.
func (s *BaseRelationalParserListener) EnterAggregateWindowedFunction(ctx *AggregateWindowedFunctionContext) {
}

// ExitAggregateWindowedFunction is called when production aggregateWindowedFunction is exited.
func (s *BaseRelationalParserListener) ExitAggregateWindowedFunction(ctx *AggregateWindowedFunctionContext) {
}

// EnterNonAggregateWindowedFunction is called when production nonAggregateWindowedFunction is entered.
func (s *BaseRelationalParserListener) EnterNonAggregateWindowedFunction(ctx *NonAggregateWindowedFunctionContext) {
}

// ExitNonAggregateWindowedFunction is called when production nonAggregateWindowedFunction is exited.
func (s *BaseRelationalParserListener) ExitNonAggregateWindowedFunction(ctx *NonAggregateWindowedFunctionContext) {
}

// EnterOverClause is called when production overClause is entered.
func (s *BaseRelationalParserListener) EnterOverClause(ctx *OverClauseContext) {}

// ExitOverClause is called when production overClause is exited.
func (s *BaseRelationalParserListener) ExitOverClause(ctx *OverClauseContext) {}

// EnterWindowName is called when production windowName is entered.
func (s *BaseRelationalParserListener) EnterWindowName(ctx *WindowNameContext) {}

// ExitWindowName is called when production windowName is exited.
func (s *BaseRelationalParserListener) ExitWindowName(ctx *WindowNameContext) {}

// EnterWindowSpec is called when production windowSpec is entered.
func (s *BaseRelationalParserListener) EnterWindowSpec(ctx *WindowSpecContext) {}

// ExitWindowSpec is called when production windowSpec is exited.
func (s *BaseRelationalParserListener) ExitWindowSpec(ctx *WindowSpecContext) {}

// EnterWindowOptionsClause is called when production windowOptionsClause is entered.
func (s *BaseRelationalParserListener) EnterWindowOptionsClause(ctx *WindowOptionsClauseContext) {}

// ExitWindowOptionsClause is called when production windowOptionsClause is exited.
func (s *BaseRelationalParserListener) ExitWindowOptionsClause(ctx *WindowOptionsClauseContext) {}

// EnterWindowOption is called when production windowOption is entered.
func (s *BaseRelationalParserListener) EnterWindowOption(ctx *WindowOptionContext) {}

// ExitWindowOption is called when production windowOption is exited.
func (s *BaseRelationalParserListener) ExitWindowOption(ctx *WindowOptionContext) {}

// EnterPartitionClause is called when production partitionClause is entered.
func (s *BaseRelationalParserListener) EnterPartitionClause(ctx *PartitionClauseContext) {}

// ExitPartitionClause is called when production partitionClause is exited.
func (s *BaseRelationalParserListener) ExitPartitionClause(ctx *PartitionClauseContext) {}

// EnterScalarFunctionName is called when production scalarFunctionName is entered.
func (s *BaseRelationalParserListener) EnterScalarFunctionName(ctx *ScalarFunctionNameContext) {}

// ExitScalarFunctionName is called when production scalarFunctionName is exited.
func (s *BaseRelationalParserListener) ExitScalarFunctionName(ctx *ScalarFunctionNameContext) {}

// EnterUserDefinedScalarFunctionName is called when production userDefinedScalarFunctionName is entered.
func (s *BaseRelationalParserListener) EnterUserDefinedScalarFunctionName(ctx *UserDefinedScalarFunctionNameContext) {
}

// ExitUserDefinedScalarFunctionName is called when production userDefinedScalarFunctionName is exited.
func (s *BaseRelationalParserListener) ExitUserDefinedScalarFunctionName(ctx *UserDefinedScalarFunctionNameContext) {
}

// EnterFunctionArgs is called when production functionArgs is entered.
func (s *BaseRelationalParserListener) EnterFunctionArgs(ctx *FunctionArgsContext) {}

// ExitFunctionArgs is called when production functionArgs is exited.
func (s *BaseRelationalParserListener) ExitFunctionArgs(ctx *FunctionArgsContext) {}

// EnterFunctionArg is called when production functionArg is entered.
func (s *BaseRelationalParserListener) EnterFunctionArg(ctx *FunctionArgContext) {}

// ExitFunctionArg is called when production functionArg is exited.
func (s *BaseRelationalParserListener) ExitFunctionArg(ctx *FunctionArgContext) {}

// EnterNamedFunctionArg is called when production namedFunctionArg is entered.
func (s *BaseRelationalParserListener) EnterNamedFunctionArg(ctx *NamedFunctionArgContext) {}

// ExitNamedFunctionArg is called when production namedFunctionArg is exited.
func (s *BaseRelationalParserListener) ExitNamedFunctionArg(ctx *NamedFunctionArgContext) {}

// EnterPredicatedExpression is called when production predicatedExpression is entered.
func (s *BaseRelationalParserListener) EnterPredicatedExpression(ctx *PredicatedExpressionContext) {}

// ExitPredicatedExpression is called when production predicatedExpression is exited.
func (s *BaseRelationalParserListener) ExitPredicatedExpression(ctx *PredicatedExpressionContext) {}

// EnterNotExpression is called when production notExpression is entered.
func (s *BaseRelationalParserListener) EnterNotExpression(ctx *NotExpressionContext) {}

// ExitNotExpression is called when production notExpression is exited.
func (s *BaseRelationalParserListener) ExitNotExpression(ctx *NotExpressionContext) {}

// EnterLogicalExpression is called when production logicalExpression is entered.
func (s *BaseRelationalParserListener) EnterLogicalExpression(ctx *LogicalExpressionContext) {}

// ExitLogicalExpression is called when production logicalExpression is exited.
func (s *BaseRelationalParserListener) ExitLogicalExpression(ctx *LogicalExpressionContext) {}

// EnterExistsExpressionAtom is called when production existsExpressionAtom is entered.
func (s *BaseRelationalParserListener) EnterExistsExpressionAtom(ctx *ExistsExpressionAtomContext) {}

// ExitExistsExpressionAtom is called when production existsExpressionAtom is exited.
func (s *BaseRelationalParserListener) ExitExistsExpressionAtom(ctx *ExistsExpressionAtomContext) {}

// EnterBetweenComparisonPredicate is called when production betweenComparisonPredicate is entered.
func (s *BaseRelationalParserListener) EnterBetweenComparisonPredicate(ctx *BetweenComparisonPredicateContext) {
}

// ExitBetweenComparisonPredicate is called when production betweenComparisonPredicate is exited.
func (s *BaseRelationalParserListener) ExitBetweenComparisonPredicate(ctx *BetweenComparisonPredicateContext) {
}

// EnterInPredicate is called when production inPredicate is entered.
func (s *BaseRelationalParserListener) EnterInPredicate(ctx *InPredicateContext) {}

// ExitInPredicate is called when production inPredicate is exited.
func (s *BaseRelationalParserListener) ExitInPredicate(ctx *InPredicateContext) {}

// EnterLikePredicate is called when production likePredicate is entered.
func (s *BaseRelationalParserListener) EnterLikePredicate(ctx *LikePredicateContext) {}

// ExitLikePredicate is called when production likePredicate is exited.
func (s *BaseRelationalParserListener) ExitLikePredicate(ctx *LikePredicateContext) {}

// EnterIsExpression is called when production isExpression is entered.
func (s *BaseRelationalParserListener) EnterIsExpression(ctx *IsExpressionContext) {}

// ExitIsExpression is called when production isExpression is exited.
func (s *BaseRelationalParserListener) ExitIsExpression(ctx *IsExpressionContext) {}

// EnterSubqueryExpressionAtom is called when production subqueryExpressionAtom is entered.
func (s *BaseRelationalParserListener) EnterSubqueryExpressionAtom(ctx *SubqueryExpressionAtomContext) {
}

// ExitSubqueryExpressionAtom is called when production subqueryExpressionAtom is exited.
func (s *BaseRelationalParserListener) ExitSubqueryExpressionAtom(ctx *SubqueryExpressionAtomContext) {
}

// EnterBinaryComparisonPredicate is called when production binaryComparisonPredicate is entered.
func (s *BaseRelationalParserListener) EnterBinaryComparisonPredicate(ctx *BinaryComparisonPredicateContext) {
}

// ExitBinaryComparisonPredicate is called when production binaryComparisonPredicate is exited.
func (s *BaseRelationalParserListener) ExitBinaryComparisonPredicate(ctx *BinaryComparisonPredicateContext) {
}

// EnterSubscriptExpression is called when production subscriptExpression is entered.
func (s *BaseRelationalParserListener) EnterSubscriptExpression(ctx *SubscriptExpressionContext) {}

// ExitSubscriptExpression is called when production subscriptExpression is exited.
func (s *BaseRelationalParserListener) ExitSubscriptExpression(ctx *SubscriptExpressionContext) {}

// EnterConstantExpressionAtom is called when production constantExpressionAtom is entered.
func (s *BaseRelationalParserListener) EnterConstantExpressionAtom(ctx *ConstantExpressionAtomContext) {
}

// ExitConstantExpressionAtom is called when production constantExpressionAtom is exited.
func (s *BaseRelationalParserListener) ExitConstantExpressionAtom(ctx *ConstantExpressionAtomContext) {
}

// EnterFunctionCallExpressionAtom is called when production functionCallExpressionAtom is entered.
func (s *BaseRelationalParserListener) EnterFunctionCallExpressionAtom(ctx *FunctionCallExpressionAtomContext) {
}

// ExitFunctionCallExpressionAtom is called when production functionCallExpressionAtom is exited.
func (s *BaseRelationalParserListener) ExitFunctionCallExpressionAtom(ctx *FunctionCallExpressionAtomContext) {
}

// EnterFullColumnNameExpressionAtom is called when production fullColumnNameExpressionAtom is entered.
func (s *BaseRelationalParserListener) EnterFullColumnNameExpressionAtom(ctx *FullColumnNameExpressionAtomContext) {
}

// ExitFullColumnNameExpressionAtom is called when production fullColumnNameExpressionAtom is exited.
func (s *BaseRelationalParserListener) ExitFullColumnNameExpressionAtom(ctx *FullColumnNameExpressionAtomContext) {
}

// EnterBitExpressionAtom is called when production bitExpressionAtom is entered.
func (s *BaseRelationalParserListener) EnterBitExpressionAtom(ctx *BitExpressionAtomContext) {}

// ExitBitExpressionAtom is called when production bitExpressionAtom is exited.
func (s *BaseRelationalParserListener) ExitBitExpressionAtom(ctx *BitExpressionAtomContext) {}

// EnterPreparedStatementParameterAtom is called when production preparedStatementParameterAtom is entered.
func (s *BaseRelationalParserListener) EnterPreparedStatementParameterAtom(ctx *PreparedStatementParameterAtomContext) {
}

// ExitPreparedStatementParameterAtom is called when production preparedStatementParameterAtom is exited.
func (s *BaseRelationalParserListener) ExitPreparedStatementParameterAtom(ctx *PreparedStatementParameterAtomContext) {
}

// EnterRecordConstructorExpressionAtom is called when production recordConstructorExpressionAtom is entered.
func (s *BaseRelationalParserListener) EnterRecordConstructorExpressionAtom(ctx *RecordConstructorExpressionAtomContext) {
}

// ExitRecordConstructorExpressionAtom is called when production recordConstructorExpressionAtom is exited.
func (s *BaseRelationalParserListener) ExitRecordConstructorExpressionAtom(ctx *RecordConstructorExpressionAtomContext) {
}

// EnterArrayConstructorExpressionAtom is called when production arrayConstructorExpressionAtom is entered.
func (s *BaseRelationalParserListener) EnterArrayConstructorExpressionAtom(ctx *ArrayConstructorExpressionAtomContext) {
}

// ExitArrayConstructorExpressionAtom is called when production arrayConstructorExpressionAtom is exited.
func (s *BaseRelationalParserListener) ExitArrayConstructorExpressionAtom(ctx *ArrayConstructorExpressionAtomContext) {
}

// EnterMathExpressionAtom is called when production mathExpressionAtom is entered.
func (s *BaseRelationalParserListener) EnterMathExpressionAtom(ctx *MathExpressionAtomContext) {}

// ExitMathExpressionAtom is called when production mathExpressionAtom is exited.
func (s *BaseRelationalParserListener) ExitMathExpressionAtom(ctx *MathExpressionAtomContext) {}

// EnterInList is called when production inList is entered.
func (s *BaseRelationalParserListener) EnterInList(ctx *InListContext) {}

// ExitInList is called when production inList is exited.
func (s *BaseRelationalParserListener) ExitInList(ctx *InListContext) {}

// EnterPreparedStatementParameter is called when production preparedStatementParameter is entered.
func (s *BaseRelationalParserListener) EnterPreparedStatementParameter(ctx *PreparedStatementParameterContext) {
}

// ExitPreparedStatementParameter is called when production preparedStatementParameter is exited.
func (s *BaseRelationalParserListener) ExitPreparedStatementParameter(ctx *PreparedStatementParameterContext) {
}

// EnterUnaryOperator is called when production unaryOperator is entered.
func (s *BaseRelationalParserListener) EnterUnaryOperator(ctx *UnaryOperatorContext) {}

// ExitUnaryOperator is called when production unaryOperator is exited.
func (s *BaseRelationalParserListener) ExitUnaryOperator(ctx *UnaryOperatorContext) {}

// EnterComparisonOperator is called when production comparisonOperator is entered.
func (s *BaseRelationalParserListener) EnterComparisonOperator(ctx *ComparisonOperatorContext) {}

// ExitComparisonOperator is called when production comparisonOperator is exited.
func (s *BaseRelationalParserListener) ExitComparisonOperator(ctx *ComparisonOperatorContext) {}

// EnterLogicalOperator is called when production logicalOperator is entered.
func (s *BaseRelationalParserListener) EnterLogicalOperator(ctx *LogicalOperatorContext) {}

// ExitLogicalOperator is called when production logicalOperator is exited.
func (s *BaseRelationalParserListener) ExitLogicalOperator(ctx *LogicalOperatorContext) {}

// EnterBitOperator is called when production bitOperator is entered.
func (s *BaseRelationalParserListener) EnterBitOperator(ctx *BitOperatorContext) {}

// ExitBitOperator is called when production bitOperator is exited.
func (s *BaseRelationalParserListener) ExitBitOperator(ctx *BitOperatorContext) {}

// EnterMathOperator is called when production mathOperator is entered.
func (s *BaseRelationalParserListener) EnterMathOperator(ctx *MathOperatorContext) {}

// ExitMathOperator is called when production mathOperator is exited.
func (s *BaseRelationalParserListener) ExitMathOperator(ctx *MathOperatorContext) {}

// EnterJsonOperator is called when production jsonOperator is entered.
func (s *BaseRelationalParserListener) EnterJsonOperator(ctx *JsonOperatorContext) {}

// ExitJsonOperator is called when production jsonOperator is exited.
func (s *BaseRelationalParserListener) ExitJsonOperator(ctx *JsonOperatorContext) {}

// EnterCharsetNameBase is called when production charsetNameBase is entered.
func (s *BaseRelationalParserListener) EnterCharsetNameBase(ctx *CharsetNameBaseContext) {}

// ExitCharsetNameBase is called when production charsetNameBase is exited.
func (s *BaseRelationalParserListener) ExitCharsetNameBase(ctx *CharsetNameBaseContext) {}

// EnterIntervalTypeBase is called when production intervalTypeBase is entered.
func (s *BaseRelationalParserListener) EnterIntervalTypeBase(ctx *IntervalTypeBaseContext) {}

// ExitIntervalTypeBase is called when production intervalTypeBase is exited.
func (s *BaseRelationalParserListener) ExitIntervalTypeBase(ctx *IntervalTypeBaseContext) {}

// EnterKeywordsCanBeId is called when production keywordsCanBeId is entered.
func (s *BaseRelationalParserListener) EnterKeywordsCanBeId(ctx *KeywordsCanBeIdContext) {}

// ExitKeywordsCanBeId is called when production keywordsCanBeId is exited.
func (s *BaseRelationalParserListener) ExitKeywordsCanBeId(ctx *KeywordsCanBeIdContext) {}

// EnterFunctionNameBase is called when production functionNameBase is entered.
func (s *BaseRelationalParserListener) EnterFunctionNameBase(ctx *FunctionNameBaseContext) {}

// ExitFunctionNameBase is called when production functionNameBase is exited.
func (s *BaseRelationalParserListener) ExitFunctionNameBase(ctx *FunctionNameBaseContext) {}

// EnterFunctionNameKeyword is called when production functionNameKeyword is entered.
func (s *BaseRelationalParserListener) EnterFunctionNameKeyword(ctx *FunctionNameKeywordContext) {}

// ExitFunctionNameKeyword is called when production functionNameKeyword is exited.
func (s *BaseRelationalParserListener) ExitFunctionNameKeyword(ctx *FunctionNameKeywordContext) {}

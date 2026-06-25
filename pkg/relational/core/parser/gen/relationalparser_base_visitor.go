// Code generated from RelationalParser.g4 by ANTLR 4.13.1. DO NOT EDIT.

package antlrgen // RelationalParser
import "github.com/antlr4-go/antlr/v4"

type BaseRelationalParserVisitor struct {
	*antlr.BaseParseTreeVisitor
}

func (v *BaseRelationalParserVisitor) VisitRoot(ctx *RootContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitStatements(ctx *StatementsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitStatement(ctx *StatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitDmlStatement(ctx *DmlStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitDdlStatement(ctx *DdlStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTransactionStatement(ctx *TransactionStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitPreparedStatement(ctx *PreparedStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitAdministrationStatement(ctx *AdministrationStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitUtilityStatement(ctx *UtilityStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTemplateClause(ctx *TemplateClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCreateSchemaStatement(ctx *CreateSchemaStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCreateSchemaTemplateStatement(ctx *CreateSchemaTemplateStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCreateDatabaseStatement(ctx *CreateDatabaseStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitOptionsClause(ctx *OptionsClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitOption(ctx *OptionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitDropDatabaseStatement(ctx *DropDatabaseStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitDropSchemaTemplateStatement(ctx *DropSchemaTemplateStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitDropSchemaStatement(ctx *DropSchemaStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitStructDefinition(ctx *StructDefinitionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTableDefinition(ctx *TableDefinitionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitColumnDefinition(ctx *ColumnDefinitionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitFunctionColumnType(ctx *FunctionColumnTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitColumnType(ctx *ColumnTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitPrimitiveType(ctx *PrimitiveTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitVectorType(ctx *VectorTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitVectorElementType(ctx *VectorElementTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitNullColumnConstraint(ctx *NullColumnConstraintContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitPrimaryKeyDefinition(ctx *PrimaryKeyDefinitionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitFullIdList(ctx *FullIdListContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitEnumDefinition(ctx *EnumDefinitionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIndexAsSelectDefinition(ctx *IndexAsSelectDefinitionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIndexOnSourceDefinition(ctx *IndexOnSourceDefinitionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitVectorIndexDefinition(ctx *VectorIndexDefinitionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIndexColumnList(ctx *IndexColumnListContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIndexColumnSpec(ctx *IndexColumnSpecContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIncludeClause(ctx *IncludeClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIndexType(ctx *IndexTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIndexPartitionClause(ctx *IndexPartitionClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIndexOptions(ctx *IndexOptionsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIndexOption(ctx *IndexOptionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitVectorIndexOptions(ctx *VectorIndexOptionsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitVectorIndexOption(ctx *VectorIndexOptionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitHnswMetric(ctx *HnswMetricContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIndexAttributes(ctx *IndexAttributesContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIndexAttribute(ctx *IndexAttributeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCreateTempFunction(ctx *CreateTempFunctionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitDropTempFunction(ctx *DropTempFunctionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitViewDefinition(ctx *ViewDefinitionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTempSqlInvokedFunction(ctx *TempSqlInvokedFunctionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSqlInvokedFunction(ctx *SqlInvokedFunctionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitFunctionSpecification(ctx *FunctionSpecificationContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSqlParameterDeclarationList(ctx *SqlParameterDeclarationListContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSqlParameterDeclarations(ctx *SqlParameterDeclarationsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSqlParameterDeclaration(ctx *SqlParameterDeclarationContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitParameterMode(ctx *ParameterModeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitReturnsClause(ctx *ReturnsClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitReturnsType(ctx *ReturnsTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitReturnsTableType(ctx *ReturnsTableTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTableFunctionColumnList(ctx *TableFunctionColumnListContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTableFunctionColumnListElement(ctx *TableFunctionColumnListElementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitRoutineCharacteristics(ctx *RoutineCharacteristicsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitLanguageClause(ctx *LanguageClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitLanguageName(ctx *LanguageNameContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitParameterStyle(ctx *ParameterStyleContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitDeterministicCharacteristic(ctx *DeterministicCharacteristicContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitNullCallClause(ctx *NullCallClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitDispatchClause(ctx *DispatchClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitStatementBody(ctx *StatementBodyContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitUserDefinedScalarFunctionStatementBody(ctx *UserDefinedScalarFunctionStatementBodyContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitExpressionBody(ctx *ExpressionBodyContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSqlReturnStatement(ctx *SqlReturnStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitReturnValue(ctx *ReturnValueContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCharSet(ctx *CharSetContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIntervalType(ctx *IntervalTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSchemaId(ctx *SchemaIdContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitPath(ctx *PathContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSchemaTemplateId(ctx *SchemaTemplateIdContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitDeleteStatement(ctx *DeleteStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitInsertStatement(ctx *InsertStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitContinuationAtom(ctx *ContinuationAtomContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSelectStatement(ctx *SelectStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitQuery(ctx *QueryContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCtes(ctx *CtesContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTraversalOrderClause(ctx *TraversalOrderClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitNamedQuery(ctx *NamedQueryContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTableFunction(ctx *TableFunctionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTableFunctionArgs(ctx *TableFunctionArgsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTableFunctionName(ctx *TableFunctionNameContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitQueryTermDefault(ctx *QueryTermDefaultContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSetQuery(ctx *SetQueryContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitInsertStatementValueSelect(ctx *InsertStatementValueSelectContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitInsertStatementValueValues(ctx *InsertStatementValueValuesContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitUpdatedElement(ctx *UpdatedElementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitAssignmentField(ctx *AssignmentFieldContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitUpdateStatement(ctx *UpdateStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitOrderByClause(ctx *OrderByClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitOrderByExpression(ctx *OrderByExpressionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitOrderClause(ctx *OrderClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTableSources(ctx *TableSourcesContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTableSourceBase(ctx *TableSourceBaseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitAtomTableItem(ctx *AtomTableItemContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSubqueryTableItem(ctx *SubqueryTableItemContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitInlineTableItem(ctx *InlineTableItemContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTableValuedFunction(ctx *TableValuedFunctionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIndexHint(ctx *IndexHintContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIndexHintType(ctx *IndexHintTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitInlineTableDefinition(ctx *InlineTableDefinitionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitInnerJoin(ctx *InnerJoinContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitStraightJoin(ctx *StraightJoinContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitOuterJoin(ctx *OuterJoinContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitNaturalJoin(ctx *NaturalJoinContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSimpleTable(ctx *SimpleTableContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitParenthesisQuery(ctx *ParenthesisQueryContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSelectElements(ctx *SelectElementsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSelectStarElement(ctx *SelectStarElementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSelectQualifierStarElement(ctx *SelectQualifierStarElementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSelectExpressionElement(ctx *SelectExpressionElementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitFromClause(ctx *FromClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitGroupByClause(ctx *GroupByClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitWhereExpr(ctx *WhereExprContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitHavingClause(ctx *HavingClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitQualifyClause(ctx *QualifyClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitGroupByItem(ctx *GroupByItemContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitLimitClause(ctx *LimitClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitLimitClauseAtom(ctx *LimitClauseAtomContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitQueryOptions(ctx *QueryOptionsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitQueryOption(ctx *QueryOptionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitStartTransaction(ctx *StartTransactionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCommitStatement(ctx *CommitStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitRollbackStatement(ctx *RollbackStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSetAutocommitStatement(ctx *SetAutocommitStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSetTransactionStatement(ctx *SetTransactionStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTransactionOption(ctx *TransactionOptionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTransactionLevel(ctx *TransactionLevelContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitPrepareStatement(ctx *PrepareStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitExecuteStatement(ctx *ExecuteStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitShowDatabasesStatement(ctx *ShowDatabasesStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitShowSchemaTemplatesStatement(ctx *ShowSchemaTemplatesStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSetVariable(ctx *SetVariableContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSetCharset(ctx *SetCharsetContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSetNames(ctx *SetNamesContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSetTransaction(ctx *SetTransactionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSetAutocommit(ctx *SetAutocommitContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSetNewValueInsideTrigger(ctx *SetNewValueInsideTriggerContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitVariableClause(ctx *VariableClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitKillStatement(ctx *KillStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitResetStatement(ctx *ResetStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitExecuteContinuationStatement(ctx *ExecuteContinuationStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCopyExportStatement(ctx *CopyExportStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCopyImportStatement(ctx *CopyImportStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTableIndexes(ctx *TableIndexesContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitLoadedTableIndexes(ctx *LoadedTableIndexesContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSimpleDescribeSchemaStatement(ctx *SimpleDescribeSchemaStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSimpleDescribeSchemaTemplateStatement(ctx *SimpleDescribeSchemaTemplateStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitFullDescribeStatement(ctx *FullDescribeStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitHelpStatement(ctx *HelpStatementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitDescribeStatements(ctx *DescribeStatementsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitDescribeConnection(ctx *DescribeConnectionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitFullId(ctx *FullIdContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTableName(ctx *TableNameContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitFullColumnName(ctx *FullColumnNameContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIndexColumnName(ctx *IndexColumnNameContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCharsetName(ctx *CharsetNameContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCollationName(ctx *CollationNameContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitUid(ctx *UidContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSimpleId(ctx *SimpleIdContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitNullNotnull(ctx *NullNotnullContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitDecimalLiteral(ctx *DecimalLiteralContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitStringLiteral(ctx *StringLiteralContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitBooleanLiteral(ctx *BooleanLiteralContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitBytesLiteral(ctx *BytesLiteralContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitNullLiteral(ctx *NullLiteralContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitStringConstant(ctx *StringConstantContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitDecimalConstant(ctx *DecimalConstantContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitNegativeDecimalConstant(ctx *NegativeDecimalConstantContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitBytesConstant(ctx *BytesConstantContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitBooleanConstant(ctx *BooleanConstantContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitBitStringConstant(ctx *BitStringConstantContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitNullConstant(ctx *NullConstantContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitStringDataType(ctx *StringDataTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitNationalStringDataType(ctx *NationalStringDataTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitNationalVaryingStringDataType(ctx *NationalVaryingStringDataTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitDimensionDataType(ctx *DimensionDataTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSimpleDataType(ctx *SimpleDataTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCollectionDataType(ctx *CollectionDataTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSpatialDataType(ctx *SpatialDataTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitLongVarcharDataType(ctx *LongVarcharDataTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitLongVarbinaryDataType(ctx *LongVarbinaryDataTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCollectionOptions(ctx *CollectionOptionsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitConvertedDataType(ctx *ConvertedDataTypeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitLengthOneDimension(ctx *LengthOneDimensionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitLengthTwoDimension(ctx *LengthTwoDimensionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitLengthTwoOptionalDimension(ctx *LengthTwoOptionalDimensionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitUidList(ctx *UidListContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitUidWithNestings(ctx *UidWithNestingsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitUidListWithNestingsInParens(ctx *UidListWithNestingsInParensContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitUidListWithNestings(ctx *UidListWithNestingsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTables(ctx *TablesContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIndexColumnNames(ctx *IndexColumnNamesContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitExpressions(ctx *ExpressionsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitExpressionsWithDefaults(ctx *ExpressionsWithDefaultsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitRecordConstructorForInsert(ctx *RecordConstructorForInsertContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitRecordConstructorForInlineTable(ctx *RecordConstructorForInlineTableContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitRecordConstructor(ctx *RecordConstructorContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitOfTypeClause(ctx *OfTypeClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitArrayConstructor(ctx *ArrayConstructorContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitUserVariables(ctx *UserVariablesContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitDefaultValue(ctx *DefaultValueContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCurrentTimestamp(ctx *CurrentTimestampContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitExpressionOrDefault(ctx *ExpressionOrDefaultContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitExpressionWithOptionalName(ctx *ExpressionWithOptionalNameContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIfExists(ctx *IfExistsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIfNotExists(ctx *IfNotExistsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitAggregateFunctionCall(ctx *AggregateFunctionCallContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitNonAggregateFunctionCall(ctx *NonAggregateFunctionCallContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSpecificFunctionCall(ctx *SpecificFunctionCallContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitScalarFunctionCall(ctx *ScalarFunctionCallContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitUserDefinedScalarFunctionCall(ctx *UserDefinedScalarFunctionCallContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSimpleFunctionCall(ctx *SimpleFunctionCallContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitDataTypeFunctionCall(ctx *DataTypeFunctionCallContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitValuesFunctionCall(ctx *ValuesFunctionCallContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCaseExpressionFunctionCall(ctx *CaseExpressionFunctionCallContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCaseFunctionCall(ctx *CaseFunctionCallContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCharFunctionCall(ctx *CharFunctionCallContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitPositionFunctionCall(ctx *PositionFunctionCallContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSubstrFunctionCall(ctx *SubstrFunctionCallContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitTrimFunctionCall(ctx *TrimFunctionCallContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitWeightFunctionCall(ctx *WeightFunctionCallContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitExtractFunctionCall(ctx *ExtractFunctionCallContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitGetFormatFunctionCall(ctx *GetFormatFunctionCallContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCaseFuncAlternative(ctx *CaseFuncAlternativeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitLevelWeightList(ctx *LevelWeightListContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitLevelWeightRange(ctx *LevelWeightRangeContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitLevelInWeightListElement(ctx *LevelInWeightListElementContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitAggregateWindowedFunction(ctx *AggregateWindowedFunctionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitNonAggregateWindowedFunction(ctx *NonAggregateWindowedFunctionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitOverClause(ctx *OverClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitWindowName(ctx *WindowNameContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitWindowSpec(ctx *WindowSpecContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitWindowOptionsClause(ctx *WindowOptionsClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitWindowOption(ctx *WindowOptionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitPartitionClause(ctx *PartitionClauseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitScalarFunctionName(ctx *ScalarFunctionNameContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitUserDefinedScalarFunctionName(ctx *UserDefinedScalarFunctionNameContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitFunctionArgs(ctx *FunctionArgsContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitFunctionArg(ctx *FunctionArgContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitNamedFunctionArg(ctx *NamedFunctionArgContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitPredicatedExpression(ctx *PredicatedExpressionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitNotExpression(ctx *NotExpressionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitLogicalExpression(ctx *LogicalExpressionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitExistsExpressionAtom(ctx *ExistsExpressionAtomContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitBetweenComparisonPredicate(ctx *BetweenComparisonPredicateContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitInPredicate(ctx *InPredicateContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitLikePredicate(ctx *LikePredicateContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIsExpression(ctx *IsExpressionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSubqueryExpressionAtom(ctx *SubqueryExpressionAtomContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitBinaryComparisonPredicate(ctx *BinaryComparisonPredicateContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitSubscriptExpression(ctx *SubscriptExpressionContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitConstantExpressionAtom(ctx *ConstantExpressionAtomContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitFunctionCallExpressionAtom(ctx *FunctionCallExpressionAtomContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitFullColumnNameExpressionAtom(ctx *FullColumnNameExpressionAtomContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitBitExpressionAtom(ctx *BitExpressionAtomContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitPreparedStatementParameterAtom(ctx *PreparedStatementParameterAtomContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitRecordConstructorExpressionAtom(ctx *RecordConstructorExpressionAtomContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitArrayConstructorExpressionAtom(ctx *ArrayConstructorExpressionAtomContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitMathExpressionAtom(ctx *MathExpressionAtomContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitInList(ctx *InListContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitPreparedStatementParameter(ctx *PreparedStatementParameterContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitUnaryOperator(ctx *UnaryOperatorContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitComparisonOperator(ctx *ComparisonOperatorContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitLogicalOperator(ctx *LogicalOperatorContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitBitOperator(ctx *BitOperatorContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitMathOperator(ctx *MathOperatorContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitJsonOperator(ctx *JsonOperatorContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitCharsetNameBase(ctx *CharsetNameBaseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitIntervalTypeBase(ctx *IntervalTypeBaseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitKeywordsCanBeId(ctx *KeywordsCanBeIdContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitFunctionNameBase(ctx *FunctionNameBaseContext) interface{} {
	return v.VisitChildren(ctx)
}

func (v *BaseRelationalParserVisitor) VisitFunctionNameKeyword(ctx *FunctionNameKeywordContext) interface{} {
	return v.VisitChildren(ctx)
}

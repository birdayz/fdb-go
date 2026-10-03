package embedded

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// execCreateTempFunction is CreateTemporaryFunctionConstantAction: the
// function joins the transaction's schema template and is dropped when the
// transaction ends (ON COMMIT DROP). In auto-commit that is at once.
func (c *EmbeddedConnection) execCreateTempFunction(ctx context.Context, ct *antlrgen.CreateTempFunctionContext) (int64, error) {
	if err := c.ensureMetaData(ctx); err != nil {
		return 0, err
	}
	md := c.cachedMetaData()
	if md == nil {
		return 0, api.NewError(api.ErrCodeUndefinedSchema, "no schema set for a temporary function")
	}
	tf := ct.TempSqlInvokedFunction()
	spec := tf.FunctionSpecification()
	if err := checkRoutineCharacteristics(spec); err != nil {
		return 0, err
	}
	name := functions.FullIdToName(spec.GetSchemaQualifiedRoutineName())
	if ct.REPLACE() == nil {
		for _, f := range md.UserDefinedFunctions() {
			if recordlayer.UserDefinedFunctionName(f) == name {
				return 0, api.NewErrorf(api.ErrCodeDuplicateFunction, "function '%s' already exists", name)
			}
		}
	}
	var stored *gen.PUserDefinedFunction
	if body, isMacro := tf.RoutineBody().(*antlrgen.UserDefinedMacroFunctionStatementBodyContext); isMacro {
		macro, err := buildMacroFunction(spec, body, md, nil)
		if err != nil {
			return 0, err
		}
		if stored, err = macro.ToProto(); err != nil {
			return 0, api.WrapErrorf(err, api.ErrCodeUnsupportedOperation, "function %s", name)
		}
	} else {
		if spec.ReturnsClause() != nil {
			return 0, api.NewError(api.ErrCodeUnsupportedOperation, "unsupported explicit return type for SQL table function")
		}
		fn, err := sqlFunctionOf(spec, tf.RoutineBody())
		if err != nil {
			return 0, err
		}
		// Its body resolves against the functions declared so far.
		if err := compileSQLFunction(fn, md, (&cascadesGenerator{c: c}).sessionTemplate()); err != nil {
			return 0, err
		}
		definition := ctxText(ct)
		stored = &gen.PUserDefinedFunction{SpecificFunction: &gen.PUserDefinedFunction_SqlFunction{
			SqlFunction: &gen.PRawSqlFunction{Name: &name, Definition: &definition},
		}}
	}
	if tx := c.activeTx; tx != nil {
		tx.setTempFunction(name, stored)
	}
	return 0, nil
}

// execDropTempFunction is DropTemporaryFunctionConstantAction.
func (c *EmbeddedConnection) execDropTempFunction(ctx context.Context, dt *antlrgen.DropTempFunctionContext) (int64, error) {
	name := functions.FullIdToName(dt.GetSchemaQualifiedRoutineName())
	if tx := c.activeTx; tx != nil && tx.dropTempFunction(name) {
		return 0, nil
	}
	if err := c.ensureMetaData(ctx); err == nil {
		if md := c.cachedMetaData(); md != nil {
			for _, f := range md.UserDefinedFunctions() {
				if recordlayer.UserDefinedFunctionName(f) == name {
					return 0, api.NewErrorf(api.ErrCodeInvalidFunctionDefinition, "Attempt to DROP an non-temporary function: %s", name)
				}
			}
		}
	}
	if dt.EXISTS() == nil {
		return 0, api.NewErrorf(api.ErrCodeUndefinedFunction, "Attempt to DROP an undefined temporary function: %s", name)
	}
	return 0, nil
}

func (tx *embeddedTx) setTempFunction(name string, f *gen.PUserDefinedFunction) {
	tx.dropTempFunction(name)
	tx.tempFunctions = append(tx.tempFunctions, f)
	tx.tempMD = nil
}

func (tx *embeddedTx) dropTempFunction(name string) bool {
	for i, f := range tx.tempFunctions {
		if recordlayer.UserDefinedFunctionName(f) == name {
			tx.tempFunctions = append(tx.tempFunctions[:i:i], tx.tempFunctions[i+1:]...)
			tx.tempMD = nil
			return true
		}
	}
	return false
}

// withTempFunctions is md as this transaction sees it.
func (tx *embeddedTx) withTempFunctions(md *recordlayer.RecordMetaData) *recordlayer.RecordMetaData {
	if md == nil || len(tx.tempFunctions) == 0 {
		return md
	}
	if tx.tempMD == nil || tx.tempBase != md {
		tx.tempMD, tx.tempBase = md.WithUserDefinedFunctions(tx.tempFunctions), md
	}
	return tx.tempMD
}

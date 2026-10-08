package javacorpus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/antlr4-go/antlr/v4"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/javayamsql"
	"fdb.dev/pkg/relational/core/functions"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// privateFixture is established before any DDL executes. The runner must CREATE
// its namespace successfully, without dropping a pre-existing database, before
// this reset/load unit can run. No test or assertion belongs to the replay unit.
type privateFixture struct {
	target connTarget
	// template is the normalized name of the fixture's schema template, the
	// only qualifier a setup INSERT's target may carry.
	template string
	tables   []string
}

// validatePrivateFixture does not admit includes, connection switching, template
// variants, UDFs/views, setup DDL, SELECT-based INSERTs, parameters, or arbitrary
// function calls. It admits one template/setup/test triple, with table/index and
// struct/enum declarations and INSERT VALUES of closed literal expressions.
func validatePrivateFixture(file *javayamsql.File, prefix string) (*privateFixture, error) {
	if file == nil || len(file.Blocks) != 3 || prefix == "" {
		return nil, fmt.Errorf("factory reset/load requires one private template/setup/test triple")
	}
	for _, c := range prefix {
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
			return nil, fmt.Errorf("factory reset/load identifier prefix is not a legal fragment")
		}
	}
	kinds := []javayamsql.BlockKind{javayamsql.BlockSchemaTemplate, javayamsql.BlockSetup, javayamsql.BlockTest}
	for i, kind := range kinds {
		b := file.Blocks[i]
		if b == nil || b.Kind != kind || len(b.Inert) != 0 || b.Options != nil || b.Include != nil || b.Copy != nil || b.TransactionSetups != nil {
			return nil, fmt.Errorf("factory reset/load block %d is not the approved %s topology", i, kind)
		}
	}
	template, setup, test := file.Blocks[0].SchemaTemplate, file.Blocks[1].Setup, file.Blocks[2].Test
	if template == nil || setup == nil || test == nil || len(template.Variants) != 1 || template.ListForm ||
		template.Variants[0].AtLeast != nil || template.Variants[0].LessThan != nil || len(setup.Steps) == 0 || len(test.Tests) == 0 ||
		len(setup.ConnectionOptions) != 0 || len(test.Options.ConnectionOptions) != 0 {
		return nil, fmt.Errorf("factory reset/load requires one unconditional template and nonempty local setup/test blocks")
	}
	for _, c := range []*javayamsql.Value{setup.Connect, test.Connect} {
		if c != nil && !c.IsNull() {
			if n, ok := c.AsInt(); !ok || n != 1 {
				return nil, fmt.Errorf("factory reset/load connect must resolve to its sole local schema")
			}
		}
	}
	id := generatedID(prefix, 1)
	// The runner creates this file's schema template as generatedID(prefix, 1)
	// + "_TEMPLATE" (executeSchemaTemplate, the file's first block), and a
	// table's qualifier is that TEMPLATE's name, not the schema's
	// (functions.ResolveTargetTablePath).
	fixture := &privateFixture{target: connTarget{Path: "/FRL/" + id + "_DB", Schema: id + "_SCHEMA"}, template: strings.ToUpper(id + "_TEMPLATE")}
	stmt, err := parseFixtureStatement("CREATE SCHEMA TEMPLATE fixture " + template.Variants[0].Definition)
	if err != nil {
		return nil, err
	}
	ddl := stmt.DdlStatement()
	if ddl == nil || ddl.CreateStatement() == nil {
		return nil, fmt.Errorf("factory reset/load requires schema-template DDL")
	}
	create, ok := ddl.CreateStatement().(*antlrgen.CreateSchemaTemplateStatementContext)
	if !ok {
		return nil, fmt.Errorf("factory reset/load requires schema-template DDL")
	}
	names := make(map[string]bool)
	for _, clause := range create.AllTemplateClause() {
		switch {
		case clause.TableDefinition() != nil:
			name := functions.NormalizeIdentifier(clause.TableDefinition().Uid().GetText())
			if names[name] {
				return nil, fmt.Errorf("factory reset/load duplicate table %q", name)
			}
			names[name] = true
			fixture.tables = append(fixture.tables, name)
		case clause.IndexDefinition() != nil, clause.StructDefinition() != nil, clause.EnumDefinition() != nil:
			// These declarations cannot add durable effects outside the new store.
		default:
			return nil, fmt.Errorf("factory reset/load does not admit this template declaration")
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("factory reset/load template has no tables")
	}
	for _, step := range setup.Steps {
		if step == nil || step.Kind != javayamsql.CommandQuery || len(step.Segments) != 0 || len(step.Configs) != 0 {
			return nil, fmt.Errorf("factory reset/load setup must contain only unparameterized INSERT VALUES without assertions")
		}
		stmt, err := parseFixtureStatement(step.Query)
		if err != nil {
			return nil, err
		}
		dml := stmt.DmlStatement()
		if dml == nil || dml.InsertStatement() == nil {
			return nil, fmt.Errorf("factory reset/load setup must contain only INSERT VALUES")
		}
		ins := dml.InsertStatement()
		body, ok := ins.InsertStatementValue().(*antlrgen.InsertStatementValueValuesContext)
		if !ok || ins.StatementOptions() != nil {
			return nil, fmt.Errorf("factory reset/load setup must contain only INSERT VALUES without statement options")
		}
		parts := ins.TableName().FullId().AllUid()
		if len(parts) == 0 || len(parts) > 2 || (len(parts) == 2 && functions.NormalizeIdentifier(parts[0].GetText()) != fixture.template) {
			return nil, fmt.Errorf("factory reset/load INSERT target is outside the private schema")
		}
		name := functions.NormalizeIdentifier(parts[len(parts)-1].GetText())
		if !names[name] {
			return nil, fmt.Errorf("factory reset/load INSERT target %q is not a declared fixture table", name)
		}
		if err := validateFixtureValues(body); err != nil {
			return nil, err
		}
	}
	return fixture, nil
}

func parseFixtureStatement(sql string) (antlrgen.IStatementContext, error) {
	root, err := parser.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("factory reset/load SQL: %w", err)
	}
	if root.Statements() == nil || len(root.Statements().AllStatement()) != 1 {
		return nil, fmt.Errorf("factory reset/load requires exactly one statement per step")
	}
	return root.Statements().Statement(0), nil
}

func validateFixtureValues(node antlr.Tree) error {
	if atom, ok := node.(antlrgen.IExpressionAtomContext); ok {
		switch atom := atom.(type) {
		case *antlrgen.ConstantExpressionAtomContext, *antlrgen.MathExpressionAtomContext,
			*antlrgen.RecordConstructorExpressionAtomContext, *antlrgen.ArrayConstructorExpressionAtomContext:
		case *antlrgen.FunctionCallExpressionAtomContext:
			call, ok := atom.FunctionCall().(*antlrgen.SpecificFunctionCallContext)
			if !ok {
				return fmt.Errorf("factory reset/load only admits CAST of closed fixture values")
			}
			cast, ok := call.SpecificFunction().(*antlrgen.DataTypeFunctionCallContext)
			if !ok || cast.CAST() == nil {
				return fmt.Errorf("factory reset/load only admits CAST of closed fixture values")
			}
		default:
			return fmt.Errorf("factory reset/load does not admit value expression %T", atom)
		}
	}
	for i := 0; i < node.GetChildCount(); i++ {
		if err := validateFixtureValues(node.GetChild(i)); err != nil {
			return err
		}
	}
	return nil
}

const fixtureLoadMaxAttempts = 3

// txBeginner is a *sql.DB or a pinned *sql.Conn.
type txBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

func (f *privateFixture) load(ctx context.Context, db txBeginner, steps []*javayamsql.Command, result *FileResult) error {
	// A transaction that outlived FDB's 5-second MVCC window committed
	// nothing, so the whole reset/load replays; on an oversubscribed machine
	// the load alone can take that long. Any other step error is final.
	windowLost := func(err error, attempt int) bool {
		if !api.IsTransactionTimeLimit(err) || attempt == fixtureLoadMaxAttempts {
			return false
		}
		result.FixtureWindowLosses = append(result.FixtureWindowLosses, err)
		return true
	}
attempts:
	for attempt := 1; attempt <= fixtureLoadMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		result.FixtureLoadAttempts++
		for _, name := range f.tables {
			// A DELETE's target is always a table, so the unqualified name
			// reaches only the fixture's own table in its private schema.
			query := "DELETE FROM " + quoteFixtureIdentifier(name)
			if _, err := tx.ExecContext(ctx, query); err != nil {
				err = errors.Join(fmt.Errorf("fixture reset %q: %w", name, err), tx.Rollback())
				if windowLost(err, attempt) {
					continue attempts
				}
				return err
			}
		}
		for _, step := range steps {
			if _, err := tx.ExecContext(ctx, step.Query); err != nil {
				err = errors.Join(&setupError{line: step.Line, query: truncate(step.Query), err: err}, tx.Rollback())
				if windowLost(err, attempt) {
					continue attempts
				}
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(err, tx.Rollback())
		}
		// Only this error is eligible for the ambiguous-commit replay. No SQL
		// execution error and no assertion failure can reach that
		// classification.
		err = tx.Commit()
		if err == nil {
			return nil
		}
		if windowLost(err, attempt) {
			continue
		}
		if !fixtureCommitUnknown(err) {
			return err
		}
		result.FixtureCommitAmbiguities = append(result.FixtureCommitAmbiguities, err)
		if attempt == fixtureLoadMaxAttempts {
			return fmt.Errorf("fixture reset/load exhausted %d ambiguous commits: %w", attempt, err)
		}
	}
	return fmt.Errorf("fixture reset/load did not execute an attempt")
}

func quoteFixtureIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func fixtureCommitUnknown(err error) bool {
	var relational *api.Error
	if !errors.As(err, &relational) || relational.Code != api.ErrCodeStatementCompletionUnknown {
		return false
	}
	var value fdb.Error
	if errors.As(err, &value) {
		return value.Code == 1021
	}
	var pointer *fdb.Error
	return errors.As(err, &pointer) && pointer.Code == 1021
}

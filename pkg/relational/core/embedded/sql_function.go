package embedded

import (
	"fmt"
	"sort"
	"strings"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/expr"
	"github.com/antlr4-go/antlr/v4"
)

// sqlFunction is a schema-template table-valued SQL function, Java's
// CompiledSqlFunction, read from its stored CREATE FUNCTION text.
type sqlFunction struct {
	name   string
	params []sqlFunctionParam
	body   string
}

type sqlFunctionParam struct {
	name       string // normalized
	uid        string // as written
	typ        string // declared type, as written
	def        string // default expression, as written
	defIsNull  bool
	hasDefault bool
	// structTyped is a `TYPE name` parameter, which CAST cannot name.
	structTyped bool
}

// parseSQLFunction reads a stored `CREATE FUNCTION ...` or
// `CREATE [OR REPLACE] TEMPORARY FUNCTION ...` text.
func parseSQLFunction(definition string) (*sqlFunction, error) {
	if root, err := parser.Parse(definition); err == nil {
		if ct, ok := root.Statements().AllStatement()[0].DdlStatement().CreateTempFunction().(*antlrgen.CreateTempFunctionContext); ok && ct != nil {
			tf := ct.TempSqlInvokedFunction()
			return sqlFunctionOf(tf.FunctionSpecification(), tf.RoutineBody())
		}
	}
	root, err := parser.Parse("CREATE SCHEMA TEMPLATE F " + definition)
	if err != nil {
		return nil, err
	}
	cs, _ := root.Statements().AllStatement()[0].DdlStatement().CreateStatement().(*antlrgen.CreateSchemaTemplateStatementContext)
	if cs == nil || len(cs.AllTemplateClause()) != 1 || cs.AllTemplateClause()[0].SqlInvokedFunction() == nil {
		return nil, api.NewErrorf(api.ErrCodeInternalError, "not a SQL function: %s", definition)
	}
	fd := cs.AllTemplateClause()[0].SqlInvokedFunction()
	return sqlFunctionOf(fd.FunctionSpecification(), fd.RoutineBody())
}

func sqlFunctionOf(spec antlrgen.IFunctionSpecificationContext, routine antlrgen.IRoutineBodyContext) (*sqlFunction, error) {
	fn := &sqlFunction{name: functions.FullIdToName(spec.GetSchemaQualifiedRoutineName())}
	body, ok := routine.(*antlrgen.StatementBodyContext)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "function %s: only query bodies are supported", fn.name)
	}
	fn.body = ctxText(body.QueryTerm())
	seen := map[string]bool{}
	if decls := spec.SqlParameterDeclarationList().SqlParameterDeclarations(); decls != nil {
		for _, d := range decls.AllSqlParameterDeclaration() {
			if d.GetSqlParameterName() == nil {
				return nil, api.NewErrorf(api.ErrCodeSyntaxError, "function %s: unnamed parameter", fn.name)
			}
			p := sqlFunctionParam{
				name: functions.NormalizeIdentifier(d.GetSqlParameterName().GetText()),
				uid:  ctxText(d.GetSqlParameterName()),
				typ:  ctxText(d.GetParameterType()),
			}
			p.structTyped = d.GetParameterType().TYPE() != nil
			if seen[p.name] {
				return nil, api.NewErrorf(api.ErrCodeSyntaxError, "function %s: duplicate parameter %s", fn.name, p.name)
			}
			seen[p.name] = true
			if d.GetParameterDefault() != nil {
				p.def, p.hasDefault = ctxText(d.GetParameterDefault()), true
				p.defIsNull = expr.IsBareNullLiteral(d.GetParameterDefault())
			}
			fn.params = append(fn.params, p)
		}
	}
	return fn, nil
}

// sqlFunctionCatalog resolves the functions a query may call.
type sqlFunctionCatalog struct{ md *recordlayer.RecordMetaData }

func metaDataFunctions(md *recordlayer.RecordMetaData) sqlFunctionCatalog {
	return sqlFunctionCatalog{md: md}
}

func (c sqlFunctionCatalog) function(name string) (*sqlFunction, error) {
	if c.md == nil {
		return nil, nil
	}
	for _, f := range c.md.UserDefinedFunctions() {
		if raw := f.GetSqlFunction(); raw != nil && raw.GetName() == name {
			return parseSQLFunction(raw.GetDefinition())
		}
	}
	return nil, nil
}

// shadowsFunction reports a table or view of that name, which a bare FROM
// name reads before a function (LogicalOperator.generateAccess).
func (c sqlFunctionCatalog) shadowsFunction(name string) bool {
	return c.md != nil && (c.md.GetRecordType(name) != nil || findView(c.md, name) != nil)
}

// maxFunctionExpansionDepth bounds nested and (refused) recursive calls.
const maxFunctionExpansionDepth = 32

// expandSQLFunctions rewrites each call of a catalog function in a FROM
// clause into the derived table Java's CompiledSqlFunction.encapsulate builds:
// a one-row table of the arguments beside the body, which reads its
// parameters from it. It reports whether anything was rewritten.
func expandSQLFunctions(sql string, tree antlr.Tree, catalog sqlFunctionCatalog) (string, bool, error) {
	changed := false
	for depth := 0; ; depth++ {
		calls := collectTableFunctionCalls(tree)
		type edit struct {
			start, stop int
			text        string
		}
		var edits []edit
		for _, call := range calls {
			tf := call.TableFunction().(*antlrgen.TableFunctionContext)
			name := functions.FullIdToName(tf.TableFunctionName().FullId())
			fn, err := catalog.function(name)
			if err != nil {
				return "", false, err
			}
			if fn == nil {
				continue
			}
			alias := ctxText(tf.TableFunctionName())
			if call.GetAlias() != nil {
				alias = ctxText(call.GetAlias())
			}
			text, err := fn.invocation(tf, alias, len(edits)+1+depth*1000)
			if err != nil {
				return "", false, err
			}
			edits = append(edits, edit{call.GetStart().GetStart(), call.GetStop().GetStop(), text})
		}
		for _, item := range collectBareFunctionReferences(tree) {
			uids := item.TableName().FullId().AllUid()
			name := functions.FullIdToName(item.TableName().FullId())
			if len(uids) != 1 || catalog.shadowsFunction(name) || cteInScope(item, name) {
				continue
			}
			fn, err := catalog.function(name)
			if err != nil {
				return "", false, err
			}
			if fn == nil {
				continue
			}
			alias := ctxText(item.TableName())
			if item.GetAlias() != nil {
				alias = ctxText(item.GetAlias())
			}
			text, err := fn.invocation(nil, alias, len(edits)+1+depth*1000)
			if err != nil {
				return "", false, err
			}
			edits = append(edits, edit{item.GetStart().GetStart(), item.GetStop().GetStop(), text})
		}
		if len(edits) == 0 {
			return sql, changed, nil
		}
		sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
		if depth >= maxFunctionExpansionDepth {
			return "", false, api.NewError(api.ErrCodeUnsupportedOperation, "SQL function calls nest too deeply")
		}
		var b strings.Builder
		pos := 0
		for _, e := range edits {
			b.WriteString(sql[pos:e.start])
			b.WriteString(e.text)
			pos = e.stop + 1
		}
		b.WriteString(sql[pos:])
		sql, changed = b.String(), true
		root, err := parser.Parse(sql)
		if err != nil {
			return "", false, err
		}
		tree = root
	}
}

// collectTableFunctionCalls lists FROM-clause function calls, outermost
// first and never one inside another's arguments.
func collectTableFunctionCalls(n antlr.Tree) []*antlrgen.TableValuedFunctionContext {
	if tv, ok := n.(*antlrgen.TableValuedFunctionContext); ok {
		return []*antlrgen.TableValuedFunctionContext{tv}
	}
	var out []*antlrgen.TableValuedFunctionContext
	for i := 0; i < n.GetChildCount(); i++ {
		out = append(out, collectTableFunctionCalls(n.GetChild(i))...)
	}
	return out
}

// mayCallSQLFunction reports a non-DDL statement with a FROM item that could
// name a SQL function.
func mayCallSQLFunction(root antlrgen.IRootContext) bool {
	stmts := root.Statements()
	if stmts == nil {
		return false
	}
	for _, st := range stmts.AllStatement() {
		if st.DdlStatement() == nil && (len(collectTableFunctionCalls(st)) > 0 || len(collectBareFunctionReferences(st)) > 0) {
			return true
		}
	}
	return false
}

func collectBareFunctionReferences(n antlr.Tree) []*antlrgen.AtomTableItemContext {
	if item, ok := n.(*antlrgen.AtomTableItemContext); ok {
		return []*antlrgen.AtomTableItemContext{item}
	}
	var out []*antlrgen.AtomTableItemContext
	for i := 0; i < n.GetChildCount(); i++ {
		out = append(out, collectBareFunctionReferences(n.GetChild(i))...)
	}
	return out
}

// cteInScope reports a WITH around n declaring name.
func cteInScope(n antlr.Tree, name string) bool {
	for p := n.GetParent(); p != nil; p = p.GetParent() {
		q, ok := p.(*antlrgen.QueryContext)
		if !ok || q.Ctes() == nil {
			continue
		}
		for _, nq := range q.Ctes().AllNamedQuery() {
			if functions.FullIdToName(nq.GetName()) == name {
				return true
			}
		}
	}
	return false
}

// invocation is the derived table for one call, with arguments bound to
// parameters positionally or by name, and defaults for the rest.
func (fn *sqlFunction) invocation(tf *antlrgen.TableFunctionContext, alias string, n int) (string, error) {
	if tf != nil && tf.InlineTableDefinition() != nil {
		return "", api.NewErrorf(api.ErrCodeUnsupportedOperation, "function %s: column aliases are not supported", fn.name)
	}
	argText := map[string]string{}
	argNull := map[string]bool{}
	var order []string
	var args *antlrgen.NamedOrUnnamedFunctionArgsContext
	if tf != nil {
		args, _ = tf.NamedOrUnnamedFunctionArgs().(*antlrgen.NamedOrUnnamedFunctionArgsContext)
	}
	// UserDefinedFunctionCatalog.lookup's validateCall: an argument naming no
	// parameter, too many arguments, or a missing parameter without a default
	// is no such function.
	notFound := api.NewErrorf(api.ErrCodeUndefinedFunction, "could not find function '%s'", fn.name)
	if args != nil {
		if named := args.AllNamedFunctionArg(); len(named) > 0 {
			keys := make([]string, len(named))
			for i, na := range named {
				keys[i] = functions.NormalizeIdentifier(na.GetKey().GetText())
			}
			if err := expr.DuplicateArgumentNamesError(keys); err != nil {
				return "", err
			}
			for i, na := range named {
				key := keys[i]
				if fn.param(key) == nil {
					return "", notFound
				}
				argText[key] = ctxText(na.GetValue())
				argNull[key] = expr.IsBareNullLiteral(na.GetValue())
				order = append(order, key)
			}
		} else {
			positional := args.AllFunctionArg()
			if len(positional) > len(fn.params) {
				return "", notFound
			}
			for i, a := range positional {
				argText[fn.params[i].name] = ctxText(a)
				argNull[fn.params[i].name] = expr.IsBareNullLiteral(a)
				order = append(order, fn.params[i].name)
			}
		}
	}
	var cols []string
	for _, key := range order {
		cols = append(cols, fn.param(key).column(argText[key], argNull[key]))
	}
	for _, p := range fn.params {
		if _, given := argText[p.name]; given {
			continue
		}
		if !p.hasDefault {
			return "", notFound
		}
		cols = append(cols, p.column(p.def, p.defIsNull))
	}
	if len(cols) == 0 {
		return fmt.Sprintf("(%s) AS %s", fn.body, alias), nil
	}
	return fmt.Sprintf("(SELECT FNB_%d.* FROM (SELECT %s) AS FNP_%d, (%s) AS FNB_%d) AS %s",
		n, strings.Join(cols, ", "), n, fn.body, n, alias), nil
}

func (fn *sqlFunction) param(name string) *sqlFunctionParam {
	for i := range fn.params {
		if fn.params[i].name == name {
			return &fn.params[i]
		}
	}
	return nil
}

// column binds one argument, promoted to the declared type.
func (p *sqlFunctionParam) column(arg string, isNull bool) string {
	if p.structTyped {
		return arg + " AS " + p.uid
	}
	typed := "CAST(NULL AS " + p.typ + ")"
	if isNull {
		arg = typed
	}
	return `"` + expr.SQLFunctionArgument + `"(` + arg + ", " + typed + ") AS " + p.uid
}

// parseQueryWithFunctions parses a query, expanding its SQL function calls.
func parseQueryWithFunctions(sql string, catalog sqlFunctionCatalog) (antlrgen.IQueryContext, error) {
	q, err := parser.ParseView(sql)
	if err != nil {
		return nil, err
	}
	expanded, changed, err := expandSQLFunctions(sql, q, catalog)
	if err != nil || !changed {
		return q, err
	}
	return parser.ParseView(expanded)
}

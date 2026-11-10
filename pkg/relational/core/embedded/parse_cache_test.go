package embedded

// Regression tests for the connection's parse cache (parse_cache.go): a
// repeated statement reuses its parse tree, and parameters and literals bound
// to that tree for one statement never leak into the next.

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/antlr4-go/antlr/v4"

	"fdb.dev/gen/demopb"
	"fdb.dev/internal/unicode15"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/expr"
)

// parseCacheQuery runs sql with args on c and returns its single BIGINT column.
func parseCacheQuery(t *testing.T, c *EmbeddedConnection, sql string, args ...any) []int64 {
	t.Helper()
	named := make([]driver.NamedValue, len(args))
	for i, a := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	rows, err := c.QueryContext(context.Background(), sql, named)
	if err != nil {
		t.Fatalf("%s %v: %v", sql, args, err)
	}
	defer rows.Close() //nolint:errcheck
	var got []int64
	dest := make([]driver.Value, 1)
	for {
		if err := rows.Next(dest); err == io.EOF {
			return got
		} else if err != nil {
			t.Fatalf("%s %v: next: %v", sql, args, err)
		}
		got = append(got, dest[0].(int64))
	}
}

func TestParseCacheRepeatedStatementBindsEachCallsValues(t *testing.T) {
	t.Parallel()
	c := newSimConnection(t, 71)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if _, err := c.ExecContext(ctx, fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", i, 10*i), nil); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	const byID = "SELECT v FROM t WHERE id = ?"
	for _, id := range []int64{1, 2, 3, 2, 1, 4} {
		got := parseCacheQuery(t, c, byID, id)
		want := []int64{10 * id}
		if id == 4 {
			want = nil
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("id %d: got %v, want %v", id, got, want)
		}
	}
	first, ok := c.parses.trees[byID]
	if !ok {
		t.Fatalf("a repeated statement was not cached")
	}
	again, err := c.parseStatement(byID)
	if err != nil || again != first {
		t.Fatalf("a cached statement was parsed again (%v)", err)
	}
	// The statement's binding walk was compiled once and is reused: the
	// parameter values above all bound through the same program.
	if len(first.programs.m) != 1 {
		t.Fatalf("statement keeps %d binding programs, want 1", len(first.programs.m))
	}
	var program *bindingProgram
	for _, p := range first.programs.m {
		program = p
	}
	parseCacheQuery(t, c, byID, int64(3))
	for _, p := range first.programs.m {
		if p != program {
			t.Fatal("a repeated statement compiled its binding walk again")
		}
	}

	// Literals bind to the tree's constant tokens for one statement; the
	// release must restore them so the next run of the same text, and a
	// run of a text with another literal, read their own values.
	for _, id := range []int64{2, 3, 2} {
		sql := fmt.Sprintf("SELECT v FROM t WHERE id = %d", id)
		for range 2 {
			if got := parseCacheQuery(t, c, sql); fmt.Sprint(got) != fmt.Sprint([]int64{10 * id}) {
				t.Fatalf("%s: got %v", sql, got)
			}
		}
	}
	// A statement mixing a parameter and a literal.
	const mixed = "SELECT v FROM t WHERE id = ? OR id = 3 ORDER BY id"
	for _, id := range []int64{1, 2} {
		if got := parseCacheQuery(t, c, mixed, id); fmt.Sprint(got) != fmt.Sprint([]int64{10 * id, 30}) {
			t.Fatalf("mixed id %d: got %v", id, got)
		}
	}
}

func TestParseCacheKeepsNoFailedParse(t *testing.T) {
	t.Parallel()
	c := newSimConnection(t, 72)
	const bad = "SELEC v FROM t"
	for range 2 {
		if _, err := c.QueryContext(context.Background(), bad, nil); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
	if _, ok := c.parses.trees[bad]; ok {
		t.Fatalf("a failed parse was cached")
	}
}

func TestParseCacheIsBounded(t *testing.T) {
	t.Parallel()
	var c EmbeddedConnection
	text := func(i int) string { return fmt.Sprintf("SELECT %d FROM t", i) }
	for i := range parseCacheEntries + 1 {
		if _, err := c.parseStatement(text(i)); err != nil {
			t.Fatalf("parse %d: %v", i, err)
		}
	}
	if len(c.parses.trees) != parseCacheEntries || len(c.parses.order) != parseCacheEntries {
		t.Fatalf("cache holds %d trees, %d in order, want %d", len(c.parses.trees), len(c.parses.order), parseCacheEntries)
	}
	if _, ok := c.parses.trees[text(0)]; ok {
		t.Fatalf("the oldest statement was not evicted")
	}
	if _, ok := c.parses.trees[text(parseCacheEntries)]; !ok {
		t.Fatalf("the newest statement was not cached")
	}

	long := "SELECT 1 FROM t WHERE " + strings.Repeat("1 = 1 AND ", parseCacheMaxText/10) + "1 = 1"
	if _, err := c.parseStatement(long); err != nil {
		t.Fatalf("parse long: %v", err)
	}
	if _, ok := c.parses.trees[long]; ok {
		t.Fatalf("a statement over %d bytes was cached", parseCacheMaxText)
	}
}

// literalProofMetaData is meta-data with an index whose key holds a literal,
// so a statement over it keeps its literals exact (requiresLiteralIndexProof).
func literalProofMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	b.AddIndex("Order", recordlayer.NewIndex("Order$literal_price",
		recordlayer.Concat(recordlayer.Literal(int64(1)), recordlayer.Field("price"))))
	md, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if required, err := requiresLiteralIndexProof(md); err != nil || !required {
		t.Fatalf("an index keyed by a literal must require a literal proof: %v", err)
	}
	return md
}

// A compiled binding walk replays the walk it was compiled from: the cache
// text, equivalence, literal bindings and constants of a fresh compile and of
// a cached program equal those of the interleaved walk it replaced
// (referenceQueryBindings), for parameters, runtime literals, NULL and
// non-runtime sites, with literals as runtime bindings (no meta-data) and
// kept exact (meta-data whose index needs a literal proof). One tree's
// programs for the two are kept apart.
func TestBindingProgramReplaysTheWalk(t *testing.T) {
	t.Parallel()
	exactMD := literalProofMetaData(t)
	differing := 0 // statements whose walk depends on the flag
	for _, sql := range []string{
		"SELECT v FROM t WHERE id = ? AND v = 3 AND v <> 'x' ORDER BY 1 LIMIT 5",
		"SELECT v FROM t WHERE id IN (1, 2, ?) OR v IS NULL OR v = NULL",
		"SELECT COALESCE(v, 7), v + 1.5 FROM t WHERE id = ? GROUP BY v",
	} {
		root, err := parser.Parse(sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		release, err := bindStatementParameters(root, []driver.NamedValue{{Ordinal: 1, Value: int64(9)}})
		if err != nil {
			t.Fatalf("%s: bind: %v", sql, err)
		}
		var programs statementPrograms
		var texts []string
		for _, md := range []*recordlayer.RecordMetaData{nil, exactMD, nil} {
			runtimeTop := md == nil
			want, releaseWant, err := referenceQueryBindings(root, runtimeTop)
			if err != nil {
				t.Fatalf("%s: reference: %v", sql, err)
			}
			releaseWant()
			if want.text == "" || len(want.literals) == 0 {
				t.Fatalf("%s: the reference walk bound nothing: %q %v", sql, want.text, want.literals)
			}
			texts = append(texts, want.text)
			for run, cache := range []*statementPrograms{nil, &programs, &programs} {
				got, releaseGot, err := normalizeQueryBindingsWith(root, md, cache)
				if err != nil {
					t.Fatalf("%s runtime %t run %d: %v", sql, runtimeTop, run, err)
				}
				releaseGot()
				if got.text != want.text || got.equivalence != want.equivalence ||
					fmt.Sprint(got.literals) != fmt.Sprint(want.literals) || fmt.Sprint(got.constants) != fmt.Sprint(want.constants) {
					t.Fatalf("%s runtime %t run %d:\n got %q %q %v %v\nwant %q %q %v %v", sql, runtimeTop, run,
						got.text, got.equivalence, got.literals, got.constants, want.text, want.equivalence, want.literals, want.constants)
				}
			}
		}
		if texts[0] != texts[1] {
			differing++
		}
		if len(programs.m) != 2 {
			t.Fatalf("%s: %d programs kept, want one per runtime flag", sql, len(programs.m))
		}
		release()
	}
	if differing < 2 {
		t.Fatalf("%d statements' cache text depends on the runtime flag, want 2: the exact walk is barely exercised", differing)
	}
}

// A statement keeps at most statementProgramsMax compiled walks; a walk past
// the bound is compiled each time and still replays the reference.
func TestStatementProgramsAreBounded(t *testing.T) {
	t.Parallel()
	root, err := parser.Parse("SELECT v + 1, v - 2 FROM t WHERE id = 3 AND v > 4 AND v < 5 OR v = 6 ORDER BY v LIMIT 7")
	if err != nil {
		t.Fatal(err)
	}
	var nodes []antlr.ParseTree
	var collect func(antlr.Tree)
	collect = func(n antlr.Tree) {
		if rule, ok := n.(antlr.RuleNode); ok {
			nodes = append(nodes, rule)
		}
		for i := 0; i < n.GetChildCount(); i++ {
			collect(n.GetChild(i))
		}
	}
	collect(root)
	if len(nodes) <= statementProgramsMax+1 {
		t.Fatalf("%d subtrees, want more than %d", len(nodes), statementProgramsMax+1)
	}
	nodes = nodes[:statementProgramsMax+2]
	var programs statementPrograms
	for run := range 2 {
		for i, node := range nodes {
			want, releaseWant, err := referenceQueryBindings(node, true)
			if err != nil {
				t.Fatalf("subtree %d: reference: %v", i, err)
			}
			releaseWant()
			got, releaseGot, err := normalizeQueryBindingsWith(node, nil, &programs)
			if err != nil {
				t.Fatalf("subtree %d: %v", i, err)
			}
			releaseGot()
			if got.text != want.text || got.equivalence != want.equivalence || fmt.Sprint(got.literals) != fmt.Sprint(want.literals) {
				t.Fatalf("run %d subtree %d:\n got %q %q %v\nwant %q %q %v", run, i,
					got.text, got.equivalence, got.literals, want.text, want.equivalence, want.literals)
			}
		}
		if len(programs.m) != statementProgramsMax {
			t.Fatalf("run %d: %d programs kept for %d walks, want the bound %d", run, len(programs.m), len(nodes), statementProgramsMax)
		}
	}
}

// referenceQueryBindings is the binding walk as normalizeQueryBindings ran it
// before it was compiled into a program (one interleaved walk of the tree):
// the oracle the compiled program's replay must match. Its bindings are
// released with the returned func, as the code under test's are.
func referenceQueryBindings(tree antlr.ParseTree, runtimeTop bool) (queryBindings, func(), error) {
	result := queryBindings{constants: make(map[string]any), pins: literalPins{}}
	var text, equivalence strings.Builder
	byValue := make(map[string]int)
	type replacement struct {
		token antlr.Token
		value values.Value
	}
	var replaced []replacement
	var refs []*values.ConstantObjectValue
	release := func() {
		for _, ref := range refs {
			expr.ForgetSpecializable(ref)
		}
		for _, old := range replaced {
			if old.value == nil {
				expr.UnbindParameter(old.token)
			} else {
				expr.BindParameter(old.token, old.value)
			}
		}
	}
	appendText := func(s string) {
		if text.Len() != 0 {
			text.WriteByte(' ')
		}
		text.WriteString(s)
	}
	bind := func(token antlr.Token, v values.Value, runtime bool) {
		var exact strings.Builder
		writeLengthPrefixed(&exact, v.Type().String())
		writeBindingKey(&exact, constantPayload(v))
		position := len(result.literals)
		literal := queryLiteralBinding{typeName: v.Type().String(), valueKey: exact.String(), equalTo: position}
		switch v.Type().Code() {
		case values.TypeCodeNull, values.TypeCodeBoolean, values.TypeCodeFloat, values.TypeCodeDouble:
			runtime = false
		}
		if !runtime {
			literal.exact = true
			result.literals = append(result.literals, literal)
			equivalence.WriteByte('E')
			writeLengthPrefixed(&equivalence, exact.String())
			return
		}
		if first, exists := byValue[exact.String()]; exists {
			literal.equalTo = first
		} else {
			byValue[exact.String()] = position
		}
		result.literals = append(result.literals, literal)
		result.constants[strconv.Itoa(position)] = constantPayload(v)
		id := strconv.Itoa(literal.equalTo)
		equivalence.WriteByte('R')
		writeLengthPrefixed(&equivalence, v.Type().String())
		writeLengthPrefixed(&equivalence, id)
		old, _ := expr.BoundParameter(token)
		replaced = append(replaced, replacement{token: token, value: old})
		ref := values.NewConstantObjectValue(queryConstantsAlias, id, v.Type())
		refs = append(refs, ref)
		pins := result.pins
		expr.BindSpecializableParameter(token, ref, v, func() { pins[position] = true })
	}
	var walk func(antlr.Tree, bool) error
	walk = func(node antlr.Tree, runtime bool) error {
		switch n := node.(type) {
		case *antlrgen.LimitClauseContext, *antlrgen.InListContext:
			runtime = false
		case *antlrgen.ScalarFunctionCallContext:
			if n.ScalarFunctionName().GetStart().GetTokenType() == antlrgen.RelationalParserCOALESCE {
				runtime = false
			}
		case *antlrgen.OrderByExpressionContext:
			if _, positional := selectListPosition(n.Expression()); positional {
				runtime = false
			}
		case *antlrgen.GroupByItemContext:
			if _, positional := selectListPosition(n.Expression()); positional {
				runtime = false
			}
		case *antlrgen.PreparedStatementParameterContext:
			if value, ok := expr.BoundParameter(n.GetStart()); ok {
				bind(n.GetStart(), value, runtime)
			}
		case antlrgen.IConstantContext:
			if runtime {
				value, err := expr.ResolveLiteral(n)
				if err != nil {
					if mapped := mapPredicateWalkError(err); mapped != nil {
						return mapped
					}
					return err
				}
				if value.Type().Code() != values.TypeCodeNull {
					bind(n.GetStart(), value, true)
					appendText("?")
					return nil
				}
			}
		}
		if terminal, ok := node.(antlr.TerminalNode); ok {
			token := terminal.GetSymbol()
			if token != nil && token.GetTokenType() != antlr.TokenEOF {
				spelling := token.GetText()
				if foldsInCacheText(token.GetTokenType()) {
					spelling = unicode15.ToUpperString(spelling)
				}
				appendText(spelling)
			}
			return nil
		}
		for i := 0; i < node.GetChildCount(); i++ {
			if err := walk(node.GetChild(i), runtime); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(tree, runtimeTop); err != nil {
		release()
		return queryBindings{}, func() {}, err
	}
	result.text, result.equivalence = text.String(), equivalence.String()
	return result, release, nil
}

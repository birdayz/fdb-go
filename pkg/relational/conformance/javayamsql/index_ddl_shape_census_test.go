package javayamsql_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/antlr4-go/antlr/v4"

	"fdb.dev/pkg/relational/conformance/javayamsql"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// The corpus's CREATE INDEX shape census — the instrument behind RFC-202 §3.
//
// RFC-202 sizes the index-generator port by counting statement SHAPES, and every
// one of those counts is load-bearing: they decide which generator branches get a
// corpus witness and which get a ported Java unit test instead. A count carried
// only in prose rots on the next corpus bump and cannot be re-derived without
// re-inventing the classifier, so the classifier IS the artefact and the numbers
// below are its pinned output.
//
// It walks TYPED parse-tree nodes, never text. An earlier ad-hoc grep sweep for
// this same table silently truncated every statement at the first `n` (`[^\n]`
// inside an ERE bracket expression is "not backslash, not n"), which turned
// `select count(*)` into `select cou` and mis-classified 6 aggregate statements
// as value indexes. Regexes over SQL do not survive contact with SQL.
//
// A count that moves is not a failure to paper over: it means upstream changed
// the corpus, and the RFC's branch-coverage argument has to be re-read against
// the new shape set.

// indexShape is one CREATE INDEX statement, classified.
type indexShape struct {
	File string
	Name string

	// OnSource is true for `CREATE INDEX … ON t(cols)`, false for `… AS SELECT`.
	OnSource bool

	Unique        bool
	HasAttributes bool // `WITH ATTRIBUTES …` (AS-SELECT) / `OPTIONS(…)` (ON-source)

	// AS-SELECT only.
	Aggregate      bool // an aggregate function anywhere in the SELECT list
	SelectElements int
	Star           bool
	Where          bool
	FromSources    int // comma-separated FROM sources; >1 means unnest/derived
	OrderByCount   int
	Reordered      bool // reorderValues would change the key order (see below)
	Covering       bool // 0 < len(ORDER BY) < len(projection) → keyWithValue split
	ExplicitOrder  bool // at least one explicit ASC/DESC/NULLS
	RowVersion     bool
	Dotted         bool // a dotted path appears in the SELECT list

	// ON-source only.
	KeyColumns     int
	IncludeColumns int
}

// TestIndexDDLShapeCensus classifies every CREATE INDEX statement in every
// vendored schema template and pins the aggregate counts.
func TestIndexDDLShapeCensus(t *testing.T) {
	t.Parallel()

	shapes := collectIndexShapes(t)

	var (
		asSelect, onSource                       int
		nonAgg, agg                              int
		withOrderBy, withoutOrderBy              int
		explicitOrder, where, unique, rowVersion int
		multiColumn, covering, reordered         int
		multiSource, star, dotted, attributes    int
		onSourceOrdered, onSourceInclude         int
		aggAttributes                            int
	)
	files := map[string]bool{}
	nonAggFiles := map[string]bool{}
	for _, s := range shapes {
		files[s.File] = true
		if s.OnSource {
			onSource++
			if s.ExplicitOrder {
				onSourceOrdered++
			}
			if s.IncludeColumns > 0 {
				onSourceInclude++
			}
			continue
		}
		asSelect++
		if s.Aggregate {
			agg++
			if s.HasAttributes {
				aggAttributes++
			}
		} else {
			nonAgg++
			nonAggFiles[s.File] = true
			if s.OrderByCount > 0 {
				withOrderBy++
			} else {
				withoutOrderBy++
			}
			if s.ExplicitOrder {
				explicitOrder++
			}
			if s.Where {
				where++
			}
			if s.Unique {
				unique++
			}
			if s.RowVersion {
				rowVersion++
			}
			if s.SelectElements > 1 {
				multiColumn++
			}
			if s.Covering {
				covering++
			}
			if s.Reordered {
				reordered++
			}
			if s.FromSources > 1 {
				multiSource++
			}
			if s.Star {
				star++
			}
			if s.Dotted {
				dotted++
			}
			if s.HasAttributes {
				attributes++
			}
		}
	}

	// The pinned census. Each number is cited in RFC-202 §3; a mismatch means
	// the corpus moved and the RFC's per-branch coverage argument is stale.
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"files declaring any CREATE INDEX", len(files), 60},
		{"AS-SELECT statements", asSelect, 276},
		{"  non-aggregate", nonAgg, 194},
		{"  aggregate", agg, 82},
		{"files with a non-aggregate AS-SELECT", len(nonAggFiles), 56},
		{"non-agg with ORDER BY", withOrderBy, 134},
		{"non-agg without ORDER BY", withoutOrderBy, 60},
		{"non-agg with explicit ASC/DESC/NULLS", explicitOrder, 16},
		{"non-agg with WHERE", where, 17},
		{"non-agg UNIQUE", unique, 2},
		{"non-agg with __ROW_VERSION", rowVersion, 14},
		{"non-agg multi-column", multiColumn, 76},
		{"non-agg covering (0 < |ORDER BY| < |projection|)", covering, 18},
		{"non-agg reordered (reorderValues changes key order)", reordered, 6},
		{"non-agg multi-source FROM", multiSource, 15},
		{"non-agg SELECT *", star, 3},
		{"non-agg dotted path in projection", dotted, 26},
		{"non-agg WITH ATTRIBUTES", attributes, 0},
		{"aggregate WITH ATTRIBUTES (LEGACY_EXTREMUM_EVER)", aggAttributes, 4},
		{"ON-source statements", onSource, 52},
		{"  with an explicit column orderClause", onSourceOrdered, 16},
		{"  with INCLUDE", onSourceInclude, 17},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, pinned %d\n\n"+
				"The corpus's index-DDL shape surface moved. RFC-202 §3 maps each "+
				"generator branch to the statements that witness it; a moved count "+
				"means a branch may have gained or lost its only witness. Re-read §3 "+
				"against the new shapes before re-pinning.", c.name, c.got, c.want)
		}
	}

	if t.Failed() {
		t.Logf("full shape dump:\n%s", dumpShapes(shapes))
	}
}

// TestIndexDDLReorderWitnesses pins the exact statements for which Java's
// reorderValues (MaterializedViewIndexGenerator.java:387-394) produces a key
// order different from the projection order.
//
// These are the only corpus evidence that the ORDER BY — not the SELECT list —
// fixes the index key order. RFC-202 gate (a) mutation 1 flips that rule; with
// no witness the mutation stays green and the wrong key order ships.
func TestIndexDDLReorderWitnesses(t *testing.T) {
	t.Parallel()

	var got []string
	for _, s := range collectIndexShapes(t) {
		if !s.OnSource && !s.Aggregate && s.Reordered {
			got = append(got, s.File+":"+s.Name)
		}
	}
	sort.Strings(got)

	want := []string{
		// `select name, price … where price > 20 order by price` — the key is
		// (PRICE) and NAME is pushed past the split point, so the projection
		// order and the key order genuinely differ.
		"index-ddl-values-only.yamsql:IDX_MV_FILTERED_EXPENSIVE",
		"orderby.yamsql:I2",
		"orderby.yamsql:I3",
		"orderby.yamsql:I4",
		"orderby.yamsql:I6",
		"orderby.yamsql:I8",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("reorder witnesses = %v, pinned %v\n\n"+
			"These statements are the only ones where the ORDER BY list is a "+
			"permutation of the projection rather than a prefix of it. Losing them "+
			"disarms RFC-202 gate (a) mutation 1 (key order taken from the "+
			"projection instead of the ORDER BY).", got, want)
	}
}

// TestIndexDDLCensusScope pins what the census does NOT see.
//
// The census walks `schema_template` blocks, which is where index DDL is
// declared for the engine's DDL path. One corpus file additionally issues index
// DDL as ad-hoc statements inside a test block, and those statements are outside
// the census's reach. That exclusion is a fact about the instrument, so it is
// asserted rather than described: if another file starts declaring indexes
// outside a schema template, §3's surface silently shrinks unless this fails.
func TestIndexDDLCensusScope(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for _, s := range collectIndexShapes(t) {
		seen[s.File] = true
	}
	// Measured: this file declares two `create index … as select` statements as
	// test-block DDL, not in its schema template.
	const outsideTemplate = "create-drop-create-template.yamsql"
	if seen[outsideTemplate] {
		t.Errorf("%s now declares index DDL inside a schema template; the census "+
			"scope note in RFC-202 §3 is stale and its totals moved by 2 statements",
			outsideTemplate)
	}
}

func collectIndexShapes(t *testing.T) []indexShape {
	t.Helper()
	corpus, err := javayamsql.OpenCorpus()
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	paths, err := corpus.List()
	if err != nil {
		t.Fatalf("list corpus: %v", err)
	}

	var shapes []indexShape
	for _, p := range paths {
		file, err := corpus.ParseFile(p)
		if err != nil {
			// A file the corpus parser rejects is the parse gate's business,
			// not this census's.
			continue
		}
		for _, block := range file.Blocks {
			if block.SchemaTemplate == nil {
				continue
			}
			for _, variant := range block.SchemaTemplate.Variants {
				got, err := classifyTemplate(p, variant.Definition, variant.Line)
				if err != nil {
					t.Logf("UNPARSED %s (template at line %d): %v", p, variant.Line, err)
					continue
				}
				shapes = append(shapes, got...)
			}
		}
	}
	return shapes
}

// classifyTemplate parses one CREATE SCHEMA TEMPLATE body and classifies each
// index definition in it. baseLine is the body's first line in the corpus file,
// so reported lines are file lines, not body offsets.
func classifyTemplate(path, definition string, baseLine int) ([]indexShape, error) {
	body := definition
	if !strings.Contains(strings.ToUpper(body), "CREATE SCHEMA TEMPLATE") {
		// The prefix stays on line 1 so every token's line number is still the
		// body's own.
		body = "CREATE SCHEMA TEMPLATE census_template " + body
	}
	root, err := parser.Parse(body)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("parser returned no tree")
	}
	var shapes []indexShape
	for _, node := range descendants(root) {
		switch def := node.(type) {
		case *antlrgen.IndexAsSelectDefinitionContext:
			shapes = append(shapes, classifyAsSelect(path, def, baseLine))
		case *antlrgen.IndexOnSourceDefinitionContext:
			shapes = append(shapes, classifyOnSource(path, def, baseLine))
		}
	}
	return shapes, nil
}

func classifyAsSelect(path string, def *antlrgen.IndexAsSelectDefinitionContext, baseLine int) indexShape {
	s := indexShape{
		File:          path,
		Name:          strings.ToUpper(strings.ReplaceAll(def.GetIndexName().GetText(), "\"", "")),
		Unique:        def.UNIQUE() != nil,
		HasAttributes: def.IndexAttributes() != nil,
	}
	qt := def.QueryTerm()
	if qt == nil {
		return s
	}
	kids := descendants(qt)

	var simple *antlrgen.SimpleTableContext
	for _, n := range kids {
		if st, ok := n.(*antlrgen.SimpleTableContext); ok {
			simple = st
			break
		}
	}
	if simple == nil {
		return s
	}

	// Aggregate: any aggregate function call in the projection. Java decides
	// this on the flattened result VALUES (MaterializedViewIndexGenerator.java:181);
	// on the parse tree the aggregate node is the faithful proxy, and it is what
	// separates the generator's two arms.
	selElems := simple.SelectElements()
	if selElems != nil {
		for _, n := range descendants(selElems) {
			if _, ok := n.(*antlrgen.AggregateWindowedFunctionContext); ok {
				s.Aggregate = true
			}
		}
		s.SelectElements = len(selElems.AllSelectElement())
		for _, e := range selElems.AllSelectElement() {
			if _, ok := e.(*antlrgen.SelectStarElementContext); ok {
				s.Star = true
			}
			for _, n := range descendants(e) {
				if fc, ok := n.(*antlrgen.FullColumnNameContext); ok {
					if fid := fc.FullId(); fid != nil && len(fid.AllDOT()) > 0 {
						s.Dotted = true
					}
				}
			}
		}
	}
	// GROUP BY without an aggregate call cannot happen in a legal index
	// definition, but classify it with the aggregate arm regardless: it is the
	// arm Java's generate() takes.
	if simple.GroupByClause() != nil {
		s.Aggregate = true
	}

	if fc := simple.FromClause(); fc != nil {
		s.Where = fc.WhereExpr() != nil
		if ts := fc.TableSources(); ts != nil {
			s.FromSources = len(ts.AllTableSource())
		}
	}

	projection := projectionKeys(selElems)
	var orderKeys []string
	if ob := simple.OrderByClause(); ob != nil {
		for _, oe := range ob.AllOrderByExpression() {
			s.OrderByCount++
			if oc := oe.OrderClause(); oc != nil {
				s.ExplicitOrder = true
			}
			orderKeys = append(orderKeys, canonicalKey(oe.Expression()))
		}
	}
	s.RowVersion = strings.Contains(strings.ToUpper(canonicalKey(selElems)), "__ROW_VERSION")

	// Covering and Reordered are stated in reorderValues' own terms
	// (MaterializedViewIndexGenerator.java:387-394 + :195-203):
	//   key   = ORDER BY values, in ORDER BY order
	//   value = the remaining projection values, in projection order
	// so the statement is COVERING when the split point falls strictly inside
	// the projection, and REORDERED when that concatenation differs from the
	// projection order — which is exactly when taking the key order from the
	// projection instead would build a different index.
	if s.OrderByCount > 0 && s.SelectElements > s.OrderByCount {
		s.Covering = true
	}
	// `SELECT *` has no enumerable projection to compare against, so the
	// reorder question is not askable of it here — Java answers it after star
	// expansion, which needs the catalog this census does not have.
	if len(orderKeys) > 0 && len(projection) > 0 && !s.Star {
		s.Reordered = !prefixMatches(projection, orderKeys)
	}
	return s
}

func classifyOnSource(path string, def *antlrgen.IndexOnSourceDefinitionContext, baseLine int) indexShape {
	s := indexShape{
		File:     path,
		Name:     strings.ToUpper(strings.ReplaceAll(def.GetIndexName().GetText(), "\"", "")),
		OnSource: true,
		Unique:   def.UNIQUE() != nil,
	}
	if def.IndexOptions() != nil {
		s.HasAttributes = true
	}
	if cl := def.IndexColumnList(); cl != nil {
		for _, spec := range cl.AllIndexColumnSpec() {
			s.KeyColumns++
			if spec.OrderClause() != nil {
				s.ExplicitOrder = true
			}
		}
	}
	if inc := def.IncludeClause(); inc != nil {
		if ul := inc.UidList(); ul != nil {
			s.IncludeColumns = len(ul.AllUid())
		}
	}
	return s
}

// projectionKeys returns the addressable names of each SELECT element, in
// projection order: the underlying expression AND the `AS` alias, because an
// ORDER BY may name either (`SELECT CARDINALITY(a) AS card … ORDER BY card`).
func projectionKeys(selElems antlrgen.ISelectElementsContext) [][]string {
	if selElems == nil {
		return nil
	}
	keys := make([][]string, 0, len(selElems.AllSelectElement()))
	for _, e := range selElems.AllSelectElement() {
		switch el := e.(type) {
		case *antlrgen.SelectExpressionElementContext:
			names := []string{canonicalKey(el.Expression())}
			if uid := el.Uid(); uid != nil {
				names = append(names, canonicalKey(uid))
			}
			keys = append(keys, names)
		default:
			keys = append(keys, []string{canonicalKey(e)})
		}
	}
	return keys
}

// prefixMatches reports whether the order keys are a positional prefix of the
// projection — i.e. reorderValues leaves the order unchanged.
func prefixMatches(projection [][]string, orderKeys []string) bool {
	if len(orderKeys) > len(projection) {
		return false
	}
	for i, k := range orderKeys {
		matched := false
		for _, name := range projection[i] {
			if sameKey(name, k) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// sameKey compares two canonical keys ignoring a leading table qualifier, which
// the corpus uses inconsistently between the projection and the ORDER BY
// (`select q.b, q.a from t2 order by q.a` vs `select t.col2 … order by col2`).
func sameKey(a, b string) bool {
	if a == b {
		return true
	}
	return lastSegment(a) == lastSegment(b)
}

func lastSegment(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

// canonicalKey renders a parse subtree as an upper-cased, quote-stripped,
// whitespace-free key. It is an identity proxy for THIS census only: two
// statements are compared against each other, never a name resolved against a
// catalog. The generator itself must use value identity, never text
// (RFC-202 D3).
func canonicalKey(node antlr.Tree) string {
	if node == nil {
		return ""
	}
	rc, ok := node.(antlr.ParserRuleContext)
	if !ok {
		return ""
	}
	text := rc.GetText()
	text = strings.ReplaceAll(text, "\"", "")
	text = strings.ReplaceAll(text, " ", "")
	return strings.ToUpper(text)
}

func descendants(node antlr.Tree) []antlr.Tree {
	var out []antlr.Tree
	var walk func(antlr.Tree)
	walk = func(n antlr.Tree) {
		if n == nil {
			return
		}
		out = append(out, n)
		for i := 0; i < n.GetChildCount(); i++ {
			walk(n.GetChild(i))
		}
	}
	walk(node)
	return out
}

func dumpShapes(shapes []indexShape) string {
	var b strings.Builder
	for _, s := range shapes {
		fmt.Fprintf(&b, "%s:%s onSource=%t agg=%t sel=%d ob=%d cover=%t reorder=%t "+
			"where=%t uniq=%t attrs=%t src=%d star=%t rv=%t dotted=%t explicitOrder=%t key=%d incl=%d\n",
			s.File, s.Name, s.OnSource, s.Aggregate, s.SelectElements, s.OrderByCount,
			s.Covering, s.Reordered, s.Where, s.Unique, s.HasAttributes, s.FromSources,
			s.Star, s.RowVersion, s.Dotted, s.ExplicitOrder, s.KeyColumns, s.IncludeColumns)
	}
	return b.String()
}

package embedded

import (
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"github.com/antlr4-go/antlr/v4"
)

const (
	// parseCacheEntries bounds a connection's parse cache.
	parseCacheEntries = 64
	// parseCacheMaxText bounds the text of a cached statement. A repeated
	// statement is parameterised and short; a long one (a literal batch
	// INSERT) is rarely repeated and its tree is large.
	parseCacheMaxText = 8 << 10
)

// parseCache keeps a connection's recent parse trees by exact SQL text, so a
// repeated statement skips the ANTLR parse that precedes the plan cache. A
// tree is a pure function of its text and is only read after parsing. It is reused by one connection only,
// one statement at a time: parameter values are bound to its tokens for the
// statement (bindStatementParameters) and released at the statement's end,
// so two statements must not hold one tree at once. Nothing mutates a tree
// after parsing. A failed parse is not kept. The cache outlives ResetSession,
// which changes nothing a parse reads.
type parseCache struct {
	trees map[string]*parsedStatement
	order []string // insertion order, for FIFO eviction
}

// parsedStatement is a cached parse tree and the binding walks compiled over
// it, which live and die with the tree.
type parsedStatement struct {
	root     antlrgen.IRootContext
	programs statementPrograms
}

// statementProgramsMax bounds a statement's compiled walks: one per planned
// query node and runtime flag, so a handful; a node of another tree (a view
// body parsed per statement) must not grow it without bound.
const statementProgramsMax = 8

// statementPrograms are a statement's compiled binding walks
// (query_bindings.go). A nil *statementPrograms keeps nothing.
type statementPrograms struct {
	m map[bindingProgramKey]*bindingProgram
}

func (s *statementPrograms) get(tree antlr.ParseTree, runtime bool) *bindingProgram {
	if s == nil {
		return nil
	}
	return s.m[bindingProgramKey{tree, runtime}]
}

func (s *statementPrograms) put(tree antlr.ParseTree, runtime bool, p *bindingProgram) {
	if s == nil || len(s.m) >= statementProgramsMax {
		return
	}
	if s.m == nil {
		s.m = make(map[bindingProgramKey]*bindingProgram)
	}
	s.m[bindingProgramKey{tree, runtime}] = p
}

// parseStatement is parser.Parse through the connection's cache, returning
// the cache entry; a statement too long to cache gets an entry of its own.
func (c *EmbeddedConnection) parseStatement(sql string) (*parsedStatement, error) {
	if st, ok := c.parses.trees[sql]; ok {
		return st, nil
	}
	root, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	st := &parsedStatement{root: root}
	if len(sql) > parseCacheMaxText {
		return st, nil
	}
	if c.parses.trees == nil {
		c.parses.trees = make(map[string]*parsedStatement)
	}
	if len(c.parses.order) >= parseCacheEntries {
		delete(c.parses.trees, c.parses.order[0])
		c.parses.order = c.parses.order[1:]
	}
	c.parses.trees[sql] = st
	c.parses.order = append(c.parses.order, sql)
	return st, nil
}

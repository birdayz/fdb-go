package embedded

import (
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
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
// repeated statement skips the ANTLR parse that precedes the plan cache (about
// a quarter of a cached point query's CPU). A tree is a pure function of its
// text and is only read after parsing. It is reused by one connection only,
// one statement at a time: parameter values are bound to its tokens for the
// statement (bindStatementParameters) and released at the statement's end,
// so two statements must not hold one tree at once. Nothing mutates a tree
// after parsing. A failed parse is not kept. The cache outlives ResetSession,
// which changes nothing a parse reads.
type parseCache struct {
	trees map[string]antlrgen.IRootContext
	order []string // insertion order, for FIFO eviction
}

// parse is parser.Parse through the connection's cache.
func (c *EmbeddedConnection) parse(sql string) (antlrgen.IRootContext, error) {
	if root, ok := c.parses.trees[sql]; ok {
		return root, nil
	}
	root, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	if len(sql) > parseCacheMaxText {
		return root, nil
	}
	if c.parses.trees == nil {
		c.parses.trees = make(map[string]antlrgen.IRootContext)
	}
	if len(c.parses.order) >= parseCacheEntries {
		delete(c.parses.trees, c.parses.order[0])
		c.parses.order = c.parses.order[1:]
	}
	c.parses.trees[sql] = root
	c.parses.order = append(c.parses.order, sql)
	return root, nil
}

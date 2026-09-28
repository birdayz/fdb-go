package conformance_test

import (
	"strconv"
	"strings"
)

// wsfAccessPath reduces one engine's EXPLAIN text to the access path the WS-F
// acceptance compares (ws-f-design.md section 13): which leaves read which
// relation or index, with which kinds of bound comparison, covering or not;
// where the fetch, the residual filter and any sort sit; and the IN, union and
// join operators above them. Projections, the IN list's own values, predicate
// text and output names are dropped: they are explain syntax, not access path.
// engine is "java" (the target's pipeline syntax, stages joined by " | ") or
// "go" (Go's nested-call syntax). An operator the reducer does not know is
// rendered as "?NAME", so a new shape shows up as a difference rather than
// being folded into a known one.
func wsfAccessPath(engine, explain string) string {
	explain = strings.TrimSpace(explain)
	if engine == "java" {
		return wsfJavaPipeline(explain).String()
	}
	return wsfGoPlan(explain).String()
}

// wsfPathNode is one operator of an access path.
type wsfPathNode struct {
	op       string
	name     string   // the relation or index a leaf reads
	cmps     []string // a leaf's bound comparison kinds, in key order
	children []*wsfPathNode
}

func (n *wsfPathNode) String() string {
	if n == nil {
		return "<nil>"
	}
	var args []string
	if n.name != "" {
		args = append(args, n.name)
	}
	if len(n.cmps) > 0 {
		args = append(args, "["+strings.Join(n.cmps, ",")+"]")
	}
	for _, c := range n.children {
		args = append(args, c.String())
	}
	if len(args) == 0 {
		return n.op
	}
	return n.op + "(" + strings.Join(args, " ") + ")"
}

// wsfPathClass masks the index names of an access path, for a row whose index
// choice only a plan hash decides (section 13: asserted at class level).
func wsfPathClass(path string) string {
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		for _, op := range []string{"ISCAN(", "COVERING("} {
			if strings.HasPrefix(path[i:], op) {
				b.WriteString(op + "*")
				i += len(op)
				for i < len(path) && path[i] != ' ' && path[i] != ')' {
					i++
				}
				break
			}
		}
		if i < len(path) {
			b.WriteByte(path[i])
		}
	}
	return b.String()
}

// wsfSplitTop splits s on sep where sep is outside every (), [], {} and quote.
func wsfSplitTop(s, sep string) []string {
	var parts []string
	depth, start := 0, 0
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			inQuote = !inQuote
		case inQuote:
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		case depth == 0 && strings.HasPrefix(s[i:], sep):
			parts = append(parts, s[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	return append(parts, s[start:])
}

// wsfBracketed returns the text inside the first bracket pair opening at or
// after from (open is one of "(", "[", "{"), and the index after its close.
func wsfBracketed(s string, from int, open byte) (string, int) {
	close := map[byte]byte{'(': ')', '[': ']', '{': '}'}[open]
	i := strings.IndexByte(s[from:], open)
	if i < 0 {
		return "", len(s)
	}
	i += from
	depth := 0
	for j := i; j < len(s); j++ {
		switch s[j] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[i+1 : j], j + 1
			}
		}
	}
	return s[i+1:], len(s)
}

// wsfJavaCmpKind is the kind of one of the target's scan comparisons.
func wsfJavaCmpKind(c string) string {
	word := strings.Fields(strings.TrimSpace(c))
	if len(word) == 0 {
		return ""
	}
	switch word[0] {
	case "IS": // the record-type restriction of a primary scan, not a key bound
		return ""
	case "EQUALS":
		return "="
	case "NOT_DISTINCT_FROM":
		return "≡"
	case "GREATER_THAN", "GREATER_THAN_OR_EQUALS", "LESS_THAN", "LESS_THAN_OR_EQUALS", "STARTS_WITH", "NOT_NULL":
		return "<>"
	}
	return "?" + word[0]
}

// wsfJavaLeaf reads SCAN([...]), ISCAN(NAME [cmps]) and COVERING(NAME [cmps] -> [...]).
func wsfJavaLeaf(op, s string) *wsfPathNode {
	body, _ := wsfBracketed(s, 0, '(')
	n := &wsfPathNode{op: op}
	if op == "SCAN" {
		inner, _ := wsfBracketed(body, 0, '[')
		for _, c := range wsfSplitTop(inner, ",") {
			if f := strings.Fields(strings.TrimSpace(c)); len(f) == 2 && f[0] == "IS" {
				n.name = f[1]
			} else if k := wsfJavaCmpKind(c); k != "" {
				n.cmps = append(n.cmps, k)
			}
		}
		return n
	}
	body = wsfSplitTop(body, " -> ")[0]
	fields := strings.Fields(body)
	if len(fields) > 0 {
		n.name = fields[0]
	}
	if i := strings.IndexByte(body, '['); i >= 0 {
		inner, _ := wsfBracketed(body, i, '[')
		for _, c := range wsfSplitTop(inner, ",") {
			if k := wsfJavaCmpKind(c); k != "" {
				n.cmps = append(n.cmps, k)
			}
		}
	}
	return n
}

// wsfJavaPipeline reduces the target's pipeline syntax.
func wsfJavaPipeline(s string) *wsfPathNode {
	var cur *wsfPathNode
	for _, stage := range wsfSplitTop(s, " | ") {
		cur = wsfJavaStage(strings.TrimSpace(stage), cur)
	}
	return cur
}

func wsfJavaStage(s string, prev *wsfPathNode) *wsfPathNode {
	if union := wsfSplitTop(s, " ∪ "); len(union) > 1 {
		n := &wsfPathNode{op: "UNION"}
		for _, leg := range union {
			leg = wsfSplitTop(leg, " COMPARE BY ")[0]
			n.children = append(n.children, wsfJavaPipeline(leg))
		}
		return n
	}
	first := strings.Fields(s)
	if len(first) == 0 {
		return prev
	}
	switch {
	case strings.HasPrefix(s, "SCAN("):
		return wsfJavaLeaf("SCAN", s)
	case strings.HasPrefix(s, "ISCAN("):
		return wsfJavaLeaf("ISCAN", s)
	case strings.HasPrefix(s, "COVERING("):
		return wsfJavaLeaf("COVERING", s)
	case strings.HasPrefix(s, "["):
		// An IN source list: the in-join's outer (the next stage consumes it), or
		// an in-union whose inner follows in braces.
		if len(wsfSplitTop(s, " INUNION ")) > 1 {
			inner, _ := wsfBracketed(s, strings.Index(s, " INUNION "), '{')
			return &wsfPathNode{op: "INUNION", children: []*wsfPathNode{wsfJavaPipeline(inner)}}
		}
		return &wsfPathNode{op: "INSOURCE"}
	case first[0] == "INJOIN":
		inner, _ := wsfBracketed(s, 0, '{')
		return &wsfPathNode{op: "INJOIN", children: []*wsfPathNode{wsfJavaPipeline(inner)}}
	case first[0] == "FLATMAP":
		inner, _ := wsfBracketed(s, 0, '{')
		return &wsfPathNode{op: "FLATMAP", children: []*wsfPathNode{prev, wsfJavaPipeline(inner)}}
	case first[0] == "FILTER":
		return &wsfPathNode{op: "FILTER", children: []*wsfPathNode{prev}}
	case first[0] == "FETCH":
		return &wsfPathNode{op: "FETCH", children: []*wsfPathNode{prev}}
	case first[0] == "MAP":
		return prev
	case strings.HasPrefix(s, "ON EMPTY NULL"):
		return &wsfPathNode{op: "DEFAULTONEMPTY", children: []*wsfPathNode{prev}}
	case first[0] == "EXPLODE":
		return &wsfPathNode{op: "EXPLODE"}
	case first[0] == "INSERT" && len(first) >= 3:
		return &wsfPathNode{op: "INSERT", name: first[2]}
	}
	return &wsfPathNode{op: "?" + first[0], children: []*wsfPathNode{prev}}
}

// wsfGoCmpKinds reads Go's comparison list, e.g. "[=, *] COVERING".
func wsfGoCmpKinds(s string) []string {
	inner, _ := wsfBracketed(s, 0, '[')
	var out []string
	for _, c := range wsfSplitTop(inner, ",") {
		switch c = strings.TrimSpace(c); c {
		case "", "*":
		case "=":
			out = append(out, "=")
		case "<>", "<", ">", "<=", ">=", "[]":
			out = append(out, "<>")
		default:
			out = append(out, "?"+c)
		}
	}
	return out
}

// wsfGoPlan reduces Go's nested-call syntax.
func wsfGoPlan(s string) *wsfPathNode {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "outer="), "inner=")
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return &wsfPathNode{op: "?" + s}
	}
	name := s[:open]
	body, _ := wsfBracketed(s, open, '(')
	args := wsfSplitTop(body, ", ")
	arg := func(i int) string {
		if i < 0 {
			i += len(args)
		}
		if i < 0 || i >= len(args) {
			return ""
		}
		return strings.TrimSpace(args[i])
	}
	plan := func(i int) *wsfPathNode { return wsfGoPlan(arg(i)) }
	switch name {
	case "Scan":
		n := &wsfPathNode{op: "SCAN", name: arg(0)}
		if len(args) > 1 {
			n.cmps = wsfGoCmpKinds(arg(1))
		}
		return n
	case "IndexScan":
		n := &wsfPathNode{op: "ISCAN", name: arg(0)}
		if len(args) > 1 {
			n.cmps = wsfGoCmpKinds(arg(1))
			if strings.HasSuffix(arg(1), " COVERING") {
				n.op = "COVERING"
			}
		}
		return n
	case "Fetch":
		return &wsfPathNode{op: "FETCH", children: []*wsfPathNode{plan(0)}}
	case "PredicatesFilter":
		return &wsfPathNode{op: "FILTER", children: []*wsfPathNode{plan(0)}}
	case "Project", "Map", "TypeFilter":
		// The target folds the record type into its scan's [IS T]; Go's type filter
		// restricts the same scan.
		return plan(-1)
	case "InMemorySort":
		return &wsfPathNode{op: "SORT", children: []*wsfPathNode{plan(-1)}}
	case "InUnion":
		return &wsfPathNode{op: "INUNION", children: []*wsfPathNode{plan(0)}}
	case "InJoin":
		return &wsfPathNode{op: "INJOIN", children: []*wsfPathNode{plan(0)}}
	case "FlatMap":
		return &wsfPathNode{op: "FLATMAP", children: []*wsfPathNode{plan(0), plan(1)}}
	case "DefaultOnEmpty":
		return &wsfPathNode{op: "DEFAULTONEMPTY", children: []*wsfPathNode{plan(0)}}
	case "NestedLoopJoin":
		return &wsfPathNode{op: "NLJ", children: []*wsfPathNode{plan(-2), plan(-1)}}
	case "Union", "UnorderedUnion":
		n := &wsfPathNode{op: "UNION"}
		for i := range args {
			if a := arg(i); strings.Contains(a, "(") {
				n.children = append(n.children, wsfGoPlan(a))
			}
		}
		return n
	case "Insert":
		return &wsfPathNode{op: "INSERT", name: arg(0)}
	}
	return &wsfPathNode{op: "?" + name}
}

// wsfExplainOf is the EXPLAIN text of a rendered probe line, or false.
func wsfExplainOf(line string) (string, bool) {
	const prefix = `OK EXPLAIN "`
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, `"`) {
		return "", false
	}
	text, err := strconv.Unquote(line[len("OK EXPLAIN "):])
	if err != nil {
		return "", false
	}
	return text, true
}

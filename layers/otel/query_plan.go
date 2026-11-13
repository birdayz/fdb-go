package otel

// Storage-free query planning: validation, digests, time windows,
// continuation token framing and MACs, message query plans and trace graph
// construction.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

const (
	maxQueryPredicates   = 16
	maxContinuationBytes = 4096
	continuationKind     = "same_span"
	continuationKeyBytes = 32
	// sameSpanProbeBatch bounds how many verified driver candidates share one
	// pipelined round of point probes for the remaining predicates.
	sameSpanProbeBatch = 64
)

// timeWindow is a validated [lo, hi) over stored event times, which are 0..MaxInt64.
type timeWindow struct {
	set, empty bool
	lo, hi     int64
}

func newTimeWindow(r *TimeRange) (timeWindow, error) {
	if r == nil {
		return timeWindow{}, nil
	}
	if err := r.Validate(); err != nil {
		return timeWindow{}, err
	}
	w := timeWindow{set: true, lo: max(r.Start, 0), hi: r.End}
	w.empty = w.hi <= w.lo
	return w, nil
}

func (w timeWindow) contains(t int64) bool { return !w.set || (t >= w.lo && t < w.hi) }

func attributeLess(x, y Attribute) bool {
	if x.Scope != y.Scope {
		return x.Scope < y.Scope
	}
	if x.Key != y.Key {
		return x.Key < y.Key
	}
	if x.Type != y.Type {
		return x.Type < y.Type
	}
	return x.Value < y.Value
}

// uniqueAttributes drops repeated predicates, keeping first positions.
func uniqueAttributes(in []Attribute) []Attribute {
	out := make([]Attribute, 0, len(in))
	seen := make(map[Attribute]bool, len(in))
	for _, a := range in {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// queryDigest binds the predicate set (not its driver order) and time window.
func queryDigest(kind string, preds []Attribute, w timeWindow) []byte {
	sorted := append([]Attribute(nil), preds...)
	sort.Slice(sorted, func(i, j int) bool { return attributeLess(sorted[i], sorted[j]) })
	t := tuple.Tuple{kind, int64(len(sorted))}
	for _, a := range sorted {
		t = append(t, a.Scope, a.Key, a.Type, a.Value)
	}
	t = append(t, w.set, w.lo, w.hi)
	h := sha256.Sum256(t.Pack())
	return h[:]
}

// continuationMACIn is HMAC-SHA256 under secret of domain‖payload; the
// domain separates token versions.
func continuationMACIn(domain string, secret, payload []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(domain))
	m.Write(payload)
	return m.Sum(nil)
}

// splitContinuation splits a token into its payload's tuple fields and its
// MAC. A token of another length or a payload that is not a tuple is config.
func splitContinuation(token []byte) (fields tuple.Tuple, payload, mac []byte, err error) {
	const op = "continuation"
	if len(token) <= sha256.Size || len(token) > maxContinuationBytes {
		return nil, nil, nil, fail("config", op, "malformed continuation token length")
	}
	payload, mac = token[:len(token)-sha256.Size], token[len(token)-sha256.Size:]
	if fields, err = tuple.Unpack(payload); err != nil {
		return nil, nil, nil, typedError("config", op, "malformed continuation token", err)
	}
	return fields, payload, mac, nil
}

// checkContinuationMAC authenticates mac over payload under the tenant
// store's signing key; an absent key or a failed MAC is corruption.
func checkContinuationMAC(secret, payload, mac []byte) error {
	const op = "continuation"
	if len(secret) == 0 {
		return fail("corruption", op, "continuation signing key is absent for this store")
	}
	if !hmac.Equal(mac, continuationMACIn(relContinuationDomain, secret, payload)) {
		return fail("corruption", op, "continuation token failed authentication")
	}
	return nil
}

func validIdentity(s string, size int) bool {
	wire, err := hex.DecodeString(s)
	if err != nil || len(wire) != size || hex.EncodeToString(wire) != s {
		return false
	}
	var nonzero byte
	for _, b := range wire {
		nonzero |= b
	}
	return nonzero != 0
}

const (
	maxMessageQueryBytes    = 4096
	messageContinuationKind = "message_fts"
)

type messagePhraseTerm struct {
	term   string
	offset int
}

// tokenizeQueryTerms rejects overlong, unindexed query tokens because no answer could be exhaustive.
func tokenizeQueryTerms(text string) ([]messagePhraseTerm, error) {
	const op = "message query"
	if len(text) > maxMessageQueryBytes {
		return nil, failf("config", op, "query exceeds %d bytes", maxMessageQueryBytes)
	}
	list, err := recordlayer.DefaultTextTokenizerInstance().TokenizeToList(text, recordlayer.GlobalMinVersion, recordlayer.TokenizerModeQuery)
	if err != nil {
		return nil, typedError("config", op, "default tokenizer", err)
	}
	var out []messagePhraseTerm
	for i, tok := range list {
		if tok == "" {
			continue
		}
		if len(tok) > maxIndexedTokenBytes {
			return nil, failf("config", op, "query term exceeds the %d-byte indexed token cap", maxIndexedTokenBytes)
		}
		out = append(out, messagePhraseTerm{tok, i})
	}
	return out, nil
}

type messageQueryPlan struct {
	mode      string
	terms     []string // distinct, in query order
	phrase    []messagePhraseTerm
	attribute int64 // 0: any attribute
	attrKey   string
	role      string
	window    timeWindow
	firstTurn bool
	limit     int
	digest    []byte
}

// newMessageQueryPlan plans q's text, role, attribute, window and limit;
// newRelMessageQueryPlan plans and digests its same-span predicates.
func newMessageQueryPlan(op string, q MessageFullTextQuery, k int) (*messageQueryPlan, error) {
	mode := q.Mode
	if mode == "" {
		mode = MessageMatchAll
	}
	if mode != MessageMatchAll && mode != MessageMatchAny && mode != MessageMatchPhrase {
		return nil, failf("config", op, "unsupported message match mode %q", q.Mode)
	}
	phrase, err := tokenizeQueryTerms(q.Query)
	if err != nil {
		return nil, err
	}
	if len(phrase) == 0 || len(phrase) > maxMessageQueryTerms {
		return nil, failf("config", op, "expected 1..%d searchable query terms", maxMessageQueryTerms)
	}
	p := &messageQueryPlan{mode: mode, phrase: phrase, attrKey: q.AttributeKey, role: normalizeMessageRole(q.Role), firstTurn: q.FirstTurnOnly}
	base := phrase[0].offset
	seen := map[string]bool{}
	for i := range p.phrase {
		p.phrase[i].offset -= base
		if t := p.phrase[i].term; !seen[t] {
			seen[t] = true
			p.terms = append(p.terms, t)
		}
	}
	if q.AttributeKey != "" {
		if p.attribute = messageAttributeCode(q.AttributeKey); p.attribute <= 0 {
			return nil, failf("config", op, "attribute_key %q is not a GenAI message attribute", q.AttributeKey)
		}
	}
	if len(q.Role) > maxMessageRoleBytes {
		return nil, failf("config", op, "role exceeds %d bytes", maxMessageRoleBytes)
	}
	if q.Limit < 0 {
		return nil, fail("config", op, "limit must not be negative")
	}
	if p.window, err = newTimeWindow(q.TimeWindow); err != nil {
		return nil, err
	}
	p.limit = q.Limit
	if p.limit == 0 {
		p.limit = k
	}
	t := tuple.Tuple{messageContinuationKind, mode, int64(len(p.phrase))}
	if mode == MessageMatchPhrase {
		for _, pt := range p.phrase {
			t = append(t, pt.term, int64(pt.offset))
		}
	} else {
		sorted := append([]string(nil), p.terms...)
		sort.Strings(sorted)
		t = append(t, int64(len(sorted)))
		for _, s := range sorted {
			t = append(t, s)
		}
	}
	t = append(t, p.attrKey, p.role, p.firstTurn, queryDigest(messageContinuationKind, nil, p.window))
	h := sha256.Sum256(t.Pack())
	p.digest = h[:]
	return p, nil
}

// checkMessageAttributeQuery rejects equality on unindexed GenAI manifests
// rather than returning a misleading empty answer.
func checkMessageAttributeQuery(op string, a Attribute, decomposed bool) error {
	if decomposed && a.Scope == "span" && IsMessageDeltaAttribute(a.Key) {
		return failf("config", op, "%s holds decomposed GenAI messages; search it with QueryMessageFullText", a.Key)
	}
	return nil
}

func intersectTimeWindows(a, b timeWindow) timeWindow {
	if !a.set {
		return b
	}
	if !b.set {
		return a
	}
	w := timeWindow{set: true, lo: max(a.lo, b.lo), hi: min(a.hi, b.hi)}
	w.empty = a.empty || b.empty || w.hi <= w.lo
	return w
}

type graphSpan struct {
	match SpanMatch
	span  *tracepb.Span
}

func zeroID(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// buildTraceGraph is iterative and linear in the number of spans, so
// malformed parent chains (cycles, deep chains) cannot exhaust the stack.
func buildTraceGraph(traceID string, in []graphSpan) TraceGraph {
	g := TraceGraph{Trace: traceID, Spans: []SpanMatch{}, Roots: []string{}, Children: map[string][]string{}, Parent: map[string]string{}, MissingParents: []string{}, Orphans: []string{}, Cycles: []string{}, Depth: map[string]int{}, Links: map[string][]SpanLink{}}
	byID := make(map[string]graphSpan, len(in))
	for _, s := range in {
		byID[s.match.ID] = s
		g.Spans = append(g.Spans, s.match)
	}
	less := func(x, y string) bool {
		a, b := byID[x].match, byID[y].match
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		return a.ID < b.ID
	}
	sort.Slice(g.Spans, func(i, j int) bool { return less(g.Spans[i].ID, g.Spans[j].ID) })
	missing := map[string]bool{}
	for _, m := range g.Spans {
		sp := byID[m.ID].span
		if p := sp.GetParentSpanId(); len(p) > 0 && !zeroID(p) {
			parent := hex.EncodeToString(p)
			g.Parent[m.ID] = parent
			if _, ok := byID[parent]; ok {
				g.Children[parent] = append(g.Children[parent], m.ID)
			} else {
				missing[parent] = true
				g.Orphans = append(g.Orphans, m.ID)
			}
		} else {
			g.Roots = append(g.Roots, m.ID)
		}
		for _, l := range sp.GetLinks() {
			link := SpanLink{TraceID: hex.EncodeToString(l.GetTraceId()), SpanID: hex.EncodeToString(l.GetSpanId()), TraceState: l.GetTraceState(), Flags: l.GetFlags(), Attributes: l.GetAttributes()}
			_, stored := byID[link.SpanID]
			link.Resolved = link.TraceID == traceID && stored
			g.Links[m.ID] = append(g.Links[m.ID], link)
		}
	}
	for p := range missing {
		g.MissingParents = append(g.MissingParents, p)
	}
	sort.Strings(g.MissingParents)
	for _, c := range g.Children {
		sort.Slice(c, func(i, j int) bool { return less(c[i], c[j]) })
	}
	// Each span has at most one stored parent, so a colored walk up the chain
	// finds every cycle once: a node met again on the current path closes one.
	const (
		unvisited = iota
		onPath
		done
	)
	state := make(map[string]int, len(in))
	for _, m := range g.Spans {
		var path []string
		pos := map[string]int{}
		cur := m.ID
		for {
			if _, ok := byID[cur]; !ok || state[cur] == done {
				break
			}
			if state[cur] == onPath {
				g.Cycles = append(g.Cycles, path[pos[cur]:]...)
				break
			}
			state[cur] = onPath
			pos[cur] = len(path)
			path = append(path, cur)
			parent, ok := g.Parent[cur]
			if !ok {
				break
			}
			cur = parent
		}
		for _, id := range path {
			state[id] = done
		}
	}
	sort.Strings(g.Cycles)
	queue := append([]string(nil), g.Roots...)
	for _, r := range g.Roots {
		g.Depth[r] = 0
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range g.Children[cur] {
			if _, seen := g.Depth[c]; !seen {
				g.Depth[c] = g.Depth[cur] + 1
				g.MaxDepth = max(g.MaxDepth, g.Depth[c])
				queue = append(queue, c)
			}
		}
	}
	return g
}

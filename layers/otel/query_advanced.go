package otel

import (
	"bytes"
	"context"
	"crypto/rand"
	"sort"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// SameSpanQuery intersects predicates on one (trace_id, span_id), in posting order.
// Predicates[0] drives the scan; put the most selective predicate first.
type SameSpanQuery struct {
	Predicates   []Attribute `json:"predicates"`
	TimeWindow   *TimeRange  `json:"time_window,omitempty"`
	Limit        int         `json:"limit,omitempty"`
	Continuation []byte      `json:"continuation,omitempty"`
}

// SpanQueryPage is one snapshot_page: every row was verified at ReadVersion.
// A full page always carries a continuation; the following page may be empty.
type SpanQueryPage struct {
	Spans        []SpanMatch `json:"spans"`
	Continuation []byte      `json:"continuation,omitempty"`
	Order        string      `json:"order"`
	ReadVersion  int64       `json:"read_version"`
	Candidates   int         `json:"candidates"`
	Bytes        int64       `json:"logical_read_bytes"`
}

// TraceExistentialQuery allows different span/log/message witnesses within one trace (RFC-283 §6.1).
// Logs use LOG_TRACE correlation; message clauses take neither Limit nor Continuation.
type TraceExistentialQuery struct {
	Clauses        []Attribute            `json:"clauses"`
	LogClauses     []Attribute            `json:"log_clauses,omitempty"`
	MessageClauses []MessageFullTextQuery `json:"message_clauses,omitempty"`
	TimeWindow     *TimeRange             `json:"time_window,omitempty"`
	Limit          int                    `json:"limit,omitempty"`
}

// TraceExistentialHit keeps newest-first witnesses per matching span/log/message clause.
// Latest is the newest timestamp across all witnesses.
type TraceExistentialHit struct {
	Trace            string             `json:"trace_id"`
	Latest           int64              `json:"latest"`
	Witnesses        [][]SpanMatch      `json:"witnesses"`
	LogWitnesses     [][]LogMatch       `json:"log_witnesses,omitempty"`
	MessageWitnesses [][]MessageSpanHit `json:"message_witnesses,omitempty"`
}

type TraceExistentialResult struct {
	Hits        []TraceExistentialHit `json:"hits"`
	ReadVersion int64                 `json:"read_version"`
	Candidates  int                   `json:"candidates"`
	Bytes       int64                 `json:"logical_read_bytes"`
}

// SpanLink is one stored OTLP link; Resolved reports that its target is a
// span stored in the same trace. Other targets stay unresolved, never absent.
type SpanLink struct {
	TraceID    string               `json:"trace_id"`
	SpanID     string               `json:"span_id"`
	TraceState string               `json:"trace_state,omitempty"`
	Flags      uint32               `json:"flags,omitempty"`
	Attributes []*commonpb.KeyValue `json:"attributes,omitempty"`
	Resolved   bool                 `json:"resolved"`
}

// TraceGraph is the observed parent adjacency of one trace. A span whose
// parent is not stored is an orphan, not a root; its ancestry is unknown.
type TraceGraph struct {
	Trace          string                `json:"trace_id"`
	Spans          []SpanMatch           `json:"spans"`
	Roots          []string              `json:"roots"`
	Children       map[string][]string   `json:"children"`
	Parent         map[string]string     `json:"parent"`
	MissingParents []string              `json:"missing_parents"`
	Orphans        []string              `json:"orphans"`
	Cycles         []string              `json:"cycles"`
	Depth          map[string]int        `json:"depth"`
	MaxDepth       int                   `json:"max_depth"`
	Links          map[string][]SpanLink `json:"links"`
}

// descendingRange is the key range of prefix P whose first element is -t for
// t inside w; w must be set and nonempty, so 1-hi and 1-lo cannot overflow.
func (w timeWindow) descendingRange(p subspace.Subspace) (fdb.Key, fdb.Key) {
	return p.Pack(tuple.Tuple{1 - w.hi}), p.Pack(tuple.Tuple{1 - w.lo})
}

// verify authenticates the token, then checks its bindings to this run and
// query, and returns the validated last emitted sort key.
func (c *continuation) verify(secret []byte, b *FDBBackend, digest []byte) (tuple.Tuple, error) {
	const op = "continuation"
	if err := c.authenticate(secret, b, digest); err != nil {
		return nil, err
	}
	last, err := tuple.Unpack(c.last)
	if err != nil {
		return nil, typedError("corruption", op, "continuation sort key", err)
	}
	if err := checkSortKey(last, c.order); err != nil {
		return nil, err
	}
	return last, nil
}

// authenticate checks the MAC, then the token's run, tenant, order and query.
func (c *continuation) authenticate(secret []byte, b *FDBBackend, digest []byte) error {
	const op = "continuation"
	if err := c.checkMAC(secret); err != nil {
		return err
	}
	switch {
	case c.run != b.cfg.Run:
		return fail("config", op, "continuation belongs to another run")
	case c.tenant != int64(b.cfg.Tenant):
		return fail("config", op, "continuation belongs to another tenant")
	case c.order != b.cfg.Order:
		return fail("config", op, "continuation belongs to another posting order")
	case !bytes.Equal(c.digest, digest):
		return fail("config", op, "continuation belongs to another query")
	}
	return nil
}

func checkSortKey(t tuple.Tuple, order string) error {
	bad := fail("corruption", "continuation", "malformed continuation sort key")
	if order == "time" {
		if len(t) != 3 {
			return bad
		}
		start, sok := t[0].(int64)
		rev, rok := t[1].([]byte)
		span, pok := t[2].(string)
		if !sok || !rok || !pok || start > 0 || !validIdentity(restoreTrace(rev), 16) || !validIdentity(span, 8) {
			return bad
		}
		return nil
	}
	if len(t) != 2 {
		return bad
	}
	trace, tok := t[0].(string)
	span, pok := t[1].(string)
	if !tok || !pok || !validIdentity(trace, 16) || !validIdentity(span, 8) {
		return bad
	}
	return nil
}

func (b *FDBBackend) continuationKey() fdb.Key {
	return b.keys.queryKey
}

func (b *FDBBackend) readContinuationSecret(rc *recordlayer.FDBRecordContext) ([]byte, error) {
	v, err := rc.ReadTransaction(true).Get(b.continuationKey()).Get()
	if err != nil {
		return nil, err
	}
	if len(v) != 0 && len(v) != continuationKeyBytes {
		return nil, fail("corruption", "continuation", "malformed continuation signing key")
	}
	return v, nil
}

// ensureContinuationSecret publishes the run's signing key on first use; the
// candidate is drawn outside the retry loop and loses to any concurrent winner.
func (b *FDBBackend) ensureContinuationSecret(ctx context.Context) ([]byte, error) {
	fresh := make([]byte, continuationKeyBytes)
	if _, err := rand.Read(fresh); err != nil {
		return nil, wrap("operation", "continuation key", err)
	}
	var out []byte
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		if err := configureStoreContext(rc); err != nil {
			return nil, err
		}
		if err := b.readManifest(rc, true); err != nil {
			return nil, err
		}
		v, err := rc.Transaction().Get(b.continuationKey()).Get()
		if err != nil {
			return nil, err
		}
		switch len(v) {
		case 0:
			rc.Transaction().Set(b.continuationKey(), fresh)
			out = fresh
		case continuationKeyBytes:
			out = v
		default:
			return nil, fail("corruption", "continuation", "malformed continuation signing key")
		}
		return nil, nil
	})
	return out, err
}

// openPostings checks run ownership and readability and returns the typed
// equality maintainer whose layout (width, order, value format) the scans use.
func (b *FDBBackend) openPostings(rc *recordlayer.FDBRecordContext, op string) (*postingMaintainer, error) {
	_, pm, err := b.openPostingsStore(rc, op)
	return pm, err
}

// openPostingsStore is openPostings that also returns the opened store.
func (b *FDBBackend) openPostingsStore(rc *recordlayer.FDBRecordContext, op string) (*recordlayer.FDBRecordStore, *postingMaintainer, error) {
	if err := b.readManifest(rc, true); err != nil {
		return nil, nil, err
	}
	store, err := b.open(rc)
	if err != nil {
		return nil, nil, err
	}
	state, err := store.ReadIndexState(b.idx.Name)
	if err != nil {
		return nil, nil, err
	}
	if !state.IsScannable() {
		return nil, nil, fail("ownership", op, "index is not readable")
	}
	m, err := store.GetIndexMaintainer(b.idx)
	if err != nil {
		return nil, nil, err
	}
	pm, ok := recordlayer.IndexMaintainerAs[*postingMaintainer](m)
	if !ok {
		return nil, nil, fail("config", op, "custom scanner not linked")
	}
	return store, pm, nil
}

// decodeSpanPosting validates one posting below its term prefix and returns
// the span it names with the covering attribute it carries.
func decodeSpanPosting(pm *postingMaintainer, term subspace.Subspace, kv fdb.KeyValue) (SpanMatch, Attribute, error) {
	v, err := decodeCoveringValue(kv.Value, pm.valueFormat)
	if err != nil {
		return SpanMatch{}, Attribute{}, err
	}
	pk, err := term.Unpack(kv.Key)
	if err != nil {
		return SpanMatch{}, Attribute{}, wrap("corruption", "posting key", err)
	}
	bad := func(msg string) (SpanMatch, Attribute, error) {
		return SpanMatch{}, Attribute{}, fail("corruption", "posting", msg)
	}
	m := SpanMatch{Start: v.Start}
	if pm.order == "time" {
		if len(pk) != 4 {
			return bad("malformed time posting")
		}
		ts, ok := pk[0].(int64)
		if !ok || ts != -v.Start {
			return bad("invalid covering time")
		}
		rev, rok := pk[1].([]byte)
		span, sok := pk[2].(string)
		_, ook := pk[3].(int64)
		if !rok || !sok || !ook {
			return bad("malformed time posting identity")
		}
		m.Trace, m.ID = restoreTrace(rev), span
	} else {
		if len(pk) != 3 {
			return bad("malformed trace posting")
		}
		trace, tok := pk[0].(string)
		span, sok := pk[1].(string)
		_, ook := pk[2].(int64)
		if !tok || !sok || !ook {
			return bad("malformed trace posting identity")
		}
		m.Trace, m.ID = trace, span
	}
	if !validIdentity(m.Trace, 16) || !validIdentity(m.ID, 8) {
		return bad("invalid posting span identity")
	}
	return m, v.Attribute, nil
}

// spanTermRange is the driver range of one term: bounded by the window in
// time order and starting strictly after the resumed sort key.
func spanTermRange(term subspace.Subspace, order string, w timeWindow, resume tuple.Tuple) (fdb.KeyRange, bool, error) {
	b, e := term.FDBRangeKeys()
	begin, end := b.FDBKey(), e.FDBKey()
	if w.set && order == "time" {
		begin, end = w.descendingRange(term)
	}
	if resume != nil {
		after, err := fdb.Strinc(term.Pack(resume))
		if err != nil {
			return fdb.KeyRange{}, false, wrap("corruption", "continuation", err)
		}
		if bytes.Compare(after, begin) > 0 {
			begin = after
		}
	}
	return fdb.KeyRange{Begin: begin, End: end}, bytes.Compare(begin, end) < 0, nil
}

func (b *FDBBackend) newBudget(op string) *queryBudget {
	return &queryBudget{op: op, maxCandidates: b.cfg.MaxCandidates, maxBytes: b.cfg.MaxBytes}
}

// QuerySameSpan returns a posting-ordered snapshot_page; signed continuations
// resume at a new read version.
func (b *FDBBackend) QuerySameSpan(ctx context.Context, q SameSpanQuery) (SpanQueryPage, error) {
	const op = "same-span query"
	b.lastWork = QueryResult{}
	page := SpanQueryPage{Spans: []SpanMatch{}, Order: b.cfg.Order}
	plan, err := newSameSpanPlan(q, b.cfg.K)
	if err != nil {
		return page, err
	}
	for _, a := range plan.preds {
		if err := checkMessageAttributeQuery(op, a, b.cfg.Messages != messagesNone); err != nil {
			return page, err
		}
	}
	var token *continuation
	if len(q.Continuation) > 0 {
		if token, err = parseContinuation(q.Continuation); err != nil {
			return page, err
		}
	}
	var secret []byte
	budget := b.newBudget(op)
	_, err = b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		page = SpanQueryPage{Spans: []SpanMatch{}, Order: b.cfg.Order}
		budget = b.newBudget(op)
		pm, err := b.openPostings(rc, op)
		if err != nil {
			return nil, err
		}
		if secret, err = b.readContinuationSecret(rc); err != nil {
			return nil, err
		}
		var resume tuple.Tuple
		if token != nil {
			if resume, err = token.verify(secret, b, plan.digest); err != nil {
				return nil, err
			}
		}
		if page.ReadVersion, err = rc.GetReadVersion(); err != nil {
			return nil, err
		}
		if plan.window.empty {
			return nil, nil
		}
		return nil, b.sameSpanPage(rc.ReadTransaction(false), pm, plan, resume, budget, &page)
	})
	page.Candidates, page.Bytes = budget.candidates, budget.bytes
	b.lastWork = QueryResult{Candidates: budget.candidates, Bytes: budget.bytes}
	if err != nil || len(page.Spans) < plan.limit {
		return page, err
	}
	if len(secret) == 0 {
		if secret, err = b.ensureContinuationSecret(ctx); err != nil {
			return page, err
		}
	}
	last := page.Spans[len(page.Spans)-1]
	key := postingSuffix(Span{Trace: last.Trace, ID: last.ID, Start: last.Start}, b.cfg.Order).Pack()
	page.Continuation = signContinuation(secret, b.cfg.Run, b.cfg.Tenant, b.cfg.Order, plan.digest, key)
	return page, nil
}

func (b *FDBBackend) sameSpanPage(tx fdb.ReadTransaction, pm *postingMaintainer, plan sameSpanPlan, resume tuple.Tuple, budget *queryBudget, page *SpanQueryPage) error {
	tenant := b.cfg.Tenant
	driver := plan.preds[0]
	others := plan.preds[1:]
	driverTerm := termPrefix(pm.secondary, tenant, driver, pm.width)
	otherTerms := make([]subspace.Subspace, len(others))
	for i, a := range others {
		otherTerms[i] = termPrefix(pm.secondary, tenant, a, pm.width)
	}
	rng, nonempty, err := spanTermRange(driverTerm, pm.order, plan.window, resume)
	if err != nil || !nonempty {
		return err
	}
	var batch []SpanMatch
	// flush probes every candidate's other terms in one pipelined round, then
	// accepts candidates in scan order; it reports whether the page is full.
	flush := func() (bool, error) {
		probes := make([][]fdb.RangeResult, len(batch))
		for i, c := range batch {
			suffix := postingSuffix(Span{Trace: c.Trace, ID: c.ID, Start: c.Start}, pm.order)
			probes[i] = make([]fdb.RangeResult, len(others))
			for j, term := range otherTerms {
				r, err := fdb.PrefixRange(term.Pack(suffix))
				if err != nil {
					return false, err
				}
				probes[i][j] = tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll})
			}
		}
		full := false
		for i, c := range batch {
			accepted := true
			for j, term := range otherTerms {
				rows, err := probes[i][j].GetSliceWithError()
				if err != nil {
					return false, err
				}
				found := false
				for _, kv := range rows {
					if err := budget.charge(kv); err != nil {
						return false, err
					}
					m, a, err := decodeSpanPosting(pm, term, kv)
					if err != nil {
						return false, err
					}
					if m.Trace != c.Trace || m.ID != c.ID {
						return false, fail("corruption", "same-span probe", "probe returned another span identity")
					}
					if a == others[j] {
						if m.Start != c.Start {
							return false, fail("corruption", "same-span probe", "covering start disagrees across postings of one span")
						}
						found = true
					}
				}
				accepted = accepted && found
			}
			if accepted && !full {
				page.Spans = append(page.Spans, c)
				full = len(page.Spans) == plan.limit
			}
		}
		batch = batch[:0]
		return full, nil
	}
	it := tx.GetRange(rng, fdb.RangeOptions{Mode: fdb.StreamingModeIterator}).Iterator()
	last := ""
	for it.Advance() {
		kv, err := it.Get()
		if err != nil {
			return err
		}
		if err := budget.charge(kv); err != nil {
			return err
		}
		m, a, err := decodeSpanPosting(pm, driverTerm, kv)
		if err != nil {
			return err
		}
		if a != driver || !plan.window.contains(m.Start) || m.Trace+m.ID == last {
			continue
		}
		last = m.Trace + m.ID
		batch = append(batch, m)
		if len(batch) >= min(sameSpanProbeBatch, plan.limit-len(page.Spans)) {
			full, err := flush()
			if err != nil || full {
				return err
			}
		}
	}
	if _, err := it.Get(); err != nil {
		return err
	}
	if len(batch) > 0 {
		_, err := flush()
		return err
	}
	return nil
}

// scanSpanWitnesses visits every verified span posting of one term inside the window.
func scanSpanWitnesses(tx fdb.ReadTransaction, pm *postingMaintainer, tenant int, a Attribute, w timeWindow, budget *queryBudget, visit func(SpanMatch)) error {
	term := termPrefix(pm.secondary, tenant, a, pm.width)
	rng, nonempty, err := spanTermRange(term, pm.order, w, nil)
	if err != nil || !nonempty {
		return err
	}
	it := tx.GetRange(rng, fdb.RangeOptions{Mode: fdb.StreamingModeIterator}).Iterator()
	for it.Advance() {
		kv, err := it.Get()
		if err != nil {
			return err
		}
		if err := budget.charge(kv); err != nil {
			return err
		}
		m, got, err := decodeSpanPosting(pm, term, kv)
		if err != nil {
			return err
		}
		if got == a && w.contains(m.Start) {
			visit(m)
		}
	}
	_, err = it.Get()
	return err
}

type traceWitnesses struct {
	spans [][]SpanMatch
	logs  [][]LogMatch
	msgs  [][]MessageSpanHit
}

// QueryTraceExistential unions each clause's witnesses over the complete term
// before joining traces, so witnesses may be different spans or logs.
func (b *FDBBackend) QueryTraceExistential(ctx context.Context, q TraceExistentialQuery) (TraceExistentialResult, error) {
	const op = "trace existential"
	b.lastWork = QueryResult{}
	out := TraceExistentialResult{Hits: []TraceExistentialHit{}}
	n := len(q.Clauses) + len(q.LogClauses) + len(q.MessageClauses)
	if n == 0 || n > maxQueryPredicates {
		return out, failf("config", op, "expected 1..%d clauses", maxQueryPredicates)
	}
	for _, a := range q.Clauses {
		if err := validateQueryAttribute(op, a, "resource", "scope", "span"); err != nil {
			return out, err
		}
		if err := checkMessageAttributeQuery(op, a, b.cfg.Messages != messagesNone); err != nil {
			return out, err
		}
	}
	for _, a := range q.LogClauses {
		if err := validateQueryAttribute(op, a, logAttributeScopes...); err != nil {
			return out, err
		}
	}
	msgPlans := make([]*messageQueryPlan, len(q.MessageClauses))
	for i, mq := range q.MessageClauses {
		if b.cfg.Messages != messagesFullText {
			return out, fail("config", op, "message clauses need the fulltext message layout")
		}
		if mq.Limit != 0 || len(mq.Continuation) > 0 {
			return out, fail("config", op, "message clauses take no limit or continuation")
		}
		p, err := newMessageQueryPlan(op, mq, 0)
		if err != nil {
			return out, err
		}
		msgPlans[i] = p
	}
	if q.Limit < 0 {
		return out, fail("config", op, "limit must not be negative")
	}
	limit := q.Limit
	if limit == 0 {
		limit = b.cfg.K
	}
	w, err := newTimeWindow(q.TimeWindow)
	if err != nil {
		return out, err
	}
	budget := b.newBudget(op)
	_, err = b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		out = TraceExistentialResult{Hits: []TraceExistentialHit{}}
		budget = b.newBudget(op)
		store, pm, err := b.openPostingsStore(rc, op)
		if err != nil {
			return nil, err
		}
		if out.ReadVersion, err = rc.GetReadVersion(); err != nil {
			return nil, err
		}
		if w.empty {
			return nil, nil
		}
		tx := rc.ReadTransaction(false)
		var joined map[string]*traceWitnesses // nil until the first clause is unioned
		join := func(found map[string]*traceWitnesses) bool {
			if joined == nil {
				joined = found
			} else {
				for tr, tw := range joined {
					f := found[tr]
					if f == nil {
						delete(joined, tr)
						continue
					}
					for i, s := range f.spans {
						if s != nil {
							tw.spans[i] = s
						}
					}
					for i, l := range f.logs {
						if l != nil {
							tw.logs[i] = l
						}
					}
					for i, m := range f.msgs {
						if m != nil {
							tw.msgs[i] = m
						}
					}
				}
			}
			return len(joined) > 0
		}
		entry := func(found map[string]*traceWitnesses, trace string) *traceWitnesses {
			if joined != nil && joined[trace] == nil {
				return nil
			}
			tw := found[trace]
			if tw == nil {
				tw = &traceWitnesses{spans: make([][]SpanMatch, len(q.Clauses)), logs: make([][]LogMatch, len(q.LogClauses)), msgs: make([][]MessageSpanHit, len(q.MessageClauses))}
				found[trace] = tw
			}
			return tw
		}
		for i, a := range q.Clauses {
			found := map[string]*traceWitnesses{}
			seen := map[string]bool{}
			err := scanSpanWitnesses(tx, pm, b.cfg.Tenant, a, w, budget, func(m SpanMatch) {
				if seen[m.Trace+m.ID] {
					return
				}
				seen[m.Trace+m.ID] = true
				if tw := entry(found, m.Trace); tw != nil {
					tw.spans[i] = append(tw.spans[i], m)
				}
			})
			if err != nil {
				return nil, err
			}
			if !join(found) {
				return nil, nil
			}
		}
		for i, a := range q.LogClauses {
			found := map[string]*traceWitnesses{}
			seen := map[string]bool{}
			err := scanLogTerm(tx, b.keys, b.cfg.Tenant, b.cfg.HashBytes, a, w, budget, func(m LogMatch) (bool, error) {
				if m.Trace == "" || seen[m.EventID] {
					return false, nil
				}
				seen[m.EventID] = true
				if tw := entry(found, m.Trace); tw != nil {
					tw.logs[i] = append(tw.logs[i], m)
				}
				return false, nil
			})
			if err != nil {
				return nil, err
			}
			if !join(found) {
				return nil, nil
			}
		}
		for i, p := range msgPlans {
			found := map[string]*traceWitnesses{}
			err := b.runMessageQuery(ctx, store, pm, p, nil, w, 0, budget, func(h MessageSpanHit, _ []byte) {
				if tw := entry(found, h.Trace); tw != nil {
					tw.msgs[i] = append(tw.msgs[i], h)
				}
			})
			if err != nil {
				return nil, err
			}
			if !join(found) {
				return nil, nil
			}
		}
		for tr, tw := range joined {
			hit := TraceExistentialHit{Trace: tr, Latest: -1, Witnesses: tw.spans}
			for _, ws := range tw.spans {
				sort.Slice(ws, func(x, y int) bool {
					if ws[x].Start != ws[y].Start {
						return ws[x].Start > ws[y].Start
					}
					return ws[x].ID < ws[y].ID
				})
				hit.Latest = max(hit.Latest, ws[0].Start)
			}
			if len(tw.logs) > 0 {
				hit.LogWitnesses = tw.logs
			}
			for _, ls := range tw.logs {
				sort.Slice(ls, func(x, y int) bool {
					if ls[x].Time != ls[y].Time {
						return ls[x].Time > ls[y].Time
					}
					return ls[x].EventID < ls[y].EventID
				})
				hit.Latest = max(hit.Latest, ls[0].Time)
			}
			if len(tw.msgs) > 0 {
				hit.MessageWitnesses = tw.msgs
			}
			for _, ms := range tw.msgs {
				sort.Slice(ms, func(x, y int) bool {
					a, c := ms[x], ms[y]
					if a.Start != c.Start {
						return a.Start > c.Start
					}
					if a.Span != c.Span {
						return a.Span < c.Span
					}
					if a.AttributeKey != c.AttributeKey {
						return a.AttributeKey < c.AttributeKey
					}
					return a.MessageID < c.MessageID
				})
				hit.Latest = max(hit.Latest, ms[0].Start)
			}
			out.Hits = append(out.Hits, hit)
		}
		sort.Slice(out.Hits, func(x, y int) bool {
			if out.Hits[x].Latest != out.Hits[y].Latest {
				return out.Hits[x].Latest > out.Hits[y].Latest
			}
			return out.Hits[x].Trace > out.Hits[y].Trace
		})
		if len(out.Hits) > limit {
			out.Hits = out.Hits[:limit]
		}
		return nil, nil
	})
	out.Candidates, out.Bytes = budget.candidates, budget.bytes
	b.lastWork = QueryResult{Candidates: budget.candidates, Bytes: budget.bytes}
	return out, err
}

// TraverseTraceGraph loads every stored span of the trace in one bounded
// transaction and builds its observed parent adjacency and links.
func (b *FDBBackend) TraverseTraceGraph(ctx context.Context, traceID string) (TraceGraph, error) {
	const op = "trace graph"
	b.lastWork = QueryResult{}
	if !validIdentity(traceID, 16) {
		return TraceGraph{}, fail("config", op, "expected lowercase hex nonzero 16-byte trace identifier")
	}
	var spans []graphSpan
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		spans = nil
		b.lastWork = QueryResult{}
		if err := b.readManifest(rc, true); err != nil {
			return nil, err
		}
		store, err := b.open(rc)
		if err != nil {
			return nil, err
		}
		props := recordlayer.ForwardScan()
		props.ExecuteProperties.ReturnedRowLimit = b.cfg.MaxCandidates + 1
		props.ExecuteProperties.ScannedBytesLimit = b.cfg.MaxBytes
		prefix := tuple.Tuple{int64(b.cfg.Tenant), traceID}
		cur := store.ScanRecordsInRange(prefix, prefix, recordlayer.EndpointTypeRangeInclusive, recordlayer.EndpointTypeRangeInclusive, nil, props)
		defer cur.Close()
		for {
			r, err := cur.OnNext(ctx)
			if err != nil {
				return nil, err
			}
			if !r.HasNext() {
				if r.GetNoNextReason() != recordlayer.SourceExhausted {
					return nil, failf("budget", op, "trace scan stopped: %v", r.GetNoNextReason())
				}
				return nil, nil
			}
			record := r.GetValue()
			b.lastWork.Candidates++
			b.lastWork.Bytes += int64(record.KeySize + record.ValueSize)
			if b.lastWork.Candidates > b.cfg.MaxCandidates || b.lastWork.Bytes > b.cfg.MaxBytes {
				return nil, failf("budget", op, "candidates=%d logical_bytes=%d", b.lastWork.Candidates, b.lastWork.Bytes)
			}
			s, err := decodeRecord(record.Record)
			if err != nil {
				return nil, err
			}
			if s.Trace != traceID || s.Tenant != b.cfg.Tenant {
				return nil, fail("corruption", op, "record outside the requested trace")
			}
			msg := record.Record.ProtoReflect()
			rs, err := restoreEnvelopeOTLP(msg)
			if err != nil {
				return nil, err
			}
			spans = append(spans, graphSpan{SpanMatch{Trace: s.Trace, ID: s.ID, Start: s.Start}, rs.ScopeSpans[0].Spans[0]})
		}
	})
	if err != nil {
		return TraceGraph{}, err
	}
	return buildTraceGraph(traceID, spans), nil
}

// QuerySameSpan returns one verified snapshot_page of same-span matches.
func (e *Exporter) QuerySameSpan(ctx context.Context, q SameSpanQuery) (SpanQueryPage, error) {
	if err := e.begin("same-span query"); err != nil {
		return SpanQueryPage{Spans: []SpanMatch{}, Order: e.cfg.Order}, err
	}
	defer e.active.Done()
	e.queryMu.Lock()
	defer e.queryMu.Unlock()
	return e.backend.QuerySameSpan(e.txnContext(ctx), q)
}

// QueryTraceExistential returns the newest traces satisfying every clause, with witnesses.
func (e *Exporter) QueryTraceExistential(ctx context.Context, q TraceExistentialQuery) (TraceExistentialResult, error) {
	if len(q.MessageClauses) > 0 && !e.cfg.EnableMessageFullText {
		return TraceExistentialResult{Hits: []TraceExistentialHit{}}, fail("config", "trace existential", "message clauses need enable_message_full_text")
	}
	if err := e.begin("trace existential"); err != nil {
		return TraceExistentialResult{Hits: []TraceExistentialHit{}}, err
	}
	defer e.active.Done()
	e.queryMu.Lock()
	defer e.queryMu.Unlock()
	return e.backend.QueryTraceExistential(e.txnContext(ctx), q)
}

// TraverseTraceGraph returns the observed parent/child/link topology of one trace.
func (e *Exporter) TraverseTraceGraph(ctx context.Context, traceID string) (TraceGraph, error) {
	if err := e.begin("trace graph"); err != nil {
		return TraceGraph{}, err
	}
	defer e.active.Done()
	e.queryMu.Lock()
	defer e.queryMu.Unlock()
	return e.backend.TraverseTraceGraph(e.txnContext(ctx), traceID)
}

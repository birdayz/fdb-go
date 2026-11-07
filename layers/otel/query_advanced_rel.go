package otel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"sort"
	"strconv"
	"sync"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Advanced span queries on a tenant store (rfcs/relational-exporter-migration.md
// §1.4, §3.2): same-span conjunctions paged by v2 continuation tokens, the
// trace existential join and the trace graph. Each is one transaction of
// budgeted direct reads of the standard OTEL_V1 span indexes.

// ---------------------------------------------------------------------------
// Continuation tokens v2
// ---------------------------------------------------------------------------

const (
	relContinuationVersion = int64(2)
	relContinuationDomain  = "otel/continuation/v2\x00"
)

// relContinuation is a parsed, not yet authenticated, v2 token:
// tuple(2, kind, database_path, schema, digest, last) and its MAC.
type relContinuation struct {
	databasePath, schema string
	digest, last         []byte
	payload, mac         []byte
}

// signRelContinuation signs a token of one query family of a tenant store;
// the kind is authenticated, so families never accept each other's tokens.
func signRelContinuation(secret []byte, kind, databasePath, schema string, digest, last []byte) []byte {
	payload := tuple.Tuple{relContinuationVersion, kind, databasePath, schema, digest, last}.Pack()
	return append(payload, continuationMACIn(relContinuationDomain, secret, payload)...)
}

// parseRelContinuation parses a v2 token of kind. Any other version (v1
// tokens included), another kind or another shape is config.
func parseRelContinuation(token []byte, kind string) (*relContinuation, error) {
	const op = "continuation"
	fields, payload, mac, err := splitContinuation(token)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, fail("config", op, "malformed continuation token fields")
	}
	if version, ok := fields[0].(int64); !ok || version != relContinuationVersion {
		return nil, fail("config", op, "unsupported continuation token")
	}
	if len(fields) != 6 {
		return nil, fail("config", op, "malformed continuation token fields")
	}
	c := &relContinuation{payload: payload, mac: mac}
	got, kok := fields[1].(string)
	var pok, sok, dok, lok bool
	c.databasePath, pok = fields[2].(string)
	c.schema, sok = fields[3].(string)
	c.digest, dok = fields[4].([]byte)
	c.last, lok = fields[5].([]byte)
	if !kok || !pok || !sok || !dok || !lok || got != kind {
		return nil, fail("config", op, "unsupported continuation token")
	}
	return c, nil
}

// authenticate checks the MAC under the store's key, then that the token
// belongs to this store and query.
func (c *relContinuation) authenticate(secret []byte, s *tenantStore, digest []byte) error {
	const op = "continuation"
	if err := checkContinuationMAC(relContinuationDomain, "store", secret, c.payload, c.mac); err != nil {
		return err
	}
	switch {
	case c.databasePath != s.databasePath || c.schema != s.schema:
		return fail("config", op, "continuation belongs to another store")
	case !bytes.Equal(c.digest, digest):
		return fail("config", op, "continuation belongs to another query")
	}
	return nil
}

// spanSortKey is a same-span page's last emitted span, the v2 sort key
// (start_ns, trace_id, span_id).
type spanSortKey struct {
	start       int64
	trace, span []byte
}

func (k spanSortKey) pack() []byte { return tuple.Tuple{k.start, k.trace, k.span}.Pack() }

// parseSpanSortKey validates an authenticated token's sort key.
func parseSpanSortKey(raw []byte) (spanSortKey, error) {
	bad := fail("corruption", "continuation", "malformed continuation sort key")
	t, err := tuple.Unpack(raw)
	if err != nil || len(t) != 3 {
		return spanSortKey{}, bad
	}
	start, sok := t[0].(int64)
	trace, tok := t[1].([]byte)
	span, pok := t[2].([]byte)
	if !sok || !tok || !pok || start < 0 || len(trace) != 16 || zeroID(trace) || len(span) != 8 || zeroID(span) {
		return spanSortKey{}, bad
	}
	return spanSortKey{start, trace, span}, nil
}

// continuationKeys holds a tenant store's continuation signing key: 32
// bytes, published once and never replaced.
type continuationKeys interface {
	// load reads the published key in the page's transaction; nil when none is.
	load(store *recordlayer.FDBRecordStore) ([]byte, error)
	// publish stores candidate unless a key exists, in its own transaction,
	// and returns the key that won.
	publish(ctx context.Context, s *tenantStore, candidate []byte) ([]byte, error)
}

// memoryContinuationKeys keeps the key in memory: for tests, which share one
// value among the readers of one store as STORE_META would.
type memoryContinuationKeys struct {
	mu  sync.Mutex
	key []byte
}

func (m *memoryContinuationKeys) load(*recordlayer.FDBRecordStore) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.key, nil
}

func (m *memoryContinuationKeys) publish(_ context.Context, _ *tenantStore, candidate []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.key == nil {
		m.key = candidate
	}
	return m.key, nil
}

// loadContinuationKey reads the store's published key, if any.
func (r *SpanReader) loadContinuationKey(store *recordlayer.FDBRecordStore) ([]byte, error) {
	key, err := r.keys.load(store)
	if err != nil {
		return nil, err
	}
	if len(key) != 0 && len(key) != continuationKeyBytes {
		return nil, fail("corruption", "continuation", "malformed continuation signing key")
	}
	return key, nil
}

// ensureContinuationKey publishes the store's signing key on first use; the
// candidate is drawn outside the retry loop and loses to a concurrent winner.
func (r *SpanReader) ensureContinuationKey(ctx context.Context) ([]byte, error) {
	fresh := make([]byte, continuationKeyBytes)
	if _, err := rand.Read(fresh); err != nil {
		return nil, wrap("operation", "continuation key", err)
	}
	key, err := r.keys.publish(ctx, r.store, fresh)
	if err != nil {
		return nil, err
	}
	if len(key) != continuationKeyBytes {
		return nil, fail("corruption", "continuation", "malformed continuation signing key")
	}
	return key, nil
}

// ---------------------------------------------------------------------------
// Typed predicates
// ---------------------------------------------------------------------------

// validateRelQueryAttribute checks an equality predicate for typed equality
// (D1): string, int, double and bool values, each in its canonical text.
func validateRelQueryAttribute(op string, a Attribute, scopes ...string) error {
	if !slices.Contains(scopes, a.Scope) {
		return failf("config", op, "unsupported attribute scope %q", a.Scope)
	}
	switch a.Type {
	case "string":
	case "int":
		n, err := strconv.ParseInt(a.Value, 10, 64)
		if err != nil || strconv.FormatInt(n, 10) != a.Value {
			return fail("config", op, "int attribute value must be a canonical base-10 int64")
		}
	case "double":
		f, err := parseCanonicalDouble(a.Value)
		if err != nil || otelsql.FormatDouble(f) != a.Value {
			return fail("config", op, "double attribute value must be canonical: shortest round-trip text, NaN, +Inf or -Inf")
		}
	case "bool":
		if a.Value != "true" && a.Value != "false" {
			return fail("config", op, "bool attribute value must be true or false")
		}
	default:
		return failf("config", op, "unsupported attribute type %q; indexed types are string, int, double and bool", a.Type)
	}
	// Larger keys and values are never indexed; an empty answer would not be exhaustive.
	if len(a.Key) > maxIndexedKeyBytes || len(a.Value) > maxIndexedValueBytes {
		return fail("config", op, "attribute exceeds the indexed key/value cap")
	}
	return nil
}

// relAttr is a validated predicate and the index entries that equal it:
// those under prefix (scope, key, value or long-string digest).
type relAttr struct {
	a      Attribute
	q      attrQuery
	prefix tuple.Tuple
}

func newRelAttr(op string, a Attribute, scopes ...string) (relAttr, error) {
	if err := validateRelQueryAttribute(op, a, scopes...); err != nil {
		return relAttr{}, err
	}
	q, ok := spanAttrQuery(a)
	if !ok {
		return relAttr{}, failf("config", op, "attribute %s %q has no index", a.Type, a.Key)
	}
	return relAttr{a: a, q: q, prefix: tuple.Tuple{a.Scope, a.Key, q.value}}, nil
}

// spanAttrRange is the reverse-scan range of a's entries: those starting in
// w ([lo, hi) on start_ns), below the resumed sort key when there is one. It
// reports false when nothing can remain.
func spanAttrRange(prefix tuple.Tuple, w timeWindow, resume *spanSortKey) (recordlayer.TupleRange, bool) {
	at := func(t ...tuple.TupleElement) tuple.Tuple { return append(slices.Clip(prefix), t...) }
	rng := recordlayer.TupleRangeAllOf(prefix)
	if w.set {
		rng = recordlayer.TupleRangeBetween(at(w.lo), at(w.hi))
	}
	if resume != nil {
		if w.set && resume.start < w.lo {
			return rng, false
		}
		if !w.set || resume.start < w.hi {
			// Exclusive: the resumed span's entry and every key below it in
			// tuple order is pack(prefix, start, trace, span) or above.
			rng.High, rng.HighEndpoint = at(resume.start, resume.trace, resume.span), recordlayer.EndpointTypeRangeExclusive
		}
	}
	return rng, true
}

// spanEntryID reads the (start, trace, span) columns of a span attribute
// index entry: (scope, key, value, start_ns, trace_id, span_id).
func spanEntryID(op string, e *recordlayer.IndexEntry) (spanSortKey, error) {
	start, err := entryInt64(op, e, 3)
	if err != nil {
		return spanSortKey{}, err
	}
	trace, err := entryBytes(op, e, 4)
	if err != nil {
		return spanSortKey{}, err
	}
	span, err := entryBytes(op, e, 5)
	if err != nil {
		return spanSortKey{}, err
	}
	if len(trace) != 16 || len(span) != 8 {
		return spanSortKey{}, failf("corruption", op, "%s entry is not a span identity", e.Index.Name)
	}
	return spanSortKey{start, trace, span}, nil
}

func (k spanSortKey) match() SpanMatch {
	return SpanMatch{Trace: hex.EncodeToString(k.trace), ID: hex.EncodeToString(k.span), Start: k.start}
}

// holdLongStrings loads the spans of pks, keeping loads in flight, and
// reports per span whether it holds every long-string attribute of attrs
// verbatim: their index entries carry a digest, which may collide.
func holdLongStrings(store *recordlayer.FDBRecordStore, op string, attrs []relAttr, pks []tuple.Tuple, work *queryWork) ([]bool, error) {
	out := make([]bool, len(pks))
	err := loadRecords(store, op, pks, work, func(i int, rec *recordlayer.FDBStoredRecord[proto.Message]) error {
		if rec == nil {
			return failf("corruption", op, "a span index entry has no %s record", spansRecordType)
		}
		m := rec.Record.ProtoReflect()
		fd := m.Descriptor().Fields().ByName("LONG_STR_ATTRS")
		if fd == nil {
			return failf("ownership", op, "%s has no LONG_STR_ATTRS", spansRecordType)
		}
		held := m.Get(fd).Message()
		out[i] = true
		for _, a := range attrs {
			if a.q.long != nil && !longStringsHold(held, a.a.Scope, a.a.Key, *a.q.long) {
				out[i] = false
				break
			}
		}
		return nil
	})
	return out, err
}

func anyLong(attrs []relAttr) bool {
	return slices.ContainsFunc(attrs, func(a relAttr) bool { return a.q.long != nil })
}

// ---------------------------------------------------------------------------
// Same-span queries
// ---------------------------------------------------------------------------

type relSameSpanPlan struct {
	attrs  []relAttr
	window timeWindow
	limit  int
	digest []byte
}

func (r *SpanReader) newSameSpanPlan(op string, q SameSpanQuery) (relSameSpanPlan, error) {
	preds := uniqueAttributes(q.Predicates)
	if len(preds) == 0 || len(q.Predicates) > maxQueryPredicates {
		return relSameSpanPlan{}, failf("config", op, "expected 1..%d predicates", maxQueryPredicates)
	}
	p := relSameSpanPlan{attrs: make([]relAttr, len(preds))}
	for i, a := range preds {
		var err error
		if p.attrs[i], err = newRelAttr(op, a, "resource", "scope", "span"); err != nil {
			return relSameSpanPlan{}, err
		}
		if err := checkMessageAttributeQuery(op, a, r.cfg.Messages != messagesNone); err != nil {
			return relSameSpanPlan{}, err
		}
	}
	if q.Limit < 0 {
		return relSameSpanPlan{}, fail("config", op, "limit must not be negative")
	}
	var err error
	if p.window, err = newTimeWindow(q.TimeWindow); err != nil {
		return relSameSpanPlan{}, err
	}
	p.limit = q.Limit
	if p.limit == 0 {
		p.limit = r.cfg.K
	}
	// The digest binds the predicate set and window, not the driver order.
	p.digest = queryDigest(continuationKind, preds, p.window)
	return p, nil
}

// QuerySameSpan returns one snapshot page of the spans matching every
// predicate, in index order: start desc, trace desc, span desc (D3). A full
// page carries a signed continuation; the next page may be empty.
// Predicates[0] drives the scan; put the most selective first.
func (r *SpanReader) QuerySameSpan(ctx context.Context, q SameSpanQuery) (SpanQueryPage, error) {
	const op = "same-span query"
	empty := func() SpanQueryPage { return SpanQueryPage{Spans: []SpanMatch{}} }
	page := empty()
	plan, err := r.newSameSpanPlan(op, q)
	if err != nil {
		return page, err
	}
	var token *relContinuation
	if len(q.Continuation) > 0 {
		if token, err = parseRelContinuation(q.Continuation, continuationKind); err != nil {
			return page, err
		}
	}
	var secret []byte
	var last spanSortKey
	work := newQueryWork(r.cfg)
	err = r.store.read(ctx, op, func(store *recordlayer.FDBRecordStore) error {
		page, work, secret = empty(), newQueryWork(r.cfg), nil
		var resume *spanSortKey
		var err error
		if token != nil {
			if secret, err = r.loadContinuationKey(store); err != nil {
				return err
			}
			if err := token.authenticate(secret, r.store, plan.digest); err != nil {
				return err
			}
			k, err := parseSpanSortKey(token.last)
			if err != nil {
				return err
			}
			resume = &k
		}
		if page.ReadVersion, err = store.Context().GetReadVersion(); err != nil {
			return err
		}
		if plan.window.empty {
			return nil
		}
		if err := r.sameSpanPage(ctx, store, plan, resume, work, &page, &last); err != nil {
			return err
		}
		if len(page.Spans) == plan.limit && secret == nil {
			secret, err = r.loadContinuationKey(store)
		}
		return err
	})
	page.Candidates, page.Bytes = work.candidates, work.bytes
	if err != nil || len(page.Spans) < plan.limit {
		return page, err
	}
	if len(secret) == 0 {
		if secret, err = r.ensureContinuationKey(ctx); err != nil {
			return page, err
		}
	}
	page.Continuation = signRelContinuation(secret, continuationKind, r.store.databasePath, r.store.schema, plan.digest, last.pack())
	return page, nil
}

// sameSpanCandidate is a verified driver entry awaiting its probes.
type sameSpanCandidate struct {
	key spanSortKey
	pk  tuple.Tuple
}

// sameSpanPage reverse-scans the driver's entries and probes each batch of
// candidates for the other predicates in one pipelined round of prefix
// reads, accepting candidates in scan order until the page is full.
func (r *SpanReader) sameSpanPage(ctx context.Context, store *recordlayer.FDBRecordStore, plan relSameSpanPlan, resume *spanSortKey, work *queryWork, page *SpanQueryPage, last *spanSortKey) error {
	const op = "same-span query"
	driver, others := plan.attrs[0], plan.attrs[1:]
	spaces := make([]subspace.Subspace, len(others))
	for j, a := range others {
		idx, err := readableIndex(store, op, a.q.index)
		if err != nil {
			return err
		}
		spaces[j] = store.IndexSubspace(idx)
	}
	rng, ok := spanAttrRange(driver.prefix, plan.window, resume)
	if !ok {
		return nil
	}
	long := anyLong(plan.attrs)
	tx := store.Context().ReadTransaction(false)
	batch := make([]sameSpanCandidate, 0, min(sameSpanProbeBatch, plan.limit))
	probes := make([]fdb.RangeResult, 0, cap(batch)*len(others))
	// flush probes every candidate for every other predicate, verifies long
	// strings on the survivors' records, and reports whether the page is full.
	flush := func() (bool, error) {
		probes = probes[:0]
		for _, c := range batch {
			for j, a := range others {
				kr, err := fdb.PrefixRange(spaces[j].Pack(append(slices.Clip(a.prefix), c.key.start, c.key.trace, c.key.span)))
				if err != nil {
					return false, err
				}
				probes = append(probes, tx.GetRange(kr, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}))
			}
		}
		accepted := make([]bool, len(batch))
		var verify []int
		var pks []tuple.Tuple
		for i, c := range batch {
			accepted[i] = true
			for j := range others {
				rows, err := probes[i*len(others)+j].GetSliceWithError()
				if err != nil {
					return false, err
				}
				for _, kv := range rows {
					if err := work.charge(op, int64(len(kv.Key)+len(kv.Value))); err != nil {
						return false, err
					}
					if _, err := spaces[j].Unpack(kv.Key); err != nil {
						return false, wrap("corruption", op, err)
					}
				}
				accepted[i] = accepted[i] && len(rows) > 0
			}
			if accepted[i] && long {
				verify = append(verify, i)
				pks = append(pks, c.pk)
			}
		}
		if len(verify) > 0 {
			held, err := holdLongStrings(store, op, plan.attrs, pks, work)
			if err != nil {
				return false, err
			}
			for k, i := range verify {
				accepted[i] = held[k]
			}
		}
		full := false
		for i, c := range batch {
			if accepted[i] && !full {
				page.Spans = append(page.Spans, c.key.match())
				*last = c.key
				full = len(page.Spans) == plan.limit
			}
		}
		batch = batch[:0]
		return full, nil
	}
	var prev spanSortKey
	err := scanIndex(ctx, store, op, driver.q.index, rng, true, work, func(e *recordlayer.IndexEntry) (bool, error) {
		k, err := spanEntryID(op, e)
		if err != nil {
			return false, err
		}
		if !plan.window.contains(k.start) || (bytes.Equal(k.trace, prev.trace) && bytes.Equal(k.span, prev.span)) {
			return false, nil
		}
		prev = k
		batch = append(batch, sameSpanCandidate{key: k, pk: e.PrimaryKey()})
		if len(batch) < min(sameSpanProbeBatch, plan.limit-len(page.Spans)) {
			return false, nil
		}
		return flush()
	})
	if err != nil || len(batch) == 0 {
		return err
	}
	_, err = flush()
	return err
}

// ---------------------------------------------------------------------------
// Trace existential
// ---------------------------------------------------------------------------

// witnessScanner scans one existential clause: it visits every witness of
// the clause inside w once, with the hex trace the witness belongs to. Span
// clauses are spanClause; logClauses and messageClauses plan the log and
// message clauses' scanners.
type witnessScanner[W any] interface {
	scanWitnesses(ctx context.Context, store *recordlayer.FDBRecordStore, w timeWindow, work *queryWork, visit func(trace string, witness W)) error
}

// spanClause scans the spans carrying one typed attribute, newest first.
type spanClause struct{ attr relAttr }

func (c spanClause) scanWitnesses(ctx context.Context, store *recordlayer.FDBRecordStore, w timeWindow, work *queryWork, visit func(string, SpanMatch)) error {
	const op = "trace existential"
	rng, _ := spanAttrRange(c.attr.prefix, w, nil)
	var pending []spanSortKey
	var pks []tuple.Tuple
	// flush verifies a batch of long-string candidates on their records.
	flush := func() error {
		held, err := holdLongStrings(store, op, []relAttr{c.attr}, pks, work)
		if err != nil {
			return err
		}
		for i, k := range pending {
			if held[i] {
				m := k.match()
				visit(m.Trace, m)
			}
		}
		pending, pks = pending[:0], pks[:0]
		return nil
	}
	var prev spanSortKey
	err := scanIndex(ctx, store, op, c.attr.q.index, rng, true, work, func(e *recordlayer.IndexEntry) (bool, error) {
		k, err := spanEntryID(op, e)
		if err != nil {
			return false, err
		}
		if !w.contains(k.start) || (bytes.Equal(k.trace, prev.trace) && bytes.Equal(k.span, prev.span)) {
			return false, nil
		}
		prev = k
		if c.attr.q.long == nil {
			m := k.match()
			visit(m.Trace, m)
			return false, nil
		}
		pending, pks = append(pending, k), append(pks, e.PrimaryKey())
		if len(pending) < loadRecordsBatch {
			return false, nil
		}
		return false, flush()
	})
	if err != nil || len(pending) == 0 {
		return err
	}
	return flush()
}

// relWitnesses are one trace's witnesses, per clause of each kind.
type relWitnesses struct {
	spans [][]SpanMatch
	logs  [][]LogMatch
	msgs  [][]MessageSpanHit
}

// traceJoin intersects clauses on trace: joined is nil until the first clause
// is unioned, then holds the traces every clause so far witnessed.
type traceJoin struct {
	spans, logs, msgs int
	joined            map[string]*relWitnesses
}

// entry is trace's witnesses in found, or nil when an earlier clause
// already excluded the trace.
func (j *traceJoin) entry(found map[string]*relWitnesses, trace string) *relWitnesses {
	if j.joined != nil && j.joined[trace] == nil {
		return nil
	}
	tw := found[trace]
	if tw == nil {
		tw = &relWitnesses{spans: make([][]SpanMatch, j.spans), logs: make([][]LogMatch, j.logs), msgs: make([][]MessageSpanHit, j.msgs)}
		found[trace] = tw
	}
	return tw
}

// join keeps the joined traces found also witnessed, with their new
// witnesses, and reports whether any remain.
func (j *traceJoin) join(found map[string]*relWitnesses) bool {
	if j.joined == nil {
		j.joined = found
		return len(found) > 0
	}
	for tr, tw := range j.joined {
		f := found[tr]
		if f == nil {
			delete(j.joined, tr)
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
	return len(j.joined) > 0
}

// joinClauses unions each clause's complete term inside w, then joins it on
// trace; it reports false once no trace satisfies every clause so far.
func joinClauses[W any](ctx context.Context, store *recordlayer.FDBRecordStore, clauses []witnessScanner[W], w timeWindow, work *queryWork, j *traceJoin, slot func(*relWitnesses) [][]W) (bool, error) {
	for i, c := range clauses {
		found := map[string]*relWitnesses{}
		err := c.scanWitnesses(ctx, store, w, work, func(trace string, witness W) {
			if tw := j.entry(found, trace); tw != nil {
				s := slot(tw)
				s[i] = append(s[i], witness)
			}
		})
		if err != nil {
			return false, err
		}
		if !j.join(found) {
			return false, nil
		}
	}
	return true, nil
}

// hits orders each clause's witnesses newest first and the traces by Latest,
// the newest witness of any clause, desc, then trace desc, cut at limit.
func (j *traceJoin) hits(limit int) []TraceExistentialHit {
	out := []TraceExistentialHit{}
	for tr, tw := range j.joined {
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
		out = append(out, hit)
	}
	sort.Slice(out, func(x, y int) bool {
		if out[x].Latest != out[y].Latest {
			return out[x].Latest > out[y].Latest
		}
		return out[x].Trace > out[y].Trace
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// logClauses plans the existential log clauses: typed equalities (D1) over
// LOGS_<T>_ATTR, or LOGS_EVENT for intrinsic event_name, whose entries cover
// trace_id and span_id (logs_rel.go).
func (r *SpanReader) logClauses(op string, clauses []Attribute) ([]witnessScanner[LogMatch], error) {
	out := make([]witnessScanner[LogMatch], len(clauses))
	for i, a := range clauses {
		c, err := newLogClause(op, a)
		if err != nil {
			return nil, err
		}
		out[i] = c
	}
	return out, nil
}

// messageClauses plans the existential message clauses: each is a message
// full-text query (messages_query_rel.go) without limit or continuation.
func (r *SpanReader) messageClauses(op string, clauses []MessageFullTextQuery) ([]witnessScanner[MessageSpanHit], error) {
	if len(clauses) == 0 {
		return nil, nil
	}
	if r.cfg.Messages != messagesFullText {
		return nil, fail("config", op, "message clauses need the fulltext message layout")
	}
	out := make([]witnessScanner[MessageSpanHit], len(clauses))
	for i, mq := range clauses {
		if mq.Limit != 0 || len(mq.Continuation) > 0 {
			return nil, fail("config", op, "message clauses take no limit or continuation")
		}
		plan, err := newRelMessageQueryPlan(op, mq, 0)
		if err != nil {
			return nil, err
		}
		out[i] = messageClause{r: r, plan: plan}
	}
	return out, nil
}

// QueryTraceExistential returns the newest traces satisfying every clause,
// each clause witnessed by any of the trace's spans, logs or messages. Each
// clause's complete term inside the window is unioned before the join, so
// witnesses may be different spans.
func (r *SpanReader) QueryTraceExistential(ctx context.Context, q TraceExistentialQuery) (TraceExistentialResult, error) {
	const op = "trace existential"
	empty := func() TraceExistentialResult { return TraceExistentialResult{Hits: []TraceExistentialHit{}} }
	out := empty()
	n := len(q.Clauses) + len(q.LogClauses) + len(q.MessageClauses)
	if n == 0 || n > maxQueryPredicates {
		return out, failf("config", op, "expected 1..%d clauses", maxQueryPredicates)
	}
	spans := make([]witnessScanner[SpanMatch], len(q.Clauses))
	for i, a := range q.Clauses {
		attr, err := newRelAttr(op, a, "resource", "scope", "span")
		if err != nil {
			return out, err
		}
		if err := checkMessageAttributeQuery(op, a, r.cfg.Messages != messagesNone); err != nil {
			return out, err
		}
		spans[i] = spanClause{attr}
	}
	logs, err := r.logClauses(op, q.LogClauses)
	if err != nil {
		return out, err
	}
	msgs, err := r.messageClauses(op, q.MessageClauses)
	if err != nil {
		return out, err
	}
	if q.Limit < 0 {
		return out, fail("config", op, "limit must not be negative")
	}
	limit := q.Limit
	if limit == 0 {
		limit = r.cfg.K
	}
	w, err := newTimeWindow(q.TimeWindow)
	if err != nil {
		return out, err
	}
	work := newQueryWork(r.cfg)
	err = r.store.read(ctx, op, func(store *recordlayer.FDBRecordStore) error {
		out, work = empty(), newQueryWork(r.cfg)
		var err error
		if out.ReadVersion, err = store.Context().GetReadVersion(); err != nil {
			return err
		}
		if w.empty {
			return nil
		}
		j := &traceJoin{spans: len(spans), logs: len(logs), msgs: len(msgs)}
		if ok, err := joinClauses(ctx, store, spans, w, work, j, func(tw *relWitnesses) [][]SpanMatch { return tw.spans }); err != nil || !ok {
			return err
		}
		if ok, err := joinClauses(ctx, store, logs, w, work, j, func(tw *relWitnesses) [][]LogMatch { return tw.logs }); err != nil || !ok {
			return err
		}
		if ok, err := joinClauses(ctx, store, msgs, w, work, j, func(tw *relWitnesses) [][]MessageSpanHit { return tw.msgs }); err != nil || !ok {
			return err
		}
		out.Hits = j.hits(limit)
		return nil
	})
	out.Candidates, out.Bytes = work.candidates, work.bytes
	return out, err
}

// ---------------------------------------------------------------------------
// Trace graph
// ---------------------------------------------------------------------------

// TraverseTraceGraph reads every stored span of the trace in one budgeted
// transaction and returns its observed parent adjacency and links, with the
// work it read.
func (r *SpanReader) TraverseTraceGraph(ctx context.Context, traceID string) (TraceGraph, QueryResult, error) {
	const op = "trace graph"
	var work QueryResult
	if !validIdentity(traceID, 16) {
		return TraceGraph{}, work, fail("config", op, "expected lowercase hex nonzero 16-byte trace identifier")
	}
	trace, err := hex.DecodeString(traceID)
	if err != nil {
		return TraceGraph{}, work, wrap("config", op, err)
	}
	var spans []graphSpan
	err = r.store.read(ctx, op, func(store *recordlayer.FDBRecordStore) error {
		spans, work = nil, QueryResult{}
		w := newQueryWork(r.cfg)
		defer w.result(&work)
		return scanTraceSpans(ctx, store, op, trace, w, func(m protoreflect.Message, f spanFields) error {
			s, err := graphSpanOf(op, trace, m, f)
			if err != nil {
				return err
			}
			spans = append(spans, s)
			return nil
		})
	})
	if err != nil {
		return TraceGraph{}, work, err
	}
	return buildTraceGraph(traceID, spans), work, nil
}

// graphSpanOf decodes a SPANS record's otlp column, the single-span
// ResourceSpans the exporter stored, for the span's parent and links.
func graphSpanOf(op string, trace []byte, m protoreflect.Message, f spanFields) (graphSpan, error) {
	rs := &tracepb.ResourceSpans{}
	if err := proto.Unmarshal(m.Get(f.otlp).Bytes(), rs); err != nil {
		return graphSpan{}, typedError("corruption", op, "span otlp decode", err)
	}
	if len(rs.ScopeSpans) != 1 || len(rs.ScopeSpans[0].GetSpans()) != 1 || rs.ScopeSpans[0].Spans[0] == nil {
		return graphSpan{}, fail("corruption", op, "span otlp is not a single-span ResourceSpans")
	}
	sp := rs.ScopeSpans[0].Spans[0]
	id, start := m.Get(f.spanID).Bytes(), m.Get(f.start).Int()
	if !bytes.Equal(sp.TraceId, trace) || !bytes.Equal(sp.SpanId, id) || sp.StartTimeUnixNano != uint64(start) {
		return graphSpan{}, fail("corruption", op, "span otlp disagrees with its record")
	}
	return graphSpan{SpanMatch{Trace: hex.EncodeToString(trace), ID: hex.EncodeToString(id), Start: start}, sp}, nil
}

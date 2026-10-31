package otel

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

const (
	maxQueryPredicates   = 16
	maxContinuationBytes = 4096
	continuationVersion  = int64(1)
	continuationKind     = "same_span"
	continuationDomain   = "otel/continuation/v1\x00"
	continuationKeyBytes = 32
	// sameSpanProbeBatch bounds how many verified driver candidates share one
	// pipelined round of point probes for the remaining predicates.
	sameSpanProbeBatch = 64
)

// SameSpanQuery asks for spans carrying every predicate on the same
// (trace_id, span_id). Predicates[0] drives the scan; callers should put the
// most selective predicate first. Result order is the run's posting order.
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

// TraceExistentialQuery is RFC-283 section 6.1's trace existential: each
// clause is satisfied by some (possibly different) witness in the same trace.
// LogClauses match trace-correlated logs through their LOG_TRACE correlation.
type TraceExistentialQuery struct {
	Clauses    []Attribute `json:"clauses"`
	LogClauses []Attribute `json:"log_clauses,omitempty"`
	TimeWindow *TimeRange  `json:"time_window,omitempty"`
	Limit      int         `json:"limit,omitempty"`
}

// TraceExistentialHit retains every verified witness per clause: Witnesses[i]
// satisfies Clauses[i] and LogWitnesses[j] satisfies LogClauses[j], newest first.
// Latest is the newest witness timestamp across all clauses.
type TraceExistentialHit struct {
	Trace        string        `json:"trace_id"`
	Latest       int64         `json:"latest"`
	Witnesses    [][]SpanMatch `json:"witnesses"`
	LogWitnesses [][]LogMatch  `json:"log_witnesses,omitempty"`
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

// queryBudget charges every key read, including rejected candidates and probes.
type queryBudget struct {
	op            string
	candidates    int
	bytes         int64
	maxCandidates int
	maxBytes      int64
}

func (q *queryBudget) charge(kv fdb.KeyValue) error {
	q.candidates++
	q.bytes += int64(len(kv.Key) + len(kv.Value))
	if q.candidates > q.maxCandidates || q.bytes > q.maxBytes {
		return failf("budget", q.op, "candidates=%d logical_bytes=%d", q.candidates, q.bytes)
	}
	return nil
}

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

// descendingRange is the key range of prefix P whose first element is -t for
// t inside w; w must be set and nonempty, so 1-hi and 1-lo cannot overflow.
func (w timeWindow) descendingRange(p subspace.Subspace) (fdb.Key, fdb.Key) {
	return p.Pack(tuple.Tuple{1 - w.hi}), p.Pack(tuple.Tuple{1 - w.lo})
}

func validateQueryAttribute(op string, a Attribute, scopes ...string) error {
	known := false
	for _, s := range scopes {
		known = known || a.Scope == s
	}
	if !known {
		return failf("config", op, "unsupported attribute scope %q", a.Scope)
	}
	switch a.Type {
	case "string":
	case "int":
		n, err := strconv.ParseInt(a.Value, 10, 64)
		if err != nil || strconv.FormatInt(n, 10) != a.Value {
			return fail("config", op, "int attribute value must be a canonical base-10 int64")
		}
	default:
		return failf("config", op, "unsupported attribute type %q; indexed types are string and int", a.Type)
	}
	// Larger keys and values are never indexed; an empty answer would not be exhaustive.
	if len(a.Key) > maxIndexedKeyBytes || len(a.Value) > maxIndexedValueBytes {
		return fail("config", op, "attribute exceeds the indexed key/value cap")
	}
	return nil
}

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

type sameSpanPlan struct {
	preds  []Attribute
	window timeWindow
	limit  int
	digest []byte
}

func newSameSpanPlan(q SameSpanQuery, k int) (sameSpanPlan, error) {
	const op = "same-span query"
	preds := uniqueAttributes(q.Predicates)
	if len(preds) == 0 || len(q.Predicates) > maxQueryPredicates {
		return sameSpanPlan{}, failf("config", op, "expected 1..%d predicates", maxQueryPredicates)
	}
	for _, a := range preds {
		if err := validateQueryAttribute(op, a, "resource", "scope", "span"); err != nil {
			return sameSpanPlan{}, err
		}
	}
	if q.Limit < 0 {
		return sameSpanPlan{}, fail("config", op, "limit must not be negative")
	}
	w, err := newTimeWindow(q.TimeWindow)
	if err != nil {
		return sameSpanPlan{}, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = k
	}
	return sameSpanPlan{preds: preds, window: w, limit: limit, digest: queryDigest(continuationKind, preds, w)}, nil
}

// continuation is a parsed, not yet authenticated, token.
type continuation struct {
	run     string
	tenant  int64
	order   string
	digest  []byte
	last    []byte
	payload []byte
	mac     []byte
}

func continuationMAC(secret, payload []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(continuationDomain))
	m.Write(payload)
	return m.Sum(nil)
}

func signContinuation(secret []byte, run string, tenant int, order string, digest, last []byte) []byte {
	payload := tuple.Tuple{continuationVersion, continuationKind, run, int64(tenant), order, digest, last}.Pack()
	return append(payload, continuationMAC(secret, payload)...)
}

func parseContinuation(token []byte) (*continuation, error) {
	const op = "continuation"
	if len(token) <= sha256.Size || len(token) > maxContinuationBytes {
		return nil, fail("config", op, "malformed continuation token length")
	}
	payload, mac := token[:len(token)-sha256.Size], token[len(token)-sha256.Size:]
	fields, err := tuple.Unpack(payload)
	if err != nil {
		return nil, typedError("config", op, "malformed continuation token", err)
	}
	if len(fields) != 7 {
		return nil, fail("config", op, "malformed continuation token fields")
	}
	version, vok := fields[0].(int64)
	kind, kok := fields[1].(string)
	c := &continuation{payload: payload, mac: mac}
	var rok, tok, ook, dok, lok bool
	c.run, rok = fields[2].(string)
	c.tenant, tok = fields[3].(int64)
	c.order, ook = fields[4].(string)
	c.digest, dok = fields[5].([]byte)
	c.last, lok = fields[6].([]byte)
	if !vok || !kok || !rok || !tok || !ook || !dok || !lok || version != continuationVersion || kind != continuationKind {
		return nil, fail("config", op, "unsupported continuation token")
	}
	return c, nil
}

// verify authenticates the token, then checks its bindings to this run and
// query, and returns the validated last emitted sort key.
func (c *continuation) verify(secret []byte, b *FDBBackend, digest []byte) (tuple.Tuple, error) {
	const op = "continuation"
	if len(secret) == 0 {
		return nil, fail("corruption", op, "continuation signing key is absent for this run")
	}
	if !hmac.Equal(c.mac, continuationMAC(secret, c.payload)) {
		return nil, fail("corruption", op, "continuation token failed authentication")
	}
	switch {
	case c.run != b.cfg.Run:
		return nil, fail("config", op, "continuation belongs to another run")
	case c.tenant != int64(b.cfg.Tenant):
		return nil, fail("config", op, "continuation belongs to another tenant")
	case c.order != b.cfg.Order:
		return nil, fail("config", op, "continuation belongs to another posting order")
	case !bytes.Equal(c.digest, digest):
		return nil, fail("config", op, "continuation belongs to another query")
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
	return b.root.Sub("query_v1", "continuation_hmac_v1").FDBKey()
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
	if err := b.readManifest(rc, true); err != nil {
		return nil, err
	}
	store, err := b.open(rc)
	if err != nil {
		return nil, err
	}
	state, err := store.ReadIndexState(b.idx.Name)
	if err != nil {
		return nil, err
	}
	if !state.IsScannable() {
		return nil, fail("ownership", op, "index is not readable")
	}
	m, err := store.GetIndexMaintainer(b.idx)
	if err != nil {
		return nil, err
	}
	pm, ok := recordlayer.IndexMaintainerAs[*postingMaintainer](m)
	if !ok {
		return nil, fail("config", op, "custom scanner not linked")
	}
	return pm, nil
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

// QuerySameSpan returns one snapshot_page of spans that satisfy every
// predicate on the same span, in posting order, resuming after a signed
// continuation at a new read version.
func (b *FDBBackend) QuerySameSpan(ctx context.Context, q SameSpanQuery) (SpanQueryPage, error) {
	const op = "same-span query"
	b.lastWork = QueryResult{}
	page := SpanQueryPage{Spans: []SpanMatch{}, Order: b.cfg.Order}
	plan, err := newSameSpanPlan(q, b.cfg.K)
	if err != nil {
		return page, err
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
}

// QueryTraceExistential unions each clause's witnesses over the complete term
// before joining traces, so witnesses may be different spans or logs.
func (b *FDBBackend) QueryTraceExistential(ctx context.Context, q TraceExistentialQuery) (TraceExistentialResult, error) {
	const op = "trace existential"
	b.lastWork = QueryResult{}
	out := TraceExistentialResult{Hits: []TraceExistentialHit{}}
	n := len(q.Clauses) + len(q.LogClauses)
	if n == 0 || n > maxQueryPredicates {
		return out, failf("config", op, "expected 1..%d clauses", maxQueryPredicates)
	}
	for _, a := range q.Clauses {
		if err := validateQueryAttribute(op, a, "resource", "scope", "span"); err != nil {
			return out, err
		}
	}
	for _, a := range q.LogClauses {
		if err := validateQueryAttribute(op, a, logAttributeScopes...); err != nil {
			return out, err
		}
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
		pm, err := b.openPostings(rc, op)
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
				tw = &traceWitnesses{spans: make([][]SpanMatch, len(q.Clauses)), logs: make([][]LogMatch, len(q.LogClauses))}
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
			err := scanLogTerm(tx, b.root, b.cfg.Tenant, b.cfg.HashBytes, a, w, budget, func(m LogMatch) (bool, error) {
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

type graphSpan struct {
	match SpanMatch
	span  *tracepb.Span
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
			rs, err := restoreOTLP(msg.Get(msg.Descriptor().Fields().ByName("otlp")).Bytes())
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

// QuerySameSpan returns one verified snapshot_page of same-span matches.
func (e *Exporter) QuerySameSpan(ctx context.Context, q SameSpanQuery) (SpanQueryPage, error) {
	if err := e.begin("same-span query"); err != nil {
		return SpanQueryPage{Spans: []SpanMatch{}, Order: e.cfg.Order}, err
	}
	defer e.active.Done()
	e.queryMu.Lock()
	defer e.queryMu.Unlock()
	return e.backend.QuerySameSpan(ctx, q)
}

// QueryTraceExistential returns the newest traces satisfying every clause, with witnesses.
func (e *Exporter) QueryTraceExistential(ctx context.Context, q TraceExistentialQuery) (TraceExistentialResult, error) {
	if err := e.begin("trace existential"); err != nil {
		return TraceExistentialResult{Hits: []TraceExistentialHit{}}, err
	}
	defer e.active.Done()
	e.queryMu.Lock()
	defer e.queryMu.Unlock()
	return e.backend.QueryTraceExistential(ctx, q)
}

// TraverseTraceGraph returns the observed parent/child/link topology of one trace.
func (e *Exporter) TraverseTraceGraph(ctx context.Context, traceID string) (TraceGraph, error) {
	if err := e.begin("trace graph"); err != nil {
		return TraceGraph{}, err
	}
	defer e.active.Done()
	e.queryMu.Lock()
	defer e.queryMu.Unlock()
	return e.backend.TraverseTraceGraph(ctx, traceID)
}

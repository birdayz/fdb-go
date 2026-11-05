package otel

// Message full-text queries over the Message TEXT index and the span back
// references of messages.go.
//
// A query follows Message TEXT hits through MessageTrace markers to each trace's
// envelope_refs_v1 chunks; newest-element references identify first-turn hits.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"sort"
	"sync"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

const (
	maxMessageQueryBytes    = 4096
	messageContinuationKind = "message_fts"
)

const (
	MessageMatchAll    = "all"
	MessageMatchAny    = "any"
	MessageMatchPhrase = "phrase"
)

// MessageFullTextQuery matches message references: all (default), any, or consecutive-position phrase.
// Role/attribute/span predicates filter references; FirstTurnOnly selects the introducing span, independent of arrival.
type MessageFullTextQuery struct {
	Query         string      `json:"query"`
	Mode          string      `json:"mode,omitempty"`
	AttributeKey  string      `json:"attribute_key,omitempty"`
	Role          string      `json:"role,omitempty"`
	Predicates    []Attribute `json:"predicates,omitempty"`
	TimeWindow    *TimeRange  `json:"time_window,omitempty"`
	FirstTurnOnly bool        `json:"first_turn_only,omitempty"`
	Limit         int         `json:"limit,omitempty"`
	Continuation  []byte      `json:"continuation,omitempty"`
}

// MessageSpanHit identifies a message ("writer.seq") reference with 0-based manifest TurnOrdinals.
type MessageSpanHit struct {
	Trace        string `json:"trace_id"`
	Span         string `json:"span_id"`
	Start        int64  `json:"start_ns"`
	AttributeKey string `json:"attribute_key"`
	Role         string `json:"role,omitempty"`
	MessageID    string `json:"message_id"`
	TurnOrdinals []int  `json:"turn_ordinals"`
}

// MessageFullTextPage is one snapshot page in the run's order (span sort key,
// then attribute key, then writer/sequence message ID). A full page carries a continuation.
type MessageFullTextPage struct {
	Hits         []MessageSpanHit `json:"hits"`
	Continuation []byte           `json:"continuation,omitempty"`
	Order        string           `json:"order"`
	ReadVersion  int64            `json:"read_version"`
	Candidates   int              `json:"candidates"`
	Bytes        int64            `json:"logical_read_bytes"`
}

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
	preds     []Attribute
	window    timeWindow
	firstTurn bool
	limit     int
	digest    []byte
}

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
	p.preds = uniqueAttributes(q.Predicates)
	if len(q.Predicates) > maxQueryPredicates {
		return nil, failf("config", op, "expected at most %d predicates", maxQueryPredicates)
	}
	for _, a := range p.preds {
		if err := validateQueryAttribute(op, a, "resource", "scope", "span"); err != nil {
			return nil, err
		}
		if err := checkMessageAttributeQuery(op, a, true); err != nil {
			return nil, err
		}
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
	t = append(t, p.attrKey, p.role, p.firstTurn, queryDigest(messageContinuationKind, p.preds, p.window))
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

// messageCandidate is one matching message.
type messageCandidate struct {
	id   messageID
	role string
}

// messageRoles lists the roles of the tenant's TEXT index groups, one
// positioned index scan per role.
func messageRoles(ctx context.Context, store *recordlayer.FDBRecordStore, idx *recordlayer.Index, tenant int, budget *queryBudget) ([]string, error) {
	var out []string
	low := recordlayer.TupleRangeAllOf(tuple.Tuple{int64(tenant)})
	for {
		props := recordlayer.ForwardScan()
		props.ExecuteProperties.ReturnedRowLimit = 1
		props.ExecuteProperties.ScannedRecordsLimit = max(budget.maxCandidates-budget.candidates, 0) + 1
		props.ExecuteProperties.ScannedBytesLimit = max(budget.maxBytes-budget.bytes, 0) + 1
		state := recordlayer.NewScanLimiterState()
		props.ExecuteProperties.ScanState = state
		cur := store.ScanIndexByType(idx, recordlayer.IndexScanByTextToken, low, nil, props)
		r, err := cur.OnNext(ctx)
		cur.Close()
		if err != nil {
			return nil, err
		}
		budget.candidates += state.RecordsScanned()
		budget.bytes += state.BytesScanned()
		if budget.candidates > budget.maxCandidates || budget.bytes > budget.maxBytes {
			return nil, failf("budget", "message roles", "candidates=%d logical_bytes=%d", budget.candidates, budget.bytes)
		}
		if !r.HasNext() {
			if reason := r.GetNoNextReason(); reason != recordlayer.SourceExhausted && reason != recordlayer.ReturnLimitReached {
				return nil, failf("budget", "message roles", "text scan stopped: %v", reason)
			}
			return out, nil
		}
		e := r.GetValue()
		if len(e.Key) < 2 {
			return nil, fail("corruption", "message roles", "malformed message text index entry")
		}
		role, ok := e.Key[1].(string)
		if !ok {
			return nil, fail("corruption", "message roles", "malformed message text index entry")
		}
		out = append(out, role)
		low = recordlayer.TupleRange{Low: tuple.Tuple{int64(tenant), role}, High: tuple.Tuple{int64(tenant)}, LowEndpoint: recordlayer.EndpointTypeRangeExclusive, HighEndpoint: recordlayer.EndpointTypeRangeInclusive}
	}
}

// textScanIDs runs one Record Layer TextScan and returns the IDs of the
// messages its entries name, charging its reads to budget.
func textScanIDs(ctx context.Context, store *recordlayer.FDBRecordStore, scan *recordlayer.TextScan, limit int, budget *queryBudget) ([]messageID, error) {
	const op = "message full-text query"
	props := recordlayer.ForwardScan()
	props.ExecuteProperties.ReturnedRowLimit = limit
	props.ExecuteProperties.ScannedRecordsLimit = max(budget.maxCandidates-budget.candidates, 0) + 1
	props.ExecuteProperties.ScannedBytesLimit = max(budget.maxBytes-budget.bytes, 0) + 1
	state := recordlayer.NewScanLimiterState()
	props.ExecuteProperties.ScanState = state
	cur, err := scan.Scan(store, nil, props)
	if err != nil {
		return nil, typedError("config", op, "text scan", err)
	}
	defer cur.Close()
	var out []messageID
	for {
		r, err := cur.OnNext(ctx)
		if err != nil {
			return nil, err
		}
		if !r.HasNext() {
			budget.candidates += state.RecordsScanned()
			budget.bytes += state.BytesScanned()
			if reason := r.GetNoNextReason(); reason != recordlayer.SourceExhausted && reason != recordlayer.ReturnLimitReached {
				return nil, failf("budget", op, "text scan stopped: %v; candidates=%d logical_bytes=%d", reason, budget.candidates, budget.bytes)
			}
			if budget.candidates > budget.maxCandidates || budget.bytes > budget.maxBytes {
				return nil, failf("budget", op, "candidates=%d logical_bytes=%d", budget.candidates, budget.bytes)
			}
			return out, nil
		}
		pk := r.GetValue().PrimaryKey()
		var id messageID
		ok := len(pk) == 4
		if ok {
			id.writer, ok = pk[1].(int64)
		}
		if ok {
			id.seq, ok = pk[2].(int64)
		}
		if !ok || !id.valid() {
			return nil, fail("corruption", op, "malformed message text index entry")
		}
		out = append(out, id)
	}
}

// matchMessages returns role-matching messages ordered by ID; overlap makes per-part phrase/any scans exact.
// For all-term matches spanning parts, it probes each multipart message for every term.
func (b *FDBBackend) matchMessages(ctx context.Context, store *recordlayer.FDBRecordStore, plan *messageQueryPlan, budget *queryBudget) ([]*messageCandidate, error) {
	idx := b.md.GetIndex(messageTextIndexName)
	tenant := b.cfg.Tenant
	roles := []string{plan.role}
	if plan.role == "" {
		var err error
		if roles, err = messageRoles(ctx, store, idx, tenant, budget); err != nil {
			return nil, err
		}
	}
	typ, tokens := recordlayer.TextContainsAll, plan.terms
	switch plan.mode {
	case MessageMatchAny:
		typ = recordlayer.TextContainsAny
	case MessageMatchPhrase:
		typ = recordlayer.TextContainsPhrase
		tokens = make([]string, plan.phrase[len(plan.phrase)-1].offset+1)
		for _, pt := range plan.phrase {
			tokens[pt.offset] = pt.term
		}
	}
	seen := map[messageID]bool{}
	var out []*messageCandidate
	for _, role := range roles {
		ids, err := shardTextScans(ctx, store, idx, int64(tenant), role, typ, tokens, budget)
		if err != nil {
			return nil, err
		}
		if plan.mode == MessageMatchAll && len(plan.terms) > 1 {
			spread, err := b.spreadAllMatches(ctx, store, idx, int64(tenant), role, plan.terms, budget)
			if err != nil {
				return nil, err
			}
			ids = append(ids, spread...)
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				out = append(out, &messageCandidate{id: id, role: role})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id.less(out[j].id) })
	return out, nil
}

// shardTextScans unions concurrent shard scans; each may use the remaining budget,
// but their aggregate cost must also fit.
func shardTextScans(ctx context.Context, store *recordlayer.FDBRecordStore, idx *recordlayer.Index, tenant int64, role string, typ recordlayer.TextComparisonType, tokens []string, budget *queryBudget) ([]messageID, error) {
	found := make([][]messageID, messageTextShards)
	subs := make([]queryBudget, messageTextShards)
	errs := make([]error, messageTextShards)
	var wg sync.WaitGroup
	for shard := range messageTextShards {
		subs[shard] = queryBudget{op: budget.op, maxCandidates: budget.maxCandidates - budget.candidates, maxBytes: budget.maxBytes - budget.bytes}
		wg.Add(1)
		go func() {
			defer wg.Done()
			scan := recordlayer.NewTextScan(idx, tuple.Tuple{tenant, role, int64(shard)}, recordlayer.NewTextComparisonTokens(typ, tokens, "", ""), nil)
			found[shard], errs[shard] = textScanIDs(ctx, store, scan, 0, &subs[shard])
		}()
	}
	wg.Wait()
	for _, s := range subs {
		budget.candidates += s.candidates
		budget.bytes += s.bytes
	}
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	if budget.candidates > budget.maxCandidates || budget.bytes > budget.maxBytes {
		return nil, failf("budget", budget.op, "candidates=%d logical_bytes=%d", budget.candidates, budget.bytes)
	}
	var ids []messageID
	for _, f := range found {
		ids = append(ids, f...)
	}
	return ids, nil
}

// spreadAllMatches checks multipart messages with one message-bounded scan per term.
func (b *FDBBackend) spreadAllMatches(ctx context.Context, store *recordlayer.FDBRecordStore, idx *recordlayer.Index, tenant int64, role string, terms []string, budget *queryBudget) ([]messageID, error) {
	const op = "message full-text query"
	cur := store.ScanIndex(b.md.GetIndex(messageLargeIndexName), recordlayer.TupleRangeAllOf(tuple.Tuple{tenant}), nil, recordlayer.ForwardScan())
	defer cur.Close()
	var out []messageID
	for {
		r, err := cur.OnNext(ctx)
		if err != nil {
			return nil, err
		}
		if !r.HasNext() {
			return out, nil
		}
		e := r.GetValue()
		if err := budget.charge(fdb.KeyValue{Key: e.Key.Pack()}); err != nil {
			return nil, err
		}
		pk := e.PrimaryKey()
		var id messageID
		ok := len(pk) == 4
		if ok {
			id.writer, ok = pk[1].(int64)
		}
		if ok {
			id.seq, ok = pk[2].(int64)
		}
		if !ok || !id.valid() {
			return nil, fail("corruption", op, "malformed large message index entry")
		}
		all := true
		group := tuple.Tuple{tenant, role, messageShard(id.writer)}
		within := recordlayer.TupleRangeAllOf(tuple.Tuple{id.writer, id.seq})
		for _, t := range terms {
			cmp := recordlayer.NewTextComparisonTokens(recordlayer.TextContainsAll, []string{t}, "", "")
			found, err := textScanIDs(ctx, store, recordlayer.NewTextScan(idx, group, cmp, &within), 1, budget)
			if err != nil {
				return nil, err
			}
			if len(found) == 0 {
				all = false
				break
			}
		}
		if all {
			out = append(out, id)
		}
	}
}

// messageRef is one candidate (span, attribute, message) reference.
type messageRef struct {
	cand      *messageCandidate
	trace     string
	span      string
	start     int64
	attribute int64
	ordinals  []int
	key       []byte // sort key: posting suffix, attribute key, writer, seq
}

func messageRefKey(order, trace, span string, start int64, attribute int64, id messageID) []byte {
	t := append(postingSuffix(Span{Trace: trace, ID: span, Start: start}, order), messageAttributeKeys[attribute], id.writer, id.seq)
	return t.Pack()
}

// messageRefReads bounds the range reads in flight at once.
const messageRefReads = 256

// pipelinedRanges reads up to messageRefReads ranges concurrently, charging each entry
// to budget and passing at most limit() entries per range to visit.
func pipelinedRanges(ctx context.Context, tx fdb.ReadTransaction, ranges []fdb.KeyRange, budget *queryBudget, visit func(i int, kv fdb.KeyValue) error) error {
	const op = "message full-text query"
	for lo := 0; lo < len(ranges); lo += messageRefReads {
		if err := canceled(ctx, op); err != nil {
			return err
		}
		hi := min(lo+messageRefReads, len(ranges))
		// No range returns more entries than the budget has left.
		limit := max(budget.maxCandidates-budget.candidates, 0) + 1
		pending := make([]fdb.RangeResult, hi-lo)
		for i := lo; i < hi; i++ {
			pending[i-lo] = tx.GetRange(ranges[i], fdb.RangeOptions{Limit: limit, Mode: fdb.StreamingModeWantAll})
		}
		for i := lo; i < hi; i++ {
			rows, err := pending[i-lo].GetSliceWithError()
			if err != nil {
				return err
			}
			for _, kv := range rows {
				if err := budget.charge(kv); err != nil {
					return err
				}
				if err := visit(i, kv); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// messageRefs joins matching messages through MessageTrace markers to packed envelope_refs_v1 ranges.
// Both rounds read concurrently without loading span records; windowed references are unordered.
func (b *FDBBackend) messageRefs(ctx context.Context, store *recordlayer.FDBRecordStore, plan *messageQueryPlan, w timeWindow, budget *queryBudget) ([]messageRef, error) {
	const op = "message full-text query"
	cands, err := b.matchMessages(ctx, store, plan, budget)
	if err != nil || len(cands) == 0 {
		return nil, err
	}
	tenant := int64(b.cfg.Tenant)
	tx := store.Context().ReadTransaction(false)
	bad := fail("corruption", op, "malformed message trace record key")

	// Round 1: the traces of each matching message.
	records := store.RecordsSubspace()
	ranges := make([]fdb.KeyRange, len(cands))
	for i, c := range cands {
		r, err := fdb.PrefixRange(records.Pack(tuple.Tuple{messageTraceTypeKey, tenant, c.id.writer, c.id.seq}))
		if err != nil {
			return nil, err
		}
		ranges[i] = r
	}
	byTrace := map[string]map[messageID]*messageCandidate{}
	var traces []string
	if err := pipelinedRanges(ctx, tx, ranges, budget, func(i int, kv fdb.KeyValue) error {
		t, err := records.Unpack(kv.Key)
		// (type key, tenant, writer, seq, trace_id) and the split suffix.
		if err != nil || len(t) < 5 {
			return bad
		}
		trace, ok := t[4].(string)
		if !ok || !validIdentity(trace, 16) {
			return bad
		}
		set := byTrace[trace]
		if set == nil {
			set = map[messageID]*messageCandidate{}
			byTrace[trace] = set
			traces = append(traces, trace)
		}
		set[cands[i].id] = cands[i]
		return nil
	}); err != nil {
		return nil, err
	}

	// Round 2: every span of those traces, with its references.
	sort.Strings(traces)
	space := store.IndexSubspace(b.md.GetIndex(envelopeRefsIndexName))
	ranges = ranges[:0]
	for _, trace := range traces {
		r, err := fdb.PrefixRange(space.Pack(tuple.Tuple{tenant, trace}))
		if err != nil {
			return nil, err
		}
		ranges = append(ranges, r)
	}
	var out []messageRef
	badRef := fail("corruption", op, "malformed span reference index entry")
	err = pipelinedRanges(ctx, tx, ranges, budget, func(i int, kv fdb.KeyValue) error {
		trace := traces[i]
		// (tenant, trace_id, start_ns, chunk) and the primary key's span_id;
		// tenant and trace_id are not repeated.
		k, err := space.Unpack(kv.Key)
		if err != nil || len(k) != 5 || k[0] != tenant || k[1] != trace {
			return badRef
		}
		start, sok := k[2].(int64)
		span, pok := k[4].(string)
		if !sok || !pok || !validIdentity(span, 8) {
			return badRef
		}
		if !w.contains(start) {
			return nil
		}
		v, err := tuple.Unpack(kv.Value)
		if err != nil || len(v) != 1 {
			return badRef
		}
		chunk, ok := v[0].([]byte)
		if !ok {
			return badRef
		}
		refs, err := unpackSpanRefs(chunk)
		if err != nil {
			return err
		}
		set := byTrace[trace]
		for _, r := range refs {
			c := set[r.id]
			if c == nil || (plan.attribute > 0 && r.attribute != plan.attribute) || (plan.firstTurn && !r.newest) {
				continue
			}
			out = append(out, messageRef{cand: c, trace: trace, span: span, start: start, attribute: r.attribute, ordinals: r.ordinals})
		}
		return nil
	})
	return out, err
}

type messageProbeSpan struct {
	trace, span string
	start       int64
}

// probeMessagePredicates checks every predicate on every distinct span of
// the batch in one pipelined round of point probes.
func probeMessagePredicates(tx fdb.ReadTransaction, pm *postingMaintainer, tenant int, preds []Attribute, batch []messageRef, budget *queryBudget) ([]bool, error) {
	index := map[messageProbeSpan]int{}
	var spans []messageProbeSpan
	for _, r := range batch {
		k := messageProbeSpan{r.trace, r.span, r.start}
		if _, ok := index[k]; !ok {
			index[k] = len(spans)
			spans = append(spans, k)
		}
	}
	terms := make([]subspace.Subspace, len(preds))
	for j, a := range preds {
		terms[j] = termPrefix(pm.secondary, tenant, a, pm.width)
	}
	probes := make([][]fdb.RangeResult, len(spans))
	for i, s := range spans {
		suffix := postingSuffix(Span{Trace: s.trace, ID: s.span, Start: s.start}, pm.order)
		probes[i] = make([]fdb.RangeResult, len(preds))
		for j, term := range terms {
			r, err := fdb.PrefixRange(term.Pack(suffix))
			if err != nil {
				return nil, err
			}
			probes[i][j] = tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll})
		}
	}
	ok := make([]bool, len(spans))
	for i, s := range spans {
		ok[i] = true
		for j, term := range terms {
			rows, err := probes[i][j].GetSliceWithError()
			if err != nil {
				return nil, err
			}
			found := false
			for _, kv := range rows {
				if err := budget.charge(kv); err != nil {
					return nil, err
				}
				m, a, err := decodeSpanPosting(pm, term, kv)
				if err != nil {
					return nil, err
				}
				if m.Trace != s.trace || m.ID != s.span {
					return nil, fail("corruption", "message predicate probe", "probe returned another span identity")
				}
				found = found || (a == preds[j] && m.Start == s.start)
			}
			ok[i] = ok[i] && found
		}
	}
	out := make([]bool, len(batch))
	for i, r := range batch {
		out[i] = ok[index[messageProbeSpan{r.trace, r.span, r.start}]]
	}
	return out, nil
}

func messageHit(r messageRef) MessageSpanHit {
	c := r.cand
	return MessageSpanHit{
		Trace:        r.trace,
		Span:         r.span,
		Start:        r.start,
		AttributeKey: messageAttributeKeys[r.attribute],
		Role:         c.role,
		MessageID:    c.id.String(),
		TurnOrdinals: append([]int(nil), r.ordinals...),
	}
}

// runMessageQuery emits hits in sort-key order strictly after resume (a sort
// key), stopping after limit hits (0: unbounded). w further restricts start.
func (b *FDBBackend) runMessageQuery(ctx context.Context, store *recordlayer.FDBRecordStore, pm *postingMaintainer, plan *messageQueryPlan, resume []byte, w timeWindow, limit int, budget *queryBudget, emit func(MessageSpanHit, []byte)) error {
	w = intersectTimeWindows(plan.window, w)
	if w.empty {
		return nil
	}
	refs, err := b.messageRefs(ctx, store, plan, w, budget)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	kept := refs[:0]
	for _, r := range refs {
		r.key = messageRefKey(b.cfg.Order, r.trace, r.span, r.start, r.attribute, r.cand.id)
		if (resume != nil && bytes.Compare(r.key, resume) <= 0) || seen[string(r.key)] {
			continue
		}
		seen[string(r.key)] = true
		kept = append(kept, r)
	}
	sort.Slice(kept, func(i, j int) bool { return bytes.Compare(kept[i].key, kept[j].key) < 0 })
	accepted := 0
	tx := store.Context().ReadTransaction(false)
	for lo := 0; lo < len(kept); {
		if err := canceled(ctx, "message full-text query"); err != nil {
			return err
		}
		n := sameSpanProbeBatch
		if limit > 0 {
			n = min(n, limit-accepted)
		}
		batch := kept[lo:min(lo+n, len(kept))]
		lo += len(batch)
		var ok []bool
		if len(plan.preds) > 0 {
			if ok, err = probeMessagePredicates(tx, pm, b.cfg.Tenant, plan.preds, batch, budget); err != nil {
				return err
			}
		}
		for i, r := range batch {
			if ok != nil && !ok[i] {
				continue
			}
			emit(messageHit(r), r.key)
			accepted++
			if limit > 0 && accepted == limit {
				return nil
			}
		}
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

// parseMessageResume validates an authenticated continuation's sort key.
func parseMessageResume(order string, raw []byte) ([]byte, error) {
	bad := fail("corruption", "continuation", "malformed message continuation sort key")
	t, err := tuple.Unpack(raw)
	if err != nil {
		return nil, bad
	}
	n := 5
	if order == "time" {
		n = 6
	}
	if len(t) != n {
		return nil, bad
	}
	writer, wok := t[n-2].(int64)
	seq, sok := t[n-1].(int64)
	attr, aok := t[n-3].(string)
	if !wok || !sok || !aok || !(messageID{writer, seq}).valid() || messageAttributeCode(attr) <= 0 {
		return nil, bad
	}
	if err := checkSortKey(t[:n-3], order); err != nil {
		return nil, err
	}
	return raw, nil
}

// QueryMessageFullText returns one snapshot page of message hits, resuming
// after a signed continuation at a new read version.
func (b *FDBBackend) QueryMessageFullText(ctx context.Context, q MessageFullTextQuery) (MessageFullTextPage, error) {
	const op = "message full-text query"
	b.lastWork = QueryResult{}
	page := MessageFullTextPage{Hits: []MessageSpanHit{}, Order: b.cfg.Order}
	if b.cfg.Messages != messagesFullText {
		return page, fail("config", op, "message text is not indexed in this run's layout")
	}
	plan, err := newMessageQueryPlan(op, q, b.cfg.K)
	if err != nil {
		return page, err
	}
	var token *continuation
	if len(q.Continuation) > 0 {
		if token, err = parseContinuationKind(q.Continuation, messageContinuationKind); err != nil {
			return page, err
		}
	}
	var secret, lastKey []byte
	budget := b.newBudget(op)
	_, err = b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		page = MessageFullTextPage{Hits: []MessageSpanHit{}, Order: b.cfg.Order}
		budget = b.newBudget(op)
		lastKey = nil
		store, pm, err := b.openPostingsStore(rc, op)
		if err != nil {
			return nil, err
		}
		if secret, err = b.readContinuationSecret(rc); err != nil {
			return nil, err
		}
		var resume []byte
		if token != nil {
			if err := token.authenticate(secret, b, plan.digest); err != nil {
				return nil, err
			}
			if resume, err = parseMessageResume(b.cfg.Order, token.last); err != nil {
				return nil, err
			}
		}
		if page.ReadVersion, err = rc.GetReadVersion(); err != nil {
			return nil, err
		}
		return nil, b.runMessageQuery(ctx, store, pm, plan, resume, timeWindow{}, plan.limit, budget, func(h MessageSpanHit, key []byte) {
			page.Hits = append(page.Hits, h)
			lastKey = key
		})
	})
	page.Candidates, page.Bytes = budget.candidates, budget.bytes
	b.lastWork = QueryResult{Candidates: budget.candidates, Bytes: budget.bytes}
	if err != nil || len(page.Hits) < plan.limit {
		return page, err
	}
	if len(secret) == 0 {
		if secret, err = b.ensureContinuationSecret(ctx); err != nil {
			return page, err
		}
	}
	page.Continuation = signContinuationKind(secret, messageContinuationKind, b.cfg.Run, b.cfg.Tenant, b.cfg.Order, plan.digest, lastKey)
	return page, nil
}

// QueryMessageFullText searches message text; the exporter must index it.
func (e *Exporter) QueryMessageFullText(ctx context.Context, q MessageFullTextQuery) (MessageFullTextPage, error) {
	const op = "message full-text query"
	empty := MessageFullTextPage{Hits: []MessageSpanHit{}, Order: e.cfg.Order}
	if !e.cfg.EnableMessageFullText {
		return empty, fail("config", op, "enable_message_full_text is off; message text is not indexed")
	}
	if err := e.begin(op); err != nil {
		return empty, err
	}
	defer e.active.Done()
	e.queryMu.Lock()
	defer e.queryMu.Unlock()
	return e.backend.QueryMessageFullText(e.txnContext(ctx), q)
}

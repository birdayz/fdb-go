package otel

// Message full-text queries on a tenant store (rfcs/relational-exporter-migration.md
// §1.5, §3.2): MESSAGES_TEXT matches, across parts through MESSAGES_LARGE for
// "all", then MESSAGE_TRACES markers name each match's traces and the traces'
// SPANS_REF_CHUNKS entries name the referencing spans. Every read is charged
// to the query's budget; one page is one snapshot.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"sync"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// relMessageQueryPlan is a validated message query: the storage-free plan of
// its text, role, attribute, window and limit, and its typed same-span
// predicates (D1).
type relMessageQueryPlan struct {
	base  *messageQueryPlan
	attrs []relAttr
	// digest binds the base plan and the predicate set.
	digest []byte
}

func newRelMessageQueryPlan(op string, q MessageFullTextQuery, k int) (*relMessageQueryPlan, error) {
	if len(q.Predicates) > maxQueryPredicates {
		return nil, failf("config", op, "expected at most %d predicates", maxQueryPredicates)
	}
	preds := uniqueAttributes(q.Predicates)
	q.Predicates = nil
	base, err := newMessageQueryPlan(op, q, k)
	if err != nil {
		return nil, err
	}
	p := &relMessageQueryPlan{base: base, attrs: make([]relAttr, len(preds))}
	for i, a := range preds {
		if p.attrs[i], err = newRelAttr(op, a, "resource", "scope", "span"); err != nil {
			return nil, err
		}
		if err := checkMessageAttributeQuery(op, a, true); err != nil {
			return nil, err
		}
	}
	h := sha256.New()
	h.Write(base.digest)
	h.Write(queryDigest(messageContinuationKind, preds, base.window))
	p.digest = h.Sum(nil)
	return p, nil
}

// relMessageRefKey is a hit's sort key and a message_fts token's last:
// (start_ns, trace_id, span_id, attribute key, writer, seq). Hits are in
// index order, start, trace and span descending (D3), then attribute key and
// message ID ascending.
type relMessageRefKey struct {
	start       int64
	trace, span []byte
	attr        string
	id          messageID
}

func (k relMessageRefKey) pack() []byte {
	return tuple.Tuple{k.start, k.trace, k.span, k.attr, k.id.writer, k.id.seq}.Pack()
}

// before reports whether k sorts before o.
func (k relMessageRefKey) before(o relMessageRefKey) bool {
	if k.start != o.start {
		return k.start > o.start
	}
	if c := bytes.Compare(k.trace, o.trace); c != 0 {
		return c > 0
	}
	if c := bytes.Compare(k.span, o.span); c != 0 {
		return c > 0
	}
	if k.attr != o.attr {
		return k.attr < o.attr
	}
	return k.id.less(o.id)
}

// parseRelMessageRefKey validates an authenticated token's sort key.
func parseRelMessageRefKey(raw []byte) (relMessageRefKey, error) {
	bad := fail("corruption", "continuation", "malformed message continuation sort key")
	t, err := tuple.Unpack(raw)
	if err != nil || len(t) != 6 {
		return relMessageRefKey{}, bad
	}
	start, sok := t[0].(int64)
	trace, tok := t[1].([]byte)
	span, pok := t[2].([]byte)
	attr, aok := t[3].(string)
	writer, wok := t[4].(int64)
	seq, qok := t[5].(int64)
	k := relMessageRefKey{start, trace, span, attr, messageID{writer, seq}}
	if !sok || !tok || !pok || !aok || !wok || !qok || start < 0 || len(trace) != 16 || zeroID(trace) || len(span) != 8 || zeroID(span) ||
		messageAttributeCode(attr) <= 0 || !k.id.valid() {
		return relMessageRefKey{}, bad
	}
	return k, nil
}

// QueryMessageFullText returns one snapshot page of message hits, one per
// (span, attribute, message), in relMessageRefKey order. A full page carries
// a signed message_fts continuation; the next page may be empty.
func (r *SpanReader) QueryMessageFullText(ctx context.Context, q MessageFullTextQuery) (MessageFullTextPage, error) {
	const op = "message full-text query"
	empty := func() MessageFullTextPage { return MessageFullTextPage{Hits: []MessageSpanHit{}, Order: "time"} }
	page := empty()
	if r.cfg.Messages != messagesFullText {
		return page, fail("config", op, "message text is not indexed in this store's layout")
	}
	plan, err := newRelMessageQueryPlan(op, q, r.cfg.K)
	if err != nil {
		return page, err
	}
	var token *relContinuation
	if len(q.Continuation) > 0 {
		if token, err = parseRelContinuation(q.Continuation, messageContinuationKind); err != nil {
			return page, err
		}
	}
	var secret []byte
	var last relMessageRefKey
	work := newQueryWork(r.cfg)
	err = r.store.read(ctx, op, func(store *recordlayer.FDBRecordStore) error {
		page, work, secret = empty(), newQueryWork(r.cfg), nil
		var resume *relMessageRefKey
		var err error
		if token != nil {
			if secret, err = r.loadContinuationKey(store); err != nil {
				return err
			}
			if err := token.authenticate(secret, r.store, plan.digest); err != nil {
				return err
			}
			k, err := parseRelMessageRefKey(token.last)
			if err != nil {
				return err
			}
			resume = &k
		}
		if page.ReadVersion, err = store.Context().GetReadVersion(); err != nil {
			return err
		}
		err = r.runMessageQuery(ctx, store, plan, resume, timeWindow{}, plan.base.limit, work, func(h MessageSpanHit, k relMessageRefKey) {
			page.Hits = append(page.Hits, h)
			last = k
		})
		if err == nil && len(page.Hits) == plan.base.limit && secret == nil {
			secret, err = r.loadContinuationKey(store)
		}
		return err
	})
	page.Candidates, page.Bytes = work.candidates, work.bytes
	if err != nil || len(page.Hits) < plan.base.limit {
		return page, err
	}
	if len(secret) == 0 {
		if secret, err = r.ensureContinuationKey(ctx); err != nil {
			return page, err
		}
	}
	page.Continuation = signRelContinuation(secret, messageContinuationKind, r.store.databasePath, r.store.schema, plan.digest, last.pack())
	return page, nil
}

// messageClause is an existential message clause: every hit of its query
// inside the window witnesses the hit's trace.
type messageClause struct {
	r    *SpanReader
	plan *relMessageQueryPlan
}

func (c messageClause) scanWitnesses(ctx context.Context, store *recordlayer.FDBRecordStore, w timeWindow, work *queryWork, visit func(string, MessageSpanHit)) error {
	return c.r.runMessageQuery(ctx, store, c.plan, nil, w, 0, work, func(h MessageSpanHit, _ relMessageRefKey) { visit(h.Trace, h) })
}

// relMessageCandidate is one matching message and the role group it matched in.
type relMessageCandidate struct {
	id   messageID
	role string
}

// relMessageRef is one (span, attribute, message) reference of a match.
type relMessageRef struct {
	cand     *relMessageCandidate
	key      relMessageRefKey
	ordinals []int
}

func (ref relMessageRef) hit() MessageSpanHit {
	return MessageSpanHit{
		Trace:        hex.EncodeToString(ref.key.trace),
		Span:         hex.EncodeToString(ref.key.span),
		Start:        ref.key.start,
		AttributeKey: ref.key.attr,
		Role:         ref.cand.role,
		MessageID:    ref.cand.id.String(),
		TurnOrdinals: append([]int(nil), ref.ordinals...),
	}
}

// runMessageQuery emits plan's hits in sort-key order strictly after resume,
// stopping after limit hits (0: unbounded); w further restricts start.
func (r *SpanReader) runMessageQuery(ctx context.Context, store *recordlayer.FDBRecordStore, plan *relMessageQueryPlan, resume *relMessageRefKey, w timeWindow, limit int, work *queryWork, emit func(MessageSpanHit, relMessageRefKey)) error {
	const op = "message full-text query"
	w = intersectTimeWindows(plan.base.window, w)
	if w.empty {
		return nil
	}
	refs, err := r.messageRefs(ctx, store, plan, w, work)
	if err != nil {
		return err
	}
	if resume != nil {
		refs = slices.DeleteFunc(refs, func(ref relMessageRef) bool { return !resume.before(ref.key) })
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].key.before(refs[j].key) })
	var space []subspace.Subspace
	if len(plan.attrs) > 0 {
		space = make([]subspace.Subspace, len(plan.attrs))
		for j, a := range plan.attrs {
			idx, err := readableIndex(store, op, a.q.index)
			if err != nil {
				return err
			}
			space[j] = store.IndexSubspace(idx)
		}
	}
	accepted := 0
	for lo := 0; lo < len(refs); {
		if err := canceled(ctx, op); err != nil {
			return err
		}
		n := sameSpanProbeBatch
		if limit > 0 {
			n = min(n, limit-accepted)
		}
		batch := refs[lo:min(lo+n, len(refs))]
		lo += len(batch)
		var ok []bool
		if len(plan.attrs) > 0 {
			if ok, err = probeRelMessagePredicates(store, plan.attrs, space, batch, work); err != nil {
				return err
			}
		}
		for i, ref := range batch {
			if ok != nil && !ok[i] {
				continue
			}
			emit(ref.hit(), ref.key)
			accepted++
			if limit > 0 && accepted == limit {
				return nil
			}
		}
	}
	return nil
}

// probeRelMessagePredicates checks every predicate on every distinct span
// of the batch in one pipelined round of prefix probes of the span
// attribute indexes, then verifies long strings on the survivors' records.
func probeRelMessagePredicates(store *recordlayer.FDBRecordStore, attrs []relAttr, spaces []subspace.Subspace, batch []relMessageRef, work *queryWork) ([]bool, error) {
	const op = "message predicate probe"
	type span struct {
		start       int64
		trace, span string
	}
	index := map[span]int{}
	var spans []relMessageRefKey
	for _, ref := range batch {
		k := span{ref.key.start, string(ref.key.trace), string(ref.key.span)}
		if _, ok := index[k]; !ok {
			index[k] = len(spans)
			spans = append(spans, ref.key)
		}
	}
	tx := store.Context().ReadTransaction(false)
	probes := make([]fdb.RangeResult, 0, len(spans)*len(attrs))
	for _, s := range spans {
		for j, a := range attrs {
			kr, err := fdb.PrefixRange(spaces[j].Pack(append(slices.Clip(a.prefix), s.start, s.trace, s.span)))
			if err != nil {
				return nil, err
			}
			probes = append(probes, tx.GetRange(kr, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}))
		}
	}
	ok := make([]bool, len(spans))
	var verify []int
	var pks []tuple.Tuple
	for i, s := range spans {
		ok[i] = true
		for j := range attrs {
			rows, err := probes[i*len(attrs)+j].GetSliceWithError()
			if err != nil {
				return nil, err
			}
			for _, kv := range rows {
				if err := work.charge(op, int64(len(kv.Key)+len(kv.Value))); err != nil {
					return nil, err
				}
				if _, err := spaces[j].Unpack(kv.Key); err != nil {
					return nil, wrap("corruption", op, err)
				}
			}
			ok[i] = ok[i] && len(rows) > 0
		}
		if ok[i] && anyLong(attrs) {
			pk, err := tableKey(store, op, spansRecordType, s.trace, s.span)
			if err != nil {
				return nil, err
			}
			verify = append(verify, i)
			pks = append(pks, pk)
		}
	}
	if len(verify) > 0 {
		held, err := holdLongStrings(store, op, attrs, pks, work)
		if err != nil {
			return nil, err
		}
		for k, i := range verify {
			ok[i] = held[k]
		}
	}
	out := make([]bool, len(batch))
	for i, ref := range batch {
		out[i] = ok[index[span{ref.key.start, string(ref.key.trace), string(ref.key.span)}]]
	}
	return out, nil
}

// messageRefs follows the matching messages through their MESSAGE_TRACES
// markers to their traces' SPANS_REF_CHUNKS entries, keeping the references
// to a match inside w that pass the attribute and first-turn filters. Both
// rounds read concurrently and load no span record.
func (r *SpanReader) messageRefs(ctx context.Context, store *recordlayer.FDBRecordStore, plan *relMessageQueryPlan, w timeWindow, work *queryWork) ([]relMessageRef, error) {
	const op = "message full-text query"
	cands, err := matchMessages(ctx, store, plan.base, work)
	if err != nil || len(cands) == 0 {
		return nil, err
	}
	tx := store.Context().ReadTransaction(false)

	// Round 1: the traces of each matching message.
	records := store.RecordsSubspace()
	ranges := make([]fdb.KeyRange, len(cands))
	var prefixLen int
	for i, c := range cands {
		pk, err := tableKey(store, op, messageTracesRecordType, c.id.writer, c.id.seq)
		if err != nil {
			return nil, err
		}
		prefixLen = len(pk)
		if ranges[i], err = fdb.PrefixRange(records.Pack(pk)); err != nil {
			return nil, err
		}
	}
	byTrace := map[string]map[messageID]*relMessageCandidate{}
	var traces [][]byte
	badMarker := fail("corruption", op, "malformed message trace record key")
	if err := relPipelinedRanges(ctx, tx, ranges, work, func(i int, kv fdb.KeyValue) error {
		// The primary key (writer, seq, trace_id) and the record's split suffix.
		t, err := records.Unpack(kv.Key)
		if err != nil || len(t) <= prefixLen {
			return badMarker
		}
		trace, ok := t[prefixLen].([]byte)
		if !ok || len(trace) != 16 || zeroID(trace) {
			return badMarker
		}
		set := byTrace[string(trace)]
		if set == nil {
			set = map[messageID]*relMessageCandidate{}
			byTrace[string(trace)] = set
			traces = append(traces, trace)
		}
		set[cands[i].id] = cands[i]
		return nil
	}); err != nil {
		return nil, err
	}

	// Round 2: every span of those traces with its packed references.
	idx, err := readableIndex(store, op, spansRefChunksIndex)
	if err != nil {
		return nil, err
	}
	space := store.IndexSubspace(idx)
	sort.Slice(traces, func(i, j int) bool { return bytes.Compare(traces[i], traces[j]) < 0 })
	ranges = ranges[:0]
	for _, trace := range traces {
		kr, err := fdb.PrefixRange(space.Pack(tuple.Tuple{trace}))
		if err != nil {
			return nil, err
		}
		ranges = append(ranges, kr)
	}
	type refID struct {
		trace, span string
		attr        int64
		id          messageID
	}
	var out []relMessageRef
	at := map[refID]int{}
	badRef := fail("corruption", op, "malformed span reference index entry")
	err = relPipelinedRanges(ctx, tx, ranges, work, func(i int, kv fdb.KeyValue) error {
		// (trace_id, start_ns, span_id, chunk, refs) and the primary key.
		k, err := space.Unpack(kv.Key)
		if err != nil || len(k) < 5 {
			return badRef
		}
		trace, tok := k[0].([]byte)
		start, sok := k[1].(int64)
		span, pok := k[2].([]byte)
		packed, rok := k[4].([]byte)
		if !tok || !sok || !pok || !rok || !bytes.Equal(trace, traces[i]) || len(span) != 8 || zeroID(span) {
			return badRef
		}
		if !w.contains(start) {
			return nil
		}
		refs, err := otelsql.UnpackRefChunk(packed)
		if err != nil {
			return typedError("corruption", op, "span reference chunk", err)
		}
		set := byTrace[string(trace)]
		for _, ref := range refs {
			id := messageID{ref.Writer, ref.Seq}
			c := set[id]
			if c == nil || (plan.base.attribute > 0 && ref.Attribute != plan.base.attribute) || (plan.base.firstTurn && !ref.Newest) {
				continue
			}
			// A reference whose ordinals outgrow a chunk continues in the
			// next one (otelsql.PackRefChunks): one hit, all its ordinals.
			rid := refID{string(trace), string(span), ref.Attribute, id}
			if j, ok := at[rid]; ok {
				out[j].ordinals = append(out[j].ordinals, ref.Ordinals...)
				continue
			}
			at[rid] = len(out)
			out = append(out, relMessageRef{cand: c, key: relMessageRefKey{start, trace, span, messageAttributeKeys[ref.Attribute], id}, ordinals: ref.Ordinals})
		}
		return nil
	})
	return out, err
}

// relPipelinedRanges reads up to messageRefReads ranges concurrently,
// charging every row read and passing it to visit.
func relPipelinedRanges(ctx context.Context, tx fdb.ReadTransaction, ranges []fdb.KeyRange, work *queryWork, visit func(i int, kv fdb.KeyValue) error) error {
	const op = "message full-text query"
	for lo := 0; lo < len(ranges); lo += messageRefReads {
		if err := canceled(ctx, op); err != nil {
			return err
		}
		hi := min(lo+messageRefReads, len(ranges))
		// No range returns more rows than the budget has left.
		limit := max(work.maxCandidates-work.candidates, 0) + 1
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
				if err := work.charge(op, int64(len(kv.Key)+len(kv.Value))); err != nil {
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

// matchMessages returns the messages matching plan, by ID, with the role
// group each matched in: one TextScan per (role, shard), the shards of a
// role concurrently, and for "all" across parts one message-bounded scan
// per term of every multipart message. Segments overlap by
// messageSegmentOverlap tokens, so phrase and any matches are per part.
func matchMessages(ctx context.Context, store *recordlayer.FDBRecordStore, plan *messageQueryPlan, work *queryWork) ([]*relMessageCandidate, error) {
	const op = "message full-text query"
	idx, err := readableIndex(store, op, messagesTextIndex)
	if err != nil {
		return nil, err
	}
	roles := []string{plan.role}
	if plan.role == "" {
		if roles, err = messageTextRoles(ctx, store, idx, work); err != nil {
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
	var out []*relMessageCandidate
	for _, role := range roles {
		ids, err := shardTextMatches(ctx, store, idx, role, typ, tokens, work)
		if err != nil {
			return nil, err
		}
		if plan.mode == MessageMatchAll && len(plan.terms) > 1 {
			spread, err := spreadAllMatches(ctx, store, idx, role, plan.terms, work)
			if err != nil {
				return nil, err
			}
			ids = append(ids, spread...)
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				out = append(out, &relMessageCandidate{id: id, role: role})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id.less(out[j].id) })
	return out, nil
}

// limitedScan sets props' scan limiter one past the budget's remaining
// work, so a scan over the budget is seen, not cut short.
func limitedScan(props recordlayer.ScanProperties, work *queryWork) (recordlayer.ScanProperties, *recordlayer.ScanLimiterState) {
	props.ExecuteProperties.ScannedRecordsLimit = max(work.maxCandidates-work.candidates, 0) + 1
	props.ExecuteProperties.ScannedBytesLimit = max(work.maxBytes-work.bytes, 0) + 1
	state := recordlayer.NewScanLimiterState()
	props.ExecuteProperties.ScanState = state
	return props, state
}

// messageTextRoles lists the roles of MESSAGES_TEXT's groups by one
// positioned token scan per role, each skipping the role's (role, shard)
// groups.
func messageTextRoles(ctx context.Context, store *recordlayer.FDBRecordStore, idx *recordlayer.Index, work *queryWork) ([]string, error) {
	const op = "message roles"
	var out []string
	rng := recordlayer.TupleRangeAll
	for {
		props := recordlayer.ForwardScan()
		props.ExecuteProperties.ReturnedRowLimit = 1
		props, state := limitedScan(props, work)
		cur := store.ScanIndexByType(idx, recordlayer.IndexScanByTextToken, rng, nil, props)
		r, err := cur.OnNext(ctx)
		cur.Close()
		if err != nil {
			return nil, err
		}
		work.candidates += state.RecordsScanned()
		work.bytes += state.BytesScanned()
		if err := work.check(op); err != nil {
			return nil, err
		}
		if !r.HasNext() {
			if reason := r.GetNoNextReason(); reason != recordlayer.SourceExhausted && reason != recordlayer.ReturnLimitReached {
				return nil, failf("budget", op, "text scan stopped: %v", reason)
			}
			return out, nil
		}
		e := r.GetValue()
		role, ok := "", len(e.Key) > 0
		if ok {
			role, ok = e.Key[0].(string)
		}
		if !ok {
			return nil, fail("corruption", op, "malformed message text index entry")
		}
		out = append(out, role)
		rng = recordlayer.TupleRange{Low: tuple.Tuple{role}, LowEndpoint: recordlayer.EndpointTypeRangeExclusive, HighEndpoint: recordlayer.EndpointTypeTreeEnd}
	}
}

// textScanMatches runs one TextScan and returns the IDs of the messages its
// entries name, charging its reads to work.
func textScanMatches(ctx context.Context, store *recordlayer.FDBRecordStore, scan *recordlayer.TextScan, limit int, work *queryWork) ([]messageID, error) {
	const op = "message full-text query"
	props := recordlayer.ForwardScan()
	props.ExecuteProperties.ReturnedRowLimit = limit
	props, state := limitedScan(props, work)
	cur, err := scan.Scan(store, nil, props)
	if err != nil {
		return nil, typedError("config", op, "text scan", err)
	}
	defer cur.Close()
	md := store.GetRecordMetaData()
	var out []messageID
	for {
		r, err := cur.OnNext(ctx)
		if err != nil {
			return nil, err
		}
		if !r.HasNext() {
			work.candidates += state.RecordsScanned()
			work.bytes += state.BytesScanned()
			if reason := r.GetNoNextReason(); reason != recordlayer.SourceExhausted && reason != recordlayer.ReturnLimitReached {
				return nil, failf("budget", op, "text scan stopped: %v; candidates=%d logical_bytes=%d", reason, work.candidates, work.bytes)
			}
			return out, work.check(op)
		}
		id, _, err := messagePK(md, op, r.GetValue().PrimaryKey())
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
}

// shardTextMatches unions the role's 16 shard scans, run concurrently; each
// may use the remaining budget, and their sum must fit it too.
func shardTextMatches(ctx context.Context, store *recordlayer.FDBRecordStore, idx *recordlayer.Index, role string, typ recordlayer.TextComparisonType, tokens []string, work *queryWork) ([]messageID, error) {
	const op = "message full-text query"
	found := make([][]messageID, messageTextShards)
	subs := make([]queryWork, messageTextShards)
	errs := make([]error, messageTextShards)
	var wg sync.WaitGroup
	for shard := range messageTextShards {
		subs[shard] = queryWork{maxCandidates: work.maxCandidates - work.candidates, maxBytes: work.maxBytes - work.bytes}
		wg.Add(1)
		go func() {
			defer wg.Done()
			scan := recordlayer.NewTextScan(idx, tuple.Tuple{role, int64(shard)}, recordlayer.NewTextComparisonTokens(typ, tokens, "", ""), nil)
			found[shard], errs[shard] = textScanMatches(ctx, store, scan, 0, &subs[shard])
		}()
	}
	wg.Wait()
	for _, s := range subs {
		work.candidates += s.candidates
		work.bytes += s.bytes
	}
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	if err := work.check(op); err != nil {
		return nil, err
	}
	var ids []messageID
	for _, f := range found {
		ids = append(ids, f...)
	}
	return ids, nil
}

// spreadAllMatches finds the multipart messages of role holding every term
// in some part: one scan of MESSAGES_LARGE, then per message one scan per
// term bounded to the message's entries.
func spreadAllMatches(ctx context.Context, store *recordlayer.FDBRecordStore, text *recordlayer.Index, role string, terms []string, work *queryWork) ([]messageID, error) {
	const op = "message full-text query"
	large, err := readableIndex(store, op, messagesLargeIndex)
	if err != nil {
		return nil, err
	}
	md := store.GetRecordMetaData()
	props, state := limitedScan(recordlayer.ForwardScan(), work)
	cur := store.ScanIndex(large, recordlayer.TupleRangeAllOf(tuple.Tuple{int64(1)}), nil, props)
	defer cur.Close()
	var out []messageID
	var scanned int
	var scannedBytes int64
	for {
		r, err := cur.OnNext(ctx)
		work.candidates += state.RecordsScanned() - scanned
		work.bytes += state.BytesScanned() - scannedBytes
		scanned, scannedBytes = state.RecordsScanned(), state.BytesScanned()
		if err != nil {
			return nil, err
		}
		if err := work.check(op); err != nil {
			return nil, err
		}
		if !r.HasNext() {
			if reason := r.GetNoNextReason(); reason != recordlayer.SourceExhausted {
				return nil, failf("budget", op, "large message scan stopped: %v; candidates=%d logical_bytes=%d", reason, work.candidates, work.bytes)
			}
			return out, nil
		}
		id, _, err := messagePK(md, op, r.GetValue().PrimaryKey())
		if err != nil {
			return nil, err
		}
		// The TEXT entries after the token are the message's primary key.
		prefix, err := tableKey(store, op, messagesRecordType, id.writer, id.seq)
		if err != nil {
			return nil, err
		}
		within := recordlayer.TupleRangeAllOf(prefix)
		group := tuple.Tuple{role, otelsql.MessageShard(id.writer)}
		all := true
		for _, t := range terms {
			cmp := recordlayer.NewTextComparisonTokens(recordlayer.TextContainsAll, []string{t}, "", "")
			found, err := textScanMatches(ctx, store, recordlayer.NewTextScan(text, group, cmp, &within), 1, work)
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

// QueryMessageFullText searches message text; the exporter must index it.
func (e *relExporter) QueryMessageFullText(ctx context.Context, q MessageFullTextQuery) (MessageFullTextPage, error) {
	const op = "message full-text query"
	empty := MessageFullTextPage{Hits: []MessageSpanHit{}, Order: "time"}
	if !e.cfg.EnableMessageFullText {
		return empty, fail("config", op, "enable_message_full_text is off; message text is not indexed")
	}
	if err := e.begin(op); err != nil {
		return empty, err
	}
	defer e.active.Done()
	return e.reader.QueryMessageFullText(e.txnContext(ctx), q)
}

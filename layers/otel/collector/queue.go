package collector

import (
	"context"
	"errors"
	"math"
	"slices"
	"time"

	"fdb.dev/layers/otel"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

type request struct {
	seq   uint64
	spans []*tracepb.ResourceSpans
	count int
}

type batch struct {
	parts []request
	count int
}

func (b batch) resourceSpans() []*tracepb.ResourceSpans {
	var out []*tracepb.ResourceSpans
	for _, p := range b.parts {
		out = append(out, p.spans...)
	}
	return out
}

// ConsumeTraces rejects invalid identities synchronously and takes ownership of admitted spans.
// Queued calls return on admission; Flush and Stats report export failures.
func (x *TracesExporter) ConsumeTraces(ctx context.Context, spans []*tracepb.ResourceSpans) error {
	n, err := admitSpans(spans)
	if err != nil {
		x.reject(n)
		return err
	}
	if n == 0 {
		return nil
	}
	if !x.cfg.SendingQueue.Enabled {
		return x.exportSync(ctx, spans, n)
	}
	return x.enqueue(ctx, spans, n)
}

func (x *TracesExporter) reject(n int) {
	x.mu.Lock()
	x.stats.RejectedSpans += int64(n)
	x.mu.Unlock()
}

// admitSpans counts non-nil spans and applies the exporter's identity checks.
func admitSpans(in []*tracepb.ResourceSpans) (int, error) {
	n := 0
	var err error
	for _, rs := range in {
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				if sp == nil {
					continue
				}
				n++
				switch {
				case err != nil:
				case !nonzeroID(sp.TraceId, 16) || !nonzeroID(sp.SpanId, 8):
					err = permanent("span %d: expected nonzero 16-byte trace and 8-byte span identifiers", n)
				case sp.StartTimeUnixNano > math.MaxInt64:
					err = permanent("span %d: start_time_unix_nano exceeds int64", n)
				}
			}
		}
	}
	return n, err
}

func nonzeroID(b []byte, size int) bool {
	return len(b) == size && slices.ContainsFunc(b, func(c byte) bool { return c != 0 })
}

func (x *TracesExporter) exportSync(ctx context.Context, spans []*tracepb.ResourceSpans, n int) error {
	x.mu.Lock()
	if x.state != stateStarted {
		x.stats.RejectedSpans += int64(n)
		err := &NotRunningError{Operation: "export", State: x.state}
		x.mu.Unlock()
		return err
	}
	x.active.Add(1)
	seq := x.nextSeq
	x.nextSeq++
	x.outstanding += n
	x.remaining[seq] = n
	x.order = append(x.order, seq)
	x.stats.AcceptedRequests++
	x.stats.AcceptedSpans += int64(n)
	x.mu.Unlock()
	defer x.active.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(x.workCtx, cancel)
	defer stop()
	st, err := x.exportWithRetry(ctx, spans)
	x.complete([]request{{seq: seq, count: n}}, st, err)
	return err
}

func (x *TracesExporter) enqueue(ctx context.Context, spans []*tracepb.ResourceSpans, n int) error {
	q := x.cfg.SendingQueue
	if n > q.QueueSize {
		x.reject(n)
		return permanent("request of %d spans exceeds sending_queue.queue_size %d", n, q.QueueSize)
	}
	x.mu.Lock()
	for {
		if x.state != stateStarted {
			x.stats.RejectedSpans += int64(n)
			err := &NotRunningError{Operation: "enqueue", State: x.state}
			x.mu.Unlock()
			return err
		}
		if x.outstanding+n <= q.QueueSize {
			break
		}
		if !q.BlockOnOverflow {
			x.stats.RejectedSpans += int64(n)
			err := &QueueFullError{Capacity: q.QueueSize, Queued: x.outstanding, Requested: n}
			x.mu.Unlock()
			return err
		}
		wait := x.changed
		queued := x.outstanding
		x.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			x.reject(n)
			return &QueueFullError{Capacity: q.QueueSize, Queued: queued, Requested: n, Cause: ctx.Err()}
		}
		x.mu.Lock()
	}
	seq := x.nextSeq
	x.nextSeq++
	x.outstanding += n
	x.remaining[seq] = n
	x.order = append(x.order, seq)
	x.pending = append(x.pending, request{seq: seq, spans: spans, count: n})
	x.stats.AcceptedRequests++
	x.stats.AcceptedSpans += int64(n)
	x.mu.Unlock()
	x.signal()
	return nil
}

func (x *TracesExporter) signal() {
	select {
	case x.wake <- struct{}{}:
	default:
	}
}

func (x *TracesExporter) batchLoop() {
	defer x.workers.Done()
	defer close(x.batches)
	q := x.cfg.SendingQueue
	timer := time.NewTimer(q.Batch.FlushTimeout)
	timer.Stop()
	armed, expired := false, false
	var cur []request
	count := 0
	for {
		x.mu.Lock()
		for _, r := range x.pending {
			count += r.count
		}
		cur = append(cur, x.pending...)
		x.pending = nil
		stopping := x.state != stateStarted
		force := expired || x.flushWanted || stopping
		x.flushWanted, expired = false, false
		x.mu.Unlock()
		dispatched := false
		for count > 0 && (count >= q.Batch.MinSize || force) {
			var b batch
			before := cur
			b, cur, count = cutBatch(cur, count, q.Batch.MaxSize)
			if x.batchCutProbe != nil {
				x.batchCutProbe(before, cur)
			}
			x.dispatch(b)
			dispatched = true
		}
		if dispatched || count == 0 {
			timer.Stop()
			armed = false
		}
		if count > 0 && !armed {
			timer.Reset(q.Batch.FlushTimeout)
			armed = true
		}
		if stopping && count == 0 {
			return
		}
		select {
		case <-x.wake:
		case <-timer.C:
			armed, expired = false, true
		}
	}
}

func (x *TracesExporter) dispatch(b batch) {
	x.mu.Lock()
	x.stats.Flushes++
	x.mu.Unlock()
	select {
	case x.batches <- b:
	case <-x.workCtx.Done():
		x.mu.Lock()
		x.stats.Flushes--
		x.mu.Unlock()
		x.complete(b.parts, otel.ExportStats{}, componentErrorf("%s: export abandoned: %w", ComponentType, x.workCtx.Err()))
	}
}

// cutBatch takes up to max spans from the front of cur, splitting a request
// at a span boundary when needed.
func cutBatch(cur []request, count, max int) (batch, []request, int) {
	take := min(count, max)
	b := batch{count: take}
	for take > 0 {
		r := cur[0]
		if r.count <= take {
			b.parts = append(b.parts, r)
			take -= r.count
			cur[0] = request{}
			cur = cur[1:]
			continue
		}
		head, tail := splitSpans(r.spans, take)
		b.parts = append(b.parts, request{seq: r.seq, spans: head, count: take})
		cur[0] = request{seq: r.seq, spans: tail, count: r.count - take}
		take = 0
	}
	if len(cur) == 0 {
		cur = nil
	}
	return b, cur, count - b.count
}

// splitSpans moves the first n non-nil spans into head, sharing resources and
// scopes and keeping wrapper unknown fields on both sides.
func splitSpans(in []*tracepb.ResourceSpans, n int) (head, tail []*tracepb.ResourceSpans) {
	for _, rs := range in {
		if rs == nil {
			continue
		}
		var h, t *tracepb.ResourceSpans
		for _, ss := range rs.ScopeSpans {
			if ss == nil {
				continue
			}
			var hs, ts []*tracepb.Span
			for _, sp := range ss.Spans {
				switch {
				case sp == nil:
				case n > 0:
					hs = append(hs, sp)
					n--
				default:
					ts = append(ts, sp)
				}
			}
			if len(hs) > 0 {
				if h == nil {
					h = shallowResourceSpans(rs)
				}
				h.ScopeSpans = append(h.ScopeSpans, shallowScopeSpans(ss, hs))
			}
			if len(ts) > 0 {
				if t == nil {
					t = shallowResourceSpans(rs)
				}
				t.ScopeSpans = append(t.ScopeSpans, shallowScopeSpans(ss, ts))
			}
		}
		if h != nil {
			head = append(head, h)
		}
		if t != nil {
			tail = append(tail, t)
		}
	}
	return head, tail
}

func shallowResourceSpans(rs *tracepb.ResourceSpans) *tracepb.ResourceSpans {
	out := &tracepb.ResourceSpans{Resource: rs.Resource, SchemaUrl: rs.SchemaUrl}
	out.ProtoReflect().SetUnknown(rs.ProtoReflect().GetUnknown())
	return out
}

func shallowScopeSpans(ss *tracepb.ScopeSpans, spans []*tracepb.Span) *tracepb.ScopeSpans {
	out := &tracepb.ScopeSpans{Scope: ss.Scope, SchemaUrl: ss.SchemaUrl, Spans: spans}
	out.ProtoReflect().SetUnknown(ss.ProtoReflect().GetUnknown())
	return out
}

func (x *TracesExporter) consumeLoop() {
	defer x.workers.Done()
	for b := range x.batches {
		st, err := x.exportWithRetry(x.workCtx, b.resourceSpans())
		if err == nil || !IsPermanent(err) || len(b.parts) == 1 {
			x.complete(b.parts, st, err)
			continue
		}
		// Messages or span batches may already be committed after admission.
		// Replay sources idempotently to isolate the offending request.
		for _, p := range b.parts {
			if cerr := x.workCtx.Err(); cerr != nil {
				// Abandoned before its own attempt: the merged failure may
				// belong to another part, so it is reported, not wrapped.
				x.complete([]request{p}, otel.ExportStats{}, componentErrorf("%s: export abandoned after its merged batch failed (%v): %w", ComponentType, err, cerr))
				continue
			}
			st, perr := x.exportWithRetry(x.workCtx, p.spans)
			x.complete([]request{p}, st, perr)
		}
	}
}

func (x *TracesExporter) exportWithRetry(ctx context.Context, spans []*tracepb.ResourceSpans) (otel.ExportStats, error) {
	r := x.cfg.RetryOnFailure
	started := time.Now()
	interval := r.InitialInterval
	for {
		attempt, cancel := context.WithTimeout(ctx, x.cfg.Timeout)
		st, err := x.sink.ExportResourceSpans(attempt, spans)
		cancel()
		x.mu.Lock()
		x.stats.Exports++
		x.mu.Unlock()
		if err == nil {
			return st, nil
		}
		if ctx.Err() != nil {
			return st, abandonedExport(ctx, err)
		}
		if !r.Enabled || IsPermanent(err) {
			return st, err
		}
		if r.MaxElapsedTime > 0 && time.Since(started)+interval > r.MaxElapsedTime {
			return st, componentErrorf("%s: retries exhausted after %s: %w", ComponentType, time.Since(started).Round(time.Millisecond), err)
		}
		x.mu.Lock()
		x.stats.Retries++
		x.mu.Unlock()
		wait := time.NewTimer(interval)
		select {
		case <-wait.C:
		case <-ctx.Done():
			wait.Stop()
			return st, abandonedExport(ctx, err)
		}
		interval = min(2*interval, r.MaxInterval)
	}
}

// abandonedExport retains both the failed attempt and worker cancellation,
// whether cancellation arrived during the attempt or backoff.
func abandonedExport(ctx context.Context, err error) error {
	if errors.Is(err, ctx.Err()) {
		return err
	}
	return errors.Join(err, ctx.Err())
}

// record updates counters for one export outcome; callers hold x.mu.
func (x *TracesExporter) record(st otel.ExportStats, spans int, err error) {
	x.stats.StoredSpans += st.Spans
	x.stats.DuplicateSpans += st.DuplicateSpans
	if err != nil {
		x.stats.FailedSpans += int64(spans)
	} else {
		x.stats.ExportedSpans += int64(spans)
	}
}

func (x *TracesExporter) complete(parts []request, st otel.ExportStats, err error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	spans := 0
	for _, p := range parts {
		spans += p.count
		x.outstanding -= p.count
		if x.remaining[p.seq] -= p.count; x.remaining[p.seq] <= 0 {
			delete(x.remaining, p.seq)
		}
	}
	for len(x.order) > 0 {
		if _, open := x.remaining[x.order[0]]; open {
			break
		}
		x.order = x.order[1:]
	}
	x.record(st, spans, err)
	if err != nil {
		if len(x.failures) < maxFailures {
			x.failures = append(x.failures, componentErrorf("%d spans: %w", spans, err))
		} else {
			x.dropped++
		}
	}
	close(x.changed)
	x.changed = make(chan struct{})
}

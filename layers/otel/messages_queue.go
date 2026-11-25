package otel

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// Writers
// ---------------------------------------------------------------------------

// messageWriter numbers the messages its holder stores. A writer is held by
// one transaction at a time, so its seqs are stored in increasing order.
type messageWriter struct {
	id   int64
	next int64 // the last seq handed out
}

// messageWriters limits boundary-bunch rewrites: fewer writers reach past
// the next writer’s first posting sooner.
type messageWriters struct {
	once   sync.Once
	slots  chan struct{}
	mu     sync.Mutex
	free   []*messageWriter
	shards [messageTextShards]bool // shards of this exporter's writers
}

// nextWriterID is the first ID above every allocated one (last) in this
// exporter's free shard.
func nextWriterID(last, shard int64) int64 {
	return last + 1 + ((shard-(last+1))%messageTextShards+messageTextShards)%messageTextShards
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

// messageQueue groups small messages into size/count-bounded transactions on the exporter's writers,
// amortizing read-version, digest, TEXT bunch and commit costs across waiting calls.
type messageQueue struct {
	mu      sync.Mutex
	pending []queuedMessage
	loops   int
	closed  bool
	workers sync.WaitGroup
}

type queuedMessage struct {
	plan *messagePlan
	sub  *messageSubmission
}

// messageSubmission is one call's queued messages.
type messageSubmission struct {
	left  int // queued messages not yet stored or dropped
	err   error
	stats MessageStoreStats
	done  chan struct{}
}

// close rejects new submissions and joins workers without canceling shared transactions.
func (q *messageQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.workers.Wait()
}

// take removes the next transaction's messages, skipping those of failed
// submissions; with none queued the loop ends.
func (q *messageQueue) take() []queuedMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []queuedMessage
	var raw int64
	n := 0
	for ; n < len(q.pending) && len(out) < messageTxnMessages; n++ {
		m := q.pending[n]
		if m.sub.err != nil {
			q.pending[n] = queuedMessage{}
			q.settleLocked(m.sub)
			continue
		}
		if len(out) > 0 && raw+m.plan.raw > messagePartRawBytes {
			break
		}
		out = append(out, m)
		q.pending[n] = queuedMessage{}
		raw += m.plan.raw
	}
	q.pending = q.pending[n:]
	if len(q.pending) == 0 {
		q.pending = nil
	}
	if len(out) == 0 {
		q.loops--
	}
	return out
}

// settleLocked counts one of sub's messages as done; callers hold q.mu.
func (q *messageQueue) settleLocked(sub *messageSubmission) {
	if sub.left--; sub.left == 0 && sub.err == nil {
		close(sub.done)
	}
}

// finish charges per-message counters to their submissions; the first message’s
// submission receives shared transaction, retry, TEXT-work and time counters.
func (q *messageQueue) finish(batch []queuedMessage, stats MessageStoreStats, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	txn := stats
	txn.Written, txn.Present, txn.Parts, txn.Postings, txn.UnindexedTokens = 0, 0, 0, 0, 0
	for i, m := range batch {
		s := &m.sub.stats
		if i == 0 {
			s.Add(txn)
		}
		s.Parts += m.plan.element.outcome.Parts
		s.Postings += m.plan.element.outcome.Postings
		s.UnindexedTokens += m.plan.element.outcome.UnindexedTokens
		s.Written += m.plan.element.outcome.Written
		s.Present += m.plan.element.outcome.Present
		if err != nil {
			if m.sub.err == nil {
				m.sub.err = err
				close(m.sub.done)
			}
			m.sub.left--
			continue
		}
		q.settleLocked(m.sub)
	}
}

// runMessageJobs runs independent jobs on at most messageWorkers goroutines;
// the first failure cancels remaining work and is returned.
func runMessageJobs(ctx context.Context, op string, n int, job func(context.Context, int) error) error {
	switch n {
	case 0:
		return nil
	case 1:
		return job(ctx, 0)
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		firstErr error
		done     int
		next     atomic.Int64
		workers  sync.WaitGroup
	)
	for range min(messageWorkers, n) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= n || workCtx.Err() != nil {
					return
				}
				err := job(workCtx, i)
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
						cancel()
					}
				} else {
					done++
				}
				mu.Unlock()
			}
		}()
	}
	workers.Wait()
	if firstErr != nil {
		return firstErr
	}
	if done != n {
		return wrap("canceled", op, ctx.Err())
	}
	return nil
}

// parallelMessageWork runs CPU-only fn(i) for i in [0, n) on at most
// messageWorkers goroutines and returns the error of the lowest failing index.
func parallelMessageWork(n int, fn func(int) error) error {
	errs := make([]error, n)
	var next atomic.Int64
	var failed atomic.Bool
	var workers sync.WaitGroup
	for range min(messageWorkers, n) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for !failed.Load() {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				if errs[i] = fn(i); errs[i] != nil {
					failed.Store(true)
				}
			}
		}()
	}
	workers.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Span back references
// ---------------------------------------------------------------------------

// traceMarker is a MESSAGE_TRACES row: message id occurs in trace.
type traceMarker struct {
	id    messageID
	trace string
}

// markerCache remembers the markers this exporter knows to be committed; a run
// never deletes one. It holds at most markerCacheSize, forgetting the oldest.
type markerCache struct {
	mu   sync.Mutex
	set  map[traceMarker]struct{}
	ring []traceMarker
	next int
}

const markerCacheSize = 1 << 18

func (c *markerCache) has(k traceMarker) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.set[k]
	return ok
}

func (c *markerCache) add(ks []traceMarker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set == nil {
		c.set = make(map[traceMarker]struct{}, markerCacheSize)
		c.ring = make([]traceMarker, 0, markerCacheSize)
	}
	for _, k := range ks {
		if _, ok := c.set[k]; ok {
			continue
		}
		if len(c.ring) < markerCacheSize {
			c.ring = append(c.ring, k)
		} else {
			delete(c.set, c.ring[c.next])
			c.ring[c.next] = k
			c.next = (c.next + 1) % markerCacheSize
		}
		c.set[k] = struct{}{}
	}
}

// messageMarkerTxn bounds the markers checked and saved per transaction.
const messageMarkerTxn = 512

// inflightMessage is a message one export call of this exporter is storing;
// other calls that need it wait for its ID instead of storing it again.
type inflightMessage struct {
	done chan struct{}
	id   messageID // valid once done, if the store succeeded
}

// ---------------------------------------------------------------------------
// Reconstruction
// ---------------------------------------------------------------------------

type storedSite struct {
	kv       *commonpb.KeyValue
	manifest *messageManifest
}

// reconstructPlan is the manifests of clones of decomposed spans and the
// messages they reference, in first-reference order.
type reconstructPlan struct {
	spans     []*tracepb.ResourceSpans
	sites     []storedSite
	refs      []messageID
	encodings map[messageID]string
}

func planReconstruct(ctx context.Context, spans []*tracepb.ResourceSpans) (*reconstructPlan, error) {
	const op = "message reconstruct"
	out := make([]*tracepb.ResourceSpans, len(spans))
	var sites []storedSite
	encodings := map[messageID]string{}
	var refs []messageID
	for i, rs := range spans {
		if err := canceled(ctx, op); err != nil {
			return nil, err
		}
		if rs == nil {
			continue
		}
		clone := proto.Clone(rs).(*tracepb.ResourceSpans)
		err := forEachMessageAttribute(clone, func(_ *tracepb.Span, kv *commonpb.KeyValue) error {
			if kv == nil {
				return nil
			}
			m, err := parseMessageManifest(kv.Value)
			if err != nil {
				return err
			}
			for _, id := range m.ids {
				if prev, ok := encodings[id]; ok {
					if prev != m.encoding() {
						return fail("corruption", op, "message referenced with two encodings")
					}
					continue
				}
				encodings[id] = m.encoding()
				refs = append(refs, id)
			}
			sites = append(sites, storedSite{kv, m})
			return nil
		})
		if err != nil {
			return nil, err
		}
		out[i] = clone
	}
	return &reconstructPlan{spans: out, sites: sites, refs: refs, encodings: encodings}, nil
}

// apply installs the original attribute values from the raw elements.
func (p *reconstructPlan) apply(raws map[messageID][]byte) ([]*tracepb.ResourceSpans, error) {
	const op = "message reconstruct"
	sites, out := p.sites, p.spans
	element := func(id messageID) (*commonpb.AnyValue, error) {
		v := &commonpb.AnyValue{}
		if err := proto.Unmarshal(raws[id], v); err != nil {
			return nil, typedError("corruption", op, "message element decode", err)
		}
		return v, nil
	}
	for _, s := range sites {
		m := s.manifest
		switch m.layout {
		case messageLayoutNone:
			s.kv.Value = nil
		case messageLayoutValue:
			v, err := element(m.ids[0])
			if err != nil {
				return nil, err
			}
			s.kv.Value = v
		case messageLayoutArray:
			arr := &commonpb.ArrayValue{Values: make([]*commonpb.AnyValue, len(m.ids))}
			for i, id := range m.ids {
				v, err := element(id)
				if err != nil {
					return nil, err
				}
				arr.Values[i] = v
			}
			s.kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: arr}}
		case messageLayoutJSON:
			var b strings.Builder
			b.Write(m.head)
			for i, id := range m.ids {
				if i > 0 {
					if m.seps == nil {
						b.WriteByte(',')
					} else {
						b.Write(m.seps[i-1])
					}
				}
				b.Write(raws[id])
			}
			b.Write(m.tail)
			s.kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: b.String()}}
		}
	}
	return out, nil
}

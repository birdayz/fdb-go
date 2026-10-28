package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

type ingestReplayBatchSnapshot struct {
	first, next  int
	estimate     int
	logicalBytes int64
	messages     []proto.Message
	wire         [][]byte
}

func snapshotIngestReplayBatch(batch preparedBatch) (ingestReplayBatchSnapshot, error) {
	out := ingestReplayBatchSnapshot{
		first: batch.first, next: batch.next, estimate: batch.estimate, logicalBytes: batch.logicalBytes,
		messages: append([]proto.Message(nil), batch.messages...),
	}
	for _, message := range batch.messages {
		wire, err := (proto.MarshalOptions{Deterministic: true}).Marshal(message)
		if err != nil {
			return out, err
		}
		out.wire = append(out.wire, append([]byte(nil), wire...))
	}
	return out, nil
}

func sameIngestReplayBatch(a, b ingestReplayBatchSnapshot) bool {
	if a.first != b.first || a.next != b.next || a.estimate != b.estimate || a.logicalBytes != b.logicalBytes || len(a.messages) != len(b.messages) || len(a.wire) != len(b.wire) {
		return false
	}
	for i := range a.messages {
		if a.messages[i] != b.messages[i] || !bytes.Equal(a.wire[i], b.wire[i]) {
			return false
		}
	}
	return true
}

type ingestReplaySizeSample struct {
	attempt int
	bytes   int64
	err     error
}

type ingestReplayState struct {
	mu               sync.Mutex
	prepared         []ingestReplayBatchSnapshot
	attempts         []ingestReplayBatchSnapshot
	sizes            []ingestReplaySizeSample
	closureEntries   int
	injected         int
	successfulBodies int
	rollbackObserved bool
	commits          []otelIngestCommit
	observerFailures int
	problems         []string
}

func (s *ingestReplayState) probe(event ingestEvent) {
	if event.kind != "prepared" && event.kind != "attempt" {
		return
	}
	snapshot, err := snapshotIngestReplayBatch(event.batch)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.problems = append(s.problems, "snapshot "+event.kind+": "+err.Error())
		return
	}
	if event.kind == "prepared" {
		s.prepared = append(s.prepared, snapshot)
		return
	}
	s.attempts = append(s.attempts, snapshot)
	if len(s.prepared) != 1 || !sameIngestReplayBatch(s.prepared[0], snapshot) {
		s.problems = append(s.problems, "attempt did not reuse the one immutable prepared batch")
	}
}

type ingestReplayTransactor struct {
	fdb.Database
	parent       context.Context
	state        *ingestReplayState
	beforeReplay func() error
}

type ingestReplayTransaction struct {
	fdb.WritableTransaction
	state   *ingestReplayState
	attempt int
}

type ingestReplaySizeFuture struct {
	fdb.FutureInt64
	state   *ingestReplayState
	attempt int
}

func (f ingestReplaySizeFuture) Get() (int64, error) {
	n, err := f.FutureInt64.Get()
	f.state.mu.Lock()
	f.state.sizes = append(f.state.sizes, ingestReplaySizeSample{attempt: f.attempt, bytes: n, err: err})
	f.state.mu.Unlock()
	return n, err
}

func (tx ingestReplayTransaction) GetApproximateSize() fdb.FutureInt64 {
	return ingestReplaySizeFuture{FutureInt64: tx.WritableTransaction.GetApproximateSize(), state: tx.state, attempt: tx.attempt}
}

func (d ingestReplayTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.TransactCtx(context.Background(), fn)
}

func (d ingestReplayTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		// Ingest uses its parent for initialization/ready publication, and a
		// derived worker context for source batches. Only source work replays.
		if ctx == d.parent {
			return fn(tx)
		}
		d.state.mu.Lock()
		d.state.closureEntries++
		attempt := d.state.closureEntries
		d.state.mu.Unlock()
		if attempt > 2 {
			return nil, fmt.Errorf("source replay exceeded exactly two closure entries: %d", attempt)
		}
		if attempt == 2 {
			if err := d.beforeReplay(); err != nil {
				return nil, err
			}
			d.state.mu.Lock()
			d.state.rollbackObserved = true
			d.state.mu.Unlock()
		}
		result, err := fn(ingestReplayTransaction{WritableTransaction: tx, state: d.state, attempt: attempt})
		if err != nil {
			return result, err
		}
		d.state.mu.Lock()
		successfulSamples := 0
		for _, sample := range d.state.sizes {
			if sample.attempt == attempt && sample.err == nil && sample.bytes > 0 && sample.bytes <= 500000 {
				successfulSamples++
			}
		}
		if successfulSamples != 1 {
			d.state.mu.Unlock()
			return nil, fmt.Errorf("source closure %d completed without exactly one real successful size guard: %d", attempt, successfulSamples)
		}
		if attempt == 1 {
			d.state.injected++
			d.state.mu.Unlock()
			// The real saves, secondary-index updates and size guard completed;
			// refuse this attempt before the native client can commit it.
			return nil, fdb.Error{Code: 1020}
		}
		d.state.successfulBodies++
		d.state.mu.Unlock()
		return result, nil
	})
}

func testOtelIngestReplay(t *testing.T, ctx context.Context, cluster, order string) {
	t.Helper()
	c := testConfig()
	c.Count, c.Tenants, c.Batch, c.Workers = 2, 1, 2, 1
	c.Order, c.SaveMode, c.Diagnostics = order, "batch", true
	b := openOtelFixture(t, ctx, cluster, c, false)
	c = b.cfg
	state := &ingestReplayState{}
	original := b.db
	b.db = recordlayer.NewFDBDatabaseWithTransactor(ingestReplayTransactor{Database: b.raw, parent: ctx, state: state, beforeReplay: func() error {
		commit, err := readOtelIngestCommit(ctx, b, recordlayer.AttemptCall{})
		if err != nil {
			return err
		}
		if commit.owner != manifest(c, "loading") || commit.recordKeys != 0 || commit.postingKeys != 0 {
			return fmt.Errorf("failed source attempt became durable before replay: %+v", commit)
		}
		_, err = b.raw.ReadTransactCtx(ctx, func(tx fdb.ReadTransaction) (any, error) {
			for _, name := range []string{spanIndexName, timeIndexName} {
				index := b.md.GetIndex(name)
				if index == nil {
					return nil, fmt.Errorf("missing maintained intrinsic index %s", name)
				}
				prefix := b.store.Sub(recordlayer.IndexKey, index.SubspaceTupleKey())
				entries, err := tx.GetRange(prefix, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				if len(entries) != 0 {
					return nil, fmt.Errorf("failed source attempt left durable %s entries before replay", name)
				}
			}
			return nil, nil
		})
		return err
	}}, b.raw)
	b.ingestProbe = state.probe
	defer func() {
		b.ingestProbe = nil
		b.db.SetAttemptObserver(nil)
		b.db = original
	}()
	b.db.SetAttemptObserver(func(call recordlayer.AttemptCall, attemptErr error) {
		if attemptErr != nil {
			state.mu.Lock()
			state.observerFailures++
			state.mu.Unlock()
			return
		}
		commit, err := readOtelIngestCommit(ctx, b, call)
		state.mu.Lock()
		defer state.mu.Unlock()
		if err != nil {
			state.problems = append(state.problems, "inspect successful attempt: "+err.Error())
			return
		}
		state.commits = append(state.commits, commit)
	})
	seedOtelProfileClientMetrics(t, ctx, b)
	metricsBefore := b.raw.Metrics()
	stats, err := b.Ingest(ctx)
	metricsAfter := b.raw.Metrics()
	b.ingestProbe = nil
	b.db.SetAttemptObserver(nil)
	b.db = original
	assertOtelProfileClientMetrics(t, stats, metricsBefore, metricsAfter)
	if err != nil {
		t.Fatalf("public ingestion replay failed: %v", err)
	}
	// Ingest has joined its worker, so all probe/future/observer writes ended.
	if len(state.problems) != 0 || len(state.prepared) != 1 || len(state.attempts) != 2 || state.closureEntries != 2 || state.injected != 1 || state.successfulBodies != 1 || !state.rollbackObserved {
		t.Fatalf("replay population: prepared=%d probes=%d closures=%d injected=%d successful=%d rollbackObserved=%t problems=%v", len(state.prepared), len(state.attempts), state.closureEntries, state.injected, state.successfulBodies, state.rollbackObserved, state.problems)
	}
	prepared := state.prepared[0]
	if prepared.first != 0 || prepared.next != 2 || len(prepared.messages) != 2 || prepared.estimate <= 0 || prepared.estimate > 300000 || prepared.logicalBytes <= 0 {
		t.Fatalf("invalid immutable prepared bounds: %+v", prepared)
	}
	for i, attempt := range state.attempts {
		if !sameIngestReplayBatch(prepared, attempt) {
			t.Fatalf("attempt %d changed message identities, wire bytes or batch bounds", i+1)
		}
	}
	finalPrepared, err := snapshotIngestReplayBatch(preparedBatch{first: prepared.first, next: prepared.next, messages: prepared.messages, logicalBytes: prepared.logicalBytes, estimate: prepared.estimate})
	if err != nil || !sameIngestReplayBatch(prepared, finalPrepared) {
		t.Fatalf("save attempts mutated prepared messages after probe entry: %v", err)
	}
	if len(state.sizes) != 2 {
		t.Fatalf("want both actual size samples, including the failed attempt: %+v", state.sizes)
	}
	var sampledBytes int64
	for i, sample := range state.sizes {
		if sample.attempt != i+1 || sample.err != nil || sample.bytes <= 0 || sample.bytes > 500000 {
			t.Fatalf("actual source size sample %d: %+v", i, sample)
		}
		sampledBytes += sample.bytes
	}
	d := stats.Diagnostics
	if d == nil || d.Scope != "source_batches_only" || d.SourceRuns != 1 || d.SourceAttempts != 2 || d.SourceReplays != 1 || d.CommittedBatches != 1 || d.CommittedRows != 2 || d.ApproximateMutationBytes != sampledBytes {
		t.Fatalf("diagnostics lost replay/source-only/sample accounting: %+v want mutation bytes=%d", d, sampledBytes)
	}
	assertOtelProfileMoments(t, stats, map[string]int64{
		"preparation": int64(len(state.prepared)),
		"manifest":    2, "open": 2, "save": 2, "guard": 2,
		"run": 1, "timeout_backoff": 0, "commit": 1,
	})
	phases := []string{"preparation", "manifest", "open", "save", "guard", "run"}
	if len(d.PhaseNanos) != len(phases) {
		t.Fatalf("unexpected measured phases: %v", d.PhaseNanos)
	}
	for _, phase := range phases {
		if d.PhaseNanos[phase] <= 0 {
			t.Fatalf("missing/nonpositive phase %s: %v", phase, d.PhaseNanos)
		}
	}
	for _, expected := range []struct {
		event recordlayer.Event
		count int64
	}{{recordlayer.EventCommit, 1}, {recordlayer.EventOpenStore, 2}} {
		counter := d.Timer[expected.event.Name]
		if counter == nil || counter.Event != expected.event || counter.Count != expected.count || counter.CumulativeValue <= 0 {
			t.Fatalf("source-scoped native timer %s: %+v want count=%d", expected.event.Name, counter, expected.count)
		}
	}
	// SaveRecordBatch does not emit scalar save timers/counters. Do not turn
	// their absence into invented measurements of the work replayed above.
	for _, event := range []recordlayer.Event{recordlayer.EventSaveRecord, recordlayer.CountSaveRecordKey, recordlayer.CountSaveRecordKeyBytes, recordlayer.CountSaveRecordValueBytes} {
		if counter := d.Timer[event.Name]; counter != nil && (counter.Count != 0 || counter.CumulativeValue != 0) {
			t.Fatalf("batch unexpectedly reported scalar save timer/counter %s: %+v", event.Name, counter)
		}
	}
	// The native client may replay inside one TransactCtx, hiding the failed
	// entry from the outer observer. Durable successful populations are fixed.
	if len(state.commits) != 3 {
		t.Fatalf("observer successful commits=%+v, failed outer attempts=%d", state.commits, state.observerFailures)
	}
	seenCalls := map[uint64]bool{}
	for i, commit := range state.commits {
		wantRows, wantState := 2, "loading"
		if i == 0 {
			wantRows = 0
		}
		if i == 2 {
			wantState = "ready"
		}
		if commit.call.CallID == 0 || seenCalls[commit.call.CallID] || commit.call.Owner != "run" || commit.call.Route != recordlayer.RouteAttempt || commit.owner != manifest(c, wantState) || commit.recordKeys != wantRows || commit.postingKeys != wantRows*c.Attributes {
			t.Fatalf("successful publication %d: %+v want state=%s rows=%d", i, commit, wantState, wantRows)
		}
		seenCalls[commit.call.CallID] = true
	}
	sources := []Span{Generate(c, 0), Generate(c, 1)}
	var logicalBytes int64
	wantEstimate := 0
	for i, source := range sources {
		assertOtelSource(t, ctx, b, source)
		message, err := toRecord(source)
		if err != nil || !proto.Equal(prepared.messages[i], message) {
			t.Fatalf("prepared source %d did not match deterministic fixture: %v", i, err)
		}
		wire, err := json.Marshal(source)
		if err != nil {
			t.Fatal(err)
		}
		logicalBytes += int64(len(wire))
		wantEstimate += 2048
		for _, attribute := range source.Attributes {
			wantEstimate += 2 * (len(attribute.Key) + len(attribute.Value) + 256)
		}
	}
	if prepared.logicalBytes != logicalBytes || prepared.estimate != wantEstimate || stats.Rows != 2 || stats.Attributes != int64(2*c.Attributes) || stats.LogicalPayloadBytes != logicalBytes || stats.Details["committed_batches"] != "1" || stats.Details["save_mode"] != "batch" || stats.Details["workers"] != "1" {
		t.Fatalf("replay duplicated or changed acknowledged source work: batch=%+v stats=%+v want logical=%d estimate=%d", prepared, stats, logicalBytes, wantEstimate)
	}
	assertOtelCommittedPostings(t, ctx, b, sources)
	audit, err := b.Audit(ctx)
	if err != nil || audit.Rows != 2 || audit.Attributes != int64(2*c.Attributes) || audit.Structures["eq_secondary"].Keys != int64(2*c.Attributes) || audit.Structures[spanIndexName].Keys != 2 || audit.Structures[timeIndexName].Keys != 2 {
		t.Fatalf("replayed source/index census: %+v %v", audit, err)
	}
	if err := AuditQueries(ctx, b, c); err != nil {
		t.Fatalf("replayed public query/source oracle audit: %v", err)
	}
}

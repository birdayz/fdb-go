package otel

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	wiretypes "fdb.dev/pkg/fdbgo/wire/types"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// CaptureWireLog is process-global; both layout fixtures share this lock.
var ingestPrefetchCaptureMu sync.Mutex

type ingestPrefetchWireLog struct {
	mu     sync.Mutex
	data   bytes.Buffer
	starts []int
}

func (l *ingestPrefetchWireLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.data.Write(p)
}

func (l *ingestPrefetchWireLog) begin() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.starts = append(l.starts, l.data.Len())
}

func ingestPrefetchCommit(t *testing.T, b *FDBBackend, run func() error, injectedReadKey ...fdb.Key) wiretypes.CommitTransactionRef {
	t.Helper()
	ingestPrefetchCaptureMu.Lock()
	defer ingestPrefetchCaptureMu.Unlock()
	var log ingestPrefetchWireLog
	previousProbe := b.ingestProbe
	b.ingestProbe = func(event ingestEvent) {
		if event.kind == "attempt" {
			log.begin()
		}
		if previousProbe != nil {
			previousProbe(event)
		}
	}
	defer func() { b.ingestProbe = previousProbe }()
	stop := transport.CaptureWireLog(&log)
	defer stop()
	err := run()
	stop()
	if err != nil {
		t.Fatal(err)
	}
	if len(log.starts) == 0 {
		t.Fatal("wire capture has no source-attempt markers")
	}
	input := bytes.NewReader(log.data.Bytes())
	type capturedCommit struct {
		request wiretypes.CommitTransactionRequest
		attempt int
	}
	var requests []capturedCommit
	replies := map[[16]byte][]byte{}
	for {
		offset := log.data.Len() - input.Len()
		attempt := 0
		for i, start := range log.starts {
			if start <= offset {
				attempt = i + 1
			}
		}
		var header [29]byte
		if _, err := io.ReadFull(input, header[:]); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		n := binary.LittleEndian.Uint32(header[25:29])
		if uint64(n) > uint64(input.Len()) {
			t.Fatalf("truncated wire capture: body=%d remaining=%d", n, input.Len())
		}
		body := make([]byte, int(n))
		if _, err := io.ReadFull(input, body); err != nil {
			t.Fatal(err)
		}
		if header[0] == 'R' {
			var token [16]byte
			copy(token[:], header[9:25])
			replies[token] = body
			continue
		}
		if header[0] != 'S' {
			continue
		}
		r, err := wire.NewReader(body)
		if err != nil || r.FileIdentifier() != wiretypes.CommitTransactionRequestFileID {
			continue
		}
		var request wiretypes.CommitTransactionRequest
		if err := request.UnmarshalFromReader(r); err != nil {
			t.Fatal(err)
		}
		for _, mutation := range request.Transaction.Mutations {
			if bytes.HasPrefix(mutation.Param1, b.keys.data.Bytes()) {
				requests = append(requests, capturedCommit{request: request, attempt: attempt})
				break
			}
		}
	}
	var found []wiretypes.CommitTransactionRef
	injectedRejections := 0
	for _, captured := range requests {
		request := captured.request
		body, ok := replies[request.Reply.Token]
		if !ok {
			if captured.attempt == len(log.starts) {
				t.Fatal("successful source commit has no matching reply")
			}
			continue
		}
		injectedAttempt := false
		if len(injectedReadKey) == 1 {
			for _, conflict := range request.Transaction.ReadConflictRanges {
				if bytes.Equal(conflict.Begin, injectedReadKey[0]) {
					injectedAttempt = true
				}
			}
		}
		var reader wire.Reader
		code := 0
		if err := wire.ReadErrorOrInto(body, &reader); err != nil {
			var rejected *wire.FDBError
			if !errors.As(err, &rejected) {
				if captured.attempt == len(log.starts) {
					t.Fatalf("malformed successful commit reply: %v", err)
				}
				continue
			}
			code = rejected.Code
		} else {
			var reply wiretypes.CommitID
			if err := reply.UnmarshalFromReader(&reader); err != nil {
				if captured.attempt == len(log.starts) {
					t.Fatal(err)
				}
				continue
			}
			if reply.Version == -1 {
				code = 1020
			} else if reply.Version <= 0 {
				t.Fatalf("invalid successful commit version: %d", reply.Version)
			}
		}
		if injectedAttempt {
			if code != 1020 {
				t.Fatalf("injected completed attempt reply=%d want1020", code)
			}
			injectedRejections++
		}
		if code == 0 && captured.attempt == len(log.starts) {
			found = append(found, request.Transaction)
		}
	}
	if len(injectedReadKey) == 1 && injectedRejections != 1 {
		t.Fatalf("injected attempt correlated rejections=%d want1", injectedRejections)
	}
	if len(found) != 1 || len(found[0].Mutations) == 0 || len(found[0].ReadConflictRanges) == 0 {
		t.Fatalf("need exactly one populated successful source commit, got %d", len(found))
	}
	return found[0]
}

// prefetchRetryTransactor injects only after a completed callback, so an
// unrelated early storage retry cannot consume the intended fault.
type prefetchRetryTransactor struct {
	fdb.Database
	inner           fdb.CtxTransactor
	raw             fdb.Database
	codes, injected []int
	outcomes        []error
	conflictKey     fdb.Key
	conflicted      bool
}

func (p *prefetchRetryTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return p.TransactCtx(context.Background(), fn)
}

func (p *prefetchRetryTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return p.inner.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		result, err := fn(tx)
		if err == nil && len(p.codes) > 0 {
			code := p.codes[0]
			p.codes = p.codes[1:]
			p.injected = append(p.injected, code)
			err = fdb.Error{Code: code}
		}
		if err == nil && p.conflictKey != nil && !p.conflicted {
			if _, err = tx.Get(p.conflictKey).Get(); err == nil {
				_, err = p.raw.TransactCtx(ctx, func(other fdb.WritableTransaction) (any, error) {
					other.Set(p.conflictKey, []byte("commit conflict after completed observations"))
					return nil, nil
				})
				if err == nil {
					p.conflicted = true
				}
			}
		}
		p.outcomes = append(p.outcomes, err)
		return result, err
	})
}

func initializeIngestPrefetchRoot(t *testing.T, ctx context.Context, b *FDBBackend) {
	t.Helper()
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		wire, err := json.Marshal(manifest(b.cfg, "ready"))
		if err != nil {
			return nil, err
		}
		rc.Transaction().Set(b.keys.owner(), wire)
		_, err = b.open(rc)
		return nil, err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func restoreIngestPrefetchRoot(t *testing.T, ctx context.Context, b *FDBBackend, rows map[string]string) {
	t.Helper()
	_, err := b.raw.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		for _, r := range otelOwnedRanges(t, b) {
			tx.ClearRange(r)
		}
		for key, value := range rows {
			tx.Set(fdb.Key(key), []byte(value))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func controlIngestPrefetchSave(ctx context.Context, b *FDBBackend, batch preparedBatch) error {
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		b.observeIngestRun("attempt", batch, nil)
		if err := b.readManifest(rc, false); err != nil {
			return nil, err
		}
		store, err := b.open(rc)
		if err != nil {
			return nil, err
		}
		if _, err := store.SaveRecordBatch(batch.messages); err != nil {
			return nil, err
		}
		n, err := rc.Transaction().GetApproximateSize().Get()
		if err != nil {
			return nil, err
		}
		if n > 500000 {
			return nil, fmt.Errorf("control exceeds mutation guard: %d", n)
		}
		return nil, nil
	})
	return err
}

func testOtelIngestPrefetch(t *testing.T, ctx context.Context, cluster, order string) {
	t.Helper()
	for _, scenario := range []string{"absent", "updated", "deleted", "split", "inline_version", "legacy_bare", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			c := testConfig()
			c.Count, c.Tenants, c.Batch, c.Workers, c.Diagnostics, c.SaveMode, c.Order = 1, 1, 1, 1, true, "batch", order
			b := openOtelFixture(t, ctx, cluster, c, false)
			if scenario == "legacy_bare" {
				builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(b.md.FileDescriptor()).SetSplitLongRecords(false)
				builder.GetRecordType("Envelope").SetPrimaryKey(b.md.GetRecordType("Envelope").PrimaryKey)
				for _, index := range b.md.GetAllIndexes() {
					builder.AddIndex("Envelope", index)
				}
				var err error
				b.md, err = builder.Build()
				if err != nil {
					t.Fatal(err)
				}
				b.idx = b.md.GetIndex(b.idx.Name)
			}
			initializeIngestPrefetchRoot(t, ctx, b)
			old := Generate(b.cfg, 0)
			old.Start += 1000000
			old.Attributes[0] = Attribute{"span", "prefetch.old", "string", "remove-old-covering-entry"}
			var oldSplit bool
			if scenario != "absent" {
				_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					store, err := b.open(rc)
					if err != nil {
						return nil, err
					}
					message, err := toRecord(old)
					if err != nil {
						return nil, err
					}
					if scenario == "split" {
						unknown := protowire.AppendTag(nil, 1000, protowire.BytesType)
						message.ProtoReflect().SetUnknown(protowire.AppendBytes(unknown, bytes.Repeat([]byte("x"), 110000)))
					}
					var saved *recordlayer.FDBStoredRecord[proto.Message]
					if scenario == "inline_version" {
						version, versionErr := recordlayer.NewCompleteVersion(bytes.Repeat([]byte{1}, 10), 7)
						if versionErr != nil {
							return nil, versionErr
						}
						saved, err = store.SaveRecordWithVersion(message, recordlayer.RecordExistenceCheckNone, version, recordlayer.VersionstampSaveBehaviorWithVersion)
					} else {
						saved, err = store.SaveRecord(message)
					}
					if err != nil {
						return nil, err
					}
					oldSplit = saved.Split
					if scenario == "inline_version" && (!saved.VersionedInline || saved.Version == nil) {
						return nil, errors.New("inline version control was not materialized")
					}
					if scenario == "deleted" {
						_, err = store.DeleteRecord(saved.PrimaryKey)
					}
					if scenario == "corrupt" {
						rc.Transaction().Set(b.store.Sub(recordlayer.RecordKey).Pack(append(otelPK(old), int64(0))), []byte("corrupt record bytes"))
					}
					if scenario == "legacy_bare" {
						header := store.GetStoreHeader()
						header.OmitUnsplitRecordSuffix = proto.Bool(true)
						data, marshalErr := header.MarshalVT()
						if marshalErr != nil {
							return nil, marshalErr
						}
						key := b.store.Sub(recordlayer.RecordKey).Pack(append(otelPK(old), int64(0)))
						value, readErr := rc.Transaction().Get(key).Get()
						if readErr != nil {
							return nil, readErr
						}
						rc.Transaction().Set(b.store.Sub(recordlayer.RecordKey).Pack(otelPK(old)), value)
						rc.Transaction().Clear(key)
						rc.Transaction().Set(b.store.Pack(tuple.Tuple{recordlayer.StoreInfoKey}), data)
					}
					return nil, err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "split" && !oldSplit {
				t.Fatal("split control was not split")
			}
			batch, err := prepareIngestBatch(ctx, b.cfg, 0)
			if err != nil {
				t.Fatal(err)
			}
			before := otelRawRootSnapshot(t, ctx, b)
			if scenario == "corrupt" {
				controlErr := controlIngestPrefetchSave(ctx, b, batch)
				prefetchErr := b.saveIngestBatch(ctx, batch, newIngestRecorder(true))
				var controlDecode, prefetchDecode *recordlayer.RecordDeserializationError
				if !errors.As(controlErr, &controlDecode) || !errors.As(prefetchErr, &prefetchDecode) {
					t.Fatalf("prefetch changed corrupt-old-image rejection: control=%v prefetch=%v", controlErr, prefetchErr)
				}
				if after := otelRawRootSnapshot(t, ctx, b); !reflect.DeepEqual(before, after) {
					t.Fatal("corrupt old image was overwritten")
				}
				return
			}
			control := ingestPrefetchCommit(t, b, func() error { return controlIngestPrefetchSave(ctx, b, batch) })
			controlRows := otelRawRootSnapshot(t, ctx, b)
			restoreIngestPrefetchRoot(t, ctx, b, before)
			originalDB := b.db
			var injectedReadKey []fdb.Key
			var retry *prefetchRetryTransactor
			if scenario == "absent" {
				key := append(fdb.Key("\x02otel-prefetch-retry/"), b.keys.data.Bytes()...)
				retry = &prefetchRetryTransactor{Database: b.raw, inner: b.raw, raw: b.raw, conflictKey: key}
				b.db = recordlayer.NewFDBDatabaseWithTransactor(retry, b.raw)
				defer func() {
					b.db = originalDB
					_, err := b.raw.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) { tx.Clear(key); return nil, nil })
					if err != nil {
						t.Error(err)
					}
				}()
				injectedReadKey = []fdb.Key{key}
			}
			prefetch := ingestPrefetchCommit(t, b, func() error { return b.saveIngestBatch(ctx, batch, newIngestRecorder(true)) }, injectedReadKey...)
			if retry != nil && (!retry.conflicted || len(retry.outcomes) < 2 || retry.outcomes[len(retry.outcomes)-1] != nil) {
				t.Fatalf("completed source attempt did not replay: %+v", retry)
			}
			b.db = originalDB
			if !reflect.DeepEqual(control.ReadConflictRanges, prefetch.ReadConflictRanges) || !reflect.DeepEqual(control.WriteConflictRanges, prefetch.WriteConflictRanges) || !reflect.DeepEqual(control.Mutations, prefetch.Mutations) {
				t.Fatalf("prefetch changed shipped canonical conflicts/mutations: control=%+v prefetch=%+v", control, prefetch)
			}
			if after := otelRawRootSnapshot(t, ctx, b); !reflect.DeepEqual(controlRows, after) {
				t.Fatal("prefetch changed persisted record/index bytes")
			}
			assertOtelSource(t, ctx, b, Generate(b.cfg, 0))
			assertOtelCommittedPostings(t, ctx, b, []Span{Generate(b.cfg, 0)})
			if audit, err := b.Audit(ctx); err != nil || audit.Rows != 1 || audit.Attributes != 8 {
				t.Fatalf("prefetch census: %+v %v", audit, err)
			}
		})
	}
	for _, scenario := range []string{"prefetch_error", "owner_precedence", "budget"} {
		t.Run(scenario, func(t *testing.T) {
			c := testConfig()
			c.Count, c.Tenants, c.Batch, c.Workers, c.Diagnostics, c.SaveMode, c.Order = 1, 1, 1, 1, true, "batch", order
			b := openOtelFixture(t, ctx, cluster, c, false)
			initializeIngestPrefetchRoot(t, ctx, b)
			before := otelRawRootSnapshot(t, ctx, b)
			wrapped := newIngestReadWaveTransactor(b, ctx)
			d := newIngestRecorder(true)
			var guardAtAttemptStart int64
			wrapped.beforeAttempt = func(int) error { guardAtAttemptStart = d.out.PhaseLatencies["guard"].Count; return nil }
			cause := errors.New("prefetch storage read failed")
			wrapped.prefetchError = cause
			ownerCause := errors.New("authoritative owner failed before prefetch")
			if scenario == "owner_precedence" {
				wrapped.ownerError = ownerCause
			}
			if scenario == "budget" {
				wrapped.prefetchError = nil
				base := time.Now()
				now := base
				b.ingestClock = func() time.Time { return now }
				wrapped.beforeAttempt = func(int) error { now = base; guardAtAttemptStart = d.out.PhaseLatencies["guard"].Count; return nil }
				wrapped.afterPrefetch = func(*ingestReadWaveAttempt) { now = now.Add(2 * time.Second) }
			}
			original := b.db
			b.db = recordlayer.NewFDBDatabaseWithTransactor(wrapped, b.raw)
			defer func() { b.db, b.ingestClock = original, nil }()
			batch, err := prepareIngestBatch(ctx, b.cfg, 0)
			if err != nil {
				t.Fatal(err)
			}
			sourceCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			err = b.saveIngestBatch(sourceCtx, batch, d)
			if err == nil {
				t.Fatal("prefetch failure/budget accepted")
			}
			if len(wrapped.attempts) == 0 {
				t.Fatal("prefetch fixture never entered a transaction callback")
			}
			for i, attempt := range wrapped.attempts {
				if len(attempt.problems) != 0 || attempt.owners != 1 || attempt.headers != 1 || attempt.states != 1 || attempt.resolutions != 1 || attempt.timeout != 1500 || attempt.retries != 10 {
					t.Fatalf("attempt%d lost exact read-wave/options: %+v", i, attempt)
				}
			}
			state := wrapped.attempts[len(wrapped.attempts)-1]
			if scenario == "prefetch_error" && (!errors.Is(err, cause) || state.recordWrites != 0 || state.resolved != 1) {
				t.Fatalf("prefetch failure reached normal saves/lost error: state=%+v err=%v", state, err)
			}
			if scenario == "owner_precedence" && (!errors.Is(err, ownerCause) || state.recordWrites != 0 || state.resolved != 0) {
				t.Fatalf("prefetch masked owner error: state=%+v err=%v", state, err)
			}
			if scenario == "budget" && (!strings.Contains(err.Error(), "transaction guard exceeded") || state.recordWrites != 1 || d.out.PhaseLatencies["guard"].Count-guardAtAttemptStart != 1) {
				t.Fatalf("prefetch elapsed escaped save+guard budget: state=%+v profile=%+v err=%v", state, d.out, err)
			}
			if d.out.PhaseLatencies["commit"].Count != 0 {
				t.Fatal("failed prefetch/budget recorded successful commit")
			}
			b.db = original
			if after := otelRawRootSnapshot(t, ctx, b); !reflect.DeepEqual(before, after) {
				t.Fatal("failed prefetch/budget persisted source mutations")
			}
		})
	}
	// Exporter commits are bounded by their transaction timeout, not the
	// benchmark's 1s elapsed guard, and have no retry limit; the size guard
	// still applies to both.
	t.Run("exporter_elapsed_exempt", func(t *testing.T) {
		c := testConfig()
		c.Count, c.Tenants, c.Batch, c.Workers, c.Diagnostics, c.SaveMode, c.Order = 1, 1, 1, 1, true, "batch", order
		b := openOtelFixture(t, ctx, cluster, c, false)
		initializeIngestPrefetchRoot(t, ctx, b)
		wrapped := newIngestReadWaveTransactor(b, ctx)
		wrapped.wantTimeout = defaultExporterTxnTimeout.Milliseconds()
		base := time.Now()
		now := base
		b.ingestClock = func() time.Time { return now }
		wrapped.beforeAttempt = func(int) error { now = base; return nil }
		wrapped.afterPrefetch = func(*ingestReadWaveAttempt) { now = now.Add(2 * time.Second) }
		retry := &prefetchRetryTransactor{Database: b.raw, inner: wrapped, raw: b.raw, codes: []int{1020}}
		original := b.db
		b.db = recordlayer.NewFDBDatabaseWithTransactor(retry, b.raw)
		defer func() { b.db, b.ingestClock = original, nil }()
		batch, err := prepareIngestBatch(ctx, b.cfg, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.saveIngestBatch(withExporterTxns(ctx, defaultExporterTxnTimeout), batch, newIngestRecorder(true)); err != nil {
			t.Fatalf("exporter commit hit the benchmark elapsed guard: %v", err)
		}
		if !reflect.DeepEqual(retry.injected, []int{1020}) || len(retry.codes) != 0 || len(retry.outcomes) < 2 || retry.outcomes[len(retry.outcomes)-1] != nil || len(wrapped.attempts) != len(retry.outcomes) {
			t.Fatalf("elapsed fixture failed to replay completed callback: %+v", retry)
		}
		for i, state := range wrapped.attempts {
			if len(state.problems) != 0 || state.timeout != defaultExporterTxnTimeout.Milliseconds() || state.retries != noRetryLimit {
				t.Fatalf("exporter attempt%d: %+v", i, state)
			}
		}
		b.db = original
		assertOtelSource(t, ctx, b, Generate(b.cfg, 0))
	})
	// An exporter batch is one Run: FDB's OnError loop absorbs a retryable
	// code in the same transaction, and a 1031 fails the batch as a transient
	// error without the benchmark's fresh Runs.
	for _, code := range []int{1020, 1031} {
		t.Run(fmt.Sprintf("exporter_inject_%d", code), func(t *testing.T) {
			c := testConfig()
			c.Count, c.Tenants, c.Batch, c.Workers, c.Diagnostics, c.SaveMode, c.Order = 1, 1, 1, 1, true, "batch", order
			b := openOtelFixture(t, ctx, cluster, c, false)
			initializeIngestPrefetchRoot(t, ctx, b)
			wrapped := newIngestReadWaveTransactor(b, ctx)
			wrapped.wantTimeout = defaultExporterTxnTimeout.Milliseconds()
			retry := &prefetchRetryTransactor{Database: b.raw, inner: wrapped, raw: b.raw, codes: []int{1020, code}}
			original := b.db
			b.db = recordlayer.NewFDBDatabaseWithTransactor(retry, b.raw)
			defer func() { b.db = original }()
			var runs []error
			b.db.SetAttemptObserver(func(_ recordlayer.AttemptCall, err error) { runs = append(runs, err) })
			batch, err := prepareIngestBatch(ctx, b.cfg, 0)
			if err != nil {
				t.Fatal(err)
			}
			err = b.saveIngestBatch(withExporterTxns(ctx, defaultExporterTxnTimeout), batch, nil)
			if len(runs) != 1 {
				t.Fatalf("exporter batch made %d Record Layer attempts, want 1: %v", len(runs), runs)
			}
			if !reflect.DeepEqual(retry.injected, []int{1020, code}) || len(retry.codes) != 0 || len(wrapped.attempts) != len(retry.outcomes) {
				t.Fatalf("intended extra/primary injections not observed exactly: %+v", retry)
			}
			for i, state := range wrapped.attempts {
				if len(state.problems) != 0 || state.timeout != defaultExporterTxnTimeout.Milliseconds() || state.retries != noRetryLimit {
					t.Fatalf("attempt %d: %+v", i, state)
				}
			}
			b.db = original
			if code == 1020 {
				if err != nil || len(retry.outcomes) < 3 || retry.outcomes[len(retry.outcomes)-1] != nil {
					t.Fatalf("two intended 1020 injections must be followed by a successful callback: err=%v outcomes=%v", err, retry.outcomes)
				}
				assertOtelSource(t, ctx, b, Generate(b.cfg, 0))
				return
			}
			requireOtelKind(t, err, "transient")
			lastCode, lastOK := fdbErrorCode(retry.outcomes[len(retry.outcomes)-1])
			if got, ok := fdbErrorCode(err); !ok || got != 1031 || !lastOK || lastCode != 1031 {
				t.Fatalf("injected 1031 after extra retry: err=%v outcomes=%v", err, retry.outcomes)
			}
		})
	}
}

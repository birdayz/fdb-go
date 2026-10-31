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

func ingestPrefetchCommit(t *testing.T, b *FDBBackend, run func() error) wiretypes.CommitTransactionRef {
	t.Helper()
	ingestPrefetchCaptureMu.Lock()
	defer ingestPrefetchCaptureMu.Unlock()
	var log bytes.Buffer
	stop := transport.CaptureWireLog(&log)
	defer stop()
	err := run()
	stop()
	if err != nil {
		t.Fatal(err)
	}
	input := bytes.NewReader(log.Bytes())
	var found []wiretypes.CommitTransactionRef
	for {
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
			if bytes.HasPrefix(mutation.Param1, b.root.Bytes()) {
				found = append(found, request.Transaction)
				break
			}
		}
	}
	if len(found) != 1 || len(found[0].Mutations) == 0 || len(found[0].ReadConflictRanges) == 0 {
		t.Fatalf("need exactly one populated successful source commit, got %d", len(found))
	}
	return found[0]
}

func initializeIngestPrefetchRoot(t *testing.T, ctx context.Context, b *FDBBackend) {
	t.Helper()
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		wire, err := json.Marshal(manifest(b.cfg, "ready"))
		if err != nil {
			return nil, err
		}
		rc.Transaction().Set(b.root.Sub("owner").FDBKey(), wire)
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
		r, err := fdb.PrefixRange(b.root.Bytes())
		if err != nil {
			return nil, err
		}
		tx.ClearRange(r)
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
			prefetch := ingestPrefetchCommit(t, b, func() error { return b.saveIngestBatch(ctx, batch, newIngestRecorder(true)) })
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
			cause := errors.New("prefetch storage read failed")
			wrapped.prefetchError = cause
			ownerCause := errors.New("authoritative owner failed before prefetch")
			if scenario == "owner_precedence" {
				wrapped.ownerError = ownerCause
			}
			if scenario == "budget" {
				wrapped.prefetchError = nil
				now := time.Now()
				b.ingestClock = func() time.Time { return now }
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
			d := newIngestRecorder(true)
			err = b.saveIngestBatch(sourceCtx, batch, d)
			if err == nil {
				t.Fatal("prefetch failure/budget accepted")
			}
			assertIngestReadWave(t, wrapped.attempts, 1)
			state := wrapped.attempts[0]
			if scenario == "prefetch_error" && (!errors.Is(err, cause) || state.recordWrites != 0 || state.resolved != 1) {
				t.Fatalf("prefetch failure reached normal saves/lost error: state=%+v err=%v", state, err)
			}
			if scenario == "owner_precedence" && (!errors.Is(err, ownerCause) || state.recordWrites != 0 || state.resolved != 0) {
				t.Fatalf("prefetch masked owner error: state=%+v err=%v", state, err)
			}
			if scenario == "budget" && (!strings.Contains(err.Error(), "transaction guard exceeded") || state.recordWrites != 1 || d.out.PhaseLatencies["guard"].Count != 1) {
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
}

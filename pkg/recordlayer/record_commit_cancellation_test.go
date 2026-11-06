package recordlayer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/simfdb"
)

type commitCancellationBackend struct {
	*simfdb.SimDB
	commits, cancels, flushes, open atomic.Int64
}

type commitCancellationTxn struct {
	fdb.WritableTransaction
	backend *commitCancellationBackend
	once    sync.Once
}

func (b *commitCancellationBackend) CreateWritableTransaction() (fdb.WritableTransaction, error) {
	tx, err := b.SimDB.CreateWritableTransaction()
	if err != nil {
		return nil, err
	}
	b.open.Add(1)
	return &commitCancellationTxn{WritableTransaction: tx, backend: b}, nil
}

func (b *commitCancellationBackend) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	tx, err := b.CreateWritableTransaction()
	if err != nil {
		return nil, err
	}
	defer tx.Cancel()
	result, err := fn(tx)
	if err != nil {
		return nil, err
	}
	return result, tx.Commit().Get()
}

func (tx *commitCancellationTxn) Commit() fdb.FutureNil {
	tx.backend.commits.Add(1)
	tx.once.Do(func() { tx.backend.open.Add(-1) })
	return tx.WritableTransaction.Commit()
}

func (tx *commitCancellationTxn) Cancel() {
	tx.once.Do(func() { tx.backend.open.Add(-1); tx.backend.cancels.Add(1) })
	tx.WritableTransaction.Cancel()
}

func (tx *commitCancellationTxn) SetVersionstampedValue(key fdb.KeyConvertible, value []byte) {
	tx.backend.flushes.Add(1)
	tx.WritableTransaction.SetVersionstampedValue(key, value)
}

func TestRunCommitChecksRetainCallerCancellationAndFinalizeHandles(t *testing.T) {
	t.Parallel()
	for _, route := range []string{"Run", "runner"} {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			env := dst.NewSim(471)
			b := &commitCancellationBackend{SimDB: simfdb.New(env)}
			db := NewFDBDatabaseWithBackend(b).SetEnv(env)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			checks, posts := 0, 0
			var joined sync.WaitGroup
			body := func(rc *FDBRecordContext) (any, error) {
				version, err := IncompleteVersion(1)
				if err != nil {
					return nil, err
				}
				rc.AddVersionMutation(MutationTypeSetVersionstampedValue, fdb.Key("pending"), buildOldFormatVersionstampedValue(version))
				rc.AddPostCommit(func() { posts++ })
				rc.AddCommitCheck(func() error {
					checks++
					entered := make(chan error, 1)
					joined.Add(1)
					go func() {
						defer joined.Done()
						finish, err := rc.beginRecordWrite()
						entered <- err
						if err != nil {
							return
						}
						defer finish()
						<-ctx.Done()
					}()
					if err := <-entered; err != nil {
						return err
					}
					cancel()
					return nil
				})
				return nil, nil
			}
			var err error
			if route == "Run" {
				_, err = db.Run(ctx, body)
			} else {
				_, err = NewFDBDatabaseRunner(db).RunWithRetry(ctx, body)
			}
			joined.Wait()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("caller cancellation lost: %v", err)
			}
			if checks != 1 || posts != 0 || b.commits.Load() != 0 || b.flushes.Load() != 0 || b.cancels.Load() != 1 || b.open.Load() != 0 {
				t.Fatalf("checks=%d posts=%d commits=%d flushes=%d cancels=%d open=%d", checks, posts, b.commits.Load(), b.flushes.Load(), b.cancels.Load(), b.open.Load())
			}
		})
	}
}

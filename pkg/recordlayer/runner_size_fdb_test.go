package recordlayer

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

func TestFDB_RunnerTransactionSizeConfig(t *testing.T) {
	t.Parallel()
	db := newLoadParityDatabase(t)
	metadata := loadParityMetaData(t, false, false)
	for _, route := range []string{"OpenContext", "RunWithRetry"} {
		for _, operation := range []string{"save", "delete"} {
			for _, limit := range []string{"disabled", "warning", "error"} {
				t.Run(route+"/"+operation+"/"+limit, func(t *testing.T) {
					t.Parallel()
					ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
					defer cancel()
					root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
					openStore := func(rc *FDBRecordContext) (*FDBRecordStore, error) {
						return NewStoreBuilder().SetContext(rc).SetMetaDataProvider(metadata).SetSubspace(root).CreateOrOpen()
					}
					_, err := db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
						store, err := openStore(rc)
						if err != nil || operation != "delete" {
							return nil, err
						}
						return store.SaveRecord(&demopb.Order{OrderId: proto.Int64(1)})
					})
					if err != nil {
						t.Fatal(err)
					}

					timer := NewStoreTimer()
					config := &RecordContextConfig{Timer: timer}
					switch limit {
					case "warning":
						config.TransactionSizeWarnBytes = 1
					case "error":
						config.TransactionSizeWarnBytes = 1
						config.TransactionSizeErrorBytes = 1
					}
					runner := NewFDBDatabaseRunner(db).SetContextConfig(config)
					attempts := 0
					body := func(rc *FDBRecordContext) (any, error) {
						attempts++
						store, err := openStore(rc)
						if err != nil {
							return nil, err
						}
						if operation == "save" {
							_, err = store.SaveRecord(&demopb.Order{OrderId: proto.Int64(1)})
						} else {
							_, err = store.DeleteRecord(tuple.Tuple{int64(1)})
						}
						if err != nil {
							return nil, err
						}
						// Rechecking a successful write must not count a second warning.
						return nil, rc.CheckTransactionSize()
					}
					if route == "RunWithRetry" {
						_, err = runner.RunWithRetry(ctx, body)
					} else {
						var rc *FDBRecordContext
						rc, err = runner.OpenContext(ctx)
						if err == nil {
							defer rc.Cancel()
							_, err = body(rc)
							if err == nil {
								err = rc.Commit()
							}
						}
					}
					if limit == "error" {
						var exceeded *TransactionSizeExceededError
						if !errors.As(err, &exceeded) || exceeded.LimitBytes != 1 || exceeded.CurrentBytes < 1 {
							t.Fatalf("size limit error = %v, want TransactionSizeExceededError at 1 byte", err)
						}
						if attempts != 1 {
							t.Fatalf("non-retryable size error took %d attempts", attempts)
						}
					} else if err != nil {
						t.Fatal(err)
					}
					wantWarnings := int64(0)
					if limit == "warning" {
						wantWarnings = int64(attempts)
					}
					if got := timer.GetCount(CountTransactionSizeWarning); got != wantWarnings {
						t.Fatalf("warnings = %d, want %d", got, wantWarnings)
					}
					_, err = db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
						store, err := openStore(rc)
						if err != nil {
							return nil, err
						}
						record, err := store.LoadRecord(tuple.Tuple{int64(1)})
						if err == nil {
							wantPresent := (operation == "save") != (limit == "error")
							if (record != nil) != wantPresent {
								t.Fatalf("record present = %v, want %v after %s", record != nil, wantPresent, operation)
							}
						}
						return nil, err
					})
					if err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func TestFDB_RunnerSizeWarningReappliedOnRetry(t *testing.T) {
	t.Parallel()
	db := newLoadParityDatabase(t)
	metadata := loadParityMetaData(t, false, false)
	root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	timer := NewStoreTimer()
	runner := NewFDBDatabaseRunner(db).SetMaxAttempts(2).SetContextConfig(&RecordContextConfig{
		TransactionSizeWarnBytes: 1,
		Timer:                    timer,
	})
	attempts := 0
	_, err := runner.RunWithRetry(t.Context(), func(rc *FDBRecordContext) (any, error) {
		attempts++
		store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(metadata).SetSubspace(root).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		if _, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(1)}); err != nil {
			return nil, err
		}
		if err := rc.CheckTransactionSize(); err != nil {
			return nil, err
		}
		if attempts == 1 {
			return nil, fdb.Error{Code: 1020}
		}
		return nil, nil
	})
	if err != nil || attempts != 2 || timer.GetCount(CountTransactionSizeWarning) != 2 {
		t.Fatalf("retry result: err=%v attempts=%d warnings=%d, want success and one warning per attempt", err, attempts, timer.GetCount(CountTransactionSizeWarning))
	}
}

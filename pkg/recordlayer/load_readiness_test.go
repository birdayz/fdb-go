package recordlayer

import (
	"bytes"
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// The backend-neutral range adapter must publish errors as well as rows: the
// index pipeline keeps only a completed prefix when its source hits a deadline.
func TestRecordRangeReadiness(t *testing.T) {
	t.Parallel()
	ss := subspace.FromBytes([]byte("record-readiness"))
	pk := tuple.Tuple{int64(7)}
	for _, fails := range []bool{false, true} {
		name := "rows"
		if fails {
			name = "error"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			release := make(chan struct{})
			rows := []fdb.KeyValue{
				{Key: ss.Pack(appendToTuple(pk, 1)), Value: []byte("first")},
				{Key: ss.Pack(appendToTuple(pk, 2)), Value: []byte("second")},
			}
			var wantErr error
			if fails {
				wantErr = fdb.Error{Code: 1007}
			}
			p := pendingRecordLoad{
				primaryKey: pk, prefix: ss.Pack(pk),
				rangeRead: gatedRecordRange{release: release, rows: rows, err: wantErr},
			}
			p.startReadiness()
			if p.ready() {
				t.Fatal("range reported ready before its rows/error were available")
			}
			close(release)
			<-p.asyncRange.done
			if !p.ready() {
				t.Fatal("completed range did not report ready")
			}
			var sizes sizeInfo
			value, version, err := p.finish(&sizes)
			if fails {
				var got fdb.Error
				if !errors.As(err, &got) || got.Code != 1007 {
					t.Fatalf("range error = %v, want FDB 1007", err)
				}
				return
			}
			if err != nil || version != nil || !bytes.Equal(value, []byte("firstsecond")) {
				t.Fatalf("range result = %q, %v, %v", value, version, err)
			}
			wantSizes := sizeInfo{KeyCount: 2, KeySize: len(rows[0].Key) + len(rows[1].Key), ValueSize: 11, IsSplit: true}
			if sizes != wantSizes {
				t.Fatalf("sizes = %+v, want %+v", sizes, wantSizes)
			}
		})
	}
}

type gatedRecordRange struct {
	fdb.RangeResult
	release <-chan struct{}
	rows    []fdb.KeyValue
	err     error
}

func (r gatedRecordRange) GetSliceWithError() ([]fdb.KeyValue, error) {
	<-r.release
	return r.rows, r.err
}

func TestRecordLoadReadinessIncludesLegacyVersion(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	var versionReady bool
	load := recordLoad{loaded: true, record: &FDBStoredRecord[proto.Message]{}, first: pendingStoreRecordLoad{
		raw: pendingRecordLoad{asyncRange: &pendingRecordRange{done: done}},
		oldVersion: NewPendingRead(func() (*FDBRecordVersion, error) { return nil, nil },
			func() bool { return versionReady }),
	}}
	if load.ready() {
		t.Fatal("neither range nor version is ready")
	}
	versionReady = true
	if load.ready() {
		t.Fatal("version ready must not hide a pending range")
	}
	versionReady = false
	close(done)
	if load.ready() {
		t.Fatal("range ready must not hide a pending legacy version")
	}
	versionReady = true
	if !load.ready() {
		t.Fatal("both range and version are ready")
	}
	load.first.oldVersion = nil
	if !load.ready() {
		t.Fatal("modern range needs no separate version read")
	}
}

func TestRecordLoadReadinessDoesNotWaitForUnusedLegacyVersion(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"missing", "range-error", "deserialize-error", "inline-version"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			done := make(chan struct{})
			close(done)
			load := recordLoad{first: pendingStoreRecordLoad{
				raw: pendingRecordLoad{asyncRange: &pendingRecordRange{done: done}},
				oldVersion: NewPendingRead(func() (*FDBRecordVersion, error) {
					t.Fatal("unused legacy version was awaited")
					return nil, nil
				}, func() bool { return false }),
			}}
			switch outcome {
			case "range-error":
				load.first.raw.asyncRange.err = fdb.Error{Code: 1007}
			case "deserialize-error":
				load.loaded = true
				load.loadErr = &RecordDeserializationError{PrimaryKey: tuple.Tuple{int64(1)}}
			case "inline-version":
				version, err := IncompleteVersion(7)
				if err != nil {
					t.Fatal(err)
				}
				load.loaded = true
				load.record = &FDBStoredRecord[proto.Message]{Version: version}
			}
			if !load.ready() {
				t.Fatal("completed record outcome was hidden by an unused legacy version")
			}
			if outcome != "inline-version" {
				_, _ = load.finish()
			} else if _, err := load.first.finishVersion(load.record.Version); err != nil {
				t.Fatal(err)
			}
		})
	}
}

package recordlayer

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"testing"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// The ledger records issued native observations, independently of ScanState's
// attempted reads (which also include limiter refusals). Fixtures bypass Set.
type partitionReadLedger struct {
	fdb.WritableTransaction
	data                     map[string][]byte
	reads, snapshots, writes int
	errAt                    int
	err                      error
	after                    func(int)
}

type partitionLedgerFuture struct {
	value []byte
	err   error
	after func()
}

func (f partitionLedgerFuture) Get() ([]byte, error) {
	if f.after != nil {
		f.after()
	}
	return bytes.Clone(f.value), f.err
}

func (f partitionLedgerFuture) MustGet() []byte {
	v, err := f.Get()
	if err != nil {
		panic(err)
	}
	return v
}
func (partitionLedgerFuture) BlockUntilReady() {}
func (partitionLedgerFuture) IsReady() bool    { return true }
func (partitionLedgerFuture) Cancel()          {}

type partitionLedgerRange struct {
	rows  []fdb.KeyValue
	err   error
	after func()
}

func (r partitionLedgerRange) GetSliceWithError() ([]fdb.KeyValue, error) {
	if r.after != nil {
		r.after()
	}
	return r.rows, r.err
}

func (r partitionLedgerRange) GetSliceOrPanic() []fdb.KeyValue {
	rows, err := r.GetSliceWithError()
	if err != nil {
		panic(err)
	}
	return rows
}

func (partitionLedgerRange) Iterator() fdb.RangeIterator {
	panic("partition native reads must use bounded materialization")
}

type partitionLedgerSnapshot struct {
	fdb.ReadTransaction
	ledger *partitionReadLedger
}

func (s partitionLedgerSnapshot) Get(k fdb.KeyConvertible) fdb.FutureByteSlice {
	s.ledger.snapshots++
	return s.ledger.Get(k)
}

func (s partitionLedgerSnapshot) GetRange(r fdb.Range, o fdb.RangeOptions) fdb.RangeResult {
	s.ledger.snapshots++
	return s.ledger.GetRange(r, o)
}

func (l *partitionReadLedger) Snapshot() fdb.ReadTransaction {
	return partitionLedgerSnapshot{ledger: l}
}

func (l *partitionReadLedger) observation() (error, func()) {
	l.reads++
	n := l.reads
	var err error
	if n == l.errAt {
		err = l.err
	}
	return err, func() {
		if l.after != nil {
			l.after(n)
		}
	}
}

func (l *partitionReadLedger) Get(k fdb.KeyConvertible) fdb.FutureByteSlice {
	err, after := l.observation()
	return partitionLedgerFuture{value: bytes.Clone(l.data[string(k.FDBKey())]), err: err, after: after}
}

func (l *partitionReadLedger) GetRange(r fdb.Range, o fdb.RangeOptions) fdb.RangeResult {
	if o.Limit != 1 || o.Mode != fdb.StreamingModeExact {
		panic(fmt.Sprintf("unbounded partition native read: %+v", o))
	}
	b, e := r.(fdb.ExactRange).FDBRangeKeys()
	keys := make([]string, 0)
	for k := range l.data {
		if bytes.Compare([]byte(k), b.FDBKey()) >= 0 && bytes.Compare([]byte(k), e.FDBKey()) < 0 {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	if o.Reverse {
		slices.Reverse(keys)
	}
	var rows []fdb.KeyValue
	if len(keys) != 0 {
		rows = []fdb.KeyValue{{Key: fdb.Key([]byte(keys[0])), Value: bytes.Clone(l.data[keys[0]])}}
	}
	err, after := l.observation()
	return partitionLedgerRange{rows: rows, err: err, after: after}
}

func (l *partitionReadLedger) Set(k fdb.KeyConvertible, v []byte) {
	l.writes++
	l.data[string(k.FDBKey())] = bytes.Clone(v)
}

func (l *partitionReadLedger) Clear(k fdb.KeyConvertible) {
	l.writes++
	delete(l.data, string(k.FDBKey()))
}

func (l *partitionReadLedger) ClearRange(r fdb.ExactRange) {
	l.writes++
	b, e := r.FDBRangeKeys()
	for k := range l.data {
		if bytes.Compare([]byte(k), b.FDBKey()) >= 0 && bytes.Compare([]byte(k), e.FDBKey()) < 0 {
			delete(l.data, k)
		}
	}
}
func (*partitionReadLedger) Cancel() {}

func partitionLedgerStore(t *testing.T) (*FDBRecordStore, *partitionReadLedger) {
	t.Helper()
	b := partitionTestBuilder().SetSplitLongRecords(true)
	b.GetRecordType("Order").SetRecordTypeKey("DOCS")
	md, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	l := &partitionReadLedger{data: make(map[string][]byte)}
	rc := NewFDBRecordContext(l, dst.NewSim(17))
	root := subspace.FromBytes(nil)
	s := &FDBRecordStore{context: rc, metaData: md, subspace: root, recordsSubspace: root.Sub(RecordKey), storeHeader: &gen.DataStoreInfo{FormatVersion: proto.Int32(14)}, indexStates: make(map[string]IndexState)}
	return s, l
}

func partitionLedgerSeed(s *FDBRecordStore, l *partitionReadLedger, coordinates ...int64) {
	ss := partitionSubspace(s.subspace, s.metaData.GetRecordType("Order"))
	header, _ := hex.DecodeString("150115061503150a0100ff0102030405060708090a0b0c0d0e0f00")
	l.data[string(ss.Pack(tuple.Tuple{"header"}))] = header
	l.data[string(ss.Pack(tuple.Tuple{"epoch"}))] = []byte{0x15, 0x01}
	for _, p := range coordinates {
		l.data[string(ss.Pack(tuple.Tuple{"active", p}))] = []byte{0x15, 0x01}
		l.data[string(ss.Pack(tuple.Tuple{"fence", p}))] = []byte{0x15, 0x01, 0x27}
	}
}

func partitionLedgerBody(s *FDBRecordStore, l *partitionReadLedger, p, source, id, suffix int64) {
	l.data[string(s.recordsSubspace.Pack(tuple.Tuple{"DOCS", p, source, id, suffix}))] = []byte{1, 2, 3, 4, 5}
}

func partitionNext[T any](t *testing.T, c RecordCursor[T]) RecordCursorResult[T] {
	t.Helper()
	r, err := c.OnNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func partitionToken[T any](t *testing.T, r RecordCursorResult[T]) []byte {
	t.Helper()
	b, err := r.GetContinuation().ToBytes()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPartitionDirectoryLiteralLedger(t *testing.T) {
	t.Parallel()
	for _, body := range []bool{false, true} {
		for _, snapshot := range []bool{false, true} {
			s, l := partitionLedgerStore(t)
			partitionLedgerSeed(s, l, 0)
			if body {
				partitionLedgerBody(s, l, 0, 0, 1, 0)
			}
			before := maps.Clone(l.data)
			effects := partitionVersionEffectsOf(s.context)
			counter := s.context.localVersion.Load()
			state := NewScanLimiterStateIn(s.context.Env())
			props := ExecuteProperties{ReturnedRowLimit: 1, ScanState: state, IsolationLevel: IsolationLevelSerializable}
			if snapshot {
				props.IsolationLevel = IsolationLevelSnapshot
			}
			c := s.ScanPartitions("Order", PartitionCoordinateRange{}, nil, NewScanProperties(props))
			r := partitionNext(t, c)
			if !r.HasNext() || r.GetValue().Coordinate != 0 || r.GetValue().Generation != 1 {
				t.Fatalf("entry = %+v", r)
			}
			wantBytes := int64(210)
			if body {
				wantBytes = 228
			}
			if l.reads != 6 || state.RecordsScanned() != 6 || state.BytesScanned() != wantBytes {
				t.Fatalf("body=%v snapshot=%v: issued=%d attempts=%d bytes=%d; want 6/6/%d", body, snapshot, l.reads, state.RecordsScanned(), state.BytesScanned(), wantBytes)
			}
			for range 2 {
				stop := partitionNext(t, c)
				if stop.GetNoNextReason() != ReturnLimitReached || stop.GetContinuation().IsEnd() {
					t.Fatalf("stop=%+v", stop)
				}
			}
			if l.reads != 6 || state.RecordsScanned() != 6 || (snapshot && l.snapshots != 6) || (!snapshot && l.snapshots != 0) {
				t.Fatalf("row limit speculated: %+v", l)
			}
			if l.writes != 0 || !maps.EqualFunc(before, l.data, bytes.Equal) || !partitionReadEffectsEqual(effects, partitionVersionEffectsOf(s.context)) || counter != s.context.localVersion.Load() {
				t.Fatal("directory mutated record/version state")
			}
			if len(s.context.commitChecks) != 0 || len(s.context.namedCommitChecks) != 0 {
				t.Fatal("read-only directory registered a mutation guard")
			}
		}
	}
}

func partitionReadEffectsEqual(a, b partitionVersionEffects) bool {
	return maps.Equal(a.local, b.local) && maps.EqualFunc(a.mutations, b.mutations, func(x, y versionMutation) bool {
		return x.mutationType == y.mutationType && bytes.Equal(x.value, y.value)
	})
}

func TestPartitionDirectorySparsePagingLedger(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		for _, pageSize := range []int{0, 1} {
			for _, chunks := range []int{1, 100} {
				s, l := partitionLedgerStore(t)
				partitionLedgerSeed(s, l, -3, 0, 4, 1000000000000)
				for _, p := range []int64{-3, 4} {
					for i := range chunks {
						partitionLedgerBody(s, l, p, p*10, 1, int64(i))
					}
				}
				var token []byte
				var got []int64
				for pages := 0; ; pages++ {
					if pages > 8 {
						t.Fatal("non-progressing pagination")
					}
					c := s.ScanPartitions("Order", PartitionCoordinateRange{}, token, NewScanProperties(ExecuteProperties{ReturnedRowLimit: pageSize}).WithReverse(reverse))
					for {
						r := partitionNext(t, c)
						if r.HasNext() {
							got = append(got, r.GetValue().Coordinate)
							continue
						}
						if r.GetNoNextReason() == SourceExhausted {
							token = nil
						} else {
							token = partitionToken(t, r)
						}
						break
					}
					if token == nil {
						break
					}
				}
				want := []int64{-3, 0, 4, 1000000000000}
				if reverse {
					slices.Reverse(want)
				}
				if !slices.Equal(got, want) {
					t.Fatalf("got %v want %v", got, want)
				}
				wantReads := 15
				if pageSize == 1 {
					wantReads = 24
					if reverse {
						wantReads = 25
					}
				}
				if l.reads != wantReads || l.writes != 0 {
					t.Fatalf("reverse=%v page=%d chunks=%d issued=%d; want %d", reverse, pageSize, chunks, l.reads, wantReads)
				}
			}
		}
	}
}

func TestPartitionDirectoryRootBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                     string
		props                    ExecuteProperties
		precharge                bool
		wantReason               NoNextReason
		wantNative, wantAttempts int
		fail                     bool
	}{
		{"records-soft", ExecuteProperties{ScannedRecordsLimit: 1}, false, ScanLimitReached, 6, 7, false},
		{"bytes-soft", ExecuteProperties{ScannedBytesLimit: 1}, false, ByteLimitReached, 6, 7, false},
		{"time-soft", ExecuteProperties{TimeLimit: time.Nanosecond}, false, TimeLimitReached, 6, 7, false},
		{"precharged-soft", ExecuteProperties{ScannedRecordsLimit: 1, ScannedBytesLimit: 1, TimeLimit: time.Nanosecond}, true, ScanLimitReached, 6, 8, false},
		{"records-hard", ExecuteProperties{ScannedRecordsLimit: 1, FailOnScanLimitReached: true}, false, ScanLimitReached, 1, 2, true},
		{"precharged-hard", ExecuteProperties{ScannedRecordsLimit: 1, FailOnScanLimitReached: true}, true, ScanLimitReached, 0, 2, true},
		{"bytes-fail", ExecuteProperties{ScannedBytesLimit: 1, FailOnScanLimitReached: true}, false, ByteLimitReached, 6, 7, true},
		{"time-fail", ExecuteProperties{TimeLimit: time.Nanosecond, FailOnScanLimitReached: true}, false, TimeLimitReached, 6, 7, true},
		{"priority-record", ExecuteProperties{ScannedRecordsLimit: 6, ScannedBytesLimit: 1, TimeLimit: time.Nanosecond}, false, ScanLimitReached, 6, 7, false},
		{"priority-byte", ExecuteProperties{ScannedBytesLimit: 1, TimeLimit: time.Nanosecond}, false, ByteLimitReached, 6, 7, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, l := partitionLedgerStore(t)
			partitionLedgerSeed(s, l, 0, 1, 2)
			state := NewScanLimiterStateIn(s.context.Env())
			tc.props.ScanState = state
			if tc.precharge {
				state.AddRecordScanned()
				state.AddBytesScanned(1)
			}
			l.after = func(int) { s.context.Env().Clock.(*dst.SimClock).Advance(time.Second) }
			c := s.ScanPartitions("Order", PartitionCoordinateRange{}, nil, NewScanProperties(tc.props))
			r, err := c.OnNext(context.Background())
			if !tc.fail || tc.wantNative == 6 {
				if err != nil || !r.HasNext() {
					t.Fatalf("initial bundle=%+v,%v", r, err)
				}
				r, err = c.OnNext(context.Background())
			}
			if tc.fail {
				var limit *ScanLimitReachedError
				if !errors.As(err, &limit) || limit.Reason != tc.wantReason {
					t.Fatalf("limit=%v", err)
				}
			} else if err != nil || r.GetNoNextReason() != tc.wantReason {
				t.Fatalf("stop=%+v,%v", r, err)
			}
			for range 2 {
				_, _ = c.OnNext(context.Background())
			}
			if l.reads != tc.wantNative || state.RecordsScanned() != tc.wantAttempts || l.writes != 0 {
				t.Fatalf("issued=%d attempts=%d; want %d/%d", l.reads, state.RecordsScanned(), tc.wantNative, tc.wantAttempts)
			}
		})
	}
}

func TestPartitionDirectoryUnconsumedOrphanAndEmptyIdentity(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		s, l := partitionLedgerStore(t)
		partitionLedgerSeed(s, l, 0, 4)
		partitionLedgerBody(s, l, 2, 20, 1, 0)
		c := s.ScanPartitions("Order", PartitionCoordinateRange{}, nil, ForwardScan().WithReverse(reverse))
		r := partitionNext(t, c)
		if !r.HasNext() {
			t.Fatal("missing first active")
		}
		token := partitionToken(t, r)
		for _, next := range []RecordCursor[PartitionEntry]{c, s.ScanPartitions("Order", PartitionCoordinateRange{}, token, ForwardScan().WithReverse(reverse))} {
			_, err := next.OnNext(context.Background())
			var state *PartitionStateError
			if !errors.As(err, &state) || state.Coordinate != 2 {
				t.Fatalf("orphan disappeared behind head/token: %v", err)
			}
		}
	}
	for _, pollution := range []string{"none", "protocol", "body", "version"} {
		s, l := partitionLedgerStore(t)
		switch pollution {
		case "protocol":
			l.data[string(partitionSubspace(s.subspace, s.metaData.GetRecordType("Order")).Pack(tuple.Tuple{"unknown"}))] = []byte{1}
		case "body":
			partitionLedgerBody(s, l, 0, 0, 1, 0)
		case "version":
			l.data[string(s.subspace.Sub(RecordVersionKey).Pack(tuple.Tuple{"DOCS", int64(0), int64(0), int64(1)}))] = []byte{1}
		}
		c := s.ScanPartitions("Order", PartitionCoordinateRange{Empty: true}, nil, ForwardScan())
		r, err := c.OnNext(context.Background())
		if pollution == "none" {
			if err != nil || r.HasNext() || !r.GetContinuation().IsEnd() || l.reads != 4 {
				t.Fatalf("virgin=%+v,%v reads=%d", r, err, l.reads)
			}
		} else {
			var state *PartitionStateError
			if !errors.As(err, &state) {
				t.Fatalf("%s escaped missing-header census: %v", pollution, err)
			}
		}
		if l.writes != 0 {
			t.Fatal("bootstrapped on read")
		}
	}
}

func TestPartitionDirectoryRangeAndEndpointLiterals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		p, w      int64
		low, high PartitionSourceEndpoint
	}{
		{-922337203685477581, 10, PartitionSourceEndpoint{Unbounded: true}, PartitionSourceEndpoint{Value: -9223372036854775800}},
		{922337203685477580, 10, PartitionSourceEndpoint{Value: 9223372036854775800}, PartitionSourceEndpoint{Unbounded: true}},
		{math.MinInt64, 1, PartitionSourceEndpoint{Value: math.MinInt64}, PartitionSourceEndpoint{Value: -9223372036854775807}},
		{math.MaxInt64, 1, PartitionSourceEndpoint{Value: math.MaxInt64}, PartitionSourceEndpoint{Unbounded: true}},
		{-2, math.MaxInt64, PartitionSourceEndpoint{Unbounded: true}, PartitionSourceEndpoint{Value: -9223372036854775807}},
		{1, math.MaxInt64, PartitionSourceEndpoint{Value: math.MaxInt64}, PartitionSourceEndpoint{Unbounded: true}},
	} {
		lo, hi := partitionSourceInterval(tc.p, tc.w)
		if lo != tc.low || hi != tc.high {
			t.Fatalf("p=%d w=%d: %v/%v want %v/%v", tc.p, tc.w, lo, hi, tc.low, tc.high)
		}
	}
	for _, tc := range []struct {
		r    PartitionCoordinateRange
		want []int64
		bad  bool
	}{
		{PartitionCoordinateRange{}, []int64{-3, 0, 4}, false},
		{PartitionCoordinateRange{Lower: &PartitionCoordinateBound{Value: 0, Inclusive: true}, Upper: &PartitionCoordinateBound{Value: 4, Inclusive: false}}, []int64{0}, false},
		{PartitionCoordinateRange{Lower: &PartitionCoordinateBound{Value: 0}, Upper: &PartitionCoordinateBound{Value: 4, Inclusive: true}}, []int64{4}, false},
		{PartitionCoordinateRange{Lower: &PartitionCoordinateBound{Value: 4}, Upper: &PartitionCoordinateBound{Value: 4, Inclusive: true}}, nil, false},
		{PartitionCoordinateRange{Empty: true}, nil, false},
		{PartitionCoordinateRange{Lower: &PartitionCoordinateBound{Value: math.MaxInt64}}, nil, false},
		{PartitionCoordinateRange{Lower: &PartitionCoordinateBound{Value: 4}, Upper: &PartitionCoordinateBound{Value: 0}}, nil, true},
		{PartitionCoordinateRange{Empty: true, Lower: &PartitionCoordinateBound{}}, nil, true},
	} {
		s, l := partitionLedgerStore(t)
		partitionLedgerSeed(s, l, -3, 0, 4)
		c := s.ScanPartitions("Order", tc.r, nil, ForwardScan())
		var got []int64
		for {
			r, err := c.OnNext(context.Background())
			if tc.bad {
				if err == nil || l.reads != 0 {
					t.Fatalf("bad range=%v reads=%d", err, l.reads)
				}
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if !r.HasNext() {
				break
			}
			got = append(got, r.GetValue().Coordinate)
		}
		if !tc.bad && !slices.Equal(got, tc.want) {
			t.Fatalf("range %+v =%v want%v", tc.r, got, tc.want)
		}
	}
}

func TestPartitionDirectoryTokenValidationBeforeIO(t *testing.T) {
	t.Parallel()
	s, l := partitionLedgerStore(t)
	partitionLedgerSeed(s, l, 0, 4)
	partitionLedgerBody(s, l, 4, 40, 1, 0)
	r := partitionNext(t, s.ScanPartitions("Order", PartitionCoordinateRange{}, nil, ForwardScan()))
	token := partitionToken(t, r)
	parts, err := tuple.Unpack(token)
	if err != nil {
		t.Fatal(err)
	}
	var malformed [][]byte
	for i := range parts {
		mutated := slices.Clone(parts)
		mutated[i] = int64(0)
		if _, integer := parts[i].(int64); integer {
			mutated[i] = []byte{}
		}
		malformed = append(malformed, mutated.Pack())
	}
	for _, change := range []struct {
		i int
		v any
	}{{1, int64(99)}, {2, partitionCheckerKind}, {3, []byte("other")}, {5, "TypedRecord"}, {6, int64(-99)}, {7, []byte("layout")}, {8, true}, {15, true}, {16, []byte{1}}, {17, int64(0)}, {18, []byte{}}, {19, int64(-1)}, {20, true}, {21, int64(1)}, {22, int64(7)}, {23, []byte("outside")}, {24, partitionPositionKey}, {25, []byte("outside")}, {28, partitionPositionEnd}} {
		mutated := slices.Clone(parts)
		mutated[change.i] = change.v
		malformed = append(malformed, mutated.Pack())
	}
	malformed = append(malformed, []byte{}, []byte{0x05, 0x05, 0x00, 0x00}, bytes.Repeat([]byte{0x14}, partitionTokenMaximum+1), token[:len(token)-1], append(bytes.Clone(token), 0x14))
	for i, b := range malformed {
		before := l.reads
		c := s.ScanPartitions("Order", PartitionCoordinateRange{}, b, ForwardScan())
		_, err := c.OnNext(context.Background())
		var parse *ContinuationParseError
		if !errors.As(err, &parse) || l.reads != before {
			t.Fatalf("mutation %d accepted/read storage: %v reads %d->%d", i, err, before, l.reads)
		}
	}
	if _, err := (&partitionContinuation{raw: make([]byte, partitionTokenMaximum+1)}).ToBytes(); err == nil {
		t.Fatal("issued an oversized continuation")
	}
	alias := partitionToken(t, r)
	alias[0] ^= 0xff
	if bytes.Equal(alias, partitionToken(t, r)) {
		t.Fatal("token aliases returned buffer")
	}
	input := bytes.Clone(token)
	c := s.ScanPartitions("Order", PartitionCoordinateRange{}, input, ForwardScan())
	clear(input)
	next := partitionNext(t, c)
	if !next.HasNext() || next.GetValue().Coordinate != 4 {
		t.Fatal("token parser retained caller buffer")
	}
	for _, props := range []ScanProperties{ReverseScan()} {
		before := l.reads
		_, err := s.ScanPartitions("Order", PartitionCoordinateRange{}, token, props).OnNext(context.Background())
		var parse *ContinuationParseError
		if !errors.As(err, &parse) || l.reads != before {
			t.Fatalf("identity mismatch performed I/O: %v", err)
		}
	}
	before := l.reads
	_, err = s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, token, ForwardScan()).OnNext(context.Background())
	var parse *ContinuationParseError
	if !errors.As(err, &parse) || l.reads != before {
		t.Fatalf("cross-kind token performed I/O: %v", err)
	}
}

func TestPartitionDirectoryCancellationAndCachedStops(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"cancel", "ended", "deactivated", "sealed", "context"} {
		for _, cached := range []string{"head", "stop", "eof"} {
			s, l := partitionLedgerStore(t)
			partitionLedgerSeed(s, l, 0, 4)
			partitionLedgerBody(s, l, 4, 40, 1, 0)
			props := ExecuteProperties{}
			if cached == "stop" {
				props.ReturnedRowLimit = 1
			}
			c := s.ScanPartitions("Order", PartitionCoordinateRange{}, nil, NewScanProperties(props))
			partitionNext(t, c)
			if cached == "stop" {
				partitionNext(t, c)
			}
			if cached == "eof" {
				for partitionNext(t, c).HasNext() {
				}
			}
			switch kind {
			case "cancel":
				s.context.Cancel()
			case "ended":
				s.context.partitionReadEnded.Store(true)
			case "deactivated":
				s.context.deactivated.Store(true)
			case "sealed":
				s.context.recordPhysical.mu.Lock()
				s.context.recordPhysical.sealed = true
				s.context.recordPhysical.mu.Unlock()
			case "context":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				s.context.ctx = ctx
			}
			before := l.reads
			for range 2 {
				_, err := c.OnNext(context.Background())
				if err == nil {
					t.Fatalf("%s %s returned cached result after attempt end", kind, cached)
				}
			}
			if l.reads != before {
				t.Fatalf("%s %s read after end", kind, cached)
			}
		}
	}
	for at := 0; at <= 6; at++ {
		s, l := partitionLedgerStore(t)
		partitionLedgerSeed(s, l, 0)
		ctx, cancel := context.WithCancel(context.Background())
		if at == 0 {
			cancel()
		} else {
			l.after = func(n int) {
				if n == at {
					cancel()
				}
			}
		}
		c := s.ScanPartitions("Order", PartitionCoordinateRange{}, nil, ForwardScan())
		_, err := c.OnNext(ctx)
		if !errors.Is(err, context.Canceled) || l.reads != at {
			t.Fatalf("cancel at %d: %v reads=%d", at, err, l.reads)
		}
	}
	for at := 1; at <= 6; at++ {
		s, l := partitionLedgerStore(t)
		partitionLedgerSeed(s, l, 0)
		l.errAt, l.err = at, errors.New("native read failure")
		c := s.ScanPartitions("Order", PartitionCoordinateRange{}, nil, ForwardScan())
		_, err := c.OnNext(context.Background())
		if !errors.Is(err, l.err) || l.reads != at {
			t.Fatalf("backend error at %d became EOF: %v", at, err)
		}
		_, _ = c.OnNext(context.Background())
		if l.reads != at {
			t.Fatal("replayed failed observation")
		}
	}
}

func TestPartitionDirectoryPartialSkipResume(t *testing.T) {
	t.Parallel()
	s, l := partitionLedgerStore(t)
	partitionLedgerSeed(s, l, -3, 0, 4)
	props := NewScanProperties(ExecuteProperties{Skip: 2, ScannedRecordsLimit: 1})
	c := s.ScanPartitions("Order", PartitionCoordinateRange{}, nil, props)
	stop := partitionNext(t, c)
	if stop.GetNoNextReason() != ScanLimitReached {
		t.Fatal("expected partial skip stop")
	}
	// Skip is per invocation, like ordinary record cursors: the caller accounts
	// for the one entry skipped on the previous page.
	c = s.ScanPartitions("Order", PartitionCoordinateRange{}, partitionToken(t, stop), NewScanProperties(ExecuteProperties{Skip: 1}))
	r := partitionNext(t, c)
	if !r.HasNext() || r.GetValue().Coordinate != 4 {
		t.Fatalf("partial skip resume=%+v", r)
	}
}

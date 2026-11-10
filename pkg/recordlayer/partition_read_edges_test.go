package recordlayer

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

func TestPartitionReadersRequireLoadedStoreInfoWithoutIO(t *testing.T) {
	t.Parallel()
	s, l := partitionLedgerStore(t)
	unchecked, err := NewStoreBuilder().SetContext(s.context).SetMetaDataProvider(s.metaData).SetSubspace(s.subspace).Build()
	if err != nil {
		t.Fatal(err)
	}
	before, effects, counter := maps.Clone(l.data), partitionVersionEffectsOf(s.context), s.context.localVersion.Load()
	for _, kind := range []string{"directory", "checker"} {
		var err error
		if kind == "directory" {
			_, err = unchecked.ScanPartitions("Order", PartitionCoordinateRange{}, nil, ForwardScan()).OnNext(context.Background())
		} else {
			_, err = unchecked.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, ForwardScan()).OnNext(context.Background())
		}
		var metadata *PartitionMetadataError
		if !errors.As(err, &metadata) || l.reads != 0 || l.writes != 0 {
			t.Fatalf("%s unchecked Build read/accepted: %v reads=%d writes=%d", kind, err, l.reads, l.writes)
		}
	}
	if !maps.EqualFunc(before, l.data, bytes.Equal) || !partitionReadEffectsEqual(effects, partitionVersionEffectsOf(s.context)) || counter != s.context.localVersion.Load() {
		t.Fatal("unchecked Build refusal changed data/version state")
	}
	// The refusal is not an empty-store refusal: an opened, genuinely virgin
	// store may prove empty using the bounded header-absence integrity probes.
	for _, kind := range []string{"directory", "checker"} {
		start := l.reads
		if kind == "directory" {
			r := partitionNext(t, s.ScanPartitions("Order", PartitionCoordinateRange{}, nil, ForwardScan()))
			if r.HasNext() || r.GetNoNextReason() != SourceExhausted {
				t.Fatal("opened empty directory refused")
			}
		} else {
			r := partitionNext(t, s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, ForwardScan()))
			if r.HasNext() || r.GetNoNextReason() != SourceExhausted {
				t.Fatal("opened empty checker refused")
			}
		}
		if l.reads-start != 4 || l.writes != 0 {
			t.Fatalf("opened empty %s used %d reads", kind, l.reads-start)
		}
	}
}

func TestPartitionCheckerOmittedSuffixAndLegacyBackendError(t *testing.T) {
	t.Parallel()
	for _, format := range []int32{4, 14} {
		for _, backendFailure := range []bool{false, true} {
			s, l := partitionLedgerStore(t)
			s.metaData.splitLongRecords = false
			s.metaData.storeRecordVersions = true
			s.storeHeader.FormatVersion = proto.Int32(format)
			s.storeHeader.OmitUnsplitRecordSuffix = proto.Bool(true)
			partitionLedgerSeed(s, l, 0)
			key := partitionLedgerValidBody(t, s, l, 0, 0, 1, partitionOrder(1, 0, 7))
			pk := tuple.Tuple{"DOCS", int64(0), int64(0), int64(1)}
			raw := l.data[string(key)]
			delete(l.data, string(key))
			l.data[string(s.recordsSubspace.Pack(pk))] = raw
			legacyKey := s.subspace.Sub(RecordVersionKey).Pack(pk)
			l.data[string(legacyKey)] = make([]byte, 12)
			c := s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, ForwardScan()).(*partitionChecker)
			// Complete directory/protocol first so the injected failure belongs to
			// the unsplitter's legacy version hook, not a discovery observation.
			for range 20 {
				r := partitionNext(t, c)
				if r.GetValue().Kind == PartitionConsistencyProtocol && bytes.Equal(c.read.checkpoint.positions[3].key, partitionSubspace(s.subspace, s.metaData.GetRecordType("Order")).Pack(tuple.Tuple{"header"})) {
					break
				}
			}
			if backendFailure {
				// The next call performs protocol EOF, body GetRange, then exact
				// legacy Get. It must preserve a native error rather than call it corruption.
				l.errAt = l.reads + 3
				l.err = errors.New("legacy backend failure")
				_, err := c.OnNext(context.Background())
				var state *PartitionStateError
				if !errors.Is(err, l.err) || errors.As(err, &state) {
					t.Fatalf("format=%d hook error became corruption/EOF: %v", format, err)
				}
			} else {
				items, err := partitionCheckerDrain(t, c)
				if err != nil {
					t.Fatal(err)
				}
				var bodies, versions int
				for _, item := range items {
					if item.Kind == PartitionConsistencyBody {
						bodies++
					}
					if item.Kind == PartitionConsistencyVersion {
						versions++
					}
				}
				if bodies != 1 || versions != 1 {
					t.Fatalf("format=%d omitted suffix work bodies%d versions%d", format, bodies, versions)
				}
			}
			if l.writes != 0 {
				t.Fatal("legacy audit wrote data")
			}
		}
	}
}

// All four union messages are literal eight-byte Order encodings. Their DOCS
// record KVs cost 13+8=21 bytes; native legacy-version KVs cost 12+12=24.
func partitionPhaseFixture(t *testing.T, split bool) (*FDBRecordStore, *partitionReadLedger) {
	t.Helper()
	s, l := partitionLedgerStore(t)
	partitionLedgerSeed(s, l, 0)
	for id := int64(1); id <= 4; id++ {
		pk := tuple.Tuple{"DOCS", int64(0), int64(0), id}
		raw := []byte{0x0a, 0x06, 0x08, byte(id), 0x18, 0x07, 0x30, 0x00}
		if split && id == 3 {
			for i, part := range [][]byte{raw[:3], raw[3:6], raw[6:]} {
				l.data[string(s.recordsSubspace.Pack(appendToTuple(pk, int64(i+1))))] = bytes.Clone(part)
			}
		} else {
			l.data[string(s.recordsSubspace.Pack(appendToTuple(pk, int64(0))))] = raw
		}
		l.data[string(s.subspace.Sub(RecordVersionKey).Pack(pk))] = make([]byte, 12)
	}
	return s, l
}

func partitionPhaseToken(t *testing.T, s *FDBRecordStore, phase string) []byte {
	t.Helper()
	kind := map[string]PartitionConsistencyKind{"protocol": PartitionConsistencyProtocol, "body": PartitionConsistencyBody, "legacy": PartitionConsistencyVersion}[phase]
	c := s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, ForwardScan())
	for range 32 {
		r := partitionNext(t, c)
		if !r.HasNext() {
			t.Fatalf("phase %s had no positive work control", phase)
		}
		if r.GetValue().Kind == kind {
			return partitionToken(t, r)
		}
	}
	t.Fatalf("phase %s was not reached", phase)
	return nil
}

func partitionAssertReadOnly(t *testing.T, s *FDBRecordStore, l *partitionReadLedger, before map[string][]byte, effects partitionVersionEffects, counter int32) {
	t.Helper()
	if l.writes != 0 || !maps.EqualFunc(before, l.data, bytes.Equal) || !partitionReadEffectsEqual(effects, partitionVersionEffectsOf(s.context)) || counter != s.context.localVersion.Load() {
		t.Fatal("partition read changed whole-store/pending/cache/local-version state")
	}
}

// This context's deadline is driven by the fixture's logical clock. The native
// future wait below observes its Done channel, not an unrelated OnNext context.
type partitionWaitContext struct {
	context.Context
	done chan struct{}
	once sync.Once
	err  error
}

func (c *partitionWaitContext) Deadline() (time.Time, bool) { return dst.Epoch.Add(time.Second), true }
func (c *partitionWaitContext) Done() <-chan struct{}       { return c.done }
func (c *partitionWaitContext) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

func (c *partitionWaitContext) finish(err error) { c.once.Do(func() { c.err = err; close(c.done) }) }

func TestPartitionReadersInheritedContextDuringNativeWait(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"directory", "checker", "protocol", "body", "legacy"} {
		for _, ending := range []string{"cancel", "deadline", "live"} {
			t.Run(phase+"/"+ending, func(t *testing.T) {
				s, l := partitionPhaseFixture(t, false)
				var token []byte
				blockAt, wantLiveNative, wantLiveBytes := 1, 6, int64(255)
				wantCancelledBytes := int64(71)
				if phase != "directory" && phase != "checker" {
					token = partitionPhaseToken(t, s, phase)
					blockAt, wantLiveNative, wantLiveBytes = 3, 3, 161
					wantCancelledBytes = 161
					if phase == "body" {
						wantLiveBytes, wantCancelledBytes = 137, 137
					}
					if phase == "legacy" {
						wantLiveNative, wantCancelledBytes = 4, 140
					}
				}
				before, effects, counter := maps.Clone(l.data), partitionVersionEffectsOf(s.context), s.context.localVersion.Load()
				l.reads = 0
				inherited := &partitionWaitContext{Context: context.Background(), done: make(chan struct{})}
				s.context.ctx = inherited
				entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
				l.after = func(n int) {
					if n == blockAt {
						close(entered)
						select {
						case <-inherited.Done():
						case <-release:
						}
					}
				}
				state := NewScanLimiterStateIn(s.context.Env())
				state.AddRecordScanned()
				props := NewScanProperties(ExecuteProperties{ScannedRecordsLimit: 1, ScannedBytesLimit: 1, TimeLimit: time.Nanosecond, ScanState: state})
				type outcome struct {
					hasNext bool
					err     error
				}
				result := make(chan outcome, 1)
				var next func() outcome
				if phase == "directory" {
					c := s.ScanPartitions("Order", PartitionCoordinateRange{}, nil, props)
					next = func() outcome { r, err := c.OnNext(context.Background()); return outcome{r.HasNext(), err} }
				} else {
					c := s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, token, props)
					next = func() outcome { r, err := c.OnNext(context.Background()); return outcome{r.HasNext(), err} }
				}
				go func() { defer close(done); result <- next() }()
				t.Cleanup(func() {
					inherited.finish(context.Canceled)
					select {
					case <-done:
					case <-time.After(10 * time.Second):
						t.Error("native wait goroutine did not terminate")
					}
				})
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					t.Fatal("native future never entered its bounded wait")
				}
				if l.reads != blockAt {
					t.Fatalf("wait issued %d observations; want %d", l.reads, blockAt)
				}
				var wantErr error
				switch ending {
				case "live":
					close(release)
				case "cancel":
					wantErr = context.Canceled
					inherited.finish(wantErr)
				case "deadline":
					s.context.Env().Clock.(*dst.SimClock).Advance(time.Second)
					wantErr = context.DeadlineExceeded
					inherited.finish(wantErr)
				}
				var got outcome
				select {
				case got = <-result:
				case <-time.After(10 * time.Second):
					t.Fatal("inherited context did not release native wait")
				}
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("completed wait leaked its reader")
				}
				if ending == "live" {
					if got.err != nil || !got.hasNext || l.reads != wantLiveNative || state.RecordsScanned() != wantLiveNative+1 || state.BytesScanned() != wantLiveBytes {
						t.Fatalf("live control=%+v native=%d attempts=%d bytes=%d", got, l.reads, state.RecordsScanned(), state.BytesScanned())
					}
				} else {
					if !errors.Is(got.err, wantErr) || got.hasNext || l.reads != blockAt || state.RecordsScanned() != blockAt+1 || state.BytesScanned() != wantCancelledBytes {
						t.Fatalf("initial allowance bypassed inherited end: %+v native=%d attempts=%d bytes=%d", got, l.reads, state.RecordsScanned(), state.BytesScanned())
					}
					for range 2 {
						if got = next(); !errors.Is(got.err, wantErr) || got.hasNext {
							t.Fatalf("ended reader returned %+v", got)
						}
					}
					if l.reads != blockAt || state.RecordsScanned() != blockAt+1 {
						t.Fatal("ended reader added observations")
					}
				}
				partitionAssertReadOnly(t, s, l, before, effects, counter)
			})
		}
	}
}

func TestPartitionCheckerResumedPhaseBudgetsAndProgress(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"protocol", "body", "legacy"} {
		for _, tc := range []struct {
			name   string
			props  ExecuteProperties
			reason NoNextReason
		}{
			{"records", ExecuteProperties{ScannedRecordsLimit: 1}, ScanLimitReached},
			{"bytes", ExecuteProperties{ScannedBytesLimit: 1}, ByteLimitReached},
			{"time", ExecuteProperties{TimeLimit: time.Nanosecond}, TimeLimitReached},
			{"strict-records", ExecuteProperties{ScannedRecordsLimit: 3, FailOnScanLimitReached: true}, ScanLimitReached},
			{"fail-bytes", ExecuteProperties{ScannedBytesLimit: 1, FailOnScanLimitReached: true}, ByteLimitReached},
			{"fail-time", ExecuteProperties{TimeLimit: time.Nanosecond, FailOnScanLimitReached: true}, TimeLimitReached},
		} {
			t.Run(phase+"/"+tc.name, func(t *testing.T) {
				s, l := partitionPhaseFixture(t, false)
				input := partitionPhaseToken(t, s, phase)
				want, err := partitionCheckerDrain(t, s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, input, ForwardScan()))
				if err != nil || len(want) == 0 {
					t.Fatalf("empty/failed remaining-work control: %v", err)
				}
				before, effects, counter := maps.Clone(l.data), partitionVersionEffectsOf(s.context), s.context.localVersion.Load()
				l.reads = 0
				l.after = func(int) { s.context.Env().Clock.(*dst.SimClock).Advance(time.Second) }
				state := NewScanLimiterStateIn(s.context.Env())
				props := tc.props
				props.ScanState = state
				c := s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, input, NewScanProperties(props))
				r, err := c.OnNext(context.Background())
				var got []PartitionConsistencyResult
				last := input
				if err == nil {
					if !r.HasNext() {
						t.Fatal("initial phase bundle emitted no work")
					}
					got = append(got, r.GetValue())
					last = partitionToken(t, r)
					r, err = c.OnNext(context.Background())
				}
				wantNative, wantBytes := 3, int64(161)
				if phase == "body" {
					wantBytes = 137
				}
				if phase == "legacy" {
					wantNative, wantBytes = 4, 161
					if tc.name == "strict-records" {
						wantNative, wantBytes = 3, 140
						if len(got) != 0 {
							t.Fatal("strict legacy read advanced before validating its body")
						}
					}
				}
				if tc.props.FailOnScanLimitReached {
					var limit *ScanLimitReachedError
					if !errors.As(err, &limit) || limit.Reason != tc.reason {
						t.Fatalf("phase fail=%v", err)
					}
				} else {
					if err != nil || r.HasNext() || r.GetNoNextReason() != tc.reason {
						t.Fatalf("phase stop=%+v %v", r, err)
					}
					last = partitionToken(t, r)
				}
				for range 2 {
					_, _ = c.OnNext(context.Background())
				}
				if l.reads != wantNative || state.RecordsScanned() != wantNative+1 || state.BytesScanned() != wantBytes {
					t.Fatalf("phase=%s mode=%s native=%d attempts=%d bytes=%d; want %d/%d/%d", phase, tc.name, l.reads, state.RecordsScanned(), state.BytesScanned(), wantNative, wantNative+1, wantBytes)
				}
				if tc.props.FailOnScanLimitReached {
					rest, err := partitionCheckerDrain(t, s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, last, ForwardScan()))
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, rest...)
				} else {
					if bytes.Equal(last, input) {
						t.Fatal("phase bundle made no continuation progress")
					}
					for pages := 0; ; pages++ {
						if pages >= 32 {
							t.Fatal("phase budget pagination did not finish")
						}
						start := bytes.Clone(last)
						props := tc.props
						props.ScanState = NewScanLimiterStateIn(s.context.Env())
						page := s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, start, NewScanProperties(props))
						done := false
						for range 32 {
							r := partitionNext(t, page)
							if r.HasNext() {
								got = append(got, r.GetValue())
								continue
							}
							if r.GetNoNextReason() == SourceExhausted {
								done = true
							} else {
								if r.GetNoNextReason() != tc.reason || r.GetContinuation().IsEnd() {
									t.Fatalf("wrong phase stop: %+v", r)
								}
								last = partitionToken(t, r)
								if bytes.Equal(start, last) {
									t.Fatal("zero-output phase stop did not advance")
								}
							}
							break
						}
						if done {
							break
						}
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("phase budget skipped/repeated healthy work: got=%v want=%v", got, want)
				}
				partitionAssertReadOnly(t, s, l, before, effects, counter)
			})
		}
	}
}

func TestPartitionCheckerResumedPhaseCancellation(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"protocol", "body", "legacy"} {
		for _, at := range []int{0, 3} {
			t.Run(fmt.Sprintf("%s/observation-%d", phase, at), func(t *testing.T) {
				s, l := partitionPhaseFixture(t, false)
				token := partitionPhaseToken(t, s, phase)
				before, effects, counter := maps.Clone(l.data), partitionVersionEffectsOf(s.context), s.context.localVersion.Load()
				l.reads = 0
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if at == 0 {
					cancel()
				} else {
					l.after = func(n int) {
						if n == at {
							cancel()
						}
					}
				}
				state := NewScanLimiterStateIn(s.context.Env())
				c := s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, token, NewScanProperties(ExecuteProperties{ScannedRecordsLimit: 1, ScannedBytesLimit: 1, ScanState: state}))
				r, err := c.OnNext(ctx)
				if !errors.Is(err, context.Canceled) || r.HasNext() {
					t.Fatalf("phase cancellation=%+v %v", r, err)
				}
				wantBytes := int64(0)
				if at != 0 {
					wantBytes = 161
					if phase == "body" {
						wantBytes = 137
					}
					if phase == "legacy" {
						wantBytes = 140
					}
				}
				for range 2 {
					_, _ = c.OnNext(ctx)
				}
				if l.reads != at || state.RecordsScanned() != at || state.BytesScanned() != wantBytes {
					t.Fatalf("cancel native=%d attempts=%d bytes=%d; want %d/%d/%d", l.reads, state.RecordsScanned(), state.BytesScanned(), at, at, wantBytes)
				}
				partitionAssertReadOnly(t, s, l, before, effects, counter)
			})
		}
	}
}

func TestPartitionCheckerProtocolPartialObservationProgress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		props  ExecuteProperties
		reason NoNextReason
	}{
		{"records", ExecuteProperties{ScannedRecordsLimit: 4}, ScanLimitReached},
		{"bytes", ExecuteProperties{ScannedBytesLimit: 162}, ByteLimitReached},
		{"time", ExecuteProperties{TimeLimit: 4 * time.Second}, TimeLimitReached},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, l := partitionPhaseFixture(t, false)
			input := partitionPhaseToken(t, s, "protocol")
			before, effects, counter := maps.Clone(l.data), partitionVersionEffectsOf(s.context), s.context.localVersion.Load()
			l.reads = 0
			l.after = func(int) { s.context.Env().Clock.(*dst.SimClock).Advance(time.Second) }
			state := NewScanLimiterStateIn(s.context.Env())
			tc.props.ScanState = state
			c := s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, input, NewScanProperties(tc.props))
			first := partitionNext(t, c)
			if first.GetValue().Kind != PartitionConsistencyProtocol {
				t.Fatal("missing protocol epoch work")
			}
			lastSafe := partitionToken(t, first)
			stop := partitionNext(t, c)
			if stop.HasNext() || stop.GetNoNextReason() != tc.reason || !bytes.Equal(lastSafe, partitionToken(t, stop)) {
				t.Fatal("partly observed fence advanced its safe position")
			}
			if l.reads != 4 || state.RecordsScanned() != 5 || state.BytesScanned() != 208 {
				t.Fatalf("partial fence native=%d attempts=%d bytes=%d; want 4/5/208", l.reads, state.RecordsScanned(), state.BytesScanned())
			}
			partitionNext(t, c)
			if l.reads != 4 || state.RecordsScanned() != 5 {
				t.Fatal("cached partial stop added I/O")
			}
			l.reads = 0
			pageState := NewScanLimiterStateIn(s.context.Env())
			page := s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, partitionToken(t, stop), NewScanProperties(ExecuteProperties{ReturnedRowLimit: 1, ScannedRecordsLimit: 1, ScanState: pageState}))
			item := partitionNext(t, page)
			if !item.HasNext() || item.GetValue().Kind != PartitionConsistencyFence || bytes.Equal(lastSafe, partitionToken(t, item)) {
				t.Fatal("resumed fence work made no progress")
			}
			rowStop := partitionNext(t, page)
			if rowStop.GetNoNextReason() != ReturnLimitReached || l.reads != 4 || pageState.RecordsScanned() != 4 || pageState.BytesScanned() != 210 {
				t.Fatalf("resumed fence native=%d attempts=%d bytes=%d; want 4/4/210", l.reads, pageState.RecordsScanned(), pageState.BytesScanned())
			}
			partitionAssertReadOnly(t, s, l, before, effects, counter)
		})
	}
}

func partitionAssertSplitFixture(t *testing.T, l *partitionReadLedger) {
	t.Helper()
	for _, tc := range []struct {
		key, value        string
		keyBytes, kvBytes int
	}{
		{"150102444f4353001414150214", "0a06080218073000", 13, 21},
		{"150102444f435300141415031501", "0a0608", 14, 17},
		{"150102444f435300141415031502", "031807", 14, 17},
		{"150102444f435300141415031503", "3000", 14, 16},
		{"150802444f43530014141503", "000000000000000000000000", 12, 24},
	} {
		key, err := hex.DecodeString(tc.key)
		if err != nil {
			t.Fatal(err)
		}
		value, found := l.data[string(key)]
		if !found || hex.EncodeToString(value) != tc.value || len(key) != tc.keyBytes || len(key)+len(value) != tc.kvBytes {
			t.Fatalf("literal fixture key=%s found=%v value=%x keyBytes=%d kvBytes=%d; want %s/%d/%d", tc.key, found, value, len(key), len(key)+len(value), tc.value, tc.keyBytes, tc.kvBytes)
		}
	}
	prefix, err := hex.DecodeString("150102444f43530014141503")
	if err != nil {
		t.Fatal(err)
	}
	var fragments, fragmentBytes int
	for key, value := range l.data {
		if bytes.HasPrefix([]byte(key), prefix) {
			fragments++
			fragmentBytes += len(key) + len(value)
		}
	}
	if len(prefix) != 12 || fragments != 3 || fragmentBytes != 50 {
		t.Fatalf("split fixture prefixBytes=%d fragments=%d totalBytes=%d; want 12/3/50", len(prefix), fragments, fragmentBytes)
	}
}

// A positive suffix occupies two tuple bytes, versus one for suffix zero.
// Split KVs cost 17+17+16; body lookahead adds 21, while legacy audit reads EOF.
func TestPartitionCheckerSplitAtomicBudgetsAfterInitialWork(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"body", "legacy"} {
		for _, budget := range []string{"records", "bytes", "time"} {
			for _, fail := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/fail=%v", phase, budget, fail), func(t *testing.T) {
					s, l := partitionPhaseFixture(t, true)
					partitionAssertSplitFixture(t, l)
					input := partitionPhaseToken(t, s, phase)
					want, err := partitionCheckerDrain(t, s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, input, ForwardScan()))
					if err != nil || len(want) == 0 {
						t.Fatalf("split positive control=%v", err)
					}
					before, effects, counter := maps.Clone(l.data), partitionVersionEffectsOf(s.context), s.context.localVersion.Load()
					l.reads = 0
					l.after = func(int) { s.context.Env().Clock.(*dst.SimClock).Advance(time.Second) }
					limit, byteLimit, wantFirstNative, wantNative, wantFailNative := 4, int64(138), 3, 7, 4
					wantFirstBytes, wantBytes, wantFailBytes := int64(137), int64(208), int64(154)
					if phase == "legacy" {
						limit, byteLimit, wantFirstNative, wantNative, wantFailNative = 6, 186, 4, 9, 6
						wantFirstBytes, wantBytes, wantFailBytes = 161, 235, 202
					}
					state := NewScanLimiterStateIn(s.context.Env())
					props := ExecuteProperties{FailOnScanLimitReached: fail, ScanState: state}
					reason := ScanLimitReached
					switch budget {
					case "records":
						props.ScannedRecordsLimit = limit
					case "bytes":
						props.ScannedBytesLimit = byteLimit
						reason = ByteLimitReached
					case "time":
						props.TimeLimit = time.Duration(limit) * time.Second
						reason = TimeLimitReached
					}
					c := s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, input, NewScanProperties(props))
					first := partitionNext(t, c)
					if !first.HasNext() || first.GetValue().PhysicalKey[3] != int64(2) || l.reads != wantFirstNative || state.BytesScanned() != wantFirstBytes {
						t.Fatalf("initial non-split work=%+v native=%d bytes=%d", first, l.reads, state.BytesScanned())
					}
					got := []PartitionConsistencyResult{first.GetValue()}
					last := partitionToken(t, first)
					second, err := c.OnNext(context.Background())
					if fail {
						var limitErr *ScanLimitReachedError
						if !errors.As(err, &limitErr) || limitErr.Reason != reason || second.HasNext() {
							t.Fatalf("split fail=%+v %v", second, err)
						}
						wantNative, wantBytes = wantFailNative, wantFailBytes
					} else {
						if err != nil || !second.HasNext() || second.GetValue().PhysicalKey[3] != int64(3) {
							t.Fatalf("split did not finish atomically: %+v %v", second, err)
						}
						got = append(got, second.GetValue())
						last = partitionToken(t, second)
						stop := partitionNext(t, c)
						if stop.HasNext() || stop.GetNoNextReason() != reason || !bytes.Equal(last, partitionToken(t, stop)) {
							t.Fatal("post-split stop lost its safe continuation")
						}
					}
					for range 2 {
						_, _ = c.OnNext(context.Background())
					}
					if l.reads != wantNative || state.RecordsScanned() != wantNative+1 || state.BytesScanned() != wantBytes {
						t.Fatalf("split native=%d attempts=%d bytes=%d; want %d/%d/%d", l.reads, state.RecordsScanned(), state.BytesScanned(), wantNative, wantNative+1, wantBytes)
					}
					rest, err := partitionCheckerDrain(t, s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, last, ForwardScan()))
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, rest...)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("split checkpoint skipped/repeated work: got=%v want=%v", got, want)
					}
					partitionAssertReadOnly(t, s, l, before, effects, counter)
				})
			}
		}
	}
}

func TestPartitionDirectoryUnlimitedAndRowLimitPriority(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, -1} {
		s, l := partitionLedgerStore(t)
		partitionLedgerSeed(s, l, 0, 1)
		c := s.ScanPartitions("Order", PartitionCoordinateRange{}, nil, NewScanProperties(ExecuteProperties{ScannedRecordsLimit: limit, ScannedBytesLimit: int64(limit)}))
		var count int
		for partitionNext(t, c).HasNext() {
			count++
		}
		if count != 2 || l.writes != 0 {
			t.Fatalf("unlimited %d count=%d", limit, count)
		}
	}
	s, l := partitionLedgerStore(t)
	partitionLedgerSeed(s, l, 0, 1)
	state := NewScanLimiterStateIn(s.context.Env())
	c := s.ScanPartitions("Order", PartitionCoordinateRange{}, nil, NewScanProperties(ExecuteProperties{ReturnedRowLimit: 1, ScannedRecordsLimit: 1, ScannedBytesLimit: 1, ScanState: state}))
	partitionNext(t, c)
	r := partitionNext(t, c)
	if r.GetNoNextReason() != ReturnLimitReached || l.reads != 6 || state.RecordsScanned() != 6 {
		t.Fatalf("row limit lost priority: %+v native%d attempts%d", r, l.reads, state.RecordsScanned())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !c.IsClosed() {
		t.Fatal("Close not retained")
	}
	before := l.reads
	_, err := c.OnNext(context.Background())
	if err == nil || l.reads != before {
		t.Fatal("closed cursor read/returned cached stop")
	}
}

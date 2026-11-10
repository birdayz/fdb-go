package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"testing"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

func partitionLedgerValidBody(t *testing.T, s *FDBRecordStore, l *partitionReadLedger, p, source, id int64, record proto.Message) []byte {
	t.Helper()
	rt, err := s.metaData.GetRecordTypeForDescriptor(record.ProtoReflect().Descriptor())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := serializeUnionOver(record, rt, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := s.recordsSubspace.Pack(tuple.Tuple{"DOCS", p, source, id, int64(0)})
	l.data[string(key)] = raw
	return key
}

func partitionCheckerDrain(t *testing.T, c RecordCursor[PartitionConsistencyResult]) ([]PartitionConsistencyResult, error) {
	t.Helper()
	var out []PartitionConsistencyResult
	for range 1000 {
		r, err := c.OnNext(context.Background())
		if err != nil {
			return out, err
		}
		if !r.HasNext() {
			if r.GetNoNextReason() != SourceExhausted {
				return out, fmt.Errorf("unexpected checker stop %v", r.GetNoNextReason())
			}
			return out, nil
		}
		out = append(out, r.GetValue())
	}
	return out, fmt.Errorf("non-progressing checker")
}

func TestPartitionCheckerHealthyPagingAndReadOnly(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		for _, pageSize := range []int{0, 1, 2} {
			s, l := partitionLedgerStore(t)
			partitionLedgerSeed(s, l, -3, 0, 4)
			partitionLedgerValidBody(t, s, l, -3, -30, 1, partitionOrder(1, -30, 7))
			partitionLedgerValidBody(t, s, l, 4, 40, 2, partitionOrder(2, 40, 7))
			ss := partitionSubspace(s.subspace, s.metaData.GetRecordType("Order"))
			l.data[string(ss.Pack(tuple.Tuple{"fence", int64(2)}))] = (tuple.Tuple{int64(9), false}).Pack()
			before, effects, counter := maps.Clone(l.data), partitionVersionEffectsOf(s.context), s.context.localVersion.Load()
			var token []byte
			counts := map[PartitionConsistencyKind]int{}
			for pages := 0; ; pages++ {
				if pages > 30 {
					t.Fatal("checker pagination did not progress")
				}
				c := s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, token, NewScanProperties(ExecuteProperties{ReturnedRowLimit: pageSize}).WithReverse(reverse))
				var done bool
				for {
					r := partitionNext(t, c)
					if r.HasNext() {
						counts[r.GetValue().Kind]++
						continue
					}
					done = r.GetNoNextReason() == SourceExhausted
					if !done {
						if r.GetNoNextReason() != ReturnLimitReached {
							t.Fatalf("reason=%v", r.GetNoNextReason())
						}
						token = partitionToken(t, r)
					}
					reads := l.reads
					partitionNext(t, c)
					if l.reads != reads {
						t.Fatal("cached checker stop performed I/O")
					}
					break
				}
				if done {
					break
				}
			}
			want := map[PartitionConsistencyKind]int{PartitionConsistencyDirectory: 3, PartitionConsistencyProtocol: 5, PartitionConsistencyFence: 4, PartitionConsistencyBody: 2}
			if !maps.Equal(counts, want) {
				t.Fatalf("reverse=%v page=%d work=%v want%v", reverse, pageSize, counts, want)
			}
			if l.writes != 0 || !maps.EqualFunc(before, l.data, bytes.Equal) || !partitionReadEffectsEqual(effects, partitionVersionEffectsOf(s.context)) || counter != s.context.localVersion.Load() {
				t.Fatal("checker changed store/pending/cache/local-version state")
			}
		}
	}
}

func TestPartitionCheckerCorruptionCensus(t *testing.T) {
	t.Parallel()
	for _, corruption := range []string{"unknown-protocol", "malformed-protocol", "bad-header", "bad-epoch", "active-zero", "fence-missing", "fence-inactive", "fence-generation", "live-fence-only", "malformed-tombstone", "out-of-domain", "legacy-orphan", "legacy-malformed", "second-body", "null-source", "wrong-source", "wrong-id", "wrong-type", "bad-PK-type", "bad-suffix-type", "bad-coordinate-type", "unsplit-tail", "split-gap", "split-start", "fragment-extra", "inline-only", "invalid-inline"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%v", corruption, reverse), func(t *testing.T) {
				s, l := partitionLedgerStore(t)
				partitionLedgerSeed(s, l, 0)
				partitionLedgerValidBody(t, s, l, 0, 0, 1, partitionOrder(1, 0, 7))
				ss := partitionSubspace(s.subspace, s.metaData.GetRecordType("Order"))
				pk := tuple.Tuple{"DOCS", int64(0), int64(0), int64(2)}
				switch corruption {
				case "unknown-protocol":
					l.data[string(ss.Pack(tuple.Tuple{"mystery", int64(0)}))] = []byte{1}
				case "malformed-protocol":
					l.data[string(ss.Pack(tuple.Tuple{"fence", int64(0), "extra"}))] = []byte{1}
				case "bad-header":
					l.data[string(ss.Pack(tuple.Tuple{"header"}))] = []byte{0xff}
				case "bad-epoch":
					l.data[string(ss.Pack(tuple.Tuple{"epoch"}))] = []byte{0x14}
				case "active-zero":
					l.data[string(ss.Pack(tuple.Tuple{"active", int64(0)}))] = []byte{0x14}
				case "fence-missing":
					delete(l.data, string(ss.Pack(tuple.Tuple{"fence", int64(0)})))
				case "fence-inactive":
					l.data[string(ss.Pack(tuple.Tuple{"fence", int64(0)}))] = (tuple.Tuple{int64(1), false}).Pack()
				case "fence-generation":
					l.data[string(ss.Pack(tuple.Tuple{"fence", int64(0)}))] = (tuple.Tuple{int64(2), true}).Pack()
				case "live-fence-only":
					l.data[string(ss.Pack(tuple.Tuple{"fence", int64(2)}))] = (tuple.Tuple{int64(1), true}).Pack()
				case "malformed-tombstone":
					l.data[string(ss.Pack(tuple.Tuple{"fence", int64(2)}))] = (tuple.Tuple{int64(0), false}).Pack()
				case "out-of-domain":
					l.data[string(ss.Pack(tuple.Tuple{"fence", int64(math.MaxInt64)}))] = (tuple.Tuple{int64(1), false}).Pack()
				case "legacy-orphan":
					l.data[string(s.subspace.Sub(RecordVersionKey).Pack(tuple.Tuple{"DOCS", int64(3), int64(30), int64(2)}))] = make([]byte, 12)
				case "legacy-malformed":
					l.data[string(s.subspace.Sub(RecordVersionKey).Pack(pk))] = []byte{1}
				case "second-body":
					l.data[string(s.recordsSubspace.Pack(appendToTuple(pk, int64(0))))] = []byte{0xff}
				case "null-source":
					partitionLedgerValidBody(t, s, l, 0, 0, 2, &demopb.Order{OrderId: proto.Int64(2)})
				case "wrong-source":
					partitionLedgerValidBody(t, s, l, 0, 0, 2, partitionOrder(2, 1, 7))
				case "wrong-id":
					partitionLedgerValidBody(t, s, l, 0, 0, 2, partitionOrder(3, 0, 7))
				case "wrong-type":
					partitionLedgerValidBody(t, s, l, 0, 0, 2, &demopb.Customer{CustomerId: proto.Int64(2)})
				case "bad-PK-type":
					key := partitionLedgerValidBody(t, s, l, 0, 0, 2, partitionOrder(2, 0, 7))
					raw := l.data[string(key)]
					delete(l.data, string(key))
					l.data[string(s.recordsSubspace.Pack(tuple.Tuple{"DOCS", int64(0), int64(0), "id", int64(0)}))] = raw
				case "bad-suffix-type":
					l.data[string(s.recordsSubspace.Pack(append(slices.Clone(pk), "suffix")))] = []byte{1}
				case "bad-coordinate-type":
					l.data[string(s.recordsSubspace.Pack(tuple.Tuple{"DOCS", "coordinate", int64(0), int64(2), int64(0)}))] = []byte{1}
				case "unsplit-tail":
					partitionLedgerValidBody(t, s, l, 0, 0, 2, partitionOrder(2, 0, 7))
					l.data[string(s.recordsSubspace.Pack(appendToTuple(pk, int64(2))))] = []byte{1}
				case "split-gap":
					l.data[string(s.recordsSubspace.Pack(appendToTuple(pk, int64(1))))] = []byte{1}
					l.data[string(s.recordsSubspace.Pack(appendToTuple(pk, int64(3))))] = []byte{2}
				case "split-start":
					l.data[string(s.recordsSubspace.Pack(appendToTuple(pk, int64(2))))] = []byte{1}
				case "fragment-extra":
					l.data[string(s.recordsSubspace.Pack(append(slices.Clone(pk), int64(1), "tail")))] = []byte{1}
				case "inline-only":
					l.data[string(s.recordsSubspace.Pack(appendToTuple(pk, int64(-1))))] = make([]byte, 13)
				case "invalid-inline":
					partitionLedgerValidBody(t, s, l, 0, 0, 2, partitionOrder(2, 0, 7))
					l.data[string(s.recordsSubspace.Pack(appendToTuple(pk, int64(-1))))] = []byte{1}
				}
				before, effects, counter := maps.Clone(l.data), partitionVersionEffectsOf(s.context), s.context.localVersion.Load()
				c := s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, ForwardScan().WithReverse(reverse))
				_, err := partitionCheckerDrain(t, c)
				var state *PartitionStateError
				if !errors.As(err, &state) {
					t.Fatalf("%s escaped or wrong error: %v", corruption, err)
				}
				reads := l.reads
				_, _ = c.OnNext(context.Background())
				if l.reads != reads {
					t.Fatal("corruption result was not cached")
				}
				if l.writes != 0 || !maps.EqualFunc(before, l.data, bytes.Equal) || !partitionReadEffectsEqual(effects, partitionVersionEffectsOf(s.context)) || counter != s.context.localVersion.Load() {
					t.Fatal("corruption audit mutated state")
				}
			})
		}
	}
}

func TestPartitionCheckerSharedBudgetAndLifecycle(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		s, l := partitionLedgerStore(t)
		partitionLedgerSeed(s, l, 0, 1)
		state := NewScanLimiterStateIn(s.context.Env())
		c := s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, NewScanProperties(ExecuteProperties{ScannedRecordsLimit: 1, FailOnScanLimitReached: fail, ScanState: state}))
		r, err := c.OnNext(context.Background())
		if fail {
			var limit *ScanLimitReachedError
			if !errors.As(err, &limit) || l.reads != 1 || state.RecordsScanned() != 2 {
				t.Fatalf("strict initial attempt=%+v %v reads%d", r, err, l.reads)
			}
		} else {
			if err != nil || !r.HasNext() {
				t.Fatalf("initial=%+v %v", r, err)
			}
			r = partitionNext(t, c)
			if r.GetNoNextReason() != ScanLimitReached || l.reads != 6 || state.RecordsScanned() != 7 {
				t.Fatalf("renewed checker allowance=%+v reads%d attempts%d", r, l.reads, state.RecordsScanned())
			}
		}
	}
	for _, cached := range []string{"head", "stop", "eof"} {
		s, l := partitionLedgerStore(t)
		partitionLedgerSeed(s, l, 0, 4)
		partitionLedgerValidBody(t, s, l, 4, 40, 1, partitionOrder(1, 40, 7))
		props := ExecuteProperties{}
		if cached == "stop" {
			props.ReturnedRowLimit = 1
		}
		c := s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, NewScanProperties(props))
		partitionNext(t, c)
		if cached == "stop" {
			partitionNext(t, c)
		}
		if cached == "eof" {
			if _, err := partitionCheckerDrain(t, c); err != nil {
				t.Fatal(err)
			}
		}
		s.context.Cancel()
		reads := l.reads
		for range 2 {
			_, err := c.OnNext(context.Background())
			if err == nil {
				t.Fatalf("%s returned after Cancel", cached)
			}
		}
		if l.reads != reads {
			t.Fatal("ended checker performed I/O")
		}
	}
	s, l := partitionLedgerStore(t)
	partitionLedgerSeed(s, l, 0)
	failure := &PartitionMutationError{Cause: errors.New("writer failed")}
	state := s.partitionContextState()
	state.mu.Lock()
	state.failed = failure
	state.mu.Unlock()
	for _, kind := range []int64{partitionDirectoryKind, partitionCheckerKind} {
		before := l.reads
		var err error
		if kind == partitionDirectoryKind {
			_, err = s.ScanPartitions("Order", PartitionCoordinateRange{}, nil, ForwardScan()).OnNext(context.Background())
		} else {
			_, err = s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, ForwardScan()).OnNext(context.Background())
		}
		if !errors.Is(err, failure) || l.reads != before {
			t.Fatalf("failed partition state passed/read: %v", err)
		}
	}
}

func TestPartitionCheckerConsumedSuffixAndPhaseTokens(t *testing.T) {
	t.Parallel()
	s, l := partitionLedgerStore(t)
	partitionLedgerSeed(s, l, 0)
	partitionLedgerValidBody(t, s, l, 0, 0, 1, partitionOrder(1, 0, 7))
	c := s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, ForwardScan())
	var token []byte
	for range 20 {
		r := partitionNext(t, c)
		if !r.HasNext() {
			t.Fatal("body proof missing")
		}
		if r.GetValue().Kind == PartitionConsistencyBody {
			token = partitionToken(t, r)
			break
		}
	}
	if token == nil {
		t.Fatal("no body token")
	}
	parts, err := tuple.Unpack(token)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct {
		i int
		v any
	}{{21, int64(4)}, {28, partitionPositionStart}, {30, partitionPositionEnd}, {31, s.recordsSubspace.Pack(tuple.Tuple{"DOCS", int64(0), int64(0), int64(1), "bad"})}, {31, s.recordsSubspace.Pack(tuple.Tuple{"DOCS", int64(0), int64(0), int64(1), int64(-1)})}, {32, partitionPositionEnd}} {
		p := slices.Clone(parts)
		p[mutation.i] = mutation.v
		before := l.reads
		_, err = s.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, p.Pack(), ForwardScan()).OnNext(context.Background())
		var parse *ContinuationParseError
		if !errors.As(err, &parse) || l.reads != before {
			t.Fatalf("checker token mutation %d accepted/read: %v", mutation.i, err)
		}
	}
}

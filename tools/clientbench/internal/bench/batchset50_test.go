package bench

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

// This recorder checks harness dispatch and operation shape only. It is not an
// FDB implementation or a substitute for real-backend persistence validation.
type batchShapeRecorder struct {
	readOnly []bool
	sets     []KV
	gets     [][]byte
	values   map[string][]byte
}

func (r *batchShapeRecorder) Transact(readOnly bool, fn func(Transaction) error) error {
	r.readOnly = append(r.readOnly, readOnly)
	return fn(r)
}

func (r *batchShapeRecorder) Local(func(Transaction) error) error {
	return fmt.Errorf("unexpected Local call")
}

func (r *batchShapeRecorder) Counters() ClientCounters {
	return ClientCounters{Observation: "unobserved"}
}
func (r *batchShapeRecorder) Close() {}
func (r *batchShapeRecorder) Get(key []byte) ([]byte, error) {
	r.gets = append(r.gets, bytes.Clone(key))
	return bytes.Clone(r.values[string(key)]), nil
}

func (r *batchShapeRecorder) Set(key, value []byte) {
	r.sets = append(r.sets, KV{bytes.Clone(key), bytes.Clone(value)})
	if r.values == nil {
		r.values = make(map[string][]byte)
	}
	r.values[string(key)] = bytes.Clone(value)
}

func (r *batchShapeRecorder) Range([]byte, []byte, int) ([]KV, error) {
	return nil, fmt.Errorf("unexpected Range call")
}

func (r *batchShapeRecorder) ReadVersion() (int64, error) {
	return 0, fmt.Errorf("unexpected explicit ReadVersion call")
}

func TestBatchSet50OperationShape(t *testing.T) {
	t.Parallel()
	const prefix = "clientbench/distinct-arm/"
	d := Prepare(prefix, 2, 3, 5)
	d.PrepareBatchSet50(prefix)
	r := &batchShapeRecorder{}
	attempts, err := d.Operation(r, "batchset50", 1, d.Measure[3])
	if err != nil || attempts != 1 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
	if len(r.readOnly) != 1 || r.readOnly[0] || len(r.sets) != 50 || len(r.gets) != 0 {
		t.Fatalf("want exactly one write transaction, 50 Sets and no Gets: tx=%v sets=%d gets=%d", r.readOnly, len(r.sets), len(r.gets))
	}
	keys, values := map[string]bool{}, map[string]bool{}
	for slot, kv := range r.sets {
		wantKey := fmt.Sprintf("%sbatchset50/0001/%04d", prefix, slot)
		if string(kv.Key) != wantKey || keys[string(kv.Key)] {
			t.Fatalf("slot %d: nonunique or wrong namespace key %q", slot, kv.Key)
		}
		keys[string(kv.Key)] = true
		if len(kv.Value) != 100 || values[string(kv.Value)] {
			t.Fatalf("slot %d: nonunique or wrong-sized value", slot)
		}
		values[string(kv.Value)] = true
		if kv.Value[0] != 'm' || binary.LittleEndian.Uint64(kv.Value[1:9]) != 1 || binary.LittleEndian.Uint64(kv.Value[9:17]) != 3 || binary.LittleEndian.Uint64(kv.Value[17:25]) != uint64(slot) {
			t.Fatalf("slot %d: wrong phase/worker/operation/slot identity", slot)
		}
		for i := 25; i < 100; i++ {
			if kv.Value[i] != byte((i*31+17)%251) {
				t.Fatalf("slot %d byte %d: wrong payload", slot, i)
			}
		}
	}
}

func TestBatchSet50PreparationIsOptInAndDistinct(t *testing.T) {
	t.Parallel()
	const prefix = "clientbench/batch-preparation/"
	d := Prepare(prefix, 3, 4, 5)
	if d.BatchWrites != nil || d.BatchInitial != nil || len(d.Warmup[0]) != 100 || len(d.Measure[0]) != 100 {
		t.Fatal("original dataset changed without batch opt-in")
	}
	d.PrepareBatchSet50(prefix)
	if len(d.BatchWrites) != 3 || len(d.BatchInitial) != 3 {
		t.Fatal("wrong worker population")
	}
	keys, values := map[string]bool{}, map[string]bool{}
	for worker, batch := range d.BatchWrites {
		if len(batch) != 50 {
			t.Fatal("wrong batch size")
		}
		for slot, key := range batch {
			want := fmt.Sprintf("%sbatchset50/%04d/%04d", prefix, worker, slot)
			if string(key) != want || keys[string(key)] {
				t.Fatalf("nonunique/wrong worker key %q", key)
			}
			keys[string(key)] = true
		}
	}
	for _, phase := range [][][]byte{d.BatchInitial, d.Warmup, d.Measure} {
		for _, payload := range phase {
			if len(payload) != 5000 {
				t.Fatalf("payload length %d", len(payload))
			}
			for slot := range 50 {
				value := string(payload[slot*100 : (slot+1)*100])
				if values[value] {
					t.Fatal("duplicate value across slots, workers, operations or phases")
				}
				values[value] = true
			}
		}
	}
}

func TestBatchSet50PersistedValidationAllSlotsAndBoundaries(t *testing.T) {
	t.Parallel()
	for _, counts := range [][2]int{{0, 0}, {2, 0}, {5, 2}, {0, 5}, {3, 7}} {
		t.Run(fmt.Sprintf("warm%d-measure%d", counts[0], counts[1]), func(t *testing.T) {
			t.Parallel()
			const prefix = "clientbench/batch-validation/"
			d := Prepare(prefix, 4, counts[0], counts[1])
			d.PrepareBatchSet50(prefix)
			r := &batchShapeRecorder{}
			if err := d.Seed(r); err != nil {
				t.Fatal(err)
			}
			for _, phase := range [][][]byte{d.Warmup, d.Measure} {
				for i, payload := range phase {
					if _, err := d.Operation(r, "batchset50", i%4, payload); err != nil {
						t.Fatal(err)
					}
				}
			}
			r.readOnly, r.gets, r.sets = nil, nil, nil
			if err := d.ValidateWrites(r, "batchset50", counts[0], counts[1]); err != nil {
				t.Fatal(err)
			}
			if len(r.readOnly) != 1 || !r.readOnly[0] || len(r.sets) != 0 || len(r.gets) != 204 {
				t.Fatalf("want fresh read-only validation of 4 existing + 200 batch keys: tx=%v sets=%d gets=%d", r.readOnly, len(r.sets), len(r.gets))
			}
			seen := map[string]bool{}
			for _, key := range r.gets {
				if seen[string(key)] {
					t.Fatalf("repeated validation key %q", key)
				}
				seen[string(key)] = true
			}
		})
	}
}

func TestBatchSet50PersistedValidationRejectsEveryCorruptSlot(t *testing.T) {
	t.Parallel()
	const prefix = "clientbench/batch-corruption/"
	d := Prepare(prefix, 1, 2, 3)
	d.PrepareBatchSet50(prefix)
	r := &batchShapeRecorder{}
	if err := d.Seed(r); err != nil {
		t.Fatal(err)
	}
	for _, phase := range [][][]byte{d.Warmup, d.Measure} {
		for _, payload := range phase {
			if _, err := d.Operation(r, "batchset50", 0, payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	for slot, key := range d.BatchWrites[0] {
		original := bytes.Clone(r.values[string(key)])
		r.values[string(key)][99] ^= 1 // Preserve length: validation must compare bytes.
		err := d.ValidateWrites(r, "batchset50", 2, 3)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("slot %d:", slot)) {
			t.Fatalf("corrupt slot %d accepted or misidentified: %v", slot, err)
		}
		r.values[string(key)] = original
	}
}

func TestBatchSet50RejectsUnpreparedShape(t *testing.T) {
	t.Parallel()
	d := Prepare("clientbench/unprepared/", 1, 0, 1)
	r := &batchShapeRecorder{}
	if _, err := d.Operation(r, "batchset50", 0, d.Measure[0]); err == nil || len(r.sets) != 0 {
		t.Fatal("unprepared batch accepted or issued Sets")
	}
}

func TestBatchSet50ConfigAccepted(t *testing.T) {
	t.Parallel()
	cfg := Config{ClusterFile: "cluster", Output: "output", AllowWrites: true, Prefix: "clientbench/v5/", Iterations: 100, Warmup: 40, Workers: 1, TimeoutMS: 30000, RetryLimit: 10, Workload: "batchset50", Profile: "none"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}
